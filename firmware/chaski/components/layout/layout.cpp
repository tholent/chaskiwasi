// Implementation of components/layout — see include/chaski/layout.h for the
// contract and the spec clauses it must satisfy.
//
// Wire contract (server §4.9): line breaks fall on grapheme-cluster
// boundaries, never inside one. Every function below that slices `body`
// slices only at offsets that come out of GraphemeBoundaries — never at an
// arbitrary byte index — which is what makes that contract structural rather
// than a matter of getting the arithmetic right in more than one place.
#include "chaski/layout.h"

#include <utf8proc.h>

#include <algorithm>

namespace chaski::layout {

int Metrics::CharsPerLine() const {
  if (glyph_w_px <= 0) return 0;
  int usable_px = panel_w_px - 2 * margin_px;
  int n = usable_px / glyph_w_px;
  return n > 0 ? n : 0;
}

int Metrics::LinesPerPage() const {
  if (glyph_h_px <= 0) return 0;
  int reserved_px = (reserved_top_rows + reserved_bottom_rows) * glyph_h_px;
  int usable_px = panel_h_px - reserved_px;
  int n = usable_px / glyph_h_px;
  return n > 0 ? n : 0;
}

Metrics MetricsFor(FontSize s) {
  Metrics m;  // defaults are the reference 6x13 face (client §8.2, small).
  if (s == FontSize::kLarge) {
    // Accessibility bump. No pixel values are specified for it; these keep
    // the same aspect ratio as the reference face on the reference panel.
    m.glyph_w_px = 8;
    m.glyph_h_px = 16;
  }
  return m;
}

namespace {

// Decodes the codepoint starting at byte offset `pos`, returning its length
// in bytes via `len`. An invalid byte sequence decodes as U+FFFD and advances
// one byte, so a corrupt body still lays out instead of aborting — v1 renders
// unknown things honestly rather than crashing (client B.10).
utf8proc_int32_t DecodeAt(std::string_view s, std::size_t pos,
                           utf8proc_ssize_t* len) {
  utf8proc_int32_t cp = 0;
  *len = utf8proc_iterate(
      reinterpret_cast<const utf8proc_uint8_t*>(s.data() + pos),
      static_cast<utf8proc_ssize_t>(s.size() - pos), &cp);
  if (*len <= 0) {
    *len = 1;
    return 0xFFFD;
  }
  return cp;
}

// A cluster the line-breaker treats as a forced break: CR, LF, or the CRLF
// cluster (server §0 / C-9's "crlf" vector: CR LF is one grapheme cluster,
// deliberately — so the forced break has to be recognised at the cluster
// level, not by scanning for '\n' and risking a split).
bool IsNewlineCluster(std::string_view c) {
  if (c.empty()) return false;
  for (char ch : c) {
    if (ch != '\n' && ch != '\r') return false;
  }
  return true;
}

// A cluster the line-breaker treats as a preferred, collapsible break point.
// Only single-codepoint clusters qualify — a space does not combine with a
// following mark in practice, so this never risks misclassifying part of a
// larger cluster.
bool IsSpaceCluster(std::string_view c) {
  if (c.size() == 1 && (c[0] == ' ' || c[0] == '\t')) return true;
  utf8proc_int32_t cp = 0;
  utf8proc_ssize_t len = utf8proc_iterate(
      reinterpret_cast<const utf8proc_uint8_t*>(c.data()),
      static_cast<utf8proc_ssize_t>(c.size()), &cp);
  if (len != static_cast<utf8proc_ssize_t>(c.size())) return false;
  return utf8proc_category(cp) == UTF8PROC_CATEGORY_ZS;
}

}  // namespace

std::vector<std::size_t> GraphemeBoundaries(std::string_view s) {
  std::vector<std::size_t> bounds;
  bounds.push_back(0);
  if (s.empty()) return bounds;

  // utf8proc_grapheme_break_stateful requires being called in order on every
  // consecutive codepoint pair, with `state` starting at 0 (utf8proc.h).
  utf8proc_int32_t state = 0;
  std::size_t pos = 0;
  utf8proc_ssize_t len = 0;
  utf8proc_int32_t cp = DecodeAt(s, pos, &len);
  pos += static_cast<std::size_t>(len);

  while (pos < s.size()) {
    utf8proc_ssize_t next_len = 0;
    utf8proc_int32_t next_cp = DecodeAt(s, pos, &next_len);
    if (utf8proc_grapheme_break_stateful(cp, next_cp, &state)) {
      bounds.push_back(pos);
    }
    cp = next_cp;
    pos += static_cast<std::size_t>(next_len);
  }
  bounds.push_back(s.size());
  return bounds;
}

std::size_t CountGraphemes(std::string_view s) {
  // bounds always holds n+1 offsets for n clusters (0 and size included),
  // even for the empty string (bounds == {0}, zero clusters).
  return GraphemeBoundaries(s).size() - 1;
}

std::string TruncateGraphemes(std::string_view s, std::size_t n) {
  std::vector<std::size_t> bounds = GraphemeBoundaries(s);
  std::size_t idx = std::min(n, bounds.size() - 1);
  return std::string(s.substr(0, bounds[idx]));
}

std::vector<std::string> BreakLines(std::string_view body, int chars_per_line) {
  std::vector<std::string> lines;
  if (chars_per_line < 1) chars_per_line = 1;
  if (body.empty()) return lines;
  const std::size_t cpl = static_cast<std::size_t>(chars_per_line);

  std::vector<std::size_t> bounds = GraphemeBoundaries(body);
  std::size_t total = bounds.size() - 1;

  std::string line;
  std::size_t line_len = 0;
  bool pending_space = false;

  std::string word;
  std::size_t word_len = 0;

  // Places the accumulated `word` (client §8.2: prefer breaking at spaces; a
  // word longer than a line breaks at a cluster boundary rather than
  // overflowing). All slicing below uses grapheme boundaries of `word`
  // itself, so a hard-wrapped word never splits a cluster either.
  auto place_word = [&]() {
    if (word_len == 0) {
      pending_space = false;
      return;
    }
    std::size_t sep = (pending_space && line_len > 0) ? 1 : 0;
    if (line_len > 0 && line_len + sep + word_len > cpl) {
      lines.push_back(std::move(line));
      line.clear();
      line_len = 0;
      sep = 0;  // no leading space on a fresh line
    }
    if (sep) {
      line += ' ';
      ++line_len;
    }
    if (word_len <= cpl) {
      line += word;
      line_len += word_len;
    } else {
      // line_len == 0 here: either the line started empty or was just
      // flushed above. Hard-wrap the word at its own cluster boundaries.
      std::vector<std::size_t> wb = GraphemeBoundaries(word);
      std::size_t start_g = 0;
      while (word_len - start_g > cpl) {
        std::size_t end_g = start_g + cpl;
        lines.push_back(
            std::string(word.substr(wb[start_g], wb[end_g] - wb[start_g])));
        start_g = end_g;
      }
      line = word.substr(wb[start_g]);
      line_len = word_len - start_g;
    }
    word.clear();
    word_len = 0;
    pending_space = false;
  };

  for (std::size_t i = 0; i < total; ++i) {
    std::string_view cluster =
        body.substr(bounds[i], bounds[i + 1] - bounds[i]);
    if (IsNewlineCluster(cluster)) {
      place_word();
      lines.push_back(std::move(line));
      line.clear();
      line_len = 0;
      pending_space = false;
      continue;
    }
    if (IsSpaceCluster(cluster)) {
      place_word();
      if (line_len > 0) pending_space = true;
      continue;
    }
    word += cluster;
    ++word_len;
  }
  place_word();
  if (line_len > 0 || lines.empty()) {
    lines.push_back(std::move(line));
  }
  return lines;
}

std::vector<Page> Paginate(std::string_view body, const Metrics& m) {
  std::vector<Page> pages;
  int cpl = m.CharsPerLine();
  int lpp = m.LinesPerPage();
  if (cpl <= 0 || lpp <= 0) return pages;  // no usable area: nothing to show

  std::vector<std::string> lines = BreakLines(body, cpl);
  if (lines.empty()) {
    pages.push_back(Page{});  // an empty letter is still one (blank) page
    return pages;
  }

  const std::size_t page_size = static_cast<std::size_t>(lpp);
  for (std::size_t i = 0; i < lines.size(); i += page_size) {
    std::size_t end = std::min(lines.size(), i + page_size);
    Page p;
    p.lines.assign(lines.begin() + static_cast<std::ptrdiff_t>(i),
                    lines.begin() + static_cast<std::ptrdiff_t>(end));
    pages.push_back(std::move(p));
  }
  return pages;
}

}  // namespace chaski::layout

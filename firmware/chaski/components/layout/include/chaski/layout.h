// Package layout turns a letter body into pages of lines, on the device.
//
// Rendering is device-owned because font size is a runtime accessibility
// setting: a page break computed on the server goes stale the moment the
// reader changes size, and could only be repaired by re-downloading over a
// per-MB link (server §4.9, A.10). Consequently there is no page count on the
// wire and this component owns every layout number in the system.
//
// The one rule the wire contract imposes: line breaks fall on grapheme-cluster
// boundaries, NEVER inside one (server §4.9). Splitting a cluster corrupts an
// emoji or drops a combining accent, and the failure is silent — which is why
// C-9 checks it against vectors generated from the server's own segmenter
// (tools/graphvectors, decision B.7).
//
// Portability: utf8proc only, no esp_* (implementation-plan rule 3).
#pragma once

#include <cstddef>
#include <string>
#include <string_view>
#include <vector>

namespace chaski::layout {

// Metrics describe the panel and the chosen font. They are board-profile and
// settings data, never server data.
struct Metrics {
  int panel_w_px = 264;   // GDEY027T91-FL02 reference panel (client §2)
  int panel_h_px = 176;
  int glyph_w_px = 6;     // fixed-pitch reference face
  int glyph_h_px = 13;
  int margin_px = 2;
  int reserved_top_rows = 1;   // header: sender + date
  int reserved_bottom_rows = 1;  // footer: page indicator, flags

  int CharsPerLine() const;
  int LinesPerPage() const;
};

// FontSize is the runtime accessibility setting (client §8.2). At least two
// sizes exist in v1; changing size repaginates instantly and locally.
enum class FontSize { kSmall, kLarge };

Metrics MetricsFor(FontSize s);

// Page is one screenful of already-broken lines. Lines hold whole grapheme
// clusters only.
struct Page {
  std::vector<std::string> lines;
};

// Paginate breaks `body` into pages under `m`. Breaking prefers spaces; a word
// longer than a line is broken at a cluster boundary rather than overflowing.
// The input is UTF-8 and may contain anything a relative typed.
std::vector<Page> Paginate(std::string_view body, const Metrics& m);

// BreakLines is Paginate's inner half, exposed because it is what C-9's
// vectors exercise directly.
std::vector<std::string> BreakLines(std::string_view body, int chars_per_line);

// CountGraphemes counts extended grapheme clusters per UAX #29 — the unit the
// reader perceives as one character, and the unit every cap in this system
// counts (server §0). Byte and rune counts silently disagree with what the
// panel renders the moment an emoji or combining accent appears.
std::size_t CountGraphemes(std::string_view s);

// TruncateGraphemes returns the longest prefix of `s` holding at most `n`
// clusters, never splitting one. Used by the compose counter against the
// server's max_letter_chars (client §11.2).
std::string TruncateGraphemes(std::string_view s, std::size_t n);

// GraphemeBoundaries returns byte offsets of every cluster boundary, including
// 0 and s.size(). This is the primitive C-9 compares against the Go vectors.
std::vector<std::size_t> GraphemeBoundaries(std::string_view s);

}  // namespace chaski::layout

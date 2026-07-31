// Wave 1C's real suite for components/layout.
//
// C-9 is the referee for client B.7: tools/graphvectors emits vectors from
// the server's own segmenter (rivo/uniseg) into testdata/graphemes.json, and
// this file checks that utf8proc reproduces them exactly. Disagreeing here
// means a letter the compose counter accepted would come back with a
// terminal `invalid` ack on the wire — a "couldn't send" the child did
// nothing to deserve (client B.7).
#include <gtest/gtest.h>
#include <utf8proc.h>

#include <cjson/cJSON.h>

#include <cstdio>
#include <fstream>
#include <sstream>
#include <string>
#include <vector>

#include "chaski/layout.h"

using chaski::layout::BreakLines;
using chaski::layout::CountGraphemes;
using chaski::layout::FontSize;
using chaski::layout::GraphemeBoundaries;
using chaski::layout::Metrics;
using chaski::layout::MetricsFor;
using chaski::layout::Paginate;
using chaski::layout::TruncateGraphemes;

namespace {

struct Vector {
  std::string name;
  std::string text;
  std::vector<std::size_t> boundaries;
  std::size_t count = 0;
};

struct VectorFile {
  std::string unicode_version;
  std::vector<Vector> vectors;
};

// Reads and parses testdata/graphemes.json. CHASKI_TESTDATA_DIR is set by the
// top-level CMakeLists so this works regardless of where the build tree
// lives.
VectorFile LoadVectors() {
  std::string path = std::string(CHASKI_TESTDATA_DIR) + "/graphemes.json";
  std::ifstream in(path, std::ios::binary);
  if (!in) {
    ADD_FAILURE() << "cannot open " << path;
    return {};
  }
  std::ostringstream ss;
  ss << in.rdbuf();
  std::string contents = ss.str();

  cJSON* root = cJSON_Parse(contents.c_str());
  if (root == nullptr) {
    ADD_FAILURE() << "cannot parse " << path;
    return {};
  }

  VectorFile out;
  cJSON* uv = cJSON_GetObjectItemCaseSensitive(root, "unicode_version");
  if (cJSON_IsString(uv)) out.unicode_version = uv->valuestring;

  cJSON* vectors = cJSON_GetObjectItemCaseSensitive(root, "vectors");
  cJSON* item = nullptr;
  cJSON_ArrayForEach(item, vectors) {
    Vector v;
    cJSON* name = cJSON_GetObjectItemCaseSensitive(item, "name");
    cJSON* text = cJSON_GetObjectItemCaseSensitive(item, "text");
    cJSON* count = cJSON_GetObjectItemCaseSensitive(item, "count");
    cJSON* boundaries = cJSON_GetObjectItemCaseSensitive(item, "boundaries");
    if (cJSON_IsString(name)) v.name = name->valuestring;
    if (cJSON_IsString(text)) v.text = text->valuestring;
    if (cJSON_IsNumber(count)) v.count = static_cast<std::size_t>(count->valueint);
    cJSON* b = nullptr;
    cJSON_ArrayForEach(b, boundaries) {
      if (cJSON_IsNumber(b)) v.boundaries.push_back(static_cast<std::size_t>(b->valueint));
    }
    out.vectors.push_back(std::move(v));
  }
  cJSON_Delete(root);
  return out;
}

const Vector* Find(const VectorFile& f, const char* name) {
  for (const auto& v : f.vectors) {
    if (v.name == name) return &v;
  }
  return nullptr;
}

}  // namespace

// server §4.9 / client §8.2: layout owns every layout number in the system;
// the reference panel is 264x176 (client §2).
TEST(Layout, ReferenceMetricsMatchThePanel) {
  chaski::layout::Metrics m;
  EXPECT_EQ(m.panel_w_px, 264);
  EXPECT_EQ(m.panel_h_px, 176);
}

// C-9: utf8proc's boundaries and count must reproduce every vector exactly.
TEST(C9, VectorsFromServerSegmenterReproduceExactly) {
  VectorFile f = LoadVectors();
  ASSERT_FALSE(f.vectors.empty()) << "no vectors loaded";
  for (const auto& v : f.vectors) {
    SCOPED_TRACE(v.name);
    EXPECT_EQ(GraphemeBoundaries(v.text), v.boundaries);
    EXPECT_EQ(CountGraphemes(v.text), v.count);
  }
}

// B.7: a Unicode-version skew between utf8proc and uniseg must fail loudly,
// here, rather than silently rejecting a child's letter on the wire.
TEST(C9, UnicodeVersionMatchesServerVectors) {
  VectorFile f = LoadVectors();
  ASSERT_FALSE(f.unicode_version.empty());
  EXPECT_STREQ(utf8proc_unicode_version(), f.unicode_version.c_str());
}

TEST(C9, EmptyInput) {
  EXPECT_EQ(GraphemeBoundaries(""), (std::vector<std::size_t>{0}));
  EXPECT_EQ(CountGraphemes(""), 0u);
  EXPECT_EQ(TruncateGraphemes("", 5), "");
}

TEST(C9, SingleClusterInput) {
  VectorFile f = LoadVectors();
  const Vector* v = Find(f, "emoji_simple");
  ASSERT_NE(v, nullptr);
  EXPECT_EQ(CountGraphemes(v->text), 1u);
  EXPECT_EQ(TruncateGraphemes(v->text, 0), "");
  EXPECT_EQ(TruncateGraphemes(v->text, 1), v->text);
  EXPECT_EQ(TruncateGraphemes(v->text, 99), v->text);  // clamps, doesn't overrun
}

// TruncateGraphemes must never land inside a multi-codepoint ZWJ sequence: it
// can only cut at an offset GraphemeBoundaries itself reports.
TEST(C9, TruncateGraphemesNeverSplitsAZwjSequence) {
  VectorFile f = LoadVectors();
  const Vector* v = Find(f, "zwj_at_cap_boundary");
  ASSERT_NE(v, nullptr);
  ASSERT_GE(v->boundaries.size(), 5u);  // 4 a's + 1 family cluster

  // Cap right at the cluster boundary just before the ZWJ sequence: the
  // longest prefix holding at most 4 clusters is exactly the four a's.
  std::string cut4 = TruncateGraphemes(v->text, 4);
  EXPECT_EQ(cut4.size(), v->boundaries[4]);
  EXPECT_EQ(cut4, v->text.substr(0, v->boundaries[4]));
  EXPECT_EQ(CountGraphemes(cut4), 4u);

  // A cap of 5 (the full count) must include the whole family cluster, never
  // a byte-prefix of it.
  std::string cut5 = TruncateGraphemes(v->text, v->count);
  EXPECT_EQ(cut5, v->text);
}

// Builds a realistic multi-script, multi-emoji letter out of every vector's
// text (skipping "empty"), joined with plain ASCII spaces so the seam between
// vectors is never itself ambiguous.
std::string BuildRealisticLetter(const VectorFile& f) {
  std::string body;
  int i = 0;
  for (const auto& v : f.vectors) {
    if (v.name == "empty") continue;
    if (!body.empty()) body += ((i % 4 == 0) ? "\n" : " ");
    body += v.text;
    ++i;
  }
  return body;
}

// A cluster the test treats as an intentional, ASCII separator it inserted or
// that a vector itself contains (plain space/tab, or the all-CR/LF forced
// break) — mirrors layout.cpp's own classification for the plain-ASCII cases
// exercised here, so this check is independent of BreakLines' internals
// while still agreeing on what counts as a separator.
bool IsAsciiSeparatorCluster(const std::string& c) {
  if (c.size() == 1 && (c[0] == ' ' || c[0] == '\t')) return true;
  bool all_newline = !c.empty();
  for (char ch : c) {
    if (ch != '\n' && ch != '\r') all_newline = false;
  }
  return all_newline;
}

// C-9 / client §8.2: no line break falls inside a cluster, checked against
// the boundary set for a realistic multi-line letter. Verified by
// re-tokenising every output line back into clusters and confirming the
// resulting sequence — separators removed — matches the original letter's
// clusters exactly, in order. A mid-cluster split would produce a different
// (but still individually valid) cluster sequence, so this catches it even
// though every line remains well-formed UTF-8 on its own.
TEST(C9, NoLineBreakSplitsAClusterOnARealisticLetter) {
  VectorFile f = LoadVectors();
  std::string body = BuildRealisticLetter(f);
  ASSERT_FALSE(body.empty());

  std::vector<std::string> want;
  {
    std::vector<std::size_t> b = GraphemeBoundaries(body);
    for (std::size_t i = 0; i + 1 < b.size(); ++i) {
      std::string cluster = body.substr(b[i], b[i + 1] - b[i]);
      if (!IsAsciiSeparatorCluster(cluster)) want.push_back(cluster);
    }
  }

  // A small width forces both space-preferred breaks and hard mid-word
  // breaks through the wide emoji/flag clusters in the letter.
  std::vector<std::string> lines = BreakLines(body, /*chars_per_line=*/7);
  ASSERT_GT(lines.size(), 1u) << "test letter did not actually wrap";

  std::vector<std::string> got;
  for (const auto& line : lines) {
    std::vector<std::size_t> b = GraphemeBoundaries(line);
    for (std::size_t i = 0; i + 1 < b.size(); ++i) {
      std::string cluster = line.substr(b[i], b[i + 1] - b[i]);
      if (!IsAsciiSeparatorCluster(cluster)) got.push_back(cluster);
    }
  }

  EXPECT_EQ(got, want);
}

// Every returned line must itself hold at most chars_per_line clusters.
TEST(Layout, BreakLinesNeverExceedsWidth) {
  VectorFile f = LoadVectors();
  std::string body = BuildRealisticLetter(f);
  for (int cpl : {1, 5, 20, 43}) {
    std::vector<std::string> lines = BreakLines(body, cpl);
    for (const auto& line : lines) {
      EXPECT_LE(CountGraphemes(line), static_cast<std::size_t>(cpl))
          << "chars_per_line=" << cpl << " line=\"" << line << "\"";
    }
  }
}

TEST(Layout, BreakLinesPrefersSpaceOverSplittingAWordThatFits) {
  std::vector<std::string> lines = BreakLines("hello world", 5);
  ASSERT_EQ(lines.size(), 2u);
  EXPECT_EQ(lines[0], "hello");
  EXPECT_EQ(lines[1], "world");
}

TEST(Layout, BreakLinesHardWrapsAWordLongerThanTheLine) {
  std::vector<std::string> lines = BreakLines("abcdefgh", 3);
  ASSERT_EQ(lines.size(), 3u);
  EXPECT_EQ(lines[0], "abc");
  EXPECT_EQ(lines[1], "def");
  EXPECT_EQ(lines[2], "gh");
}

// Hard-wrapping (client §8.2: "a word longer than a line breaks at a cluster
// boundary rather than overflowing") must cut between wide multi-byte
// clusters, never inside one. Uses the ZWJ family emoji vector — a single
// grapheme spanning many bytes — repeated with no spaces, so BreakLines is
// forced to hard-wrap mid-"word" through it.
TEST(C9, HardWrapNeverSplitsAWideMultiByteCluster) {
  VectorFile f = LoadVectors();
  const Vector* v = Find(f, "emoji_zwj_family");
  ASSERT_NE(v, nullptr);
  ASSERT_EQ(v->count, 1u);
  const std::size_t cluster_bytes = v->boundaries[1];

  std::string word;
  for (int i = 0; i < 5; ++i) word += v->text;

  std::vector<std::string> lines = BreakLines(word, /*chars_per_line=*/2);
  ASSERT_EQ(lines.size(), 3u);
  EXPECT_EQ(CountGraphemes(lines[0]), 2u);
  EXPECT_EQ(CountGraphemes(lines[1]), 2u);
  EXPECT_EQ(CountGraphemes(lines[2]), 1u);
  for (const auto& line : lines) {
    EXPECT_EQ(line.size() % cluster_bytes, 0u)
        << "a line's byte length that isn't a multiple of the cluster's own "
           "length means a cluster was cut mid-byte-sequence";
  }
  EXPECT_EQ(lines[0] + lines[1] + lines[2], word);  // lossless reconstruction
}

TEST(Layout, BreakLinesTreatsNewlineAsAParagraphBreak) {
  std::vector<std::string> lines = BreakLines("hi\nthere", 40);
  ASSERT_EQ(lines.size(), 2u);
  EXPECT_EQ(lines[0], "hi");
  EXPECT_EQ(lines[1], "there");
}

TEST(Layout, MetricsForProducesTwoDistinctSizes) {
  Metrics small = MetricsFor(FontSize::kSmall);
  Metrics large = MetricsFor(FontSize::kLarge);
  EXPECT_GT(large.glyph_w_px, small.glyph_w_px);
  EXPECT_GT(large.glyph_h_px, small.glyph_h_px);
  EXPECT_GT(small.CharsPerLine(), large.CharsPerLine());
  EXPECT_GT(small.LinesPerPage(), large.LinesPerPage());
}

// C-9 / client §8.2: a full 500-grapheme letter — "one 2.7in screen" at the
// reference font (client §8.2) — paginates cleanly at both font sizes, and
// the larger, sparser face takes at least as many pages.
TEST(Layout, PaginatesA500GraphemeLetterAtBothFontSizes) {
  std::string word = "café ";  // 5 graphemes (4 + space) per repeat
  std::string body;
  while (CountGraphemes(body) < 500) body += word;
  ASSERT_GE(CountGraphemes(body), 500u);

  Metrics small = MetricsFor(FontSize::kSmall);
  Metrics large = MetricsFor(FontSize::kLarge);

  std::vector<chaski::layout::Page> small_pages = Paginate(body, small);
  std::vector<chaski::layout::Page> large_pages = Paginate(body, large);

  ASSERT_FALSE(small_pages.empty());
  ASSERT_FALSE(large_pages.empty());
  EXPECT_GE(large_pages.size(), small_pages.size());

  for (const auto& p : small_pages) {
    EXPECT_LE(p.lines.size(), static_cast<std::size_t>(small.LinesPerPage()));
    for (const auto& line : p.lines) {
      EXPECT_LE(CountGraphemes(line), static_cast<std::size_t>(small.CharsPerLine()));
    }
  }
}

TEST(Layout, PaginateOfEmptyBodyIsOneBlankPage) {
  std::vector<chaski::layout::Page> pages = Paginate("", MetricsFor(FontSize::kSmall));
  ASSERT_EQ(pages.size(), 1u);
  EXPECT_TRUE(pages[0].lines.empty());
}

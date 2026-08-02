#include "text_util.h"

#include <ctime>

#include "chaski/layout.h"
#include "chaski_strings.h"

namespace chaski::ui {
namespace {

// Weekday ids in tm_wday order: 0 is Sunday.
const int kDayIds[7] = {STR_DAY_SUN, STR_DAY_MON, STR_DAY_TUE, STR_DAY_WED,
                        STR_DAY_THU, STR_DAY_FRI, STR_DAY_SAT};

}  // namespace

std::string Format(const char* tmpl, const std::vector<std::string>& args) {
  std::string out;
  if (tmpl == nullptr) return out;
  std::size_t next = 0;
  for (const char* p = tmpl; *p != '\0'; ++p) {
    if (p[0] == '{' && p[1] == '}') {
      if (next < args.size()) out += args[next];
      ++next;
      ++p;
      continue;
    }
    out.push_back(*p);
  }
  return out;
}

std::string Int(long long v) {
  if (v == 0) return std::string(1, '0');
  const bool neg = v < 0;
  unsigned long long m = neg ? 0ULL - static_cast<unsigned long long>(v)
                             : static_cast<unsigned long long>(v);
  char buf[24];
  int n = 0;
  while (m > 0 && n < static_cast<int>(sizeof(buf))) {
    buf[n++] = static_cast<char>('0' + (m % 10));
    m /= 10;
  }
  std::string out;
  if (neg) out.push_back('-');
  while (n > 0) out.push_back(buf[--n]);
  return out;
}

std::string Pad2(int v) {
  std::string s = Int(v);
  if (s.size() < 2) s.insert(s.begin(), '0');
  return s;
}

std::string TimeLabel(std::int64_t epoch, bool clock_valid, TextFn text) {
  if (!clock_valid || epoch <= 0 || text == nullptr) return std::string();
  const std::time_t t = static_cast<std::time_t>(epoch);
  std::tm tm{};
  if (::gmtime_r(&t, &tm) == nullptr) return std::string();
  const int wday = (tm.tm_wday >= 0 && tm.tm_wday < 7) ? tm.tm_wday : 0;
  return Format(text(STR_TIME_FMT),
                {text(kDayIds[wday]), Pad2(tm.tm_hour), Pad2(tm.tm_min)});
}

void AppendCodepoint(std::string& s, unsigned int cp) {
  if (cp < 0x80) {
    s.push_back(static_cast<char>(cp));
  } else if (cp < 0x800) {
    s.push_back(static_cast<char>(0xC0 | (cp >> 6)));
    s.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
  } else if (cp < 0x10000) {
    s.push_back(static_cast<char>(0xE0 | (cp >> 12)));
    s.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
    s.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
  } else if (cp <= 0x10FFFF) {
    s.push_back(static_cast<char>(0xF0 | (cp >> 18)));
    s.push_back(static_cast<char>(0x80 | ((cp >> 12) & 0x3F)));
    s.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
    s.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
  }
}

void EraseLastGrapheme(std::string& s) {
  if (s.empty()) return;
  const std::vector<std::size_t> b = layout::GraphemeBoundaries(s);
  // Boundaries always include 0 and size(), so the second-to-last entry is
  // where the final cluster starts.
  if (b.size() < 2) {
    s.clear();
    return;
  }
  s.resize(b[b.size() - 2]);
}

std::string Ellipsize(const std::string& s, std::size_t max_graphemes) {
  if (layout::CountGraphemes(s) <= max_graphemes) return s;
  return layout::TruncateGraphemes(s, max_graphemes);
}

}  // namespace chaski::ui

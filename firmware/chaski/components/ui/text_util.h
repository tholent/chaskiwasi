// Text assembly for the screens: placeholder substitution, dates, and the two
// grapheme edits a text field needs.
//
// Why a formatter at all: several strings are sentences built from parts —
// "battery 64%, good signal, 1 letter waiting" — and assembling them with
// literals in this component would put user-visible text outside the strings
// table, which is the one thing C-15 forbids. The template lives in the table
// with its punctuation; only the numbers come from here.
//
// printf is deliberately not used: a format string read from a table and
// passed to printf is a format-string bug waiting for a table edit. `{}` has
// no such failure mode — a mismatched count yields a missing word, not a stack
// read.
#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "chaski/ui.h"

namespace chaski::ui {

// Format substitutes each "{}" in `tmpl` with the next argument. Surplus
// placeholders become empty; surplus arguments are dropped.
std::string Format(const char* tmpl, const std::vector<std::string>& args);

std::string Int(long long v);

// Pad2 renders clock components: 9 minutes past is "09", never "9".
std::string Pad2(int v);

// TimeLabel renders an epoch as the child sees it: "Tue 15:04". It returns
// empty when the clock has not been disciplined by a sync, because a wrong
// date is worse than no date on a device with no battery-backed RTC
// (client §5.6, C-21).
std::string TimeLabel(std::int64_t epoch, bool clock_valid, TextFn text);

// AppendCodepoint encodes one codepoint as UTF-8 onto `s`.
void AppendCodepoint(std::string& s, unsigned int cp);

// EraseLastGrapheme removes one extended grapheme cluster from the end —
// backspace deletes what the reader sees as one character, never a byte and
// never half an emoji (server §0, B.10).
void EraseLastGrapheme(std::string& s);

// Ellipsize shortens `s` to at most `max_graphemes` clusters for a list row,
// cutting on a cluster boundary.
std::string Ellipsize(const std::string& s, std::size_t max_graphemes);

}  // namespace chaski::ui

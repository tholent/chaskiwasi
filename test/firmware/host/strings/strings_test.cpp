// C-15 (partial): the vocabulary boundary, asserted from inside the build.
//
// tools/fwgates does the file-level scan (no user-visible literals outside
// strings.c, no address-shaped text). This test covers what only the compiled
// table can answer: every id has text, and no rendered string carries an
// internal identifier.
#include <gtest/gtest.h>

#include <cstring>
#include <string>

#include "chaski_strings.h"

// design §0.1 / client §0: pututu, ayllu, and kipu are greppable internal
// identifiers. They may appear in code and in wire field names — the wire is
// machine-facing — but never on a screen a child reads.
TEST(C15, NoInternalVocabularyInAnyUserVisibleString) {
  static const char* const kForbidden[] = {"pututu", "ayllu", "kipu"};
  for (int i = 0; i < STR__COUNT; ++i) {
    std::string s = S(static_cast<chaski_string_id_t>(i));
    for (const char* bad : kForbidden) {
      EXPECT_EQ(s.find(bad), std::string::npos)
          << "string id " << i << " contains internal vocabulary: " << s;
    }
  }
}

TEST(C15, EveryStringIdHasText) {
  for (int i = 0; i < STR__COUNT; ++i) {
    EXPECT_GT(std::strlen(S(static_cast<chaski_string_id_t>(i))), 0u)
        << "string id " << i << " has no text";
  }
}

// server A.16: the system sender is "Wasi", not "Home". Pinned by test because
// notice letters graduate — this name is on those records for as long as the
// archive lasts, and an archive whose sender name changes halfway through
// reads as two different correspondents.
TEST(C15, SystemSenderIsWasi) {
  EXPECT_STREQ(S(STR_SYSTEM_SENDER), "Wasi");
}

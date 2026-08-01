// Host tests for components/wire.
//
// The point of this suite is stated in implementation-plan §2: the C++ mirror
// of internal/protocol can drift from the server only by failing a test, never
// silently in a pocket. Every fixture under testdata/wire/ is generated from
// the Go types by tools/wirefixtures; the request fixtures are matched against
// what EncodeRequest emits, the response fixtures against what DecodeResponse
// accepts, and index.json drives the sweep so a newly generated fixture joins
// the suite without anyone remembering to add it.
//
// The second half of the suite is about bytes off the road: unknown fields are
// ignored (client §5.5), and no malformed input may crash the parser.
#include <gtest/gtest.h>

#include <cstdio>
#include <string>
#include <vector>

#include "cJSON.h"
#include "chaski/wire.h"

using chaski::wire::AckIsReject;
using chaski::wire::AckStatus;
using chaski::wire::DecodeResponse;
using chaski::wire::EncodeRequest;
using chaski::wire::Kipu;
using chaski::wire::Outbound;
using chaski::wire::ParseAckStatus;
using chaski::wire::Request;
using chaski::wire::Response;

namespace {

std::string FixturePath(const std::string& name) {
  return std::string(CHASKI_TESTDATA_DIR) + "/wire/" + name;
}

std::string ReadFixture(const std::string& name) {
  std::FILE* f = std::fopen(FixturePath(name).c_str(), "rb");
  if (f == nullptr) return std::string();
  std::string out;
  char buf[4096];
  std::size_t n = 0;
  while ((n = std::fread(buf, 1, sizeof(buf), f)) > 0) out.append(buf, n);
  std::fclose(f);
  return out;
}

// JsonEqual compares by structure, not by bytes: key order and whitespace are
// the Go encoder's business, the shape of the request is the contract.
::testing::AssertionResult JsonEqual(const std::string& got, const std::string& want) {
  cJSON* a = cJSON_ParseWithLength(got.data(), got.size());
  cJSON* b = cJSON_ParseWithLength(want.data(), want.size());
  const bool same = a != nullptr && b != nullptr && cJSON_Compare(a, b, 1) != 0;
  cJSON_Delete(a);
  cJSON_Delete(b);
  if (same) return ::testing::AssertionSuccess();
  return ::testing::AssertionFailure() << "encoded: " << got << "\nfixture: " << want;
}

// FixtureNames reads index.json so the sweep covers whatever tools/wirefixtures
// last emitted, not a list copied here that can go stale.
std::vector<std::string> FixtureNames() {
  std::vector<std::string> names;
  const std::string index = ReadFixture("index.json");
  cJSON* root = cJSON_ParseWithLength(index.data(), index.size());
  const cJSON* fixtures = cJSON_GetObjectItemCaseSensitive(root, "fixtures");
  const cJSON* it = nullptr;
  cJSON_ArrayForEach(it, fixtures) {
    if (it->string != nullptr) names.push_back(it->string);
  }
  cJSON_Delete(root);
  return names;
}

// StringField pulls one string out of encoded JSON, for the cases where the
// exact bytes matter (UTF-8 preservation).
std::string StringField(const std::string& json, const std::string& key) {
  cJSON* root = cJSON_ParseWithLength(json.data(), json.size());
  const cJSON* it = cJSON_GetObjectItemCaseSensitive(root, key.c_str());
  std::string out;
  if (cJSON_IsString(it) && it->valuestring != nullptr) out = it->valuestring;
  cJSON_Delete(root);
  return out;
}

}  // namespace

// ---------------------------------------------------------------- requests

// The heartbeat is the shape most syncs take (server §4.2): cursor and version
// and nothing else. Emitting an empty outbound array or a zero counter here
// would cost bytes on a per-MB link for no information.
TEST(Wire, HeartbeatRequestMatchesFixture) {
  Request r;
  r.cursor = "b64cursorAAAA";
  r.ayllu_version = 7;
  EXPECT_TRUE(JsonEqual(EncodeRequest(r), ReadFixture("request_heartbeat.json")));
}

TEST(Wire, FullRequestMatchesFixture) {
  Request r;
  r.cursor = "b64cursorBBBB";
  r.ayllu_version = 8;
  r.pututu_counter_seen = 41;
  Kipu k;
  k.battery_pct = 84;
  k.charging = false;
  k.rat = "ltem";
  k.rssi = -97;
  k.queue_depth = 2;
  k.fw = "0.1.0";
  r.kipu = k;
  r.outbound.push_back(Outbound{"o-000123", "c_07", "camping!", "we went to the lake"});
  // The second entry carries no subject: absent means the server generates one
  // (server §6.2), and the fixture proves the field is omitted, not empty.
  r.outbound.push_back(Outbound{"o-000124", "c_02", "", "no subject on this one"});
  EXPECT_TRUE(JsonEqual(EncodeRequest(r), ReadFixture("request_full.json")));
}

// An empty cursor is a value, not a missing field: it asks for a window resync
// (server §4.4). Omitting it would silently turn a recovering device into one
// that never receives anything.
TEST(Wire, WindowResyncRequestMatchesFixture) {
  Request r;
  r.cursor = "";
  r.ayllu_version = 0;
  const std::string encoded = EncodeRequest(r);
  EXPECT_TRUE(JsonEqual(encoded, ReadFixture("request_window_resync.json")));
  EXPECT_NE(encoded.find("cursor"), std::string::npos);
}

TEST(Wire, EmojiRequestMatchesFixtureByteForByte) {
  const std::string body = "familia \xF0\x9F\x91\xA8\xE2\x80\x8D\xF0\x9F\x91\xA9\xE2\x80\x8D"
                           "\xF0\x9F\x91\xA7\xE2\x80\x8D\xF0\x9F\x91\xA6 desde "
                           "\xF0\x9F\x87\xB5\xF0\x9F\x87\xAA \xE2\x80\x94 caf\xC3\xA9 "
                           "\xE2\x98\x95 est\xC3\xA1 muy bien";
  Request r;
  r.cursor = "b64cursorCCCC";
  r.ayllu_version = 8;
  r.outbound.push_back(
      Outbound{"o-000125", "c_07", "hola \xF0\x9F\x91\x8B\xF0\x9F\x8F\xBD", body});
  const std::string encoded = EncodeRequest(r);
  EXPECT_TRUE(JsonEqual(encoded, ReadFixture("request_emoji_body.json")));

  // ZWJ sequences and flags survive the encoder unaltered — a mangled grapheme
  // is a letter the child did not write.
  cJSON* root = cJSON_ParseWithLength(encoded.data(), encoded.size());
  const cJSON* arr = cJSON_GetObjectItemCaseSensitive(root, "outbound");
  const cJSON* first = cJSON_GetArrayItem(arr, 0);
  const cJSON* got = cJSON_GetObjectItemCaseSensitive(first, "body");
  ASSERT_TRUE(cJSON_IsString(got));
  EXPECT_EQ(std::string(got->valuestring), body);
  cJSON_Delete(root);
}

// The cursor is opaque and the device never parses it (server §4.4): whatever
// arrives is echoed byte for byte, including bytes that would break a parser
// the device is not allowed to have.
TEST(Wire, CursorIsEchoedVerbatim) {
  const std::string opaque = "not/base64:{\"uidvalidity\":9}\\\"";
  Request r;
  r.cursor = opaque;
  EXPECT_EQ(StringField(EncodeRequest(r), "cursor"), opaque);
}

// A kipu block over the cap is dropped rather than sent: the server drops it on
// arrival anyway (server §4.8), and health telemetry may never cost a letter
// its sync.
TEST(Wire, OversizedKipuIsDroppedNotSent) {
  Request r;
  r.cursor = "c";
  Kipu k;
  k.fw = std::string(chaski::wire::kMaxKipuBytes + 64, 'x');
  r.kipu = k;
  const std::string encoded = EncodeRequest(r);
  EXPECT_EQ(encoded.find("kipu"), std::string::npos);
  EXPECT_NE(encoded.find("cursor"), std::string::npos);
}

// --------------------------------------------------------------- responses

TEST(Wire, LettersFixtureDecodes) {
  Response r;
  ASSERT_TRUE(DecodeResponse(ReadFixture("response_letters.json"), r));
  EXPECT_EQ(r.server_time, 1785420202);
  EXPECT_EQ(r.cursor, "b64cursorDDDD");
  EXPECT_EQ(r.pututu_counter, 41u);
  EXPECT_FALSE(r.more);
  ASSERT_EQ(r.letters.size(), 4u);

  EXPECT_EQ(r.letters[0].id, "l-9f3a2c41d0");
  EXPECT_EQ(r.letters[0].contact_id, "c_07");
  EXPECT_EQ(r.letters[0].subject, "camping");
  EXPECT_EQ(r.letters[0].date, 1785349200);
  EXPECT_FALSE(r.letters[0].body.empty());

  // trimmed, truncated and degraded are three distinct events the device may
  // render differently (server §4.3, §5.3); collapsing any pair loses meaning.
  EXPECT_TRUE(r.letters[0].trimmed);
  EXPECT_FALSE(r.letters[0].truncated);
  EXPECT_TRUE(r.letters[1].truncated);
  EXPECT_FALSE(r.letters[1].trimmed);
  EXPECT_TRUE(r.letters[3].degraded);

  // Notices arrive from the reserved system contact (server §7.4).
  EXPECT_EQ(r.letters[2].contact_id, chaski::wire::kSysContactId);
}

// Every terminal status the wire can carry, including one this firmware has
// never heard of. An uninterpretable ack is still terminal and still a reject
// (D-5, client §5.4) — the alternative is a letter retried forever.
TEST(Wire, AllAckStatusesDecodeAndAreTerminal) {
  Response r;
  ASSERT_TRUE(DecodeResponse(ReadFixture("response_all_ack_statuses.json"), r));
  ASSERT_EQ(r.acks.size(), 6u);

  EXPECT_EQ(r.acks[0].status, AckStatus::kSent);
  EXPECT_EQ(r.acks[1].status, AckStatus::kRejectedInactive);
  EXPECT_EQ(r.acks[2].status, AckStatus::kRejectedUnknownContact);
  EXPECT_EQ(r.acks[3].status, AckStatus::kInvalid);
  EXPECT_EQ(r.acks[4].status, AckStatus::kRejectedUndeliverable);
  EXPECT_EQ(r.acks[5].status, AckStatus::kUnknown);

  EXPECT_FALSE(AckIsReject(r.acks[0].status));
  for (std::size_t i = 1; i < r.acks.size(); ++i) {
    EXPECT_TRUE(AckIsReject(r.acks[i].status)) << "ack " << i;
  }
  EXPECT_EQ(r.acks[5].local_id, "o-000006");
}

TEST(Wire, AckStatusParsingIsExact) {
  EXPECT_EQ(ParseAckStatus("sent"), AckStatus::kSent);
  EXPECT_EQ(ParseAckStatus("Sent"), AckStatus::kUnknown);
  EXPECT_EQ(ParseAckStatus(""), AckStatus::kUnknown);
  EXPECT_EQ(ParseAckStatus("rejected"), AckStatus::kUnknown);
  EXPECT_TRUE(AckIsReject(AckStatus::kUnknown));
}

TEST(Wire, AylluAndConfigFixtureDecodes) {
  Response r;
  ASSERT_TRUE(DecodeResponse(ReadFixture("response_ayllu_and_config.json"), r));
  ASSERT_TRUE(r.ayllu.has_value());
  EXPECT_EQ(r.ayllu->version, 8);
  ASSERT_EQ(r.ayllu->contacts.size(), 3u);

  EXPECT_TRUE(r.ayllu->contacts[0].active);
  EXPECT_TRUE(r.ayllu->contacts[0].pinned);
  EXPECT_EQ(r.ayllu->contacts[0].portrait, "p01");

  // A tombstone arrives inactive and must still decode: old letters keep
  // showing the name (server §7.2).
  EXPECT_EQ(r.ayllu->contacts[1].id, "c_07");
  EXPECT_FALSE(r.ayllu->contacts[1].active);
  EXPECT_EQ(r.ayllu->contacts[1].name, "Rosa");

  // The system contact ships as a tombstone named Wasi (server A.15, A.16).
  EXPECT_EQ(r.ayllu->contacts[2].id, chaski::wire::kSysContactId);
  EXPECT_EQ(r.ayllu->contacts[2].name, "Wasi");
  EXPECT_FALSE(r.ayllu->contacts[2].active);

  ASSERT_TRUE(r.config.has_value());
  EXPECT_EQ(r.config->max_letter_chars.value_or(-1), 500);
  EXPECT_EQ(r.config->sync_interval_s.value_or(-1), 21600);
  EXPECT_EQ(r.config->rat, "ltem");
  EXPECT_EQ(r.config->cover, "road");
}

TEST(Wire, MoreTrueFixtureDecodes) {
  Response r;
  ASSERT_TRUE(DecodeResponse(ReadFixture("response_more_true.json"), r));
  EXPECT_TRUE(r.more);
  EXPECT_EQ(r.letters.size(), 1u);
}

// The sweep: whatever tools/wirefixtures emitted, this suite sees. A fixture
// added for a new wire field fails here until the mirror learns it.
TEST(Wire, EveryFixtureIsHandled) {
  const std::vector<std::string> names = FixtureNames();
  ASSERT_FALSE(names.empty()) << "index.json listed no fixtures";
  for (const std::string& name : names) {
    const std::string body = ReadFixture(name);
    ASSERT_FALSE(body.empty()) << name;
    if (name.rfind("response_", 0) == 0) {
      Response r;
      EXPECT_TRUE(DecodeResponse(body, r)) << name;
      EXPECT_FALSE(r.cursor.empty()) << name;
    } else {
      // Request fixtures are asserted against the encoder in the named tests
      // above; here they only have to be well-formed JSON.
      cJSON* root = cJSON_ParseWithLength(body.data(), body.size());
      EXPECT_TRUE(cJSON_IsObject(root)) << name;
      cJSON_Delete(root);
    }
  }
}

// ------------------------------------------------- tolerance and rejection

// Forward compatibility (client §5.5): a server that grows a field must not
// stop an already-shipped device from reading its letters.
TEST(Wire, UnknownFieldsAreIgnored) {
  const std::string json =
      "{\"cursor\":\"c1\",\"server_time\":10,\"more\":false,\"future_top\":{\"a\":[1,2]},"
      "\"letters\":[{\"id\":\"l-1\",\"contact_id\":\"c_02\",\"subject\":\"s\",\"date\":9,"
      "\"body\":\"b\",\"future_letter_field\":true}],"
      "\"acks\":[{\"local_id\":\"o-1\",\"status\":\"sent\",\"future_ack_field\":7}],"
      "\"config\":{\"max_letter_chars\":300,\"future_config_field\":\"x\"},"
      "\"ayllu\":{\"version\":2,\"contacts\":[{\"id\":\"c_02\",\"name\":\"Abuela\","
      "\"active\":true,\"future_contact_field\":null}]}}";
  Response r;
  ASSERT_TRUE(DecodeResponse(json, r));
  EXPECT_EQ(r.cursor, "c1");
  ASSERT_EQ(r.letters.size(), 1u);
  EXPECT_EQ(r.letters[0].id, "l-1");
  ASSERT_EQ(r.acks.size(), 1u);
  EXPECT_EQ(r.acks[0].status, AckStatus::kSent);
  ASSERT_TRUE(r.config.has_value());
  EXPECT_EQ(r.config->max_letter_chars.value_or(-1), 300);
  ASSERT_TRUE(r.ayllu.has_value());
  EXPECT_EQ(r.ayllu->contacts.size(), 1u);
}

// An absent field stays absent, so the caller leaves the device's current value
// alone (client §5.5). Decoding it to a default would let a server that stopped
// sending max_letter_chars silently reset every device to 500 — a config change
// nobody made and no log records.
TEST(Wire, AbsentConfigFieldsStayAbsent) {
  Response r;
  ASSERT_TRUE(DecodeResponse("{\"cursor\":\"c\",\"config\":{\"rat\":\"nbiot\"}}", r));
  ASSERT_TRUE(r.config.has_value());
  EXPECT_FALSE(r.config->max_letter_chars.has_value());
  EXPECT_FALSE(r.config->sync_interval_s.has_value());
  EXPECT_FALSE(r.config->cover.has_value());
  ASSERT_TRUE(r.config->rat.has_value());
  EXPECT_EQ(*r.config->rat, "nbiot");
}

// A wrong-typed field the device knows is not worth refusing a whole response
// over: fall back and keep the letters moving.
TEST(Wire, WrongTypedKnownFieldsFallBack) {
  Response r;
  ASSERT_TRUE(DecodeResponse(
      "{\"cursor\":\"c\",\"server_time\":\"soon\",\"more\":\"yes\",\"pututu_counter\":null}", r));
  EXPECT_EQ(r.server_time, 0);
  EXPECT_FALSE(r.more);
  EXPECT_EQ(r.pututu_counter, 0u);
}

// These are bytes that arrived from the road. None of them may crash, hang, or
// half-apply: the device retries the identical request instead (server §4.1).
TEST(Wire, MalformedInputIsRejectedWithoutCrashing) {
  const char* const bad[] = {
      "",
      "   ",
      "not json at all",
      "{",
      "[]",
      "null",
      "\"a string\"",
      "12345",
      "{\"cursor\":\"c\",",
      "{\"server_time\":1}",                                 // no cursor
      "{\"cursor\":7}",                                      // cursor not a string
      "{\"cursor\":\"c\",\"acks\":[{\"status\":\"sent\"}]}",  // ack without a local_id
      "{\"cursor\":\"c\",\"acks\":[{\"local_id\":\"\",\"status\":\"sent\"}]}",
      "{\"cursor\":\"c\",\"acks\":[42]}",
      "{\"cursor\":\"c\",\"letters\":[{\"body\":\"b\"}]}",  // letter without an id
      "{\"cursor\":\"c\",\"letters\":[{\"id\":\"\"}]}",
      "{\"cursor\":\"c\",\"letters\":[\"l-1\"]}",
      "{\"cursor\":\"c\",\"ayllu\":{\"version\":2,\"contacts\":[{\"name\":\"Rosa\"}]}}",
  };
  for (const char* json : bad) {
    Response r;
    EXPECT_FALSE(DecodeResponse(json, r)) << json;
  }
}

// Rejection must be total: a response that fails validation leaves the output
// untouched, so a caller that ignores the return value cannot half-apply it.
TEST(Wire, RejectedResponseLeavesOutputUntouched) {
  Response r;
  ASSERT_TRUE(DecodeResponse(ReadFixture("response_letters.json"), r));
  const std::size_t before = r.letters.size();
  EXPECT_FALSE(DecodeResponse("{\"cursor\":\"c2\",\"letters\":[{\"body\":\"x\"}]}", r));
  EXPECT_EQ(r.cursor, "b64cursorDDDD");
  EXPECT_EQ(r.letters.size(), before);
}

// The body is a byte range, not a C string: a NUL in the middle must not
// truncate the parse at it. A strlen-based parser would see only
// {"cursor":"c", here, reject the response, and drop letters the server
// actually sent.
TEST(Wire, EmbeddedNulDoesNotTruncateTheParse) {
  std::string json = "{\"cursor\":\"c\",";
  json.push_back('\0');
  json += "\"more\":true}";
  Response r;
  ASSERT_TRUE(DecodeResponse(json, r));
  EXPECT_TRUE(r.more);
  EXPECT_EQ(r.cursor, "c");
}

// The wire's own caps, mirrored from internal/protocol. They change on the
// server or not at all.
TEST(Wire, MirroredCapsMatchTheServer) {
  EXPECT_EQ(chaski::wire::kMaxKipuBytes, 512);
  EXPECT_EQ(chaski::wire::kMaxSubjectGraphemes, 100);
  EXPECT_EQ(chaski::wire::kMaxLocalIdBytes, 32);
  EXPECT_STREQ(chaski::wire::kSysContactId, "c_sys");
}

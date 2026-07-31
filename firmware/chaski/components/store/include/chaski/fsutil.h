// Package fsutil is the write discipline every durable component on this
// device shares: atomic replacement and a tiny line record format.
//
// It lives in components/store because the store is its primary user, and it
// is exported rather than private because components/ayllu needs exactly the
// same guarantees for the snapshot and the overlay (client §4.4). It is the
// device's counterpart to the server's internal/atomicfile.
//
// Portability rule (implementation-plan ground rule 3): POSIX only. No esp_*,
// no FreeRTOS. On the target these calls land on LittleFS through the IDF VFS;
// in host tests they land in a temp directory.
//
// No exceptions, no RTTI: every function reports failure through its return
// value, because a device on a road cannot unwind out of a bad flash write.
#pragma once

#include <cstdint>
#include <string>
#include <string_view>
#include <utility>
#include <vector>

namespace chaski::fsutil {

// WriteAtomic replaces `path` with `data` or leaves the previous version
// entirely intact: temp file in the same directory, fsync, rename, fsync the
// directory (client §4). A power cut mid-write is the normal case on a device
// whose battery can die at any moment, so a half-written file where the
// previous version used to be is not an acceptable outcome (client §4.1).
bool WriteAtomic(const std::string& path, const std::string& data);

// AppendLine adds one newline-terminated line and fsyncs. It is the one write
// that is not a full replacement, and it is safe for the same reason: a torn
// append can only damage the line being written, never the lines already
// there. Readers drop an unterminated final line (see SplitCompleteLines).
// The alternative — rewriting the whole seen-ring on every letter — costs a
// 12 KB flash write per letter, and flash writes are the thing this device
// spends its battery and its wear budget on (design §6.4).
bool AppendLine(const std::string& path, const std::string& line);

bool ReadAll(const std::string& path, std::string& out);
bool Remove(const std::string& path);
bool Exists(const std::string& path);
bool MkdirAll(const std::string& path);

// ListNames returns the directory's entries, excluding "." , ".." and any
// dotfile — which is what WriteAtomic's temp files are named, so a crash
// during a write can never make a partial file look like a stored letter.
bool ListNames(const std::string& dir, std::vector<std::string>& out);

// NameIsSafe gates every string that becomes a path component. Letter ids and
// contact ids arrive from the server (server §4.5) and are therefore untrusted
// input: without this a crafted id containing "/" or ".." would write outside
// the store. Conservative by construction — the real ids are "l-" plus hex.
bool NameIsSafe(std::string_view name);

// Join concatenates a directory and a name with a single separator.
std::string Join(const std::string& dir, const std::string& name);

// A Record is an ordered list of key/value pairs, serialised one per line as
// "key=value". Values carry arbitrary bytes — a letter body has newlines in it
// — so they are escaped; keys are compile-time constants and are not. Repeated
// keys are preserved in order, which is how the ayllu stores a contact list
// without needing a nested format.
//
// This is deliberately not JSON: the device already links cJSON for the wire,
// but these files are private to the device, are written on a battery budget,
// and must survive being read back by a parser that cannot throw.
using Record = std::vector<std::pair<std::string, std::string>>;

std::string EncodeRecord(const Record& r);
Record DecodeRecord(std::string_view text);

// Find returns the first value for `key`, or nullptr. A missing key is never
// an error at this layer: the caller decides what a default means.
const std::string* Find(const Record& r, std::string_view key);

// Convenience readers that leave `out` untouched and return false when the key
// is absent or malformed. Nothing here throws; std::stoi does.
bool ReadInt(const Record& r, std::string_view key, int& out);
bool ReadI64(const Record& r, std::string_view key, std::int64_t& out);
bool ReadU64(const Record& r, std::string_view key, std::uint64_t& out);
bool ReadBool(const Record& r, std::string_view key, bool& out);

// SplitCompleteLines returns only newline-terminated lines. An unterminated
// tail is the signature of a power cut during AppendLine and is discarded:
// half a letter id is not a letter id.
std::vector<std::string> SplitCompleteLines(std::string_view text);

}  // namespace chaski::fsutil

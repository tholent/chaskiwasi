// Implementation of the §14 framing codec. See include/chaski/frame.h for the
// wire layout and the reasoning behind it.
#include "chaski/frame.h"

#include <cstring>

namespace chaski::transport::frame {
namespace {

void PutU16(std::string* out, std::uint16_t v) {
  out->push_back(static_cast<char>((v >> 8) & 0xFF));
  out->push_back(static_cast<char>(v & 0xFF));
}

void PutU32(std::string* out, std::uint32_t v) {
  out->push_back(static_cast<char>((v >> 24) & 0xFF));
  out->push_back(static_cast<char>((v >> 16) & 0xFF));
  out->push_back(static_cast<char>((v >> 8) & 0xFF));
  out->push_back(static_cast<char>(v & 0xFF));
}

std::uint16_t GetU16(const std::uint8_t* p) {
  return static_cast<std::uint16_t>((static_cast<std::uint16_t>(p[0]) << 8) | p[1]);
}

std::uint32_t GetU32(const std::uint8_t* p) {
  return (static_cast<std::uint32_t>(p[0]) << 24) | (static_cast<std::uint32_t>(p[1]) << 16) |
         (static_cast<std::uint32_t>(p[2]) << 8) | static_cast<std::uint32_t>(p[3]);
}

const std::uint8_t* Bytes(const std::string& s) {
  return reinterpret_cast<const std::uint8_t*>(s.data());
}

}  // namespace

// Bitwise CRC-32/IEEE. A table would cost 1 KB of flash to save microseconds
// on a link that moves a few kilobytes per sync; the loop is the right trade
// on this device.
std::uint32_t Crc32(const std::uint8_t* data, std::size_t n) {
  std::uint32_t crc = 0xFFFFFFFFu;
  for (std::size_t i = 0; i < n; ++i) {
    crc ^= data[i];
    for (int bit = 0; bit < 8; ++bit) {
      const std::uint32_t mask = static_cast<std::uint32_t>(-static_cast<std::int32_t>(crc & 1u));
      crc = (crc >> 1) ^ (0xEDB88320u & mask);
    }
  }
  return ~crc;
}

bool Encode(Type type, const std::string& payload, std::string* out) {
  if (out == nullptr || payload.size() > kMaxPayloadBytes) return false;

  std::string covered;
  covered.reserve(5 + payload.size());
  covered.push_back(static_cast<char>(type));
  PutU32(&covered, static_cast<std::uint32_t>(payload.size()));
  covered.append(payload);

  out->reserve(out->size() + kOverheadBytes + payload.size());
  PutU32(out, kMagic);
  out->append(covered);
  PutU32(out, Crc32(Bytes(covered), covered.size()));
  return true;
}

void Decoder::Append(const std::uint8_t* data, std::size_t n) {
  if (data == nullptr || n == 0) return;
  buf_.append(reinterpret_cast<const char*>(data), n);
}

void Decoder::Reset() {
  buf_.clear();
  resyncs_ = 0;
}

DecodeStatus Decoder::Next(Type* type, std::string* payload) {
  for (;;) {
    // Find a candidate frame start. Everything before it is line noise, the
    // tail of a frame that was cut off by a reset, or a payload we already
    // walked past — all of it is discarded, never interpreted.
    std::size_t start = std::string::npos;
    for (std::size_t i = 0; i + 4 <= buf_.size(); ++i) {
      if (GetU32(Bytes(buf_) + i) == kMagic) {
        start = i;
        break;
      }
    }
    if (start == std::string::npos) {
      // Keep only the bytes that could still be the head of a magic.
      if (buf_.size() > 3) {
        buf_.erase(0, buf_.size() - 3);
        ++resyncs_;
      }
      return DecodeStatus::kNeedMoreBytes;
    }
    if (start > 0) {
      buf_.erase(0, start);
      ++resyncs_;
    }

    if (buf_.size() < kHeaderBytes) return DecodeStatus::kNeedMoreBytes;

    const std::uint32_t declared = GetU32(Bytes(buf_) + 5);
    if (declared > kMaxPayloadBytes) {
      // Either a corrupted header or a magic that was never a frame start.
      // Drop one byte so the scan resumes *inside* this candidate: dropping
      // the whole header could step over a real frame that begins in it.
      buf_.erase(0, 1);
      ++resyncs_;
      continue;
    }

    const std::size_t total = kHeaderBytes + declared + kTrailerBytes;
    if (buf_.size() < total) return DecodeStatus::kNeedMoreBytes;

    const std::size_t covered = 5 + declared;  // type + length + payload
    const std::uint32_t want = GetU32(Bytes(buf_) + kHeaderBytes + declared);
    if (Crc32(Bytes(buf_) + 4, covered) != want) {
      buf_.erase(0, 1);
      ++resyncs_;
      continue;
    }

    if (type != nullptr) *type = static_cast<Type>(static_cast<std::uint8_t>(buf_[4]));
    if (payload != nullptr) payload->assign(buf_, kHeaderBytes, declared);
    buf_.erase(0, total);
    return DecodeStatus::kFrame;
  }
}

bool EncodeRequest(const RequestPayload& in, std::string* out) {
  if (out == nullptr || in.authorization.size() > 0xFFFFu) return false;
  out->clear();
  PutU16(out, in.seq);
  PutU16(out, static_cast<std::uint16_t>(in.authorization.size()));
  out->append(in.authorization);
  out->append(in.body);
  return out->size() <= kMaxPayloadBytes;
}

bool DecodeRequest(const std::string& in, RequestPayload* out) {
  constexpr std::size_t kFixed = 4;
  if (out == nullptr || in.size() < kFixed) return false;
  out->seq = GetU16(Bytes(in));
  const std::size_t auth_len = GetU16(Bytes(in) + 2);
  if (kFixed + auth_len > in.size()) return false;
  out->authorization.assign(in, kFixed, auth_len);
  out->body.assign(in, kFixed + auth_len, in.size() - kFixed - auth_len);
  return true;
}

bool EncodeResponse(const ResponsePayload& in, std::string* out) {
  if (out == nullptr || in.retry_after.size() > 0xFFFFu) return false;
  out->clear();
  PutU16(out, in.seq);
  out->push_back(static_cast<char>(in.outcome));
  PutU16(out, in.http_status);
  PutU16(out, static_cast<std::uint16_t>(in.retry_after.size()));
  out->append(in.retry_after);
  out->append(in.body);
  return out->size() <= kMaxPayloadBytes;
}

bool DecodeResponse(const std::string& in, ResponsePayload* out) {
  constexpr std::size_t kFixed = 7;
  if (out == nullptr || in.size() < kFixed) return false;
  out->seq = GetU16(Bytes(in));
  const std::uint8_t outcome = static_cast<std::uint8_t>(in[2]);
  // An outcome this build does not know is not a licence to guess: treat it as
  // "no response arrived", which is the safe reading — letters wait in the
  // outbox and nothing is acked (§5.3, D-5).
  out->outcome = outcome <= static_cast<std::uint8_t>(WireOutcome::kTlsTrustFail)
                     ? static_cast<WireOutcome>(outcome)
                     : WireOutcome::kTransportFail;
  out->http_status = GetU16(Bytes(in) + 3);
  const std::size_t ra_len = GetU16(Bytes(in) + 5);
  if (kFixed + ra_len > in.size()) return false;
  out->retry_after.assign(in, kFixed, ra_len);
  out->body.assign(in, kFixed + ra_len, in.size() - kFixed - ra_len);
  return true;
}

int ParseRetryAfterSeconds(const std::string& header_value) {
  std::size_t i = 0;
  while (i < header_value.size() && (header_value[i] == ' ' || header_value[i] == '\t')) ++i;
  if (i >= header_value.size()) return 0;

  long long v = 0;
  std::size_t digits = 0;
  for (; i < header_value.size(); ++i, ++digits) {
    const char c = header_value[i];
    if (c < '0' || c > '9') break;
    v = v * 10 + (c - '0');
    // Clamp rather than overflow. A server asking for a decade is a server
    // that will be asked again at the next scheduled wake either way.
    if (v > 86400) v = 86400;
  }
  if (digits == 0) return 0;

  // Trailing non-space after the digits means this was not delta-seconds — an
  // HTTP-date starts with a weekday, but "3 days" or a malformed value must
  // not be read as 3 seconds.
  while (i < header_value.size() && (header_value[i] == ' ' || header_value[i] == '\t')) ++i;
  if (i != header_value.size()) return 0;

  return static_cast<int>(v);
}

}  // namespace chaski::transport::frame

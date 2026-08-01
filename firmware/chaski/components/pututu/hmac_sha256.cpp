// FIPS 180-4 SHA-256 and RFC 2104 HMAC — see hmac_sha256.h for why they live
// here instead of coming from a platform library.
#include "hmac_sha256.h"

#include <algorithm>
#include <cstring>

namespace chaski::pututu::crypto {
namespace {

constexpr std::uint32_t kRoundConstants[64] = {
    0x428a2f98u, 0x71374491u, 0xb5c0fbcfu, 0xe9b5dba5u, 0x3956c25bu, 0x59f111f1u,
    0x923f82a4u, 0xab1c5ed5u, 0xd807aa98u, 0x12835b01u, 0x243185beu, 0x550c7dc3u,
    0x72be5d74u, 0x80deb1feu, 0x9bdc06a7u, 0xc19bf174u, 0xe49b69c1u, 0xefbe4786u,
    0x0fc19dc6u, 0x240ca1ccu, 0x2de92c6fu, 0x4a7484aau, 0x5cb0a9dcu, 0x76f988dau,
    0x983e5152u, 0xa831c66du, 0xb00327c8u, 0xbf597fc7u, 0xc6e00bf3u, 0xd5a79147u,
    0x06ca6351u, 0x14292967u, 0x27b70a85u, 0x2e1b2138u, 0x4d2c6dfcu, 0x53380d13u,
    0x650a7354u, 0x766a0abbu, 0x81c2c92eu, 0x92722c85u, 0xa2bfe8a1u, 0xa81a664bu,
    0xc24b8b70u, 0xc76c51a3u, 0xd192e819u, 0xd6990624u, 0xf40e3585u, 0x106aa070u,
    0x19a4c116u, 0x1e376c08u, 0x2748774cu, 0x34b0bcb5u, 0x391c0cb3u, 0x4ed8aa4au,
    0x5b9cca4fu, 0x682e6ff3u, 0x748f82eeu, 0x78a5636fu, 0x84c87814u, 0x8cc70208u,
    0x90befffau, 0xa4506cebu, 0xbef9a3f7u, 0xc67178f2u};

std::uint32_t Ror(std::uint32_t x, unsigned n) {
  return (x >> n) | (x << (32u - n));
}

struct Context {
  std::uint32_t h[8] = {0x6a09e667u, 0xbb67ae85u, 0x3c6ef372u, 0xa54ff53au,
                        0x510e527fu, 0x9b05688cu, 0x1f83d9abu, 0x5be0cd19u};
  std::uint64_t total = 0;  // message bytes consumed, for the length field
  std::uint8_t block[kSha256BlockBytes] = {};
  std::size_t filled = 0;
};

void Compress(Context& c, const std::uint8_t* p) {
  std::uint32_t w[64];
  for (std::size_t i = 0; i < 16; ++i) {
    w[i] = (static_cast<std::uint32_t>(p[i * 4]) << 24) |
           (static_cast<std::uint32_t>(p[i * 4 + 1]) << 16) |
           (static_cast<std::uint32_t>(p[i * 4 + 2]) << 8) |
           static_cast<std::uint32_t>(p[i * 4 + 3]);
  }
  for (std::size_t i = 16; i < 64; ++i) {
    const std::uint32_t s0 = Ror(w[i - 15], 7) ^ Ror(w[i - 15], 18) ^ (w[i - 15] >> 3);
    const std::uint32_t s1 = Ror(w[i - 2], 17) ^ Ror(w[i - 2], 19) ^ (w[i - 2] >> 10);
    w[i] = w[i - 16] + s0 + w[i - 7] + s1;
  }

  std::uint32_t a = c.h[0], b = c.h[1], cc = c.h[2], d = c.h[3];
  std::uint32_t e = c.h[4], f = c.h[5], g = c.h[6], h = c.h[7];
  for (std::size_t i = 0; i < 64; ++i) {
    const std::uint32_t s1 = Ror(e, 6) ^ Ror(e, 11) ^ Ror(e, 25);
    const std::uint32_t ch = (e & f) ^ (~e & g);
    const std::uint32_t t1 = h + s1 + ch + kRoundConstants[i] + w[i];
    const std::uint32_t s0 = Ror(a, 2) ^ Ror(a, 13) ^ Ror(a, 22);
    const std::uint32_t maj = (a & b) ^ (a & cc) ^ (b & cc);
    const std::uint32_t t2 = s0 + maj;
    h = g;
    g = f;
    f = e;
    e = d + t1;
    d = cc;
    cc = b;
    b = a;
    a = t1 + t2;
  }
  c.h[0] += a;
  c.h[1] += b;
  c.h[2] += cc;
  c.h[3] += d;
  c.h[4] += e;
  c.h[5] += f;
  c.h[6] += g;
  c.h[7] += h;
}

void Update(Context& c, const std::uint8_t* data, std::size_t len) {
  c.total += len;
  while (len > 0) {
    const std::size_t take = std::min(kSha256BlockBytes - c.filled, len);
    std::memcpy(c.block + c.filled, data, take);
    c.filled += take;
    data += take;
    len -= take;
    if (c.filled == kSha256BlockBytes) {
      Compress(c, c.block);
      c.filled = 0;
    }
  }
}

void Final(Context& c, std::uint8_t out[kSha256DigestBytes]) {
  const std::uint64_t bits = c.total * 8;
  const std::uint8_t pad = 0x80;
  Update(c, &pad, 1);
  const std::uint8_t zero = 0;
  while (c.filled != kSha256BlockBytes - 8) Update(c, &zero, 1);

  std::uint8_t len_be[8];
  for (std::size_t i = 0; i < 8; ++i) {
    len_be[i] = static_cast<std::uint8_t>(bits >> (56 - 8 * i));
  }
  // Written straight into the block: Update would count these eight bytes
  // into total, which is already frozen in `bits`.
  std::memcpy(c.block + c.filled, len_be, 8);
  Compress(c, c.block);

  for (std::size_t i = 0; i < 8; ++i) {
    out[i * 4] = static_cast<std::uint8_t>(c.h[i] >> 24);
    out[i * 4 + 1] = static_cast<std::uint8_t>(c.h[i] >> 16);
    out[i * 4 + 2] = static_cast<std::uint8_t>(c.h[i] >> 8);
    out[i * 4 + 3] = static_cast<std::uint8_t>(c.h[i]);
  }
}

constexpr char kBase64Alphabet[] =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";

}  // namespace

void Sha256(const std::uint8_t* data, std::size_t len, std::uint8_t out[kSha256DigestBytes]) {
  Context c;
  Update(c, data, len);
  Final(c, out);
}

void HmacSha256(const std::uint8_t* key, std::size_t key_len,
                const std::uint8_t* data, std::size_t data_len,
                std::uint8_t out[kSha256DigestBytes]) {
  std::uint8_t k[kSha256BlockBytes] = {};
  if (key_len > kSha256BlockBytes) {
    Sha256(key, key_len, k);
  } else if (key_len > 0) {
    std::memcpy(k, key, key_len);
  }

  std::uint8_t pad[kSha256BlockBytes];
  for (std::size_t i = 0; i < kSha256BlockBytes; ++i) pad[i] = k[i] ^ 0x36;
  Context inner;
  Update(inner, pad, kSha256BlockBytes);
  Update(inner, data, data_len);
  std::uint8_t inner_digest[kSha256DigestBytes];
  Final(inner, inner_digest);

  for (std::size_t i = 0; i < kSha256BlockBytes; ++i) pad[i] = k[i] ^ 0x5c;
  Context outer;
  Update(outer, pad, kSha256BlockBytes);
  Update(outer, inner_digest, kSha256DigestBytes);
  Final(outer, out);

  // The padded key is the only copy of the secret this function owns, and it
  // outlives nothing (D-4's spirit: a secret at rest anywhere is a secret that
  // can leak). volatile so the write is not optimised away.
  volatile std::uint8_t* wipe = k;
  for (std::size_t i = 0; i < kSha256BlockBytes; ++i) wipe[i] = 0;
}

std::string Base64(const std::uint8_t* data, std::size_t len) {
  std::string out;
  out.reserve(((len + 2) / 3) * 4);
  std::size_t i = 0;
  for (; i + 3 <= len; i += 3) {
    const std::uint32_t v = (static_cast<std::uint32_t>(data[i]) << 16) |
                            (static_cast<std::uint32_t>(data[i + 1]) << 8) |
                            static_cast<std::uint32_t>(data[i + 2]);
    out.push_back(kBase64Alphabet[(v >> 18) & 0x3f]);
    out.push_back(kBase64Alphabet[(v >> 12) & 0x3f]);
    out.push_back(kBase64Alphabet[(v >> 6) & 0x3f]);
    out.push_back(kBase64Alphabet[v & 0x3f]);
  }
  const std::size_t rest = len - i;
  if (rest == 1) {
    const std::uint32_t v = static_cast<std::uint32_t>(data[i]) << 16;
    out.push_back(kBase64Alphabet[(v >> 18) & 0x3f]);
    out.push_back(kBase64Alphabet[(v >> 12) & 0x3f]);
    out.push_back('=');
    out.push_back('=');
  } else if (rest == 2) {
    const std::uint32_t v = (static_cast<std::uint32_t>(data[i]) << 16) |
                            (static_cast<std::uint32_t>(data[i + 1]) << 8);
    out.push_back(kBase64Alphabet[(v >> 18) & 0x3f]);
    out.push_back(kBase64Alphabet[(v >> 12) & 0x3f]);
    out.push_back(kBase64Alphabet[(v >> 6) & 0x3f]);
    out.push_back('=');
  }
  return out;
}

bool Equals(const std::string& a, const std::string& b) {
  if (a.size() != b.size()) return false;
  unsigned char diff = 0;
  for (std::size_t i = 0; i < a.size(); ++i) {
    diff |= static_cast<unsigned char>(a[i]) ^ static_cast<unsigned char>(b[i]);
  }
  return diff == 0;
}

}  // namespace chaski::pututu::crypto

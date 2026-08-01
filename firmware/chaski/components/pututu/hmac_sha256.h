// SHA-256, HMAC-SHA256 and base64 for the doorbell token (server §10.2).
//
// Private to components/pututu: quoted include, no entry in include/, nothing
// else in the tree may grow a second caller without moving it deliberately.
//
// Why this is vendored rather than taken from mbedTLS on the target and
// something else on the host: the device's verifier and the server's minter
// must agree byte for byte, and the only way the host tier can prove that is
// to compile the SAME code the device runs. Two builds of two different MAC
// implementations is precisely the skew F-C3 (cJSON header spelling) and F-C8
// (utf8proc) already cost this project once each; utf8proc is vendored for the
// same reason. The primitives here are FIPS 180-4 and RFC 2104 exactly, with
// no options, no modes, and no key handling beyond the standard block padding
// — and they are checked against RFC 4231 vectors and against tokens minted by
// the server's own Go code (C-8).
#pragma once

#include <cstddef>
#include <cstdint>
#include <string>

namespace chaski::pututu::crypto {

inline constexpr std::size_t kSha256DigestBytes = 32;
inline constexpr std::size_t kSha256BlockBytes = 64;

// Sha256 writes the digest of [data, data+len) to out.
void Sha256(const std::uint8_t* data, std::size_t len,
            std::uint8_t out[kSha256DigestBytes]);

// HmacSha256 is RFC 2104 with SHA-256: keys longer than the block are hashed
// first, shorter ones zero-padded.
void HmacSha256(const std::uint8_t* key, std::size_t key_len,
                const std::uint8_t* data, std::size_t data_len,
                std::uint8_t out[kSha256DigestBytes]);

// Base64 is RFC 4648 standard alphabet with padding — what Go's
// base64.StdEncoding emits, which is what the server MACs with.
std::string Base64(const std::uint8_t* data, std::size_t len);

// Equals compares in time independent of where the difference is. The MAC is
// the only thing standing between a stranger with the device's number and a
// wake, so it is compared the way a MAC is compared.
bool Equals(const std::string& a, const std::string& b);

}  // namespace chaski::pututu::crypto

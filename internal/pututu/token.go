package pututu

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
)

// tokenPrefix is the fixed token version tag (§10.2). It must match
// tools/chaskisim's pututuPrefix exactly, byte for byte — see
// TestCrossVerify_MatchesChaskisim, which exists specifically because a
// silent drift here means the doorbell never rings on a real device and
// nothing looks broken (A.8).
const tokenPrefix = "CH1"

// macBytes is how many bytes of the HMAC-SHA256 digest are carried,
// base64-encoded (§10.2: "the first 12 bytes ... base64 of").
const macBytes = 12

// Token builds a CH1.<counter>.<mac> doorbell token (§10.2): the ASCII
// decimal counter, HMAC-SHA256'd with the pututu key, truncated to the first
// 12 bytes and base64-encoded. Its only inputs are an integer and a key, so
// there is nothing here that could carry a sender name or letter content —
// the "opaque doorbell" requirement (§10.2) holds by construction, not by
// convention.
//
// Exported so tests — most importantly the cross-verification against
// tools/chaskisim.MintPututuToken, the device-side implementation this
// server must agree with — can mint a token without spending a real counter
// value from state.Store. Production sending goes through mintToken, which
// wraps this with the stateful counter increment (§10.2's "incremented per
// SMS sent").
func Token(counter uint64, key []byte) string {
	return tokenPrefix + "." + strconv.FormatUint(counter, 10) + "." + computeMAC(counter, key)
}

func computeMAC(counter uint64, key []byte) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(strconv.FormatUint(counter, 10)))
	sum := h.Sum(nil)
	return base64.StdEncoding.EncodeToString(sum[:macBytes])
}

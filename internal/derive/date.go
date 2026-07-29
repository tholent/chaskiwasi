package derive

import "time"

// Bounds for §4.3's "mailbox Date header, sanity-checked against
// INTERNALDATE". The Date header is written by the sender's own mail client
// and cannot be trusted on its own, but discarding it whenever it merely
// lags INTERNALDATE — the normal case, since Date is stamped at send time
// and INTERNALDATE at receipt, and mail can sit in a queue for a while —
// would make every ordinary letter show its arrival time instead of when it
// was actually written. So the check is deliberately asymmetric:
//
//   - A Date more than maxFutureSkew after INTERNALDATE is impossible under
//     any honest clock (nothing is received before it's sent) and is always
//     treated as a broken sender clock.
//   - A Date more than maxPastSkew before INTERNALDATE is allowed to be a
//     slow-delivery artifact up to a month back — a provider outage or a
//     stuck queue can genuinely be that slow — but beyond it, it reads as a
//     stuck or garbage clock (a year-zero bug, an epoch-adjacent default on
//     a misconfigured sender) rather than a real letter that took a month in
//     transit.
const (
	maxFutureSkew = 24 * time.Hour
	maxPastSkew   = 30 * 24 * time.Hour
)

// sanityCheckedDate implements the rule above. headerOK reports whether the
// Date header was present and parseable at all; when it is not, or when the
// parsed value falls outside the window, internal (the mailbox's own
// INTERNALDATE) is used instead. internal is server-observed and cannot be
// forged by the sender, which is what makes it the safe fallback truth
// rather than just "some other guess."
func sanityCheckedDate(header time.Time, headerOK bool, internal time.Time) time.Time {
	if !headerOK {
		return internal
	}
	skew := header.Sub(internal)
	if skew > maxFutureSkew || skew < -maxPastSkew {
		return internal
	}
	return header
}

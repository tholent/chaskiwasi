// Package chaskisim simulates a Chaski device: enough of the firmware's side
// of wasi-server-plan §4's wire contract to drive the e2e suite (§15) and
// make the whole system demonstrable with zero hardware. It speaks the real
// wire types from internal/protocol directly — never a private copy — so a
// change to the contract cannot be made on one side without the other
// noticing.
//
// This package is a library, imported by test/e2e for tight, in-process
// control over a simulated device's state across a whole scenario (multiple
// syncs, restarts, factory resets). tools/chaskisim/cmd/chaskisim wraps it in
// a small CLI for interactive, zero-hardware demonstration of the same
// behaviour.
//
// The firmware requirements the wire contract imposes (§4, §10), each
// modelled here because the e2e suite tests them:
//
//   - The cursor is stored and echoed verbatim, never parsed (§4.4). See
//     State.Cursor's doc comment for why this matters more than it looks
//     like it should.
//   - At least the 1000 most recently seen letter ids are remembered and
//     repeats are silently dropped (§4.5), persisted across restarts so
//     V-21 (UIDVALIDITY reset -> window resync -> no duplicates) is
//     actually testable.
//   - On more: true, the device syncs again immediately, looping until
//     false, hard-capped at 10 rounds per wake (§4.6). See Wake.
//   - Every ack is terminal: on any ack the letter leaves the outbox and is
//     never resent (§4.7). See Device.applyResponse.
//   - pututu_counter_seen is tracked and reported every sync (§10.3); an
//     incoming CH1.<counter>.<mac> token is verified, accepted only on a
//     strictly greater counter, persisted across power loss, and failures
//     are ignored silently (§10.2). See AcceptPututu and the pututu.go file.
//   - Rendering line-breaks only on grapheme-cluster boundaries, never
//     inside one (§4.9) — the one place this simulator can demonstrate that
//     the wire contract's rendering requirement is satisfiable. See Wrap.
package chaskisim

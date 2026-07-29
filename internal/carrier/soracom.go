package carrier

import "fmt"

// Soracom is left unimplemented for v1 (§10.4). Soracom keys device identity
// on an IMSI or SIM id rather than Hologram's device id — exactly the
// disagreement the [carrier] registry indirection exists to absorb — but no
// client has been written against it yet. A half-written client that sends
// nothing correctly is worse than an honest, clearly-labelled gap: New
// routes the name to newSoracom below, which fails loudly and says so,
// rather than silently constructing something that would compile but never
// actually ring a device.
//
// Adding it later needs no Carrier interface change: implement Name,
// Pututu, and Balance against Soracom's Beam/SORACOM API the way Hologram.go
// does, wire its option keys (likely "sim_id") into New, and run it through
// carriertest.
func newSoracom(apiKey string, options map[string]any) (Carrier, error) {
	return nil, fmt.Errorf("carrier: soracom: not implemented (§10.4)")
}

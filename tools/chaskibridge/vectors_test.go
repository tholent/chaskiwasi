package chaskibridge

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"hash/crc32"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Shared framing vectors, generated here and parsed by the firmware's host
// tests (test/firmware/host/transport). The argument is tools/graphvectors'
// argument for graphemes (B.7): two implementations of one wire that are only
// reviewed against each other drift, and the drift is silent until a device in
// a pocket stops understanding its bridge. Generating the bytes on one side
// and parsing them on the other means a divergence fails a test instead.
//
// Regenerate after any change to the frame layout, and commit the result:
//
//	go test ./tools/chaskibridge -run TestC1_FrameVectors -update
//
// Both suites then run against the same bytes with no toolchain shared.

var updateVectors = flag.Bool("update", false, "rewrite the committed framing vectors")

const vectorsPath = "test/firmware/host/testdata/frames/vectors.json"

// vectorFile is the committed document. Bytes are hex because the consumers
// are a Go test and a C++ test with cJSON, and hex is the one encoding both
// read without a library decision.
type vectorFile struct {
	Note    string         `json:"note"`
	Magic   string         `json:"magic"`
	Frames  []frameVector  `json:"frames"`
	Streams []streamVector `json:"streams"`
}

// frameVector is one framed message: the structured payload, the payload
// bytes, and the whole frame. A parser is checked against all three, so a
// disagreement is localised to the layer that has it.
type frameVector struct {
	Name       string          `json:"name"`
	Why        string          `json:"why"`
	Type       uint8           `json:"type"`
	PayloadHex string          `json:"payload_hex"`
	FrameHex   string          `json:"frame_hex"`
	CRC32      uint32          `json:"crc32"`
	Request    *requestVector  `json:"request,omitempty"`
	Response   *responseVector `json:"response,omitempty"`
}

type requestVector struct {
	Seq           uint16 `json:"seq"`
	Authorization string `json:"authorization"`
	BodyHex       string `json:"body_hex"`
}

type responseVector struct {
	Seq        uint16 `json:"seq"`
	Outcome    uint8  `json:"outcome"`
	HTTPStatus uint16 `json:"http_status"`
	RetryAfter string `json:"retry_after"`
	BodyHex    string `json:"body_hex"`
}

// streamVector is a byte stream a decoder must survive: the frames it must
// find, and the resync count it must reach doing so. These are the cases the
// magic and the CRC exist for.
type streamVector struct {
	Name      string        `json:"name"`
	Why       string        `json:"why"`
	StreamHex string        `json:"stream_hex"`
	Expect    []expectFrame `json:"expect"`
	Resyncs   int           `json:"resyncs"`
	// DiscardedHex is what a host-side reader routes to its console capture:
	// on the real link, the device's serial log (D-7, C-19). The firmware
	// decoder drops these bytes and ignores this field.
	DiscardedHex string `json:"discarded_hex"`
}

type expectFrame struct {
	Type       uint8  `json:"type"`
	PayloadHex string `json:"payload_hex"`
}

func mustFrame(t *testing.T, ft FrameType, payload []byte) []byte {
	t.Helper()
	f, err := EncodeFrame(ft, payload)
	if err != nil {
		t.Fatalf("encoding a %s frame: %v", ft, err)
	}
	return f
}

func buildVectors(t *testing.T) vectorFile {
	t.Helper()

	// Request: a normal sync with a bearer and a small body.
	reqPayload, err := EncodeRequestPayload(RequestPayload{
		Seq:           1,
		Authorization: "Bearer dev-device-token",
		Body:          []byte(`{"cursor":"b64cursorAAAA","ayllu_version":7}`),
	})
	if err != nil {
		t.Fatalf("encoding a request payload: %v", err)
	}

	// Request with no bearer at all. The bridge must forward the absence, not
	// repair it: the 401 the device sees has to be the server's answer (§14).
	noAuthPayload, err := EncodeRequestPayload(RequestPayload{
		Seq:  2,
		Body: []byte(`{"cursor":""}`),
	})
	if err != nil {
		t.Fatalf("encoding a request payload: %v", err)
	}

	// Response: 200 with a body carrying multi-byte UTF-8, because a length
	// prefix that is secretly a character count is a classic way to lose the
	// last letter of a letter.
	okPayload, err := EncodeResponsePayload(ResponsePayload{
		Seq:        1,
		Outcome:    OutcomeOK,
		HTTPStatus: 200,
		Body:       []byte(`{"cursor":"b64cursorBBBB","server_time":1750000000,"letters":[{"body":"café 👨‍👩‍👧"}]}`),
	})
	if err != nil {
		t.Fatalf("encoding a response payload: %v", err)
	}

	// 503 + Retry-After, verbatim as the header text (§5.3).
	busyPayload, err := EncodeResponsePayload(ResponsePayload{
		Seq:        7,
		Outcome:    OutcomeOK,
		HTTPStatus: 503,
		RetryAfter: "120",
		Body:       []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("encoding a response payload: %v", err)
	}

	// TLS trust failure: no status, no body, its own outcome (D-6).
	tlsPayload, err := EncodeResponsePayload(ResponsePayload{
		Seq:     8,
		Outcome: OutcomeTLSTrustFail,
	})
	if err != nil {
		t.Fatalf("encoding a response payload: %v", err)
	}

	// An empty payload. Zero-length is a length like any other and must not be
	// confused with "no frame".
	eventPayload := []byte{}

	frames := []frameVector{
		{
			Name:       "request_bearer",
			Why:        "the ordinary case: sequence, bearer carried verbatim, JSON body",
			Type:       uint8(FrameRequest),
			PayloadHex: hex.EncodeToString(reqPayload),
			FrameHex:   hex.EncodeToString(mustFrame(t, FrameRequest, reqPayload)),
			Request: &requestVector{
				Seq: 1, Authorization: "Bearer dev-device-token",
				BodyHex: hex.EncodeToString([]byte(`{"cursor":"b64cursorAAAA","ayllu_version":7}`)),
			},
		},
		{
			Name:       "request_no_bearer",
			Why:        "an absent Authorization must survive the round trip as absent (§14)",
			Type:       uint8(FrameRequest),
			PayloadHex: hex.EncodeToString(noAuthPayload),
			FrameHex:   hex.EncodeToString(mustFrame(t, FrameRequest, noAuthPayload)),
			Request: &requestVector{
				Seq: 2, Authorization: "",
				BodyHex: hex.EncodeToString([]byte(`{"cursor":""}`)),
			},
		},
		{
			Name:       "response_200_utf8",
			Why:        "a body of bytes, not characters: multi-byte UTF-8 must survive the length prefix",
			Type:       uint8(FrameResponse),
			PayloadHex: hex.EncodeToString(okPayload),
			FrameHex:   hex.EncodeToString(mustFrame(t, FrameResponse, okPayload)),
			Response: &responseVector{
				Seq: 1, Outcome: uint8(OutcomeOK), HTTPStatus: 200,
				BodyHex: hex.EncodeToString([]byte(`{"cursor":"b64cursorBBBB","server_time":1750000000,"letters":[{"body":"café 👨‍👩‍👧"}]}`)),
			},
		},
		{
			Name:       "response_503_retry_after",
			Why:        "Retry-After crosses as the header's verbatim text; the device parses it (§5.3)",
			Type:       uint8(FrameResponse),
			PayloadHex: hex.EncodeToString(busyPayload),
			FrameHex:   hex.EncodeToString(mustFrame(t, FrameResponse, busyPayload)),
			Response: &responseVector{
				Seq: 7, Outcome: uint8(OutcomeOK), HTTPStatus: 503, RetryAfter: "120",
				BodyHex: hex.EncodeToString([]byte(`{}`)),
			},
		},
		{
			Name:       "response_tls_trust_fail",
			Why:        "the third outcome has no status and no body, and must not read as a transport failure (D-6)",
			Type:       uint8(FrameResponse),
			PayloadHex: hex.EncodeToString(tlsPayload),
			FrameHex:   hex.EncodeToString(mustFrame(t, FrameResponse, tlsPayload)),
			Response: &responseVector{
				Seq: 8, Outcome: uint8(OutcomeTLSTrustFail), HTTPStatus: 0, RetryAfter: "",
				BodyHex: "",
			},
		},
		{
			Name:       "event_empty_payload",
			Why:        "a zero-length payload is a length like any other, not the absence of a frame",
			Type:       uint8(FrameEvent),
			PayloadHex: hex.EncodeToString(eventPayload),
			FrameHex:   hex.EncodeToString(mustFrame(t, FrameEvent, eventPayload)),
		},
	}
	for i := range frames {
		raw, err := hex.DecodeString(frames[i].FrameHex)
		if err != nil {
			t.Fatalf("%s: %v", frames[i].Name, err)
		}
		frames[i].CRC32 = crc32.ChecksumIEEE(raw[4 : len(raw)-4])
	}

	good := mustFrame(t, FrameRequest, reqPayload)
	// A payload that contains the magic. The decoder must not mistake it for a
	// frame start when scanning inside a payload it already framed correctly,
	// and must recover when it does hit it during a resync.
	magicInside, err := EncodeRequestPayload(RequestPayload{
		Seq: 9, Authorization: "Bearer x", Body: append([]byte("CHK1"), []byte(`{"a":1}`)...),
	})
	if err != nil {
		t.Fatalf("encoding a request payload: %v", err)
	}
	magicFrame := mustFrame(t, FrameRequest, magicInside)

	consoleText := []byte("I (218) chaski: sync start id=lt_0912\r\n")

	corruptCRC := append([]byte(nil), good...)
	corruptCRC[len(corruptCRC)-1] ^= 0xFF

	// A header claiming more than the cap. Nothing may be buffered on its word.
	oversized := []byte{0x43, 0x48, 0x4B, 0x31, 0x01, 0xFF, 0xFF, 0xFF, 0xFF}

	truncated := good[:len(good)-5]

	streams := []streamVector{
		{
			Name:         "console_text_then_frame",
			Why:          "the dev console shares the cable; log lines between frames are normal traffic, not corruption",
			StreamHex:    hex.EncodeToString(concat(consoleText, good)),
			Expect:       []expectFrame{{Type: uint8(FrameRequest), PayloadHex: hex.EncodeToString(reqPayload)}},
			Resyncs:      1,
			DiscardedHex: hex.EncodeToString(consoleText),
		},
		{
			Name:         "truncated_then_frame",
			Why:          "a device reset mid-frame (which is what C-4 does) must cost that frame and nothing after it",
			StreamHex:    hex.EncodeToString(concat(truncated, good)),
			Expect:       []expectFrame{{Type: uint8(FrameRequest), PayloadHex: hex.EncodeToString(reqPayload)}},
			Resyncs:      2,
			DiscardedHex: hex.EncodeToString(truncated),
		},
		{
			Name:         "bad_crc_then_frame",
			Why:          "a frame that fails its checksum is discarded, never handed up half-trusted",
			StreamHex:    hex.EncodeToString(concat(corruptCRC, good)),
			Expect:       []expectFrame{{Type: uint8(FrameRequest), PayloadHex: hex.EncodeToString(reqPayload)}},
			Resyncs:      2,
			DiscardedHex: hex.EncodeToString(corruptCRC),
		},
		{
			Name:         "oversized_length_then_frame",
			Why:          "a declared length over the 64 KB cap is rejected, not buffered on the sender's word (server §4.1)",
			StreamHex:    hex.EncodeToString(concat(oversized, good)),
			Expect:       []expectFrame{{Type: uint8(FrameRequest), PayloadHex: hex.EncodeToString(reqPayload)}},
			Resyncs:      2,
			DiscardedHex: hex.EncodeToString(oversized),
		},
		{
			Name:      "magic_inside_payload",
			Why:       "a payload containing the magic is still one frame; the CRC is what tells a real boundary from a coincidence",
			StreamHex: hex.EncodeToString(concat(magicFrame, good)),
			Expect: []expectFrame{
				{Type: uint8(FrameRequest), PayloadHex: hex.EncodeToString(magicInside)},
				{Type: uint8(FrameRequest), PayloadHex: hex.EncodeToString(reqPayload)},
			},
			Resyncs:      0,
			DiscardedHex: "",
		},
		{
			Name:      "back_to_back_frames",
			Why:       "one read can complete several frames; a decoder that returns only the first stalls the link",
			StreamHex: hex.EncodeToString(concat(good, mustFrame(t, FrameResponse, okPayload), mustFrame(t, FrameEvent, eventPayload))),
			Expect: []expectFrame{
				{Type: uint8(FrameRequest), PayloadHex: hex.EncodeToString(reqPayload)},
				{Type: uint8(FrameResponse), PayloadHex: hex.EncodeToString(okPayload)},
				{Type: uint8(FrameEvent), PayloadHex: hex.EncodeToString(eventPayload)},
			},
			Resyncs:      0,
			DiscardedHex: "",
		},
	}

	return vectorFile{
		Note: "Generated by `go test ./tools/chaskibridge -run TestC1_FrameVectors -update`. " +
			"Do not hand-edit. The host end (tools/chaskibridge) emits these; the device end " +
			"(firmware/chaski/components/transport) parses them. Client §14.",
		Magic:   "0x43484B31",
		Frames:  frames,
		Streams: streams,
	}
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestC1_FrameVectors keeps the committed vectors in step with this
// implementation, and with -update rewrites them. C-1 is the round trip these
// bytes are the shared half of.
func TestC1_FrameVectors(t *testing.T) {
	want := buildVectors(t)
	encoded, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("encoding vectors: %v", err)
	}
	encoded = append(encoded, '\n')

	path := filepath.Join(repoRoot(t), vectorsPath)
	if *updateVectors {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating the vector directory: %v", err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatalf("writing vectors: %v", err)
		}
		t.Logf("wrote %s", path)
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading committed vectors: %v (regenerate with -update)", err)
	}
	if !bytes.Equal(got, encoded) {
		t.Fatalf("%s is stale; regenerate with:\n"+
			"  go test ./tools/chaskibridge -run TestC1_FrameVectors -update", vectorsPath)
	}
}

// TestC1_FrameVectorsRoundTrip decodes every committed vector with this
// package's own decoder, so the file is checked as data and not merely as the
// encoder's output.
func TestC1_FrameVectorsRoundTrip(t *testing.T) {
	var f vectorFile
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), vectorsPath))
	if err != nil {
		t.Fatalf("reading committed vectors: %v", err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parsing committed vectors: %v", err)
	}
	if len(f.Frames) == 0 || len(f.Streams) == 0 {
		t.Fatal("the vector file is empty; regenerate it")
	}

	for _, v := range f.Frames {
		t.Run(v.Name, func(t *testing.T) {
			frame := mustHex(t, v.FrameHex)
			fr := NewFrameReader(bytes.NewReader(frame))
			gotType, gotPayload, err := fr.Next()
			if err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if uint8(gotType) != v.Type {
				t.Errorf("type = %d, want %d", gotType, v.Type)
			}
			if got := hex.EncodeToString(gotPayload); got != v.PayloadHex {
				t.Errorf("payload = %s, want %s", got, v.PayloadHex)
			}
			if fr.Resyncs() != 0 {
				t.Errorf("resyncs = %d on a clean frame, want 0", fr.Resyncs())
			}

			switch {
			case v.Request != nil:
				req, err := DecodeRequestPayload(gotPayload)
				if err != nil {
					t.Fatalf("decoding the request payload: %v", err)
				}
				if req.Seq != v.Request.Seq || req.Authorization != v.Request.Authorization ||
					hex.EncodeToString(req.Body) != v.Request.BodyHex {
					t.Errorf("request = %+v, want %+v", req, *v.Request)
				}
			case v.Response != nil:
				resp, err := DecodeResponsePayload(gotPayload)
				if err != nil {
					t.Fatalf("decoding the response payload: %v", err)
				}
				if resp.Seq != v.Response.Seq || uint8(resp.Outcome) != v.Response.Outcome ||
					resp.HTTPStatus != v.Response.HTTPStatus || resp.RetryAfter != v.Response.RetryAfter ||
					hex.EncodeToString(resp.Body) != v.Response.BodyHex {
					t.Errorf("response = %+v, want %+v", resp, *v.Response)
				}
			}
		})
	}

	for _, s := range f.Streams {
		t.Run(s.Name, func(t *testing.T) {
			var console bytes.Buffer
			fr := NewFrameReader(bytes.NewReader(mustHex(t, s.StreamHex)))
			fr.Console = &console

			for i, want := range s.Expect {
				gotType, gotPayload, err := fr.Next()
				if err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
				if uint8(gotType) != want.Type {
					t.Errorf("frame %d: type = %d, want %d", i, gotType, want.Type)
				}
				if got := hex.EncodeToString(gotPayload); got != want.PayloadHex {
					t.Errorf("frame %d: payload = %s, want %s", i, got, want.PayloadHex)
				}
			}
			if _, _, err := fr.Next(); err == nil {
				t.Error("a frame was found past the end of the vector's expectations")
			}
			if fr.Resyncs() != s.Resyncs {
				t.Errorf("resyncs = %d, want %d", fr.Resyncs(), s.Resyncs)
			}
			if got := hex.EncodeToString(console.Bytes()); got != s.DiscardedHex {
				t.Errorf("console capture = %s, want %s", got, s.DiscardedHex)
			}
		})
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex in the vector file: %v", err)
	}
	return b
}

// repoRoot locates the repository from this file's compiled-in path, so the
// tests do not depend on the working directory `go test` chose.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this source file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

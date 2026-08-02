package chaskibridge

import (
	"bytes"
	"strings"
	"testing"
)

// The shared stream vectors in vectors_test.go pin what the decoder finds and
// how often it resynchronises, against the firmware's decoder. These pin the
// host-only half of the same reader: which bytes reach the console capture, and
// whether that capture can be trusted as pure device log.

func TestFrameReaderRoutesConsoleTextOnly(t *testing.T) {
	payload, err := EncodeRequestPayload(RequestPayload{
		Seq: 1, Authorization: "Bearer dev-device-token", Body: []byte(`{"cursor":""}`),
	})
	if err != nil {
		t.Fatalf("encoding a request payload: %v", err)
	}
	frame, err := EncodeFrame(FrameRequest, payload)
	if err != nil {
		t.Fatalf("encoding a frame: %v", err)
	}

	const log = "I (218) chaski: sync ok stored=1 acks=1\r\n"
	var console bytes.Buffer
	fr := NewFrameReader(bytes.NewReader(append([]byte(log), frame...)))
	fr.Console = &console

	ft, got, err := fr.Next()
	if err != nil {
		t.Fatalf("reading the frame: %v", err)
	}
	if ft != FrameRequest || !bytes.Equal(got, payload) {
		t.Fatalf("frame = (%s, %d bytes), want (request, %d bytes)", ft, len(got), len(payload))
	}
	if console.String() != log {
		t.Errorf("console capture = %q, want the log line only", console.String())
	}
	if fr.Torn() != 0 {
		t.Errorf("Torn() = %d on an undamaged stream, want 0", fr.Torn())
	}
}

// A torn frame is the case C-19's grep has to know about: the reader cannot
// tell it from console text, so the letter bytes it carried land in the
// capture. The count is what makes that visible instead of a phantom leak.
func TestFrameReaderCountsTornFrames(t *testing.T) {
	const secret = "we saw a fox by the river"
	payload, err := EncodeRequestPayload(RequestPayload{
		Seq: 2, Authorization: "Bearer dev-device-token", Body: []byte(`{"body":"` + secret + `"}`),
	})
	if err != nil {
		t.Fatalf("encoding a request payload: %v", err)
	}
	damaged, err := EncodeFrame(FrameRequest, payload)
	if err != nil {
		t.Fatalf("encoding a frame: %v", err)
	}
	damaged[len(damaged)-1] ^= 0xFF // the CRC, not the payload

	good, err := EncodeFrame(FrameResponse, []byte{0, 3, 0, 0, 200, 0, 0})
	if err != nil {
		t.Fatalf("encoding a frame: %v", err)
	}

	var console bytes.Buffer
	fr := NewFrameReader(bytes.NewReader(append(damaged, good...)))
	fr.Console = &console

	ft, _, err := fr.Next()
	if err != nil {
		t.Fatalf("reading past the damaged frame: %v", err)
	}
	if ft != FrameResponse {
		t.Fatalf("frame = %s, want response: a damaged frame must cost itself and nothing after it", ft)
	}
	if fr.Torn() != 1 {
		t.Fatalf("Torn() = %d after one damaged frame, want 1", fr.Torn())
	}
	// The point of the counter, stated as an assertion: this is exactly the
	// condition under which the capture is not evidence.
	if !strings.Contains(console.String(), secret) {
		t.Error("expected the damaged frame's bytes in the console capture; " +
			"if this no longer holds, Torn() and the C-19 caveat can go")
	}
}

// A false magic that declares an impossible length must not make the reader
// buffer on the sender's word, and must not be counted as a torn frame — the
// CRC was never reached, so nothing about a real frame was lost.
func TestFrameReaderIgnoresOversizedLength(t *testing.T) {
	oversized := []byte{0x43, 0x48, 0x4B, 0x31, 0x01, 0xFF, 0xFF, 0xFF, 0xFF}
	good, err := EncodeFrame(FrameEvent, []byte(`{"ev":"hello"}`))
	if err != nil {
		t.Fatalf("encoding a frame: %v", err)
	}

	fr := NewFrameReader(bytes.NewReader(append(oversized, good...)))
	ft, got, err := fr.Next()
	if err != nil {
		t.Fatalf("reading past an oversized header: %v", err)
	}
	if ft != FrameEvent || string(got) != `{"ev":"hello"}` {
		t.Fatalf("frame = (%s, %q), want the event frame", ft, got)
	}
	if fr.Torn() != 0 {
		t.Errorf("Torn() = %d, want 0: an oversized length never reached a CRC", fr.Torn())
	}
}

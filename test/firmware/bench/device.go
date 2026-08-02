//go:build bench

// Package bench drives a real Chaski over the USB link, against a real Wasi.
//
// The harness is BOTH ends of the cable's host side at once, because the device
// only has one cable:
//
//   - it answers the device's sync requests by forwarding them to Wasi, which
//     is what tools/chaskibridge does in ordinary development (client §14);
//   - it sends bench-control commands and reads the events they answer with,
//     which is how a device with no keyboard is made to compose and sync.
//
// One read loop demultiplexes both, because both arrive as frames on the same
// wire. The device's rule is one command at a time (bench_control.h), and this
// enforces it: Do blocks until the matching event arrives, and a sync command
// stays outstanding while the device's own request frames are being served.
//
// Console text shares the cable too. It is captured rather than discarded,
// because it is C-19's evidence: the dev build's serial output is greppable
// proof that no letter body, subject, or sender was ever logged (D-7, B.11).
package bench

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tholent/chaskiwasi/tools/chaskibridge"
)

// consoleSink funnels the frame reader's discarded bytes — the device's serial
// log — into the capture C-19 greps, under the same lock as reads of it.
type consoleSink struct{ d *Device }

func (c *consoleSink) Write(p []byte) (int, error) {
	c.d.consoleMu.Lock()
	c.d.console.Write(p)
	c.d.consoleMu.Unlock()
	return len(p), nil
}

// Device is one attached board.
type Device struct {
	port io.ReadWriteCloser
	wasi string
	hc   *http.Client

	mu      sync.Mutex
	nextID  int
	pending map[int]chan map[string]any
	hello   chan map[string]any

	consoleMu     sync.Mutex
	console       strings.Builder
	consoleWriter consoleSink

	reader *chaskibridge.FrameReader

	errMu sync.Mutex
	err   error

	closed chan struct{}
	wg     sync.WaitGroup
}

// OpenDevice attaches to the board and starts serving it. `wasi` is the sync
// endpoint the device's requests are forwarded to.
func OpenDevice(portPath, wasi string, insecure bool) (*Device, error) {
	port, err := chaskibridge.OpenSerialWait(
		chaskibridge.Config{SerialPort: portPath}, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", portPath, err)
	}

	d := &Device{
		port:    port,
		wasi:    wasi,
		pending: make(map[int]chan map[string]any),
		hello:   make(chan map[string]any, 4),
		closed:  make(chan struct{}),
		hc: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				// The bench stack's certificate is self-signed. This relaxation
				// is the HARNESS's connection on a developer machine and says
				// nothing about the device, whose TLS terminates in the modem
				// against two pinned private CAs and is never relaxed (D-6).
				TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec
			},
		},
	}

	d.consoleWriter.d = d
	d.wg.Add(1)
	go d.readLoop()
	return d, nil
}

func (d *Device) Close() error {
	close(d.closed)
	err := d.port.Close()
	d.wg.Wait()
	return err
}

// Console returns everything the device has printed. C-19 greps this.
func (d *Device) Console() string {
	d.consoleMu.Lock()
	defer d.consoleMu.Unlock()
	return d.console.String()
}

// Torn reports candidate frames that failed their CRC. A device->host frame
// carries the child's outbound letter, so a torn one spills letter bytes into
// the console capture — which makes a C-19 grep inconclusive rather than clean.
// The test asserts this is zero before trusting its own result.
func (d *Device) Torn() int {
	if d.reader == nil {
		return 0
	}
	return d.reader.Torn()
}

func (d *Device) setErr(err error) {
	d.errMu.Lock()
	if d.err == nil {
		d.err = err
	}
	d.errMu.Unlock()
}

// Err reports the first fatal link error, if any.
func (d *Device) Err() error {
	d.errMu.Lock()
	defer d.errMu.Unlock()
	return d.err
}

// readLoop demultiplexes the one wire: request frames are served, event frames
// are routed to whoever is waiting, console text is captured.
func (d *Device) readLoop() {
	defer d.wg.Done()
	r := chaskibridge.NewFrameReader(d.port)
	// Console text shares the cable with frames. Capturing it rather than
	// discarding it is what gives C-19 its evidence.
	r.Console = &d.consoleWriter
	d.reader = r

	for {
		select {
		case <-d.closed:
			return
		default:
		}

		typ, payload, err := r.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				select {
				case <-d.closed:
				default:
					d.setErr(fmt.Errorf("reading the link: %w", err))
				}
			}
			return
		}

		switch typ {
		case chaskibridge.FrameRequest:
			if err := d.serveSync(payload); err != nil {
				d.setErr(err)
				return
			}
		case chaskibridge.FrameEvent:
			d.routeEvent(payload)
		}
	}
}

// serveSync forwards one device sync request to Wasi and frames the answer
// back. The bearer header travels verbatim: the bridge is a wire, and inventing
// auth here would mean the firmware never exercises the real thing (§14).
func (d *Device) serveSync(payload []byte) error {
	req, err := chaskibridge.DecodeRequestPayload(payload)
	if err != nil {
		return fmt.Errorf("decoding a request frame: %w", err)
	}

	out := chaskibridge.ResponsePayload{Seq: req.Seq}

	hreq, err := http.NewRequest(http.MethodPost, d.wasi, strings.NewReader(string(req.Body)))
	if err != nil {
		return err
	}
	hreq.Header.Set("Content-Type", "application/json; charset=utf-8")
	if req.Authorization != "" {
		hreq.Header.Set("Authorization", req.Authorization)
	}

	resp, err := d.hc.Do(hreq)
	if err != nil {
		// No answer is a transport failure, which is what the device would see
		// with no signal — letters wait, and nothing reads as broken (§5.3).
		out.Outcome = chaskibridge.OutcomeTransportFail
	} else {
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			out.Outcome = chaskibridge.OutcomeTransportFail
		} else {
			out.Outcome = chaskibridge.OutcomeOK
			out.HTTPStatus = uint16(resp.StatusCode)
			out.Body = body
			out.RetryAfter = resp.Header.Get("Retry-After")
		}
	}

	frame, err := chaskibridge.EncodeResponsePayload(out)
	if err != nil {
		return err
	}
	wire, err := chaskibridge.EncodeFrame(chaskibridge.FrameResponse, frame)
	if err != nil {
		return err
	}
	_, err = d.port.Write(wire)
	return err
}

func (d *Device) routeEvent(payload []byte) {
	var ev map[string]any
	if err := json.Unmarshal(payload, &ev); err != nil {
		return
	}

	// `hello` is unsolicited: it is how a boot announces itself, and the only
	// way the harness learns a reset it asked for actually happened.
	if name, _ := ev["event"].(string); name == "hello" {
		select {
		case d.hello <- ev:
		default:
		}
		return
	}

	id := 0
	if f, ok := ev["id"].(float64); ok {
		id = int(f)
	}

	d.mu.Lock()
	ch, ok := d.pending[id]
	if ok {
		delete(d.pending, id)
	}
	d.mu.Unlock()

	if ok {
		ch <- ev
	}
}

// Do sends one command and waits for the event carrying its id.
//
// It is deliberately serial. During a sync the device's transport owns the
// link and its decoder discards frame kinds that are not its own, so a command
// sent while a sync is in flight is eaten rather than queued (bench_control.h).
func (d *Device) Do(cmd map[string]any, timeout time.Duration) (map[string]any, error) {
	d.mu.Lock()
	d.nextID++
	id := d.nextID
	ch := make(chan map[string]any, 1)
	d.pending[id] = ch
	d.mu.Unlock()

	cmd["id"] = id
	body, err := json.Marshal(cmd)
	if err != nil {
		return nil, err
	}
	wire, err := chaskibridge.EncodeFrame(chaskibridge.FrameCommand, body)
	if err != nil {
		return nil, err
	}
	if _, err := d.port.Write(wire); err != nil {
		return nil, fmt.Errorf("writing command %v: %w", cmd["cmd"], err)
	}

	select {
	case ev := <-ch:
		if name, _ := ev["event"].(string); name == "error" {
			why, _ := ev["why"].(string)
			return ev, fmt.Errorf("device refused %v: %s", cmd["cmd"], why)
		}
		return ev, nil
	case <-time.After(timeout):
		d.mu.Lock()
		delete(d.pending, id)
		d.mu.Unlock()
		if err := d.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("timed out waiting for the answer to %v", cmd["cmd"])
	}
}

// WaitHello blocks for the next boot announcement. Commands that answer with a
// reset (`reboot`, and `sync` with cut_at armed) are confirmed this way.
func (d *Device) WaitHello(timeout time.Duration) (map[string]any, error) {
	select {
	case ev := <-d.hello:
		return ev, nil
	case <-time.After(timeout):
		if err := d.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("no hello: the device did not come back up")
	}
}

// DrainHello clears a stale boot announcement so a later wait cannot match it.
func (d *Device) DrainHello() {
	for {
		select {
		case <-d.hello:
		default:
			return
		}
	}
}

package chaskibridge

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sync"
)

// Bridge is the serial-to-HTTP proxy. One Bridge drives one device.
type Bridge struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger

	mu    sync.Mutex
	link  io.Writer // set while Serve is running
	stats Stats
}

// New builds a Bridge. It fails on a missing endpoint or an unreadable CA
// bundle rather than discovering either mid-run, when a bench case would read
// the failure as a device fault.
func New(cfg Config) (*Bridge, error) {
	if cfg.WasiURL == "" {
		return nil, errors.New("chaskibridge: no WasiURL configured")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12, // server §4.1
		// The device's own TLS is terminated in the modem against two pinned
		// private CAs (D-6). This setting governs only this process's hop to a
		// developer's compose stack.
		InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // dev fixture; see Config.InsecureSkipVerify
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("chaskibridge: reading CA bundle: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("chaskibridge: no certificates in %s", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}

	return &Bridge{
		cfg: cfg,
		log: cfg.Logger,
		client: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// Stats returns a snapshot of the content-free counters.
func (b *Bridge) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stats
}

// Serve reads frames from link until the stream ends or ctx is cancelled.
//
// Cancellation closes the link when it is a Closer, because a blocking serial
// read is not otherwise interruptible. Serve reports io.EOF as a nil error:
// the device rebooting or the cable being pulled is how a bench session ends,
// not a failure of the bridge.
func (b *Bridge) Serve(ctx context.Context, link io.ReadWriter) error {
	b.mu.Lock()
	b.link = link
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.link = nil
		b.mu.Unlock()
	}()

	if closer, ok := link.(io.Closer); ok {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-ctx.Done():
				_ = closer.Close()
			case <-stop:
			}
		}()
	}

	fr := NewFrameReader(link)
	fr.Console = b.cfg.Console

	// Forwarding runs off the read loop. A sync across the compose stack can
	// take seconds, and the device's console shares this cable: a reader
	// parked on an HTTP call lets the OS serial buffer overflow, which costs
	// C-19 its evidence and can cut a frame in half. The device issues one
	// exchange at a time, and the sequence number in each response is what
	// makes an overlap safe if it ever does not.
	var inflight sync.WaitGroup
	defer inflight.Wait()

	for {
		t, payload, err := fr.Next()
		b.mu.Lock()
		b.stats.Resyncs = fr.Resyncs()
		b.stats.Torn = fr.Torn()
		b.mu.Unlock()
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("chaskibridge: reading the link: %w", err)
		}

		b.mu.Lock()
		b.stats.Frames++
		b.stats.BytesIn += len(payload)
		b.mu.Unlock()

		switch t {
		case FrameRequest:
			inflight.Add(1)
			go func(p []byte) {
				defer inflight.Done()
				b.handleRequest(ctx, p)
			}(payload)
		case FrameEvent:
			b.mu.Lock()
			b.stats.Events++
			b.mu.Unlock()
			if b.cfg.OnEvent != nil {
				b.cfg.OnEvent(payload)
			}
		default:
			// Forward compatibility: a frame kind this build does not know is
			// the other end being newer, not an error.
			b.log.Debug("ignoring frame", "type", t.String(), "bytes", len(payload))
		}
	}
}

// SendCommand writes a bench control frame to the device. It is the harness's
// only way to make a device with no keyboard compose and sync (C-1, C-4).
func (b *Bridge) SendCommand(payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.link == nil {
		return errors.New("chaskibridge: no link; Serve is not running")
	}
	frame, err := EncodeFrame(FrameCommand, payload)
	if err != nil {
		return err
	}
	if _, err := b.link.Write(frame); err != nil {
		return fmt.Errorf("chaskibridge: writing a command: %w", err)
	}
	b.stats.Commands++
	b.stats.BytesOut += len(payload)
	return nil
}

// handleRequest forwards one sync request and frames the answer back. Every
// outcome — including "could not reach Wasi" — produces a response frame, so
// the device never waits out its own timeout for a failure the host already
// knows about.
func (b *Bridge) handleRequest(ctx context.Context, payload []byte) {
	req, err := DecodeRequestPayload(payload)
	if err != nil {
		// A framed payload that does not decode means the two sides disagree
		// about the layout. There is no sequence number to answer with, so the
		// device's own timeout is the only honest outcome.
		b.log.Error("undecodable request payload", "bytes", len(payload), "err", err)
		return
	}
	b.mu.Lock()
	b.stats.Requests++
	b.mu.Unlock()

	resp := b.forward(ctx, req)
	if err := b.writeResponse(resp); err != nil {
		b.log.Error("writing a response frame", "seq", resp.Seq, "err", err)
	}
}

func (b *Bridge) writeResponse(resp ResponsePayload) error {
	payload, err := EncodeResponsePayload(resp)
	if err != nil {
		return err
	}
	frame, err := EncodeFrame(FrameResponse, payload)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.link == nil {
		return errors.New("chaskibridge: no link")
	}
	if _, err := b.link.Write(frame); err != nil {
		return err
	}
	b.stats.Responses++
	b.stats.BytesOut += len(payload)
	switch resp.Outcome {
	case OutcomeTransportFail:
		b.stats.TransportFails++
	case OutcomeTLSTrustFail:
		b.stats.TLSTrustFails++
	case OutcomeOK:
	}
	return nil
}

// forward performs the HTTP exchange. The device's Authorization value is set
// verbatim and nothing else about the request is the bridge's opinion (§14).
func (b *Bridge) forward(ctx context.Context, req RequestPayload) ResponsePayload {
	out := ResponsePayload{Seq: req.Seq, Outcome: OutcomeTransportFail}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.WasiURL, bytes.NewReader(req.Body))
	if err != nil {
		b.log.Error("building the forward request", "seq", req.Seq, "err", err)
		return out
	}
	httpReq.Header.Set("Content-Type", contentType)
	if req.Authorization != "" {
		// Verbatim. A device that sent no bearer must reach Wasi with none, so
		// the 401 the firmware sees is the server's answer and not the
		// bridge's guess about what it meant to send (§14, server §4.1).
		httpReq.Header.Set("Authorization", req.Authorization)
	}

	httpResp, err := b.client.Do(httpReq)
	if err != nil {
		out.Outcome = classifyTransportError(err)
		// The error text can name the endpoint but never a body: the request
		// body is not part of a transport error and is not logged here.
		b.log.Info("forward failed", "seq", req.Seq, "outcome", out.Outcome.String(),
			"req_bytes", len(req.Body))
		return out
	}
	defer httpResp.Body.Close()

	out.RetryAfter = httpResp.Header.Get("Retry-After")

	// A body that cannot fit in a frame must not be silently truncated: half a
	// response is a response the device would parse and act on. Cap, detect,
	// and report a transport failure instead — the device retries the
	// identical request and the server's dedup makes that safe (server §4.1).
	budget := MaxFrameBytes - 7 - len(out.RetryAfter)
	body, err := io.ReadAll(io.LimitReader(httpResp.Body, int64(budget)+1))
	if err != nil {
		out.Outcome = OutcomeTransportFail
		out.RetryAfter = ""
		b.log.Info("reading the response body failed", "seq", req.Seq, "err", err)
		return out
	}
	if len(body) > budget {
		out.Outcome = OutcomeTransportFail
		out.RetryAfter = ""
		b.log.Error("response too large for one frame", "seq", req.Seq,
			"status", httpResp.StatusCode, "limit_bytes", budget)
		return out
	}

	out.Outcome = OutcomeOK
	out.HTTPStatus = uint16(httpResp.StatusCode)
	out.Body = body
	b.log.Info("forwarded", "seq", req.Seq, "req_bytes", len(req.Body),
		"status", httpResp.StatusCode, "resp_bytes", len(body),
		"retry_after", out.RetryAfter != "")
	return out
}

// classifyTransportError splits "never got there" from "got there and did not
// trust it" (D-6, §5.3). The device renders the two differently, so collapsing
// them here would erase a distinction the whole fault-state design rests on.
func classifyTransportError(err error) WireOutcome {
	var verify *tls.CertificateVerificationError
	if errors.As(err, &verify) {
		return OutcomeTLSTrustFail
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return OutcomeTLSTrustFail
	}
	return OutcomeTransportFail
}

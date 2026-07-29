//go:build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/protocol"
	"github.com/tholent/chaskiwasi/tools/chaskisim"
)

// syncTimeout bounds one POST /sync from the suite's side. A sync that has to
// reconcile INBOX, submit a letter over SMTP and call strip is still a
// sub-second operation against this fixture; anything near this bound is a
// wedge, not slowness.
const syncTimeout = 45 * time.Second

// newDeviceClient builds a device-side HTTPS client pinned to the fixture's
// CA-A.
//
// Pinning rather than skipping verification is the point of §12.2: the device
// trusts only certificates the operator signed, and the whole argument for a
// private CA is that this holds even when the client's hostname checking is
// weak. A suite running with InsecureSkipVerify would be unable to notice if
// the device listener started serving something else entirely.
func newDeviceClient(t testing.TB, caPEM []byte) *chaskisim.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("ca.crt from the wasi TLS volume is not a PEM certificate")
	}
	return chaskisim.NewClient(chaskisim.ClientConfig{
		BaseURL:   deviceSyncURL,
		Token:     deviceToken,
		TLSConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		Timeout:   syncTimeout,
	})
}

// probeDeviceListener reports whether the device listener is up, without
// syncing.
//
// The distinction matters: a sync runs §5.1's reconciliation pass, so using
// one as a liveness probe would quarantine the mail V-15 is waiting to watch
// the *startup* pass quarantine. A deliberately wrong bearer token gets
// §4.1's 401 and changes nothing.
func probeDeviceListener(caPEM []byte) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return errors.New("ca.crt is not a PEM certificate")
	}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}

	req, err := http.NewRequest(http.MethodPost, deviceSyncURL, strings.NewReader(`{"cursor":"","ayllu_version":0}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer not-the-device-token")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("device listener answered %d to a bad token, want 401 (§4.1)", resp.StatusCode)
	}
	return nil
}

// simDevice is a chaskisim.Device plus a client, with the ceremony of context
// and error handling folded away.
//
// The firmware behaviour lives in chaskisim, not here: dedup by letter id,
// terminal acks, the more-drain cap, doorbell counter rules. That is
// deliberate — a suite that reimplemented those rules would be asserting
// against its own idea of a device rather than against the one the wire
// contract describes and the CLI demonstrates.
type simDevice struct {
	*chaskisim.Device
	client *chaskisim.Client
}

// sync performs one round trip and applies it to device state.
func (d *simDevice) sync(t *testing.T) *protocol.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()

	resp, err := d.SyncOnce(ctx, d.client)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return resp
}

// wake runs a whole wake: sync, then sync again for every more: true, capped
// at chaskisim.MaxMoreRounds (§4.6).
func (d *simDevice) wake(t *testing.T) []*protocol.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout*chaskisim.MaxMoreRounds)
	defer cancel()

	resps, err := d.Wake(ctx, d.client)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	return resps
}

// raw sends a request the caller composed, without touching device state.
//
// This is how the suite asks for things a well-behaved device would never ask
// for on its own: an empty cursor mid-life (a window resync, §4.4), or a
// byte-identical replay of a sync that was already answered (§4.7's
// idempotency, V-9 and V-12). Device.SyncOnce always builds its request from
// current state, which is right for it and wrong for those.
func (d *simDevice) raw(t *testing.T, req protocol.Request) *protocol.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), syncTimeout)
	defer cancel()

	resp, err := d.client.Sync(ctx, req)
	if err != nil {
		t.Fatalf("raw sync: %v", err)
	}
	return resp
}

// windowResync asks for the most recent resync_window letters, which is what a
// factory-reset device and a device whose UIDVALIDITY moved both get (§4.4).
func (d *simDevice) windowResync(t *testing.T) *protocol.Response {
	t.Helper()
	return d.raw(t, protocol.Request{Cursor: "", AylluVersion: d.State().AylluVersion})
}

// letterWithBody finds the one delivered letter whose body contains marker.
func letterWithBody(t *testing.T, resp *protocol.Response, marker string) protocol.Letter {
	t.Helper()
	var found []protocol.Letter
	for _, l := range resp.Letters {
		if strings.Contains(l.Body, marker) || strings.Contains(l.Subject, marker) {
			found = append(found, l)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one delivered letter carrying %q, got %d of %d delivered",
			marker, len(found), len(resp.Letters))
	}
	return found[0]
}

// ackFor returns the ack for a local id, or fails: §4.7 makes every outbound
// letter's outcome terminal, so a missing ack is itself a defect.
func ackFor(t *testing.T, resp *protocol.Response, localID string) protocol.Ack {
	t.Helper()
	for _, a := range resp.Acks {
		if a.LocalID == localID {
			return a
		}
	}
	t.Fatalf("no ack for %q in %+v", localID, resp.Acks)
	return protocol.Ack{}
}

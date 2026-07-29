package mailbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

const (
	testUsername = "chaski"
	testPassword = "correct horse battery staple"
)

// newTestServer starts an in-process IMAP fixture (imapmemserver, from
// go-imap/v2 itself) on a loopback TCP port and returns a Config wired to
// dial it. This is deliberately not maddy: the point is a Mailbox test that
// runs with `go test ./internal/mailbox/...` and no docker compose, the same
// pattern go-imap/v2's own client tests use (see imapclient/client_test.go).
//
// mailboxes lists additional folders to create beyond INBOX (e.g. "Held").
func newTestServer(t *testing.T, mailboxes ...string) Config {
	t.Helper()

	user := imapmemserver.NewUser(testUsername, testPassword)
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatalf("create INBOX: %v", err)
	}
	for _, name := range mailboxes {
		if err := user.Create(name, nil); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	memServer := imapmemserver.New()
	memServer.AddUser(user)

	cert := generateTestCert(t)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		TLSConfig:    &tls.Config{Certificates: []tls.Certificate{cert}},
		InsecureAuth: true, // fixture only; production always dials over TLS
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
		},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		ln.Close()
	})

	return Config{
		Addr:     ln.Addr().String(),
		Username: testUsername,
		Password: testPassword,
		Dial: func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", ln.Addr().String())
		},
	}
}

func testMessage(t *testing.T, seq int, subject string) []byte {
	t.Helper()
	return []byte(fmt.Sprintf(
		"From: rosa@example.com\r\n"+
			"To: kid@example.com\r\n"+
			"Subject: %s\r\n"+
			"Message-Id: <msg-%d@example.com>\r\n"+
			"Date: Mon, 2 Jan 2006 15:04:05 +0000\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"\r\n"+
			"letter body %d\r\n",
		subject, seq, seq,
	))
}

func TestUIDValidity(t *testing.T) {
	cfg := newTestServer(t)
	mb := NewIMAPMailbox(cfg)
	defer mb.Close()

	uv, err := mb.UIDValidity(context.Background())
	if err != nil {
		t.Fatalf("UIDValidity: %v", err)
	}
	if uv == 0 {
		t.Errorf("UIDValidity = 0, want non-zero")
	}
}

func TestAppendAndFetchAbove(t *testing.T) {
	cfg := newTestServer(t)
	mb := NewIMAPMailbox(cfg)
	defer mb.Close()

	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		msg := testMessage(t, i, fmt.Sprintf("letter %d", i))
		if err := mb.Append(ctx, "INBOX", msg, time.Time{}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	raws, err := mb.FetchAbove(ctx, 0, 10)
	if err != nil {
		t.Fatalf("FetchAbove: %v", err)
	}
	if len(raws) != 3 {
		t.Fatalf("FetchAbove returned %d messages, want 3", len(raws))
	}
	for i, r := range raws {
		if r.UID == 0 {
			t.Errorf("message %d: UID = 0, want non-zero", i)
		}
		if r.InternalDate.IsZero() {
			t.Errorf("message %d: InternalDate is zero", i)
		}
		wantSubject := fmt.Sprintf("Subject: letter %d\r\n", i+1)
		if !contains(r.Data, wantSubject) {
			t.Errorf("message %d: Data missing %q", i, wantSubject)
		}
	}

	// FetchAbove the highest UID so far returns nothing further.
	more, err := mb.FetchAbove(ctx, raws[len(raws)-1].UID, 10)
	if err != nil {
		t.Fatalf("FetchAbove above last uid: %v", err)
	}
	if len(more) != 0 {
		t.Errorf("FetchAbove above last uid returned %d messages, want 0", len(more))
	}

	// The max parameter bounds how many come back even though more exist.
	limited, err := mb.FetchAbove(ctx, 0, 2)
	if err != nil {
		t.Fatalf("FetchAbove with max=2: %v", err)
	}
	if len(limited) != 2 {
		t.Errorf("FetchAbove with max=2 returned %d messages, want 2", len(limited))
	}
}

func TestRecent(t *testing.T) {
	cfg := newTestServer(t)
	mb := NewIMAPMailbox(cfg)
	defer mb.Close()

	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		msg := testMessage(t, i, fmt.Sprintf("letter %d", i))
		if err := mb.Append(ctx, "INBOX", msg, time.Time{}); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	raws, err := mb.Recent(ctx, 2)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(raws) != 2 {
		t.Fatalf("Recent(2) returned %d messages, want 2", len(raws))
	}
	if !contains(raws[0].Data, "Subject: letter 4\r\n") || !contains(raws[1].Data, "Subject: letter 5\r\n") {
		t.Errorf("Recent(2) did not return the newest span: got subjects %q, %q",
			subjectOf(raws[0].Data), subjectOf(raws[1].Data))
	}

	// Asking for more than exist returns everything, not an error.
	all, err := mb.Recent(ctx, 100)
	if err != nil {
		t.Fatalf("Recent(100): %v", err)
	}
	if len(all) != 5 {
		t.Errorf("Recent(100) returned %d messages, want 5", len(all))
	}
}

func TestMoveAndList(t *testing.T) {
	cfg := newTestServer(t, "Held")
	mb := NewIMAPMailbox(cfg)
	defer mb.Close()

	ctx := context.Background()
	msg := testMessage(t, 1, "stranger")
	if err := mb.Append(ctx, "INBOX", msg, time.Time{}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	raws, err := mb.FetchAbove(ctx, 0, 10)
	if err != nil || len(raws) != 1 {
		t.Fatalf("FetchAbove: %v, %d messages", err, len(raws))
	}
	uid := raws[0].UID

	if err := mb.Move(ctx, "INBOX", uid, "Held"); err != nil {
		t.Fatalf("Move: %v", err)
	}

	inbox, err := mb.List(ctx, "INBOX")
	if err != nil {
		t.Fatalf("List INBOX: %v", err)
	}
	if len(inbox) != 0 {
		t.Errorf("INBOX has %d messages after Move, want 0", len(inbox))
	}

	held, err := mb.List(ctx, "Held")
	if err != nil {
		t.Fatalf("List Held: %v", err)
	}
	if len(held) != 1 {
		t.Fatalf("Held has %d messages after Move, want 1", len(held))
	}
	if !contains(held[0].Data, "Subject: stranger\r\n") {
		t.Errorf("moved message lost its content: %q", subjectOf(held[0].Data))
	}
}

func TestIdleNotifiesOnNewMail(t *testing.T) {
	cfg := newTestServer(t)
	mb := NewIMAPMailbox(cfg)
	defer mb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	notify := make(chan struct{}, 1)
	idleErr := make(chan error, 1)
	go func() { idleErr <- mb.Idle(ctx, notify) }()

	// Give the IDLE connection a moment to actually be idling before we
	// append, since the notification is unilateral server data, not a
	// polled state.
	time.Sleep(200 * time.Millisecond)

	msg := testMessage(t, 1, "ring the doorbell")
	if err := mb.Append(context.Background(), "INBOX", msg, time.Time{}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	select {
	case <-notify:
		// good: Idle is a notification path only (§5.1) — it doesn't hand us
		// the message, just tells us to go look.
	case <-time.After(5 * time.Second):
		t.Fatal("Idle did not signal notify after a new message arrived")
	}

	cancel()
	select {
	case err := <-idleErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Idle returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Idle did not return after ctx cancellation")
	}
}

func TestUIDValidity_Unreachable(t *testing.T) {
	// Bind and immediately close a port so the dial fails fast with
	// "connection refused" rather than timing out.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	mb := NewIMAPMailbox(Config{
		Addr:           addr,
		Username:       "irrelevant",
		Password:       "irrelevant",
		RetryBaseDelay: 10 * time.Millisecond,
		RetryMaxDelay:  20 * time.Millisecond,
		Logger:         silentLogger(),
	})
	defer mb.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = mb.UIDValidity(ctx)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("UIDValidity error = %v, want errors.Is(err, ErrUnreachable)", err)
	}
	// The point of the retry-with-backoff design is that it gives up when
	// the caller's context does, not sometime much later.
	if elapsed > 2*time.Second {
		t.Errorf("UIDValidity took %v after a 500ms ctx deadline; retry loop is not honouring ctx", elapsed)
	}
}

func contains(data []byte, substr string) bool {
	return bytes.Contains(data, []byte(substr))
}

func subjectOf(data []byte) string {
	const prefix = "Subject: "
	i := bytes.Index(data, []byte(prefix))
	if i < 0 {
		return ""
	}
	rest := string(data[i+len(prefix):])
	if j := strings.Index(rest, "\r\n"); j >= 0 {
		return rest[:j]
	}
	return rest
}

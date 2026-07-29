package syncsvc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
	"github.com/tholent/chaskiwasi/internal/config"
	"github.com/tholent/chaskiwasi/internal/mailbox"
	"github.com/tholent/chaskiwasi/internal/protocol"
)

// testToken is the bearer token every test presents; the config below carries
// its SHA-256, exactly as wasi.toml does (§4.1).
const testToken = "test-device-token"

func testTokenHash() string {
	sum := sha256.Sum256([]byte(testToken))
	return hex.EncodeToString(sum[:])
}

// fakeConfig is a ConfigSource over one fixed config.
type fakeConfig struct{ cfg *config.Config }

func (f *fakeConfig) Current() *config.Config { return f.cfg }

func testConfig() *config.Config {
	return &config.Config{
		Owner: config.Owner{Name: "Maya"},
		Mail: config.Mail{
			IMAP:       "mail.test:993",
			SMTP:       "mail.test:465",
			Address:    "kid@chaski.test",
			HeldFolder: "Held",
		},
		Device: config.Device{TokenHash: testTokenHash()},
		Sync: config.Sync{
			MaxLetterChars: 500,
			BudgetBytes:    2048,
			ResyncWindow:   200,
			IntervalS:      21600,
		},
		Ayllu:        config.Ayllu{MaxContacts: 24},
		DeviceConfig: config.DeviceConfig{RAT: "ltem", Cover: "Chaski"},
	}
}

// fakeAyllu is an AylluStore over a fixed contact table.
type fakeAyllu struct {
	version  int
	contacts []ayllu.Contact
}

func (f *fakeAyllu) ByID(id string) (ayllu.Contact, bool) {
	if id == protocol.SysContactID {
		return ayllu.Contact{ID: protocol.SysContactID, Name: "Home", Address: ayllu.SystemAddress, Active: true}, true
	}
	for _, c := range f.contacts {
		if c.ID == id {
			return c, true
		}
	}
	return ayllu.Contact{}, false
}

// DeviceView mirrors ayllu.FileStore's: nil when the device is current, and
// never an address (I-2).
func (f *fakeAyllu) DeviceView(requestVersion int) *protocol.Ayllu {
	if requestVersion == f.version {
		return nil
	}
	out := &protocol.Ayllu{Version: f.version}
	for _, c := range f.contacts {
		out.Contacts = append(out.Contacts, protocol.AylluContact{
			ID: c.ID, Name: c.Name, Active: c.Active,
			Pinned: c.Pinned, Order: c.Order, Portrait: c.Portrait,
		})
	}
	return out
}

func testAyllu() *fakeAyllu {
	return &fakeAyllu{
		version: 7,
		contacts: []ayllu.Contact{
			{ID: "c_01", Name: "Abuela", Address: "abuela@example.test", Active: true},
			{ID: "c_07", Name: "Rosa", Address: "rosa@example.test", Active: false},
		},
	}
}

// fakeMailbox serves a fixed INBOX. Setting err makes every method fail with
// it, which is how the 503 path is exercised without a network.
type fakeMailbox struct {
	uidValidity uint32
	messages    []mailbox.Raw
	err         error

	mu          sync.Mutex
	recentCalls []int
	aboveCalls  []uint32
}

func (f *fakeMailbox) UIDValidity(ctx context.Context) (uint32, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.uidValidity, nil
}

func (f *fakeMailbox) FetchAbove(ctx context.Context, uid uint32, max int) ([]mailbox.Raw, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	f.aboveCalls = append(f.aboveCalls, uid)
	f.mu.Unlock()

	var out []mailbox.Raw
	for _, m := range f.messages {
		if m.UID > uid && len(out) < max {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeMailbox) Recent(ctx context.Context, n int) ([]mailbox.Raw, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.mu.Lock()
	f.recentCalls = append(f.recentCalls, n)
	f.mu.Unlock()

	if n <= 0 || len(f.messages) == 0 {
		return nil, nil
	}
	start := 0
	if len(f.messages) > n {
		start = len(f.messages) - n
	}
	return append([]mailbox.Raw(nil), f.messages[start:]...), nil
}

func (f *fakeMailbox) List(ctx context.Context, folder string) ([]mailbox.Raw, error) {
	return nil, f.err
}
func (f *fakeMailbox) Move(ctx context.Context, folder string, uid uint32, dest string) error {
	return f.err
}
func (f *fakeMailbox) Append(ctx context.Context, folder string, msg []byte, at time.Time) error {
	return f.err
}
func (f *fakeMailbox) Idle(ctx context.Context, notify chan<- struct{}) error { return f.err }
func (f *fakeMailbox) Close() error                                           { return nil }

// message builds one raw INBOX message whose body is bodyLen 'x' characters,
// so a test can control the wire size of the derived letter.
func message(uid uint32, bodyLen int) mailbox.Raw {
	return mailbox.Raw{
		UID:          uid,
		InternalDate: time.Unix(1785349200, 0).UTC(),
		Data:         []byte(strings.Repeat("x", bodyLen)),
	}
}

// fakeDeriver renders a Raw as a letter without parsing anything: the raw data
// is the body. err, when set, fails derivation for uid errUID.
type fakeDeriver struct {
	err    error
	errUID uint32
}

func (d fakeDeriver) Derive(ctx context.Context, r mailbox.Raw) (protocol.Letter, error) {
	if d.err != nil && r.UID == d.errUID {
		return protocol.Letter{}, d.err
	}
	return protocol.Letter{
		ID:        fmt.Sprintf("l-%08x", r.UID),
		ContactID: "c_01",
		Subject:   "camping",
		Date:      r.InternalDate.Unix(),
		Body:      string(r.Data),
	}, nil
}

// sentMessage is one SMTP submission, captured for assertion.
type sentMessage struct {
	from string
	to   []string
	msg  []byte
}

// fakeSubmitter records submissions. err, when set, fails every send.
type fakeSubmitter struct {
	mu   sync.Mutex
	sent []sentMessage
	err  error
}

func (f *fakeSubmitter) Send(ctx context.Context, from string, to []string, msg []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sentMessage{from: from, to: append([]string(nil), to...), msg: append([]byte(nil), msg...)})
	return nil
}

func (f *fakeSubmitter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// fakeKipu records accepted blocks. err, when set, fails every append — which
// must never fail a sync (§4.8).
type fakeKipu struct {
	mu     sync.Mutex
	blocks []map[string]any
	err    error
}

func (f *fakeKipu) Append(block map[string]any, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocks = append(f.blocks, block)
	return f.err
}

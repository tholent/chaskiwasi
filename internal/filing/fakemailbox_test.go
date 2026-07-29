package filing

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/tholent/chaskiwasi/internal/mailbox"
)

// fakeMailbox is an in-memory mailbox.Mailbox for unit tests: no real IMAP
// server, just folders of raw messages keyed by name. It intentionally
// mimics one real IMAP behaviour that matters to this package: MOVE assigns
// the message a brand-new UID in the destination folder (§8: "a released
// message receives a new UID above the cursor"), so a test that releases a
// Held message and then checks it re-arrives in INBOX as a new UID is
// exercising the same UID-reassignment shape production sees.
type fakeMailbox struct {
	mu      sync.Mutex
	folders map[string][]mailbox.Raw
	nextUID uint32
}

var _ mailbox.Mailbox = (*fakeMailbox)(nil)

func newFakeMailbox() *fakeMailbox {
	return &fakeMailbox{folders: make(map[string][]mailbox.Raw), nextUID: 1}
}

// seed places msgs directly into folder, simulating mail that arrived
// through some path other than this package (SMTP delivery, a maddy
// imap-msgs injection, or — in these tests — a fixture setting up "already
// there" state). Messages with UID 0 are assigned the next fake UID.
func (f *fakeMailbox) seed(folder string, msgs ...mailbox.Raw) []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()

	var uids []uint32
	for _, m := range msgs {
		if m.UID == 0 {
			m.UID = f.nextUID
		}
		if m.UID >= f.nextUID {
			f.nextUID = m.UID + 1
		}
		f.folders[folder] = append(f.folders[folder], m)
		uids = append(uids, m.UID)
	}
	return uids
}

func (f *fakeMailbox) UIDValidity(ctx context.Context) (uint32, error) { return 1, nil }

func (f *fakeMailbox) FetchAbove(ctx context.Context, uid uint32, max int) ([]mailbox.Raw, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	msgs := append([]mailbox.Raw(nil), f.folders[inboxFolder]...)
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].UID < msgs[j].UID })

	var out []mailbox.Raw
	for _, m := range msgs {
		if m.UID <= uid {
			continue
		}
		out = append(out, m)
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out, nil
}

func (f *fakeMailbox) Recent(ctx context.Context, n int) ([]mailbox.Raw, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := append([]mailbox.Raw(nil), f.folders[inboxFolder]...)
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].UID < msgs[j].UID })
	if n >= 0 && len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	return msgs, nil
}

func (f *fakeMailbox) List(ctx context.Context, folder string) ([]mailbox.Raw, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]mailbox.Raw(nil), f.folders[folder]...)
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out, nil
}

func (f *fakeMailbox) Move(ctx context.Context, folder string, uid uint32, dest string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	src := f.folders[folder]
	for i, m := range src {
		if m.UID != uid {
			continue
		}
		moved := m
		moved.UID = f.nextUID
		f.nextUID++
		f.folders[folder] = append(append([]mailbox.Raw(nil), src[:i]...), src[i+1:]...)
		f.folders[dest] = append(f.folders[dest], moved)
		return nil
	}
	return fmt.Errorf("fakeMailbox: uid %d not found in %s", uid, folder)
}

func (f *fakeMailbox) Append(ctx context.Context, folder string, msg []byte, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.folders[folder] = append(f.folders[folder], mailbox.Raw{UID: f.nextUID, Data: msg, InternalDate: at})
	f.nextUID++
	return nil
}

func (f *fakeMailbox) Idle(ctx context.Context, notify chan<- struct{}) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeMailbox) Close() error { return nil }

// uidsIn returns the sorted UIDs currently in folder, for assertions.
func (f *fakeMailbox) uidsIn(folder string) []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var uids []uint32
	for _, m := range f.folders[folder] {
		uids = append(uids, m.UID)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	return uids
}

func containsUID(uids []uint32, want uint32) bool {
	for _, u := range uids {
		if u == want {
			return true
		}
	}
	return false
}

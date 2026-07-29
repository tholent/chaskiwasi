package main

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tholent/chaskiwasi/internal/ayllu"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it, so list's table output and mutateAndReport's
// "no notice was sent" line can be asserted on directly.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	w.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	return buf.String()
}

func TestContacts_AddDeactivateReactivateReaddress(t *testing.T) {
	dataDir := t.TempDir()

	if err := runContacts([]string{
		"add", "-data", dataDir, "-actor", "dad", "-name", "Rosa", "-address", "rosa@example.com",
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	store, err := ayllu.Open(dataDir, 24)
	if err != nil {
		t.Fatalf("ayllu.Open: %v", err)
	}
	_, contacts := store.List()
	if len(contacts) != 1 {
		t.Fatalf("len(contacts) = %d, want 1", len(contacts))
	}
	id := contacts[0].ID
	if contacts[0].Name != "Rosa" || contacts[0].Address != "rosa@example.com" || !contacts[0].Active {
		t.Fatalf("unexpected contact after add: %+v", contacts[0])
	}

	if err := runContacts([]string{"deactivate", "-data", dataDir, "-actor", "dad", id}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	store, _ = ayllu.Open(dataDir, 24)
	c, _ := store.ByID(id)
	if c.Active {
		t.Fatal("deactivate did not clear Active")
	}
	if c.Address != "rosa@example.com" {
		t.Fatal("deactivate must retain the address (I-5)")
	}

	if err := runContacts([]string{"reactivate", "-data", dataDir, "-actor", "dad", id}); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	store, _ = ayllu.Open(dataDir, 24)
	c, _ = store.ByID(id)
	if !c.Active {
		t.Fatal("reactivate did not set Active")
	}

	if err := runContacts([]string{
		"readdress", "-data", dataDir, "-actor", "dad", "-address", "rosa2@example.com", id,
	}); err != nil {
		t.Fatalf("readdress: %v", err)
	}
	store, _ = ayllu.Open(dataDir, 24)
	c, _ = store.ByID(id)
	if c.Address != "rosa2@example.com" {
		t.Fatalf("readdress did not update the address: %+v", c)
	}
	if len(c.PastAddresses) != 1 || c.PastAddresses[0] != "rosa@example.com" {
		t.Fatalf("readdress did not retain the old address (F-1): %+v", c)
	}

	// Every mutation above is a durable, log-worthy change (§7.6): the change
	// log is what lets the next `wasi serve` start announce what this CLI did.
	events, err := ayllu.ReadLog(dataDir, time.Time{})
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("len(events) = %d, want 4 (add, deactivate, reactivate, readdress)", len(events))
	}
	last := events[len(events)-1]
	if last.Action != ayllu.ActionReaddress || last.OldAddress != "rosa@example.com" || last.NewAddress != "rosa2@example.com" {
		t.Fatalf("last log event = %+v, want a readdress with both addresses recorded (I-2 permits this in the log)", last)
	}
}

func TestContacts_AddOnExistingAddressReactivatesInsteadOfDuplicating(t *testing.T) {
	dataDir := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		if err := runContacts(args); err != nil {
			t.Fatalf("runContacts(%v): %v", args, err)
		}
	}

	run("add", "-data", dataDir, "-name", "Rosa", "-address", "rosa@example.com")
	store, _ := ayllu.Open(dataDir, 24)
	_, contacts := store.List()
	id := contacts[0].ID

	run("deactivate", "-data", dataDir, id)
	run("add", "-data", dataDir, "-name", "Rosa", "-address", "rosa@example.com")

	store, _ = ayllu.Open(dataDir, 24)
	_, contacts = store.List()
	if len(contacts) != 1 {
		t.Fatalf("re-adding a tombstone's address created a second row: %+v (§7.2)", contacts)
	}
	if contacts[0].ID != id {
		t.Fatalf("re-adding reused a different id: got %s, want %s", contacts[0].ID, id)
	}
	if !contacts[0].Active {
		t.Fatal("re-adding did not reactivate the tombstone")
	}
}

func TestContacts_List_MarksTombstonesClearly(t *testing.T) {
	dataDir := t.TempDir()
	if err := runContacts([]string{"add", "-data", dataDir, "-name", "Rosa", "-address", "rosa@example.com"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	store, _ := ayllu.Open(dataDir, 24)
	_, contacts := store.List()
	id := contacts[0].ID
	if err := runContacts([]string{"deactivate", "-data", dataDir, id}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runContacts([]string{"list", "-data", dataDir}); err != nil {
			t.Fatalf("list: %v", err)
		}
	})

	if !strings.Contains(out, "TOMBSTONE") {
		t.Errorf("list output does not clearly mark the tombstone:\n%s", out)
	}
	if !strings.Contains(out, "Rosa") || !strings.Contains(out, "rosa@example.com") {
		t.Errorf("list output missing expected fields:\n%s", out)
	}
}

func TestContacts_MutationsPrintThatNoNoticeWasSent(t *testing.T) {
	dataDir := t.TempDir()
	out := captureStdout(t, func() {
		if err := runContacts([]string{"add", "-data", dataDir, "-name", "Rosa", "-address", "rosa@example.com"}); err != nil {
			t.Fatalf("add: %v", err)
		}
	})
	// I-4 forbids a silent change; this line is what keeps a CLI mutation
	// from reading as one.
	if !strings.Contains(out, "No notice was sent") {
		t.Errorf("add did not tell the operator that no notice was sent yet:\n%s", out)
	}
}

func TestContacts_RefusesWhileServerIsRunning(t *testing.T) {
	dataDir := t.TempDir()
	release, err := acquireDataLock(dataDir)
	if err != nil {
		t.Fatalf("acquireDataLock: %v", err)
	}
	defer release()

	err = runContacts([]string{"add", "-data", dataDir, "-name", "Rosa", "-address", "rosa@example.com"})
	if err == nil {
		t.Fatal("contacts add succeeded while the data directory was locked by another process")
	}
	if !errors.Is(err, errDataDirBusy) {
		t.Errorf("error %q does not wrap errDataDirBusy", err)
	}

	// The refusal must be a true no-op: nothing was written.
	if _, statErr := os.Stat(dataDir + "/ayllu.toml"); !os.IsNotExist(statErr) {
		t.Error("a refused mutation still wrote ayllu.toml")
	}
}

func TestContacts_ReleasesTheLockAfterEachInvocation(t *testing.T) {
	dataDir := t.TempDir()
	if err := runContacts([]string{"list", "-data", dataDir}); err != nil {
		t.Fatalf("list: %v", err)
	}
	// If the previous invocation leaked its lock, this would fail with
	// errDataDirBusy instead of running.
	if err := runContacts([]string{"add", "-data", dataDir, "-name", "Rosa", "-address", "rosa@example.com"}); err != nil {
		t.Fatalf("add after a prior invocation released its lock: %v", err)
	}
}

func TestContacts_AddRequiresNameAndAddress(t *testing.T) {
	dataDir := t.TempDir()
	tests := [][]string{
		{"add", "-data", dataDir, "-address", "rosa@example.com"},
		{"add", "-data", dataDir, "-name", "Rosa"},
	}
	for _, args := range tests {
		if err := runContacts(args); err == nil {
			t.Errorf("runContacts(%v) accepted a missing required flag", args)
		}
	}
}

func TestContacts_MaxContactsFromConfigIsHonored(t *testing.T) {
	dataDir := t.TempDir()
	configPath := dataDir + "/wasi.toml"
	mustWrite(t, configPath, `
[owner]
name = "Maya"
[mail]
imap = "mail.test:993"
smtp = "mail.test:465"
address = "kid@chaski.test"
[device]
token_hash = "7053fe692ce151a1a4e066d93850420b420ce95d823a0c7e8609fddf5272438d"
listen = "127.0.0.1:0"
[guardian]
listen = "127.0.0.1:0"
[ayllu]
max_contacts = 1
`)

	if err := runContacts([]string{
		"add", "-data", dataDir, "-config", configPath, "-name", "Rosa", "-address", "rosa@example.com",
	}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	err := runContacts([]string{
		"add", "-data", dataDir, "-config", configPath, "-name", "Ana", "-address", "ana@example.com",
	})
	if !errors.Is(err, ayllu.ErrMaxContacts) {
		t.Fatalf("second add error = %v, want ayllu.ErrMaxContacts (max_contacts=1 from config)", err)
	}
}

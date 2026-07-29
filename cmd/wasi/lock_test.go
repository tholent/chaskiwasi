//go:build unix

package main

import (
	"errors"
	"testing"
)

func TestAcquireDataLock_ExclusiveNonBlocking(t *testing.T) {
	dir := t.TempDir()

	release1, err := acquireDataLock(dir)
	if err != nil {
		t.Fatalf("first acquireDataLock: %v", err)
	}
	defer release1()

	if _, err := acquireDataLock(dir); !errors.Is(err, errDataDirBusy) {
		t.Fatalf("second acquireDataLock = %v, want errDataDirBusy", err)
	}
}

func TestAcquireDataLock_ReleasedOnRelease(t *testing.T) {
	dir := t.TempDir()

	release1, err := acquireDataLock(dir)
	if err != nil {
		t.Fatalf("acquireDataLock: %v", err)
	}
	release1()

	release2, err := acquireDataLock(dir)
	if err != nil {
		t.Fatalf("acquireDataLock after release: %v", err)
	}
	release2()
}

func TestAcquireDataLock_IndependentDirsDoNotConflict(t *testing.T) {
	release1, err := acquireDataLock(t.TempDir())
	if err != nil {
		t.Fatalf("acquireDataLock (dir 1): %v", err)
	}
	defer release1()

	release2, err := acquireDataLock(t.TempDir())
	if err != nil {
		t.Fatalf("acquireDataLock (dir 2): %v", err)
	}
	defer release2()
}

package tenstorrent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func fakeHost(t *testing.T, chips ...int) Config {
	t.Helper()
	dir := t.TempDir()
	root, procRoot = filepath.Join(dir, "dev"), filepath.Join(dir, "proc")
	for _, n := range chips {
		os.MkdirAll(filepath.Join(procRoot, itoa(n)), 0o755)
		os.MkdirAll(root, 0o755)
		os.WriteFile(filepath.Join(root, itoa(n)), nil, 0o644)
		os.WriteFile(filepath.Join(procRoot, itoa(n), "pids"), nil, 0o644)
	}
	return Config{Lock: filepath.Join(dir, "locks", "chip-{n}.lock"), Locks: map[int]string{1: filepath.Join(dir, "shared.lock")}}
}

func itoa(n int) string { return Join([]int{n}) }

func TestAcquireHoldsUntilClosedAndSeesUsers(t *testing.T) {
	c := fakeHost(t, 0, 1)
	if got := Chips(); len(got) != 2 {
		t.Fatalf("chips %v", got)
	}
	if c.LockPath(1) != c.Locks[1] {
		t.Fatalf("override ignored: %s", c.LockPath(1))
	}
	held, err := c.Acquire([]int{0, 1})
	if err != nil {
		t.Fatal(err)
	}
	if !c.Held(0) || !c.Held(1) {
		t.Fatal("locks not held")
	}
	if _, err := c.Acquire([]int{1}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire: %v", err)
	}
	for _, f := range held {
		f.Close()
	}
	if c.Held(0) {
		t.Fatal("lock survived closing its descriptor")
	}
	// A process with the chip open (not using the lock) also makes it busy, and the locks are released.
	os.WriteFile(filepath.Join(procRoot, "0", "pids"), []byte("4242\n"), 0o644)
	if err := c.Check([]int{0}); !errors.Is(err, ErrBusy) {
		t.Fatalf("open chip: %v", err)
	}
	if c.Held(0) {
		t.Fatal("failed acquire leaked a lock")
	}
}

// The lock directory is writable by every user of the host. A link planted where a lock belongs is not
// followed (no file is created or locked where it points), the chip counts as held, and nothing is reset.
func TestPlantedLinkIsNotALock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Tenstorrent hosts are Linux; creating links needs privileges on Windows")
	}
	c := fakeHost(t, 0)
	target := filepath.Join(t.TempDir(), "victim")
	os.MkdirAll(filepath.Dir(c.LockPath(0)), 0o777)
	if err := os.Symlink(target, c.LockPath(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Acquire([]int{0}); err == nil {
		t.Fatal("a symbolic link was taken as the chip's lock")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the link's target was created: %v", err)
	}
	if !c.Held(0) {
		t.Fatal("a chip whose lock cannot be opened was reported free")
	}
	marker := filepath.Join(t.TempDir(), "reset-ran")
	script := filepath.Join(t.TempDir(), "tt-smi")
	os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	c.TTSmi = script
	if err := c.Reset([]int{0}); err == nil {
		t.Fatal("reset went ahead without the chip's lock")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("tt-smi ran although the lock could not be taken")
	}
}

// A reset holds the chip's lock, so it never runs under another job.
func TestResetRefusesAChipAnotherJobHolds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake tt-smi is a shell script")
	}
	c := fakeHost(t, 0)
	marker := filepath.Join(t.TempDir(), "reset-ran")
	script := filepath.Join(t.TempDir(), "tt-smi")
	os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	c.TTSmi = script
	held, err := c.Acquire([]int{0})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reset([]int{0}); !errors.Is(err, ErrBusy) {
		t.Fatalf("reset of a held chip: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("tt-smi ran on a chip another job holds")
	}
	held[0].Close()
	if err := c.Reset([]int{0}); err != nil {
		t.Fatalf("reset of a free chip: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("tt-smi did not run on a free chip")
	}
}

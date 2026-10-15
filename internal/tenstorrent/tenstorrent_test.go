package tenstorrent

import (
	"errors"
	"os"
	"path/filepath"
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

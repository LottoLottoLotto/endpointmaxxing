// Package tenstorrent manages Tenstorrent chips as exclusive devices: a chip is
// owned by one process at a time (tt-metal opens it exclusively). Jobs on a card
// coordinate through a per-chip flock(2) lock file, and the kernel driver lists
// the processes that have a chip open in /proc/driver/tenstorrent/<n>/pids.
package tenstorrent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Config locates the lock files and tt-smi.
type Config struct {
	// Lock is the lock file pattern; {n} is the chip index.
	Lock string
	// Locks overrides single chips.
	Locks map[int]string
	// TTSmi is the tt-smi binary.
	TTSmi string
}

// LockPath is chip n's lock file.
func (c Config) LockPath(n int) string {
	if p, ok := c.Locks[n]; ok {
		return p
	}
	return strings.ReplaceAll(c.Lock, "{n}", strconv.Itoa(n))
}

// root is the device directory (a variable for tests).
var root = "/dev/tenstorrent"

// procRoot is the driver's proc directory (a variable for tests).
var procRoot = "/proc/driver/tenstorrent"

// Chips lists the chip indices present (/dev/tenstorrent/<n>), sorted.
func Chips() []int {
	ents, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range ents {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}

// Users returns the pids that have chip n open, according to the driver.
func Users(n int) []int {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(n), "pids"))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(data)) {
		if p, err := strconv.Atoi(f); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// Held reports whether another process holds chip n's lock (a non-blocking probe).
func (c Config) Held(n int) bool {
	f, err := c.open(n)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

func (c Config) open(n int) (*os.File, error) {
	p := c.LockPath(n)
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		return nil, err
	}
	// Other teams' jobs share these files: keep them group/world writable as their scripts expect.
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return nil, fmt.Errorf("chip %d lock %s: %w", n, p, err)
	}
	return f, nil
}

// ErrBusy means a chip is locked or open by another job.
var ErrBusy = errors.New("chip busy")

// Acquire takes the locks of chips without waiting and checks that no process has
// them open. The returned files hold the locks; pass them to the engine process
// (ExtraFiles) so the locks live exactly as long as it does, then close them here.
func (c Config) Acquire(chips []int) ([]*os.File, error) {
	var held []*os.File
	release := func() {
		for _, f := range held {
			f.Close()
		}
	}
	for _, n := range chips {
		f, err := c.open(n)
		if err != nil {
			release()
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			f.Close()
			release()
			return nil, fmt.Errorf("chip %d: lock %s is held by another job: %w", n, c.LockPath(n), ErrBusy)
		}
		held = append(held, f)
	}
	for _, n := range chips {
		if u := Users(n); len(u) > 0 {
			release()
			return nil, fmt.Errorf("chip %d is open by pid %v: %w", n, u, ErrBusy)
		}
	}
	return held, nil
}

// Check verifies chips are free (unlocked and unopened) without keeping the locks:
// for engines that take the locks themselves.
func (c Config) Check(chips []int) error {
	fs, err := c.Acquire(chips)
	for _, f := range fs {
		f.Close()
	}
	return err
}

// Reset resets chips with tt-smi (after a hung or killed engine left them in an unknown state).
func (c Config) Reset(chips []int) error {
	if len(chips) == 0 {
		return nil
	}
	bin := c.TTSmi
	if bin == "" {
		bin = "tt-smi"
	}
	targets := make([]string, len(chips))
	for i, n := range chips {
		targets[i] = filepath.Join(root, strconv.Itoa(n)) // device paths: unambiguous across tt-smi versions
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, append([]string{"-r"}, targets...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s -r %s: %w: %s", bin, strings.Join(targets, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Join renders chip indices as a comma list (TT_VISIBLE_DEVICES format).
func Join(chips []int) string {
	s := make([]string, len(chips))
	for i, n := range chips {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

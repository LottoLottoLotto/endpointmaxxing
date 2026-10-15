// Package lock serializes read-modify-write cycles across substrate processes
// (CLI invocations, the gateway, MCP sessions) with advisory flock(2) locks.
package lock

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// With holds an exclusive lock on path (created if absent) while fn runs.
func With(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

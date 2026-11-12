// Package lock serializes read-modify-write cycles across substrate processes
// (CLI invocations, the gateway, MCP sessions) with advisory whole-file locks:
// flock(2) on Unix, LockFileEx on Windows. A lock lives as long as the open file.
package lock

import (
	"fmt"
	"os"
	"path/filepath"
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
	if err := lockFile(f, true); err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}
	defer Unlock(f)
	return fn()
}

// Try takes an exclusive lock on f without waiting; an error means another process holds it.
func Try(f *os.File) error { return lockFile(f, false) }

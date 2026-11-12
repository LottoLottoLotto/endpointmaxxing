//go:build unix

package lock

import (
	"os"
	"syscall"
)

func lockFile(f *os.File, wait bool) error {
	how := syscall.LOCK_EX
	if !wait {
		how |= syscall.LOCK_NB
	}
	for {
		if err := syscall.Flock(int(f.Fd()), how); err != syscall.EINTR {
			return err
		}
	}
}

// Unlock releases the lock on f (closing f does too).
func Unlock(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

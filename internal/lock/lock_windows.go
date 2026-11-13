package lock

import (
	"os"
	"syscall"
	"unsafe"
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
)

var (
	kernel32     = syscall.NewLazyDLL("kernel32.dll")
	lockFileEx   = kernel32.NewProc("LockFileEx")
	unlockFileEx = kernel32.NewProc("UnlockFileEx")
)

// The lock covers the whole possible length of the file, from offset 0 (a zero Overlapped).

func lockFile(f *os.File, wait bool) error {
	flags := uintptr(lockfileExclusiveLock)
	if !wait {
		flags |= lockfileFailImmediately
	}
	var ol syscall.Overlapped
	ok, _, err := lockFileEx.Call(f.Fd(), flags, 0, 0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(&ol)))
	if ok == 0 {
		return err
	}
	return nil
}

// Unlock releases the lock on f (closing f does too).
func Unlock(f *os.File) {
	var ol syscall.Overlapped
	unlockFileEx.Call(f.Fd(), 0, 0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(&ol)))
}

// NoFollow is the Unix open flag that refuses a symbolic link; Windows has no such flag for os.OpenFile.
const NoFollow = 0

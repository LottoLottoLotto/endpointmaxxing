package audit

import (
	"os"
	"syscall"
)

const fileAppendData = 0x0004

// openAppend opens the audit file for appending. Several writers (the gateway, CLI commands) keep it open,
// and one of them renames it when it rotates: Windows allows that only when every open handle shares
// delete access, which os.OpenFile does not ask for.
func openAppend(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, fileAppendData,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

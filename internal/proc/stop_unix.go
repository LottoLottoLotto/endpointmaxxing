//go:build unix

package proc

import (
	"errors"
	"os/exec"
	"syscall"
)

// Detach starts cmd in its own session: it outlives the caller and leads a process group (pgid == pid).
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// Signal sends sig to the engine's process group.
func Signal(leader int, sig syscall.Signal) error {
	if err := syscall.Kill(-leader, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// Kill SIGKILLs the engine's group and every member, including ones that left the group.
func Kill(leader int) {
	victims := append(Members(leader), leader)
	syscall.Kill(-leader, syscall.SIGKILL)
	for _, p := range victims {
		syscall.Kill(p, syscall.SIGKILL)
	}
}

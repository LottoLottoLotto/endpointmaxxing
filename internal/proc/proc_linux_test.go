package proc

import (
	"os/exec"
	"slices"
	"testing"
)

// A process whose main thread exited while another thread runs on is listed as a zombie, yet still holds
// its resources (a vLLM worker releasing GPU memory looks like this for seconds). It must count as running:
// otherwise a stop returns, and the next start begins, while the old engine still holds the GPU.
func TestProcessWithLiveThreadsIsNotGone(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("needs python3 to make a process whose main thread exits first")
	}
	cmd := exec.Command(python, "-c", `
import ctypes, threading, time
threading.Thread(target=lambda: time.sleep(60)).start()
time.sleep(0.2)
ctypes.CDLL(None).syscall(60, 0)  # SYS_exit: ends the main thread only
`)
	Detach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	pid := cmd.Process.Pid
	t.Cleanup(func() { Kill(pid, 0) })
	waitFor(t, "the main thread to exit", func() bool {
		f := stat(pid)
		return len(f) > 0 && f[0] == "Z"
	})
	if Start(pid) == 0 {
		t.Fatal("a process with a live thread was reported gone")
	}
	if !slices.Contains(Members(pid, 0), pid) {
		t.Fatal("a process with a live thread is missing from its engine's members")
	}
	Kill(pid, 0)
	waitFor(t, "the process to be gone after the kill", func() bool { return Start(pid) == 0 && len(Members(pid, 0)) == 0 })
}

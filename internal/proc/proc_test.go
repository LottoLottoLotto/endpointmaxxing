package proc

import (
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for an engine: with PROC_TEST_CHILD set it starts one worker
// (PROC_TEST_CHILD=leader) and then just stays alive.
func TestMain(m *testing.M) {
	switch os.Getenv("PROC_TEST_CHILD") {
	case "leader":
		worker := exec.Command(os.Args[0])
		worker.Env = append(os.Environ(), "PROC_TEST_CHILD=worker")
		if err := worker.Start(); err != nil {
			os.Exit(3)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	case "worker":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEngineIsTrackedAndKilledAsAWhole(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "PROC_TEST_CHILD=leader")
	Detach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait() // reap, as the launcher does
	leader := cmd.Process.Pid
	t.Cleanup(func() { Kill(leader) })

	var start uint64
	waitFor(t, "the engine to be seen", func() bool { start = Start(leader); return start != 0 })
	if again := Start(leader); again != start {
		t.Fatalf("start token changed for a running process: %d then %d", start, again)
	}
	var members []int
	waitFor(t, "the engine and its worker to be listed", func() bool { members = Members(leader); return len(members) >= 2 })
	if !slices.Contains(members, leader) {
		t.Fatalf("members %v do not include the leader %d", members, leader)
	}
	if slices.Contains(members, os.Getpid()) {
		t.Fatalf("members %v include the launcher itself", members)
	}
	if RSSMiB(members) <= 0 {
		t.Fatalf("no resident memory reported for %v", members)
	}

	Kill(leader)
	waitFor(t, "the engine to be gone", func() bool { return Start(leader) == 0 })
	waitFor(t, "its worker to be gone", func() bool { return len(Members(leader)) == 0 })
}

func TestStartOfAMissingProcessIsZero(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "PROC_TEST_CHILD=worker")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	cmd.Process.Kill()
	cmd.Wait()
	if s := Start(pid); s != 0 {
		t.Fatalf("Start of an exited process = %d, want 0", s)
	}
}

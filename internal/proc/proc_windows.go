package proc

import (
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	processQueryLimitedInformation = 0x1000
	processTerminate               = 0x0001
	stillActive                    = 259
	createNewProcessGroup          = 0x00000200
)

var (
	psapi                = syscall.NewLazyDLL("psapi.dll")
	getProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
)

// Start is the process creation time (100 ns intervals since 1601).
func Start(pid int) uint64 {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return 0
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil || code != stillActive {
		return 0 // exited; the handle of some other process keeps the id around
	}
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0
	}
	return uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
}

// Members lists the engine's live process tree: leader (when it is still the engine) and its descendants.
// Windows has no sessions or inherited process groups to ask for; a child whose parent exited is still
// found, because its recorded parent id stays.
//
// A process id is reused once its process is gone, so a recorded parent id may name a process that is not
// the parent. Creation times tell them apart: a child was created after its parent, and the engine's
// children were created before any later process took the engine's id.
func Members(leader int, started uint64) []int {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer syscall.CloseHandle(snap)
	children := map[int][]int{}
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		if pid, parent := int(e.ProcessID), int(e.ParentProcessID); pid != parent {
			children[parent] = append(children[parent], pid)
		}
	}
	var out []int
	now := Start(leader)
	// The engine's children were created in [from, until): from the engine's start, and before a later
	// owner of its process id started (until 0: no later owner).
	from, until := started, uint64(0)
	switch {
	case now != 0 && (started == 0 || now == started): // the leader is the engine
		out, from = append(out, leader), now
	case now != 0: // the id belongs to a later process: neither it nor its children are the engine's
		until = now
	}
	type node struct {
		pid         int
		from, until uint64
	}
	seen := map[int]bool{leader: true}
	for queue := []node{{leader, from, until}}; len(queue) > 0; queue = queue[1:] {
		parent := queue[0]
		for _, c := range children[parent.pid] {
			start := Start(c)
			if seen[c] || start == 0 || start < parent.from || (parent.until != 0 && start >= parent.until) {
				continue
			}
			seen[c] = true
			out = append(out, c)
			queue = append(queue, node{c, start, 0})
		}
	}
	return out
}

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

// RSSMiB sums the working sets of pids.
func RSSMiB(pids []int) int64 {
	var bytes uint64
	for _, p := range pids {
		h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(p))
		if err != nil {
			continue
		}
		var c processMemoryCounters
		c.cb = uint32(unsafe.Sizeof(c))
		if ok, _, _ := getProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.cb)); ok != 0 {
			bytes += uint64(c.workingSetSize)
		}
		syscall.CloseHandle(h)
	}
	return int64(bytes >> 20)
}

// Detach starts cmd as the root of its own process group, so a Ctrl-C in the caller's console does not
// reach it.
func Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}

// Signal stops the engine. Windows cannot deliver a signal to another process, so every stop is a forced
// one: the engine's process tree is terminated whatever sig is.
func Signal(leader int, _ syscall.Signal) error {
	Kill(leader, 0)
	return nil
}

// Kill terminates the engine's process tree.
func Kill(leader int, started uint64) {
	for _, p := range Members(leader, started) {
		if h, err := syscall.OpenProcess(processTerminate, false, uint32(p)); err == nil {
			syscall.TerminateProcess(h, 1)
			syscall.CloseHandle(h)
		}
	}
}

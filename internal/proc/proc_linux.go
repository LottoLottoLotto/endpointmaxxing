package proc

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// stat reads /proc/<pid>/stat fields after the command name (field 3 onward); nil when gone.
func stat(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(s[i+1:])
}

// Start is the process start time in clock ticks.
func Start(pid int) uint64 {
	f := stat(pid)
	if len(f) < 20 || f[0] == "Z" {
		return 0
	}
	t, _ := strconv.ParseUint(f[19], 10, 64)
	return t
}

// Members lists live processes whose session id is leader (an engine and all its workers, also ones that
// left its process group).
func Members(leader int, _ uint64) []int {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	want := strconv.Itoa(leader)
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		// Fields after ')': state ppid pgrp session ...
		if f := stat(pid); len(f) > 3 && f[3] == want && f[0] != "Z" {
			out = append(out, pid)
		}
	}
	return out
}

// RSSMiB sums resident memory of pids.
func RSSMiB(pids []int) int64 {
	var kb int64
	for _, p := range pids {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", p))
		if err != nil {
			continue
		}
		for _, l := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
				n, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 10, 64)
				kb += n
			}
		}
	}
	return kb / 1024
}

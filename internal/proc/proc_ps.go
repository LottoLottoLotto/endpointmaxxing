//go:build unix && !linux

package proc

import (
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Without /proc (macOS, the BSDs) the answers come from ps(1), which every such system ships.

func ps(args ...string) string {
	cmd := exec.Command("ps", args...)
	cmd.Env = []string{"LC_ALL=C", "PATH=/bin:/usr/bin:/sbin:/usr/sbin"}
	out, _ := cmd.Output() // ps exits 1 when no process matches; the empty output says the same
	return string(out)
}

// Start is the process start time in seconds since the epoch.
func Start(pid int) uint64 {
	// "Z   Thu Nov 12 11:30:59 2026": state first, because lstart itself has spaces.
	// One -o per column: BSD ps reads everything after "=" as that column's header.
	f := strings.Fields(ps("-o", "stat=", "-o", "lstart=", "-p", strconv.Itoa(pid)))
	if len(f) < 6 || strings.HasPrefix(f[0], "Z") {
		return 0
	}
	t, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(f[1:6], " "), time.Local)
	if err != nil || t.Unix() <= 0 {
		return 0
	}
	return uint64(t.Unix())
}

// Members lists live processes in the process group leader leads (Detach made the engine a group leader).
func Members(leader int, _ uint64) []int {
	var out []int
	for _, line := range strings.Split(ps("-A", "-o", "pid=", "-o", "pgid=", "-o", "stat="), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || strings.HasPrefix(f[2], "Z") {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pgid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil && pgid == leader {
			out = append(out, pid)
		}
	}
	return out
}

// RSSMiB sums resident memory of pids.
func RSSMiB(pids []int) int64 {
	if len(pids) == 0 {
		return 0
	}
	list := make([]string, len(pids))
	for i, p := range pids {
		list[i] = strconv.Itoa(p)
	}
	var kb int64
	for _, f := range strings.Fields(ps("-o", "rss=", "-p", strings.Join(list, ","))) {
		n, _ := strconv.ParseInt(f, 10, 64)
		kb += n
	}
	return kb / 1024
}

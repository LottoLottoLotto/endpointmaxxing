package bench

import (
	"os/exec"
	"strconv"
	"strings"
)

func run(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func cpuModel() string { return run("sysctl", "-n", "machdep.cpu.brand_string") }

func memKB() int64 {
	bytes, _ := strconv.ParseInt(run("sysctl", "-n", "hw.memsize"), 10, 64)
	return bytes >> 10
}

func osName() string {
	if v := run("sw_vers", "-productVersion"); v != "" {
		return "macOS " + v
	}
	return "macOS"
}

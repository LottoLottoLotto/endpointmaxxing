// Package gpu reads NVIDIA GPU state through nvidia-smi (absent tool = no GPUs).
package gpu

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// GPU is one device reading.
type GPU struct {
	Index       int     `json:"index"`
	Name        string  `json:"name"`
	MemUsedMiB  int64   `json:"memory_used_mib"`
	MemTotalMiB int64   `json:"memory_total_mib"`
	UtilPct     int64   `json:"utilization_pct"`
	PowerW      float64 `json:"power_w"`
}

// Available reports whether nvidia-smi is on PATH.
func Available() bool {
	_, err := exec.LookPath("nvidia-smi")
	return err == nil
}

func query(args ...string) ([][]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi", append(args, "--format=csv,noheader,nounits")...).Output()
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		f := strings.Split(l, ",")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		rows = append(rows, f)
	}
	return rows, nil
}

func i64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// List reads every GPU; nil without nvidia-smi.
func List() ([]GPU, error) {
	if !Available() {
		return nil, nil
	}
	rows, err := query("--query-gpu=index,name,memory.used,memory.total,utilization.gpu,power.draw")
	if err != nil {
		return nil, err
	}
	var out []GPU
	for _, f := range rows {
		if len(f) != 6 {
			continue
		}
		p, _ := strconv.ParseFloat(f[5], 64) // "[N/A]" on some boards
		out = append(out, GPU{Index: int(i64(f[0])), Name: f[1], MemUsedMiB: i64(f[2]), MemTotalMiB: i64(f[3]), UtilPct: i64(f[4]), PowerW: p})
	}
	return out, nil
}

// ProcessMemory maps pid to GPU memory in MiB, summed over GPUs.
func ProcessMemory() (map[int]int64, error) {
	if !Available() {
		return nil, nil
	}
	rows, err := query("--query-compute-apps=pid,used_memory")
	if err != nil {
		return nil, err
	}
	out := map[int]int64{}
	for _, f := range rows {
		if len(f) != 2 {
			continue
		}
		out[int(i64(f[0]))] += i64(f[1])
	}
	return out, nil
}

// Package gpu reads NVIDIA GPU state through nvidia-smi (absent tool = no GPUs).
// Indices are nvidia-smi's (PCI bus order); launches set CUDA_DEVICE_ORDER=PCI_BUS_ID
// so CUDA_VISIBLE_DEVICES means the same numbers.
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
	UUID        string  `json:"uuid"`
	Name        string  `json:"name"`
	MemUsedMiB  int64   `json:"memory_used_mib"`
	MemTotalMiB int64   `json:"memory_total_mib"`
	UtilPct     int64   `json:"utilization_pct"`
	PowerW      float64 `json:"power_w"`
}

// FreeMiB is total minus used.
func (g GPU) FreeMiB() int64 { return g.MemTotalMiB - g.MemUsedMiB }

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
	rows, err := query("--query-gpu=index,uuid,name,memory.used,memory.total,utilization.gpu,power.draw")
	if err != nil {
		return nil, err
	}
	var out []GPU
	for _, f := range rows {
		if len(f) != 7 {
			continue
		}
		p, _ := strconv.ParseFloat(f[6], 64) // "[N/A]" on some boards
		out = append(out, GPU{Index: int(i64(f[0])), UUID: f[1], Name: f[2], MemUsedMiB: i64(f[3]),
			MemTotalMiB: i64(f[4]), UtilPct: i64(f[5]), PowerW: p})
	}
	return out, nil
}

// Usage is GPU memory held by processes: pid -> GPU index -> MiB.
type Usage map[int]map[int]int64

// Of sums the memory of pids per GPU.
func (u Usage) Of(pids []int) map[int]int64 {
	out := map[int]int64{}
	for _, p := range pids {
		for g, m := range u[p] {
			out[g] += m
		}
	}
	return out
}

// ProcessUsage reads per-process GPU memory; gpus maps UUIDs to indices (from List).
func ProcessUsage(gpus []GPU) (Usage, error) {
	if !Available() {
		return nil, nil
	}
	rows, err := query("--query-compute-apps=pid,gpu_uuid,used_memory")
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	for _, g := range gpus {
		idx[g.UUID] = g.Index
	}
	out := Usage{}
	for _, f := range rows {
		if len(f) != 3 {
			continue
		}
		pid := int(i64(f[0]))
		if out[pid] == nil {
			out[pid] = map[int]int64{}
		}
		out[pid][idx[f[1]]] += i64(f[2])
	}
	return out, nil
}

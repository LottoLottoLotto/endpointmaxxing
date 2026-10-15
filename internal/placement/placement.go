// Package placement decides which GPUs a launch runs on and which running
// instances must be evicted to make room. It is pure: callers supply GPU
// readings, reservations and holders, and act on the Decision.
package placement

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// GPU is one device's memory state.
type GPU struct {
	Index    int
	TotalMiB int64
	UsedMiB  int64 // as nvidia-smi reports: everything resident now
}

// Need is a launch's memory requirement.
type Need struct {
	// Count is how many GPUs the launch spans (tensor parallelism).
	Count int
	// PerGPU returns the MiB needed on a GPU with the given total memory.
	PerGPU func(totalMiB int64) int64
	// Exclusive needs the whole GPU: every holder on it is evicted.
	Exclusive bool
	// Pinned restricts the launch to exactly these GPU indices (from --gpus).
	Pinned []int
	// Source explains the estimate (declared, measured, fraction, weights, exclusive).
	Source string
}

// Holder is a running instance occupying GPU memory.
type Holder struct {
	ID        string
	Evictable bool
	Idle      bool      // no in-flight requests
	LastUsed  time.Time //
	MiB       map[int]int64
}

// Decision is where to launch and what to stop first.
type Decision struct {
	GPUs   []int
	Evict  []string
	PerGPU map[int]int64 // planned MiB per chosen GPU
}

// Error explains why nothing fits; Blockers are non-evictable holders in the way.
type Error struct {
	Msg      string
	Blockers []string
}

func (e *Error) Error() string { return e.Msg }

// Decide picks the GPU set needing the fewest evictions (then the most free memory
// left over). reserved is memory promised to instances still loading, per GPU.
func Decide(gpus []GPU, reserved map[int]int64, holders []Holder, n Need) (*Decision, error) {
	if n.Count < 1 {
		n.Count = 1
	}
	byIndex := map[int]GPU{}
	for _, g := range gpus {
		byIndex[g.Index] = g
	}
	var sets [][]int
	if len(n.Pinned) > 0 {
		for _, i := range n.Pinned {
			if _, ok := byIndex[i]; !ok {
				return nil, &Error{Msg: fmt.Sprintf("GPU %d does not exist", i)}
			}
		}
		sets = [][]int{n.Pinned}
	} else {
		idx := make([]int, 0, len(gpus))
		for _, g := range gpus {
			idx = append(idx, g.Index)
		}
		sort.Ints(idx)
		if n.Count > len(idx) {
			return nil, &Error{Msg: fmt.Sprintf("needs %d GPUs, %d present", n.Count, len(idx))}
		}
		sets = combinations(idx, n.Count)
	}

	// Victim order: idle before busy, then least recently used.
	evictable := make([]Holder, 0, len(holders))
	for _, h := range holders {
		if h.Evictable {
			evictable = append(evictable, h)
		}
	}
	sort.SliceStable(evictable, func(i, j int) bool {
		if evictable[i].Idle != evictable[j].Idle {
			return evictable[i].Idle
		}
		return evictable[i].LastUsed.Before(evictable[j].LastUsed)
	})

	var best *Decision
	var bestLeft int64
	var closest string
	var blockers []string
	for _, set := range sets {
		d, left, why, block := try(set, byIndex, reserved, holders, evictable, n)
		if d == nil {
			if closest == "" {
				closest, blockers = why, block
			}
			continue
		}
		if best == nil || len(d.Evict) < len(best.Evict) || (len(d.Evict) == len(best.Evict) && left > bestLeft) {
			best, bestLeft = d, left
		}
	}
	if best == nil {
		return nil, &Error{Msg: "not enough GPU memory: " + closest, Blockers: blockers}
	}
	return best, nil
}

func try(set []int, byIndex map[int]GPU, reserved map[int]int64, holders, evictable []Holder, n Need) (*Decision, int64, string, []string) {
	need := map[int]int64{}
	free := map[int]int64{}
	for _, i := range set {
		g := byIndex[i]
		if !n.Exclusive {
			need[i] = n.PerGPU(g.TotalMiB)
		}
		free[i] = g.TotalMiB - g.UsedMiB - reserved[i]
	}
	inSet := func(h Holder) bool {
		for _, i := range set {
			if h.MiB[i] > 0 {
				return true
			}
		}
		return false
	}
	short := func() bool {
		for _, i := range set {
			if free[i] < need[i] {
				return true
			}
		}
		return false
	}
	d := &Decision{GPUs: set, PerGPU: need}
	if n.Exclusive {
		// A whole-GPU launch shares with no substrate instance: every evictable holder on the
		// set goes, and it takes whatever is then free (non-substrate use such as a desktop stays).
		var block []string
		for _, h := range holders {
			if inSet(h) && !h.Evictable {
				block = append(block, h.ID)
			}
		}
		for _, i := range set {
			if reserved[i] > 0 {
				block = append(block, fmt.Sprintf("an instance loading on GPU %d", i))
			}
		}
		if len(block) > 0 {
			return nil, 0, "a whole-GPU launch cannot share with " + strings.Join(block, ", "), block
		}
		for _, h := range evictable {
			if inSet(h) {
				d.Evict = append(d.Evict, h.ID)
				for _, i := range set {
					free[i] += h.MiB[i]
				}
			}
		}
		for _, i := range set {
			need[i] = free[i]
		}
		return d, 0, "", nil
	}
	for _, h := range evictable {
		if !short() {
			break
		}
		if !inSet(h) || contains(d.Evict, h.ID) {
			continue
		}
		d.Evict = append(d.Evict, h.ID)
		for _, i := range set {
			free[i] += h.MiB[i]
		}
	}
	if short() {
		var parts []string
		for _, i := range set {
			parts = append(parts, fmt.Sprintf("GPU %d needs %d MiB, %d MiB free after evicting what may be evicted", i, need[i], free[i]))
		}
		var block []string
		for _, h := range holders {
			if !h.Evictable && inSet(h) {
				block = append(block, h.ID)
			}
		}
		why := strings.Join(parts, "; ")
		if len(block) > 0 {
			why += " (held by " + strings.Join(block, ", ") + ", which the gateway does not evict)"
		}
		return nil, 0, why, block
	}
	var left int64
	for _, i := range set {
		left += free[i] - need[i]
	}
	return d, left, "", nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// combinations returns every k-subset of xs, in order.
func combinations(xs []int, k int) [][]int {
	var out [][]int
	var rec func(start int, cur []int)
	rec = func(start int, cur []int) {
		if len(cur) == k {
			out = append(out, append([]int(nil), cur...))
			return
		}
		for i := start; i < len(xs); i++ {
			rec(i+1, append(cur, xs[i]))
		}
	}
	rec(0, nil)
	return out
}

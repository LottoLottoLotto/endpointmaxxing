package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/gpu"
	"github.com/lotto/substrate/internal/paths"
	"github.com/lotto/substrate/internal/placement"
	"github.com/lotto/substrate/internal/route"
	"github.com/lotto/substrate/internal/runtime"
	"github.com/lotto/substrate/internal/spec"
	"github.com/lotto/substrate/internal/store"
	"github.com/lotto/substrate/internal/tenstorrent"
)

// Evictor stops the instances placement chose to make room.
type Evictor func(ids []string) error

func placementLock() string { return filepath.Join(paths.State(), "instances", ".placement.lock") }

// placed is a placement outcome to stamp onto the plan.
type placed struct {
	gpus    []int
	perGPU  int64
	explain string
	evicted []string
}

// WouldEvict reports which instances launching t through the gateway would stop
// (empty when it fits as things are). Errors are capacity problems eviction cannot fix.
func (a *App) WouldEvict(t *Target, o LaunchOpts) ([]string, error) {
	o.Evict = func([]string) error { return nil }
	o.dryRun = true
	flags := map[string]string{}
	for k, v := range o.Flags {
		flags[k] = v
	}
	o.Flags = flags
	t = a.asHost(t, &o)
	p, err := a.place(t, &o)
	if err != nil || p == nil {
		return nil, err
	}
	return p.evicted, nil
}

func (p *placed) apply(plan *runtime.Plan) {
	if p == nil {
		return
	}
	plan.GPUs, plan.PlannedMiB, plan.Placement = p.gpus, p.perGPU, p.explain
}

// measureWindow is how far back measured footprints are trusted.
const measureWindow = 30 * 24 * time.Hour

// EngineFor returns id, or the routed engine for t when id is empty.
func (a *App) EngineFor(t *Target, id string) (string, error) {
	if id != "" {
		return id, nil
	}
	return route.Pick(a.Route(t))
}

// PlanPlaced is Plan with placement applied (GPU choice and memory note), without
// evicting or launching: what `run --dry-run` shows, including what a launch
// through the gateway would evict.
func (a *App) PlanPlaced(t *Target, o LaunchOpts) (*runtime.Plan, string, error) {
	o.Evict = func([]string) error { return nil } // record, don't stop
	o.dryRun = true
	flags := map[string]string{}
	for k, v := range o.Flags {
		flags[k] = v
	}
	o.Flags = flags
	t = a.asHost(t, &o)
	p, err := a.place(t, &o)
	if err != nil {
		return nil, "", err
	}
	plan, id, err := a.Plan(t, o)
	if err != nil {
		return nil, "", err
	}
	p.apply(plan)
	return plan, id, nil
}

// Estimate returns the GPU memory need for launching t on engine engineID with flags.
func (a *App) Estimate(t *Target, engineID string, flags map[string]string) (placement.Need, error) {
	e, err := a.Engine(engineID)
	if err != nil {
		return placement.Need{}, err
	}
	n := placement.Need{Count: 1}
	if g := flags["gpus"]; g != "" {
		for _, s := range strings.Split(g, ",") {
			i, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				return n, errs.New(errs.Usage, "--gpus %q: want comma-separated GPU indices", g)
			}
			n.Pinned = append(n.Pinned, i)
		}
		n.Count = len(n.Pinned)
	}
	if tp, err := strconv.Atoi(flags["tp"]); err == nil && tp > 1 {
		if len(n.Pinned) > 0 && len(n.Pinned) != tp {
			return n, errs.New(errs.Usage, "--tp %d needs %d GPUs, --gpus names %d", tp, tp, len(n.Pinned))
		}
		n.Count = tp
	}
	count := int64(n.Count)
	constant := func(mib int64, src string) {
		per := mib / count
		n.PerGPU, n.Source = func(int64) int64 { return per }, src
	}
	switch mode := e.MemoryMode(); {
	case a.Cfg.Gateway.Placement == config.PlacementExclusive || mode == "exclusive":
		n.Exclusive, n.Source = true, "exclusive"
		n.PerGPU = func(total int64) int64 { return total }
	case t.Model.VRAMMiB > 0:
		constant(t.Model.VRAMMiB, fmt.Sprintf("declared %d MiB", t.Model.VRAMMiB))
	case mode == "fraction":
		// These engines preallocate this share; CUDA context and buffers outside the pool
		// come on top (overhead_mib), and a measured footprint wins when it is larger.
		f := e.Memory.DefaultFraction
		if v, err := strconv.ParseFloat(flags["mem_fraction"], 64); err == nil {
			f = v
		}
		over := e.Memory.OverheadMiB
		measured := a.peak(t.Model.Name, engineID) * 105 / 100 / count
		n.PerGPU = func(total int64) int64 { return max(int64(f*float64(total))+over, measured) }
		n.Source = fmt.Sprintf("%.2f of each GPU + %d MiB", f, over)
		if measured > 0 {
			n.Source += fmt.Sprintf(", measured %d MiB", measured*count*100/105)
		}
	default:
		if peak := a.peak(t.Model.Name, engineID); peak > 0 {
			constant(peak*105/100, fmt.Sprintf("measured peak %d MiB", peak))
			break
		}
		factor, over := e.Memory.Factor, e.Memory.OverheadMiB
		if factor == 0 {
			factor = 1.1
		}
		if over == 0 {
			over = 1024
		}
		weights := int64(float64(t.Info.Bytes>>20) * factor)
		if flags["gpu_layers"] == "0" {
			weights = 0 // CPU-only: just the engine's buffers
		}
		per := weights/count + over
		n.PerGPU = func(int64) int64 { return per }
		n.Source = fmt.Sprintf("weights %d MiB × %.2f + %d MiB", t.Info.Bytes>>20, factor, over)
		if t.Info.Bytes == 0 {
			n.Source = fmt.Sprintf("unknown size: %d MiB overhead only", over)
		}
	}
	return n, nil
}

// peak is the largest footprint measured for model on engine (0 without logging or samples).
func (a *App) peak(model, engine string) int64 {
	if !a.Cfg.Logging.Enabled {
		return 0
	}
	db, err := a.DB()
	if err != nil {
		return 0
	}
	p, _ := db.PeakGPU(model, engine, time.Now().Add(-measureWindow))
	return p
}

// place decides devices for t and evicts when allowed: NVIDIA GPUs by memory (it sets
// the gpus flag on o), Tenstorrent chips as whole devices (it sets o.devices).
func (a *App) place(t *Target, o *LaunchOpts) (*placed, error) {
	engineID, err := a.EngineFor(t, o.Engine)
	if err != nil {
		return nil, err
	}
	e, err := a.Engine(engineID)
	if err != nil {
		return nil, err
	}
	if t.host && HostInstance(engineID) != nil {
		return nil, nil // the running host takes the model: nothing to place
	}
	if e.AcceleratorKind() == spec.AccelTenstorrent {
		// Chips are always assigned (the engine needs TT_VISIBLE_DEVICES and the locks);
		// --force and placement off only forbid evictions.
		return a.placeChips(t, o, e, engineID)
	}
	if o.Force || a.Cfg.Gateway.Placement == config.PlacementOff || e.MemoryMode() == "none" {
		return nil, nil
	}
	gpus, err := gpu.List()
	if err != nil || len(gpus) == 0 {
		return nil, nil // no NVIDIA GPUs visible: nothing to place
	}
	flags := map[string]string{}
	for k, v := range t.Model.Flags {
		flags[k] = v
	}
	for k, v := range o.Flags {
		flags[k] = v
	}
	need, err := a.Estimate(t, engineID, flags)
	if err != nil {
		return nil, err
	}
	usage, _ := gpu.ProcessUsage(gpus)
	pg := make([]placement.GPU, len(gpus))
	for i, g := range gpus {
		pg[i] = placement.GPU{Index: g.Index, TotalMiB: g.MemTotalMiB, UsedMiB: g.MemUsedMiB}
	}
	ins, _ := runtime.List()
	reserved := map[int]int64{}
	var holders []placement.Holder
	name := o.Name
	if name == "" {
		name = t.Model.Name
	}
	for _, in := range ins {
		if !in.Alive() {
			continue
		}
		held := usage.Of(runtime.SessionPIDs(in.PID))
		if in.ReadyAt == nil && in.PlannedMiB > 0 {
			for _, g := range in.GPUs {
				if r := in.PlannedMiB - held[g]; r > 0 {
					reserved[g] += r
				}
			}
		}
		h := placement.Holder{ID: in.ID, MiB: held, Idle: true, LastUsed: in.StartedAt,
			Evictable: in.Name != name && contains(a.Cfg.Gateway.EvictOwners, in.Owner) && in.ReadyAt != nil}
		if o.Activity != nil {
			h.Idle, h.LastUsed = o.Activity(in.ID)
		}
		holders = append(holders, h)
	}
	d, err := placement.Decide(pg, reserved, holders, need)
	if err != nil {
		return nil, errs.New(errs.Capacity, "%s on %s (%s): %v", t.Model.Name, engineID, need.Source, err)
	}
	if len(d.Evict) > 0 {
		if o.Evict == nil {
			return nil, errs.New(errs.Capacity, "%s on %s (%s) fits only after stopping %s (substrate stop ..., or launch through the gateway)",
				t.Model.Name, engineID, need.Source, strings.Join(d.Evict, ", "))
		}
		if err := o.Evict(d.Evict); err != nil {
			return nil, fmt.Errorf("evicting for %s: %w", t.Model.Name, err)
		}
	}
	sel := make([]string, len(d.GPUs))
	for i, g := range d.GPUs {
		sel[i] = strconv.Itoa(g)
	}
	if _, maps := e.Flags["gpus"]; maps && len(need.Pinned) == 0 {
		if o.Flags == nil {
			o.Flags = map[string]string{}
		}
		o.Flags["gpus"] = strings.Join(sel, ",")
	}
	explain := fmt.Sprintf("GPU %s, %d MiB each (%s)", strings.Join(sel, ","), d.PerGPU[d.GPUs[0]], need.Source)
	if len(d.Evict) > 0 {
		verb := "evicted"
		if o.dryRun {
			verb = "would evict"
		}
		explain += "; " + verb + " " + strings.Join(d.Evict, ", ")
	}
	return &placed{gpus: d.GPUs, perGPU: d.PerGPU[d.GPUs[0]], explain: explain, evicted: d.Evict}, nil
}

// placeChips assigns whole Tenstorrent chips to t, evicting evictable instances that hold them.
func (a *App) placeChips(t *Target, o *LaunchOpts, e *runtime.Engine, engineID string) (*placed, error) {
	chips := tenstorrent.Chips()
	if len(chips) == 0 {
		return nil, errs.New(errs.Unsupported, "%s runs on %s, which needs Tenstorrent chips; none are present (/dev/tenstorrent)", t.Model.Name, engineID)
	}
	count := t.Model.Devices
	if count == 0 {
		count = e.Accelerator.Devices
	}
	if count == 0 {
		count = 1
	}
	name := o.Name
	if name == "" {
		name = t.Model.Name
	}
	ins, _ := runtime.List()
	owned := map[int]bool{}
	var holders []placement.Holder
	for _, in := range ins {
		if in.Accelerator != spec.AccelTenstorrent || len(in.Devices) == 0 || !in.Alive() {
			continue
		}
		h := placement.Holder{ID: in.ID, MiB: map[int]int64{}, Idle: true, LastUsed: in.StartedAt,
			Evictable: !o.Force && a.Cfg.Gateway.Placement != config.PlacementOff && in.Name != name &&
				contains(a.Cfg.Gateway.EvictOwners, in.Owner) && in.ReadyAt != nil}
		for _, d := range in.Devices {
			h.MiB[d] = 1
			owned[d] = true
		}
		if o.Activity != nil {
			h.Idle, h.LastUsed = o.Activity(in.ID)
		}
		holders = append(holders, h)
	}
	external := map[int]bool{}
	for _, c := range chips {
		if !owned[c] && (runtime.TT.Held(c) || len(tenstorrent.Users(c)) > 0) {
			external[c] = true
		}
	}
	d, err := placement.DecideChips(chips, external, holders, count)
	if err != nil {
		return nil, errs.New(errs.Capacity, "%s on %s (%d chip(s)): %v", t.Model.Name, engineID, count, err)
	}
	if len(d.Evict) > 0 {
		if o.Evict == nil {
			return nil, errs.New(errs.Capacity, "%s on %s needs chips %s, held by %s (substrate stop ..., or launch through the gateway)",
				t.Model.Name, engineID, tenstorrent.Join(d.GPUs), strings.Join(d.Evict, ", "))
		}
		if err := o.Evict(d.Evict); err != nil {
			return nil, fmt.Errorf("evicting for %s: %w", t.Model.Name, err)
		}
	}
	o.devices = d.GPUs
	explain := fmt.Sprintf("Tenstorrent chip(s) %s", tenstorrent.Join(d.GPUs))
	if len(d.Evict) > 0 {
		verb := "evicted"
		if o.dryRun {
			verb = "would evict"
		}
		explain += "; " + verb + " " + strings.Join(d.Evict, ", ")
	}
	return &placed{explain: explain, evicted: d.Evict}, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// AwaitReady waits for a launched instance and records its measured GPU footprint,
// which later launches of the same model use instead of an estimate.
func (a *App) AwaitReady(ctx context.Context, in *runtime.Instance, timeout time.Duration) error {
	if err := in.AwaitStart(ctx, timeout); err != nil {
		return err
	}
	a.RecordFootprint(in)
	return nil
}

// RecordFootprint samples an instance's GPU memory and RSS into the usage database.
func (a *App) RecordFootprint(in *runtime.Instance) {
	if !a.Cfg.Logging.Enabled {
		return
	}
	gpus, err := gpu.List()
	if err != nil || len(gpus) == 0 {
		return
	}
	usage, _ := gpu.ProcessUsage(gpus)
	pids := runtime.SessionPIDs(in.PID)
	s := store.InstanceSample{TS: time.Now().UnixMilli(), Instance: in.ID, Engine: in.Engine, Model: in.Name,
		RSSMiB: runtime.RSSMiB(pids), Processes: int64(len(pids))}
	for _, m := range usage.Of(pids) {
		s.GPUMemMiB += m
	}
	db, err := a.DB()
	if err == nil {
		err = db.InsertSamples(nil, []store.InstanceSample{s})
	}
	if err != nil {
		a.Warn("recording footprint of %s: %v", in.ID, err)
	}
}

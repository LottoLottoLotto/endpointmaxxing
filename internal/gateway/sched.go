package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lotto/substrate/internal/app"
	"github.com/lotto/substrate/internal/audit"
	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/gpu"
	"github.com/lotto/substrate/internal/runtime"
	"github.com/lotto/substrate/internal/spec"
	"github.com/lotto/substrate/internal/store"
	"github.com/lotto/substrate/internal/tenstorrent"
)

// Run starts the background loops until ctx ends: exit monitor (and crash
// restarts), health checks, idle unload, resource sampling, retention, config
// watching and key-expiry alerts.
func (g *Gateway) Run(ctx context.Context) {
	g.watchEngines()
	go g.every(ctx, 5*time.Second, g.monitor)
	if iv := time.Duration(g.gw().HealthInterval); iv > 0 {
		go g.every(ctx, iv, g.healthCheck)
	}
	go g.every(ctx, 15*time.Second, g.idleUnload)
	if iv := time.Duration(g.App.Config().Logging.SampleInterval); g.App.Config().Logging.Enabled && iv > 0 {
		go g.every(ctx, iv, g.sample)
	}
	go g.every(ctx, 24*time.Hour, g.retention)
	go g.watchConfig(ctx, 2*time.Second)
	go g.every(ctx, time.Hour, g.checkKeyExpiry)
}

// retention prunes usage records past logging.retention_days, and the admin trail and rotated audit files
// past logging.audit_retention_days (both reloadable; 0 keeps everything).
func (g *Gateway) retention() {
	lc := g.App.Config().Logging
	if lc.RetentionDays <= 0 && lc.AuditRetentionDays <= 0 {
		return
	}
	before, auditBefore := Cutoffs(lc, time.Now())
	if db, err := g.App.DB(); err == nil && lc.Enabled {
		n, err := db.Prune(before, auditBefore)
		if err != nil {
			g.Log.Printf("retention: %v", err)
		} else {
			g.Log.Printf("retention: pruned %v", n)
		}
	}
	if p := audit.Path(lc.AuditFile); p != "" && !auditBefore.IsZero() {
		gone, err := audit.PruneRotated(p, auditBefore)
		if err != nil {
			g.Log.Printf("retention: audit files: %v", err)
		}
		if len(gone) > 0 {
			g.Log.Printf("retention: removed %d rotated audit file(s)", len(gone))
		}
	}
}

// Cutoffs turns the retention settings into prune cutoffs (zero: keep).
func Cutoffs(lc config.Logging, now time.Time) (before, auditBefore time.Time) {
	if lc.RetentionDays > 0 {
		before = now.AddDate(0, 0, -lc.RetentionDays)
	}
	if lc.AuditRetentionDays > 0 {
		auditBefore = now.AddDate(0, 0, -lc.AuditRetentionDays)
	}
	return before, auditBefore
}

func (g *Gateway) every(ctx context.Context, d time.Duration, fn func()) {
	fn()
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// ---- routing and starts ----

// pick returns the ready instances serving model with api, in round-robin
// order, starting one when allowed. started reports an autostart.
func (g *Gateway) pick(ctx context.Context, model, api string) ([]*runtime.Instance, bool, error) {
	if ins, err := g.ready(ctx, model, api); len(ins) > 0 || err != nil {
		return ins, false, err
	}
	if model == "" {
		return nil, false, errs.New(errs.Usage, "request has no model, and not exactly one instance is ready")
	}
	if !g.autostart() {
		return nil, false, errs.New(errs.NotFound, "no ready instance serves %q (autostart is off)", model)
	}
	g.Reload() // see a model added since; an invalid config keeps the running one (logged)
	if _, ok := g.App.RegisteredModel(model); !ok {
		return nil, false, errs.New(errs.NotFound, "model %q is neither running nor registered (see `substrate model add`)", model)
	}
	g.addWaiting(model, 1)
	defer g.addWaiting(model, -1)
	// Wait (as long as this client does) until the devices the model needs may be taken.
	if err := g.awaitTurn(ctx, model, ""); err != nil {
		return nil, false, err
	}
	// A start outlives the client that triggered it: other requests wait on it.
	in, err := g.ensure(context.WithoutCancel(ctx), model, "", api, 0)
	if err != nil {
		return nil, false, err
	}
	return []*runtime.Instance{in}, true, nil
}

func (g *Gateway) addWaiting(model string, d int) {
	g.activeMu.Lock()
	g.waiting[model] += d
	if g.waiting[model] <= 0 {
		delete(g.waiting, model)
	}
	g.activeMu.Unlock()
}

// swappable reports whether instance id may give up its devices now: it has been
// resident for min_residency and has no demand of its own, or it has held them for
// max_residency while others wait (its in-flight requests are then drained first).
func (g *Gateway) swappable(id string) bool {
	in, err := runtime.Get(id)
	if err != nil || !in.Alive() {
		return true
	}
	if in.ReadyAt == nil {
		return false // still loading: never interrupt a load
	}
	resident := time.Since(*in.ReadyAt)
	sw := g.gw().Swap
	if resident < time.Duration(sw.MinResidency) {
		return false
	}
	g.activeMu.Lock()
	demand := g.active[id] + g.waiting[in.Name]
	g.activeMu.Unlock()
	demand += g.lim.queued()[in.Name]
	return demand == 0 || resident >= time.Duration(sw.MaxResidency)
}

// awaitTurn blocks until starting model needs no eviction, or every instance it would
// evict is swappable and the model is first in line among those waiting for a swap.
// For Tenstorrent models it also waits out chips held by other jobs.
func (g *Gateway) awaitTurn(ctx context.Context, model, engine string) error {
	t, err := g.App.ResolveModel(model)
	if err != nil {
		return err
	}
	if ins, _ := runtime.List(); anyAlive(ins, model) {
		return nil // loading or loaded: ensure waits for it
	}
	engineID, err := g.App.EngineFor(t, engine)
	if err != nil {
		return err
	}
	e, err := g.App.Engine(engineID)
	if err != nil {
		return err
	}
	chips := e.AcceleratorKind() == spec.AccelTenstorrent
	opts := app.LaunchOpts{Engine: engine, Owner: "gateway", Activity: g.activity}
	timeout := time.Duration(g.gw().Swap.WaitTimeout)
	start := time.Now()
	queued := false
	defer func() {
		if queued {
			g.dequeueSwap(model)
		}
	}()
	for {
		victims, err := g.App.WouldEvict(t, opts)
		blockedBy := ""
		switch {
		case err != nil && !(chips && errs.CodeOf(err) == errs.Capacity):
			return err
		case err != nil:
			blockedBy = err.Error() // chips held by another job: wait for them
		case len(victims) == 0:
			return nil
		default:
			if !queued {
				g.enqueueSwap(model)
				queued = true
			}
			ready := g.headOfSwapQueue(model)
			for _, v := range victims {
				ready = ready && g.swappable(v)
			}
			if ready {
				g.Log.Printf("swap: %s takes the devices of %s after waiting %s", model, strings.Join(victims, ", "),
					time.Since(start).Round(time.Second))
				return nil
			}
			blockedBy = "waiting to swap out " + strings.Join(victims, ", ")
		}
		if timeout > 0 && time.Since(start) > timeout {
			return errs.New(errs.Timeout, "%s: not swapped in within %s (%s)", model, timeout, blockedBy)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// loadEstimate is how many seconds model last took to load (for Retry-After); 0 when unknown.
func (g *Gateway) loadEstimate(model string) int {
	db, err := g.App.DB()
	if err != nil {
		return 0
	}
	d, err := db.LastLoad(model)
	if err != nil || d <= 0 {
		return 0
	}
	return int(d.Seconds()) + 1
}

func anyAlive(ins []*runtime.Instance, model string) bool {
	for _, in := range ins {
		if in.Name == model && in.Alive() {
			return true
		}
	}
	return false
}

// The swap queue orders models waiting for devices: first come, first served.
func (g *Gateway) enqueueSwap(model string) {
	g.activeMu.Lock()
	g.swapQueue = append(g.swapQueue, model)
	g.activeMu.Unlock()
}

func (g *Gateway) dequeueSwap(model string) {
	g.activeMu.Lock()
	for i, m := range g.swapQueue {
		if m == model {
			g.swapQueue = append(g.swapQueue[:i], g.swapQueue[i+1:]...)
			break
		}
	}
	g.activeMu.Unlock()
}

func (g *Gateway) headOfSwapQueue(model string) bool {
	g.activeMu.Lock()
	defer g.activeMu.Unlock()
	return len(g.swapQueue) > 0 && g.swapQueue[0] == model
}

func (g *Gateway) modelLock(model string) *sync.Mutex {
	g.locksMu.Lock()
	defer g.locksMu.Unlock()
	m := g.startLocks[model]
	if m == nil {
		m = &sync.Mutex{}
		g.startLocks[model] = m
	}
	return m
}

// ensure returns a ready instance of model, launching it with placement and
// eviction. Starts of one model are serialized; different models start in parallel.
func (g *Gateway) ensure(ctx context.Context, model, engine, api string, timeout time.Duration) (*runtime.Instance, error) {
	mu := g.modelLock(model)
	mu.Lock()
	defer mu.Unlock()
	if api != "" {
		if ins, err := g.ready(ctx, model, api); len(ins) > 0 || err != nil {
			if err != nil {
				return nil, err
			}
			return ins[0], nil
		}
		// Refuse before loading an engine that cannot answer this API.
		t, err := g.App.ResolveModel(model)
		if err != nil {
			return nil, err
		}
		id, err := g.App.EngineFor(t, engine)
		if err != nil {
			return nil, err
		}
		if e, err := g.App.Engine(id); err == nil && !e.HasAPI(api) {
			return nil, errs.New(errs.Unsupported, "%s would run on %s, which does not implement %s", model, id, spec.APIs[api])
		}
	}
	start := time.Now()
	res, err := g.App.Ensure(ctx, model, app.LaunchOpts{Engine: engine, Owner: "gateway", Evict: g.evict(model), Activity: g.activity}, timeout)
	if err != nil {
		return nil, err
	}
	in := res.Instance
	if res.Started {
		g.Log.Printf("%s ready at %s in %s (%s)", in.ID, in.BaseURL, time.Since(in.StartedAt).Round(time.Second), placementNote(in))
	}
	if res.Loaded {
		g.Log.Printf("%s loaded into %s in %s", model, in.ID, time.Since(start).Round(10*time.Millisecond))
	}
	g.setStatus(in.ID, runtime.StatusReady)
	g.touch(in.ID)
	if api != "" && !in.HasAPI(api) {
		return nil, errs.New(errs.Unsupported, "%s is served by %s, which does not implement %s", model, in.Engine, spec.APIs[api])
	}
	return in, nil
}

func placementNote(in *runtime.Instance) string {
	if len(in.Devices) > 0 {
		return "Tenstorrent chip(s) " + tenstorrent.Join(in.Devices)
	}
	if len(in.GPUs) == 0 {
		return "no placement"
	}
	gs := make([]string, len(in.GPUs))
	for i, g := range in.GPUs {
		gs[i] = fmt.Sprint(g)
	}
	return fmt.Sprintf("GPU %s, %d MiB planned per GPU", strings.Join(gs, ","), in.PlannedMiB)
}

// ready lists ready instances for model and api, rotated round-robin; an error
// means instances exist but none serves api.
func (g *Gateway) ready(ctx context.Context, model, api string) ([]*runtime.Instance, error) {
	ins, _ := runtime.List()
	var ok, wrongAPI []*runtime.Instance
	for _, in := range ins {
		if model != "" && !in.Hosting && in.Name != model {
			continue
		}
		if g.instanceStatus(ctx, in) != runtime.StatusReady {
			continue
		}
		if model != "" && in.Hosting && !contains(g.hosted(ctx, in), model) {
			continue
		}
		if !in.HasAPI(api) {
			wrongAPI = append(wrongAPI, in)
			continue
		}
		ok = append(ok, in)
	}
	if model == "" {
		if len(ok) == 1 {
			return ok, nil
		}
		return nil, nil
	}
	if len(ok) == 0 {
		if len(wrongAPI) > 0 {
			return nil, errs.New(errs.Unsupported, "%s is served by %s, which does not implement %s", model, wrongAPI[0].Engine, spec.APIs[api])
		}
		return nil, nil
	}
	g.activeMu.Lock()
	k := g.rr[model] % len(ok)
	g.rr[model]++
	g.activeMu.Unlock()
	return append(ok[k:], ok[:k]...), nil
}

// hosted lists the models multi-model host in holds now (nil when it cannot say).
// The host may unload models on its own (to fit another), so it is asked every time.
func (g *Gateway) hosted(ctx context.Context, in *runtime.Instance) []string {
	e, err := g.App.Engine(in.Engine)
	if err != nil || e.Host == nil {
		return nil
	}
	names, err := runtime.HostedModels(ctx, in, e.Host)
	if err != nil {
		g.Log.Printf("%s: listing hosted models: %v", in.ID, err)
	}
	return names
}

// evict returns the evictor placement calls to make room for model.
func (g *Gateway) evict(model string) app.Evictor {
	return func(ids []string) error {
		for _, id := range ids {
			in, err := runtime.Get(id)
			if err != nil {
				continue // already gone
			}
			g.waitIdle(id, 2*time.Minute)
			g.Log.Printf("evicting %s to make room for %s", id, model)
			g.metrics.evicted(in.Name, model)
			if err := in.StopFor("evicted for " + model); err != nil {
				return err
			}
			g.setStatus(id, runtime.StatusExited)
		}
		return nil
	}
}

// activity reports an instance's idleness and last use for placement's LRU order.
func (g *Gateway) activity(id string) (bool, time.Time) {
	g.activeMu.Lock()
	defer g.activeMu.Unlock()
	last := g.lastUsed[id]
	if last.IsZero() {
		if in, err := runtime.Get(id); err == nil {
			last = in.StartedAt
			if in.ReadyAt != nil {
				last = *in.ReadyAt
			}
		}
	}
	return g.active[id] == 0, last
}

func (g *Gateway) touch(id string) {
	g.activeMu.Lock()
	g.lastUsed[id] = time.Now()
	g.activeMu.Unlock()
}

func (g *Gateway) acquire(id string) {
	g.activeMu.Lock()
	g.active[id]++
	g.lastUsed[id] = time.Now()
	g.activeMu.Unlock()
}

func (g *Gateway) release(id string) {
	g.activeMu.Lock()
	g.active[id]--
	g.lastUsed[id] = time.Now()
	if g.active[id] == 0 {
		delete(g.active, id)
		g.idle.Broadcast()
	}
	g.activeMu.Unlock()
}

func (g *Gateway) waitIdle(id string, max time.Duration) {
	timer := time.AfterFunc(max, func() {
		g.activeMu.Lock()
		g.idle.Broadcast()
		g.activeMu.Unlock()
	})
	defer timer.Stop()
	deadline := time.Now().Add(max)
	g.activeMu.Lock()
	for g.active[id] > 0 && time.Now().Before(deadline) {
		g.idle.Wait()
	}
	g.activeMu.Unlock()
}

// ---- idle unload ----

func (g *Gateway) idleTTL(model string) time.Duration {
	if m, ok := g.App.RegisteredModel(model); ok && m.IdleUnload > 0 {
		return time.Duration(m.IdleUnload)
	}
	return time.Duration(g.gw().IdleUnload)
}

// idleUnload stops evictable instances nobody used for their idle TTL.
func (g *Gateway) idleUnload() {
	ins, _ := runtime.List()
	for _, in := range ins {
		if !contains(g.gw().EvictOwners, in.Owner) || in.ReadyAt == nil || !in.Alive() {
			continue
		}
		ttl := g.idleTTL(in.Name)
		if ttl <= 0 {
			continue
		}
		idle, last := g.activity(in.ID)
		if !idle || time.Since(last) < ttl {
			continue
		}
		g.Log.Printf("%s idle for %s: unloading", in.ID, time.Since(last).Round(time.Second))
		if err := in.StopFor(fmt.Sprintf("idle for %s", time.Since(last).Round(time.Second))); err != nil {
			g.Log.Printf("idle unload %s: %v", in.ID, err)
		}
		g.setStatus(in.ID, runtime.StatusExited)
	}
}

// ---- crashes, health, restarts ----

// restartBudget allows a few automatic restarts per model in a window.
type restartBudget struct {
	mu   sync.Mutex
	hist map[string][]time.Time
}

const (
	restartLimit  = 3
	restartWindow = 10 * time.Minute
)

func (b *restartBudget) take(model string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hist == nil {
		b.hist = map[string][]time.Time{}
	}
	var keep []time.Time
	for _, t := range b.hist[model] {
		if time.Since(t) < restartWindow {
			keep = append(keep, t)
		}
	}
	if len(keep) >= restartLimit {
		b.hist[model] = keep
		return false
	}
	b.hist[model] = append(keep, time.Now())
	return true
}

// restart relaunches a registered model the gateway was serving, within budget.
func (g *Gateway) restart(in *runtime.Instance, why string) {
	if !g.gw().RestartCrashed || in.Owner != "gateway" {
		return
	}
	if _, ok := g.App.RegisteredModel(in.Name); !ok {
		return
	}
	if !g.restarts.take(in.Name) {
		g.Log.Printf("%s: %s; not restarting (%d restarts in %s)", in.ID, why, restartLimit, restartWindow)
		runtime.RecordEvent("restart_gave_up", in, why)
		return
	}
	runtime.RecordEvent("restart", in, why)
	g.metrics.restarted(in)
	go func() {
		g.Log.Printf("%s: %s; restarting", in.ID, why)
		if _, err := g.ensure(context.Background(), in.Name, in.Engine, "", 0); err != nil {
			g.Log.Printf("restart %s: %v", in.ID, err)
		}
	}()
}

// monitor records engine exits nobody asked for (crashes, OOM kills) and restarts gateway models.
func (g *Gateway) monitor() {
	ins, _ := runtime.List()
	for _, in := range ins {
		if in.ExitNoted || in.Alive() {
			continue
		}
		in.NoteExit()
		g.setStatus(in.ID, runtime.StatusExited)
		g.Log.Printf("%s (%s) exited unexpectedly; log: %s", in.ID, in.Engine, in.Log)
		if in.ReadyAt != nil { // a crash after serving, not a failed start (those already errored)
			g.restart(in, "crashed")
		}
	}
}

// unhealthyAfter is how many consecutive failed probes mark a live instance unhealthy.
const unhealthyAfter = 3

// healthCheck probes every ready instance; a hung engine stops receiving traffic and is restarted.
func (g *Gateway) healthCheck() {
	ins, _ := runtime.List()
	for _, in := range ins {
		if in.ReadyAt == nil || !in.Alive() {
			continue
		}
		ok := in.Healthy(context.Background())
		g.statusMu.Lock()
		s := g.status[in.ID]
		if s == nil {
			s = &instState{}
			g.status[in.ID] = s
		}
		wasUnhealthy := s.status == statusUnhealthy
		if ok {
			s.fails, s.status, s.at = 0, runtime.StatusReady, time.Now()
		} else {
			s.fails++
			if s.fails >= unhealthyAfter {
				s.status, s.at = statusUnhealthy, time.Now()
			}
		}
		becameUnhealthy := !ok && !wasUnhealthy && s.status == statusUnhealthy
		g.statusMu.Unlock()
		switch {
		case ok && wasUnhealthy:
			g.Log.Printf("%s healthy again", in.ID)
			runtime.RecordEvent("healthy", in, "")
		case becameUnhealthy:
			g.Log.Printf("%s failed %d health checks: no longer routed", in.ID, unhealthyAfter)
			runtime.RecordEvent("unhealthy", in, fmt.Sprintf("%d consecutive failed health checks", unhealthyAfter))
			if g.gw().RestartCrashed && in.Owner == "gateway" {
				g.waitIdle(in.ID, 30*time.Second)
				if err := in.StopFor("unhealthy"); err != nil {
					g.Log.Printf("stopping %s: %v", in.ID, err)
					continue
				}
				g.restart(in, "unhealthy")
			}
		}
	}
}

// ---- concurrency limits ----

// errQueueFull is answered with 429 (the client should back off and retry).
var errQueueFull = errors.New("queue full")

type limiter struct {
	slots   chan struct{}
	waiting int
}

type limiters struct {
	mu      sync.Mutex
	byModel map[string]*limiter
}

func (g *Gateway) maxConcurrency(model string) int {
	if m, ok := g.App.RegisteredModel(model); ok && m.MaxConcurrency > 0 {
		return m.MaxConcurrency
	}
	return g.gw().MaxConcurrency
}

// acquire takes one of max slots for model, queueing up to maxQueue waiters for at most timeout.
func (l *limiters) acquire(ctx context.Context, model string, max, maxQueue int, timeout time.Duration) (func(), error) {
	if max <= 0 {
		return func() {}, nil
	}
	l.mu.Lock()
	lim := l.byModel[model]
	if lim == nil || cap(lim.slots) != max {
		lim = &limiter{slots: make(chan struct{}, max)}
		l.byModel[model] = lim
	}
	release := func() { <-lim.slots }
	select {
	case lim.slots <- struct{}{}:
		l.mu.Unlock()
		return release, nil
	default:
	}
	if lim.waiting >= maxQueue {
		l.mu.Unlock()
		return nil, errs.Wrap(errs.Capacity, fmt.Errorf("%s: %d requests in flight and %d queued: %w", model, max, lim.waiting, errQueueFull))
	}
	lim.waiting++
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		lim.waiting--
		l.mu.Unlock()
	}()
	var expire <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		expire = t.C
	}
	select {
	case lim.slots <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-expire:
		return nil, errs.New(errs.Timeout, "%s: no free slot within %s (queue_timeout)", model, timeout)
	}
}

// queued reports waiting requests per model (metrics).
func (l *limiters) queued() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int{}
	for m, lim := range l.byModel {
		out[m] = lim.waiting
	}
	return out
}

// ---- sampling ----

// sample records GPU readings and each live instance's GPU memory and RSS.
func (g *Gateway) sample() {
	now := time.Now().UnixMilli()
	gs, err := gpu.List()
	if err != nil {
		g.Log.Printf("sampler: %v", err)
	}
	usage, _ := gpu.ProcessUsage(gs)
	var gss []store.GPUSample
	for _, x := range gs {
		gss = append(gss, store.GPUSample{TS: now, GPU: x.Index, Name: x.Name, MemUsedMiB: x.MemUsedMiB,
			MemTotalMiB: x.MemTotalMiB, UtilPct: x.UtilPct, PowerW: x.PowerW})
	}
	var iss []store.InstanceSample
	ins, _ := runtime.List()
	for _, in := range ins {
		if !in.Alive() {
			continue
		}
		pids := runtime.SessionPIDs(in.PID)
		s := store.InstanceSample{TS: now, Instance: in.ID, Engine: in.Engine, Model: in.Name,
			RSSMiB: runtime.RSSMiB(pids), Processes: int64(len(pids))}
		for _, m := range usage.Of(pids) {
			s.GPUMemMiB += m
		}
		iss = append(iss, s)
	}
	g.metrics.setSamples(gs, iss)
	if len(gss) == 0 && len(iss) == 0 {
		return
	}
	db, err := g.App.DB()
	if err == nil {
		err = db.InsertSamples(gss, iss)
	}
	if err != nil {
		g.Log.Printf("sampler: %v", err)
	}
}

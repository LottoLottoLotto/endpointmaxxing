// Package app wires manifests, config, the model registry, routing, the
// runtime and the usage database together; the CLI, the gateway and the MCP
// server all drive it.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/lock"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/model"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/paths"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/route"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/runtime"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/spec"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/tenstorrent"
)

// App is one loaded view of the user's emx setup.
type App struct {
	Engines *spec.Registry

	// cfg is replaced whole by Refresh (hot reload); readers keep the snapshot they loaded.
	cfg atomic.Pointer[config.Config]
	// overlay adjusts every loaded config (command-line overrides of `emx serve`).
	overlay func(*config.Config)

	mu       sync.Mutex
	models   *config.Models
	resolved map[string]*runtime.Engine
	// overrides and prefer mirror config.yaml; Refresh replaces them while the gateway runs.
	overrides map[string]config.EngineOverride
	prefer    map[string][]string
	db        *store.DB
	dbErr     error
	// Warn reports non-fatal problems (unwritable usage db, ...).
	Warn func(format string, a ...any)
}

// Config is the current config.yaml. Treat it as read-only: a reload replaces it, never edits it.
func (a *App) Config() *config.Config { return a.cfg.Load() }

// SetOverlay applies fn to the current config and every config Refresh loads later.
func (a *App) SetOverlay(fn func(*config.Config)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.overlay = fn
	c := *a.cfg.Load()
	fn(&c)
	a.cfg.Store(&c)
}

// Open loads config, manifests and the model registry, and (when logging is
// enabled) routes lifecycle events into the usage database.
func Open() (*App, error) {
	// Tighten directories an older version created world-readable (keys, prompts, request records, engine
	// logs). Best effort: doctor reports what could not be fixed.
	_ = paths.Private(paths.Config())
	_ = runtime.PrivateDirs()
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	reg, err := spec.Load(config.EnginesDir())
	if err != nil {
		return nil, err
	}
	models, err := config.LoadModels()
	if err != nil {
		return nil, err
	}
	a := &App{Engines: reg, models: models, resolved: map[string]*runtime.Engine{},
		overrides: cfg.Engines, prefer: cfg.Prefer,
		Warn: func(string, ...any) {}}
	a.cfg.Store(cfg)
	if cfg.Logging.Enabled {
		runtime.Recorder = func(e store.Event) {
			db, err := a.DB()
			if err == nil {
				err = db.InsertEvent(&e)
			}
			if err != nil {
				a.Warn("recording %s event: %v", e.Kind, err)
			}
		}
	} else {
		runtime.Recorder = nil
	}
	runtime.TT = tenstorrent.Config{Lock: cfg.Tenstorrent.Lock, Locks: cfg.Tenstorrent.Locks, TTSmi: cfg.Tenstorrent.TTSmi}
	return a, nil
}

// DB opens the usage database on first use.
func (a *App) DB() (*store.DB, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.db == nil && a.dbErr == nil {
		a.db, a.dbErr = store.Open()
	}
	return a.db, a.dbErr
}

// Close releases the usage database.
func (a *App) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.db != nil {
		a.db.Close()
		a.db = nil
	}
}

// Engine returns the resolved engine id (binary lookup is cached).
func (a *App) Engine(id string) (*runtime.Engine, error) {
	a.mu.Lock()
	if e, ok := a.resolved[id]; ok {
		a.mu.Unlock()
		return e, nil
	}
	a.mu.Unlock()
	m, ok := a.Engines.Get(id)
	if !ok {
		return nil, errs.New(errs.NotFound, "unknown engine %q (known: %s)", id, strings.Join(a.Engines.IDs(), ", "))
	}
	a.mu.Lock()
	ov := a.overrides[id]
	a.mu.Unlock()
	e := runtime.Resolve(m, ov) // may probe for seconds: outside the lock
	a.mu.Lock()
	a.resolved[id] = e
	a.mu.Unlock()
	return e, nil
}

// Reresolve drops engine id's cached binary lookup and resolves it again.
func (a *App) Reresolve(id string) (*runtime.Engine, error) {
	a.mu.Lock()
	delete(a.resolved, id)
	a.mu.Unlock()
	return a.Engine(id)
}

// Installed reports whether engine id's binary resolves.
func (a *App) Installed(id string) bool {
	e, err := a.Engine(id)
	return err == nil && e.Installed()
}

// Refresh re-reads models.yaml and config.yaml, so a long-running gateway sees `model add`, `engine set`
// and config edits without a restart. An invalid file leaves everything as it was. The new config
// replaces Config() only when it changed. Listen settings (host, port, TLS), logging.enabled and the
// tenstorrent section apply at the next start.
func (a *App) Refresh() error {
	m, err := config.LoadModels()
	if err != nil {
		return err
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.models = m
	if !reflect.DeepEqual(c.Engines, a.overrides) {
		a.resolved = map[string]*runtime.Engine{}
	}
	a.overrides, a.prefer = c.Engines, c.Prefer
	if a.overlay != nil {
		a.overlay(c)
	}
	if !reflect.DeepEqual(c, a.cfg.Load()) {
		a.cfg.Store(c)
	}
	return nil
}

// RegisteredModel returns the registry entry called name.
func (a *App) RegisteredModel(name string) (config.Model, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	m, ok := a.models.Get(name)
	if !ok {
		return config.Model{}, false
	}
	return *m, true
}

// RegisteredModels returns a copy of the registry.
func (a *App) RegisteredModels() []config.Model {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]config.Model(nil), a.models.Models...)
}

// Target is a model ready to route and launch.
type Target struct {
	Model config.Model
	Info  *model.Info
	// Registered is false for an ad hoc path or repo id.
	Registered bool
	// host marks the launch of a multi-model host itself (no model).
	host bool
}

// ResolveModel accepts a registered name, a path or a repo id.
func (a *App) ResolveModel(ref string) (*Target, error) {
	if m, ok := a.RegisteredModel(ref); ok {
		info, err := model.Inspect(m.Source)
		if err != nil {
			return nil, errs.Wrap(errs.NotFound, fmt.Errorf("model %s: %w", m.Name, err))
		}
		return &Target{Model: m, Info: info, Registered: true}, nil
	}
	info, err := model.Inspect(ref)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errs.New(errs.NotFound, "%q is not a registered model, a model path or a repo id", ref)
		}
		return nil, errs.Wrap(errs.Usage, fmt.Errorf("%q is not a registered model, and %w", ref, err))
	}
	return &Target{Model: FromInfo(DefaultName(ref), info), Info: info}, nil
}

// FromInfo builds a registry entry from inspection results.
func FromInfo(name string, info *model.Info) config.Model {
	return config.Model{Name: name, Source: info.Source, Format: info.Format, Arch: info.Arch, Quant: info.Quant}
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._:-]+`)

// DefaultName derives a model name from a path or repo id.
func DefaultName(ref string) string {
	base := filepath.Base(strings.TrimRight(ref, "/"))
	for _, ext := range []string{".gguf", ".json"} {
		base = strings.TrimSuffix(base, ext)
	}
	base = strings.TrimPrefix(base, "strata-")
	base = strings.Trim(unsafeName.ReplaceAllString(base, "-"), "-.")
	if base == "" {
		base = "model"
	}
	if len(base) > 100 {
		base = base[:100]
	}
	return strings.ToLower(base)
}

// Offer is what a registered model that is not running would give a client.
type Offer struct {
	// Engine is the engine that would serve it: its pinned engine, or the best compatible installed one.
	Engine string `json:"engine,omitempty"`
	// Context is its context window in tokens: the registered ctx flag (shared between parallel slots
	// where the engine does that), else the context declared for benchmarks; 0 when neither is set.
	Context int `json:"context,omitempty"`
	// Unavailable says why it cannot be started; empty when it can.
	Unavailable string `json:"unavailable,omitempty"`
	// Parallel is how many requests it is registered to work on at the same time (the `parallel` flag);
	// 0 when the engine is left to choose.
	Parallel int `json:"parallel,omitempty"`
	// Images says whether it takes image input, when its files say (nil: not known until it runs).
	Images *bool `json:"images,omitempty"`
	// APIs are the request formats its engine serves (chat, messages, embeddings, ...).
	APIs []string `json:"apis,omitempty"`
	// Accelerator is "tenstorrent" for an engine that runs on Tenstorrent chips, else "gpu"; Chips is how
	// many chips an instance owns, NeedMiB the GPU memory it takes (declared, or the peak measured over
	// the last 30 days; 0 when neither).
	Accelerator string `json:"accelerator,omitempty"`
	Chips       int    `json:"chips,omitempty"`
	NeedMiB     int64  `json:"need_mib,omitempty"`
}

// Offer describes registered model m as a client choosing a model should see it.
func (a *App) Offer(m config.Model) Offer {
	var o Offer
	id, err := route.Pick(route.Rank(route.Model{Format: m.Format, Quant: m.Quant, Engine: m.Engine},
		a.Engines.All(), a.Installed, a.preferences()))
	switch {
	case model.MissingLocal(m.Source):
		o.Unavailable = "files missing"
	case err != nil:
		o.Unavailable = "no installed engine serves it"
	}
	if err == nil {
		o.Engine = id
		if e, err := a.Engine(id); err == nil {
			o.Context = e.Context.Registered(m.Flags)
			o.APIs = e.Capabilities.APIs
			if e.Slots != nil {
				o.Parallel = e.Slots.Count // an engine with a fixed number of slots
			}
			o.Accelerator = "gpu"
			if e.Accelerator.Kind == spec.AccelTenstorrent {
				o.Accelerator = spec.AccelTenstorrent
				if o.Chips = m.Devices; o.Chips == 0 {
					o.Chips = max(e.Accelerator.Devices, 1)
				}
			} else if o.NeedMiB = m.VRAMMiB; o.NeedMiB == 0 {
				o.NeedMiB = a.peak(m.Name, id)
			}
		}
	}
	if o.Context == 0 && m.Bench != nil {
		o.Context = m.Bench.ContextLength
	}
	if n, err := strconv.Atoi(m.Flags["parallel"]); err == nil && n > 0 {
		o.Parallel = n
	}
	if o.Parallel == 0 {
		if n, known := model.DeclaredParallel(paths.Expand(m.Source)); known {
			o.Parallel = n
		}
	}
	if images, known := model.DeclaresVision(paths.Expand(m.Source)); known {
		o.Images = &images
	}
	return o
}

// Route ranks engines for t.
func (a *App) Route(t *Target) []route.Candidate {
	return route.Rank(route.Model{Format: t.Info.Format, Quant: t.Info.Quant, Engine: t.Model.Engine},
		a.Engines.All(), a.Installed, a.preferences())
}

func (a *App) preferences() map[string][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prefer
}

// LaunchOpts are the caller's choices for one launch.
type LaunchOpts struct {
	Engine  string            // "" routes automatically
	Name    string            // served name; "" uses the model name
	ID      string            // instance id; "" uses <name>.<engine>
	Host    string            //
	Port    int               // 0 picks a free port
	Flags   map[string]string // unified flags by manifest key (override model defaults)
	Args    []string          // extra engine args
	Lenient bool
	Owner   string
	// Force skips the GPU memory check and placement.
	Force bool
	// Evict stops instances placement chose to make room; nil only checks, and a launch
	// that needs evictions fails with code "capacity" naming them.
	Evict Evictor
	// Activity reports whether an instance is idle and when it was last used (gateway); nil treats all as idle.
	Activity func(id string) (idle bool, lastUsed time.Time)
	// dryRun makes placement describe evictions instead of performing them.
	dryRun bool
	// devices are the Tenstorrent chips placement assigned.
	devices []int
}

// Plan resolves the engine and builds the launch plan for t.
func (a *App) Plan(t *Target, o LaunchOpts) (*runtime.Plan, string, error) {
	engineID := o.Engine
	if engineID == "" {
		var err error
		if engineID, err = route.Pick(a.Route(t)); err != nil {
			return nil, "", fmt.Errorf("%s: %w", t.Model.Name, err)
		}
	} else {
		if _, err := a.Engine(engineID); err != nil {
			return nil, "", err
		}
		for _, c := range a.Route(t) {
			if c.Engine == engineID && !c.Compatible && !t.host {
				return nil, "", errs.New(errs.Unsupported, "%s cannot serve %s: %s", engineID, t.Model.Name, strings.Join(c.Reasons, "; "))
			}
		}
	}
	e, err := a.Engine(engineID)
	if err != nil {
		return nil, "", err
	}

	name := o.Name
	if name == "" {
		name = t.Model.Name
	}
	if !config.NameRe.MatchString(name) {
		return nil, "", errs.New(errs.Usage, "name %q must match %s", name, config.NameRe)
	}
	host := o.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := o.Port
	if port == 0 {
		if port, err = runtime.FreePort(host); err != nil {
			return nil, "", err
		}
	}
	flags := map[string]string{}
	for k, v := range t.Model.Flags {
		flags[k] = v
	}
	for k, v := range o.Flags {
		flags[k] = v
	}
	args := append(append([]string{}, t.Model.Args[engineID]...), o.Args...)

	id := o.ID
	if id == "" {
		id = runtime.InstanceID(name, engineID)
	}
	if !config.NameRe.MatchString(id) {
		return nil, "", errs.New(errs.Usage, "instance id %q must match %s", id, config.NameRe)
	}
	plan, err := e.Build(runtime.Request{
		ID: id, Name: name, Host: host, Port: port, Flags: flags, Args: args, Lenient: o.Lenient,
		PreArgs: t.Model.PreArgs[engineID], Vars: t.Model.Vars, Devices: o.devices,
		Model: runtime.ModelRef{Source: t.Info.Source, Path: t.Info.Path, Ref: t.Info.Ref, Format: t.Info.Format},
	})
	if err != nil {
		return nil, "", err
	}
	return plan, id, nil
}

// Launch places (unless forced), plans and starts t. Placement and start happen
// under a machine-wide lock, so concurrent launches see each other's reservations.
// With an automatic port, a port taken between choosing and binding is retried.
func (a *App) Launch(t *Target, o LaunchOpts) (*runtime.Instance, *runtime.Plan, error) {
	if o.Owner == "" {
		o.Owner = "cli"
	}
	if e := a.HostEngine(t, o.Engine); e != nil {
		return nil, nil, errs.New(errs.Usage, "%s runs inside %s's multi-model host, which loads it on demand: use ensure (or the gateway)", t.Model.Name, e.ID)
	}
	var (
		in   *runtime.Instance
		plan *runtime.Plan
	)
	err := lock.With(placementLock(), func() error {
		pl, err := a.place(t, &o)
		if err != nil {
			return err
		}
		for attempt := 0; ; attempt++ {
			var id string
			plan, id, err = a.Plan(t, o)
			if err != nil {
				return err
			}
			pl.apply(plan)
			in, err = runtime.Start(plan, id, o.Owner)
			if err != nil && o.Port == 0 && errors.Is(err, runtime.ErrPortInUse) && attempt < 3 {
				continue
			}
			return err
		}
	})
	if err != nil {
		return nil, nil, err
	}
	return in, plan, nil
}

// EnsureResult says what Ensure did.
type EnsureResult struct {
	Instance *runtime.Instance
	// Model is the served model name (a multi-model host serves several).
	Model string
	// Started is true when this call launched the instance.
	Started bool
	// Loaded is true when this call loaded the model into a multi-model host.
	Loaded bool
}

// Ensure returns a ready instance serving ref, launching one if none exists.
// An existing instance matches by served name (and engine when o.Engine is set);
// one still loading is waited for. It is idempotent: agents call it before every use.
func (a *App) Ensure(ctx context.Context, ref string, o LaunchOpts, timeout time.Duration) (*EnsureResult, error) {
	t, err := a.ResolveModel(ref)
	if err != nil {
		return nil, err
	}
	name := o.Name
	if name == "" {
		name = t.Model.Name
	}
	if e := a.HostEngine(t, o.Engine); e != nil {
		return a.ensureHosted(ctx, t, e, name, o, timeout)
	}
	ins, err := runtime.List()
	if err != nil {
		return nil, err
	}
	for _, in := range ins {
		if in.Name != name || (o.Engine != "" && in.Engine != o.Engine) || !in.Alive() {
			continue
		}
		if err := in.WaitReady(ctx, timeout); err != nil {
			return nil, err
		}
		return &EnsureResult{Instance: in, Model: name}, nil
	}
	in, _, err := a.Launch(t, o)
	if err != nil {
		return nil, err
	}
	if err := a.AwaitReady(ctx, in, timeout); err != nil {
		return nil, err
	}
	return &EnsureResult{Instance: in, Model: name, Started: true}, nil
}

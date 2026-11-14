package app

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/model"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/runtime"
)

// A multi-model host (manifest `host:`) is one instance per engine, with the
// engine id as its instance id and name. Models routed to the engine are
// loaded into it on first use instead of each getting an instance.

// hostTarget is the launch target of engine engineID's host: no model, the engine's chips.
func hostTarget(engineID string) *Target {
	return &Target{Model: config.Model{Name: engineID, Engine: engineID}, Info: &model.Info{}, host: true}
}

// HostInstance returns engine engineID's running host, or nil.
func HostInstance(engineID string) *runtime.Instance {
	ins, _ := runtime.List()
	for _, in := range ins {
		if in.Hosting && in.Engine == engineID && in.Alive() {
			return in
		}
	}
	return nil
}

// HostEngine returns the engine t routes to (engine pins it) when that engine is a multi-model host.
func (a *App) HostEngine(t *Target, engine string) *runtime.Engine {
	if t.host {
		return nil
	}
	id, err := a.EngineFor(t, engine)
	if err != nil {
		return nil
	}
	e, err := a.Engine(id)
	if err != nil || e.Host == nil {
		return nil
	}
	return e
}

// asHost turns a launch of a hosted model into the launch of its engine's host
// (what placement and dry runs consider); other targets are returned unchanged.
func (a *App) asHost(t *Target, o *LaunchOpts) *Target {
	e := a.HostEngine(t, o.Engine)
	if e == nil {
		return t
	}
	o.Engine, o.Name, o.ID, o.Flags, o.Args = e.ID, e.ID, e.ID, map[string]string{}, nil
	return hostTarget(e.ID)
}

// ensureHosted makes engine e's host serve t as name: it launches the host when none
// runs (placing it like any launch), then loads the model unless the host holds it.
func (a *App) ensureHosted(ctx context.Context, t *Target, e *runtime.Engine, name string, o LaunchOpts, timeout time.Duration) (*EnsureResult, error) {
	if t.Info.Path == "" {
		return nil, errs.New(errs.Unsupported, "%s loads models from local directories; %s is not downloaded", e.ID, t.Info.Source)
	}
	ctxLen := 0
	if v := o.Flags["ctx"]; v != "" {
		ctxLen, _ = strconv.Atoi(v)
	} else if v := t.Model.Flags["ctx"]; v != "" {
		ctxLen, _ = strconv.Atoi(v)
	}
	res := &EnsureResult{Model: name}
	host := HostInstance(e.ID)
	if host == nil {
		ho := LaunchOpts{Engine: e.ID, Name: e.ID, ID: e.ID, Host: o.Host, Owner: o.Owner, Force: o.Force,
			Evict: o.Evict, Activity: o.Activity}
		in, _, err := a.Launch(hostTarget(e.ID), ho)
		if errs.CodeOf(err) == errs.Conflict && HostInstance(e.ID) != nil {
			in, err = HostInstance(e.ID), nil // another caller started it first
		}
		if err != nil {
			return nil, err
		}
		if err := a.AwaitReady(ctx, in, timeout); err != nil {
			return nil, err
		}
		host, res.Started = in, true
	} else if err := host.WaitReady(ctx, timeout); err != nil {
		return nil, err
	}
	loaded, err := runtime.HostedModels(ctx, host, e.Host)
	if err != nil {
		return nil, err
	}
	if !contains(loaded, name) {
		start := time.Now()
		if err := runtime.LoadHosted(ctx, host, e.Host, name, t.Info.Path, ctxLen); err != nil {
			runtime.RecordEvent("load_failed", host, fmt.Sprintf("%s: %v", name, err))
			return nil, fmt.Errorf("loading %s into %s: %w", name, host.ID, err)
		}
		d := time.Since(start)
		runtime.RecordEvent("load", host, fmt.Sprintf("%s in %s", name, d.Round(10*time.Millisecond)))
		res.Loaded = true
	}
	res.Instance = host
	return res, nil
}

// UnloadHosted drops model name from engine engineID's host; errs.NotFound when no host runs.
func (a *App) UnloadHosted(ctx context.Context, engineID, name string) error {
	e, err := a.Engine(engineID)
	if err != nil {
		return err
	}
	if e.Host == nil {
		return errs.New(errs.Usage, "engine %s is not a multi-model host", engineID)
	}
	host := HostInstance(engineID)
	if host == nil {
		return errs.New(errs.NotFound, "engine %s has no running host", engineID)
	}
	err = runtime.UnloadHosted(ctx, host, e.Host, name)
	if err == nil {
		runtime.RecordEvent("unload", host, name)
	}
	return err
}

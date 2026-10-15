// Package runtime turns a manifest, a model and unified flags into a launch
// plan, and starts, tracks, probes and stops engine processes.
package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/paths"
	"github.com/lotto/substrate/internal/spec"
)

// Engine is a manifest with the user's overrides applied and its binary resolved.
type Engine struct {
	*spec.Manifest
	Override config.EngineOverride
	// Bin is the resolved entry point; "" when not installed.
	Bin string
	// Tried lists the candidates checked, for diagnostics.
	Tried []string
}

// baseVars are placeholders available before a binary is known.
func baseVars() map[string]string {
	return map[string]string{"home": paths.Home(), "data": paths.Data(), "state": paths.State()}
}

// Resolve finds m's binary: config override, then $binary.env, then candidates.
func Resolve(m *spec.Manifest, ov config.EngineOverride) *Engine {
	if ov.Memory != nil || ov.Accelerator != nil {
		mm := *m // the override is local to this install: never mutate the shared manifest
		if ov.Memory != nil {
			mm.Memory = *ov.Memory
		}
		if ov.Accelerator != nil {
			mm.Accelerator = *ov.Accelerator
		}
		m = &mm
	}
	e := &Engine{Manifest: m, Override: ov}
	try := func(c string) bool {
		e.Tried = append(e.Tried, paths.Expand(c))
		p, ok := lookup(c)
		if ok && probe(p, m.Binary.Probe) {
			e.Bin = p
		}
		return e.Bin != ""
	}
	if ov.Bin != "" {
		try(ov.Bin)
		return e // an explicit override never falls back
	}
	if m.Binary.Env != "" {
		if v := os.Getenv(m.Binary.Env); v != "" {
			try(v)
			return e
		}
	}
	for _, c := range m.Binary.Candidates {
		c, err := spec.Expand(c, baseVars())
		if err != nil {
			continue
		}
		if try(c) {
			break
		}
	}
	return e
}

func lookup(c string) (string, bool) {
	c = paths.Expand(c)
	if !strings.Contains(c, "/") {
		p, err := exec.LookPath(c)
		return p, err == nil
	}
	matches, _ := filepath.Glob(c)
	sort.Strings(matches)
	for _, p := range matches {
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			abs, _ := filepath.Abs(p)
			return abs, true
		}
	}
	return "", false
}

func probe(bin string, args []string) bool {
	if len(args) == 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, bin, args...).Run() == nil
}

// Installed reports whether the binary resolved.
func (e *Engine) Installed() bool { return e.Bin != "" }

// Version runs the manifest's version command.
func (e *Engine) Version() string {
	if e.Bin == "" || len(e.Binary.Version) == 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, e.Bin, e.Binary.Version...).CombinedOutput()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "built with") {
			return l
		}
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Request is everything needed to build a launch plan.
type Request struct {
	ID    string // instance id ({instance})
	Name  string // served model name (API model id)
	Model ModelRef
	Host  string
	Port  int
	// Flags are unified flag values by manifest key, already type-checked.
	Flags map[string]string
	// Args are extra engine arguments appended verbatim.
	Args []string
	// PreArgs replace the manifest's {pre_args} element.
	PreArgs []string
	// Vars are the model's placeholder values; they win over manifest and install vars.
	Vars map[string]string
	// Devices are the accelerator chips placement assigned (Tenstorrent).
	Devices []int
	// Lenient drops unified flags the engine cannot express instead of failing.
	Lenient bool
}

// ModelRef locates the model for templates.
type ModelRef struct {
	Source string
	Path   string // {model_path}: local file/dir
	Ref    string // {model}: local path or repo id
	Format string
}

// Plan is a fully resolved launch.
type Plan struct {
	Engine    string            `json:"engine"`
	Name      string            `json:"name"`
	Model     string            `json:"model"`
	Format    string            `json:"format"`
	Host      string            `json:"host"`
	Port      int               `json:"port"`
	Argv      []string          `json:"argv"`
	Env       map[string]string `json:"env,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	HealthURL string            `json:"health_url"`
	BaseURL   string            `json:"base_url"`
	APIs      []string          `json:"apis"`
	Warnings  []string          `json:"warnings,omitempty"`
	// GPUs and PlannedMiB are filled by placement before Start.
	GPUs       []int `json:"gpus,omitempty"`
	PlannedMiB int64 `json:"planned_mib,omitempty"`
	// Placement explains the placement decision (need, source, evictions).
	Placement string `json:"placement,omitempty"`
	// Accelerator is the engine's device kind; Devices the chips it owns (Tenstorrent).
	Accelerator string `json:"accelerator"`
	Devices     []int  `json:"devices,omitempty"`
	// Cleanup runs after the instance stops or dies.
	Cleanup [][]string `json:"cleanup,omitempty"`
	// Prepare runs before the launch.
	Prepare [][]string `json:"prepare,omitempty"`
	// Assets is where the engine's builtin files are written before the launch ("" when it ships none).
	Assets string `json:"assets,omitempty"`
	// Hosting marks a multi-model host launch (no model; models are loaded over HTTP).
	Hosting bool `json:"hosting,omitempty"`

	lockMode    string // substrate | engine (Tenstorrent)
	resetOnKill bool
	stopSignal  string
	stopTimeout time.Duration
	readyIn     time.Duration
	apiKey      string
}

// vars returns the placeholder values for one launch: base, binary and request values,
// plus manifest vars overridden by install vars overridden by model vars. Vars may
// reference each other in any order: a model that sets cache also moves every var
// derived from {cache}.
func (e *Engine) vars(r Request) (map[string]string, error) {
	v := baseVars()
	v["bin"] = e.Bin
	v["bin_dir"] = filepath.Dir(e.Bin)
	v["model"] = r.Model.Ref
	v["model_path"] = r.Model.Path
	v["name"] = r.Name
	v["host"] = r.Host
	v["port"] = fmt.Sprint(r.Port)
	v["instance"] = r.ID
	v["devices"] = joinInts(r.Devices)
	v["device_count"] = fmt.Sprint(len(r.Devices))
	v["assets"] = filepath.Join(paths.Data(), "assets", e.ID)
	raw := map[string]string{}
	for _, layer := range []map[string]string{e.Manifest.Vars, e.Override.Vars, r.Vars} {
		for k, x := range layer {
			raw[k] = x
		}
	}
	for len(raw) > 0 {
		progress := false
		for _, k := range sortedKeys(raw) {
			ready := true
			for _, ref := range spec.Placeholders(raw[k]) {
				if _, pending := raw[ref]; pending && ref != k {
					ready = false
				}
			}
			if !ready {
				continue
			}
			x, err := spec.Expand(raw[k], v)
			if err != nil {
				return nil, fmt.Errorf("vars.%s: %w", k, err)
			}
			if strings.Contains(x, "/") && !strings.Contains(x, ":") {
				x = filepath.Clean(x) // paths only: image refs (name:tag) stay as written
			}
			v[k] = x
			delete(raw, k)
			progress = true
		}
		if !progress {
			return nil, fmt.Errorf("vars %v reference each other in a cycle", sortedKeys(raw))
		}
	}
	return v, nil
}

func joinInts(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = fmt.Sprint(x)
	}
	return strings.Join(s, ",")
}

// Build turns a request into a plan without side effects.
func (e *Engine) Build(r Request) (*Plan, error) {
	if !e.Installed() {
		return nil, errs.New(errs.NotFound, "engine %s is not installed (tried: %s)", e.ID, strings.Join(e.Tried, ", "))
	}
	if strings.Contains(strings.Join(e.Launch.Command, " "), "{model_path}") && r.Model.Path == "" {
		return nil, errs.New(errs.Unsupported, "engine %s needs a local model file; %s is not downloaded", e.ID, r.Model.Source)
	}
	v, err := e.vars(r)
	if err != nil {
		return nil, err
	}
	// A manifest var declared empty is a required setting: per model (e.g. a container image),
	// or per engine for a multi-model host, whose launch serves no one model.
	for k, x := range e.Manifest.Vars {
		if x == "" && v[k] == "" {
			if e.Host != nil {
				return nil, errs.New(errs.Usage, "engine %s needs vars.%s (substrate engine set %s --var %s=...)", e.ID, k, e.ID, k)
			}
			return nil, errs.New(errs.Usage, "engine %s needs vars.%s for model %s (substrate model add ... --var %s=...)", e.ID, k, r.Name, k)
		}
	}
	p := &Plan{
		Engine: e.ID, Name: r.Name, Model: r.Model.Source, Format: r.Model.Format,
		Host: r.Host, Port: r.Port, Env: map[string]string{}, APIs: e.Capabilities.APIs,
		Accelerator: e.AcceleratorKind(), Devices: r.Devices, Hosting: e.Host != nil,
		lockMode: e.Accelerator.Lock, resetOnKill: e.Accelerator.ResetOnKill,
		stopSignal: e.Launch.StopSignal, stopTimeout: e.Launch.StopTimeout.Or(30 * time.Second),
		readyIn: e.Health.Timeout.Or(10 * time.Minute), apiKey: r.Flags["api_key"],
	}
	if spec.HasAssets(e.ID) && e.Source == "builtin" {
		p.Assets = v["assets"]
	}
	if p.lockMode == "" {
		p.lockMode = "substrate"
	}
	for _, a := range e.Launch.Command {
		if a == spec.PreArgs {
			p.Argv = append(p.Argv, r.PreArgs...)
			continue
		}
		x, err := spec.Expand(a, v)
		if err != nil {
			return nil, err
		}
		p.Argv = append(p.Argv, x)
	}
	if len(r.PreArgs) > 0 && !contains(e.Launch.Command, spec.PreArgs) {
		return nil, errs.New(errs.Unsupported, "engine %s has no %s slot for pre-args %v", e.ID, spec.PreArgs, r.PreArgs)
	}
	if p.Cwd, err = spec.Expand(e.Launch.Cwd, v); err != nil {
		return nil, err
	}
	for k, x := range e.Launch.Env {
		if p.Env[k], err = spec.Expand(x, v); err != nil {
			return nil, err
		}
	}
	for _, c := range e.Launch.Prepare {
		cmd, err := spec.ExpandAll(c, v)
		if err != nil {
			return nil, err
		}
		p.Prepare = append(p.Prepare, cmd)
	}
	for _, c := range e.Launch.Cleanup {
		cmd, err := spec.ExpandAll(c, v)
		if err != nil {
			return nil, err
		}
		p.Cleanup = append(p.Cleanup, cmd)
	}

	var unsupported []string
	for _, f := range spec.Unified { // fixed order keeps argv stable
		val, set := r.Flags[f.Key]
		if !set {
			continue
		}
		fm, ok := e.Flags[f.Key]
		if !ok {
			unsupported = append(unsupported, "--"+f.CLI)
			continue
		}
		if f.Type == spec.TypeBool && val != "true" {
			continue
		}
		if mapped, ok := fm.Values[val]; ok {
			val = mapped
		}
		v["value"] = val
		args, err := spec.ExpandAll(fm.Args, v)
		if err != nil {
			return nil, err
		}
		p.Argv = append(p.Argv, args...)
		for k, x := range fm.Env {
			if p.Env[k], err = spec.Expand(x, v); err != nil {
				return nil, err
			}
		}
		delete(v, "value")
	}
	if len(unsupported) > 0 {
		msg := fmt.Sprintf("%s has no equivalent for %s", e.ID, strings.Join(unsupported, ", "))
		if !r.Lenient {
			return nil, errs.New(errs.Unsupported, "%s (use --lenient to drop, or pass engine args after --)", msg)
		}
		p.Warnings = append(p.Warnings, msg+": dropped")
	}

	for k, x := range e.Override.Env {
		p.Env[k] = x
	}
	p.Argv = append(p.Argv, e.Override.Args...)
	p.Argv = append(p.Argv, r.Args...)

	connect := r.Host
	if connect == "0.0.0.0" || connect == "::" || connect == "" {
		connect = "127.0.0.1"
	}
	p.BaseURL = fmt.Sprintf("http://%s:%d", connect, r.Port)
	p.HealthURL = p.BaseURL + e.Health.Path
	return p, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

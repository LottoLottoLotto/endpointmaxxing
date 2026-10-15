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

// Vars returns the engine's placeholder values: base, binary, manifest vars, user vars.
func (e *Engine) Vars() (map[string]string, error) {
	v := baseVars()
	v["bin"] = e.Bin
	v["bin_dir"] = filepath.Dir(e.Bin)
	layer := func(src map[string]string) error {
		for _, k := range sortedKeys(src) {
			x, err := spec.Expand(src[k], v)
			if err != nil {
				return fmt.Errorf("vars.%s: %w", k, err)
			}
			if strings.Contains(x, "/") {
				x = filepath.Clean(x)
			}
			v[k] = x
		}
		return nil
	}
	if err := layer(e.Manifest.Vars); err != nil {
		return nil, err
	}
	return v, layer(e.Override.Vars)
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
	Name  string // served model name (API model id)
	Model ModelRef
	Host  string
	Port  int
	// Flags are unified flag values by manifest key, already type-checked.
	Flags map[string]string
	// Args are extra engine arguments appended verbatim.
	Args []string
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

	stopSignal  string
	stopTimeout time.Duration
	readyIn     time.Duration
	apiKey      string
}

// Build turns a request into a plan without side effects.
func (e *Engine) Build(r Request) (*Plan, error) {
	if !e.Installed() {
		return nil, errs.New(errs.NotFound, "engine %s is not installed (tried: %s)", e.ID, strings.Join(e.Tried, ", "))
	}
	v, err := e.Vars()
	if err != nil {
		return nil, err
	}
	if strings.Contains(strings.Join(e.Launch.Command, " "), "{model_path}") && r.Model.Path == "" {
		return nil, errs.New(errs.Unsupported, "engine %s needs a local model file; %s is not downloaded", e.ID, r.Model.Source)
	}
	v["model"] = r.Model.Ref
	v["model_path"] = r.Model.Path
	v["name"] = r.Name
	v["host"] = r.Host
	v["port"] = fmt.Sprint(r.Port)

	p := &Plan{
		Engine: e.ID, Name: r.Name, Model: r.Model.Source, Format: r.Model.Format,
		Host: r.Host, Port: r.Port, Env: map[string]string{}, APIs: e.Capabilities.APIs,
		stopSignal: e.Launch.StopSignal, stopTimeout: e.Launch.StopTimeout.Or(30 * time.Second),
		readyIn: e.Health.Timeout.Or(10 * time.Minute), apiKey: r.Flags["api_key"],
	}
	if p.Argv, err = spec.ExpandAll(e.Launch.Command, v); err != nil {
		return nil, err
	}
	if p.Cwd, err = spec.Expand(e.Launch.Cwd, v); err != nil {
		return nil, err
	}
	for k, x := range e.Launch.Env {
		if p.Env[k], err = spec.Expand(x, v); err != nil {
			return nil, err
		}
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

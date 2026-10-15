// Package spec defines the substrate engine manifest (substrate.engine/v1):
// the contract an inference engine publishes so substrate can find, install,
// launch, health-check and route to it without engine-specific code.
package spec

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Version is the manifest schema identifier every manifest must declare.
const Version = "substrate.engine/v1"

// Known model formats and APIs. Manifests may only use these; a new format or
// API is a spec change, so routing and the gateway stay in agreement.
var (
	Formats = []string{"gguf", "safetensors", "strata-config"}
	APIs    = map[string]string{
		"chat":        "/v1/chat/completions",
		"completions": "/v1/completions",
		"embeddings":  "/v1/embeddings",
		"responses":   "/v1/responses",
		"messages":    "/v1/messages",
		"rerank":      "/v1/rerank",
	}
)

// Manifest describes one inference engine.
type Manifest struct {
	Spec         string             `yaml:"spec" json:"spec"`
	ID           string             `yaml:"id" json:"id"`
	Name         string             `yaml:"name" json:"name"`
	Description  string             `yaml:"description,omitempty" json:"description,omitempty"`
	Homepage     string             `yaml:"homepage,omitempty" json:"homepage,omitempty"`
	Binary       Binary             `yaml:"binary" json:"binary"`
	Vars         map[string]string  `yaml:"vars,omitempty" json:"vars,omitempty"`
	Install      Install            `yaml:"install,omitempty" json:"install,omitempty"`
	Launch       Launch             `yaml:"launch" json:"launch"`
	Health       Health             `yaml:"health" json:"health"`
	Flags        map[string]FlagMap `yaml:"flags,omitempty" json:"flags,omitempty"`
	Capabilities Capabilities       `yaml:"capabilities" json:"capabilities"`
	Routing      Routing            `yaml:"routing,omitempty" json:"routing,omitempty"`
	Memory       Memory             `yaml:"memory,omitempty" json:"memory,omitempty"`

	// Source is "builtin" or the manifest's file path.
	Source string `yaml:"-" json:"source"`
}

// Binary says how to find the engine's entry point.
type Binary struct {
	// Env names an environment variable holding the binary path; it wins over Candidates.
	Env string `yaml:"env,omitempty" json:"env,omitempty"`
	// Candidates are tried in order: bare names via PATH, paths after ~/$VAR/{data} expansion and globbing.
	Candidates []string `yaml:"candidates" json:"candidates"`
	// Probe, when set, runs `<bin> <probe...>`; a candidate counts only if it exits 0.
	Probe []string `yaml:"probe,omitempty" json:"probe,omitempty"`
	// Version runs `<bin> <version...>`; the last non-empty output line is the version.
	Version []string `yaml:"version,omitempty" json:"version,omitempty"`
}

// Install lists shell steps that install the engine (run with sh -c, in order).
type Install struct {
	Notes string   `yaml:"notes,omitempty" json:"notes,omitempty"`
	Steps []string `yaml:"steps,omitempty" json:"steps,omitempty"`
}

// Launch is the server command.
type Launch struct {
	Command     []string          `yaml:"command" json:"command"`
	Cwd         string            `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	Env         map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	StopSignal  string            `yaml:"stop_signal,omitempty" json:"stop_signal,omitempty"`
	StopTimeout Duration          `yaml:"stop_timeout,omitempty" json:"stop_timeout,omitempty"`
}

// Health is the readiness probe: GET Path returns 200 once the model serves.
type Health struct {
	Path    string   `yaml:"path" json:"path"`
	Timeout Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
}

// FlagMap translates one unified flag. Placeholder {value} is the flag value
// after Values translation. YAML accepts a bare list as shorthand for Args.
type FlagMap struct {
	Args   []string          `yaml:"args,omitempty" json:"args,omitempty"`
	Env    map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Values map[string]string `yaml:"values,omitempty" json:"values,omitempty"`
}

func (f *FlagMap) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.SequenceNode {
		return n.Decode(&f.Args)
	}
	type plain FlagMap
	return n.Decode((*plain)(f))
}

// Capabilities is what routing and the gateway match against.
type Capabilities struct {
	Formats []string `yaml:"formats" json:"formats"`
	// Quantizations lists accepted weight quantization methods; "*" accepts any.
	Quantizations []string `yaml:"quantizations" json:"quantizations"`
	APIs          []string `yaml:"apis" json:"apis"`
	// Features are free-form tags (vision, speculative, tensor-parallel, ...).
	Features []string `yaml:"features,omitempty" json:"features,omitempty"`
}

// Memory says how much GPU memory a launch needs, for placement.
type Memory struct {
	// Mode: "weights" (default) needs model file size × factor + overhead, split over the GPUs;
	// "fraction" claims --mem-fraction (or default_fraction) of each GPU it runs on;
	// "exclusive" needs whole GPUs; "none" uses no NVIDIA GPU memory (CPU, or other accelerators).
	Mode            string  `yaml:"mode,omitempty" json:"mode,omitempty"`
	Factor          float64 `yaml:"factor,omitempty" json:"factor,omitempty"`
	OverheadMiB     int64   `yaml:"overhead_mib,omitempty" json:"overhead_mib,omitempty"`
	DefaultFraction float64 `yaml:"default_fraction,omitempty" json:"default_fraction,omitempty"`
}

// MemoryMode returns the memory mode with the default applied.
func (m *Manifest) MemoryMode() string {
	if m.Memory.Mode == "" {
		return "weights"
	}
	return m.Memory.Mode
}

// Routing ranks engines when several can serve a model: higher wins.
type Routing struct {
	Priority map[string]int `yaml:"priority,omitempty" json:"priority,omitempty"`
}

// Duration is a time.Duration written as "30s" in YAML and JSON.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// Or returns d, or def when d is zero.
func (d Duration) Or(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

// HasAPI reports whether the engine serves api.
func (m *Manifest) HasAPI(api string) bool { return contains(m.Capabilities.APIs, api) }

// HasFormat reports whether the engine loads models of format f.
func (m *Manifest) HasFormat(f string) bool { return contains(m.Capabilities.Formats, f) }

// AcceptsQuant reports whether the engine loads weights quantized with q ("" = unquantized/unknown).
func (m *Manifest) AcceptsQuant(q string) bool {
	if q == "" || contains(m.Capabilities.Quantizations, "*") {
		return true
	}
	for _, x := range m.Capabilities.Quantizations {
		if strings.EqualFold(x, q) {
			return true
		}
	}
	return false
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

var (
	idRe          = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	placeholderRe = regexp.MustCompile(`\{([a-zA-Z0-9_.]+)\}`)
)

// BaseVars are the placeholders substrate always defines for templates.
var BaseVars = []string{"bin", "bin_dir", "home", "data", "state", "model", "model_path", "name", "host", "port"}

// Validate checks a manifest against the v1 spec and returns every problem found.
func (m *Manifest) Validate() []string {
	var errs []string
	bad := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if m.Spec != Version {
		bad("spec: must be %q, got %q", Version, m.Spec)
	}
	if !idRe.MatchString(m.ID) {
		bad("id: %q must match %s", m.ID, idRe)
	}
	if m.Name == "" {
		bad("name: required")
	}
	if len(m.Binary.Candidates) == 0 && m.Binary.Env == "" {
		bad("binary: needs candidates or env")
	}
	if len(m.Launch.Command) == 0 {
		bad("launch.command: required")
	}
	if !strings.Contains(strings.Join(m.Launch.Command, " "), "{port}") {
		bad("launch.command: must place {port}: substrate assigns ports")
	}
	if m.Launch.StopSignal != "" {
		if _, ok := Signals[m.Launch.StopSignal]; !ok {
			bad("launch.stop_signal: %q unknown (use SIGTERM, SIGINT, SIGQUIT, SIGHUP)", m.Launch.StopSignal)
		}
	}
	if !strings.HasPrefix(m.Health.Path, "/") {
		bad("health.path: must start with /")
	}

	known := map[string]bool{}
	for _, v := range BaseVars {
		known[v] = true
	}
	for k := range m.Vars {
		if contains(BaseVars, k) {
			bad("vars.%s: shadows a built-in placeholder", k)
		}
		known[k] = true
	}
	check := func(where, s string, extra ...string) {
		for _, mm := range placeholderRe.FindAllStringSubmatch(s, -1) {
			if !known[mm[1]] && !contains(extra, mm[1]) && !strings.HasPrefix(mm[1], "env.") {
				bad("%s: unknown placeholder {%s}", where, mm[1])
			}
		}
	}
	for k, v := range m.Vars {
		check("vars."+k, v)
	}
	for i, a := range m.Launch.Command {
		check(fmt.Sprintf("launch.command[%d]", i), a)
	}
	check("launch.cwd", m.Launch.Cwd)
	for k, v := range m.Launch.Env {
		check("launch.env."+k, v)
	}
	for _, c := range m.Binary.Candidates {
		check("binary.candidates", c)
	}

	for k, fm := range m.Flags {
		f, ok := FlagByKey(k)
		if !ok {
			bad("flags.%s: not a unified flag (see `substrate flags`)", k)
			continue
		}
		if len(fm.Args) == 0 && len(fm.Env) == 0 {
			bad("flags.%s: needs args or env", k)
		}
		uses := strings.Join(fm.Args, " ")
		for ek, ev := range fm.Env {
			uses += " " + ev
			check("flags."+k+".env."+ek, ev, "value")
		}
		for i, a := range fm.Args {
			check(fmt.Sprintf("flags.%s.args[%d]", k, i), a, "value")
		}
		if f.Type != TypeBool && !strings.Contains(uses, "{value}") {
			bad("flags.%s: %s flag must place {value}", k, f.Type)
		}
	}

	if len(m.Capabilities.Formats) == 0 {
		bad("capabilities.formats: required")
	}
	for _, f := range m.Capabilities.Formats {
		if !contains(Formats, f) {
			bad("capabilities.formats: %q unknown (known: %s)", f, strings.Join(Formats, ", "))
		}
	}
	if len(m.Capabilities.Quantizations) == 0 {
		bad("capabilities.quantizations: required (use [\"*\"] for any)")
	}
	if len(m.Capabilities.APIs) == 0 {
		bad("capabilities.apis: required")
	}
	for _, a := range m.Capabilities.APIs {
		if _, ok := APIs[a]; !ok {
			bad("capabilities.apis: %q unknown (known: %s)", a, strings.Join(sortedKeys(APIs), ", "))
		}
	}
	switch m.MemoryMode() {
	case "weights":
		if m.Memory.Factor != 0 && m.Memory.Factor < 1 {
			bad("memory.factor: must be >= 1")
		}
		if m.Memory.OverheadMiB < 0 {
			bad("memory.overhead_mib: must be >= 0")
		}
	case "fraction":
		if m.Memory.DefaultFraction <= 0 || m.Memory.DefaultFraction > 1 {
			bad("memory.default_fraction: fraction mode needs a value in (0, 1]")
		}
	case "exclusive", "none":
	default:
		bad("memory.mode: %q unknown (weights, fraction, exclusive, none)", m.Memory.Mode)
	}
	for f := range m.Routing.Priority {
		if !contains(m.Capabilities.Formats, f) {
			bad("routing.priority.%s: format not in capabilities.formats", f)
		}
	}
	return errs
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Parse decodes and validates one manifest.
func Parse(data []byte, source string) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	m.Source = source
	if errs := m.Validate(); len(errs) > 0 {
		return nil, fmt.Errorf("%s: invalid manifest:\n  %s", source, strings.Join(errs, "\n  "))
	}
	return &m, nil
}

//go:embed engines/*.yaml
var builtin embed.FS

// Builtin returns the manifests compiled into substrate.
func Builtin() ([]*Manifest, error) {
	ents, err := builtin.ReadDir("engines")
	if err != nil {
		return nil, err
	}
	var out []*Manifest
	for _, e := range ents {
		data, err := builtin.ReadFile("engines/" + e.Name())
		if err != nil {
			return nil, err
		}
		m, err := Parse(data, "builtin")
		if err != nil {
			return nil, fmt.Errorf("builtin %s: %w", e.Name(), err)
		}
		out = append(out, m)
	}
	return out, nil
}

// BuiltinYAML returns the raw builtin manifest for id, for `engine show --raw`.
func BuiltinYAML(id string) ([]byte, bool) {
	data, err := builtin.ReadFile("engines/" + id + ".yaml")
	return data, err == nil
}

// Registry is every known manifest by ID.
type Registry struct {
	byID map[string]*Manifest
	// Errors are user manifests that failed to load; they never abort loading.
	Errors []error
}

// Load returns builtin manifests overlaid by user manifests in dir/*.yaml
// (a user manifest with a builtin's id replaces it).
func Load(dir string) (*Registry, error) {
	bs, err := Builtin()
	if err != nil {
		return nil, err
	}
	r := &Registry{byID: map[string]*Manifest{}}
	for _, m := range bs {
		r.byID[m.ID] = m
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		m, err := Parse(data, f)
		if err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		r.byID[m.ID] = m
	}
	return r, nil
}

// Get returns the manifest for id.
func (r *Registry) Get(id string) (*Manifest, bool) {
	m, ok := r.byID[id]
	return m, ok
}

// All returns every manifest sorted by ID.
func (r *Registry) All() []*Manifest {
	out := make([]*Manifest, 0, len(r.byID))
	for _, id := range sortedKeys(r.byID) {
		out = append(out, r.byID[id])
	}
	return out
}

// IDs returns every engine id, sorted.
func (r *Registry) IDs() []string { return sortedKeys(r.byID) }

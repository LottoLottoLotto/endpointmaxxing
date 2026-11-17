// Package config holds the user's config.yaml (gateway, logging, routing
// preferences, per-engine overrides) and models.yaml (the model registry).
// Writers go through Update/UpdateModels, which re-read under a file lock so
// concurrent CLI calls and the gateway never lose each other's changes.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/lock"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/paths"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/policy"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/spec"
	"gopkg.in/yaml.v3"
)

// Config is config.yaml.
type Config struct {
	Gateway Gateway `yaml:"gateway" json:"gateway"`
	Logging Logging `yaml:"logging" json:"logging"`
	// Prefer orders engines per model format, overriding manifest priorities.
	Prefer map[string][]string `yaml:"prefer,omitempty" json:"prefer,omitempty"`
	// Engines overrides per engine id.
	Engines map[string]EngineOverride `yaml:"engines,omitempty" json:"engines,omitempty"`
	// Tenstorrent configures chip locks and recovery.
	Tenstorrent Tenstorrent `yaml:"tenstorrent" json:"tenstorrent"`
	// Policy flags (or blocks) prompts matching rules; matches are recorded per request.
	Policy policy.Config `yaml:"policy" json:"policy"`
	// Classify labels each request's last user turn with a task category.
	Classify policy.ClassifyConfig `yaml:"classify" json:"classify"`
	// Pricing turns tokens into cost per model.
	Pricing Pricing `yaml:"pricing,omitempty" json:"pricing,omitempty"`
	// Teams group API keys under shared budgets.
	Teams []Team `yaml:"teams,omitempty" json:"teams,omitempty"`
	// Alerts sends operational events to webhooks.
	Alerts Alerts `yaml:"alerts,omitempty" json:"alerts,omitempty"`
	// Aliases map stable model names clients use (chat-default) to registered or running models.
	Aliases map[string]string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	// Mesh joins gateways on several machines into one endpoint.
	Mesh Mesh `yaml:"mesh,omitempty" json:"mesh,omitempty"`
}

// Tenstorrent configures how emx shares Tenstorrent chips with other jobs.
type Tenstorrent struct {
	// Lock is the per-chip lock file; {n} is the chip index. Every job on a card must use the same file.
	Lock string `yaml:"lock" json:"lock"`
	// Locks overrides the lock file of single chips (to interoperate with existing scripts).
	Locks map[int]string `yaml:"locks,omitempty" json:"locks,omitempty"`
	// TTSmi is the tt-smi binary used for resets.
	TTSmi string `yaml:"tt_smi" json:"tt_smi"`
}

// Swap governs taking exclusive devices (Tenstorrent chips, or GPU memory under pressure)
// from one model for another, so that slow model loads do not thrash.
type Swap struct {
	// MinResidency is how long a model keeps its devices after becoming ready before it may be swapped out.
	MinResidency spec.Duration `yaml:"min_residency" json:"min_residency"`
	// MaxResidency: a model with queued requests of its own yields after this long when others wait.
	MaxResidency spec.Duration `yaml:"max_residency" json:"max_residency"`
	// WaitTimeout bounds how long a request waits for its model to be swapped in (then 503 + Retry-After).
	WaitTimeout spec.Duration `yaml:"wait_timeout" json:"wait_timeout"`
}

// Gateway configures `emx serve`.
type Gateway struct {
	Host string `yaml:"host" json:"host"`
	Port int    `yaml:"port" json:"port"`
	// Autostart launches a registered model on its first request.
	Autostart bool `yaml:"autostart" json:"autostart"`
	// Placement decides what may run together: "memory" fits models into free GPU memory and
	// evicts least-recently-used instances of EvictOwners when needed; "exclusive" runs one
	// model at a time (evicts every evictable instance); "off" launches without checks.
	Placement string `yaml:"placement" json:"placement"`
	// EvictOwners lists instance owners placement may stop ("gateway", "mcp", "cli").
	EvictOwners []string `yaml:"evict_owners" json:"evict_owners"`
	// IdleUnload stops evictable instances idle this long (0 never); models may override.
	IdleUnload spec.Duration `yaml:"idle_unload" json:"idle_unload"`
	// RestartCrashed restarts gateway-started instances that crash or hang (at most 3 times in 10 minutes).
	RestartCrashed bool `yaml:"restart_crashed" json:"restart_crashed"`
	// HealthInterval probes ready instances; 3 consecutive failures mark one unhealthy.
	HealthInterval spec.Duration `yaml:"health_interval" json:"health_interval"`
	// MaxConcurrency bounds in-flight requests per model (0 unlimited); models may override.
	MaxConcurrency int `yaml:"max_concurrency" json:"max_concurrency"`
	// MaxQueue bounds requests waiting for a slot per model; more are refused with 429.
	MaxQueue int `yaml:"max_queue" json:"max_queue"`
	// QueueTimeout bounds the wait for a slot.
	QueueTimeout spec.Duration `yaml:"queue_timeout" json:"queue_timeout"`
	// UpstreamHeaderTimeout bounds the wait for an engine's response headers (prefill included).
	UpstreamHeaderTimeout spec.Duration `yaml:"upstream_header_timeout" json:"upstream_header_timeout"`
	// UpstreamIdleTimeout aborts a response when the engine sends nothing for this long.
	UpstreamIdleTimeout spec.Duration `yaml:"upstream_idle_timeout" json:"upstream_idle_timeout"`
	// MetricsPublic serves /metrics without a key.
	MetricsPublic bool `yaml:"metrics_public" json:"metrics_public"`
	// StreamUsage asks engines for a final usage chunk on streamed chat/completions
	// (stream_options.include_usage) so streamed tokens are metered exactly.
	StreamUsage bool `yaml:"stream_usage" json:"stream_usage"`
	// Swap is the anti-thrash policy for evictions.
	Swap Swap `yaml:"swap" json:"swap"`
	// TLS serves HTTPS (read at start; certificate files are re-read when they change).
	TLS TLS `yaml:"tls,omitempty" json:"tls,omitempty"`
	// MaxOutputTokens caps every request's output tokens (0 = no cap); a key's own cap can only lower it.
	// Requests without a limit get it as their limit.
	MaxOutputTokens int `yaml:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
	// DisableSettingsAPI refuses changes to keys, teams, prices, aliases and models through the management
	// API (they are then made with the CLI on the gateway host); reads stay available.
	DisableSettingsAPI bool `yaml:"disable_settings_api,omitempty" json:"disable_settings_api,omitempty"`
	// CacheIsolation keeps clients from learning about each other through the engines' prompt caches: each
	// client (key, or the client a delegating key acts for) sees and pays for only the cached prompt tokens
	// its own earlier prompts explain, and an answer sped up by someone else's cache starts no sooner than
	// one computed in full. Works with every engine; costs the speed-up of cache hits between clients.
	CacheIsolation bool `yaml:"cache_isolation,omitempty" json:"cache_isolation,omitempty"`
	// MaxBodyMB bounds one inference request body in MiB (1-256); larger bodies are refused with 413.
	MaxBodyMB int `yaml:"max_body_mb" json:"max_body_mb"`
	// MaxBufferedMB bounds the request bodies held in memory by all in-flight requests together, in MiB;
	// a body that would exceed it is refused with 503 and Retry-After.
	MaxBufferedMB int `yaml:"max_buffered_mb" json:"max_buffered_mb"`
	// AuditKeyFile names a file holding a secret of at least 32 bytes, accessible by its owner only (relative
	// paths are under the config directory). With it the audit file's chain is keyed (HMAC-SHA256): a file
	// rewritten without the secret does not verify (emx audit verify). The gateway refuses to start
	// when the file is missing, accessible by group or others, or too short.
	AuditKeyFile string `yaml:"audit_key_file,omitempty" json:"audit_key_file,omitempty"`
}

// MaxBodyMBLimit is the highest gateway.max_body_mb.
const MaxBodyMBLimit = 256

// Placement modes.
const (
	PlacementMemory    = "memory"
	PlacementExclusive = "exclusive"
	PlacementOff       = "off"
)

// Logging configures the usage database (state/usage.db).
type Logging struct {
	// Enabled records every gateway request, lifecycle events and resource samples.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Bodies also stores request and response bodies (prompts and outputs).
	Bodies bool `yaml:"bodies" json:"bodies"`
	// FlaggedBodies stores the bodies of requests the policy flagged, for review, even when Bodies is off.
	// They hold whatever was flagged (credentials, personal data): restrict access to the state directory.
	FlaggedBodies bool `yaml:"flagged_bodies" json:"flagged_bodies"`
	// BodyMaxBytes truncates each stored body.
	BodyMaxBytes int `yaml:"body_max_bytes" json:"body_max_bytes"`
	// SampleInterval is how often the gateway samples GPU and instance memory (0 disables).
	SampleInterval spec.Duration `yaml:"sample_interval" json:"sample_interval"`
	// RetentionDays prunes usage records (requests, bodies, lifecycle events, samples) older than this once a
	// day while the gateway runs (0 keeps everything). The admin trail has its own AuditRetentionDays.
	RetentionDays int `yaml:"retention_days" json:"retention_days"`
	// AuditRetentionDays prunes the admin trail (admin events) and rotated audit files older than this
	// (0 keeps them forever). Audit retention is usually longer than usage retention.
	AuditRetentionDays int `yaml:"audit_retention_days,omitempty" json:"audit_retention_days,omitempty"`
	// AuditFile appends one JSON line per request (no bodies), refused API key, administrative change and
	// alert, hash-chained so edits and deletions are detectable (emx audit verify), for shipping to a
	// SIEM or log pipeline; relative paths are under the state directory. Empty disables it.
	AuditFile string `yaml:"audit_file,omitempty" json:"audit_file,omitempty"`
	// AuditMaxMB rotates the audit file when it grows past this size (default 100); rotated files keep the chain.
	AuditMaxMB int `yaml:"audit_max_mb,omitempty" json:"audit_max_mb,omitempty"`
}

// EngineOverride is a user's local adjustment to a manifest.
type EngineOverride struct {
	Bin  string            `yaml:"bin,omitempty" json:"bin,omitempty"`
	Vars map[string]string `yaml:"vars,omitempty" json:"vars,omitempty"`
	Env  map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Args []string          `yaml:"args,omitempty" json:"args,omitempty"`
	// Memory replaces the manifest's memory block for this install, e.g. {mode: none}
	// for a Strata build that runs on a Tenstorrent card instead of the NVIDIA GPUs.
	Memory *spec.Memory `yaml:"memory,omitempty" json:"memory,omitempty"`
	// Accelerator replaces the manifest's accelerator block for this install
	// (e.g. a Strata build for Tenstorrent: {kind: tenstorrent, lock: engine}).
	Accelerator *spec.Accelerator `yaml:"accelerator,omitempty" json:"accelerator,omitempty"`
}

// Default is the configuration used when config.yaml is absent.
func Default() *Config {
	return &Config{
		Gateway: Gateway{Host: "127.0.0.1", Port: 7777, Autostart: true, Placement: PlacementMemory,
			EvictOwners: []string{"gateway"}, RestartCrashed: true, HealthInterval: spec.Duration(15 * time.Second),
			MaxQueue: 64, QueueTimeout: spec.Duration(5 * time.Minute),
			UpstreamHeaderTimeout: spec.Duration(10 * time.Minute), UpstreamIdleTimeout: spec.Duration(5 * time.Minute),
			StreamUsage: true, MaxBodyMB: 64, MaxBufferedMB: 1024,
			Swap: Swap{MinResidency: spec.Duration(30 * time.Second), MaxResidency: spec.Duration(10 * time.Minute),
				WaitTimeout: spec.Duration(15 * time.Minute)}},
		Tenstorrent: Tenstorrent{Lock: "/var/tmp/tenstorrent/chip-{n}.lock", TTSmi: "tt-smi"},
		Logging:     Logging{Enabled: true, BodyMaxBytes: 1 << 20, SampleInterval: spec.Duration(30 * time.Second)},
		Policy:      policy.Config{Enabled: true, Rules: DefaultPolicyRules},
		Classify:    policy.ClassifyConfig{Enabled: true, Default: "general"},
	}
}

// DefaultPolicyRules flag (never block) credentials, personal data and prompt-injection phrasing.
var DefaultPolicyRules = []policy.Rule{
	{Name: "secrets", Severity: "high", Detectors: []string{"api_key", "aws_access_key", "private_key", "jwt", "password_assignment"}},
	{Name: "financial", Severity: "high", Detectors: []string{"credit_card", "iban", "us_ssn"}},
	{Name: "contact_info", Severity: "low", Detectors: []string{"email", "phone"}},
	{Name: "prompt_injection", Severity: "medium", Detectors: []string{"prompt_injection"}},
}

func ConfigPath() string { return filepath.Join(paths.Config(), "config.yaml") }
func ModelsPath() string { return filepath.Join(paths.Config(), "models.yaml") }
func EnginesDir() string { return filepath.Join(paths.Config(), "engines") }
func lockPath() string   { return filepath.Join(paths.Config(), ".lock") }

// decodeStrict rejects unknown keys so a typo in a hand-edited file fails loudly.
func decodeStrict(path string, data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Load reads config.yaml, filling unset fields from Default. On a mesh follower the teams, prices and
// aliases are the copy received from the machine that holds them.
func Load() (*Config, error) {
	c, err := loadRaw()
	if err != nil {
		return nil, err
	}
	if err := c.overlayShared(); err != nil {
		return nil, err
	}
	return c, nil
}

// loadRaw reads config.yaml as it is on disk.
func loadRaw() (*Config, error) {
	c := Default()
	data, err := os.ReadFile(ConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := decodeStrict(ConfigPath(), data, c); err != nil {
		return nil, err
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	if c.Gateway.Port < 0 || c.Gateway.Port > 65535 {
		return fmt.Errorf("%s: gateway.port %d out of range", ConfigPath(), c.Gateway.Port)
	}
	switch c.Gateway.Placement {
	case PlacementMemory, PlacementExclusive, PlacementOff:
	default:
		return fmt.Errorf("%s: gateway.placement %q (want memory, exclusive or off)", ConfigPath(), c.Gateway.Placement)
	}
	for _, o := range c.Gateway.EvictOwners {
		if o != "gateway" && o != "mcp" && o != "cli" {
			return fmt.Errorf("%s: gateway.evict_owners: %q (want gateway, mcp or cli)", ConfigPath(), o)
		}
	}
	if c.Gateway.MaxConcurrency < 0 || c.Gateway.MaxQueue < 0 {
		return fmt.Errorf("%s: gateway.max_concurrency and max_queue must be >= 0", ConfigPath())
	}
	if c.Gateway.MaxBodyMB < 1 || c.Gateway.MaxBodyMB > MaxBodyMBLimit {
		return fmt.Errorf("%s: gateway.max_body_mb must be between 1 and %d", ConfigPath(), MaxBodyMBLimit)
	}
	if c.Gateway.MaxBufferedMB < c.Gateway.MaxBodyMB {
		return fmt.Errorf("%s: gateway.max_buffered_mb must be at least gateway.max_body_mb (%d)", ConfigPath(), c.Gateway.MaxBodyMB)
	}
	if !strings.Contains(c.Tenstorrent.Lock, "{n}") {
		return fmt.Errorf("%s: tenstorrent.lock must contain {n} (the chip index)", ConfigPath())
	}
	if c.Logging.BodyMaxBytes <= 0 {
		return fmt.Errorf("%s: logging.body_max_bytes must be positive", ConfigPath())
	}
	if c.Logging.RetentionDays < 0 || c.Logging.AuditRetentionDays < 0 || c.Logging.AuditMaxMB < 0 {
		return fmt.Errorf("%s: logging.retention_days, audit_retention_days and audit_max_mb must be >= 0", ConfigPath())
	}
	if c.Gateway.MaxOutputTokens < 0 {
		return fmt.Errorf("%s: gateway.max_output_tokens must be >= 0", ConfigPath())
	}
	for _, check := range []func() error{c.Gateway.TLS.Validate, c.Pricing.validate, func() error { return validateTeams(c.Teams) },
		c.Alerts.validate, func() error { return validateAliases(c.Aliases) }, c.Mesh.validate} {
		if err := check(); err != nil {
			return fmt.Errorf("%s: %w", ConfigPath(), err)
		}
	}
	if err := c.Policy.Validate(); err != nil {
		return fmt.Errorf("%s: %w", ConfigPath(), err)
	}
	if _, err := policy.CompileClassifier(&c.Classify); err != nil {
		return fmt.Errorf("%s: %w", ConfigPath(), err)
	}
	return nil
}

// Update applies fn to the current config.yaml under the config lock and saves it.
func Update(fn func(*Config) error) (*Config, error) {
	var out *Config
	err := lock.With(lockPath(), func() error {
		c, err := loadRaw() // a follower's copy of the shared settings is not written into config.yaml
		if err != nil {
			return err
		}
		if err := fn(c); err != nil {
			return err
		}
		if err := c.validate(); err != nil {
			return err
		}
		if err := writeYAML(ConfigPath(), c); err != nil {
			return err
		}
		shown := *c
		out = &shown
		return out.overlayShared()
	})
	return out, err
}

func writeYAML(path string, v any) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	if err := paths.Private(filepath.Dir(path)); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return paths.Replace(tmp, path)
}

// Model is one registry entry.
type Model struct {
	Name string `yaml:"name" json:"name"`
	// Source is a local path (file or directory) or a Hugging Face repo id.
	Source string `yaml:"source" json:"source"`
	Format string `yaml:"format" json:"format"`
	Arch   string `yaml:"arch,omitempty" json:"arch,omitempty"`
	Quant  string `yaml:"quant,omitempty" json:"quant,omitempty"`
	// Engine pins the model to one engine; empty routes automatically.
	Engine string `yaml:"engine,omitempty" json:"engine,omitempty"`
	// Flags are unified flag defaults (manifest keys), applied before CLI flags.
	Flags map[string]string `yaml:"flags,omitempty" json:"flags,omitempty"`
	// Args are extra engine arguments per engine id.
	Args map[string][]string `yaml:"args,omitempty" json:"args,omitempty"`
	// VRAMMiB declares the model's total GPU memory need, overriding estimates and measurements.
	VRAMMiB int64 `yaml:"vram_mib,omitempty" json:"vram_mib,omitempty"`
	// IdleUnload overrides gateway.idle_unload for this model.
	IdleUnload spec.Duration `yaml:"idle_unload,omitempty" json:"idle_unload,omitempty"`
	// MaxConcurrency overrides gateway.max_concurrency for this model.
	MaxConcurrency int `yaml:"max_concurrency,omitempty" json:"max_concurrency,omitempty"`
	// Devices is how many accelerator chips one instance owns (Tenstorrent; overrides the manifest).
	Devices int `yaml:"devices,omitempty" json:"devices,omitempty"`
	// Vars set manifest placeholders for this model (e.g. image, cache) and win over engine vars.
	Vars map[string]string `yaml:"vars,omitempty" json:"vars,omitempty"`
	// PreArgs are inserted at the manifest's {pre_args} element, per engine id (e.g. docker -e options).
	PreArgs map[string][]string `yaml:"pre_args,omitempty" json:"pre_args,omitempty"`
	// Bench describes the model for published benchmarks (LocalMaxxing); unset fields are detected.
	Bench *BenchMeta `yaml:"bench,omitempty" json:"bench,omitempty"`
}

// BenchMeta is what a benchmark submission needs that the registry does not say on its own.
type BenchMeta struct {
	// HFID is the canonical Hugging Face repo of the weights (default: the source, when it is a repo id).
	HFID     string `yaml:"hf_id,omitempty" json:"hf_id,omitempty"`
	Revision string `yaml:"revision,omitempty" json:"revision,omitempty"`
	// Quantization label (default: the registry's quant).
	Quantization string `yaml:"quantization,omitempty" json:"quantization,omitempty"`
	// EngineName as LocalMaxxing knows it (default: derived from the engine id).
	EngineName       string `yaml:"engine_name,omitempty" json:"engine_name,omitempty"`
	EngineVersion    string `yaml:"engine_version,omitempty" json:"engine_version,omitempty"`
	EngineRepository string `yaml:"engine_repository,omitempty" json:"engine_repository,omitempty"`
	EngineCommit     string `yaml:"engine_commit,omitempty" json:"engine_commit,omitempty"`
	// Backend: cuda, rocm, tt-metal, cpu ... (default: from the instance's accelerator).
	Backend       string `yaml:"backend,omitempty" json:"backend,omitempty"`
	ContextLength int    `yaml:"context_length,omitempty" json:"context_length,omitempty"`
	// Spec describes speculative decoding when the engine uses it.
	Spec *BenchSpec `yaml:"spec,omitempty" json:"spec,omitempty"`
	// Hardware is a JSON file replacing the detected hardware description.
	Hardware string `yaml:"hardware,omitempty" json:"hardware,omitempty"`
	// Notes are added to every submission of this model.
	Notes string `yaml:"notes,omitempty" json:"notes,omitempty"`
}

// BenchSpec is a model's speculative decoding setup.
type BenchSpec struct {
	Method     string `yaml:"method" json:"method"` // MTP, EAGLE, DFlash, ngram, draft ...
	NumTokens  int    `yaml:"num_tokens,omitempty" json:"num_tokens,omitempty"`
	DraftModel string `yaml:"draft_model,omitempty" json:"draft_model,omitempty"`
}

// NameRe is what model and instance names must match (they become file names and API model ids).
var NameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Validate checks the entry's name and unified flag defaults.
func (m *Model) Validate() error {
	if !NameRe.MatchString(m.Name) {
		return fmt.Errorf("model name %q must match %s", m.Name, NameRe)
	}
	if m.Source == "" {
		return fmt.Errorf("model %s: empty source", m.Name)
	}
	if m.VRAMMiB < 0 || m.MaxConcurrency < 0 || m.IdleUnload < 0 || m.Devices < 0 {
		return fmt.Errorf("model %s: vram_mib, max_concurrency, idle_unload and devices must be >= 0", m.Name)
	}
	for k, v := range m.Flags {
		f, ok := spec.FlagByKey(k)
		if !ok {
			return fmt.Errorf("model %s: flags.%s is not a unified flag", m.Name, k)
		}
		if _, err := f.Normalize(v); err != nil {
			return fmt.Errorf("model %s: %w", m.Name, err)
		}
	}
	return nil
}

// Models is models.yaml.
type Models struct {
	Models []Model `yaml:"models" json:"models"`
}

// LoadModels reads and validates models.yaml (empty when absent).
func LoadModels() (*Models, error) {
	var m Models
	data, err := os.ReadFile(ModelsPath())
	if errors.Is(err, os.ErrNotExist) {
		return &m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := decodeStrict(ModelsPath(), data, &m); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range m.Models {
		if err := m.Models[i].Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", ModelsPath(), err)
		}
		if seen[m.Models[i].Name] {
			return nil, fmt.Errorf("%s: model %s listed twice", ModelsPath(), m.Models[i].Name)
		}
		seen[m.Models[i].Name] = true
	}
	return &m, nil
}

// UpdateModels applies fn to the current registry under the config lock and saves it.
func UpdateModels(fn func(*Models) error) error {
	return lock.With(lockPath(), func() error {
		m, err := LoadModels()
		if err != nil {
			return err
		}
		if err := fn(m); err != nil {
			return err
		}
		sort.Slice(m.Models, func(i, j int) bool { return m.Models[i].Name < m.Models[j].Name })
		return writeYAML(ModelsPath(), m)
	})
}

// Get returns the model called name.
func (m *Models) Get(name string) (*Model, bool) {
	for i := range m.Models {
		if m.Models[i].Name == name {
			return &m.Models[i], true
		}
	}
	return nil, false
}

// Put adds or replaces a model.
func (m *Models) Put(x Model) {
	if cur, ok := m.Get(x.Name); ok {
		*cur = x
		return
	}
	m.Models = append(m.Models, x)
}

// Remove deletes the model called name; false when absent.
func (m *Models) Remove(name string) bool {
	for i := range m.Models {
		if m.Models[i].Name == name {
			m.Models = append(m.Models[:i], m.Models[i+1:]...)
			return true
		}
	}
	return false
}

// Redacted returns the model as it may be shown: the values of flags that hold credentials (the engine's
// api_key) are masked. models.yaml keeps them, readable by its owner only.
func (m Model) Redacted() Model {
	if len(m.Flags) == 0 {
		return m
	}
	flags := make(map[string]string, len(m.Flags))
	for k, v := range m.Flags {
		if k == "api_key" || strings.Contains(k, "token") || strings.Contains(k, "secret") || strings.Contains(k, "password") {
			v = "***"
		}
		flags[k] = v
	}
	m.Flags = flags
	return m
}

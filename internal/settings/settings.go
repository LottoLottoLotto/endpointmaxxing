// Package settings changes the gateway's settings: API keys, teams, prices, model aliases and the model
// registry. The CLI (and through it MCP) and the management API both call it, so every caller gets the same
// validation and the same admin-trail text; callers record the trail with their own actor.
package settings

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/lotto/substrate/internal/app"
	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/keys"
	"github.com/lotto/substrate/internal/model"
	"github.com/lotto/substrate/internal/route"
	"github.com/lotto/substrate/internal/spec"
	"github.com/lotto/substrate/internal/store"
)

// Result describes an applied change: Detail is the admin-trail line, Warnings what the caller should
// tell the person or agent who asked.
type Result struct {
	Detail   string   `json:"-"`
	Warnings []string `json:"warnings,omitempty"`
}

func (r *Result) warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ParseExpiry reads "90d", "720h", a date or RFC 3339 time, or "never" (nil).
func ParseExpiry(s string) (*time.Time, error) {
	if s == "" || s == "never" {
		return nil, nil
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && strings.HasSuffix(s, "d") && n > 0 {
		t := time.Now().AddDate(0, 0, n).UTC()
		return &t, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		t := time.Now().Add(d).UTC()
		return &t, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			t = t.UTC()
			return &t, nil
		}
	}
	return nil, errs.New(errs.Usage, "expires %q: want a duration from now (90d, 720h), a date (2027-01-31), or never", s)
}

// ExpiryText formats an expiry for the trail.
func ExpiryText(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func keyErr(err error) error {
	if errors.Is(err, keys.ErrNotFound) {
		return errs.Wrap(errs.NotFound, err)
	}
	if strings.Contains(err.Error(), "exists") {
		return errs.Wrap(errs.Conflict, err)
	}
	return errs.Wrap(errs.Usage, err)
}

func noteUnknownTeam(cfg *config.Config, team string, res *Result) {
	if team == "" {
		return
	}
	if _, ok := cfg.Team(team); !ok {
		res.warn("team %q is not configured: usage is attributed to it, but no budget or model limit applies", team)
	}
}

// ---- keys ----

// AddKey creates k and returns its secret (the only time it is visible).
func AddKey(cfg *config.Config, k keys.Key) (string, Result, error) {
	var res Result
	noteUnknownTeam(cfg, k.Team, &res)
	secret, err := keys.Add(k)
	if err != nil {
		return "", res, keyErr(err)
	}
	res.Detail = fmt.Sprintf("key add %s (role %s, team %s, expires %s)", k.Name, k.Role, dash(k.Team), ExpiryText(k.Expires))
	return secret, res, nil
}

// KeyPatch changes some of a key's fields; nil fields stay. An empty Team or Models clears it.
type KeyPatch struct {
	Role            *string
	Team            *string
	Models          *[]string
	RPM             *int
	TokensPerDay    *int64
	MaxOutputTokens *int
	// Expires: set ExpiresSet; a nil Expires then means never.
	ExpiresSet bool
	Expires    *time.Time
}

// SetKey applies p to the key called name.
func SetKey(cfg *config.Config, name string, p KeyPatch) (keys.Key, Result, error) {
	var res Result
	if p.Team != nil {
		noteUnknownTeam(cfg, *p.Team, &res)
	}
	k, err := keys.Update(name, func(k *keys.Key) error {
		if p.Role != nil {
			k.Role = *p.Role
		}
		if p.Team != nil {
			k.Team = *p.Team
		}
		if p.Models != nil {
			k.Models = *p.Models
			if len(k.Models) == 0 {
				k.Models = nil
			}
		}
		if p.RPM != nil {
			k.RPM = *p.RPM
		}
		if p.TokensPerDay != nil {
			k.TokensPerDay = *p.TokensPerDay
		}
		if p.MaxOutputTokens != nil {
			k.MaxOutputTokens = *p.MaxOutputTokens
		}
		if p.ExpiresSet {
			k.Expires = p.Expires
		}
		return nil
	})
	if err != nil {
		return k, res, keyErr(err)
	}
	res.Detail = fmt.Sprintf("key set %s (role %s, team %s, models %s, rpm %d, tokens/day %d, max output %d, expires %s)",
		k.Name, k.Role, dash(k.Team), dash(strings.Join(k.Models, ",")), k.RPM, k.TokensPerDay, k.MaxOutputTokens, ExpiryText(k.Expires))
	return k, res, nil
}

// RotateKey gives the key a new secret; the old one keeps working for grace (0 revokes it now).
func RotateKey(name string, grace time.Duration) (string, keys.Key, Result, error) {
	if grace < 0 {
		return "", keys.Key{}, Result{}, errs.New(errs.Usage, "grace must be >= 0")
	}
	secret, k, err := keys.Rotate(name, grace)
	if err != nil {
		return "", k, Result{}, keyErr(err)
	}
	return secret, k, Result{Detail: fmt.Sprintf("key rotate %s (old secret valid until %s)", k.Name, ExpiryText(k.PrevExpires))}, nil
}

// SetKeysDisabled disables (refused at once, history kept) or re-enables keys.
func SetKeysDisabled(names []string, disabled bool) (Result, error) {
	for _, n := range names {
		if _, err := keys.Update(n, func(k *keys.Key) error { k.Disabled = disabled; return nil }); err != nil {
			return Result{}, keyErr(err)
		}
	}
	verb := "enable"
	if disabled {
		verb = "disable"
	}
	return Result{Detail: fmt.Sprintf("key %s %s", verb, strings.Join(names, ", "))}, nil
}

// RemoveKeys revokes keys.
func RemoveKeys(names []string) (Result, error) {
	for _, n := range names {
		ok, err := keys.Remove(n)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{}, errs.New(errs.NotFound, "no key %q", n)
		}
	}
	return Result{Detail: "key rm " + strings.Join(names, ", ")}, nil
}

// ---- teams ----

// TeamPatch changes some of a team's fields; nil fields stay (a new team starts empty). Empty Models
// allows every model.
type TeamPatch struct {
	TokensPerDay   *int64
	TokensPerMonth *int64
	BudgetPerMonth *float64
	AlertAt        *float64
	Models         *[]string
}

// SetTeam creates the team called name or changes it.
func SetTeam(name string, p TeamPatch) (config.Team, Result, error) {
	var t config.Team
	var res Result
	for _, v := range []*int64{p.TokensPerDay, p.TokensPerMonth} {
		if v != nil && *v < 0 {
			return t, res, errs.New(errs.Usage, "token limits must be >= 0")
		}
	}
	if p.BudgetPerMonth != nil && *p.BudgetPerMonth < 0 {
		return t, res, errs.New(errs.Usage, "budget_per_month must be >= 0")
	}
	if p.AlertAt != nil && (*p.AlertAt < 0 || *p.AlertAt > 1) {
		return t, res, errs.New(errs.Usage, "alert_at must be between 0 and 1")
	}
	cfg, err := config.Update(func(c *config.Config) error {
		i := -1
		for j := range c.Teams {
			if c.Teams[j].Name == name {
				i = j
			}
		}
		if i < 0 {
			c.Teams = append(c.Teams, config.Team{Name: name})
			i = len(c.Teams) - 1
		}
		tt := &c.Teams[i]
		if p.TokensPerDay != nil {
			tt.TokensPerDay = *p.TokensPerDay
		}
		if p.TokensPerMonth != nil {
			tt.TokensPerMonth = *p.TokensPerMonth
		}
		if p.BudgetPerMonth != nil {
			tt.BudgetPerMonth = *p.BudgetPerMonth
		}
		if p.AlertAt != nil {
			tt.AlertAt = *p.AlertAt
		}
		if p.Models != nil {
			tt.Models = *p.Models
			if len(tt.Models) == 0 {
				tt.Models = nil
			}
		}
		t = *tt
		return nil
	})
	if err != nil {
		return t, res, errs.Wrap(errs.Usage, err)
	}
	if t.BudgetPerMonth > 0 && cfg.Pricing.Default == nil && len(cfg.Pricing.Models) == 0 {
		res.warn("team %s has a cost budget but no model has a price, so nothing is charged against it", t.Name)
	}
	res.Detail = fmt.Sprintf("team set %s (tokens/day %d, tokens/month %d, budget/month %g, alert at %g, models %s)",
		t.Name, t.TokensPerDay, t.TokensPerMonth, t.BudgetPerMonth, t.AlertFraction(), dash(strings.Join(t.Models, ",")))
	return t, res, nil
}

// RemoveTeam deletes a team (its keys keep the label but lose the budgets).
func RemoveTeam(name string) (Result, error) {
	ok, err := config.RemoveTeam(name)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		return Result{}, errs.New(errs.NotFound, "no team %q", name)
	}
	return Result{Detail: "team rm " + name}, nil
}

// ---- prices ----

// SetPrice prices a model name, glob or "default" per million tokens; a non-empty currency relabels all prices.
func SetPrice(pattern string, p config.Price, currency string) (Result, error) {
	if pattern == "" {
		return Result{}, errs.New(errs.Usage, "price: a model name, glob or default is required")
	}
	if p.Prompt < 0 || p.Completion < 0 || (p.Cached != nil && *p.Cached < 0) {
		return Result{}, errs.New(errs.Usage, "prices must be >= 0")
	}
	if _, err := config.Update(func(c *config.Config) error {
		if strings.EqualFold(pattern, "default") {
			c.Pricing.Default = &p
		} else {
			if c.Pricing.Models == nil {
				c.Pricing.Models = map[string]config.Price{}
			}
			c.Pricing.Models[pattern] = p
		}
		if currency != "" {
			c.Pricing.Currency = currency
		}
		return nil
	}); err != nil {
		return Result{}, errs.Wrap(errs.Usage, err)
	}
	cached := "= prompt"
	if p.Cached != nil {
		cached = fmt.Sprintf("%g", *p.Cached)
	}
	return Result{Detail: fmt.Sprintf("price set %s (prompt %g, completion %g, cached %s per million tokens)", pattern, p.Prompt, p.Completion, cached)}, nil
}

// RemovePrice removes the price of a model name, glob or "default".
func RemovePrice(pattern string) (Result, error) {
	found := false
	if _, err := config.Update(func(c *config.Config) error {
		if strings.EqualFold(pattern, "default") {
			found, c.Pricing.Default = c.Pricing.Default != nil, nil
			return nil
		}
		_, found = c.Pricing.Models[pattern]
		delete(c.Pricing.Models, pattern)
		return nil
	}); err != nil {
		return Result{}, err
	}
	if !found {
		return Result{}, errs.New(errs.NotFound, "no price for %q", pattern)
	}
	return Result{Detail: "price rm " + pattern}, nil
}

// ---- aliases ----

// SetAlias points alias at a model.
func SetAlias(a *app.App, alias, target string) (Result, error) {
	var res Result
	if !config.NameRe.MatchString(alias) {
		return res, errs.New(errs.Usage, "alias %q must match %s", alias, config.NameRe)
	}
	if target == "" {
		return res, errs.New(errs.Usage, "alias %s: a target model is required", alias)
	}
	if _, ok := a.RegisteredModel(target); !ok {
		res.warn("%s is not a registered model; the alias only works while an instance serves it", target)
	}
	if _, ok := a.RegisteredModel(alias); ok {
		res.warn("%s is also a registered model: requests for it now go to %s", alias, target)
	}
	prev := ""
	if _, err := config.Update(func(c *config.Config) error {
		if c.Aliases == nil {
			c.Aliases = map[string]string{}
		}
		prev = c.Aliases[alias]
		c.Aliases[alias] = target
		return nil
	}); err != nil {
		return res, errs.Wrap(errs.Usage, err)
	}
	res.Detail = fmt.Sprintf("alias set %s -> %s (was %s)", alias, target, dash(prev))
	return res, nil
}

// RemoveAlias removes an alias.
func RemoveAlias(alias string) (Result, error) {
	found := false
	if _, err := config.Update(func(c *config.Config) error {
		_, found = c.Aliases[alias]
		delete(c.Aliases, alias)
		return nil
	}); err != nil {
		return Result{}, err
	}
	if !found {
		return Result{}, errs.New(errs.NotFound, "no alias %q", alias)
	}
	return Result{Detail: "alias rm " + alias}, nil
}

// ---- models ----

// ModelSpec registers a model. Vars, PreArgs and Args reach engine command lines and container options;
// only the CLI (an operator on the host) sets them.
type ModelSpec struct {
	Name, Source, Engine string
	Force                bool
	Flags                map[string]string
	VRAMMiB              int64
	IdleUnload           time.Duration
	MaxConcurrency       int
	Devices              int
	Vars                 map[string]string
	PreArgs              []string
	Args                 []string
}

// Added is a registered model and how it routes.
type Added struct {
	Model  config.Model      `json:"model"`
	Engine string            `json:"engine"`
	Route  []route.Candidate `json:"route"`
}

// AddModel inspects s.Source and registers it.
func AddModel(a *app.App, s ModelSpec) (Added, Result, error) {
	var res Result
	var out Added
	if (len(s.PreArgs) > 0 || len(s.Args) > 0) && s.Engine == "" {
		return out, res, errs.New(errs.Usage, "model add: engine-specific arguments need an engine")
	}
	if s.VRAMMiB < 0 || s.IdleUnload < 0 || s.MaxConcurrency < 0 || s.Devices < 0 {
		return out, res, errs.New(errs.Usage, "model add: vram_mib, idle_unload, max_concurrency and devices must be >= 0")
	}
	info, err := model.Inspect(s.Source)
	if err != nil {
		if os.IsNotExist(err) {
			return out, res, errs.Wrap(errs.NotFound, err)
		}
		return out, res, errs.Wrap(errs.Usage, err)
	}
	// Store absolute paths: the registry must not depend on the caller's cwd.
	if info.Path != "" && info.Ref == info.Path {
		info.Source = info.Path
	}
	m := app.FromInfo(s.Name, info)
	m.Engine = s.Engine
	m.VRAMMiB, m.IdleUnload, m.MaxConcurrency, m.Devices = s.VRAMMiB, spec.Duration(s.IdleUnload), s.MaxConcurrency, s.Devices
	if len(s.Vars) > 0 {
		m.Vars = s.Vars
	}
	if len(s.PreArgs) > 0 {
		m.PreArgs = map[string][]string{s.Engine: s.PreArgs}
	}
	if len(s.Flags) > 0 {
		m.Flags = s.Flags
	}
	if len(s.Args) > 0 {
		m.Args = map[string][]string{s.Engine: s.Args}
	}
	if err := m.Validate(); err != nil {
		return out, res, errs.Wrap(errs.Usage, err)
	}
	cands := a.Route(&app.Target{Model: m, Info: info, Registered: true})
	if s.Engine != "" {
		if _, err := a.Engine(s.Engine); err != nil {
			return out, res, err
		}
		for _, c := range cands {
			if c.Engine == s.Engine && !c.Compatible {
				return out, res, errs.New(errs.Unsupported, "%s cannot serve %s: %s", s.Engine, s.Name, strings.Join(c.Reasons, "; "))
			}
		}
	}
	if err := config.UpdateModels(func(ms *config.Models) error {
		if _, exists := ms.Get(s.Name); exists && !s.Force {
			return errs.New(errs.Conflict, "model %s exists (force replaces it)", s.Name)
		}
		ms.Put(m)
		return nil
	}); err != nil {
		return out, res, err
	}
	pick, perr := route.Pick(cands)
	if perr != nil {
		res.warn("%s registered, but: %v", s.Name, perr)
	}
	res.Detail = fmt.Sprintf("model add %s (source %s, engine %s)", m.Name, m.Source, dash(pick))
	return Added{Model: m, Engine: pick, Route: cands}, res, nil
}

// RemoveModels removes models from the registry (running instances keep running).
func RemoveModels(names []string) (Result, error) {
	if err := config.UpdateModels(func(ms *config.Models) error {
		for _, n := range names {
			if !ms.Remove(n) {
				return errs.New(errs.NotFound, "no model %q", n)
			}
		}
		return nil
	}); err != nil {
		return Result{}, err
	}
	return Result{Detail: "model rm " + strings.Join(names, ", ")}, nil
}

// ---- data ----

// EraseClient deletes every request record and stored body of one client. The admin trail records the
// erasure (not the data); the audit file keeps its lines, as the tamper-evident record of what happened.
func EraseClient(db *store.DB, client string) (Result, map[string]int64, error) {
	if client == "" {
		return Result{}, nil, errs.New(errs.Usage, "erase: a client name is required")
	}
	n, err := db.EraseClient(client)
	if err != nil {
		return Result{}, nil, err
	}
	return Result{Detail: fmt.Sprintf("erase client %s (%d requests, %d bodies)", client, n["requests"], n["bodies"])}, n, nil
}

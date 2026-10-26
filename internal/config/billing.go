package config

import (
	"fmt"
	"path"
	"strings"
)

// TLS serves the gateway over HTTPS. Cert and key files are re-read when they change on disk,
// so a renewed certificate needs no restart.
type TLS struct {
	Cert string `yaml:"cert,omitempty" json:"cert,omitempty"`
	Key  string `yaml:"key,omitempty" json:"key,omitempty"`
	// ClientCA, when set, requires every client to present a certificate signed by one of these CAs (mutual TLS).
	ClientCA string `yaml:"client_ca,omitempty" json:"client_ca,omitempty"`
	// MinVersion is "1.2" (default) or "1.3".
	MinVersion string `yaml:"min_version,omitempty" json:"min_version,omitempty"`
}

// Enabled reports whether TLS is configured.
func (t TLS) Enabled() bool { return t.Cert != "" || t.Key != "" }

// Validate checks that cert and key come together and the options are known.
func (t TLS) Validate() error {
	if (t.Cert == "") != (t.Key == "") {
		return fmt.Errorf("gateway.tls: cert and key go together")
	}
	if t.ClientCA != "" && !t.Enabled() {
		return fmt.Errorf("gateway.tls.client_ca needs cert and key")
	}
	switch t.MinVersion {
	case "", "1.2", "1.3":
	default:
		return fmt.Errorf("gateway.tls.min_version %q (want 1.2 or 1.3)", t.MinVersion)
	}
	return nil
}

// Price is the cost of one million tokens of each kind, in Pricing.Currency.
type Price struct {
	Prompt     float64 `yaml:"prompt" json:"prompt"`
	Completion float64 `yaml:"completion" json:"completion"`
	// Cached prompt tokens; nil charges them at the prompt price.
	Cached *float64 `yaml:"cached,omitempty" json:"cached,omitempty"`
}

// Pricing turns metered tokens into cost. Local models have no invoice; prices are the internal rate
// the operator charges back (amortized hardware and power), or what an equivalent hosted model costs.
type Pricing struct {
	Currency string `yaml:"currency,omitempty" json:"currency,omitempty"`
	// Default prices models without an entry; nil leaves them uncosted.
	Default *Price `yaml:"default,omitempty" json:"default,omitempty"`
	// Models maps model names or path.Match globs to prices; an exact name wins over a glob.
	Models map[string]Price `yaml:"models,omitempty" json:"models,omitempty"`
}

// For returns model's price, or false when the model is not priced.
func (p *Pricing) For(model string) (Price, bool) {
	if pr, ok := p.Models[model]; ok {
		return pr, true
	}
	best := ""
	for g := range p.Models {
		if ok, _ := path.Match(g, model); ok && (best == "" || len(g) > len(best)) {
			best = g // the longest matching glob is the most specific
		}
	}
	if best != "" {
		return p.Models[best], true
	}
	if p.Default != nil {
		return *p.Default, true
	}
	return Price{}, false
}

// Cost prices a request's tokens; false when the model is unpriced or nothing was metered.
func (p *Pricing) Cost(model string, prompt, completion, cached *int64) (float64, bool) {
	pr, ok := p.For(model)
	if !ok || (prompt == nil && completion == nil) {
		return 0, false
	}
	var in, out, hit float64
	if prompt != nil {
		in = float64(*prompt)
	}
	if completion != nil {
		out = float64(*completion)
	}
	if cached != nil && pr.Cached != nil {
		hit = float64(*cached)
		if hit > in {
			hit = in
		}
		in -= hit
	}
	cachedPrice := 0.0
	if pr.Cached != nil {
		cachedPrice = *pr.Cached
	}
	return (in*pr.Prompt + hit*cachedPrice + out*pr.Completion) / 1e6, true
}

// CurrencyOrDefault is the pricing currency, USD when unset.
func (p *Pricing) CurrencyOrDefault() string {
	if p.Currency == "" {
		return "USD"
	}
	return p.Currency
}

func (p *Pricing) validate() error {
	check := func(where string, pr Price) error {
		if pr.Prompt < 0 || pr.Completion < 0 || (pr.Cached != nil && *pr.Cached < 0) {
			return fmt.Errorf("pricing.%s: prices must be >= 0", where)
		}
		return nil
	}
	if p.Default != nil {
		if err := check("default", *p.Default); err != nil {
			return err
		}
	}
	for m, pr := range p.Models {
		if _, err := path.Match(m, ""); err != nil {
			return fmt.Errorf("pricing.models: pattern %q: %w", m, err)
		}
		if err := check("models."+m, pr); err != nil {
			return err
		}
	}
	return nil
}

// Team groups API keys (keys.yaml `team:`) under shared budgets and model access.
// Zero limits are unlimited. Days and months are local calendar periods.
type Team struct {
	Name           string  `yaml:"name" json:"name"`
	TokensPerDay   int64   `yaml:"tokens_per_day,omitempty" json:"tokens_per_day,omitempty"`
	TokensPerMonth int64   `yaml:"tokens_per_month,omitempty" json:"tokens_per_month,omitempty"`
	BudgetPerMonth float64 `yaml:"budget_per_month,omitempty" json:"budget_per_month,omitempty"` // in pricing.currency
	// AlertAt is the fraction of any limit (0-1) at which the gateway raises a budget alert; 0 means 0.8.
	AlertAt float64 `yaml:"alert_at,omitempty" json:"alert_at,omitempty"`
	// Models are path.Match globs the team's keys may use (empty = all); a key's own list narrows it further.
	Models []string `yaml:"models,omitempty" json:"models,omitempty"`
}

// Allows reports whether the team may use model.
func (t *Team) Allows(model string) bool {
	if len(t.Models) == 0 {
		return true
	}
	for _, g := range t.Models {
		if ok, _ := path.Match(g, model); ok {
			return true
		}
	}
	return false
}

// AlertFraction is AlertAt with its default.
func (t *Team) AlertFraction() float64 {
	if t.AlertAt == 0 {
		return 0.8
	}
	return t.AlertAt
}

// Team returns the team called name.
func (c *Config) Team(name string) (*Team, bool) {
	for i := range c.Teams {
		if c.Teams[i].Name == name {
			return &c.Teams[i], true
		}
	}
	return nil, false
}

func validateTeams(teams []Team) error {
	seen := map[string]bool{}
	for i, t := range teams {
		if !NameRe.MatchString(t.Name) {
			return fmt.Errorf("teams[%d]: name %q must match %s", i, t.Name, NameRe)
		}
		if seen[t.Name] {
			return fmt.Errorf("teams: duplicate team %q", t.Name)
		}
		seen[t.Name] = true
		if t.TokensPerDay < 0 || t.TokensPerMonth < 0 || t.BudgetPerMonth < 0 || t.AlertAt < 0 || t.AlertAt > 1 {
			return fmt.Errorf("team %s: limits must be >= 0 and alert_at between 0 and 1", t.Name)
		}
		for _, g := range t.Models {
			if _, err := path.Match(g, ""); err != nil {
				return fmt.Errorf("team %s: model pattern %q: %w", t.Name, g, err)
			}
		}
	}
	return nil
}

// UpdateTeam creates or replaces a team in config.yaml.
func UpdateTeam(t Team) (*Config, error) {
	return Update(func(c *Config) error {
		for i := range c.Teams {
			if c.Teams[i].Name == t.Name {
				c.Teams[i] = t
				return nil
			}
		}
		c.Teams = append(c.Teams, t)
		return nil
	})
}

// RemoveTeam deletes a team from config.yaml; false when absent.
func RemoveTeam(name string) (bool, error) {
	found := false
	_, err := Update(func(c *Config) error {
		for i := range c.Teams {
			if c.Teams[i].Name == name {
				c.Teams = append(c.Teams[:i], c.Teams[i+1:]...)
				found = true
				return nil
			}
		}
		return nil
	})
	return found, err
}

// SetPrice sets (or with nil removes) the price of a model name or glob; "default" sets the default price.
func SetPrice(model string, p *Price) (*Config, error) {
	return Update(func(c *Config) error {
		if strings.EqualFold(model, "default") {
			c.Pricing.Default = p
			return nil
		}
		if p == nil {
			delete(c.Pricing.Models, model)
			return nil
		}
		if c.Pricing.Models == nil {
			c.Pricing.Models = map[string]Price{}
		}
		c.Pricing.Models[model] = *p
		return nil
	})
}

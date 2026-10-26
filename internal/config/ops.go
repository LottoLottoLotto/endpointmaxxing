package config

import (
	"fmt"
	"net/url"
	"sort"
)

// AlertKinds are the events a webhook can subscribe to.
var AlertKinds = []string{
	"budget_alert",         // a team crossed alert_at of a limit
	"config_reload_failed", // config.yaml or models.yaml was invalid; the running configuration stays
	"key_expiring",         // an API key expires within 7 days, or has expired (once a day per key)
	"engine_exited",        // an engine instance crashed or exited on its own
	"engine_start_failed",  // an engine failed to start
	"admin",                // an administrative change: keys, teams, prices, reloads, starts and stops via the API
}

// Webhook posts events to a URL. Format "text" sends {"text": "..."} (Slack, Microsoft Teams, Mattermost,
// Google Chat incoming webhooks); "json" (default) sends {kind, detail, time, host, actor}.
type Webhook struct {
	URL     string            `yaml:"url" json:"url"`
	Format  string            `yaml:"format,omitempty" json:"format,omitempty"`
	Kinds   []string          `yaml:"kinds,omitempty" json:"kinds,omitempty"` // empty = all
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

// Wants reports whether the webhook subscribes to kind.
func (w *Webhook) Wants(kind string) bool {
	if len(w.Kinds) == 0 {
		return true
	}
	for _, k := range w.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Alerts configures where operational events are sent.
type Alerts struct {
	Webhooks []Webhook `yaml:"webhooks,omitempty" json:"webhooks,omitempty"`
}

func (a *Alerts) validate() error {
	for i, w := range a.Webhooks {
		u, err := url.Parse(w.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("alerts.webhooks[%d]: url %q must be an http(s) URL", i, w.URL)
		}
		if w.Format != "" && w.Format != "json" && w.Format != "text" {
			return fmt.Errorf("alerts.webhooks[%d]: format %q (want json or text)", i, w.Format)
		}
		for _, k := range w.Kinds {
			known := false
			for _, ak := range AlertKinds {
				known = known || ak == k
			}
			if !known {
				return fmt.Errorf("alerts.webhooks[%d]: unknown kind %q (want %v)", i, k, AlertKinds)
			}
		}
	}
	return nil
}

// Alias resolves name through aliases (one level; aliases cannot point at aliases).
func (c *Config) Alias(name string) (string, bool) {
	target, ok := c.Aliases[name]
	return target, ok
}

func validateAliases(aliases map[string]string) error {
	names := make([]string, 0, len(aliases))
	for a := range aliases {
		names = append(names, a)
	}
	sort.Strings(names)
	for _, a := range names {
		t := aliases[a]
		if !NameRe.MatchString(a) || !NameRe.MatchString(t) {
			return fmt.Errorf("aliases: %q -> %q: names must match %s", a, t, NameRe)
		}
		if _, chained := aliases[t]; chained {
			return fmt.Errorf("aliases: %q points at another alias %q; point it at a model", a, t)
		}
	}
	return nil
}

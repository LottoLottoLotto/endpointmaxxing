package gateway

import (
	"context"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/audit"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
)

// live is what the gateway derives from one config snapshot. Requests take the current one and keep it
// for their whole life, so a reload never changes the rules under a request already in flight.
type live struct {
	cfg     *config.Config
	inspect *Inspector
	audit   *audit.Log
	// auditStale: cfg's audit writer could not be opened (its key file was refused), so audit is still the
	// one of the config before; the next reload tries again.
	auditStale bool
}

// reloadState tracks the current live snapshot and the last reload outcome.
type reloadState struct {
	mu       sync.Mutex
	cur      *live
	lastErr  string
	lastAt   time.Time
	failures int64
	reloads  int64
}

type fileStamp struct {
	mod  time.Time
	size int64
}

// gw is the gateway section of the current config.
func (g *Gateway) gw() *config.Gateway { return &g.App.Config().Gateway }

// autostart reports whether models start on their first request (config, unless serve --no-autostart).
func (g *Gateway) autostart() bool { return g.gw().Autostart && !g.NoAutostart }

// live returns the snapshot for the current config, rebuilding it when the config was reloaded.
func (g *Gateway) live() *live {
	c := g.App.Config()
	g.reload.mu.Lock()
	defer g.reload.mu.Unlock()
	cur := g.reload.cur
	if cur != nil && cur.cfg == c {
		return cur
	}
	next := &live{cfg: c}
	insp, err := newInspectorFrom(c)
	switch {
	case err == nil:
		next.inspect = insp
	case cur != nil: // config.Load validated it; keep the previous rules rather than run without any
		g.Log.Printf("policy/classify: %v; keeping the previous rules", err)
		next.inspect = cur.inspect
	default:
		g.Log.Printf("policy/classify disabled: %v", err)
	}
	if cur != nil && cur.audit != nil && !cur.auditStale && cur.audit.PathOf() == audit.Path(c.Logging.AuditFile) &&
		cur.cfg.Logging.AuditMaxMB == c.Logging.AuditMaxMB && cur.cfg.Gateway.AuditKeyFile == c.Gateway.AuditKeyFile {
		next.audit = cur.audit
	} else if l, err := audit.Open(audit.Path(c.Logging.AuditFile), c.Logging.AuditMaxMB, audit.KeyPath(c.Gateway.AuditKeyFile), g.Log.Printf); err != nil {
		// serve checks the key before it starts the gateway, so this is a reload: better the previous writer than none
		g.Log.Printf("audit log: %v; the audit file is written as before this configuration", err)
		next.auditStale = true
		if cur != nil {
			next.audit = cur.audit
		}
	} else {
		next.audit = l
		if cur != nil {
			cur.audit.Close() // a late write from a request on the old snapshot reopens the file; harmless
		}
	}
	g.reload.cur = next
	return next
}

// Reload re-reads config.yaml and models.yaml now. An invalid file keeps the running configuration;
// each distinct failure is logged and recorded once.
func (g *Gateway) Reload() error {
	before := g.App.Config()
	err := g.App.Refresh()
	g.reload.mu.Lock()
	g.reload.lastAt = time.Now()
	repeated := err != nil && err.Error() == g.reload.lastErr
	if err != nil {
		g.reload.lastErr = err.Error()
		g.reload.failures++
	} else {
		g.reload.lastErr = ""
	}
	g.reload.mu.Unlock()
	if err != nil {
		if !repeated {
			g.Log.Printf("config reload failed, keeping the running configuration: %v", err)
			g.recordEvent("config_reload_failed", err.Error())
		}
		return err
	}
	if after := g.App.Config(); after != before {
		g.live()
		g.reload.mu.Lock()
		g.reload.reloads++
		g.reload.mu.Unlock()
		changed := strings.Join(changedSections(before, after), ", ")
		g.Log.Printf("config reloaded (changed: %s)", changed)
		g.recordEvent("config_reloaded", "changed: "+changed)
	}
	return nil
}

// changedSections names the top-level config.yaml sections that differ between a and b.
func changedSections(a, b *config.Config) []string {
	va, vb := reflect.ValueOf(*a), reflect.ValueOf(*b)
	var out []string
	for i := 0; i < va.NumField(); i++ {
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			name, _, _ := strings.Cut(va.Type().Field(i).Tag.Get("yaml"), ",")
			out = append(out, name)
		}
	}
	return out
}

// watchConfig reloads when config.yaml or models.yaml change on disk (checked every interval).
func (g *Gateway) watchConfig(ctx context.Context, interval time.Duration) {
	files := []string{config.ConfigPath(), config.ModelsPath(), config.SharedPath()}
	stamp := func() map[string]fileStamp {
		out := map[string]fileStamp{}
		for _, f := range files {
			if st, err := os.Stat(f); err == nil {
				out[f] = fileStamp{st.ModTime(), st.Size()}
			}
		}
		return out
	}
	last := stamp()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := stamp()
		changed := len(now) != len(last)
		for f, s := range now {
			changed = changed || last[f] != s
		}
		if changed {
			last = now
			g.Reload()
		}
	}
}

// recordEvent writes a gateway event (kind, detail) to the usage database when logging is on, and sends
// alert kinds (config.AlertKinds) to the webhooks and the audit log.
func (g *Gateway) recordEvent(kind, detail string) {
	for _, k := range config.AlertKinds {
		if k == kind {
			g.alert(kind, detail)
		}
	}
	if !g.App.Config().Logging.Enabled {
		return
	}
	go func() {
		db, err := g.App.DB()
		if err == nil {
			err = db.InsertEvent(&store.Event{Kind: kind, Owner: "gateway", Detail: detail})
		}
		if err != nil {
			g.Log.Printf("recording %s event: %v", kind, err)
		}
	}()
}

// reloadStatus reports the last reload for the management API and metrics.
func (g *Gateway) reloadStatus() map[string]any {
	g.reload.mu.Lock()
	defer g.reload.mu.Unlock()
	out := map[string]any{"reloads": g.reload.reloads, "failures": g.reload.failures}
	if !g.reload.lastAt.IsZero() {
		out["last_attempt"] = g.reload.lastAt
	}
	if g.reload.lastErr != "" {
		out["last_error"] = g.reload.lastErr
	}
	return out
}

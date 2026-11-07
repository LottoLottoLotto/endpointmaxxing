package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/user"
	"sync"
	"time"

	"github.com/lotto/substrate/internal/app"
	"github.com/lotto/substrate/internal/audit"
	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/keys"
	"github.com/lotto/substrate/internal/runtime"
	"github.com/lotto/substrate/internal/store"
)

// webhookTimeout bounds one webhook delivery attempt.
const webhookTimeout = 5 * time.Second

var webhookClient = &http.Client{Timeout: webhookTimeout}

// alertPayload is the "json" webhook format.
type alertPayload struct {
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
	Actor  string    `json:"actor,omitempty"`
	Time   time.Time `json:"time"`
	Host   string    `json:"host"`
}

// sendWebhooks delivers an event to every webhook subscribed to kind, retrying a failed delivery once.
// It returns when all deliveries finished; callers that must not block run it in a goroutine.
func sendWebhooks(cfg *config.Config, kind, detail, actor string, logf func(string, ...any)) {
	host, _ := os.Hostname()
	var wg sync.WaitGroup
	for _, w := range cfg.Alerts.Webhooks {
		if !w.Wants(kind) {
			continue
		}
		var body []byte
		if w.Format == "text" {
			text := fmt.Sprintf("[substrate %s] %s: %s", host, kind, detail)
			if actor != "" {
				text += " (by " + actor + ")"
			}
			body, _ = json.Marshal(map[string]string{"text": text})
		} else {
			body, _ = json.Marshal(alertPayload{Kind: kind, Detail: detail, Actor: actor, Time: time.Now().UTC(), Host: host})
		}
		wg.Add(1)
		go func(w config.Webhook) {
			defer wg.Done()
			var err error
			for attempt := 0; attempt < 2; attempt++ {
				if attempt > 0 {
					time.Sleep(2 * time.Second)
				}
				if err = postWebhook(w, body); err == nil {
					return
				}
			}
			logf("webhook %s (%s): %v", redactURL(w.URL), kind, err)
		}(w)
	}
	wg.Wait()
}

func postWebhook(w config.Webhook, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range w.Headers {
		req.Header.Set(k, v)
	}
	resp, err := webhookClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}

// redactURL keeps a webhook URL's scheme and host: the path of an incoming webhook is its secret.
func redactURL(u string) string {
	for i := 0; i+2 < len(u); i++ {
		if u[i] == '/' && u[i+1] == '/' {
			if j := bytes.IndexByte([]byte(u[i+2:]), '/'); j >= 0 {
				return u[:i+2+j] + "/..."
			}
			return u
		}
	}
	return u
}

// Actor names the person behind a CLI action: $SUBSTRATE_ACTOR, else the OS user.
func Actor() string {
	if a := os.Getenv("SUBSTRATE_ACTOR"); a != "" {
		return a
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return "unknown"
}

// RecordAdmin records an administrative change: an "admin" event in the usage database (when logging is
// on), an audit line (when logging.audit_file is set), and the webhooks subscribed to "admin". It waits for
// webhook delivery, so a CLI process does not exit before sending.
func RecordAdmin(a *app.App, actor, detail string) {
	cfg := a.Config()
	logf := log.New(os.Stderr, "substrate: ", 0).Printf
	if a.Warn != nil {
		logf = a.Warn
	}
	if cfg.Logging.Enabled {
		if db, err := a.DB(); err == nil {
			if err := db.InsertEvent(&store.Event{Kind: "admin", Owner: actor, Detail: detail}); err != nil {
				logf("recording admin event: %v", err)
			}
		}
	}
	if l := audit.Open(audit.Path(cfg.Logging.AuditFile), cfg.Logging.AuditMaxMB, logf); l != nil {
		l.Write(auditEvent{Type: "admin", Time: time.Now().UTC().Format(time.RFC3339Nano), Actor: actor, Detail: detail})
		l.Close()
	}
	sendWebhooks(cfg, "admin", detail, actor, logf)
}

// admin records a change made through the management API by the request's key.
func (g *Gateway) admin(r *http.Request, detail string) {
	actor := principalOf(r).name
	if actor == "" {
		actor = "open-mode client " + clientIP(r)
	}
	go RecordAdmin(g.App, actor, detail)
}

// alert sends an operational event to the webhooks subscribed to kind and the audit log (without blocking).
func (g *Gateway) alert(kind, detail string) {
	cfg := g.App.Config()
	g.live().audit.Write(auditEvent{Type: kind, Time: time.Now().UTC().Format(time.RFC3339Nano), Detail: detail})
	if len(cfg.Alerts.Webhooks) > 0 {
		go sendWebhooks(cfg, kind, detail, "", g.Log.Printf)
	}
}

// watchEngines forwards engine crashes and failed starts (lifecycle events) to the alert webhooks.
// Call before Run starts its goroutines: it wraps the process-wide event recorder.
func (g *Gateway) watchEngines() {
	prev := runtime.Recorder
	runtime.Recorder = func(e store.Event) {
		if prev != nil {
			prev(e)
		}
		switch e.Kind {
		case "exited":
			g.alert("engine_exited", fmt.Sprintf("%s (%s, model %s) exited: %s", e.Instance, e.Engine, e.Model, firstLine(e.Detail)))
		case "start_failed":
			g.alert("engine_start_failed", fmt.Sprintf("%s (%s, model %s) failed to start: %s", e.Instance, e.Engine, e.Model, firstLine(e.Detail)))
		}
	}
}

func firstLine(s string) string {
	if i := bytes.IndexByte([]byte(s), '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// keyExpiryWarning is how far ahead key_expiring alerts start.
const keyExpiryWarning = 7 * 24 * time.Hour

// checkKeyExpiry alerts, once a day per key, for keys expiring within keyExpiryWarning or expired in it.
func (g *Gateway) checkKeyExpiry() {
	f, err := keys.Load()
	if err != nil {
		return
	}
	day, _ := today()
	now := time.Now()
	for _, k := range f.Keys {
		if k.Expires == nil || k.Disabled {
			continue
		}
		left := k.Expires.Sub(now)
		if left > keyExpiryWarning || left < -keyExpiryWarning {
			continue
		}
		g.expiryMu.Lock()
		seen := g.expiryAlerted[k.Name] == day
		g.expiryAlerted[k.Name] = day
		g.expiryMu.Unlock()
		if seen {
			continue
		}
		msg := fmt.Sprintf("API key %s expires %s (in %s)", k.Name, k.Expires.Local().Format("2006-01-02 15:04"), left.Round(time.Hour))
		if left <= 0 {
			msg = fmt.Sprintf("API key %s expired %s", k.Name, k.Expires.Local().Format("2006-01-02 15:04"))
		}
		g.recordEvent("key_expiring", msg)
	}
}

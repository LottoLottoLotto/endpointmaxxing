package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lotto/substrate/internal/app"
	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/paths"
	"github.com/lotto/substrate/internal/policy"
	"github.com/lotto/substrate/internal/runtime"
	"github.com/lotto/substrate/internal/store"
)

// Inspector applies config.yaml's policy and classify sections to request bodies.
type Inspector struct {
	policy     *policy.Policy     // nil: policy off
	classifier *policy.Classifier // nil: classification off
}

// NewInspector compiles a's policy and classify sections.
func NewInspector(a *app.App) (*Inspector, error) {
	in := &Inspector{}
	var err error
	if a.Cfg.Policy.Enabled {
		if in.policy, err = policy.Compile(&a.Cfg.Policy); err != nil {
			return nil, err
		}
	}
	if a.Cfg.Classify.Enabled {
		if in.classifier, err = policy.CompileClassifier(&a.Cfg.Classify); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// Inspect labels rec (Model, Client and API set) with its prompt's category and policy flags;
// it reports whether a blocking rule matched.
func (in *Inspector) Inspect(rec *store.Request, body []byte) (blocked bool) {
	if in == nil || (in.policy == nil && in.classifier == nil) {
		return false
	}
	p := policy.ExtractPrompt(body)
	rec.Category = in.classifier.Classify(p.Last)
	rec.Flags, blocked = in.policy.Scan(p.All, policy.Scope{Model: rec.Model, Client: rec.Client, API: rec.API})
	rec.FlagSeverity = maxSeverity(rec.Flags)
	return blocked
}

func maxSeverity(flags []policy.Flag) string {
	best := -1
	for _, f := range flags {
		for i, s := range policy.Severities {
			if s == f.Severity && i > best {
				best = i
			}
		}
	}
	if best < 0 {
		return ""
	}
	return policy.Severities[best]
}

// errPolicyBlocked refuses a request that matched a blocking rule; the message names the rules, never the matched text.
type errPolicyBlocked struct{ rules []string }

func (e errPolicyBlocked) Error() string {
	return "request blocked by content policy (rules: " + strings.Join(e.rules, ", ") + ")"
}

func blockingRules(flags []policy.Flag) []string {
	var out []string
	for _, f := range flags {
		if f.Action == policy.ActionBlock && !contains(out, f.Rule) {
			out = append(out, f.Rule)
		}
	}
	return out
}

// gatewayErrorKind classifies a failure the gateway itself answered (see store.ErrorKinds).
func gatewayErrorKind(status int, err error) string {
	var blocked errPolicyBlocked
	switch {
	case errors.As(err, &blocked):
		return "policy_blocked"
	case status == http.StatusUnauthorized:
		return "auth_failed"
	case status == http.StatusForbidden:
		return "forbidden"
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge:
		return "bad_request"
	case status == 499:
		return "client_disconnected"
	}
	switch errs.CodeOf(err) {
	case errs.NotFound:
		return "model_not_found"
	case errs.Capacity, errs.Timeout:
		return "no_capacity"
	case errs.EngineFailed:
		return "engine_start_failed"
	case errs.Unsupported, errs.Usage:
		return "bad_request"
	}
	return "gateway_error"
}

// auditLog appends JSON lines to logging.audit_file. It reopens the file when it was rotated away
// (renamed or deleted), so logrotate needs no copytruncate or signal.
type auditLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	log  func(format string, a ...any)
	errd bool
}

func newAuditLog(path string, logf func(string, ...any)) *auditLog {
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(paths.State(), path)
	}
	return &auditLog{path: path, log: logf}
}

// auditRecord is one audit line: a request record (bodies never included) with its kind and an RFC 3339 time.
type auditRecord struct {
	Type string `json:"type"` // "request", or "access_denied" for auth_failed / forbidden
	Time string `json:"time"`
	*store.Request
}

func (l *auditLog) write(r *store.Request) {
	if l == nil {
		return
	}
	typ := "request"
	if r.ErrorKind == "auth_failed" || r.ErrorKind == "forbidden" {
		typ = "access_denied"
	}
	line, err := json.Marshal(auditRecord{Type: typ, Time: time.UnixMilli(r.TS).UTC().Format(time.RFC3339Nano), Request: r})
	if err != nil {
		return
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		st, err1 := os.Stat(l.path)
		fst, err2 := l.f.Stat()
		if err1 != nil || err2 != nil || !os.SameFile(st, fst) {
			l.f.Close()
			l.f = nil
		}
	}
	if l.f == nil {
		if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err == nil {
			l.f, err = os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		}
		if l.f == nil {
			if !l.errd {
				l.log("audit log %s: cannot open; audit lines are being dropped", l.path)
				l.errd = true
			}
			return
		}
		l.errd = false
	}
	if _, err := l.f.Write(line); err != nil && !l.errd {
		l.log("audit log %s: %v", l.path, err)
		l.errd = true
	}
}

func (l *auditLog) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}

// Report summarizes a window of gateway traffic.
type Report struct {
	Since      *time.Time        `json:"since,omitempty"`
	Until      *time.Time        `json:"until,omitempty"`
	Totals     *store.UsageRow   `json:"totals"`
	ByModel    []store.UsageRow  `json:"by_model"`
	ByClient   []store.UsageRow  `json:"by_client"`
	ByCategory []store.UsageRow  `json:"by_category"`
	Flags      []store.FlagRow   `json:"flags"`
	Errors     []store.ErrorRow  `json:"errors"`
	Models     ReportModelCounts `json:"models"`
}

// ReportModelCounts counts the model fleet now (not windowed).
type ReportModelCounts struct {
	Registered int `json:"registered"`
	Running    int `json:"running"`
	Ready      int `json:"ready"`
	// Used is how many distinct models answered at least one request successfully in the window.
	Used int `json:"used"`
}

// BuildReport aggregates usage, flags and errors for f, plus the current model fleet.
func BuildReport(a *app.App, db *store.DB, f store.Filter) (*Report, error) {
	r := &Report{}
	if !f.Since.IsZero() {
		r.Since = &f.Since
	}
	if !f.Until.IsZero() {
		r.Until = &f.Until
	}
	f.Limit = 0
	totals, err := db.Usage("", f)
	if err != nil {
		return nil, err
	}
	if len(totals) > 0 {
		r.Totals = &totals[0]
	} else {
		r.Totals = &store.UsageRow{Key: "total"}
	}
	for _, g := range []struct {
		by  string
		out *[]store.UsageRow
	}{{"model", &r.ByModel}, {"client", &r.ByClient}, {"category", &r.ByCategory}} {
		if *g.out, err = db.Usage(g.by, f); err != nil {
			return nil, err
		}
		if *g.out == nil {
			*g.out = []store.UsageRow{}
		}
	}
	for _, m := range r.ByModel {
		if m.Key != "" && m.Requests > m.Errors {
			r.Models.Used++
		}
	}
	if r.Flags, err = db.FlagStats(f); err != nil {
		return nil, err
	}
	if r.Errors, err = db.ErrorStats(f); err != nil {
		return nil, err
	}
	if r.Flags == nil {
		r.Flags = []store.FlagRow{}
	}
	if r.Errors == nil {
		r.Errors = []store.ErrorRow{}
	}
	r.Models.Registered = len(a.RegisteredModels())
	ins, err := runtime.List()
	if err != nil {
		return nil, fmt.Errorf("listing instances: %w", err)
	}
	r.Models.Running = len(ins)
	for _, in := range ins {
		if in.ReadyAt != nil {
			r.Models.Ready++
		}
	}
	return r, nil
}

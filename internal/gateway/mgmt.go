package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/keys"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/policy"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/runtime"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
)

func (g *Gateway) mgmtInstances(w http.ResponseWriter, r *http.Request) {
	ins, _ := runtime.List()
	out := make([]map[string]any, 0, len(ins))
	for _, in := range ins {
		idle, last := g.activity(in.ID)
		g.activeMu.Lock()
		active := g.active[in.ID]
		g.activeMu.Unlock()
		out = append(out, map[string]any{"instance": in.Redacted(), "status": g.instanceStatus(r.Context(), in),
			"active_requests": active, "idle": idle, "last_used": last, "api_url": in.BaseURL + "/v1"})
	}
	writeJSON(w, http.StatusOK, out)
}

func (g *Gateway) mgmtEnsure(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model   string `json:"model"`
		Engine  string `json:"engine"`
		Timeout string `json:"timeout"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Model == "" {
		apiError(w, http.StatusBadRequest, "invalid_request_error", `body must be {"model": "...", "engine"?: "...", "timeout"?: "10m"}`)
		return
	}
	var timeout time.Duration
	if req.Timeout != "" {
		var err error
		if timeout, err = time.ParseDuration(req.Timeout); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
	}
	if err := g.App.Refresh(); err != nil {
		writeErr(w, err)
		return
	}
	g.addWaiting(req.Model, 1)
	defer g.addWaiting(req.Model, -1)
	if err := g.awaitTurn(r.Context(), req.Model, req.Engine); err != nil {
		writeErr(w, err)
		return
	}
	in, err := g.ensure(context.WithoutCancel(r.Context()), req.Model, req.Engine, "", timeout)
	if err != nil {
		writeErr(w, err)
		return
	}
	g.admin(r, fmt.Sprintf("ensure %s: instance %s ready", req.Model, in.ID))
	writeJSON(w, http.StatusOK, map[string]any{"instance": in.Redacted(), "api_url": in.BaseURL + "/v1", "status": runtime.StatusReady})
}

func (g *Gateway) mgmtStop(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.ID == "" {
		apiError(w, http.StatusBadRequest, "invalid_request_error", `body must be {"id": "<instance id or model name>"}`)
		return
	}
	in, err := runtime.Find(req.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	g.waitIdle(in.ID, 2*time.Minute)
	if err := in.StopFor("stopped through the management API"); err != nil {
		writeErr(w, err)
		return
	}
	g.setStatus(in.ID, runtime.StatusExited)
	g.admin(r, fmt.Sprintf("stop %s", in.ID))
	writeJSON(w, http.StatusOK, map[string]any{"stopped": in.ID})
}

// filterFrom reads since/until/model/engine/instance/client/limit, category/error_kind/errors/flagged/
// min_severity/rule/id (requests) and kind (events) query parameters; a user key only ever sees its own records.
func filterFrom(r *http.Request) (store.Filter, error) {
	q := r.URL.Query()
	f := store.Filter{Model: q.Get("model"), Engine: q.Get("engine"), Instance: q.Get("instance"), Client: q.Get("client"),
		Category: q.Get("category"), ErrorKind: q.Get("error_kind"), MinSeverity: q.Get("min_severity"), Rule: q.Get("rule"),
		ID: q.Get("id"), Kind: q.Get("kind"),
		Errors: q.Get("errors") == "true" || q.Get("errors") == "1", Flagged: q.Get("flagged") == "true" || q.Get("flagged") == "1",
		Entered: q.Get("entered") == "true" || q.Get("entered") == "1"}
	if f.MinSeverity != "" && !contains(policy.Severities, f.MinSeverity) {
		return f, errs.New(errs.Usage, "min_severity %q: want %s", f.MinSeverity, strings.Join(policy.Severities, ", "))
	}
	if p := principalOf(r); p.role != keys.RoleAdmin && p.role != keys.RoleAuditor {
		f.Client = p.name
	}
	f.Team = q.Get("team")
	var err error
	if s := q.Get("since"); s != "" {
		if f.Since, err = ParseSince(s); err != nil {
			return f, err
		}
	}
	if s := q.Get("until"); s != "" {
		if f.Until, err = ParseSince(s); err != nil {
			return f, err
		}
	}
	if s := q.Get("limit"); s != "" {
		if f.Limit, err = strconv.Atoi(s); err != nil {
			return f, fmt.Errorf("limit: %w", err)
		}
	}
	return f, nil
}

// ParseSince accepts a duration back from now (90m, 24h, 7d) or a date/time (2026-10-01, RFC 3339).
func ParseSince(s string) (time.Time, error) {
	if strings.HasSuffix(s, "d") {
		if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil {
			return time.Now().AddDate(0, 0, -n), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errs.New(errs.Usage, "time %q: want a duration (24h, 7d) or a date (2026-10-01)", s)
}

func (g *Gateway) withDB(w http.ResponseWriter, r *http.Request, fn func(*store.DB, store.Filter) (any, error)) {
	f, err := filterFrom(r)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	db, err := g.App.DB()
	if err != nil {
		writeErr(w, err)
		return
	}
	out, err := fn(db, f)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (g *Gateway) mgmtUsage(w http.ResponseWriter, r *http.Request) {
	g.withDB(w, r, func(db *store.DB, f store.Filter) (any, error) {
		rows, err := db.Usage(r.URL.Query().Get("by"), f)
		if err != nil {
			return nil, errs.Wrap(errs.Usage, err)
		}
		return rows, nil
	})
}

func (g *Gateway) mgmtRequests(w http.ResponseWriter, r *http.Request) {
	g.withDB(w, r, func(db *store.DB, f store.Filter) (any, error) {
		if f.Limit == 0 {
			f.Limit = 100
		}
		return db.Requests(f)
	})
}

func (g *Gateway) mgmtEvents(w http.ResponseWriter, r *http.Request) {
	g.withDB(w, r, func(db *store.DB, f store.Filter) (any, error) {
		if f.Limit == 0 {
			f.Limit = 100
		}
		return db.Events(f)
	})
}

// mgmtFlags aggregates policy matches by rule and detector.
func (g *Gateway) mgmtFlags(w http.ResponseWriter, r *http.Request) {
	g.withDB(w, r, func(db *store.DB, f store.Filter) (any, error) {
		rows, err := db.FlagStats(f)
		if rows == nil {
			rows = []store.FlagRow{}
		}
		return rows, err
	})
}

// mgmtErrors aggregates failed requests by error kind.
func (g *Gateway) mgmtErrors(w http.ResponseWriter, r *http.Request) {
	g.withDB(w, r, func(db *store.DB, f store.Filter) (any, error) {
		rows, err := db.ErrorStats(f)
		if rows == nil {
			rows = []store.ErrorRow{}
		}
		return rows, err
	})
}

// mgmtReport summarizes usage, models, categories, flags and errors (since defaults to 24h).
func (g *Gateway) mgmtReport(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("since") == "" {
		q := r.URL.Query()
		q.Set("since", "24h")
		r.URL.RawQuery = q.Encode()
	}
	g.withDB(w, r, func(db *store.DB, f store.Filter) (any, error) {
		return BuildReport(g.App, db, f)
	})
}

// mgmtPolicyTest runs the gateway's policy and classifier over {"text": "..."} or a request body
// ({"body": {...}}) without sending anything to a model.
func (g *Gateway) mgmtPolicyTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text   string          `json:"text"`
		Body   json.RawMessage `json:"body"`
		Model  string          `json:"model"`
		Client string          `json:"client"`
		API    string          `json:"api"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, bodyLimit(g.gw()))).Decode(&req); err != nil || (req.Text == "" && len(req.Body) == 0) {
		apiError(w, http.StatusBadRequest, "invalid_request_error", `body must be {"text": "..."} or {"body": {<chat request>}}, optionally with "model", "client", "api"`)
		return
	}
	body := []byte(req.Body)
	if req.Text != "" {
		body, _ = json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": req.Text}}})
	}
	if req.API == "" {
		req.API = "chat"
	}
	rec := &store.Request{Model: req.Model, Client: req.Client, API: req.API}
	insp := g.live().inspect
	blocked := insp.Inspect(rec, body)
	writeJSON(w, http.StatusOK, map[string]any{"category": rec.Category, "flags": rec.Flags, "flag_severity": rec.FlagSeverity,
		"blocked": blocked, "policy_enabled": insp != nil && insp.policy != nil,
		"classify_enabled": insp != nil && insp.classifier != nil})
}

// KeyInfo is an API key as reported to operators: no secret material, plus its status and last use.
type KeyInfo struct {
	keys.Key
	Status   string     `json:"status"`
	LastUsed *time.Time `json:"last_used"`
}

// KeyInfos lists keys.yaml with each key's status and last successful use (db may be nil).
func KeyInfos(db *store.DB) ([]KeyInfo, error) {
	f, err := keys.Load()
	if err != nil {
		return nil, err
	}
	last := map[string]int64{}
	if db != nil {
		if last, err = db.LastUsed(); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	out := make([]KeyInfo, 0, len(f.Keys))
	for _, k := range f.Keys {
		ki := KeyInfo{Key: k, Status: k.Status(now)}
		if ts, ok := last[k.Name]; ok {
			t := time.UnixMilli(ts)
			ki.LastUsed = &t
		}
		out = append(out, ki)
	}
	return out, nil
}

func (g *Gateway) mgmtKeys(w http.ResponseWriter, r *http.Request) {
	db, _ := g.App.DB() // without a usage database keys still list, without last use
	out, err := KeyInfos(db)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// keysPerTeam counts keys.yaml keys by team.
func keysPerTeam() map[string]int {
	out := map[string]int{}
	if f, err := keys.Load(); err == nil {
		for _, k := range f.Keys {
			if k.Team != "" {
				out[k.Team]++
			}
		}
	}
	return out
}

func (g *Gateway) mgmtTeams(w http.ResponseWriter, r *http.Request) {
	db, _ := g.App.DB()
	out, err := TeamStatuses(g.App.Config(), db, keysPerTeam())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (g *Gateway) mgmtReloadStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, g.reloadStatus())
}

// mgmtReload re-reads config.yaml and models.yaml now; an invalid file is reported (400) and the running
// configuration stays.
func (g *Gateway) mgmtReload(w http.ResponseWriter, r *http.Request) {
	if err := g.Reload(); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "config not reloaded: "+err.Error())
		return
	}
	g.admin(r, "reload configuration")
	writeJSON(w, http.StatusOK, g.reloadStatus())
}

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

	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/keys"
	"github.com/lotto/substrate/internal/runtime"
	"github.com/lotto/substrate/internal/store"
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
	writeJSON(w, http.StatusOK, map[string]any{"stopped": in.ID})
}

// filterFrom reads since/until/model/engine/instance/client/limit query parameters;
// a user key only ever sees its own records.
func filterFrom(r *http.Request) (store.Filter, error) {
	q := r.URL.Query()
	f := store.Filter{Model: q.Get("model"), Engine: q.Get("engine"), Instance: q.Get("instance"), Client: q.Get("client")}
	if p := principalOf(r); p.role != keys.RoleAdmin {
		f.Client = p.name
	}
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

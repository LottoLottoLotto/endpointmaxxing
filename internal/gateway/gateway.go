// Package gateway serves one OpenAI/Anthropic-compatible endpoint in front of
// every engine instance: it routes by the request's model, starts registered
// models on demand, meters every request into the usage database, samples GPU
// and instance memory, and exposes a management API for agents.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lotto/substrate/internal/app"
	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/gpu"
	"github.com/lotto/substrate/internal/meter"
	"github.com/lotto/substrate/internal/runtime"
	"github.com/lotto/substrate/internal/spec"
	"github.com/lotto/substrate/internal/store"
)

// maxBody bounds a buffered request body (images ride inline as base64).
const maxBody = 256 << 20

// Gateway is the HTTP front door.
type Gateway struct {
	App         *app.App
	Autostart   bool
	Swap        bool
	StreamUsage bool
	// APIKey, when set, is required from clients (Bearer or x-api-key).
	APIKey string
	Log    *log.Logger

	startMu  sync.Mutex // serializes start/swap decisions
	activeMu sync.Mutex
	active   map[string]int
	idle     *sync.Cond
	rr       map[string]int

	statusMu sync.Mutex
	status   map[string]cachedStatus

	recMu   sync.RWMutex // guards rec against sends after Close
	rec     chan *store.Request
	recDone chan struct{}
	dropped int64
}

type cachedStatus struct {
	status string
	at     time.Time
}

// statusTTL bounds how stale a cached ready status may be before re-probing.
const statusTTL = 2 * time.Second

// New returns a gateway over a.
func New(a *app.App, logger *log.Logger) *Gateway {
	g := &Gateway{App: a, Autostart: a.Cfg.Gateway.Autostart, Swap: a.Cfg.Gateway.Swap,
		StreamUsage: a.Cfg.Gateway.StreamUsage, Log: logger,
		active: map[string]int{}, rr: map[string]int{}, status: map[string]cachedStatus{}}
	g.idle = sync.NewCond(&g.activeMu)
	if a.Cfg.Logging.Enabled {
		g.rec = make(chan *store.Request, 4096)
		g.recDone = make(chan struct{})
		go g.writer(g.rec)
	}
	return g
}

// writer persists request records off the request path.
func (g *Gateway) writer(rec <-chan *store.Request) {
	defer close(g.recDone)
	for r := range rec {
		db, err := g.App.DB()
		if err == nil {
			err = db.InsertRequest(r)
		}
		if err != nil {
			g.Log.Printf("usage log: request %s: %v", r.ID, err)
		}
	}
}

func (g *Gateway) logRequest(r *store.Request) {
	g.recMu.RLock()
	defer g.recMu.RUnlock()
	if g.rec == nil {
		return
	}
	select {
	case g.rec <- r:
	default:
		g.activeMu.Lock()
		g.dropped++
		n := g.dropped
		g.activeMu.Unlock()
		if n == 1 || n%1000 == 0 {
			g.Log.Printf("usage log: writer behind, %d records dropped", n)
		}
	}
}

// Close flushes pending usage records.
func (g *Gateway) Close() {
	g.recMu.Lock()
	rec := g.rec
	g.rec = nil
	g.recMu.Unlock()
	if rec != nil {
		close(rec)
		<-g.recDone
	}
}

// Run starts the background loops (exit monitor, resource sampler, retention) until ctx ends.
func (g *Gateway) Run(ctx context.Context) {
	go g.every(ctx, 5*time.Second, g.monitor)
	if iv := time.Duration(g.App.Cfg.Logging.SampleInterval); g.App.Cfg.Logging.Enabled && iv > 0 {
		go g.every(ctx, iv, g.sample)
	}
	if days := g.App.Cfg.Logging.RetentionDays; g.App.Cfg.Logging.Enabled && days > 0 {
		go g.every(ctx, 24*time.Hour, func() {
			db, err := g.App.DB()
			if err != nil {
				return
			}
			n, err := db.Prune(time.Now().AddDate(0, 0, -days))
			if err != nil {
				g.Log.Printf("retention: %v", err)
				return
			}
			g.Log.Printf("retention: pruned %v", n)
		})
	}
}

func (g *Gateway) every(ctx context.Context, d time.Duration, fn func()) {
	fn()
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// monitor records engine exits that nobody stopped (crashes, OOM kills).
func (g *Gateway) monitor() {
	ins, _ := runtime.List()
	for _, in := range ins {
		if in.ExitNoted || in.Alive() {
			continue
		}
		in.NoteExit()
		g.setStatus(in.ID, runtime.StatusExited)
		g.Log.Printf("%s (%s) exited unexpectedly; log: %s", in.ID, in.Engine, in.Log)
	}
}

// sample records GPU readings and each live instance's GPU memory and RSS.
func (g *Gateway) sample() {
	now := time.Now().UnixMilli()
	gs, err := gpu.List()
	if err != nil {
		g.Log.Printf("sampler: %v", err)
	}
	procMem, _ := gpu.ProcessMemory()
	var gss []store.GPUSample
	for _, x := range gs {
		gss = append(gss, store.GPUSample{TS: now, GPU: x.Index, Name: x.Name, MemUsedMiB: x.MemUsedMiB,
			MemTotalMiB: x.MemTotalMiB, UtilPct: x.UtilPct, PowerW: x.PowerW})
	}
	var iss []store.InstanceSample
	ins, _ := runtime.List()
	for _, in := range ins {
		if !in.Alive() {
			continue
		}
		pids := runtime.SessionPIDs(in.PID)
		s := store.InstanceSample{TS: now, Instance: in.ID, Engine: in.Engine, Model: in.Name,
			RSSMiB: runtime.RSSMiB(pids), Processes: int64(len(pids))}
		for _, p := range pids {
			s.GPUMemMiB += procMem[p]
		}
		iss = append(iss, s)
	}
	if len(gss) == 0 && len(iss) == 0 {
		return
	}
	db, err := g.App.DB()
	if err == nil {
		err = db.InsertSamples(gss, iss)
	}
	if err != nil {
		g.Log.Printf("sampler: %v", err)
	}
}

// apiForPath maps request paths to manifest API names.
func apiForPath(p string) (string, bool) {
	for api, path := range spec.APIs {
		if p == path || strings.HasPrefix(p, path+"/") {
			return api, true
		}
	}
	return "", false
}

// Handler returns the gateway's routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/models", g.auth(g.models))
	for _, path := range spec.APIs {
		mux.HandleFunc("POST "+path, g.auth(g.proxy))
		mux.HandleFunc("POST "+path+"/", g.auth(g.proxy))
	}
	mux.HandleFunc("GET /substrate/v1/instances", g.auth(g.mgmtInstances))
	mux.HandleFunc("POST /substrate/v1/ensure", g.auth(g.mgmtEnsure))
	mux.HandleFunc("POST /substrate/v1/stop", g.auth(g.mgmtStop))
	mux.HandleFunc("GET /substrate/v1/usage", g.auth(g.mgmtUsage))
	mux.HandleFunc("GET /substrate/v1/requests", g.auth(g.mgmtRequests))
	mux.HandleFunc("GET /substrate/v1/events", g.auth(g.mgmtEvents))
	return mux
}

func (g *Gateway) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.APIKey != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if r.Header.Get("x-api-key") != "" {
				got = r.Header.Get("x-api-key")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(g.APIKey)) != 1 {
				apiError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
				return
			}
		}
		h(w, r)
	}
}

// instanceStatus probes an instance, caching ready results briefly.
func (g *Gateway) instanceStatus(ctx context.Context, in *runtime.Instance) string {
	g.statusMu.Lock()
	c, ok := g.status[in.ID]
	g.statusMu.Unlock()
	if ok && c.status == runtime.StatusReady && time.Since(c.at) < statusTTL && in.Alive() {
		return c.status
	}
	st := in.Status(ctx)
	g.setStatus(in.ID, st)
	return st
}

func (g *Gateway) setStatus(id, st string) {
	g.statusMu.Lock()
	g.status[id] = cachedStatus{st, time.Now()}
	g.statusMu.Unlock()
}

type modelEntry struct {
	ID        string         `json:"id"`
	Object    string         `json:"object"`
	Created   int64          `json:"created"`
	OwnedBy   string         `json:"owned_by"`
	Substrate map[string]any `json:"substrate"`
}

// models lists running instances and registered models (one entry per name).
func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	byName := map[string]*modelEntry{}
	entry := func(name string, created int64) *modelEntry {
		e := byName[name]
		if e == nil {
			e = &modelEntry{ID: name, Object: "model", Created: created, OwnedBy: "substrate",
				Substrate: map[string]any{"instances": []map[string]any{}}}
			byName[name] = e
		}
		return e
	}
	ins, _ := runtime.List()
	for _, in := range ins {
		if !in.Alive() {
			continue
		}
		e := entry(in.Name, in.StartedAt.Unix())
		e.Substrate["instances"] = append(e.Substrate["instances"].([]map[string]any),
			map[string]any{"id": in.ID, "engine": in.Engine, "status": g.instanceStatus(r.Context(), in), "apis": in.APIs})
	}
	if err := g.App.Refresh(); err != nil {
		g.Log.Printf("models.yaml: %v", err)
	}
	for _, m := range g.App.RegisteredModels() {
		e := entry(m.Name, 0)
		e.Substrate["registered"] = true
		e.Substrate["format"] = m.Format
		if m.Engine != "" {
			e.Substrate["engine"] = m.Engine
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	data := make([]*modelEntry, len(names))
	for i, n := range names {
		data[i] = byName[n]
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func newID() string {
	var b [12]byte
	rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

// injectStreamUsage asks for a final usage chunk on a streamed chat/completions
// request unless the client already decided.
func injectStreamUsage(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, false
	}
	so := map[string]json.RawMessage{}
	if raw, ok := m["stream_options"]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &so) != nil {
			return body, false
		}
		if _, set := so["include_usage"]; set {
			return body, false
		}
	}
	so["include_usage"] = json.RawMessage("true")
	raw, _ := json.Marshal(so)
	m["stream_options"] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	api, _ := apiForPath(r.URL.Path)
	rec := &store.Request{ID: r.Header.Get("X-Request-Id"), TS: start.UnixMilli(), API: api, Path: r.URL.Path,
		UserAgent: r.UserAgent(), Remote: clientIP(r), Client: r.Header.Get("X-Substrate-Client")}
	if rec.ID == "" || len(rec.ID) > 128 {
		rec.ID = newID()
	}
	w.Header().Set("X-Substrate-Request-Id", rec.ID)
	logging := g.App.Cfg.Logging
	fail := func(status int, typ, msg string) {
		apiError(w, status, typ, msg)
		rec.Status, rec.Error = status, msg
		d := time.Since(start).Milliseconds()
		rec.DurationMS = &d
		g.logRequest(rec)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		fail(http.StatusBadRequest, "invalid_request_error", "reading body: "+err.Error())
		return
	}
	rec.ReqBytes = int64(len(body))
	if len(body) > maxBody {
		fail(http.StatusRequestEntityTooLarge, "invalid_request_error", "request body over 256 MiB")
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		User   string `json:"user"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		fail(http.StatusBadRequest, "invalid_request_error", "body is not JSON: "+err.Error())
		return
	}
	rec.Model, rec.Stream = req.Model, req.Stream
	if rec.Client == "" {
		rec.Client = req.User
	}
	if logging.Bodies {
		rec.RequestBody, rec.BodyTruncated = capBytes(body, logging.BodyMaxBytes)
	}

	in, started, status, err := g.pick(r.Context(), req.Model, api)
	queue := time.Since(start).Milliseconds()
	rec.QueueMS, rec.Autostarted = &queue, started
	if err != nil {
		fail(status, errType(status), err.Error())
		return
	}
	rec.Instance, rec.Engine = in.ID, in.Engine
	if rec.Model == "" {
		rec.Model = in.Name
	}
	if g.StreamUsage && req.Stream && (api == "chat" || api == "completions") {
		body, _ = injectStreamUsage(body)
	}

	g.acquire(in.ID)
	defer g.release(in.ID)

	target, _ := url.Parse(in.BaseURL)
	mw := &meteredWriter{ResponseWriter: w, start: start, maxBody: logging.BodyMaxBytes, keepBody: logging.Bodies}
	var proxyErr error
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Body = io.NopCloser(bytes.NewReader(body))
			pr.Out.ContentLength = int64(len(body))
			pr.Out.Header.Del("x-api-key")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Substrate-Client")
			if in.APIKey != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+in.APIKey)
				if api == "messages" {
					pr.Out.Header.Set("x-api-key", in.APIKey)
				}
			}
		},
		FlushInterval: -1, // stream SSE tokens as they arrive
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			proxyErr = err
			g.setStatus(in.ID, runtime.StatusStarting) // re-probe before the next request
			if errors.Is(err, context.Canceled) {
				return
			}
			apiError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("%s (%s): %v", in.ID, in.Engine, err))
		},
	}
	w.Header().Set("X-Substrate-Instance", in.ID)
	w.Header().Set("X-Substrate-Engine", in.Engine)
	rp.ServeHTTP(mw, r)

	u := mw.finish()
	rec.Status = mw.status
	if rec.Status == 0 {
		rec.Status = http.StatusBadGateway
	}
	if proxyErr != nil {
		rec.Error = proxyErr.Error()
		if errors.Is(proxyErr, context.Canceled) {
			rec.Error = "client disconnected"
			rec.Status = 499
		}
	} else if mw.status >= 400 {
		rec.Error = upstreamError(mw.body.Bytes())
	}
	rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens, rec.ReasoningTokens = u.Prompt, u.Completion, u.Cached, u.Reasoning
	rec.UsageSource, rec.FinishReason = u.Source, u.Finish
	if t := mw.ttft(); t != nil {
		ms := t.Milliseconds()
		rec.TTFTMS = &ms
	}
	d := time.Since(start).Milliseconds()
	rec.DurationMS, rec.RespBytes = &d, mw.n
	if logging.Bodies {
		rec.ResponseBody = mw.body.Bytes()
		rec.BodyTruncated = rec.BodyTruncated || mw.truncated
	}
	g.logRequest(rec)
}

func errType(status int) string {
	switch {
	case status == http.StatusNotFound:
		return "not_found_error"
	case status >= 500:
		return "api_error"
	}
	return "invalid_request_error"
}

func capBytes(b []byte, max int) ([]byte, bool) {
	if len(b) <= max {
		return append([]byte(nil), b...), false
	}
	return append([]byte(nil), b[:max]...), true
}

// upstreamError extracts error.message from an engine's error body.
func upstreamError(b []byte) string {
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != nil {
		switch v := e.Error.(type) {
		case string:
			return v
		case map[string]any:
			if m, ok := v["message"].(string); ok {
				return m
			}
		}
	}
	if len(b) > 300 {
		b = b[:300]
	}
	return strings.TrimSpace(string(b))
}

// meteredWriter tees the proxied response into a meter (and optionally a body capture).
type meteredWriter struct {
	http.ResponseWriter
	start     time.Time
	maxBody   int
	keepBody  bool
	status    int
	n         int64
	m         *meter.Meter
	body      bytes.Buffer // captured body (bodies on), or the head of an error body
	truncated bool
}

func (w *meteredWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		stream := strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
		w.m = meter.New(w.start, stream, 32<<20)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *meteredWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	w.m.Write(p[:n])
	limit := w.maxBody
	if !w.keepBody {
		limit = 0
		if w.status >= 400 {
			limit = 4096
		}
	}
	if room := limit - w.body.Len(); room > 0 {
		if n > room {
			w.body.Write(p[:room])
			w.truncated = true
		} else {
			w.body.Write(p[:n])
		}
	} else if n > 0 && w.keepBody {
		w.truncated = true
	}
	return n, err
}

func (w *meteredWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *meteredWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *meteredWriter) finish() meter.Usage {
	if w.m == nil {
		return meter.Usage{}
	}
	return w.m.Finish()
}

func (w *meteredWriter) ttft() *time.Duration {
	if w.m == nil {
		return nil
	}
	return w.m.TTFT()
}

// pick returns a ready instance serving model with api, starting one when allowed.
// started reports an autostart; status is the HTTP status for err.
func (g *Gateway) pick(ctx context.Context, model, api string) (*runtime.Instance, bool, int, error) {
	if in, err := g.ready(ctx, model, api); in != nil || err != nil {
		return in, false, http.StatusBadRequest, err
	}
	if model == "" {
		return nil, false, http.StatusBadRequest, fmt.Errorf("request has no model, and not exactly one instance is ready")
	}
	if !g.Autostart {
		return nil, false, http.StatusNotFound, fmt.Errorf("no ready instance serves %q (autostart is off)", model)
	}
	if err := g.App.Refresh(); err != nil {
		return nil, false, http.StatusInternalServerError, err
	}
	if _, ok := g.App.RegisteredModel(model); !ok {
		return nil, false, http.StatusNotFound, fmt.Errorf("model %q is neither running nor registered (see `substrate model add`)", model)
	}

	g.startMu.Lock()
	defer g.startMu.Unlock()
	// Another request may have started it while this one waited for the lock.
	if in, err := g.ready(ctx, model, api); in != nil || err != nil {
		return in, false, http.StatusBadRequest, err
	}
	// A start outlives the client that triggered it: other requests wait on it.
	in, err := g.start(context.WithoutCancel(ctx), model, api)
	if err != nil {
		status := http.StatusServiceUnavailable
		if c := errs.CodeOf(err); c == errs.Unsupported || c == errs.NotFound {
			status = http.StatusBadRequest
		}
		return nil, false, status, err
	}
	return in, true, http.StatusOK, nil
}

// ready finds a ready instance for model and api; an error means instances
// exist but none serves api.
func (g *Gateway) ready(ctx context.Context, model, api string) (*runtime.Instance, error) {
	ins, _ := runtime.List()
	var ok, wrongAPI []*runtime.Instance
	for _, in := range ins {
		if model != "" && in.Name != model {
			continue
		}
		if g.instanceStatus(ctx, in) != runtime.StatusReady {
			continue
		}
		if !in.HasAPI(api) {
			wrongAPI = append(wrongAPI, in)
			continue
		}
		ok = append(ok, in)
	}
	if model == "" {
		if len(ok) == 1 {
			return ok[0], nil
		}
		return nil, nil
	}
	if len(ok) == 0 {
		if len(wrongAPI) > 0 {
			return nil, errs.New(errs.Unsupported, "%s is served by %s, which does not implement %s", model, wrongAPI[0].Engine, spec.APIs[api])
		}
		return nil, nil
	}
	g.activeMu.Lock()
	i := g.rr[model] % len(ok)
	g.rr[model]++
	g.activeMu.Unlock()
	return ok[i], nil
}

// start launches a registered model (caller holds g.startMu).
func (g *Gateway) start(ctx context.Context, model, api string) (*runtime.Instance, error) {
	t, err := g.App.ResolveModel(model)
	if err != nil {
		return nil, err
	}
	// An instance of this model may be loading already (started by the CLI or an earlier request).
	ins, _ := runtime.List()
	for _, in := range ins {
		if in.Name == model && in.Alive() {
			g.Log.Printf("waiting for %s to become ready", in.ID)
			if err := in.WaitReady(ctx, 0); err != nil {
				return nil, err
			}
			if !in.HasAPI(api) {
				return nil, errs.New(errs.Unsupported, "%s is served by %s, which does not implement %s", model, in.Engine, spec.APIs[api])
			}
			return in, nil
		}
	}
	plan, _, err := g.App.Plan(t, app.LaunchOpts{Owner: "gateway"})
	if err != nil {
		return nil, err
	}
	if !contains(plan.APIs, api) {
		return nil, errs.New(errs.Unsupported, "%s would run on %s, which does not implement %s", model, plan.Engine, spec.APIs[api])
	}
	if g.Swap {
		g.evict(model)
	}
	in, plan, err := g.App.Launch(t, app.LaunchOpts{Owner: "gateway"})
	if err != nil {
		return nil, err
	}
	g.Log.Printf("starting %s on %s: %s", model, plan.Engine, runtime.ShellJoin(runtime.Redact(plan.Argv, in.APIKey)))
	if err := in.AwaitStart(ctx, 0); err != nil {
		return nil, err
	}
	g.setStatus(in.ID, runtime.StatusReady)
	g.Log.Printf("%s ready at %s in %s", in.ID, in.BaseURL, time.Since(in.StartedAt).Round(time.Second))
	return in, nil
}

// evict stops gateway-owned instances of other models once their requests finish.
func (g *Gateway) evict(keep string) {
	ins, _ := runtime.List()
	for _, in := range ins {
		if in.Owner != "gateway" || in.Name == keep {
			continue
		}
		g.waitIdle(in.ID, 10*time.Minute)
		g.Log.Printf("swap: stopping %s", in.ID)
		if err := in.Stop(); err != nil {
			g.Log.Printf("swap: stopping %s: %v", in.ID, err)
		}
		g.setStatus(in.ID, runtime.StatusExited)
	}
}

func (g *Gateway) acquire(id string) {
	g.activeMu.Lock()
	g.active[id]++
	g.activeMu.Unlock()
}

func (g *Gateway) release(id string) {
	g.activeMu.Lock()
	g.active[id]--
	if g.active[id] == 0 {
		delete(g.active, id)
		g.idle.Broadcast()
	}
	g.activeMu.Unlock()
}

func (g *Gateway) waitIdle(id string, max time.Duration) {
	timer := time.AfterFunc(max, func() {
		g.activeMu.Lock()
		g.idle.Broadcast()
		g.activeMu.Unlock()
	})
	defer timer.Stop()
	deadline := time.Now().Add(max)
	g.activeMu.Lock()
	for g.active[id] > 0 && time.Now().Before(deadline) {
		g.idle.Wait()
	}
	g.activeMu.Unlock()
}

// ---- management API ----

func (g *Gateway) mgmtInstances(w http.ResponseWriter, r *http.Request) {
	ins, _ := runtime.List()
	out := make([]map[string]any, 0, len(ins))
	for _, in := range ins {
		g.activeMu.Lock()
		active := g.active[in.ID]
		g.activeMu.Unlock()
		out = append(out, map[string]any{"instance": in.Redacted(), "status": g.instanceStatus(r.Context(), in),
			"active_requests": active, "api_url": in.BaseURL + "/v1"})
	}
	writeJSON(w, http.StatusOK, out)
}

// aliveFor reports whether any live instance serves model.
func (g *Gateway) aliveFor(model string) bool {
	ins, _ := runtime.List()
	for _, in := range ins {
		if in.Name == model && in.Alive() {
			return true
		}
	}
	return false
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
	g.startMu.Lock()
	if g.Swap && !g.aliveFor(req.Model) {
		// Same policy as autostart: free the GPU of the gateway's other models first.
		g.evict(req.Model)
	}
	res, err := g.App.Ensure(context.WithoutCancel(r.Context()), req.Model, app.LaunchOpts{Engine: req.Engine, Owner: "gateway"}, timeout)
	g.startMu.Unlock()
	if err != nil {
		writeErr(w, err)
		return
	}
	g.setStatus(res.Instance.ID, runtime.StatusReady)
	writeJSON(w, http.StatusOK, map[string]any{"instance": res.Instance.Redacted(), "started": res.Started,
		"api_url": res.Instance.BaseURL + "/v1", "status": runtime.StatusReady})
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
	if err := in.Stop(); err != nil {
		writeErr(w, err)
		return
	}
	g.setStatus(in.ID, runtime.StatusExited)
	writeJSON(w, http.StatusOK, map[string]any{"stopped": in.ID})
}

// filterFrom reads since/until/model/engine/instance/client/limit query parameters.
func filterFrom(r *http.Request) (store.Filter, error) {
	q := r.URL.Query()
	f := store.Filter{Model: q.Get("model"), Engine: q.Get("engine"), Instance: q.Get("instance"), Client: q.Get("client")}
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

func (g *Gateway) withDB(w http.ResponseWriter, fn func(*store.DB, store.Filter) (any, error), r *http.Request) {
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
	g.withDB(w, func(db *store.DB, f store.Filter) (any, error) {
		rows, err := db.Usage(r.URL.Query().Get("by"), f)
		if err != nil {
			return nil, errs.Wrap(errs.Usage, err)
		}
		return rows, nil
	}, r)
}

func (g *Gateway) mgmtRequests(w http.ResponseWriter, r *http.Request) {
	g.withDB(w, func(db *store.DB, f store.Filter) (any, error) {
		if f.Limit == 0 {
			f.Limit = 100
		}
		return db.Requests(f)
	}, r)
}

func (g *Gateway) mgmtEvents(w http.ResponseWriter, r *http.Request) {
	g.withDB(w, func(db *store.DB, f store.Filter) (any, error) {
		if f.Limit == 0 {
			f.Limit = 100
		}
		return db.Events(f)
	}, r)
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

var codeStatus = map[errs.Code]int{
	errs.Usage: 400, errs.NotFound: 404, errs.Conflict: 409, errs.Unsupported: 400,
	errs.Timeout: 504, errs.EngineFailed: 502, errs.Internal: 500,
}

func writeErr(w http.ResponseWriter, err error) {
	c := errs.CodeOf(err)
	writeJSON(w, codeStatus[c], map[string]any{"error": map[string]any{"type": string(c), "message": err.Error()}})
}

// apiError answers in OpenAI's error shape, which Anthropic clients also parse (error.type, error.message).
func apiError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": typ, "message": msg}})
}

// Package gateway serves one OpenAI/Anthropic-compatible endpoint in front of
// every engine instance. It routes by the request's model across engines,
// places and starts models on demand within GPU memory, limits concurrency,
// fails over between replicas, authenticates per-client keys with quotas,
// meters every request into the usage database, and exports Prometheus metrics.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lotto/substrate/internal/app"
	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/meter"
	"github.com/lotto/substrate/internal/runtime"
	"github.com/lotto/substrate/internal/spec"
	"github.com/lotto/substrate/internal/store"
)

// maxBody bounds a buffered request body (images ride inline as base64).
const maxBody = 256 << 20

// Gateway is the HTTP front door.
type Gateway struct {
	App       *app.App
	Cfg       config.Gateway
	Autostart bool
	// APIKey is the master (admin) key; per-client keys live in keys.yaml.
	APIKey string
	Log    *log.Logger

	transport *http.Transport

	locksMu    sync.Mutex
	startLocks map[string]*sync.Mutex // per model: one start at a time, other models proceed

	activeMu sync.Mutex
	active   map[string]int       // in-flight requests per instance
	lastUsed map[string]time.Time // per instance
	idle     *sync.Cond
	rr       map[string]int
	// waiting counts requests per model waiting for it to be (swapped in and) started.
	waiting map[string]int
	// swapQueue orders models waiting for devices other models hold.
	swapQueue []string

	statusMu sync.Mutex
	status   map[string]*instState

	lim      limiters
	auth     authState
	metrics  *metrics
	restarts restartBudget

	recMu   sync.RWMutex // guards rec against sends after Close
	rec     chan *store.Request
	recDone chan struct{}
	dropped int64
}

// instState is the gateway's view of one instance's health.
type instState struct {
	status string
	at     time.Time
	fails  int // consecutive failed health probes
}

// statusTTL bounds how stale a cached ready status may be before re-probing.
const statusTTL = 2 * time.Second

// statusUnhealthy marks a live instance that failed its health checks: never routed to.
const statusUnhealthy = "unhealthy"

// New returns a gateway over a, configured from a.Cfg.Gateway.
func New(a *app.App, logger *log.Logger) *Gateway {
	c := a.Cfg.Gateway
	g := &Gateway{App: a, Cfg: c, Autostart: c.Autostart, Log: logger,
		startLocks: map[string]*sync.Mutex{}, active: map[string]int{}, lastUsed: map[string]time.Time{},
		rr: map[string]int{}, waiting: map[string]int{}, status: map[string]*instState{}, metrics: newMetrics()}
	g.idle = sync.NewCond(&g.activeMu)
	g.lim.byModel = map[string]*limiter{}
	if a.Cfg.Logging.Enabled {
		g.auth.dbFunc = a.DB
	}
	g.transport = &http.Transport{
		Proxy:                 nil, // engines are local; never route through HTTP_PROXY
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: time.Duration(c.UpstreamHeaderTimeout),
	}
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
	g.metrics.observe(r)
	g.auth.charge(r)
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

// Handler returns the gateway's routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /metrics", g.authed(roleMetrics, g.serveMetrics))
	mux.HandleFunc("GET /v1/models", g.authed(roleAny, g.models))
	for _, path := range spec.APIs {
		mux.HandleFunc("POST "+path, g.authed(roleAny, g.proxy))
		mux.HandleFunc("POST "+path+"/", g.authed(roleAny, g.proxy))
	}
	mux.HandleFunc("GET /substrate/v1/instances", g.authed(roleAdmin, g.mgmtInstances))
	mux.HandleFunc("POST /substrate/v1/ensure", g.authed(roleAdmin, g.mgmtEnsure))
	mux.HandleFunc("POST /substrate/v1/stop", g.authed(roleAdmin, g.mgmtStop))
	mux.HandleFunc("GET /substrate/v1/usage", g.authed(roleAny, g.mgmtUsage))
	mux.HandleFunc("GET /substrate/v1/requests", g.authed(roleAny, g.mgmtRequests))
	mux.HandleFunc("GET /substrate/v1/events", g.authed(roleAdmin, g.mgmtEvents))
	return mux
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

// instanceStatus probes an instance, caching ready results briefly; unhealthy sticks until the health loop clears it.
func (g *Gateway) instanceStatus(ctx context.Context, in *runtime.Instance) string {
	g.statusMu.Lock()
	s := g.status[in.ID]
	if s != nil && s.status == statusUnhealthy {
		g.statusMu.Unlock()
		return statusUnhealthy
	}
	fresh := s != nil && s.status == runtime.StatusReady && time.Since(s.at) < statusTTL
	g.statusMu.Unlock()
	if fresh && in.Alive() {
		return runtime.StatusReady
	}
	st := in.Status(ctx)
	g.setStatus(in.ID, st)
	return st
}

func (g *Gateway) setStatus(id, st string) {
	g.statusMu.Lock()
	s := g.status[id]
	if s == nil {
		s = &instState{}
		g.status[id] = s
	}
	s.status, s.at = st, time.Now()
	if st == runtime.StatusReady {
		s.fails = 0
	}
	g.statusMu.Unlock()
}

type modelEntry struct {
	ID        string         `json:"id"`
	Object    string         `json:"object"`
	Created   int64          `json:"created"`
	OwnedBy   string         `json:"owned_by"`
	Substrate map[string]any `json:"substrate"`
}

// models lists running instances and registered models (one entry per name) the caller may use.
func (g *Gateway) models(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
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
		names := []string{in.Name}
		if in.Hosting { // a multi-model host is listed under the models it holds
			names = g.hosted(r.Context(), in)
		}
		for _, n := range names {
			if !p.allows(n) {
				continue
			}
			e := entry(n, in.StartedAt.Unix())
			e.Substrate["instances"] = append(e.Substrate["instances"].([]map[string]any),
				map[string]any{"id": in.ID, "engine": in.Engine, "status": g.instanceStatus(r.Context(), in), "apis": in.APIs, "gpus": in.GPUs})
		}
	}
	if err := g.App.Refresh(); err != nil {
		g.Log.Printf("refresh: %v", err)
	}
	for _, m := range g.App.RegisteredModels() {
		if !p.allows(m.Name) {
			continue
		}
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

// maxAttempts bounds failover: the first replica plus one retry on another.
const maxAttempts = 2

func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	api, _ := apiForPath(r.URL.Path)
	p := principalOf(r)
	rec := &store.Request{ID: r.Header.Get("X-Request-Id"), TS: start.UnixMilli(), API: api, Path: r.URL.Path,
		UserAgent: r.UserAgent(), Remote: clientIP(r), Client: p.client(r)}
	if rec.ID == "" || len(rec.ID) > 128 {
		rec.ID = newID()
	}
	w.Header().Set("X-Substrate-Request-Id", rec.ID)
	logging := g.App.Cfg.Logging
	fail := func(status int, err error) {
		apiError(w, status, errType(status, err), err.Error())
		rec.Status, rec.Error = status, err.Error()
		d := time.Since(start).Milliseconds()
		rec.DurationMS = &d
		g.logRequest(rec)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		fail(http.StatusBadRequest, fmt.Errorf("reading body: %w", err))
		return
	}
	rec.ReqBytes = int64(len(body))
	if len(body) > maxBody {
		fail(http.StatusRequestEntityTooLarge, errors.New("request body over 256 MiB"))
		return
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		User   string `json:"user"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		fail(http.StatusBadRequest, fmt.Errorf("body is not JSON: %w", err))
		return
	}
	rec.Model, rec.Stream = req.Model, req.Stream
	if rec.Client == "" {
		rec.Client = req.User
	}
	if logging.Bodies {
		rec.RequestBody, rec.BodyTruncated = capBytes(body, logging.BodyMaxBytes)
	}
	if req.Model != "" && !p.allows(req.Model) {
		fail(http.StatusForbidden, errs.New(errs.Forbidden, "key %s may not use model %q", p.name, req.Model))
		return
	}
	if err := g.auth.admit(p); err != nil {
		w.Header().Set("Retry-After", "60")
		fail(http.StatusTooManyRequests, err)
		return
	}

	cands, started, err := g.pick(r.Context(), req.Model, api)
	rec.Autostarted = started
	if err != nil {
		if c := errs.CodeOf(err); c == errs.Timeout || c == errs.Capacity {
			if s := g.loadEstimate(req.Model); s > 0 {
				w.Header().Set("Retry-After", fmt.Sprint(s))
			}
		}
		fail(statusFor(err), err)
		return
	}
	if rec.Model == "" {
		rec.Model = cands[0].Name
	}
	release, err := g.lim.acquire(r.Context(), rec.Model, g.maxConcurrency(rec.Model), g.Cfg.MaxQueue, time.Duration(g.Cfg.QueueTimeout))
	queue := time.Since(start).Milliseconds()
	rec.QueueMS = &queue
	if err != nil {
		if errs.CodeOf(err) == errs.Capacity {
			w.Header().Set("Retry-After", "5")
		}
		fail(statusFor(err), err)
		return
	}
	defer release()
	if g.Cfg.StreamUsage && req.Stream && (api == "chat" || api == "completions") {
		body, _ = injectStreamUsage(body)
	}

	mw := &meteredWriter{ResponseWriter: w, start: start, maxBody: logging.BodyMaxBytes, keepBody: logging.Bodies}
	var lastErr error
	var in *runtime.Instance
	// Logging runs even when the client aborts mid-stream (ReverseProxy panics with ErrAbortHandler).
	defer func() {
		aborted := recover()
		g.finish(rec, mw, in, lastErr, aborted != nil, start)
		if aborted != nil {
			panic(aborted)
		}
	}()
	for attempt := 0; attempt < maxAttempts && attempt < len(cands); attempt++ {
		in = cands[attempt]
		lastErr = g.forward(mw, r, in, api, body)
		if lastErr == nil || mw.status != 0 || r.Context().Err() != nil {
			break
		}
		// Nothing reached the client: mark the replica suspect and try the next one.
		g.Log.Printf("%s: %v; failing over", in.ID, lastErr)
		g.setStatus(in.ID, runtime.StatusStarting)
		g.metrics.failover(in)
	}
	if lastErr != nil && mw.status == 0 && r.Context().Err() == nil {
		apiError(mw, http.StatusBadGateway, "api_error", fmt.Sprintf("%s (%s): %v", in.ID, in.Engine, lastErr))
	}
}

// forward proxies one attempt to in; it writes nothing to the client when the
// engine could not be reached, so the caller may fail over.
func (g *Gateway) forward(mw *meteredWriter, r *http.Request, in *runtime.Instance, api string, body []byte) error {
	g.acquire(in.ID)
	defer g.release(in.ID)
	target, _ := url.Parse(in.BaseURL)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// The idle timer starts with the first byte: prefill is bounded by the header timeout instead.
	idleTimeout := time.Duration(g.Cfg.UpstreamIdleTimeout)
	var idle *time.Timer
	var idledOut atomic.Bool
	mw.onWrite = func() {
		if idleTimeout <= 0 {
			return
		}
		if idle == nil {
			idle = time.AfterFunc(idleTimeout, func() { idledOut.Store(true); cancel() })
		} else {
			idle.Reset(idleTimeout)
		}
	}
	defer func() {
		if idle != nil {
			idle.Stop()
		}
	}()

	var proxyErr error
	rp := &httputil.ReverseProxy{
		Transport: g.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Body = io.NopCloser(bytes.NewReader(body))
			pr.Out.ContentLength = int64(len(body))
			pr.Out.Header.Del("x-api-key")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Substrate-Client")
			// The body parsed as JSON; strict engines (vLLM) reject other declared types.
			pr.Out.Header.Set("Content-Type", "application/json")
			if in.APIKey != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+in.APIKey)
				if api == "messages" {
					pr.Out.Header.Set("x-api-key", in.APIKey)
				}
			}
		},
		FlushInterval: -1, // stream SSE tokens as they arrive
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			proxyErr = err // the caller decides: fail over, or answer 502
		},
		ModifyResponse: func(resp *http.Response) error {
			mw.Header().Set("X-Substrate-Instance", in.ID)
			mw.Header().Set("X-Substrate-Engine", in.Engine)
			return nil
		},
	}
	rp.ServeHTTP(mw, r.WithContext(ctx))
	if idledOut.Load() {
		return fmt.Errorf("engine sent nothing for %s (upstream_idle_timeout)", idleTimeout)
	}
	return proxyErr
}

// finish records one proxied request.
func (g *Gateway) finish(rec *store.Request, mw *meteredWriter, in *runtime.Instance, proxyErr error, aborted bool, start time.Time) {
	if in != nil {
		rec.Instance, rec.Engine = in.ID, in.Engine
	}
	u := mw.finish()
	rec.Status = mw.status
	switch {
	case aborted || (proxyErr != nil && errors.Is(proxyErr, context.Canceled)):
		rec.Error, rec.Status = "client disconnected", 499
	case proxyErr != nil:
		rec.Error = proxyErr.Error()
		if rec.Status == 0 || rec.Status < 400 {
			rec.Status = http.StatusBadGateway
		}
	case mw.status >= 400:
		rec.Error = upstreamError(mw.body.Bytes())
	}
	if rec.Status == 0 {
		rec.Status = http.StatusBadGateway
	}
	rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens, rec.ReasoningTokens = u.Prompt, u.Completion, u.Cached, u.Reasoning
	rec.UsageSource, rec.FinishReason = u.Source, u.Finish
	if t := mw.ttft(); t != nil {
		ms := t.Milliseconds()
		rec.TTFTMS = &ms
	}
	d := time.Since(start).Milliseconds()
	rec.DurationMS, rec.RespBytes = &d, mw.n
	if g.App.Cfg.Logging.Bodies {
		rec.ResponseBody = mw.body.Bytes()
		rec.BodyTruncated = rec.BodyTruncated || mw.truncated
	}
	g.logRequest(rec)
}

func statusFor(err error) int {
	if errors.Is(err, errQueueFull) || errors.Is(err, errQuota) {
		return http.StatusTooManyRequests
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	return codeStatus[errs.CodeOf(err)]
}

func errType(status int, err error) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
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
	onWrite   func()
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
	if w.onWrite != nil {
		w.onWrite()
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
	errs.Timeout: 504, errs.EngineFailed: 502, errs.Internal: 500, errs.Capacity: 503, errs.Forbidden: 403,
}

func writeErr(w http.ResponseWriter, err error) {
	c := errs.CodeOf(err)
	writeJSON(w, codeStatus[c], map[string]any{"error": map[string]any{"type": string(c), "message": err.Error()}})
}

// apiError answers in OpenAI's error shape, which Anthropic clients also parse (error.type, error.message).
func apiError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"type": typ, "message": msg}})
}

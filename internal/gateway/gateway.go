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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/app"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/mesh"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/meter"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/runtime"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/spec"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
)

// maxBody is the hard upper bound on a buffered request body (images ride inline as base64);
// gateway.max_body_mb sets the limit in force.
const maxBody = config.MaxBodyMBLimit << 20

// Gateway is the HTTP front door.
type Gateway struct {
	App *app.App
	// NoAutostart overrides gateway.autostart (serve --no-autostart).
	NoAutostart bool
	// APIKey is the master (admin) key; per-client keys live in keys.yaml.
	APIKey string
	// AllowOpen permits open mode: with no master key and no keys, every caller is admin. serve decides it
	// once at start (a loopback listen address, or --insecure); without it a gateway whose last key was
	// removed answers 401 instead of opening up.
	AllowOpen bool
	// Insecure (serve --insecure) lets open-mode requests be addressed to a non-loopback host name.
	Insecure bool
	// Version is this emx's version (told to mesh peers).
	Version string
	Log     *log.Logger

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
	reload   reloadState
	buffered byteBudget  // request bodies in memory, all in-flight requests together
	reports  sync.Map    // instance id/start -> what its engine reported about the model (engineReport)
	denials  denialLimit // stored 401 records this minute

	mesh meshState
	iso  cacheIsolation
	// sampleErr is the last GPU listing error the sampler logged (only the sampler goroutine uses it).
	sampleErr string

	expiryMu      sync.Mutex
	expiryAlerted map[string]string // key -> day of its last key_expiring alert

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

// New returns a gateway over a, configured from a.Config(); Reload and the config watcher keep it current.
func New(a *app.App, logger *log.Logger) *Gateway {
	c := a.Config().Gateway
	g := &Gateway{App: a, Log: logger,
		startLocks: map[string]*sync.Mutex{}, active: map[string]int{}, lastUsed: map[string]time.Time{},
		rr: map[string]int{}, waiting: map[string]int{}, status: map[string]*instState{}, metrics: newMetrics(),
		expiryAlerted: map[string]string{}}
	g.idle = sync.NewCond(&g.activeMu)
	g.live()
	g.lim.byModel = map[string]*limiter{}
	if a.Config().Logging.Enabled {
		g.auth.dbFunc = a.DB
	}
	g.auth.peers = g.peerUsage
	g.transport = &http.Transport{
		Proxy:                 nil, // engines are local; never route through HTTP_PROXY
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: time.Duration(c.UpstreamHeaderTimeout),
	}
	if a.Config().Logging.Enabled {
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
	cfg := g.App.Config()
	if r.Cost == nil {
		if c, ok := cfg.Pricing.Cost(r.Model, r.PromptTokens, r.CompletionTokens, r.CachedTokens); ok {
			r.Cost = &c
		}
	}
	g.metrics.observe(r)
	var team *config.Team
	if r.Team != "" {
		team, _ = cfg.Team(r.Team)
	}
	if r.MeshFrom == "" { // a request forwarded here counts against its limits on the machine it entered
		g.raiseBudgetAlerts(g.auth.charge(r, team), cfg.Pricing.CurrencyOrDefault())
	}
	auditRequest(g.live().audit, r)
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
	g.live().audit.Close()
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
		mux.HandleFunc("POST "+path, g.authed(roleInfer, g.proxy))
		mux.HandleFunc("POST "+path+"/", g.authed(roleInfer, g.proxy))
	}
	mux.HandleFunc("GET /emx/v1/instances", g.authed(roleRead, g.mgmtInstances))
	mux.HandleFunc("POST /emx/v1/ensure", g.authed(roleAdmin, g.mgmtEnsure))
	mux.HandleFunc("POST /emx/v1/stop", g.authed(roleAdmin, g.mgmtStop))
	mux.HandleFunc("GET /emx/v1/usage", g.authed(roleAny, g.mgmtUsage))
	mux.HandleFunc("GET /emx/v1/requests", g.authed(roleAny, g.mgmtRequests))
	mux.HandleFunc("GET /emx/v1/events", g.authed(roleRead, g.mgmtEvents))
	mux.HandleFunc("GET /emx/v1/flags", g.authed(roleRead, g.mgmtFlags))
	mux.HandleFunc("GET /emx/v1/errors", g.authed(roleRead, g.mgmtErrors))
	mux.HandleFunc("GET /emx/v1/report", g.authed(roleRead, g.mgmtReport))
	mux.HandleFunc("GET /emx/v1/keys", g.authed(roleRead, g.mgmtKeys))
	mux.HandleFunc("GET /emx/v1/teams", g.authed(roleRead, g.mgmtTeams))
	mux.HandleFunc("POST /emx/v1/policy/test", g.authed(roleRead, g.mgmtPolicyTest))
	mux.HandleFunc("GET /emx/v1/reload", g.authed(roleRead, g.mgmtReloadStatus))
	mux.HandleFunc("POST /emx/v1/reload", g.authed(roleAdmin, g.mgmtReload))
	mux.HandleFunc("GET /emx/v1/whoami", g.authed(roleAny, g.whoami))
	mux.HandleFunc("GET /emx/v1/mesh", g.authed(roleRead, g.meshStatus))
	mux.HandleFunc("GET /emx/v1/mesh/state", g.meshOnly(g.meshStateOf))
	mux.HandleFunc("GET /emx/v1/mesh/shared", g.meshOnly(g.meshShared))
	mux.HandleFunc("POST /emx/v1/mesh/sync", g.meshOnly(g.meshSyncNow))
	g.settingsRoutes(mux)
	mux.Handle("GET /admin/", uiHandler())
	mux.Handle("GET /admin", http.RedirectHandler("/admin/", http.StatusMovedPermanently))
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

func newID() string {
	var b [12]byte
	rand.Read(b[:])
	return "req_" + hex.EncodeToString(b[:])
}

// forceStreamUsage asks for a final usage chunk on a streamed chat/completions request, whatever the client
// asked for: without it streamed tokens are not metered, and quotas and budgets would not count them.
func forceStreamUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	so := map[string]json.RawMessage{}
	if json.Unmarshal(m["stream_options"], &so) != nil || so == nil { // anything but an object is replaced
		so = map[string]json.RawMessage{}
	}
	if string(so["include_usage"]) == "true" {
		return body
	}
	so["include_usage"] = json.RawMessage("true")
	raw, _ := json.Marshal(so)
	m["stream_options"] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
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

// errUpstreamIdle marks a response aborted by upstream_idle_timeout.
var errUpstreamIdle = errors.New("upstream_idle_timeout")

func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	api, _ := apiForPath(r.URL.Path)
	p := principalOf(r)
	// The record's id is the gateway's own; a client's X-Request-Id (which it may repeat) is kept beside it.
	rec := &store.Request{ID: newID(), ClientRequestID: clip(r.Header.Get("X-Request-Id"), 128), TS: start.UnixMilli(), API: api,
		Path: clip(r.URL.Path, maxStoredField), UserAgent: clip(r.UserAgent(), maxStoredField), Remote: clientIP(r),
		Client: clip(p.client(r), maxStoredField)}
	rec.SubClient = p.sub
	w.Header().Set("X-Emx-Request-Id", rec.ID)
	w.Header().Set("X-Request-Id", rec.ID)
	lv := g.live() // this request keeps one config snapshot from here on
	logging := lv.cfg.Logging
	if p.key != nil {
		rec.Team = p.key.Team
	}
	if p.call != nil { // forwarded by another machine of the mesh, which checked the key and its limits
		rec.Team, rec.MeshFrom = clip(p.call.Team, maxStoredField), p.call.From
	}
	fail := func(status int, err error) {
		apiError(w, status, errType(status, err), err.Error())
		rec.Status, rec.Error, rec.ErrorKind = status, clip(err.Error(), maxStoredError), gatewayErrorKind(status, err)
		d := time.Since(start).Milliseconds()
		rec.DurationMS = &d
		g.logRequest(rec)
	}

	// What needs no body comes first: a key over its limits never makes the gateway buffer one.
	q, wait, err := g.auth.admit(p)
	if err != nil {
		setRateLimitHeaders(w.Header(), q)
		w.Header().Set("Retry-After", retryAfterSeconds(wait))
		fail(http.StatusTooManyRequests, err)
		return
	}
	team := p.team(lv.cfg)
	if wait, err := g.auth.admitTeam(team, lv.cfg.Pricing.CurrencyOrDefault()); err != nil {
		w.Header().Set("Retry-After", retryAfterSeconds(wait))
		fail(http.StatusTooManyRequests, err)
		return
	}
	g.auth.teamTokenQuota(team, &q)
	setRateLimitHeaders(w.Header(), q)

	// A client that declares a body and then stalls would hold its share of the buffer budget: bound the read.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	body, held, err := g.readBody(r, p.name, bodyLimit(&lv.cfg.Gateway), int64(lv.cfg.Gateway.MaxBufferedMB)<<20)
	_ = rc.SetReadDeadline(time.Time{})
	defer g.buffered.release(p.name, held) // the body stays in memory until the request is done (failover resends it)
	rec.ReqBytes = int64(len(body))
	var tooLarge errBodyTooLarge
	switch {
	case errors.As(err, &tooLarge):
		fail(http.StatusRequestEntityTooLarge, err)
		return
	case errors.Is(err, errBufferFull):
		w.Header().Set("Retry-After", "1")
		fail(http.StatusServiceUnavailable, err)
		return
	case err != nil:
		fail(http.StatusBadRequest, fmt.Errorf("reading body: %w", err))
		return
	case p.call != nil && !p.call.BodyMatches(body):
		fail(http.StatusUnauthorized, errs.New(errs.Forbidden, "mesh call: the body is not the one signed"))
		return
	}
	req, err := parseRequest(body)
	if err != nil {
		fail(http.StatusBadRequest, fmt.Errorf("body is not a JSON request: %w", err))
		return
	}
	if target, ok := lv.cfg.Alias(req.Model); ok {
		w.Header().Set("X-Emx-Alias", req.Model)
		body = setJSONField(body, "model", target)
		req.Model = target
	}
	if req.Model == "" {
		// Model access and policy apply to the model the request reaches: name it before checking them.
		if req.Model, err = g.soleModel(r.Context(), api); err != nil {
			fail(statusFor(err), err)
			return
		}
		body = setJSONField(body, "model", req.Model)
	}
	rec.Model, rec.Stream = req.Model, req.Stream // policy rules are scoped by the name as sent
	if rec.Client == "" {
		rec.Client = clip(req.User, maxStoredField)
	}
	blocked := lv.inspect.Inspect(rec, body)
	rec.Model = modelLabel(req.Model) // until the request is routed the name is only the client's word
	keepBodies := logging.Bodies || (logging.FlaggedBodies && len(rec.Flags) > 0)
	if keepBodies {
		rec.RequestBody, rec.BodyTruncated = capBytes(body, logging.BodyMaxBytes)
	}
	if !p.allows(req.Model, lv.cfg) {
		fail(http.StatusForbidden, errs.New(errs.Forbidden, "key %s may not use model %q", p.name, clip(req.Model, 128)))
		return
	}
	if blocked {
		fail(http.StatusBadRequest, policyBlocked(rec.Flags))
		return
	}
	var cands []*runtime.Instance
	var started bool
	if pin := r.Header.Get("X-Emx-Node"); pin != "" && p.call == nil && pin != g.meshCfg().Node {
		// The client names the machine: no routing decision, no failover.
		if _, ok := g.meshCfg().Peer(pin); !ok {
			fail(http.StatusNotFound, errs.New(errs.NotFound, "X-Emx-Node: no mesh peer called %q", clip(pin, 64)))
			return
		}
		cands = []*runtime.Instance{g.peerInstance(pin, req.Model)}
	} else if p.call == nil { // a forwarded request is served here, never sent on
		cands = g.meshTargets(r.Context(), req.Model, api)
	}
	forwarded := len(cands) > 0
	if !forwarded {
		cands, started, err = g.pick(r.Context(), req.Model, api)
	}
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
	rec.Model = req.Model // routed: the name of a model that exists
	g.metrics.markRouted(rec.Model)
	release := func() {}
	if !forwarded { // the machine that serves it applies its own concurrency limit
		release, err = g.lim.acquire(r.Context(), rec.Model, g.maxConcurrency(rec.Model), g.gw().MaxQueue, time.Duration(g.gw().QueueTimeout))
	}
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
	if g.gw().StreamUsage && req.Stream && (api == "chat" || api == "completions") {
		body = forceStreamUsage(body)
	}
	keyCap := 0
	if p.key != nil {
		keyCap = p.key.MaxOutputTokens
	}
	if limit := outputCap(lv.cfg.Gateway.MaxOutputTokens, keyCap); limit > 0 {
		var capped bool
		if body, capped = capOutputTokens(body, api, limit); capped {
			w.Header().Set("X-Emx-Max-Output-Tokens", fmt.Sprint(limit))
		}
	}

	mw := &meteredWriter{ResponseWriter: w, start: start, maxBody: logging.BodyMaxBytes, keepBody: keepBodies}
	// Served here and isolated: what this client's own prompts explain of the engine's prompt cache.
	var prompt promptFP
	if lv.cfg.Gateway.CacheIsolation && !forwarded {
		if pb := promptBytes(api, body); pb != nil {
			prompt = fingerprintOf(pb)
			own, covered := g.iso.ownCached(rec.Model, cacheDomain(p, rec), prompt)
			// A prefix enough clients sent counts as everyone's own.
			if t, c := g.iso.sharedPrefix(rec.Model, prompt, lv.cfg.Gateway.CacheShareAfter); c > covered {
				own, covered = max(own, t), c
			}
			mw.iso = &isolatedWrite{own: own, covered: covered, ctx: r.Context()}
		}
	}
	var lastErr error
	var in *runtime.Instance
	// Logging runs even when the client aborts mid-stream (ReverseProxy panics with ErrAbortHandler).
	defer func() {
		aborted := recover()
		if aborted == nil {
			mw.flushIsolated()
		}
		g.finish(rec, mw, in, lastErr, aborted != nil, start)
		if mw.iso != nil && rec.PromptTokens != nil {
			g.iso.remember(rec.Model, cacheDomain(p, rec), prompt, *rec.PromptTokens)
			g.iso.seenBy(rec.Model, cacheDomain(p, rec), prompt, lv.cfg.Gateway.CacheShareAfter)
			if t := mw.ttft(); t != nil {
				reported := int64(0)
				if rec.CachedReported != nil {
					reported = *rec.CachedReported
				} else if rec.CachedTokens != nil {
					reported = *rec.CachedTokens
				}
				g.iso.learn(rec.Model, len(prompt.b), *rec.PromptTokens, reported, mw.iso.own, *t)
			}
		}
		if aborted != nil {
			panic(aborted)
		}
	}()
	for attempt := 0; attempt < maxAttempts && attempt < len(cands); attempt++ {
		in = cands[attempt]
		var call *meshCall
		if in.Engine == meshEngine {
			// Another machine may take over when this one answers 5xx (it could not start the model, say),
			// unless it is the last one there is.
			last := attempt == maxAttempts-1 || attempt == len(cands)-1
			call = &meshCall{peer: in.Owner, client: rec.Client, team: rec.Team, sub: rec.SubClient, domain: cacheDomain(p, rec), failover: !last}
		}
		if mw.iso != nil && mw.status == 0 { // held from when the engine gets the request
			if d := g.iso.holdFor(rec.Model, len(prompt.b)-mw.iso.covered); d > 0 {
				mw.iso.hold = time.Now().Add(d)
			}
		}
		lastErr = g.forward(mw, r, in, api, body, rec.ID, call)
		if lastErr == nil || mw.status != 0 || r.Context().Err() != nil {
			break
		}
		var refused errPeerFailed
		if call != nil && !errors.As(lastErr, &refused) { // a peer that answered is up, just unable this time
			g.peerDown(call.peer, lastErr)
		}
		// Nothing reached the client: mark the replica suspect and try the next one.
		g.Log.Printf("%s: %v; failing over", in.ID, lastErr)
		g.setStatus(in.ID, runtime.StatusStarting)
		g.metrics.failover(in)
	}
	if lastErr != nil && mw.status == 0 && r.Context().Err() == nil {
		what := fmt.Sprintf("%s (%s)", in.ID, in.Engine)
		if in.Engine == meshEngine {
			what = "mesh peer " + in.Owner
		}
		apiError(mw, http.StatusBadGateway, "api_error", fmt.Sprintf("%s: %v", what, lastErr))
	}
}

// forward proxies one attempt to in; it writes nothing to the client when the
// engine could not be reached, so the caller may fail over. The engine sees the gateway's request id as
// X-Request-Id, so its logs can be matched to the usage record.
// meshCall is a forward to another machine of the mesh, on behalf of client (of team).
type meshCall struct {
	peer, client, team, sub, domain string
	failover                        bool // a 5xx answer is not passed on: the caller tries the next machine
}

// errPeerFailed is a peer's 5xx answer, not passed to the client because another machine is tried.
type errPeerFailed struct {
	status int
	msg    string
}

func (e errPeerFailed) Error() string { return fmt.Sprintf("answered %d: %s", e.status, e.msg) }

func (g *Gateway) forward(mw *meteredWriter, r *http.Request, in *runtime.Instance, api string, body []byte, reqID string, call *meshCall) error {
	g.acquire(in.ID)
	defer g.release(in.ID)
	target, _ := url.Parse(in.BaseURL)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// The idle timer starts with the first byte: prefill is bounded by the header timeout instead.
	idleTimeout := time.Duration(g.gw().UpstreamIdleTimeout)
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
	transport := http.RoundTripper(g.transport)
	var secret []byte
	if call != nil {
		g.mesh.mu.Lock()
		secret = g.mesh.secret
		if g.mesh.client != nil {
			transport = g.mesh.client.Transport
		}
		g.mesh.mu.Unlock()
	}
	rp := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.Out.Body = io.NopCloser(bytes.NewReader(body))
			pr.Out.ContentLength = int64(len(body))
			pr.Out.Header.Del("x-api-key")
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Emx-Client")
			pr.Out.Header.Del("X-Emx-Node") // the choice of machine is made: the peer serves it
			pr.Out.Header.Set("X-Request-Id", reqID)
			// The body parsed as JSON; strict engines (vLLM) reject other declared types.
			pr.Out.Header.Set("Content-Type", "application/json")
			if in.APIKey != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+in.APIKey)
				if api == "messages" {
					pr.Out.Header.Set("x-api-key", in.APIKey)
				}
			}
			for k := range pr.Out.Header {
				if strings.HasPrefix(k, "X-Emx-Mesh-") {
					pr.Out.Header.Del(k) // only this gateway signs what it sends
				}
			}
			if call != nil {
				mesh.Sign(secret, pr.Out.Header, g.meshCfg().Node, mesh.As{Client: call.client, Team: call.team, Sub: call.sub, Domain: call.domain}, http.MethodPost, pr.Out.URL.RequestURI(), body, time.Now())
			}
		},
		FlushInterval: -1, // stream SSE tokens as they arrive
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			proxyErr = err // the caller decides: fail over, or answer 502
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("X-Request-Id") // the client gets the gateway's id, set already
			if mw.iso != nil && call == nil {
				resp.Header.Del("Content-Length") // cached-token counts may be rewritten
			}
			if call != nil && call.failover && resp.StatusCode >= 500 {
				head, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				return errPeerFailed{resp.StatusCode, upstreamError(head)}
			}
			if call != nil {
				// What the peer served it with, named by machine; its limits and ids are its own business.
				mw.peerInstance = call.peer + "/" + resp.Header.Get("X-Emx-Instance")
				mw.peerEngine = resp.Header.Get("X-Emx-Engine")
				for k := range resp.Header {
					if strings.HasPrefix(k, "X-Emx-") || strings.HasPrefix(k, "X-Ratelimit-") {
						resp.Header.Del(k)
					}
				}
				mw.Header().Set("X-Emx-Node", call.peer)
				mw.Header().Set("X-Emx-Instance", mw.peerInstance)
				mw.Header().Set("X-Emx-Engine", mw.peerEngine)
				return nil
			}
			mw.Header().Set("X-Emx-Instance", in.ID)
			mw.Header().Set("X-Emx-Engine", in.Engine)
			return nil
		},
	}
	rp.ServeHTTP(mw, r.WithContext(ctx))
	if idledOut.Load() {
		return fmt.Errorf("engine sent nothing for %s (%w)", idleTimeout, errUpstreamIdle)
	}
	return proxyErr
}

// finish records one proxied request.
func (g *Gateway) finish(rec *store.Request, mw *meteredWriter, in *runtime.Instance, proxyErr error, aborted bool, start time.Time) {
	if in != nil {
		rec.Instance, rec.Engine = in.ID, in.Engine
		if in.Engine == meshEngine {
			rec.Node, rec.Instance, rec.Engine = in.Owner, mw.peerInstance, mw.peerEngine
			if rec.Instance == "" {
				rec.Instance = in.Owner + "/"
			}
		}
	}
	u := mw.finish()
	rec.Status = mw.status
	switch {
	case aborted || (proxyErr != nil && errors.Is(proxyErr, context.Canceled)):
		rec.Error, rec.Status, rec.ErrorKind = "client disconnected", 499, "client_disconnected"
	case proxyErr != nil:
		rec.Error, rec.ErrorKind = proxyErr.Error(), "engine_unreachable"
		if errors.Is(proxyErr, errUpstreamIdle) {
			rec.ErrorKind = "upstream_timeout"
		}
		if rec.Status == 0 || rec.Status < 400 {
			rec.Status = http.StatusBadGateway
		}
	case mw.status >= 500:
		rec.Error, rec.ErrorKind = upstreamError(mw.body.Bytes()), "engine_error"
	case mw.status >= 400:
		rec.Error, rec.ErrorKind = upstreamError(mw.body.Bytes()), "engine_rejected"
	}
	if rec.Status == 0 {
		rec.Status, rec.ErrorKind = http.StatusBadGateway, "engine_unreachable"
	}
	rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens, rec.ReasoningTokens = u.Prompt, u.Completion, u.Cached, u.Reasoning
	if mw.iso != nil && u.Cached != nil && *u.Cached > mw.iso.own {
		// The client saw (and is billed for) the cache its own prompts left; the engine's count is kept.
		rec.CachedReported, rec.CachedTokens = u.Cached, &mw.iso.own
	}
	rec.UsageSource, rec.FinishReason = u.Source, u.Finish
	if t := mw.ttft(); t != nil {
		ms := t.Milliseconds()
		rec.TTFTMS = &ms
	}
	d := time.Since(start).Milliseconds()
	rec.DurationMS, rec.RespBytes = &d, mw.n
	if mw.keepBody {
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
	// peerInstance and peerEngine: what served a request forwarded to another machine of the mesh.
	peerInstance, peerEngine string
	// iso, under gateway.cache_isolation, holds the answer back and lowers cached-token counts (cacheiso.go).
	iso *isolatedWrite
}

func (w *meteredWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		stream := strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
		w.m = meter.New(w.start, stream, 32<<20)
		if w.iso != nil {
			w.iso.sse = stream
			if code < 400 {
				w.iso.wait() // the headers too: a JSON answer's headers come when it is complete
			}
		}
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
	w.m.Write(p) // the engine's bytes, as they are: usage is metered from them
	out := p
	if iw := w.iso; iw != nil && w.status < 400 {
		iw.pending = append(iw.pending, p...)
		if !iw.sse { // a JSON answer is rewritten whole, when it ends (flushIsolated)
			return len(p), nil
		}
		i := bytes.LastIndexByte(iw.pending, '\n')
		if i < 0 {
			return len(p), nil
		}
		out = capCachedCounts(iw.pending[:i+1], iw.own)
		iw.pending = append(iw.pending[:0], iw.pending[i+1:]...)
	}
	if err := w.send(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// flushIsolated sends what cache isolation still holds when the engine's answer ends.
func (w *meteredWriter) flushIsolated() {
	if iw := w.iso; iw != nil && len(iw.pending) > 0 {
		w.send(capCachedCounts(iw.pending, iw.own))
		iw.pending = nil
	}
}

// send writes to the client and keeps the head of the body when bodies are captured (or it is an error).
func (w *meteredWriter) send(p []byte) error {
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
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
	return err
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

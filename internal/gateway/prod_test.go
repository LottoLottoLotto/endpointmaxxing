package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LottoLottoLotto/substrate/internal/app"
	"github.com/LottoLottoLotto/substrate/internal/keys"
	"github.com/LottoLottoLotto/substrate/internal/paths"
	"github.com/LottoLottoLotto/substrate/internal/runtime"
	"github.com/LottoLottoLotto/substrate/internal/spec"
	"github.com/LottoLottoLotto/substrate/internal/store"
)

// instance records an instance id serving model at url, alive as this test process.
func instance(t *testing.T, id, model, url string) {
	t.Helper()
	now := time.Now()
	in := runtime.Instance{ID: id, Name: model, Engine: "fake", PID: os.Getpid(), StartTick: selfStartTick(t),
		BaseURL: url, HealthURL: url + "/health", APIs: []string{"chat"}, ReadyAt: &now, Owner: "cli"}
	data, _ := json.Marshal(in)
	dir := filepath.Join(paths.State(), "instances")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, id+".json"), data, 0o600)
}

func okBackend() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":5}}`))
	}))
}

func newGateway(t *testing.T) (*Gateway, *httptest.Server) {
	t.Helper()
	a, err := app.Open()
	if err != nil {
		t.Fatal(err)
	}
	g := New(a, log.New(io.Discard, "", 0))
	g.AllowOpen = true // as serve decides for a loopback address
	srv := httptest.NewServer(g.Handler())
	t.Cleanup(func() { srv.Close(); g.Close() })
	return g, srv
}

func call(t *testing.T, method, url, key, body string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// direct serves one request addressed to 127.0.0.1 without a network, so its headers and declared
// length are exactly as given.
func direct(h http.Handler, method, path, key, body string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:7777"+path, strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestKeysRolesAllowlistAndRate(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	backend := okBackend()
	defer backend.Close()
	instance(t, "m.fake", "m", backend.URL)
	instance(t, "other.fake", "other", backend.URL)
	// Every request of a key counts toward its rate, refused ones too: the limit is checked before the body is read.
	user, err := keys.Add(keys.Key{Name: "alice", Role: keys.RoleUser, Models: []string{"m"}, RPM: 3})
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := keys.Add(keys.Key{Name: "ops", Role: keys.RoleAdmin})
	_, srv := newGateway(t)
	chat := srv.URL + "/v1/chat/completions"

	if got := call(t, "POST", chat, "", `{"model":"m"}`); got != 401 {
		t.Fatalf("no key: %d", got)
	}
	if got := call(t, "POST", chat, user, `{"model":"other"}`); got != 403 {
		t.Fatalf("model outside allowlist: %d", got)
	}
	for i := range 2 {
		if got := call(t, "POST", chat, user, `{"model":"m"}`); got != 200 {
			t.Fatalf("request %d within rpm: %d", i, got)
		}
	}
	if got := call(t, "POST", chat, user, `{"model":"m"}`); got != 429 {
		t.Fatalf("over rpm: %d", got)
	}
	if got := call(t, "GET", srv.URL+"/substrate/v1/instances", user, ""); got != 403 {
		t.Fatalf("user on management API: %d", got)
	}
	if got := call(t, "GET", srv.URL+"/substrate/v1/instances", admin, ""); got != 200 {
		t.Fatalf("admin on management API: %d", got)
	}
	if got := call(t, "GET", srv.URL+"/metrics", user, ""); got != 403 {
		t.Fatalf("user on metrics: %d", got)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`substrate_requests_total{model="m",engine="fake",api="chat",status="200"} 2`,
		`substrate_tokens_total{model="m",engine="fake",kind="completion"} 10`,
		`substrate_instance_up{instance="m.fake"`,
	} {
		if !strings.Contains(string(m), want) {
			t.Fatalf("metrics lack %s:\n%s", want, m)
		}
	}
	// Revocation takes effect without a restart.
	if _, err := keys.Remove("alice"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // a distinct mtime on coarse filesystems
	if got := call(t, "POST", chat, user, `{"model":"m"}`); got != 401 {
		t.Fatalf("revoked key: %d", got)
	}
}

func TestFailoverToAnotherReplica(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	good := okBackend()
	defer good.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // connection refused from now on
	instance(t, "m.a", "m", deadURL)
	instance(t, "m.b", "m", good.URL)
	g, srv := newGateway(t)
	// Both look ready: the dead one's status is cached as if it had just passed a probe.
	g.setStatus("m.a", runtime.StatusReady)
	g.setStatus("m.b", runtime.StatusReady)
	for range 4 { // round robin puts the dead replica first at least once
		if got := call(t, "POST", srv.URL+"/v1/chat/completions", "", `{"model":"m"}`); got != 200 {
			t.Fatalf("status %d, want failover to the live replica", got)
		}
	}
}

func TestSwapPolicy(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	backend := okBackend()
	defer backend.Close()
	instance(t, "a.fake", "a", backend.URL)
	g, _ := newGateway(t)
	g.gw().Swap.MinResidency = spec.Duration(30 * time.Second)
	g.gw().Swap.MaxResidency = spec.Duration(10 * time.Minute)
	setReady := func(ago time.Duration) {
		in, _ := runtime.Get("a.fake")
		at := time.Now().Add(-ago)
		in.ReadyAt = &at
		data, _ := json.Marshal(in)
		os.WriteFile(filepath.Join(paths.State(), "instances", "a.fake.json"), data, 0o600)
	}
	setReady(10 * time.Second)
	if g.swappable("a.fake") {
		t.Fatal("swapped out before min_residency")
	}
	setReady(time.Minute)
	if !g.swappable("a.fake") {
		t.Fatal("idle model past min_residency kept its devices")
	}
	g.addWaiting("a", 1) // its own request is waiting
	if g.swappable("a.fake") {
		t.Fatal("swapped out with its own demand before max_residency")
	}
	setReady(11 * time.Minute)
	if !g.swappable("a.fake") {
		t.Fatal("kept its devices past max_residency while others wait")
	}
	g.addWaiting("a", -1)
	g.acquire("a.fake") // a request in flight
	setReady(time.Minute)
	if g.swappable("a.fake") {
		t.Fatal("swapped out with a request in flight")
	}
	g.release("a.fake")
	if !g.swappable("missing.fake") {
		t.Fatal("a vanished instance blocks a swap")
	}
}

func TestConcurrencyLimitQueuesThenRefuses(t *testing.T) {
	var l limiters
	l.byModel = map[string]*limiter{}
	ctx := context.Background()
	rel, err := l.acquire(ctx, "m", 1, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		r, err := l.acquire(ctx, "m", 1, 1, 5*time.Second) // queued
		if err == nil {
			r()
		}
		got <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := l.acquire(ctx, "m", 1, 1, time.Second); !errors.Is(err, errQueueFull) {
		t.Fatalf("third request: %v, want queue full", err)
	}
	rel()
	if err := <-got; err != nil {
		t.Fatalf("queued request: %v", err)
	}
	rel2, _ := l.acquire(ctx, "m", 1, 1, time.Second)
	if _, err := l.acquire(ctx, "m", 1, 5, 20*time.Millisecond); err == nil || errors.Is(err, errQueueFull) {
		t.Fatalf("want a queue timeout, got %v", err)
	}
	rel2()
}

// In open mode every caller is admin, so a web page in the operator's browser must not reach the gateway:
// requests addressed to a non-loopback name (DNS rebinding) or sent by another origin are refused.
func TestOpenModeAnswersOnlyLocalSameOriginRequests(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	g, _ := newGateway(t)
	h := g.Handler()
	whoami := func(host string, headers ...string) int {
		r := httptest.NewRequest("GET", "http://"+host+"/substrate/v1/whoami", nil)
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct {
		name, host string
		headers    []string
		want       int
	}{
		{"loopback address", "127.0.0.1:7777", nil, 200},
		{"localhost", "localhost:7777", nil, 200},
		{"IPv6 loopback", "[::1]:7777", nil, 200},
		{"another 127/8 address", "127.8.8.8:7777", nil, 200},
		{"a rebound name", "evil.example:7777", nil, 403},
		{"same origin", "localhost:7777", []string{"Origin", "http://localhost:7777"}, 200},
		{"another site", "localhost:7777", []string{"Origin", "http://evil.example"}, 403},
		{"another port", "localhost:7777", []string{"Origin", "http://localhost:3000"}, 403},
		{"another scheme", "localhost:7777", []string{"Origin", "https://localhost:7777"}, 403},
		{"opaque origin", "localhost:7777", []string{"Origin", "null"}, 403},
		{"cross-site fetch", "localhost:7777", []string{"Sec-Fetch-Site", "cross-site"}, 403},
		{"same-origin fetch", "localhost:7777", []string{"Sec-Fetch-Site", "same-origin"}, 200},
	} {
		if got := whoami(c.host, c.headers...); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
	// serve --insecure opens the gateway to a network on purpose: any host name, but still no other origin.
	g.Insecure = true
	if got := whoami("gateway.lan:7777"); got != 200 {
		t.Errorf("--insecure, another host name: %d", got)
	}
	if got := whoami("gateway.lan:7777", "Origin", "http://evil.example"); got != 403 {
		t.Errorf("--insecure, another origin: %d", got)
	}
}

// Whether the gateway may run open is decided when it starts: removing the last key of a gateway that
// was not allowed to must lock it, not make every caller admin.
func TestRemovingTheLastKeyDoesNotOpenTheGateway(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	admin, _ := keys.Add(keys.Key{Name: "ops", Role: keys.RoleAdmin})
	a, err := app.Open()
	if err != nil {
		t.Fatal(err)
	}
	g := New(a, log.New(io.Discard, "", 0)) // AllowOpen unset: as serve leaves it for a non-loopback address
	defer g.Close()
	h := g.Handler()
	if w := direct(h, "GET", "/substrate/v1/whoami", admin, ""); w.Code != 200 {
		t.Fatalf("with a key: %d", w.Code)
	}
	if _, err := keys.Remove("ops"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // a distinct mtime on coarse filesystems
	for _, key := range []string{"", admin} {
		if w := direct(h, "GET", "/substrate/v1/whoami", key, ""); w.Code != 401 {
			t.Fatalf("no keys left, key %q: %d %s", key, w.Code, w.Body)
		}
	}
}

// A browser can send text/plain or form bodies to another origin without asking: management calls with a
// body must declare JSON, like settings calls.
func TestManagementBodiesMustBeJSON(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	admin, _ := keys.Add(keys.Key{Name: "ops", Role: keys.RoleAdmin})
	g, _ := newGateway(t)
	h := g.Handler()
	for _, path := range []string{"/substrate/v1/stop", "/substrate/v1/ensure", "/substrate/v1/reload", "/substrate/v1/policy/test"} {
		if w := direct(h, "POST", path, admin, `{"id":"nope"}`, "Content-Type", "text/plain"); w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("POST %s with a text/plain body: %d", path, w.Code)
		}
	}
	if w := direct(h, "POST", "/substrate/v1/stop", admin, `{"id":"nope"}`); w.Code == http.StatusUnsupportedMediaType || w.Code < 400 {
		t.Errorf("stop with a JSON body naming no instance: %d", w.Code)
	}
	if w := direct(h, "POST", "/substrate/v1/reload", admin, ""); w.Code != 200 {
		t.Errorf("reload without a body: %d", w.Code)
	}
}

// A request without a model is routed to the only ready one: the key's allow-list applies to that model,
// and the engine is told which. Bodies an engine could read differently from the gateway are refused.
func TestModelLessRequestsAndAmbiguousBodies(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	b := newRecordingBackend()
	defer b.Close()
	instance(t, "other.fake", "other", b.URL)
	narrow, _ := keys.Add(keys.Key{Name: "alice", Role: keys.RoleUser, Models: []string{"m"}})
	wide, _ := keys.Add(keys.Key{Name: "bob", Role: keys.RoleUser})
	_, srv := newGateway(t)
	chat := srv.URL + "/v1/chat/completions"

	if got := call(t, "POST", chat, narrow, `{"messages":[]}`); got != 403 {
		t.Fatalf("model-less request by a key not allowed the only model: %d", got)
	}
	if got := call(t, "POST", chat, wide, `{"messages":[]}`); got != 200 {
		t.Fatalf("model-less request: %d", got)
	}
	if body, _ := b.last(); body["model"] != "other" {
		t.Fatalf("engine saw model %v, want the resolved one", body["model"])
	}
	for _, body := range []string{
		`{"model":"other","model":"m","messages":[]}`,
		`{"model":"other","Model":"m","messages":[]}`,
		`{"Model":"other","messages":[]}`,
		`{"model":"other","Messages":[]}`,
		`{"model":"other","messages":[],"messages":[]}`,
		`{"model":"other","STREAM":true}`,
		`{"model":"other","temperature":1,"temperature":2}`,
		`{"model":"other"} {"model":"m"}`,
		`["other"]`,
	} {
		if got := call(t, "POST", chat, wide, body); got != 400 {
			t.Errorf("%s: status %d, want 400", body, got)
		}
	}
}

// A key over its rate is refused before its body is read (here: before the body is found to be garbage).
func TestRateLimitIsCheckedBeforeTheBody(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	backend := okBackend()
	defer backend.Close()
	instance(t, "m.fake", "m", backend.URL)
	user, _ := keys.Add(keys.Key{Name: "alice", Role: keys.RoleUser, RPM: 1})
	g, _ := newGateway(t)
	h := g.Handler()
	if w := direct(h, "POST", "/v1/chat/completions", user, chatBody("m")); w.Code != 200 {
		t.Fatalf("within rpm: %d %s", w.Code, w.Body)
	}
	if w := direct(h, "POST", "/v1/chat/completions", user, `not json`); w.Code != 429 {
		t.Fatalf("over rpm with an unreadable body: %d, want 429 before the body matters", w.Code)
	}
}

// gateway.max_body_mb bounds one body (413), and the bodies of all in-flight requests share a memory
// budget: a request that does not fit is told to retry (503), and the budget is returned when one ends.
func TestBodyLimitAndBufferBudget(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	writeConfig(t, "gateway:\n    max_body_mb: 1\n    max_buffered_mb: 8\n")
	backend := okBackend()
	defer backend.Close()
	instance(t, "m.fake", "m", backend.URL)
	g, _ := newGateway(t)
	h := g.Handler()
	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", 1<<20) + `"}]}`

	if w := direct(h, "POST", "/v1/chat/completions", "", big); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared length over the limit: %d", w.Code)
	}
	chunked := func(body string) int {
		r := httptest.NewRequest("POST", "http://127.0.0.1:7777/v1/chat/completions", strings.NewReader(body))
		r.ContentLength = -1 // as net/http reports a chunked body
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if got := chunked(big); got != http.StatusRequestEntityTooLarge {
		t.Fatalf("chunked body over the limit: %d", got)
	}
	if got := chunked(chatBody("m")); got != 200 {
		t.Fatalf("chunked body within the limit: %d", got)
	}
	if used := g.buffered.used.Load(); used != 0 {
		t.Fatalf("%d bytes of the budget still held after the requests ended", used)
	}

	if !g.buffered.reserve(8<<20, 8<<20) { // other requests hold the whole budget
		t.Fatal("an empty budget refused its own size")
	}
	for name, code := range map[string]int{
		"declared length": direct(h, "POST", "/v1/chat/completions", "", chatBody("m")).Code,
		"chunked":         chunked(chatBody("m")),
	} {
		if code != http.StatusServiceUnavailable {
			t.Errorf("budget exhausted, %s: %d, want 503", name, code)
		}
	}
	if w := direct(h, "POST", "/v1/chat/completions", "", chatBody("m")); w.Header().Get("Retry-After") != "1" {
		t.Errorf("budget exhausted: Retry-After %q", w.Header().Get("Retry-After"))
	}
	g.buffered.release(8 << 20)
	if w := direct(h, "POST", "/v1/chat/completions", "", chatBody("m")); w.Code != 200 {
		t.Fatalf("budget free again: %d", w.Code)
	}
}

// What a client chooses is stored and labelled within bounds: a model name that could not be a model's is
// "invalid" in usage rows and metric labels, and paths and user agents are cut.
func TestClientChosenTextIsBounded(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	g, _ := newGateway(t)
	h := g.Handler()
	long := strings.Repeat("a", 1000)
	for _, model := range []string{"no such/model\n", long} {
		body, _ := json.Marshal(map[string]string{"model": model})
		if w := direct(h, "POST", "/v1/chat/completions/"+long, "", string(body), "User-Agent", long); w.Code != 404 {
			t.Fatalf("unknown model %.20q: %d %s", model, w.Code, w.Body)
		}
	}
	g.Close()
	rows := requestsOf(t, g.App, store.Filter{})
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.Model != invalidModel || len(r.Path) != maxStoredField || len(r.UserAgent) != maxStoredField || len(r.Error) > maxStoredError {
			t.Errorf("stored model %q, path of %d, user agent of %d, error of %d bytes", r.Model, len(r.Path), len(r.UserAgent), len(r.Error))
		}
	}
	g.metrics.mu.Lock()
	defer g.metrics.mu.Unlock()
	for k := range g.metrics.requests {
		if k[0] != invalidModel {
			t.Errorf("metric series for model %.40q", k[0])
		}
	}
}

// Refused keys cost nothing to send: only so many a minute are stored, the rest are counted.
func TestUnauthenticatedDenialsAreStoredAtALimitedRate(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	keys.Add(keys.Key{Name: "ops", Role: keys.RoleAdmin})
	g, _ := newGateway(t)
	h := g.Handler()
	for range maxStoredDenials + 10 {
		if w := direct(h, "GET", "/substrate/v1/whoami", "sk-sub-wrong", ""); w.Code != 401 {
			t.Fatalf("wrong key: %d", w.Code)
		}
	}
	g.Close()
	if rows := requestsOf(t, g.App, store.Filter{ErrorKind: "auth_failed"}); len(rows) != maxStoredDenials {
		t.Fatalf("%d denial rows stored, want %d", len(rows), maxStoredDenials)
	}
	if n := g.metrics.unstoredDenials; n != 10 {
		t.Fatalf("%d denials counted as not stored, want 10", n)
	}
}

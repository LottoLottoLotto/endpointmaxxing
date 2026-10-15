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

	"github.com/lotto/substrate/internal/app"
	"github.com/lotto/substrate/internal/keys"
	"github.com/lotto/substrate/internal/paths"
	"github.com/lotto/substrate/internal/runtime"
	"github.com/lotto/substrate/internal/spec"
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func TestKeysRolesAllowlistAndRate(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	backend := okBackend()
	defer backend.Close()
	instance(t, "m.fake", "m", backend.URL)
	instance(t, "other.fake", "other", backend.URL)
	user, err := keys.Add(keys.Key{Name: "alice", Role: keys.RoleUser, Models: []string{"m"}, RPM: 2})
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
	g.Cfg.Swap.MinResidency = spec.Duration(30 * time.Second)
	g.Cfg.Swap.MaxResidency = spec.Duration(10 * time.Minute)
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

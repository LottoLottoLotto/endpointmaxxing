package gateway

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LottoLottoLotto/substrate/internal/app"
	"github.com/LottoLottoLotto/substrate/internal/paths"
	"github.com/LottoLottoLotto/substrate/internal/runtime"
	"github.com/LottoLottoLotto/substrate/internal/store"
)

// selfStartTick is this process's /proc starttime, so a recorded instance pointing at it counts as alive.
func selfStartTick(t *testing.T) uint64 {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Skip("needs /proc")
	}
	s := string(data)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	var tick uint64
	json.Unmarshal([]byte(f[19]), &tick)
	return tick
}

func TestProxyRoutesByModelAndAPI(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	var gotBody, gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotBody, gotAuth = string(b), r.Header.Get("Authorization")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	in := runtime.Instance{ID: "m.fake", Name: "m", Engine: "fake", PID: os.Getpid(), StartTick: selfStartTick(t),
		BaseURL: backend.URL, HealthURL: backend.URL + "/health", APIs: []string{"chat"}, APIKey: "engine-key"}
	data, _ := json.Marshal(in)
	dir := filepath.Join(paths.State(), "instances")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "m.fake.json"), data, 0o644)

	a, err := app.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	g := New(a, log.New(io.Discard, "", 0))
	g.APIKey = "client-key"
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	post := func(path, body, key string) (*http.Response, string) {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	body := `{"model":"m","messages":[]}`
	if resp, _ := post("/v1/chat/completions", body, "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad client key: status %d", resp.StatusCode)
	}
	resp, out := post("/v1/chat/completions", body, "client-key")
	if resp.StatusCode != 200 || resp.Header.Get("X-Substrate-Instance") != "m.fake" || gotBody != body {
		t.Fatalf("proxy: status %d out %s body %q", resp.StatusCode, out, gotBody)
	}
	// The client's key is replaced by the engine's own.
	if gotAuth != "Bearer engine-key" {
		t.Fatalf("engine saw Authorization %q", gotAuth)
	}
	if resp, out := post("/v1/embeddings", `{"model":"m","input":"x"}`, "client-key"); resp.StatusCode != 400 || !strings.Contains(out, "does not implement") {
		t.Fatalf("unsupported api: %d %s", resp.StatusCode, out)
	}
	if resp, _ := post("/v1/chat/completions", `{"model":"other"}`, "client-key"); resp.StatusCode != 404 {
		t.Fatalf("unknown model: status %d", resp.StatusCode)
	}
	// A request without a model goes to the only ready instance.
	if resp, _ := post("/v1/chat/completions", `{"messages":[]}`, "client-key"); resp.StatusCode != 200 {
		t.Fatalf("model-less request: status %d", resp.StatusCode)
	}
}

// fakeInstance records an instance served by backend, alive as this test process.
func fakeInstance(t *testing.T, backend *httptest.Server, apis ...string) {
	in := runtime.Instance{ID: "m.fake", Name: "m", Engine: "fake", PID: os.Getpid(), StartTick: selfStartTick(t),
		BaseURL: backend.URL, HealthURL: backend.URL + "/health", APIs: apis}
	data, _ := json.Marshal(in)
	dir := filepath.Join(paths.State(), "instances")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "m.fake.json"), data, 0o600)
}

func TestStreamIsMeteredAndUsageRequested(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		var body struct {
			StreamOptions struct {
				IncludeUsage bool `json:"include_usage"`
			} `json:"stream_options"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		if body.StreamOptions.IncludeUsage { // only an engine asked for usage reports it
			w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3}}\n\n"))
		}
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer backend.Close()
	fakeInstance(t, backend, "chat")

	a, err := app.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	g := New(a, log.New(io.Discard, "", 0))
	g.AllowOpen = true
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[]}`))
	req.Header.Set("X-Substrate-Client", "agent-7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(out), `"usage"`) {
		t.Fatalf("client did not get the stream: %s", out)
	}
	id := resp.Header.Get("X-Substrate-Request-Id")
	g.Close() // flush the usage writer

	db, err := a.DB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Requests(store.Filter{ID: id})
	if err != nil || len(rows) != 1 {
		t.Fatalf("request %s not recorded: %v %v", id, rows, err)
	}
	r := rows[0]
	if r.PromptTokens == nil || *r.PromptTokens != 12 || *r.CompletionTokens != 3 || r.UsageSource != "usage" ||
		r.Client != "agent-7" || r.Engine != "fake" || !r.Stream || r.Status != 200 || r.TTFTMS == nil {
		t.Fatalf("recorded %+v", r)
	}
}

// Streamed tokens are only metered from the final usage chunk: a client may not turn it off.
func TestStreamUsageIsForced(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	b := newRecordingBackend()
	defer b.Close()
	instance(t, "m.fake", "m", b.URL)
	_, srv := newGateway(t)
	for _, opts := range []string{`{"include_usage":false}`, `null`, `"none"`, `{"include_usage":true,"x":1}`} {
		body := `{"model":"m","stream":true,"stream_options":` + opts + `,"messages":[]}`
		if got := call(t, "POST", srv.URL+"/v1/chat/completions", "", body); got != 200 {
			t.Fatalf("stream_options %s: status %d", opts, got)
		}
		seen, _ := b.last()
		if so, _ := seen["stream_options"].(map[string]any); so["include_usage"] != true {
			t.Fatalf("stream_options %s reached the engine as %v", opts, seen["stream_options"])
		}
	}
}

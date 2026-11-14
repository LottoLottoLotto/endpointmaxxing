package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
)

// fakeEngine installs a user manifest for the engine id the test instances use. Its binary is `go`, which
// every machine that runs these tests has, so the engine counts as installed.
func fakeEngine(t *testing.T) {
	t.Helper()
	manifest := `spec: emx.engine/v1
id: fake
name: fake engine
binary:
  candidates: [go]
launch:
  command: ["{bin}", "{port}"]
health:
  path: /health
context:
  path: /props
  field: settings.n_ctx
  split_by: parallel
flags:
  ctx: [-c, "{value}"]
  parallel: [-np, "{value}"]
capabilities:
  formats: [safetensors]
  quantizations: ["*"]
  apis: [chat]
memory: {mode: none}
`
	if err := os.MkdirAll(config.EnginesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.EnginesDir(), "fake.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A client choosing a model from /v1/models learns how much context each one has, and is not offered a
// model that could not answer: one whose files are gone, or that no installed engine serves.
func TestModelListShowsContextAndLeavesOutWhatCannotStart(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	fakeEngine(t)
	ckpt := filepath.Join(t.TempDir(), "ckpt")
	os.MkdirAll(ckpt, 0o755)
	os.WriteFile(filepath.Join(ckpt, "config.json"), []byte(`{"architectures":["X"]}`), 0o600)
	gone := filepath.Join(t.TempDir(), "moved-away")
	models := `models:
  - {name: stopped, source: "` + filepath.ToSlash(ckpt) + `", format: safetensors, engine: fake, flags: {ctx: "16384", parallel: "4"}}
  - {name: no-ctx, source: "` + filepath.ToSlash(ckpt) + `", format: safetensors, engine: fake}
  - {name: running, source: "` + filepath.ToSlash(ckpt) + `", format: safetensors, engine: fake, flags: {ctx: "8192"}}
  - {name: files-gone, source: "` + filepath.ToSlash(gone) + `", format: safetensors, engine: fake}
  - {name: no-engine, source: "` + filepath.ToSlash(ckpt) + `", format: safetensors, engine: vllm}
`
	os.MkdirAll(filepath.Dir(config.ModelsPath()), 0o755)
	if err := os.WriteFile(config.ModelsPath(), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, "aliases:\n    chat: running\n    old: files-gone\n")
	// The running engine reports the context it really has, which is not what was registered.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			w.Write([]byte(`{"settings":{"n_ctx":32768}}`))
		}
	}))
	defer backend.Close()
	instance(t, "running.fake", "running", backend.URL)
	_, srv := newGateway(t)

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID            string `json:"id"`
			MaxModelLen   int    `json:"max_model_len"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, m := range list.Data {
		if m.MaxModelLen != m.ContextLength {
			t.Errorf("%s: max_model_len %d and context_length %d differ", m.ID, m.MaxModelLen, m.ContextLength)
		}
		got[m.ID] = m.MaxModelLen
	}
	want := map[string]int{
		"stopped": 4096,  // registered ctx 16384 shared between 4 slots
		"no-ctx":  0,     // nothing known: no figure is better than a wrong one
		"running": 32768, // what the engine reports, not what was registered
		"chat":    32768, // an alias has its model's context
	}
	for name, ctx := range want {
		if n, ok := got[name]; !ok || n != ctx {
			t.Errorf("%s: listed=%v context=%d, want %d", name, ok, n, ctx)
		}
	}
	for _, hidden := range []string{"files-gone", "no-engine", "old"} {
		if _, ok := got[hidden]; ok {
			t.Errorf("%s is offered although it cannot be started", hidden)
		}
	}

	// An operator asks for everything, with the reason a model cannot start.
	resp2, err := http.Get(srv.URL + "/v1/models?unavailable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var all struct {
		Data []struct {
			ID  string `json:"id"`
			EMX struct {
				Unavailable string `json:"unavailable"`
			} `json:"emx"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&all); err != nil {
		t.Fatal(err)
	}
	why := map[string]string{}
	for _, m := range all.Data {
		why[m.ID] = m.EMX.Unavailable
	}
	if why["files-gone"] != "files missing" || why["no-engine"] != "no installed engine serves it" || why["running"] != "" || why["stopped"] != "" {
		t.Errorf("with ?unavailable=1: %v", why)
	}
}

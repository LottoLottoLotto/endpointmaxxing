package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
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
vision:
  path: /props
  field: modalities.vision
slots:
  path: /props
  field: total_slots
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
			w.Write([]byte(`{"settings":{"n_ctx":32768},"modalities":{"vision":true}}`))
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

// Beyond context, the list says what else a client choosing a model needs: whether it takes images, how
// long a reply may be, which request formats it serves, how long it takes to load, how fast it has been,
// and what hardware it needs.
func TestModelListCarriesWhatAClientChoosesBy(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	fakeEngine(t)
	dir := func(config string) string {
		d := filepath.Join(t.TempDir(), "ckpt")
		os.MkdirAll(d, 0o755)
		os.WriteFile(filepath.Join(d, "config.json"), []byte(config), 0o600)
		return filepath.ToSlash(d)
	}
	models := `models:
  - {name: text-model, source: "` + dir(`{"architectures":["X"]}`) + `", format: safetensors, engine: fake, flags: {ctx: "8192"}}
  - {name: vision-model, source: "` + dir(`{"architectures":["X"],"vision_config":{}}`) + `", format: safetensors, engine: fake, flags: {ctx: "8192"}}
  - {name: tower-removed, source: "` + dir(`{"vision_config":{},"language_model_only":true}`) + `", format: safetensors, engine: fake}
  - {name: running, source: "` + dir(`{"architectures":["X"]}`) + `", format: safetensors, engine: fake}
  - {name: solo, source: "` + dir(`{}`) + `", format: safetensors, engine: fake, flags: {parallel: "1"}}
  - {name: capped, source: "` + dir(`{}`) + `", format: safetensors, engine: fake, flags: {parallel: "8"}, max_concurrency: 2}
`
	os.MkdirAll(filepath.Dir(config.ModelsPath()), 0o755)
	if err := os.WriteFile(config.ModelsPath(), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, "gateway:\n    max_output_tokens: 1000\n")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" { // the running engine was started with a projector its files do not show
			w.Write([]byte(`{"settings":{"n_ctx":32768},"modalities":{"vision":true},"total_slots":4}`))
		}
	}))
	defer backend.Close()
	instance(t, "running.fake", "running", backend.URL)
	g, srv := newGateway(t)
	db, err := g.App.DB()
	if err != nil {
		t.Fatal(err)
	}
	load := int64(12300)
	if err := db.InsertEvent(&store.Event{TS: time.Now().UnixMilli(), Kind: "ready", Model: "text-model", DurationMS: &load}); err != nil {
		t.Fatal(err)
	}
	ttft, dur, out := int64(1000), int64(11000), int64(500) // 500 tokens in 10 s of decoding
	if err := db.InsertRequest(&store.Request{ID: "r1", TS: time.Now().UnixMilli(), Model: "text-model", Status: 200, Stream: true,
		CompletionTokens: &out, TTFTMS: &ttft, DurationMS: &dur, UsageSource: "usage"}); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Data []struct {
			ID     string `json:"id"`
			Limits struct {
				MaxOutputTokens int `json:"max_output_tokens"`
			} `json:"limits"`
			Architecture *struct {
				Input []string `json:"input_modalities"`
			} `json:"architecture"`
			EMX struct {
				APIs     []string       `json:"apis"`
				Images   *bool          `json:"images"`
				Load     float64        `json:"load_seconds"`
				Speed    float64        `json:"decode_tok_per_sec"`
				Hardware map[string]any `json:"hardware"`
				Conc     struct {
					Mode     string `json:"mode"`
					Serves   int    `json:"serves"`
					Parallel int    `json:"parallel"`
					Limit    int    `json:"limit"`
				} `json:"concurrency"`
			} `json:"emx"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	images := func(id string) string {
		for _, m := range list.Data {
			if m.ID == id {
				if m.Architecture == nil || m.EMX.Images == nil {
					return "unknown"
				}
				return strings.Join(m.Architecture.Input, "+")
			}
		}
		return "not listed"
	}
	for id, want := range map[string]string{
		"text-model":    "text",
		"vision-model":  "text+image",
		"tower-removed": "text",       // exported without its vision tower
		"running":       "text+image", // the engine's word wins over the files
	} {
		if got := images(id); got != want {
			t.Errorf("%s: input %s, want %s", id, got, want)
		}
	}
	for _, m := range list.Data {
		if len(m.EMX.APIs) == 0 || m.EMX.APIs[0] != "chat" {
			t.Errorf("%s: apis %v", m.ID, m.EMX.APIs)
		}
		switch m.ID {
		case "text-model":
			if m.Limits.MaxOutputTokens != 1000 { // the gateway's cap, below the 8192 context
				t.Errorf("text-model: max output %d, want 1000", m.Limits.MaxOutputTokens)
			}
			if m.EMX.Conc.Mode != "" {
				t.Errorf("text-model: mode %q although nothing says how many it serves", m.EMX.Conc.Mode)
			}
			if m.EMX.Load != 12.3 || m.EMX.Speed < 49 || m.EMX.Speed > 51 {
				t.Errorf("text-model: load %v s, speed %v tok/s, want 12.3 and about 50", m.EMX.Load, m.EMX.Speed)
			}
		case "running":
			if m.EMX.Hardware["loaded"] != true {
				t.Errorf("running: hardware %v", m.EMX.Hardware)
			}
			// Registered without a slot count: the engine chose 4, and says so.
			if c := m.EMX.Conc; c.Mode != "shared" || c.Serves != 4 || c.Parallel != 4 {
				t.Errorf("running: concurrency %+v, want shared, 4 at once", c)
			}
		case "solo":
			if c := m.EMX.Conc; c.Mode != "single" || c.Serves != 1 {
				t.Errorf("solo: concurrency %+v, want single", c)
			}
		case "capped": // eight engine slots, but the gateway lets two through
			if c := m.EMX.Conc; c.Mode != "shared" || c.Serves != 2 || c.Parallel != 8 || c.Limit != 2 {
				t.Errorf("capped: concurrency %+v, want 2 at once of 8 slots", c)
			}
		}
	}
}

package gateway

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/keys"
	"github.com/lotto/substrate/internal/store"
)

// send makes a JSON request and decodes the answer.
func send(t *testing.T, method, url, key, body string) (int, map[string]any) {
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
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

func TestSettingsAPIKeyLifecycle(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	admin, _ := keys.Add(keys.Key{Name: "ops", Role: keys.RoleAdmin})
	auditor, _ := keys.Add(keys.Key{Name: "audit", Role: keys.RoleAuditor})
	g, srv := newGateway(t)

	if s, _ := send(t, "POST", srv.URL+"/substrate/v1/keys", auditor, `{"name":"x"}`); s != http.StatusForbidden {
		t.Fatalf("auditor created a key: %d", s)
	}
	s, out := send(t, "POST", srv.URL+"/substrate/v1/keys", admin, `{"name":"alice","team":"research","rpm":30}`)
	secret, _ := out["secret"].(string)
	if s != http.StatusCreated || !strings.HasPrefix(secret, "sk-sub-") || out["role"] != keys.RoleUser {
		t.Fatalf("add: %d %v", s, out)
	}
	if w, _ := out["warnings"].([]any); len(w) != 1 {
		t.Errorf("an unconfigured team should warn: %v", out)
	}
	if s, who := send(t, "GET", srv.URL+"/substrate/v1/whoami", secret, ""); s != http.StatusOK || who["name"] != "alice" {
		t.Fatalf("the new key does not authenticate: %d %v", s, who)
	}
	if s, _ := send(t, "POST", srv.URL+"/substrate/v1/keys", admin, `{"name":"alice"}`); s != http.StatusConflict {
		t.Fatalf("duplicate name: %d", s)
	}

	if s, out := send(t, "PATCH", srv.URL+"/substrate/v1/keys/alice", admin, `{"role":"auditor","models":["qwen*"],"expires":"30d"}`); s != http.StatusOK {
		t.Fatalf("patch: %d %v", s, out)
	}
	if s, who := send(t, "GET", srv.URL+"/substrate/v1/whoami", secret, ""); s != http.StatusOK || who["role"] != keys.RoleAuditor {
		t.Fatalf("role change not applied: %v", who)
	}

	s, out = send(t, "POST", srv.URL+"/substrate/v1/keys/alice/rotate", admin, `{"grace":"0s"}`)
	fresh, _ := out["secret"].(string)
	if s != http.StatusOK || fresh == "" || fresh == secret {
		t.Fatalf("rotate: %d %v", s, out)
	}
	if s := call(t, "GET", srv.URL+"/substrate/v1/whoami", secret, ""); s != http.StatusUnauthorized {
		t.Fatalf("secret rotated out without grace still works: %d", s)
	}

	if s, _ := send(t, "POST", srv.URL+"/substrate/v1/keys/alice/disable", admin, ""); s != http.StatusOK {
		t.Fatalf("disable: %d", s)
	}
	if s := call(t, "GET", srv.URL+"/substrate/v1/whoami", fresh, ""); s != http.StatusUnauthorized {
		t.Fatalf("disabled key accepted: %d", s)
	}
	if s, _ := send(t, "POST", srv.URL+"/substrate/v1/keys/alice/enable", admin, ""); s != http.StatusOK {
		t.Fatalf("enable: %d", s)
	}
	if s := call(t, "GET", srv.URL+"/substrate/v1/whoami", fresh, ""); s != http.StatusOK {
		t.Fatalf("re-enabled key refused: %d", s)
	}

	// An admin key cannot lock itself out.
	for _, c := range []struct{ method, path, body string }{
		{"DELETE", "/substrate/v1/keys/ops", ""},
		{"POST", "/substrate/v1/keys/ops/disable", ""},
		{"PATCH", "/substrate/v1/keys/ops", `{"role":"user"}`},
	} {
		if s, _ := send(t, c.method, srv.URL+c.path, admin, c.body); s != http.StatusConflict {
			t.Errorf("%s %s on the caller's own key: %d", c.method, c.path, s)
		}
	}

	if s, _ := send(t, "DELETE", srv.URL+"/substrate/v1/keys/alice", admin, ""); s != http.StatusOK {
		t.Fatalf("delete: %d", s)
	}
	if s := call(t, "GET", srv.URL+"/substrate/v1/whoami", fresh, ""); s != http.StatusUnauthorized {
		t.Fatalf("deleted key accepted: %d", s)
	}
	if s, _ := send(t, "DELETE", srv.URL+"/substrate/v1/keys/alice", admin, ""); s != http.StatusNotFound {
		t.Fatalf("deleting an unknown key: %d", s)
	}

	// Every applied change is in the admin trail, attributed to the key and where it came from (recorded
	// asynchronously); refused calls change nothing and add nothing.
	trail := 0
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		trail = 0
		for _, e := range mustEvents(t, g, "admin") {
			if e.Owner == "ops" && strings.HasPrefix(e.Detail, "key ") && strings.HasSuffix(e.Detail, "[api from 127.0.0.1]") {
				trail++
			}
		}
		if trail >= 6 {
			break
		}
	}
	if trail != 6 { // add, set, rotate, disable, enable, rm
		t.Fatalf("admin trail has %d key changes, want 6", trail)
	}
}

func mustEvents(t *testing.T, g *Gateway, kind string) []store.Event {
	t.Helper()
	db := mustDB(t, g.App)
	ev, err := db.Events(store.Filter{Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestSettingsAPIGuards(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	admin, _ := keys.Add(keys.Key{Name: "ops", Role: keys.RoleAdmin})
	g, srv := newGateway(t)

	// The body must be JSON, and strictly what the endpoint takes.
	req, _ := http.NewRequest("POST", srv.URL+"/substrate/v1/keys", strings.NewReader(`name=x`))
	req.Header.Set("Authorization", "Bearer "+admin)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("form body: %v %v", resp, err)
	}
	if s, _ := send(t, "POST", srv.URL+"/substrate/v1/keys", admin, `{"name":"x","rol":"admin"}`); s != http.StatusBadRequest {
		t.Fatalf("unknown field accepted: %d", s)
	}

	// Secrets never cross a network in plain HTTP: a non-loopback client needs TLS.
	h := g.Handler()
	remote := func(tlsOn bool) int {
		r := httptest.NewRequest("POST", "/substrate/v1/keys", strings.NewReader(`{"name":"remote"}`))
		r.RemoteAddr = "203.0.113.5:40000"
		r.Header.Set("Authorization", "Bearer "+admin)
		r.Header.Set("Content-Type", "application/json")
		if tlsOn {
			r.TLS = &tls.ConnectionState{}
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if s := remote(false); s != http.StatusForbidden {
		t.Fatalf("plain HTTP from a remote client: %d", s)
	}
	if s := remote(true); s != http.StatusCreated {
		t.Fatalf("HTTPS from a remote client: %d", s)
	}

	// The settings API can be switched off; reads stay.
	writeConfig(t, "gateway:\n  disable_settings_api: true\n")
	if s, _ := send(t, "POST", srv.URL+"/substrate/v1/reload", admin, ""); s != http.StatusOK {
		t.Fatalf("reload: %d", s)
	}
	if s, _ := send(t, "PUT", srv.URL+"/substrate/v1/aliases/chat", admin, `{"model":"m"}`); s != http.StatusForbidden {
		t.Fatalf("change with the settings API disabled: %d", s)
	}
	if s, _ := send(t, "GET", srv.URL+"/substrate/v1/aliases", admin, ""); s != http.StatusOK {
		t.Fatalf("read with the settings API disabled: %d", s)
	}
}

func TestSettingsAPIConfigChangesApplyAtOnce(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	admin, _ := keys.Add(keys.Key{Name: "ops", Role: keys.RoleAdmin})
	g, srv := newGateway(t)

	ckpt := filepath.Join(t.TempDir(), "tiny")
	os.MkdirAll(ckpt, 0o755)
	os.WriteFile(filepath.Join(ckpt, "config.json"), []byte(`{"architectures":["Qwen3ForCausalLM"]}`), 0o600)
	s, out := send(t, "POST", srv.URL+"/substrate/v1/models", admin, `{"name":"tiny","source":"`+ckpt+`","flags":{"ctx":"4096"}}`)
	if s != http.StatusCreated {
		t.Fatalf("model add: %d %v", s, out)
	}
	if s, _ := send(t, "POST", srv.URL+"/substrate/v1/models", admin, `{"name":"tiny","source":"`+ckpt+`"}`); s != http.StatusConflict {
		t.Fatalf("re-adding without force: %d", s)
	}
	// Engine arguments reach host command lines: only the CLI sets them.
	if s, _ := send(t, "POST", srv.URL+"/substrate/v1/models", admin, `{"name":"t2","source":"`+ckpt+`","args":["--x"]}`); s != http.StatusBadRequest {
		t.Fatalf("engine args over HTTP: %d", s)
	}
	if s, _ := send(t, "PUT", srv.URL+"/substrate/v1/aliases/chat", admin, `{"model":"tiny"}`); s != http.StatusOK {
		t.Fatalf("alias: %d", s)
	}
	if s, _ := send(t, "PUT", srv.URL+"/substrate/v1/teams/research", admin, `{"tokens_per_day":1000,"models":["tiny"]}`); s != http.StatusOK {
		t.Fatalf("team: %d", s)
	}
	if s, out := send(t, "PUT", srv.URL+"/substrate/v1/prices/tiny", admin, `{"prompt":0.1,"completion":0.4,"currency":"EUR"}`); s != http.StatusOK {
		t.Fatalf("price: %d %v", s, out)
	}
	if s, _ := send(t, "PUT", srv.URL+"/substrate/v1/prices/other", admin, `{"prompt":0.1}`); s != http.StatusBadRequest {
		t.Fatalf("price without completion: %d", s)
	}

	// The running gateway sees every change without waiting for its file watcher.
	cfg := g.App.Config()
	if cfg.Aliases["chat"] != "tiny" || cfg.Pricing.Currency != "EUR" {
		t.Fatalf("config not applied: aliases %v, currency %q", cfg.Aliases, cfg.Pricing.Currency)
	}
	if team, ok := cfg.Team("research"); !ok || team.TokensPerDay != 1000 {
		t.Fatalf("team not applied: %+v", team)
	}
	if _, ok := g.App.RegisteredModel("tiny"); !ok {
		t.Fatal("model not applied")
	}
	onDisk, _ := config.Load()
	if onDisk.Aliases["chat"] != "tiny" {
		t.Fatalf("config.yaml lacks the alias: %v", onDisk.Aliases)
	}

	for _, p := range []string{"/substrate/v1/prices/tiny", "/substrate/v1/aliases/chat", "/substrate/v1/teams/research", "/substrate/v1/models/tiny"} {
		if s, _ := send(t, "DELETE", srv.URL+p, admin, ""); s != http.StatusOK {
			t.Fatalf("DELETE %s: %d", p, s)
		}
		if s, _ := send(t, "DELETE", srv.URL+p, admin, ""); s != http.StatusNotFound {
			t.Fatalf("DELETE %s again: %d", p, s)
		}
	}
	if _, ok := g.App.RegisteredModel("tiny"); ok {
		t.Fatal("removed model still registered")
	}
}

func TestSettingsAPIErasesOneClient(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	admin, _ := keys.Add(keys.Key{Name: "ops", Role: keys.RoleAdmin})
	g, srv := newGateway(t)
	db := mustDB(t, g.App)
	for i, c := range []string{"alice", "alice", "bob"} {
		if err := db.InsertRequest(&store.Request{ID: fmt.Sprintf("r%d", i), TS: time.Now().UnixMilli(), Model: "m", Client: c, Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	s, out := send(t, "DELETE", srv.URL+"/substrate/v1/clients/alice/data", admin, "")
	if s != http.StatusOK {
		t.Fatalf("erase: %d %v", s, out)
	}
	if rows := requestsOf(t, g.App, store.Filter{}); len(rows) != 1 || rows[0].Client != "bob" {
		t.Fatalf("after erasing alice: %+v", rows)
	}
}

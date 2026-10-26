package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lotto/substrate/internal/app"
	"github.com/lotto/substrate/internal/keys"
	"github.com/lotto/substrate/internal/paths"
	"github.com/lotto/substrate/internal/policy"
	"github.com/lotto/substrate/internal/store"
)

// TestPolicyClassificationErrorsAndAudit drives flagged, blocked, failed and unauthenticated requests
// through the gateway and checks what reaches the engine, the client, the usage database and the audit file.
func TestPolicyClassificationErrorsAndAudit(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	var forwarded atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		forwarded.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "too long") {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"context length exceeded"}}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`))
	}))
	defer backend.Close()
	fakeInstance(t, backend, "chat")
	user, err := keys.Add(keys.Key{Name: "alice", Role: keys.RoleUser})
	if err != nil {
		t.Fatal(err)
	}

	a, err := app.Open()
	if err != nil {
		t.Fatal(err)
	}
	a.Config().Policy = policy.Config{Enabled: true, Rules: []policy.Rule{
		{Name: "secrets", Severity: "high", Detectors: []string{"api_key"}},
		{Name: "codenames", Severity: "critical", Action: policy.ActionBlock, Keywords: []string{"Project Falcon"}},
	}}
	a.Config().Classify = policy.ClassifyConfig{Enabled: true}
	a.Config().Logging.FlaggedBodies = true
	a.Config().Logging.AuditFile = "audit.jsonl"
	g := New(a, log.New(io.Discard, "", 0))
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()

	post := func(key, content string) (int, string, string) {
		body, _ := json.Marshal(map[string]any{"model": "m", "messages": []map[string]string{{"role": "user", "content": content}}})
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(string(body)))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(out), resp.Header.Get("X-Substrate-Request-Id")
	}

	_, _, plainID := post(user, "Translate 'good night' into French")
	_, _, secretID := post(user, "debug this python function, my key is sk-proj-abcdefghijklmnopqrstuvwx1234")
	status, out, blockedID := post(user, "Draft the launch email for project falcon")
	if status != http.StatusBadRequest || !strings.Contains(out, "codenames") || strings.Contains(strings.ToLower(out), "falcon") {
		t.Fatalf("blocked request: %d %s (want 400 naming the rule, not the matched text)", status, out)
	}
	_, _, rejectedID := post(user, "this prompt is too long")
	if status, _, _ := post("sk-sub-wrong", "hi"); status != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", status)
	}
	if n := forwarded.Load(); n != 3 {
		t.Fatalf("engine saw %d requests, want 3 (the blocked one and the bad key never reach it)", n)
	}
	g.Close()

	db, err := a.DB()
	if err != nil {
		t.Fatal(err)
	}
	get := func(id string) store.Request {
		rows, err := db.Requests(store.Filter{ID: id})
		if err != nil || len(rows) != 1 {
			t.Fatalf("request %s: %v %v", id, rows, err)
		}
		return rows[0]
	}
	if r := get(plainID); r.Category != "translation" || len(r.Flags) != 0 || r.ErrorKind != "" {
		t.Fatalf("plain request recorded %+v", r)
	}
	if r := get(secretID); r.Category != "coding" || r.FlagSeverity != "high" || len(r.Flags) != 1 || r.Flags[0].Rule != "secrets" ||
		strings.Contains(r.Flags[0].Sample, "abcdefghij") || r.Status != 200 {
		t.Fatalf("flagged request recorded %+v", r)
	}
	if req, _, _, ok, _ := db.Bodies(secretID); !ok || !strings.Contains(string(req), "sk-proj-") {
		t.Fatal("flagged_bodies: the flagged request's body was not kept for review")
	}
	if _, _, _, ok, _ := db.Bodies(plainID); ok {
		t.Fatal("flagged_bodies kept an unflagged body")
	}
	if r := get(blockedID); r.ErrorKind != "policy_blocked" || r.FlagSeverity != "critical" || r.Category != "writing" {
		t.Fatalf("blocked request recorded %+v", r)
	}
	if r := get(rejectedID); r.ErrorKind != "engine_rejected" || r.Error != "context length exceeded" {
		t.Fatalf("engine 400 recorded %+v", r)
	}

	flagged, err := db.Requests(store.Filter{MinSeverity: "critical"})
	if err != nil || len(flagged) != 1 || flagged[0].ID != blockedID {
		t.Fatalf("--min-severity critical: %+v %v", flagged, err)
	}
	byRule, err := db.Requests(store.Filter{Rule: "secrets"})
	if err != nil || len(byRule) != 1 || byRule[0].ID != secretID {
		t.Fatalf("--rule secrets: %+v %v", byRule, err)
	}
	stats, err := db.FlagStats(store.Filter{})
	if err != nil || len(stats) != 2 {
		t.Fatalf("flag stats: %+v %v", stats, err)
	}
	errRows, err := db.ErrorStats(store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int64{}
	for _, e := range errRows {
		kinds[e.Kind] = e.Count
	}
	if kinds["policy_blocked"] != 1 || kinds["engine_rejected"] != 1 || kinds["auth_failed"] != 1 || len(kinds) != 3 {
		t.Fatalf("error stats: %+v", errRows)
	}
	rep, err := BuildReport(a, db, store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.Requests != 5 || rep.Totals.Flagged != 2 || rep.Totals.Blocked != 1 || rep.Models.Used != 1 || rep.Models.Running != 1 {
		t.Fatalf("report totals %+v models %+v", rep.Totals, rep.Models)
	}

	f, err := os.Open(filepath.Join(paths.State(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	types := map[string]int{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line map[string]any
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatalf("audit line %q: %v", sc.Text(), err)
		}
		if strings.Contains(sc.Text(), "sk-proj-abcdefghij") {
			t.Fatal("audit log holds the unredacted secret")
		}
		types[line["type"].(string)]++
	}
	if types["request"] != 4 || types["access_denied"] != 1 {
		t.Fatalf("audit line types %v, want 4 request + 1 access_denied", types)
	}
}

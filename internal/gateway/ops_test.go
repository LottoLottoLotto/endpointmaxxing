package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LottoLottoLotto/substrate/internal/keys"
	"github.com/LottoLottoLotto/substrate/internal/store"
)

// recordingBackend answers chat requests and remembers each request's body and X-Request-Id.
type recordingBackend struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []map[string]any
	ids    []string
}

func newRecordingBackend() *recordingBackend {
	b := &recordingBackend{}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		b.mu.Lock()
		b.bodies, b.ids = append(b.bodies, m), append(b.ids, r.Header.Get("X-Request-Id"))
		b.mu.Unlock()
		w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":5}}`))
	}))
	return b
}

func (b *recordingBackend) last() (map[string]any, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bodies[len(b.bodies)-1], b.ids[len(b.ids)-1]
}

func do(t *testing.T, method, url, key, body string) *http.Response {
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
	return resp
}

func TestAliasesOutputCapsRequestIDsAndRateLimitHeaders(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	b := newRecordingBackend()
	defer b.Close()
	instance(t, "m.fake", "m", b.URL)
	writeConfig(t, `
gateway:
    max_output_tokens: 1000
aliases:
    chat-default: m
teams:
    - name: small
      tokens_per_day: 15
`)
	capped, _ := keys.Add(keys.Key{Name: "capped", Role: keys.RoleUser, MaxOutputTokens: 100, RPM: 5})
	teamKey, _ := keys.Add(keys.Key{Name: "t", Role: keys.RoleUser, Team: "small"})
	g, srv := newGateway(t)
	defer g.Close()

	// The alias reaches the engine as the real model, and /v1/models lists it.
	resp := do(t, "POST", srv.URL+"/v1/chat/completions", capped, `{"model":"chat-default","messages":[],"max_tokens":5000}`)
	body, id := b.last()
	if resp.StatusCode != 200 || body["model"] != "m" || resp.Header.Get("X-Substrate-Alias") != "chat-default" {
		t.Fatalf("alias: status %d, engine saw model %v", resp.StatusCode, body["model"])
	}
	// The key's cap (100) is below the gateway's (1000): the request's 5000 is lowered to it.
	if body["max_tokens"] != 100.0 || resp.Header.Get("X-Substrate-Max-Output-Tokens") != "100" {
		t.Fatalf("output cap: engine saw max_tokens %v", body["max_tokens"])
	}
	if id == "" || id != resp.Header.Get("X-Substrate-Request-Id") {
		t.Fatalf("engine saw X-Request-Id %q, client got %q", id, resp.Header.Get("X-Substrate-Request-Id"))
	}
	if resp.Header.Get("x-ratelimit-limit-requests") != "5" || resp.Header.Get("x-ratelimit-remaining-requests") != "4" {
		t.Fatalf("rpm headers: %v", resp.Header)
	}
	// Without a limit the request gets the cap; a lower limit is kept.
	do(t, "POST", srv.URL+"/v1/chat/completions", capped, `{"model":"m","messages":[]}`)
	if body, _ := b.last(); body["max_tokens"] != 100.0 {
		t.Fatalf("missing limit not set to the cap: %v", body["max_tokens"])
	}
	do(t, "POST", srv.URL+"/v1/chat/completions", capped, `{"model":"m","messages":[],"max_tokens":7}`)
	if body, _ := b.last(); body["max_tokens"] != 7.0 {
		t.Fatalf("a limit under the cap was changed: %v", body["max_tokens"])
	}

	var models struct {
		Data []struct {
			ID        string         `json:"id"`
			Substrate map[string]any `json:"substrate"`
		} `json:"data"`
	}
	mreq, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	mreq.Header.Set("Authorization", "Bearer "+capped)
	r2, err := http.DefaultClient.Do(mreq)
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(r2.Body).Decode(&models)
	r2.Body.Close()
	found := false
	for _, m := range models.Data {
		found = found || (m.ID == "chat-default" && m.Substrate["alias_for"] == "m")
	}
	if !found {
		t.Fatalf("/v1/models does not list the alias: %+v", models.Data)
	}

	// Team token budget: remaining-tokens headers, then a 429 with Retry-After until local midnight.
	first := do(t, "POST", srv.URL+"/v1/chat/completions", teamKey, `{"model":"m","messages":[]}`)
	if first.Header.Get("x-ratelimit-limit-tokens") != "15" || first.Header.Get("x-ratelimit-remaining-tokens") != "15" {
		t.Fatalf("team token headers: %v", first.Header)
	}
	do(t, "POST", srv.URL+"/v1/chat/completions", teamKey, `{"model":"m","messages":[]}`)
	refused := do(t, "POST", srv.URL+"/v1/chat/completions", teamKey, `{"model":"m","messages":[]}`)
	retry, _ := strconv.Atoi(refused.Header.Get("Retry-After"))
	if refused.StatusCode != 429 || retry < 1 || retry > 86400+3600 || int64(retry) > int64(untilNextDay().Seconds())+2 {
		t.Fatalf("budget refusal: %d, Retry-After %q", refused.StatusCode, refused.Header.Get("Retry-After"))
	}
	// The rpm limit refuses with the wait until the oldest request leaves the minute window.
	for i := 0; i < 3; i++ {
		do(t, "POST", srv.URL+"/v1/chat/completions", capped, `{"model":"m","messages":[]}`)
	}
	over := do(t, "POST", srv.URL+"/v1/chat/completions", capped, `{"model":"m","messages":[]}`)
	retry, _ = strconv.Atoi(over.Header.Get("Retry-After"))
	if over.StatusCode != 429 || retry < 1 || retry > 60 || over.Header.Get("x-ratelimit-remaining-requests") != "0" {
		t.Fatalf("rpm refusal: %d Retry-After %q remaining %q", over.StatusCode, over.Header.Get("Retry-After"), over.Header.Get("x-ratelimit-remaining-requests"))
	}
}

// The usage record's id is the gateway's own: a client repeating its X-Request-Id loses no record, and
// its id is kept beside the gateway's.
func TestRequestIDsAreGeneratedByTheGateway(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	backend := okBackend()
	defer backend.Close()
	instance(t, "m.fake", "m", backend.URL)
	g, srv := newGateway(t)

	var ids []string
	for range 2 {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(chatBody("m")))
		req.Header.Set("X-Request-Id", "client-chosen")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		id := resp.Header.Get("X-Substrate-Request-Id")
		if resp.StatusCode != 200 || id == "" || id == "client-chosen" || resp.Header.Get("X-Request-Id") != id {
			t.Fatalf("status %d, X-Substrate-Request-Id %q, X-Request-Id %q", resp.StatusCode, id, resp.Header.Get("X-Request-Id"))
		}
		ids = append(ids, id)
	}
	if ids[0] == ids[1] {
		t.Fatalf("both requests got id %s", ids[0])
	}
	g.Close()
	rows := requestsOf(t, g.App, store.Filter{ID: "client-chosen"})
	if len(rows) != 2 || rows[0].ClientRequestID != "client-chosen" || rows[1].ClientRequestID != "client-chosen" {
		t.Fatalf("records by the client's id: %+v", rows)
	}
	if rows := requestsOf(t, g.App, store.Filter{ID: ids[0]}); len(rows) != 1 || rows[0].ID != ids[0] {
		t.Fatalf("record by the gateway's id: %+v", rows)
	}
}

func TestWebhooksAndAdminRecords(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	backend := okBackend()
	defer backend.Close()
	instance(t, "m.fake", "m", backend.URL)
	type hit struct {
		path string
		body map[string]any
		auth string
	}
	hits := make(chan hit, 16)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		hits <- hit{r.URL.Path, m, r.Header.Get("Authorization")}
	}))
	defer hook.Close()
	writeConfig(t, `
logging:
    enabled: true
    audit_file: audit.jsonl
alerts:
    webhooks:
        - url: `+hook.URL+`/slack/T000/SECRET
          format: text
          kinds: [budget_alert]
        - url: `+hook.URL+`/siem
          headers: {Authorization: Bearer hook-token}
          kinds: [admin, budget_alert]
teams:
    - name: small
      tokens_per_day: 12
`)
	admin, _ := keys.Add(keys.Key{Name: "ops", Role: keys.RoleAdmin})
	user, _ := keys.Add(keys.Key{Name: "u", Role: keys.RoleUser, Team: "small"})
	g, srv := newGateway(t)
	defer g.Close()

	do(t, "POST", srv.URL+"/v1/chat/completions", user, chatBody("m")) // 10 of 12 tokens: crosses 80%
	got := map[string]hit{}
	timeout := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case h := <-hits:
			got[h.path] = h
		case <-timeout:
			t.Fatalf("budget alert webhooks: got %v", got)
		}
	}
	slack, siem := got["/slack/T000/SECRET"], got["/siem"]
	if text, _ := slack.body["text"].(string); !strings.Contains(text, "budget_alert") || !strings.Contains(text, "team small") {
		t.Fatalf("text webhook: %v", slack.body)
	}
	if siem.body["kind"] != "budget_alert" || siem.auth != "Bearer hook-token" {
		t.Fatalf("json webhook: %v (auth %q)", siem.body, siem.auth)
	}

	// An administrative call through the management API is recorded under the key and sent to "admin" hooks.
	if resp := do(t, "POST", srv.URL+"/substrate/v1/reload", admin, ""); resp.StatusCode != 200 {
		t.Fatalf("reload: %d", resp.StatusCode)
	}
	select {
	case h := <-hits:
		if h.path != "/siem" || h.body["kind"] != "admin" || h.body["actor"] != "ops" || h.body["detail"] != "reload configuration" {
			t.Fatalf("admin webhook: %+v", h)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no admin webhook")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		evs, _ := mustDB(t, g.App).Events(store.Filter{})
		ok := false
		for _, e := range evs {
			ok = ok || (e.Kind == "admin" && e.Owner == "ops" && e.Detail == "reload configuration")
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("admin event not recorded: %+v", evs)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if redactURL(hook.URL+"/slack/T000/SECRET") != hook.URL+"/..." {
		t.Fatalf("webhook URLs must be logged without their secret path: %s", redactURL(hook.URL+"/slack/T000/SECRET"))
	}
}

func TestKeyExpiryAlertsOncePerDay(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	kinds := make(chan string, 8)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		kinds <- m["kind"].(string) + ": " + m["detail"].(string)
	}))
	defer hook.Close()
	writeConfig(t, "alerts:\n    webhooks:\n        - url: "+hook.URL+"\n          kinds: [key_expiring]\n")
	soon, far, gone := time.Now().Add(48*time.Hour), time.Now().Add(60*24*time.Hour), time.Now().Add(-time.Hour)
	keys.Add(keys.Key{Name: "soon", Role: keys.RoleUser, Expires: &soon})
	keys.Add(keys.Key{Name: "far", Role: keys.RoleUser, Expires: &far})
	keys.Add(keys.Key{Name: "gone", Role: keys.RoleUser, Expires: &gone})
	g, _ := newGateway(t)
	defer g.Close()
	g.checkKeyExpiry()
	g.checkKeyExpiry() // the same day: no repeats
	got := map[string]bool{}
	timeout := time.After(5 * time.Second)
	for len(got) < 2 {
		select {
		case k := <-kinds:
			got[k] = true
		case <-timeout:
			t.Fatalf("key expiry alerts: %v", got)
		}
	}
	select {
	case k := <-kinds:
		t.Fatalf("unexpected alert %q (only soon and gone, once each)", k)
	case <-time.After(300 * time.Millisecond):
	}
	for k := range got {
		if !strings.Contains(k, "key soon expires") && !strings.Contains(k, "key gone expired") {
			t.Fatalf("alert %q", k)
		}
	}
}

package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/keys"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
)

func chatPrompt(msgs ...string) []byte {
	parts := make([]string, len(msgs))
	for i, m := range msgs {
		parts[i] = `{"role":"user","content":"` + m + `"}`
	}
	return []byte(`{"model":"m","messages":[` + strings.Join(parts, ",") + `]}`)
}

// What a client's own earlier prompts explain of the cache: all of an earlier prompt it continues, part of
// one it shares a beginning with, nothing of another client's.
func TestOwnCachedCountsOnlyTheClientsOwnPrompts(t *testing.T) {
	var c cacheIsolation
	system := strings.Repeat("You are a careful assistant. ", 40) // ~1.1 kB shared beginning
	first := chatPrompt(system, "What is a B-tree?")
	c.remember("m", "alice", fingerprintOf(promptBytes("chat", first)), 300)

	next := fingerprintOf(promptBytes("chat", chatPrompt(system, "What is a B-tree?", "It is a tree.", "And a trie?")))
	if got, _ := c.ownCached("m", "alice", next); got != 300 {
		t.Fatalf("a continued conversation: own = %d, want all 300 tokens of the earlier prompt", got)
	}
	other := fingerprintOf(promptBytes("chat", chatPrompt(system, "Something else entirely")))
	if got, _ := c.ownCached("m", "alice", other); got <= 0 || got >= 300 {
		t.Fatalf("a prompt sharing the system text: own = %d, want part of 300", got)
	}
	if got, _ := c.ownCached("m", "bob", next); got != 0 {
		t.Fatalf("bob sent nothing: own = %d", got)
	}
	if got, _ := c.ownCached("other-model", "alice", next); got != 0 {
		t.Fatalf("another model's cache: own = %d", got)
	}
	// A completions prompt that goes on from an earlier one.
	doc := strings.Repeat("Lorem ipsum dolor sit amet. ", 20)
	c.remember("m", "carol", fingerprintOf(promptBytes("completions", []byte(`{"prompt":"`+doc+`"}`))), 120)
	if got, _ := c.ownCached("m", "carol", fingerprintOf(promptBytes("completions", []byte(`{"prompt":"`+doc+` More text."}`)))); got != 120 {
		t.Fatalf("a longer completions prompt: own = %d, want 120", got)
	}
	// Re-encoding does not change what counts as the same prompt.
	spaced := []byte(`{ "messages" : [ {"content":"` + system + `", "role":"user"}, {"role":"user","content":"What is a B-tree?"}, {"role":"user","content":"x"} ], "model":"m" }`)
	if got, _ := c.ownCached("m", "alice", fingerprintOf(promptBytes("chat", spaced))); got != 300 {
		t.Fatalf("the same messages, spaced and reordered keys: own = %d", got)
	}
}

func TestCapCachedCounts(t *testing.T) {
	in := `data: {"usage":{"prompt_tokens":900,"prompt_tokens_details":{"cached_tokens": 850}}}` + "\n" +
		`{"usage":{"input_tokens":10,"cache_read_input_tokens":700}}` + "\n" + `{"cached_tokens":12}`
	got := string(capCachedCounts([]byte(in), 100))
	for _, want := range []string{`"cached_tokens": 100}`, `"cache_read_input_tokens":100}`, `{"cached_tokens":12}`, `"prompt_tokens":900`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

// Answers are held back for the time the engine takes to compute the bytes of a prompt the client's own
// cache does not cover, once the model's speed is measured on uncached prompts.
func TestHoldForComesFromMeasuredPromptSpeed(t *testing.T) {
	var c cacheIsolation
	if d := c.holdFor("m", 4000); d != 0 {
		t.Fatalf("no measurements yet: hold %v", d)
	}
	for i := 0; i < cacheMinSamples; i++ {
		c.learn("m", 4000, 1000, 0, 0, time.Second) // 1000 uncached tokens in 1 s, 4 bytes a token
	}
	c.learn("m", 4000, 1000, 900, 0, 10*time.Millisecond) // the engine reported a cache hit: not a speed sample
	c.learn("m", 4000, 1000, 0, 800, 10*time.Millisecond) // the client continued its own prompt: not one either
	c.learn("m", 4000, 1000, 0, 0, 20*time.Millisecond)   // an unreported hit: outvoted by the median
	if d := c.holdFor("m", 4000); d < 990*time.Millisecond || d > 1010*time.Millisecond {
		t.Fatalf("4000 bytes not covered by the client's own cache: hold %v, want ~1s", d)
	}
	if d := c.holdFor("m", 1000); d < 240*time.Millisecond || d > 260*time.Millisecond {
		t.Fatalf("1000 bytes not covered: hold %v, want ~250ms", d)
	}
	if d := c.holdFor("m", 0); d != 0 {
		t.Fatalf("all of it the client's own: hold %v", d)
	}
}

// A prefix counts as everyone's once enough different clients sent it; one client's prompt never does, and
// sending it many times from one client does not make it common.
func TestSharedPrefixNeedsEnoughDifferentClients(t *testing.T) {
	var c cacheIsolation
	for i := 0; i < cacheMinSamples; i++ {
		c.learn("m", 4000, 1000, 0, 0, time.Second) // 4 bytes a token
	}
	system := strings.Repeat("Shared agent instructions. ", 60) // ~1.6 kB
	ask := func(question string) promptFP {
		return fingerprintOf(promptBytes("chat", chatPrompt(system, question)))
	}
	for i := 0; i < 10; i++ {
		c.seenBy("m", "solo", ask("q"), 5)
	}
	if _, cov := c.sharedPrefix("m", ask("mine"), 5); cov != 0 {
		t.Fatalf("one client sending it ten times made %d bytes common", cov)
	}
	for _, who := range []string{"a", "b", "c", "d"} {
		c.seenBy("m", who, ask("question from "+who), 5)
	}
	tokens, cov := c.sharedPrefix("m", ask("alice's own question"), 5)
	if cov < len(system) || cov > len(system)+200 || tokens != int64(cov/4) {
		t.Fatalf("after 5 clients sent the system prompt: covered %d bytes (%d tokens), want about the %d of the system prompt", cov, tokens, len(system))
	}
	if _, cov := c.sharedPrefix("m", ask("x"), 0); cov != 0 {
		t.Fatal("cache_share_after 0 shares nothing")
	}
	if _, cov := c.sharedPrefix("m", fingerprintOf(promptBytes("chat", chatPrompt("A private document", "q"))), 5); cov != 0 {
		t.Fatalf("an unseen prompt: covered %d", cov)
	}
}

// The cache domain: a delegating key's client, or the group it names, or the whole key with cache_shared.
func TestCacheDomainOfARequest(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	plain, _ := keys.Add(keys.Key{Name: "plain", Role: keys.RoleUser})
	host, _ := keys.Add(keys.Key{Name: "host", Role: keys.RoleUser, Delegate: true})
	team, _ := keys.Add(keys.Key{Name: "team", Role: keys.RoleUser, Delegate: true, CacheShared: true})
	g, _ := newGateway(t)
	domain := func(key string, headers map[string]string) string {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		p, err := g.authenticate(r)
		if err != nil {
			t.Fatal(err)
		}
		return cacheDomain(p, &store.Request{})
	}
	for _, c := range []struct {
		key     string
		headers map[string]string
		want    string
	}{
		{plain, map[string]string{"X-Emx-Client": "alice"}, "plain"}, // only a delegating key may name a client
		{host, nil, "host"},
		{host, map[string]string{"X-Emx-Client": "alice"}, "host/alice"},
		{host, map[string]string{"X-Emx-Client": "alice", "X-Emx-Cache-Group": "listing1"}, "host/group:listing1"},
		{team, map[string]string{"X-Emx-Client": "alice"}, "team"},
	} {
		if got := domain(c.key, c.headers); got != c.want {
			t.Errorf("%v: domain %q, want %q", c.headers, got, c.want)
		}
	}
}

// isolatedResponse runs an engine's response, cut into the given writes, through an isolated meteredWriter
// whose client's own prompts explain 100 cached tokens.
func isolatedResponse(t *testing.T, contentType string, writes ...string) (string, *meteredWriter) {
	t.Helper()
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", contentType)
	mw := &meteredWriter{ResponseWriter: rec, start: time.Now(), iso: &isolatedWrite{own: 100, ctx: context.Background()}}
	for _, p := range writes {
		if _, err := mw.Write([]byte(p)); err != nil {
			t.Fatal(err)
		}
	}
	mw.flushIsolated()
	return rec.Body.String(), mw
}

// llama.cpp's timings (prompt_n is the tokens it computed, cache_n the ones it found cached) and
// prompt_progress tell a cache hit as plainly as cached_tokens: isolation removes them, in a JSON answer and
// in a stream, and the meter still reads the engine's own counts.
func TestIsolationDropsEngineCacheTells(t *testing.T) {
	timings := `"timings":{"cache_n":850,"prompt_n":50,"prompt_ms":12.5,"prompt_per_second":4000,"predicted_n":3,"predicted_ms":30}`
	usage := `"usage":{"completion_tokens":3,"prompt_tokens":900,"total_tokens":903,"prompt_tokens_details":{"cached_tokens":850}}`
	body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"<b>\"timings\": hi</b>"},"finish_reason":"stop"}],"created":1,"model":"m",` + usage + `,` + timings + `}` + "\n"
	got, mw := isolatedResponse(t, "application/json", body[:40], body[40:])
	for _, bad := range []string{`"timings"`, `cache_n`, `prompt_n"`, `prompt_ms`, `850`} {
		if strings.Contains(got, bad) {
			t.Errorf("JSON answer still has %s: %s", bad, got)
		}
	}
	for _, want := range []string{`"cached_tokens":100`, `"prompt_tokens":900`, `"content":"<b>\"timings\": hi</b>"`, `"finish_reason":"stop"`} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON answer lacks %s: %s", want, got)
		}
	}
	if !strings.HasSuffix(got, "}\n") {
		t.Errorf("trailing newline lost: %q", got)
	}
	if u := mw.finish(); u.Cached == nil || *u.Cached != 850 || u.Prompt == nil || *u.Prompt != 900 {
		t.Errorf("metered from the engine's bytes: %+v", u)
	}

	chunk := `data: {"choices":[{"index":0,"delta":{"content":"Hi"}}],"model":"m"}` + "\n\n"
	progress := `data: {"choices":[],"prompt_progress":{"total":900,"cache":850,"processed":900,"time_ms":12}}` + "\n\n"
	final := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"model":"m",` + usage + `,` + timings + `}` + "\r\n\r\n"
	stream := chunk + progress + final + "data: [DONE]\n\n"
	got, mw = isolatedResponse(t, "text/event-stream", stream[:30], stream[30:len(chunk)+50], stream[len(chunk)+50:len(stream)-3], stream[len(stream)-3:])
	if !strings.HasPrefix(got, chunk) || !strings.HasSuffix(got, "data: [DONE]\n\n") {
		t.Errorf("chunks without cache fields must pass as they are: %q", got)
	}
	for _, bad := range []string{`"timings"`, `prompt_progress`, `cache_n`, `"cache":850`, `850`} {
		if strings.Contains(got, bad) {
			t.Errorf("stream still has %s: %q", bad, got)
		}
	}
	for _, want := range []string{`"cached_tokens":100`, `"prompt_tokens":900`, `"finish_reason":"stop"`, "}\r\n\r\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream lacks %q: %q", want, got)
		}
	}
	if u := mw.finish(); u.Cached == nil || *u.Cached != 850 {
		t.Errorf("stream metered from the engine's bytes: %+v", u)
	}
}

// Anthropic-style usage counts input_tokens without the cache reads (llama.cpp, vLLM and SGLang copy that):
// lowering cache_read_input_tokens alone would leave the hit visible as missing input, so it moves there.
func TestIsolationKeepsAnthropicInputTotal(t *testing.T) {
	body := `{"type":"message","content":[{"type":"text","text":"ok"}],"usage":{"cache_read_input_tokens":800,"input_tokens":100,"output_tokens":2}}`
	got, _ := isolatedResponse(t, "application/json", body)
	for _, want := range []string{`"cache_read_input_tokens":100`, `"input_tokens":800`, `"output_tokens":2`} {
		if !strings.Contains(got, want) {
			t.Errorf("JSON answer lacks %s: %s", want, got)
		}
	}
	start := "event: message_start\n" + `data: {"type":"message_start","message":{"id":"x","usage":{"cache_read_input_tokens":800,"input_tokens":100,"output_tokens":0}}}` + "\n\n"
	delta := "event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n"
	got, _ = isolatedResponse(t, "text/event-stream", start+delta)
	if !strings.Contains(got, `"cache_read_input_tokens":100,"input_tokens":800`) || !strings.HasSuffix(got, delta) || !strings.HasPrefix(got, "event: message_start\ndata: {") {
		t.Errorf("stream: %q", got)
	}
	// Within what the client's own prompts explain, nothing changes.
	own := `{"usage":{"cache_read_input_tokens":60,"input_tokens":40}}`
	if got, _ := isolatedResponse(t, "application/json", own); got != `{"usage":{"cache_read_input_tokens":60,"input_tokens":40}}` {
		t.Errorf("own cache hit rewritten: %s", got)
	}
}

// Under isolation, requests do not get llama.cpp's per-batch prompt progress (how many events come tells how
// much of the prompt was cached) or per-token timings.
func TestDropCacheTellOptions(t *testing.T) {
	got := string(dropCacheTellOptions([]byte(`{"model":"m","stream":true,"return_progress":true,"timings_per_token":true,"messages":[]}`)))
	if strings.Contains(got, "return_progress") || strings.Contains(got, "timings_per_token") || !strings.Contains(got, `"stream":true`) || !strings.Contains(got, `"model":"m"`) {
		t.Errorf("got %s", got)
	}
	plain := `{"model":"m",  "messages":[]}`
	if got := string(dropCacheTellOptions([]byte(plain))); got != plain {
		t.Errorf("a request without them changed: %s", got)
	}
}

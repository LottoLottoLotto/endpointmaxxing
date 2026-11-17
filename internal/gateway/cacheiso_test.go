package gateway

import (
	"strings"
	"testing"
	"time"
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

package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Prompt-cache isolation (gateway.cache_isolation). Engines reuse the computed start of a prompt they saw
// before (prefix caching) whoever sent it. With several clients on one model that is a leak: a client learns
// that someone else recently sent a prompt starting the way its own does, from the cached-token count in the
// usage it gets back (and is billed by) and from how fast the answer starts. The gateway closes both without
// help from the engine, so it works for every engine:
//
//   - It keeps, per model and client, the last prompts the client sent (as hashes of 64-byte chunks of the
//     prompt, chained so equal hashes mean an equal prefix) with the prompt tokens the engine counted. For a
//     new request the client's own earlier prompts explain at most `own` cached tokens: the most tokens of
//     an earlier prompt that the new one starts with.
//   - Cached-token counts in the response (cached_tokens, cache_read_input_tokens) are lowered to `own`; the
//     engine's count is recorded beside it (cached_reported). What a client sees and pays for is the cache
//     its own prompts left, never what another client's did.
//   - The first byte of the answer is held back until the time the engine would take to compute the part of
//     the prompt not explained by `own`, at the model's measured prompt speed. An answer sped up by another
//     client's cache then starts no sooner than one that was not; the client's own cache hits stay fast.
//
// Until a model has a few measured uncached prompts the gateway cannot estimate prompt time and does not
// hold answers back; counts are lowered from the first request.

const (
	cacheChunk          = 64      // bytes per prompt chunk
	cacheMaxChunks      = 1 << 15 // a prompt is fingerprinted up to 2 MiB
	cacheRecordsPer     = 16      // remembered prompts per client and model
	cacheClientsPer     = 4096    // remembered clients per model
	cacheMinSamples     = 3       // measured prompts before answers are held back
	cacheSampleMinToken = 32      // shorter prompts are not timed (fixed overhead dominates)
	cacheEWMA           = 0.2
)

type promptPrint struct {
	chain  []uint64 // chain[i]: hash of the prompt's first (i+1)*cacheChunk bytes
	whole  uint64   // hash of all of it
	bytes  int
	tokens int64 // prompt tokens the engine counted for it
}

// promptFP is a prompt's fingerprint: chunk hashes, and the bytes to test whether it continues an earlier one.
type promptFP struct {
	chain []uint64
	b     []byte
}

func fingerprintOf(p []byte) promptFP { return promptFP{chain: fingerprint(p), b: p} }

func hashBytes(p []byte) uint64 {
	h := fnv.New64a()
	h.Write(p)
	return h.Sum64()
}

type clientPrints struct {
	prints []promptPrint
	used   time.Time
}

type modelSpeed struct {
	rates       []float64 // recent prompt tokens per second, to the first byte
	bytesPerTok float64   // prompt bytes (promptBytes) per prompt token
	n           int
}

// cacheRateSamples is how many recent prompt speeds are kept per model.
const cacheRateSamples = 32

// tokPerSec is the model's prompt speed without cache: the median of recent speeds measured on prompts
// neither the engine (cached count) nor the client's own history (a continued prompt) says were cached; the
// median keeps the odd unreported hit, and a slow first request (kernel compilation), from moving it.
func (s *modelSpeed) tokPerSec() float64 {
	sorted := append([]float64(nil), s.rates...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

type cacheIsolation struct {
	mu     sync.Mutex
	prints map[string]map[string]*clientPrints // model -> client -> prompts
	speed  map[string]*modelSpeed
}

// promptKey is the client a request belongs to for cache isolation: the key and the client it acts for.
func promptKey(client, sub string) string {
	if sub == "" {
		return client
	}
	return client + "/" + sub
}

// promptBytes is the part of a request body an engine's prefix cache keys on, in a stable form: tools and
// system text first, then the conversation or prompt. nil when the request carries none (embeddings).
func promptBytes(api string, body []byte) []byte {
	var b map[string]json.RawMessage
	if json.Unmarshal(body, &b) != nil {
		return nil
	}
	var fields []string
	switch api {
	case "chat":
		fields = []string{"tools", "messages"}
	case "completions":
		fields = []string{"prompt"}
	case "messages":
		fields = []string{"tools", "system", "messages"}
	case "responses":
		fields = []string{"tools", "instructions", "input"}
	default:
		return nil
	}
	var out bytes.Buffer
	for _, f := range fields {
		raw, ok := b[f]
		if !ok {
			continue
		}
		// Re-encoded so key order and spacing do not decide what counts as the same prompt.
		var v any
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		out.WriteByte(0)
		out.WriteString(f)
		out.WriteByte(0)
		// Array elements are written open-ended, so a conversation that grows by a turn starts with all of
		// the bytes it had before.
		switch x := v.(type) {
		case []any:
			for _, it := range x {
				enc, _ := json.Marshal(it)
				out.Write(enc)
				out.WriteByte(0x1e)
			}
		case string: // a completions prompt: as it is, so a longer one starts with all of a shorter one
			out.WriteString(x)
		default:
			enc, _ := json.Marshal(v)
			out.Write(enc)
		}
	}
	if out.Len() == 0 {
		return nil
	}
	return out.Bytes()
}

func fingerprint(p []byte) []uint64 {
	n := min((len(p)+cacheChunk-1)/cacheChunk, cacheMaxChunks)
	chain := make([]uint64, n)
	var prev [8]byte
	for i := 0; i < n; i++ {
		h := fnv.New64a()
		h.Write(prev[:])
		h.Write(p[i*cacheChunk : min((i+1)*cacheChunk, len(p))])
		chain[i] = h.Sum64()
		binary.LittleEndian.PutUint64(prev[:], chain[i])
	}
	return chain
}

// ownCached is how many prompt tokens of a request the client's own earlier prompts to model can have left
// in the engine's cache, and how many bytes of the new prompt they cover.
func (c *cacheIsolation) ownCached(model, client string, fp promptFP) (tokens int64, covered int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := c.prints[model][client]
	if cp == nil {
		return 0, 0
	}
	for _, pp := range cp.prints {
		if len(fp.b) >= pp.bytes && hashBytes(fp.b[:pp.bytes]) == pp.whole { // continues all of it
			if pp.tokens > tokens {
				tokens, covered = pp.tokens, pp.bytes
			}
			continue
		}
		common := 0
		for common < len(pp.chain) && common < len(fp.chain) && pp.chain[common] == fp.chain[common] {
			common++
		}
		shared := min(common*cacheChunk, pp.bytes)
		if t := pp.tokens * int64(shared) / int64(max(pp.bytes, 1)); t > tokens {
			tokens, covered = t, shared
		}
	}
	return tokens, covered
}

// remember records a prompt the client sent to model with the prompt tokens the engine counted.
func (c *cacheIsolation) remember(model, client string, fp promptFP, tokens int64) {
	if len(fp.chain) == 0 || tokens <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.prints == nil {
		c.prints = map[string]map[string]*clientPrints{}
	}
	byClient := c.prints[model]
	if byClient == nil {
		byClient = map[string]*clientPrints{}
		c.prints[model] = byClient
	}
	cp := byClient[client]
	if cp == nil {
		if len(byClient) >= cacheClientsPer { // forget the client idle longest
			var oldest string
			var at time.Time
			for k, v := range byClient {
				if oldest == "" || v.used.Before(at) {
					oldest, at = k, v.used
				}
			}
			delete(byClient, oldest)
		}
		cp = &clientPrints{}
		byClient[client] = cp
	}
	cp.used = time.Now()
	cp.prints = append(cp.prints, promptPrint{chain: fp.chain, whole: hashBytes(fp.b), bytes: len(fp.b), tokens: tokens})
	if len(cp.prints) > cacheRecordsPer {
		cp.prints = cp.prints[len(cp.prints)-cacheRecordsPer:]
	}
}

// learn updates model's prompt speed from a request the engine computed without cache: one of promptBytes
// bytes and prompt tokens, of which the engine reported cached and the client's own history explained own.
func (c *cacheIsolation) learn(model string, promptBytes int, prompt, cached, own int64, ttft time.Duration) {
	if prompt < cacheSampleMinToken || ttft <= 0 || cached*10 > prompt || own*10 > prompt {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.speed == nil {
		c.speed = map[string]*modelSpeed{}
	}
	s := c.speed[model]
	if s == nil {
		s = &modelSpeed{}
		c.speed[model] = s
	}
	rate, ratio := float64(prompt)/ttft.Seconds(), float64(promptBytes)/float64(prompt)
	if s.n == 0 {
		s.bytesPerTok = ratio
	} else {
		s.bytesPerTok += cacheEWMA * (ratio - s.bytesPerTok)
	}
	s.rates = append(s.rates, rate)
	if len(s.rates) > cacheRateSamples {
		s.rates = s.rates[1:]
	}
	s.n++
}

// holdFor is how long after it reaches the engine a request may not answer: the time the engine takes to
// compute the bytes of its prompt the client's own cache does not cover. Zero while the speed is unknown.
func (c *cacheIsolation) holdFor(model string, uncoveredBytes int) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.speed[model]
	if s == nil || s.n < cacheMinSamples || s.bytesPerTok <= 0 {
		return 0
	}
	rate := s.tokPerSec()
	rest := float64(uncoveredBytes) / s.bytesPerTok
	if rest <= 0 || rate <= 0 {
		return 0
	}
	return time.Duration(rest / rate * float64(time.Second))
}

// cachedCountRe finds the cached-prompt-token counts of OpenAI and Anthropic usage objects.
var cachedCountRe = regexp.MustCompile(`"(cached_tokens|cache_read_input_tokens)"(\s*:\s*)(\d+)`)

// capCachedCounts lowers every cached-token count in b to at most own.
func capCachedCounts(b []byte, own int64) []byte {
	return cachedCountRe.ReplaceAllFunc(b, func(m []byte) []byte {
		sub := cachedCountRe.FindSubmatch(m)
		n, err := strconv.ParseInt(string(sub[3]), 10, 64)
		if err != nil || n <= own {
			return m
		}
		return []byte(`"` + string(sub[1]) + `"` + string(sub[2]) + strconv.FormatInt(own, 10))
	})
}

// isolatedWrite is a response's cache-isolation state: the cached tokens the client's own prompts explain
// and when its first byte may leave.
type isolatedWrite struct {
	own      int64
	covered  int       // prompt bytes own covers
	hold     time.Time // zero: no hold
	ctx      context.Context
	sse      bool
	pending  []byte // SSE: an unfinished line; JSON: the whole body, rewritten when it ends
	held     time.Duration
	reported *int64 // the engine's own cached count, from the usage the meter read
}

// wait holds the first byte until hold (or the client leaves).
func (iw *isolatedWrite) wait() {
	if iw.hold.IsZero() {
		return
	}
	if d := time.Until(iw.hold); d > 0 {
		t := time.NewTimer(d)
		select {
		case <-t.C:
			iw.held = d
		case <-iw.ctx.Done():
		}
		t.Stop()
	}
	iw.hold = time.Time{}
}

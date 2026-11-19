package gateway

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/fnv"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
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
//     its own prompts left, never what another client's did. Anthropic-style usage, whose input_tokens
//     leave out the cache reads, gets the lowered difference added back to input_tokens. Engine fields that
//     tell the same another way go: llama.cpp's timings (prompt_n, cache_n, prompt_ms: tokens computed and
//     how fast), prompt_progress and tokens_cached.
//   - The first byte of the answer is held back until the time the engine would take to compute the part of
//     the prompt not explained by `own`, at the model's measured prompt speed. An answer sped up by another
//     client's cache then starts no sooner than one that was not; the client's own cache hits stay fast.
//
// Prompts many clients share (an agent's system prompt, common tool definitions) are not secret: once a
// prompt prefix has come from gateway.cache_share_after different cache domains, a cache hit on it says
// only that several clients use it, so it counts as everyone's own (cached count and speed). A prefix only
// one client sent never qualifies. A key with cache_shared, or a group a delegating key names, is a single
// domain: its clients share all of their cache.
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
	common map[string]map[uint64]*prefixSeen // model -> prefix (chunk-chain hash) -> who sent it
}

// prefixSeen is a prompt prefix and the distinct cache domains that sent it, until enough have.
type prefixSeen struct {
	domains []uint64
	shared  bool
	at      time.Time
}

const (
	commonMaxChunks  = 2048    // prefixes tracked for sharing: the first 128 KiB of a prompt
	commonMaxEntries = 1 << 18 // per model
	commonMaxAge     = time.Hour
)

// cacheDomain is the prompt-cache domain of a request: the principal's (see principal.domain), else, for
// the master key and open mode, the client the request names.
func cacheDomain(p *principal, rec *store.Request) string {
	if p.domain != "" {
		return p.domain
	}
	return rec.Client
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

// sharedPrefix is how many bytes of a prompt are a prefix at least shareAfter cache domains sent (0: off),
// and the prompt tokens they are, by the model's measured bytes per token (0 until it is measured).
func (c *cacheIsolation) sharedPrefix(model string, fp promptFP, shareAfter int) (tokens int64, covered int) {
	if shareAfter < 2 {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := c.common[model]
	n := 0
	for n < len(fp.chain) && n < commonMaxChunks {
		if e := seen[fp.chain[n]]; e == nil || !e.shared {
			break
		}
		n++
	}
	covered = min(n*cacheChunk, len(fp.b))
	if s := c.speed[model]; s != nil && s.bytesPerTok > 0 {
		tokens = int64(float64(covered) / s.bytesPerTok)
	}
	return tokens, covered
}

// seenBy records that a cache domain sent a prompt, for sharedPrefix.
func (c *cacheIsolation) seenBy(model, domain string, fp promptFP, shareAfter int) {
	if shareAfter < 2 || len(fp.chain) == 0 {
		return
	}
	d := hashBytes([]byte(domain))
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.common == nil {
		c.common = map[string]map[uint64]*prefixSeen{}
	}
	seen := c.common[model]
	if seen == nil {
		seen = map[uint64]*prefixSeen{}
		c.common[model] = seen
	}
	for _, h := range fp.chain[:min(len(fp.chain), commonMaxChunks)] {
		e := seen[h]
		if e == nil {
			e = &prefixSeen{}
			seen[h] = e
		}
		e.at = now
		if e.shared || slices.Contains(e.domains, d) {
			continue
		}
		e.domains = append(e.domains, d)
		if len(e.domains) >= shareAfter {
			e.shared, e.domains = true, nil
		}
	}
	if len(seen) > commonMaxEntries { // forget old prefixes, then any, down to 90%
		for h, e := range seen {
			if now.Sub(e.at) > commonMaxAge {
				delete(seen, h)
			}
		}
		for h := range seen {
			if len(seen) <= commonMaxEntries*9/10 {
				break
			}
			delete(seen, h)
		}
	}
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

// cacheTellKeys are top-level response fields of engines that report what the prompt cache held: llama.cpp's
// timings (cache_n, prompt_n, prompt_ms and per-second rates), prompt_progress (return_progress: total,
// cache, processed) and tokens_cached. Isolation removes them.
var cacheTellKeys = []string{"timings", "prompt_progress", "tokens_cached"}

// dropCacheTellOptions removes the request options that ask llama.cpp for more of them: return_progress (a
// prompt_progress event per prompt batch it computes, so their number tells how much of the prompt was
// cached) and timings_per_token (timings on every chunk).
func dropCacheTellOptions(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	_, progress := m["return_progress"]
	_, timings := m["timings_per_token"]
	if !progress && !timings {
		return body
	}
	delete(m, "return_progress")
	delete(m, "timings_per_token")
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// isolateResponse rewrites what an isolated client gets of a response: b is a whole JSON body or, when sse,
// complete event-stream lines. Cached-token counts are lowered to own, Anthropic-style input_tokens (which
// leave out the cache reads) get the lowered difference back, and cacheTellKeys are removed. Text without
// those fields only has its counts lowered, byte for byte; JSON that holds them is re-encoded.
func isolateResponse(b []byte, own int64, sse bool) []byte {
	if !hasCacheTell(b) {
		return capCachedCounts(b, own)
	}
	if !sse {
		if out, ok := isolateJSON(b, own); ok {
			return out
		}
		return capCachedCounts(b, own)
	}
	out := make([]byte, 0, len(b))
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line = b[:i+1]
		}
		b = b[len(line):]
		if !hasCacheTell(line) {
			out = append(out, capCachedCounts(line, own)...)
			continue
		}
		payload, found := bytes.CutPrefix(line, []byte("data:"))
		start := len(line) - len(bytes.TrimLeft(payload, " \t"))
		if found {
			if v, ok := isolateJSON(line[start:], own); ok {
				out = append(append(out, line[:start]...), v...)
				continue
			}
		}
		out = append(out, capCachedCounts(line, own)...)
	}
	return out
}

func hasCacheTell(b []byte) bool {
	if bytes.Contains(b, []byte(`"cache_read_input_tokens"`)) {
		return true
	}
	for _, k := range cacheTellKeys {
		if bytes.Contains(b, []byte(`"`+k+`"`)) {
			return true
		}
	}
	return false
}

// isolateJSON rewrites one JSON value (with the whitespace around it kept); false when p is not one.
func isolateJSON(p []byte, own int64) ([]byte, bool) {
	body := bytes.TrimSpace(p)
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	if obj, ok := v.(map[string]any); ok {
		for _, k := range cacheTellKeys {
			delete(obj, k)
		}
		// The Responses API and Anthropic streams nest the answer; llama.cpp puts its fields beside it.
		for _, k := range []string{"response", "message"} {
			if inner, ok := obj[k].(map[string]any); ok {
				for _, t := range cacheTellKeys {
					delete(inner, t)
				}
			}
		}
	}
	lowerCachedCounts(v, own)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return nil, false
	}
	lead := len(p) - len(bytes.TrimLeft(p, " \t\r\n"))
	trail := p[len(bytes.TrimRight(p, " \t\r\n")):]
	out := make([]byte, 0, len(p))
	out = append(out, p[:lead]...)
	out = append(out, bytes.TrimSuffix(buf.Bytes(), []byte("\n"))...)
	return append(out, trail...), true
}

// lowerCachedCounts does capCachedCounts on decoded JSON, everywhere in it, and moves what it takes off an
// object's cache_read_input_tokens to its input_tokens: Anthropic usage (and llama.cpp's, vLLM's and
// SGLang's Anthropic APIs) counts input_tokens without the cache reads, so the total prompt stays as it was.
func lowerCachedCounts(v any, own int64) {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range []string{"cached_tokens", "cache_read_input_tokens"} {
			n, ok := jsonInt(x[k])
			if !ok || n <= own {
				continue
			}
			x[k] = json.Number(strconv.FormatInt(own, 10))
			if in, ok := jsonInt(x["input_tokens"]); ok && k == "cache_read_input_tokens" {
				x["input_tokens"] = json.Number(strconv.FormatInt(in+n-own, 10))
			}
		}
		for _, e := range x {
			lowerCachedCounts(e, own)
		}
	case []any:
		for _, e := range x {
			lowerCachedCounts(e, own)
		}
	}
}

func jsonInt(v any) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
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

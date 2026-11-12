package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LottoLottoLotto/substrate/internal/config"
	"github.com/LottoLottoLotto/substrate/internal/errs"
)

// ---- open mode ----

// loopbackHost reports whether a Host header names this machine's loopback interface.
func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.EqualFold(host, "localhost") || loopback(host)
}

// sameOrigin reports whether an Origin header names the scheme, host and port the request itself was sent to.
func sameOrigin(origin string, r *http.Request) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" { // "null" (sandboxed pages, file://) has no host
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return u.Scheme == scheme && strings.EqualFold(u.Host, r.Host)
}

// openModeRefusal says why a request may not use open mode ("" when it may). In open mode every caller is
// admin, so a web page in the operator's browser must not be able to reach the gateway: the request has to
// be addressed to a loopback name (a DNS-rebound name is not; serve --insecure lifts this for gateways
// opened to a network on purpose) and must not come from another site.
func (g *Gateway) openModeRefusal(r *http.Request) string {
	if !g.Insecure && !loopbackHost(r.Host) {
		return fmt.Sprintf("without API keys the gateway only answers requests addressed to localhost, not to %q; create keys (substrate key add) or start serve with --insecure", clip(r.Host, 128))
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return "without API keys the gateway refuses requests made by other web sites (Sec-Fetch-Site: cross-site); create keys (substrate key add)"
	}
	if _, sent := r.Header["Origin"]; sent && !sameOrigin(r.Header.Get("Origin"), r) {
		return fmt.Sprintf("without API keys the gateway refuses requests from another origin (%q); create keys (substrate key add)", clip(r.Header.Get("Origin"), 128))
	}
	return ""
}

// needsJSON reports whether r is a management or settings call with a body whose Content-Type is not
// application/json: a browser can send other types across origins without a preflight.
func needsJSON(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/substrate/v1/") || r.ContentLength == 0 {
		return false
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		return err != nil || mt != "application/json"
	}
	return false
}

// ---- request bodies ----

// bodyLimit is the largest request body c accepts, in bytes.
func bodyLimit(c *config.Gateway) int64 { return min(int64(c.MaxBodyMB)<<20, maxBody) }

// byteBudget bounds the request bodies all in-flight requests hold in memory together.
type byteBudget struct{ used atomic.Int64 }

func (b *byteBudget) reserve(n, max int64) bool {
	if b.used.Add(n) > max {
		b.used.Add(-n)
		return false
	}
	return true
}

func (b *byteBudget) release(n int64) { b.used.Add(-n) }

// errBodyTooLarge is answered with 413.
type errBodyTooLarge struct{ limit int64 }

func (e errBodyTooLarge) Error() string {
	return fmt.Sprintf("request body over %d MiB (gateway.max_body_mb)", e.limit>>20)
}

// errBufferFull is answered with 503 and Retry-After.
var errBufferFull = errs.New(errs.Capacity, "the gateway is holding too many request bodies in memory (gateway.max_buffered_mb); retry shortly")

// bodyStep is how much of the budget a body of unknown length takes at a time.
const bodyStep = 64 << 10

// bodyReadTimeout bounds how long a client may take to send its request body.
const bodyReadTimeout = 5 * time.Minute

// readBody buffers r's body, of at most limit bytes, within the gateway's memory budget. held is what the
// body holds of the budget, whether or not reading succeeded: the caller releases it when the request is done.
func (g *Gateway) readBody(r *http.Request, limit, budget int64) (body []byte, held int64, err error) {
	if r.ContentLength > limit {
		return nil, 0, errBodyTooLarge{limit}
	}
	if n := r.ContentLength; n >= 0 { // net/http never hands out more than the declared length
		if !g.buffered.reserve(n, budget) {
			return nil, 0, errBufferFull
		}
		body, err = io.ReadAll(r.Body)
		return body, n, err
	}
	for {
		if !g.buffered.reserve(bodyStep, budget) {
			return nil, held, errBufferFull
		}
		held += bodyStep
		body = slices.Grow(body, bodyStep)
		n, err := io.ReadFull(r.Body, body[len(body):len(body)+bodyStep])
		body = body[:len(body)+n]
		switch {
		case int64(len(body)) > limit:
			return nil, held, errBodyTooLarge{limit}
		case err == io.EOF || err == io.ErrUnexpectedEOF:
			return body, held, nil
		case err != nil:
			return nil, held, err
		}
	}
}

// guardedFields are the top-level request fields the gateway routes, meters and inspects by. Engines differ
// in how they read a repeated or differently-cased field, so only one exact, lower-case spelling is accepted:
// the gateway and the engine then always read the same value.
var guardedFields = map[string]bool{"model": true, "messages": true, "prompt": true, "input": true, "stream": true,
	"stream_options": true, "tools": true, "system": true, "instructions": true, "query": true, "documents": true}

// requestHead is what the gateway reads of a request body before forwarding it.
type requestHead struct {
	Model  string
	Stream bool
	User   string
}

// skipped validates a JSON value without keeping it.
type skipped struct{}

func (*skipped) UnmarshalJSON([]byte) error { return nil }

// parseRequest reads the head of a request body in one pass over its top-level object. It refuses anything
// but one JSON object, repeated fields, and guardedFields in another letter case.
func parseRequest(body []byte) (head requestHead, err error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	t, err := dec.Token()
	if err != nil {
		return head, err
	}
	if t != json.Delim('{') {
		return head, errors.New("a JSON object is expected")
	}
	seen := map[string]bool{}
	for dec.More() {
		if t, err = dec.Token(); err != nil {
			return head, err
		}
		key, _ := t.(string)
		if seen[key] {
			return head, fmt.Errorf("field %q is given more than once", clip(key, 64))
		}
		seen[key] = true
		if lower := strings.ToLower(key); lower != key && guardedFields[lower] {
			return head, fmt.Errorf("field %q must be spelled %q", key, lower)
		}
		var v any = &skipped{}
		switch key {
		case "model":
			v = &head.Model
		case "stream":
			v = &head.Stream
		case "user":
			v = &head.User
		}
		if err := dec.Decode(v); err != nil {
			return head, fmt.Errorf("field %q: %w", clip(key, 64), err)
		}
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return head, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return head, errors.New("data after the JSON object")
	}
	return head, nil
}

// ---- what is recorded ----

// invalidModel labels requests whose model name could not be a model's: recording what the client sent
// would let it create unbounded metric series and store arbitrary text.
const invalidModel = "invalid"

// modelLabel is name as recorded in usage rows, metrics and the audit file.
func modelLabel(name string) string {
	if name == "" || (len(name) <= 128 && config.NameRe.MatchString(name)) {
		return name
	}
	return invalidModel
}

// Client-chosen text is stored at most this long: paths, user agents and client names; error messages.
const (
	maxStoredField = 256
	maxStoredError = 1024
)

// clip truncates s to at most n bytes.
func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// maxStoredDenials is how many refused-key (401) records are stored per minute; the rest are only counted.
const maxStoredDenials = 60

// denialLimit counts 401 records in the current minute.
type denialLimit struct {
	mu     sync.Mutex
	window time.Time
	n      int
}

// allow reports whether one more 401 record may be stored now.
func (d *denialLimit) allow(now time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if now.Sub(d.window) >= time.Minute {
		d.window, d.n = now, 0
	}
	d.n++
	return d.n <= maxStoredDenials
}

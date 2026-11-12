package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/LottoLottoLotto/substrate/internal/config"
)

// setJSONField returns body with key set to value (value is already JSON), or body unchanged on error.
func setJSONField(body []byte, key string, value any) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return body
	}
	m[key] = raw
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// outputTokenFields are the request fields that limit output tokens, per API; the first is set when none is.
var outputTokenFields = map[string][]string{
	"chat":        {"max_tokens", "max_completion_tokens"},
	"completions": {"max_tokens"},
	"messages":    {"max_tokens"},
	"responses":   {"max_output_tokens"},
}

// outputCap is the effective cap: the smaller nonzero of the gateway's and the key's (0 = none).
func outputCap(gwCap, keyCap int) int {
	switch {
	case gwCap == 0:
		return keyCap
	case keyCap == 0 || gwCap < keyCap:
		return gwCap
	}
	return keyCap
}

// capOutputTokens lowers every output-token limit in body above limit to limit, and sets one when the
// request has none. It returns the body (rewritten only when something changed) and whether it changed.
func capOutputTokens(body []byte, api string, limit int) ([]byte, bool) {
	fields, ok := outputTokenFields[api]
	if !ok || limit <= 0 {
		return body, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, false
	}
	changed, present := false, false
	for _, f := range fields {
		raw, ok := m[f]
		if !ok || string(raw) == "null" {
			continue
		}
		present = true
		var n float64
		if json.Unmarshal(raw, &n) != nil || n > float64(limit) || n <= 0 {
			m[f] = json.RawMessage(strconv.Itoa(limit))
			changed = true
		}
	}
	if !present {
		m[fields[0]] = json.RawMessage(strconv.Itoa(limit))
		changed = true
	}
	if !changed {
		return body, false
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return out, true
}

// quota is what a request's key and team may still use, for rate-limit response headers.
type quota struct {
	rpmLimit, rpmRemaining int
	rpmReset               time.Duration
	tokLimit, tokRemaining int64 // -1: no token limit
	tokReset               time.Duration
}

// setRateLimitHeaders writes OpenAI-style x-ratelimit-* headers for the limits that apply.
func setRateLimitHeaders(h http.Header, q quota) {
	if q.rpmLimit > 0 {
		h.Set("x-ratelimit-limit-requests", strconv.Itoa(q.rpmLimit))
		h.Set("x-ratelimit-remaining-requests", strconv.Itoa(max(q.rpmRemaining, 0)))
		h.Set("x-ratelimit-reset-requests", durationHeader(q.rpmReset))
	}
	if q.tokLimit >= 0 {
		h.Set("x-ratelimit-limit-tokens", strconv.FormatInt(q.tokLimit, 10))
		h.Set("x-ratelimit-remaining-tokens", strconv.FormatInt(max(q.tokRemaining, 0), 10))
		h.Set("x-ratelimit-reset-tokens", durationHeader(q.tokReset))
	}
}

// durationHeader formats like OpenAI's reset headers: "6m0s", "1.5s".
func durationHeader(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(100 * time.Millisecond).String()
}

// untilNextDay and untilNextMonth are the waits until the local calendar period turns.
func untilNextDay() time.Duration {
	_, start := today()
	return time.Until(start.AddDate(0, 0, 1))
}

func untilNextMonth() time.Duration {
	_, start := thisMonth()
	return time.Until(start.AddDate(0, 1, 0))
}

// retryAfterSeconds renders a Retry-After value (at least 1).
func retryAfterSeconds(d time.Duration) string {
	s := int64(d.Seconds() + 0.999)
	if s < 1 {
		s = 1
	}
	return fmt.Sprint(s)
}

// tighter merges a token limit into q when it leaves less room than the one already there.
func (q *quota) tighter(limit, used int64, reset time.Duration) {
	if limit <= 0 {
		return
	}
	if rem := limit - used; q.tokLimit < 0 || rem < q.tokRemaining {
		q.tokLimit, q.tokRemaining, q.tokReset = limit, rem, reset
	}
}

// teamTokenQuota adds the team's token limits to q.
func (a *authState) teamTokenQuota(t *config.Team, q *quota) {
	if t == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.teamUsage(t.Name)
	q.tighter(t.TokensPerDay, u.dayTokens, untilNextDay())
	q.tighter(t.TokensPerMonth, u.monthTokens, untilNextMonth())
}

// Package meter extracts token counts, finish reasons and time-to-first-token
// from engine responses as they stream through the gateway: OpenAI chat and
// completions (JSON and SSE), the Responses API, Anthropic Messages, embeddings,
// and llama.cpp's `timings` as a fallback.
package meter

import (
	"bytes"
	"encoding/json"
	"time"
)

// Usage is what a response reported.
type Usage struct {
	Prompt, Completion, Cached, Reasoning *int64
	// Source is "usage" (engine-reported), "timings" (llama.cpp timings),
	// "chunks" (count of streamed content events, an estimate) or "" (none).
	Source string
	Finish string
}

// Meter consumes response bytes.
type Meter struct {
	start   time.Time
	stream  bool
	maxBody int

	firstByte time.Time
	firstTok  time.Time
	buf       bytes.Buffer // whole body (non-stream) or the partial SSE line
	overflow  bool
	chunks    int64
	u         Usage
}

// New returns a meter for a response; start is when the request arrived.
// stream selects SSE parsing; maxBody bounds buffering of a JSON body.
func New(start time.Time, stream bool, maxBody int) *Meter {
	return &Meter{start: start, stream: stream, maxBody: maxBody}
}

// Write observes response bytes. It never fails.
func (m *Meter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if m.firstByte.IsZero() {
		m.firstByte = time.Now()
	}
	if !m.stream {
		if !m.overflow && m.buf.Len()+len(p) <= m.maxBody {
			m.buf.Write(p)
		} else {
			m.overflow = true
		}
		return len(p), nil
	}
	m.buf.Write(p)
	for {
		line, rest, ok := bytes.Cut(m.buf.Bytes(), []byte("\n"))
		if !ok {
			break
		}
		m.line(bytes.TrimRight(line, "\r"))
		next := append([]byte(nil), rest...)
		m.buf.Reset()
		m.buf.Write(next)
	}
	if m.buf.Len() > m.maxBody { // a runaway line without newline
		m.buf.Reset()
	}
	return len(p), nil
}

func (m *Meter) line(l []byte) {
	data, ok := bytes.CutPrefix(l, []byte("data:"))
	if !ok {
		return
	}
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return // [DONE] and keep-alives
	}
	var ev map[string]any
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	if hasContent(ev) {
		m.chunks++
		if m.firstTok.IsZero() {
			m.firstTok = time.Now()
		}
	}
	m.absorb(ev)
}

// Finish parses a buffered JSON body and returns the result.
func (m *Meter) Finish() Usage {
	if !m.stream && !m.overflow && m.buf.Len() > 0 {
		var obj map[string]any
		if json.Unmarshal(m.buf.Bytes(), &obj) == nil {
			m.absorb(obj)
		}
	}
	if m.stream && m.buf.Len() > 0 { // last line without newline
		m.line(bytes.TrimRight(m.buf.Bytes(), "\r"))
		m.buf.Reset()
	}
	if m.u.Completion == nil && m.stream && m.chunks > 0 {
		n := m.chunks
		m.u.Completion, m.u.Source = &n, "chunks"
	}
	return m.u
}

// TTFT is the time from request start to the first streamed content (nil for non-streams or none seen).
func (m *Meter) TTFT() *time.Duration {
	if m.firstTok.IsZero() {
		return nil
	}
	d := m.firstTok.Sub(m.start)
	return &d
}

// absorb takes usage, timings and finish reasons from one JSON object.
func (m *Meter) absorb(obj map[string]any) {
	// Responses API streams nest the final response; Anthropic's message_start nests the message.
	for _, k := range []string{"response", "message"} {
		if inner, ok := obj[k].(map[string]any); ok {
			m.absorb(inner)
		}
	}
	if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
		m.takeUsage(u)
	}
	if t, ok := obj["timings"].(map[string]any); ok && m.u.Source != "usage" {
		if p := num(t, "prompt_n"); p != nil {
			m.u.Prompt = p
		}
		if c := num(t, "predicted_n"); c != nil {
			m.u.Completion = c
		}
		if m.u.Prompt != nil || m.u.Completion != nil {
			m.u.Source = "timings"
		}
	}
	if fr := finish(obj); fr != "" {
		m.u.Finish = fr
	}
}

func (m *Meter) takeUsage(u map[string]any) {
	set := func(dst **int64, v *int64) {
		// Anthropic reports input in message_start and output in message_delta; later
		// partial objects must not erase earlier counts, and cumulative counts only grow.
		if v != nil && (*dst == nil || *v >= **dst) {
			*dst = v
		}
	}
	got := false
	for _, k := range []string{"prompt_tokens", "input_tokens"} {
		if v := num(u, k); v != nil {
			set(&m.u.Prompt, v)
			got = true
		}
	}
	for _, k := range []string{"completion_tokens", "output_tokens"} {
		if v := num(u, k); v != nil {
			set(&m.u.Completion, v)
			got = true
		}
	}
	// Embeddings report only prompt and total tokens.
	if m.u.Completion == nil && m.u.Prompt != nil {
		if tot := num(u, "total_tokens"); tot != nil && *tot >= *m.u.Prompt {
			c := *tot - *m.u.Prompt
			m.u.Completion = &c
		}
	}
	if v := num(u, "cache_read_input_tokens"); v != nil {
		set(&m.u.Cached, v)
	}
	for _, k := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if d, ok := u[k].(map[string]any); ok {
			set(&m.u.Cached, num(d, "cached_tokens"))
		}
	}
	for _, k := range []string{"completion_tokens_details", "output_tokens_details"} {
		if d, ok := u[k].(map[string]any); ok {
			set(&m.u.Reasoning, num(d, "reasoning_tokens"))
		}
	}
	if got {
		m.u.Source = "usage"
	}
}

func num(m map[string]any, k string) *int64 {
	f, ok := m[k].(float64)
	if !ok {
		return nil
	}
	n := int64(f)
	return &n
}

func finish(obj map[string]any) string {
	if ch, ok := obj["choices"].([]any); ok && len(ch) > 0 {
		if c, ok := ch[0].(map[string]any); ok {
			if s, ok := c["finish_reason"].(string); ok {
				return s
			}
		}
	}
	if s, ok := obj["stop_reason"].(string); ok {
		return s
	}
	if d, ok := obj["delta"].(map[string]any); ok { // Anthropic message_delta
		if s, ok := d["stop_reason"].(string); ok {
			return s
		}
	}
	if t, _ := obj["object"].(string); t == "response" {
		if s, ok := obj["status"].(string); ok && s != "in_progress" {
			return s
		}
	}
	return ""
}

// hasContent reports whether a stream event carries generated text or reasoning.
func hasContent(ev map[string]any) bool {
	if ch, ok := ev["choices"].([]any); ok {
		for _, c := range ch {
			cm, _ := c.(map[string]any)
			if t, _ := cm["text"].(string); t != "" { // completions
				return true
			}
			d, _ := cm["delta"].(map[string]any)
			for _, k := range []string{"content", "reasoning_content", "reasoning"} {
				if s, _ := d[k].(string); s != "" {
					return true
				}
			}
			if tc, _ := d["tool_calls"].([]any); len(tc) > 0 {
				return true
			}
		}
		return false
	}
	switch ev["type"] {
	case "content_block_delta": // Anthropic
		return true
	case "response.output_text.delta", "response.reasoning_text.delta", "response.function_call_arguments.delta":
		return true
	}
	return false
}

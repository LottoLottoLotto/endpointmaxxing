package meter

import (
	"strings"
	"testing"
	"time"
)

func feed(t *testing.T, stream bool, body string, chunk int) (*Meter, Usage) {
	t.Helper()
	m := New(time.Now(), stream, 1<<20)
	for i := 0; i < len(body); i += chunk {
		end := i + chunk
		if end > len(body) {
			end = len(body)
		}
		m.Write([]byte(body[i:end]))
	}
	return m, m.Finish()
}

func eq(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %d", name, got, want)
	}
}

func TestOpenAIJSON(t *testing.T) {
	_, u := feed(t, false, `{"choices":[{"finish_reason":"stop","message":{"content":"hi"}}],
		"usage":{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":4},
		"completion_tokens_details":{"reasoning_tokens":3}}}`, 5)
	eq(t, "prompt", u.Prompt, 11)
	eq(t, "completion", u.Completion, 7)
	eq(t, "cached", u.Cached, 4)
	eq(t, "reasoning", u.Reasoning, 3)
	if u.Source != "usage" || u.Finish != "stop" {
		t.Fatalf("source %q finish %q", u.Source, u.Finish)
	}
}

func TestOpenAIStreamSplitAcrossWrites(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"length\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	m, u := feed(t, true, body, 7) // events straddle writes
	eq(t, "prompt", u.Prompt, 9)
	eq(t, "completion", u.Completion, 2)
	if u.Finish != "length" || m.TTFT() == nil {
		t.Fatalf("finish %q ttft %v", u.Finish, m.TTFT())
	}
}

func TestStreamWithoutUsageEstimatesFromChunks(t *testing.T) {
	body := strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\r\n\r\n", 5)
	_, u := feed(t, true, body, 64)
	eq(t, "completion", u.Completion, 5)
	if u.Source != "chunks" || u.Prompt != nil {
		t.Fatalf("got %+v", u)
	}
}

func TestAnthropicStream(t *testing.T) {
	body := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":21,"output_tokens":1}}}

event: content_block_delta
data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}

`
	_, u := feed(t, true, body, 13)
	eq(t, "prompt", u.Prompt, 21)
	eq(t, "completion", u.Completion, 12)
	if u.Finish != "end_turn" {
		t.Fatalf("finish %q", u.Finish)
	}
}

// Anthropic's input_tokens leaves out cache reads and writes; the prompt is their sum, the cached part
// the reads. Later events may repeat the counts (message_delta) or carry only output.
func TestAnthropicUsageWithCacheCountsWholePrompt(t *testing.T) {
	_, u := feed(t, false, `{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":12,
		"cache_read_input_tokens":800,"cache_creation_input_tokens":40,"output_tokens":5}}`, 9)
	eq(t, "prompt", u.Prompt, 852)
	eq(t, "cached", u.Cached, 800)
	eq(t, "completion", u.Completion, 5)

	body := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":800,"output_tokens":1}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":12,"cache_read_input_tokens":800,"output_tokens":9}}

event: message_delta
data: {"type":"message_delta","usage":{"output_tokens":9}}

`
	_, u = feed(t, true, body, 17)
	eq(t, "prompt", u.Prompt, 812)
	eq(t, "cached", u.Cached, 800)
	eq(t, "completion", u.Completion, 9)
}

func TestResponsesStream(t *testing.T) {
	body := `data: {"type":"response.output_text.delta","delta":"Hi"}

data: {"type":"response.completed","response":{"object":"response","status":"completed","usage":{"input_tokens":30,"output_tokens":8,"input_tokens_details":{"cached_tokens":16},"output_tokens_details":{"reasoning_tokens":2}}}}

`
	_, u := feed(t, true, body, 40)
	eq(t, "prompt", u.Prompt, 30)
	eq(t, "completion", u.Completion, 8)
	eq(t, "cached", u.Cached, 16)
	eq(t, "reasoning", u.Reasoning, 2)
	if u.Finish != "completed" {
		t.Fatalf("finish %q", u.Finish)
	}
}

func TestLlamaTimingsFallbackAndUsagePrecedence(t *testing.T) {
	_, u := feed(t, false, `{"timings":{"prompt_n":40,"predicted_n":9}}`, 100)
	eq(t, "prompt", u.Prompt, 40)
	if u.Source != "timings" {
		t.Fatalf("source %q", u.Source)
	}
	// Engine usage wins over timings in the same body.
	_, u = feed(t, false, `{"usage":{"prompt_tokens":41,"completion_tokens":10},"timings":{"prompt_n":40,"predicted_n":9}}`, 100)
	eq(t, "prompt", u.Prompt, 41)
	if u.Source != "usage" {
		t.Fatalf("source %q", u.Source)
	}
}

func TestEmbeddingsAndOverflow(t *testing.T) {
	_, u := feed(t, false, `{"data":[],"usage":{"prompt_tokens":6,"total_tokens":6}}`, 100)
	eq(t, "prompt", u.Prompt, 6)
	eq(t, "completion", u.Completion, 0)

	m := New(time.Now(), false, 10)
	m.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	if u := m.Finish(); u.Prompt != nil {
		t.Fatalf("a body over the cap must not be parsed partially: %+v", u)
	}
}

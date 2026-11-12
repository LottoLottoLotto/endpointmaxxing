package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Prompt is the text a request carries, from any API the gateway proxies.
type Prompt struct {
	// All is every system, user, assistant and tool text joined by newlines (what rules scan).
	All string
	// Last is the last user turn (what classification reads): the request's current ask.
	Last string
}

type part struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// Anthropic tool_use (name, input) and tool_result (content) blocks.
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
}

type toolCall struct {
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type message struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls []toolCall      `json:"tool_calls"`
	// Responses API function_call / function_call_output items.
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// tool is a tool definition: OpenAI chat ({function: {name, description}}), or Anthropic and the Responses
// API ({name, description}).
type tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Function    struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"function"`
}

// toolText is what a message carries beside its plain text: tool call names and arguments, and
// Anthropic tool_use inputs and tool_result contents.
func (m *message) toolText() string {
	out := []string{m.Name, argText(m.Arguments), deepText(m.Output)}
	for _, c := range m.ToolCalls {
		out = append(out, c.Function.Name, argText(c.Function.Arguments))
	}
	var parts []part
	if lenient(m.Content, &parts) {
		for _, p := range parts {
			switch p.Type {
			case "tool_use":
				out = append(out, p.Name, deepText(p.Input))
			case "tool_result": // a string or content parts; any other shape is read whole
				t := text(p.Content)
				if t == "" {
					t = deepText(p.Content)
				}
				out = append(out, t)
			}
		}
	}
	return joinText(out)
}

func joinText(ss []string) string {
	out := ss[:0]
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n")
}

// argText is a tool call's arguments: the strings inside when it is JSON (as it should be), else the text itself.
func argText(s string) string {
	if t := deepText(json.RawMessage(s)); t != "" {
		return t
	}
	return s
}

// lenient decodes raw into v and reports whether it is JSON at all: a field of an unexpected type is left
// zero rather than hiding the rest of the value from the scan.
func lenient(raw []byte, v any) bool {
	err := json.Unmarshal(raw, v)
	var typeErr *json.UnmarshalTypeError
	return err == nil || errors.As(err, &typeErr)
}

// deepText joins every string (object keys included, in key order) inside a JSON value, at any depth.
func deepText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				out = append(out, k)
				walk(x[k])
			}
		}
	}
	walk(v)
	return joinText(out)
}

// ExtractPrompt pulls text from an OpenAI chat/completions/responses/embeddings/rerank or Anthropic messages
// body: prompts and messages of every role, tool definitions (name, description), tool call arguments and
// tool results, and a rerank request's query and documents. Images and other non-text parts are skipped.
func ExtractPrompt(body []byte) Prompt {
	var req struct {
		Messages []message       `json:"messages"`
		System   json.RawMessage `json:"system"`
		Prompt   json.RawMessage `json:"prompt"`
		Input    json.RawMessage `json:"input"`
		Instr    string          `json:"instructions"`
		Tools    []tool          `json:"tools"`
		Query    json.RawMessage `json:"query"`
		Docs     json.RawMessage `json:"documents"`
	}
	if !lenient(body, &req) {
		return Prompt{}
	}
	var all []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			all = append(all, s)
		}
	}
	last := ""
	add(req.Instr)
	add(text(req.System))
	for _, t := range req.Tools {
		add(joinText([]string{t.Name, t.Description, t.Function.Name, t.Function.Description}))
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		t := text(m.Content)
		add(t)
		add(m.toolText())
		if m.Role == "user" && strings.TrimSpace(t) != "" {
			last = t
		}
	}
	for _, raw := range []json.RawMessage{req.Prompt, req.Input} {
		if len(raw) == 0 {
			continue
		}
		// Responses API input: a string, or items like messages.
		var items []message
		if lenient(raw, &items) && len(items) > 0 &&
			(items[0].Role != "" || len(items[0].Content) > 0 || items[0].Arguments != "" || len(items[0].Output) > 0) {
			for i := range items {
				m := &items[i]
				t := text(m.Content)
				add(t)
				add(m.toolText())
				if (m.Role == "user" || m.Role == "") && strings.TrimSpace(t) != "" {
					last = t
				}
			}
			continue
		}
		if t := text(raw); strings.TrimSpace(t) != "" {
			add(t)
			last = t
		}
	}
	// Rerank: the query is the ask; documents are strings or {text} objects.
	if q := text(req.Query); strings.TrimSpace(q) != "" {
		add(q)
		last = q
	}
	add(text(req.Docs))
	return Prompt{All: strings.Join(all, "\n"), Last: strings.TrimSpace(last)}
}

// text reads a string, an array of strings, or an array of content parts ({type: text|input_text, text}).
func text(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if json.Unmarshal(p, &s) == nil {
			out = append(out, s)
			continue
		}
		var pt part
		if lenient(p, &pt) && pt.Text != "" && (pt.Type == "" || strings.HasSuffix(pt.Type, "text")) {
			out = append(out, pt.Text)
		}
	}
	return strings.Join(out, "\n")
}

// Category is one task class, matched by keywords (whole words, case-insensitive) and RE2 patterns.
// The category with the most matches wins; ties go to the earlier category.
type Category struct {
	Name     string   `yaml:"name" json:"name"`
	Keywords []string `yaml:"keywords,omitempty" json:"keywords,omitempty"`
	Patterns []string `yaml:"patterns,omitempty" json:"patterns,omitempty"`
}

// ClassifyConfig is config.yaml's classify section.
type ClassifyConfig struct {
	// Enabled labels every proxied request's last user turn with a category.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Categories replace the built-in set when given.
	Categories []Category `yaml:"categories,omitempty" json:"categories,omitempty"`
	// Default labels prompts no category matches.
	Default string `yaml:"default" json:"default"`
}

// DefaultCategories is the built-in set, used when the config lists none.
var DefaultCategories = []Category{
	{Name: "coding", Keywords: []string{"code", "function", "bug", "debug", "compile", "compiler", "stack trace", "traceback",
		"refactor", "unit test", "python", "javascript", "typescript", "golang", "rust", "java", "c++", "sql query", "regex",
		"api", "endpoint", "repository", "git", "dockerfile", "kubernetes", "exception", "segfault", "script"},
		Patterns: []string{"```", `\bdef \w+\(`, `\bfunc \w+\(`, `\bclass \w+[:({]`, `#include\s*<`, `\bimport \w+`, `\w+\.\w+\(\)`}},
	{Name: "math", Keywords: []string{"calculate", "equation", "integral", "derivative", "probability", "prove", "proof",
		"theorem", "solve for", "algebra", "geometry", "matrix", "percentage", "how many", "sum of"},
		Patterns: []string{`\d+\s*[-+*/^×÷]\s*\d+`, `\\frac|\\sum|\\int`}},
	{Name: "translation", Keywords: []string{"translate", "translation", "in french", "in spanish", "in german", "in japanese",
		"in chinese", "into english", "to english"}},
	{Name: "summarization", Keywords: []string{"summarize", "summarise", "summary", "tl;dr", "tldr", "key points", "condense"}},
	{Name: "data_analysis", Keywords: []string{"dataset", "csv", "spreadsheet", "table", "chart", "statistics", "regression",
		"analyze the data", "analyse the data", "trend", "pivot", "dataframe", "pandas"}},
	{Name: "writing", Keywords: []string{"write", "essay", "story", "poem", "blog post", "email", "letter", "rewrite",
		"proofread", "draft", "cover letter", "tweet", "post", "copy", "headline", "greentext", "lyrics", "outline"}},
	{Name: "extraction", Keywords: []string{"extract", "parse", "json schema", "structured output", "fill in the fields", "classify",
		"label", "tag the"}},
	{Name: "reasoning", Keywords: []string{"step by step", "reason", "explain why", "trade-off", "tradeoff", "compare", "pros and cons",
		"plan", "strategy", "decide", "evaluate"}},
	{Name: "question_answering", Keywords: []string{"what is", "what are", "who is", "who was", "when did", "where is", "why does",
		"how does", "how do", "define", "explain"}},
}

// Classifier is a compiled ClassifyConfig.
type Classifier struct {
	cats []compiledCategory
	def  string
}

type compiledCategory struct {
	name string
	res  []*regexp.Regexp
}

// CompileClassifier prepares c.
func CompileClassifier(c *ClassifyConfig) (*Classifier, error) {
	cats := c.Categories
	if len(cats) == 0 {
		cats = DefaultCategories
	}
	cl := &Classifier{def: c.Default}
	if cl.def == "" {
		cl.def = "general"
	}
	seen := map[string]bool{}
	for i, cat := range cats {
		if cat.Name == "" {
			return nil, fmt.Errorf("classify.categories[%d]: name is required", i)
		}
		if seen[cat.Name] {
			return nil, fmt.Errorf("classify.categories[%d]: duplicate category %q", i, cat.Name)
		}
		seen[cat.Name] = true
		cc := compiledCategory{name: cat.Name}
		if re := keywordRegexp(cat.Keywords); re != nil {
			cc.res = append(cc.res, re)
		}
		for _, p := range cat.Patterns {
			re, err := regexp.Compile(p)
			if err != nil {
				return nil, fmt.Errorf("classify category %q: pattern %q: %w", cat.Name, p, err)
			}
			cc.res = append(cc.res, re)
		}
		if len(cc.res) == 0 {
			return nil, fmt.Errorf("classify category %q: needs keywords or patterns", cat.Name)
		}
		cl.cats = append(cl.cats, cc)
	}
	return cl, nil
}

// classifyMaxBytes bounds the text a classification reads (the start of the turn decides).
const classifyMaxBytes = 16 << 10

// Classify returns the best-matching category of text, the default when none match, or "" for empty text.
func (c *Classifier) Classify(text string) string {
	if c == nil {
		return ""
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if len(text) > classifyMaxBytes {
		text = text[:classifyMaxBytes]
	}
	best, bestN := c.def, 0
	for _, cat := range c.cats {
		n := 0
		for _, re := range cat.res {
			n += len(re.FindAllStringIndex(text, 64))
		}
		if n > bestN {
			best, bestN = cat.name, n
		}
	}
	return best
}

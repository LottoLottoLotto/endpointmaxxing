package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
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
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ExtractPrompt pulls text from an OpenAI chat/completions/responses/embeddings or Anthropic messages body.
// Images and other non-text parts are skipped.
func ExtractPrompt(body []byte) Prompt {
	var req struct {
		Messages []message       `json:"messages"`
		System   json.RawMessage `json:"system"`
		Prompt   json.RawMessage `json:"prompt"`
		Input    json.RawMessage `json:"input"`
		Instr    string          `json:"instructions"`
	}
	if json.Unmarshal(body, &req) != nil {
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
	for _, m := range req.Messages {
		t := text(m.Content)
		add(t)
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
		if json.Unmarshal(raw, &items) == nil && len(items) > 0 && (items[0].Role != "" || len(items[0].Content) > 0) {
			for _, m := range items {
				t := text(m.Content)
				add(t)
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
		if json.Unmarshal(p, &pt) == nil && pt.Text != "" && (pt.Type == "" || strings.HasSuffix(pt.Type, "text")) {
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

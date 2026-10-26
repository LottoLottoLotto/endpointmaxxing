// Package policy inspects prompts as they pass through the gateway: it flags
// (and optionally blocks) requests that match configured rules - built-in
// detectors for secrets and personal data, keywords, regular expressions - and
// sorts each prompt into a task category. It is pure (no I/O): the gateway
// records what it returns.
package policy

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Actions a rule takes when it matches.
const (
	ActionFlag  = "flag"  // record the match; the request proceeds
	ActionBlock = "block" // refuse the request (HTTP 400, content_policy_violation)
)

// Severities, lowest first.
var Severities = []string{"low", "medium", "high", "critical"}

// Config is config.yaml's policy section.
type Config struct {
	// Enabled scans every proxied request's prompt against Rules.
	Enabled bool `yaml:"enabled" json:"enabled"`
	// ScanMaxBytes bounds how much prompt text one request is scanned for (the rest is skipped).
	ScanMaxBytes int `yaml:"scan_max_bytes" json:"scan_max_bytes"`
	// Rules are evaluated in order; every matching rule is recorded.
	Rules []Rule `yaml:"rules,omitempty" json:"rules,omitempty"`
}

// Rule matches prompt text by built-in detectors, keywords (case-insensitive, whole words) or regular
// expressions (RE2 syntax). Models, Clients and APIs narrow where it applies (empty = everywhere).
type Rule struct {
	Name      string   `yaml:"name" json:"name"`
	Detectors []string `yaml:"detectors,omitempty" json:"detectors,omitempty"`
	Keywords  []string `yaml:"keywords,omitempty" json:"keywords,omitempty"`
	Patterns  []string `yaml:"patterns,omitempty" json:"patterns,omitempty"`
	Severity  string   `yaml:"severity,omitempty" json:"severity,omitempty"` // default medium
	Action    string   `yaml:"action,omitempty" json:"action,omitempty"`     // flag (default) or block
	Models    []string `yaml:"models,omitempty" json:"models,omitempty"`
	Clients   []string `yaml:"clients,omitempty" json:"clients,omitempty"`
	APIs      []string `yaml:"apis,omitempty" json:"apis,omitempty"`
}

// Flag is one rule's matches in one prompt. Sample is a redacted example of the
// matched text (never the full secret).
type Flag struct {
	Rule     string `json:"rule"`
	Detector string `json:"detector"` // detector name, "keyword" or "pattern"
	Severity string `json:"severity"`
	Action   string `json:"action"`
	Count    int    `json:"count"`
	Sample   string `json:"sample,omitempty"`
}

// detector finds candidates with re and keeps those valid passes.
type detector struct {
	re    *regexp.Regexp
	valid func(string) bool
	help  string
}

var detectors = map[string]detector{
	"email": {re: regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`),
		help: "e-mail addresses"},
	"credit_card": {re: regexp.MustCompile(`\b(?:\d[ -]?){12,18}\d\b`), valid: luhn,
		help: "payment card numbers (13-19 digits passing the Luhn check)"},
	"us_ssn": {re: regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`), valid: ssn,
		help: "US social security numbers (AAA-GG-SSSS, valid area/group/serial)"},
	"phone": {re: regexp.MustCompile(`(?:\+\d{1,3}[ .-]?)?(?:\(\d{3}\)|\b\d{3})[ .-]\d{3}[ .-]\d{4}\b`),
		help: "phone numbers (NANP-style, optional country code)"},
	"ipv4": {re: regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`),
		help: "IPv4 addresses"},
	"iban": {re: regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,3})?\b`), valid: iban,
		help: "IBAN bank account numbers (checksum verified)"},
	"aws_access_key": {re: regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`),
		help: "AWS access key ids"},
	"private_key": {re: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`),
		help: "PEM / OpenSSH / PGP private key blocks"},
	"jwt": {re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),
		help: "JSON web tokens"},
	"api_key": {re: regexp.MustCompile(`\b(?:sk-(?:proj-|ant-)?[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{40,}|` +
		`xox[abposr]-[A-Za-z0-9-]{10,}|hf_[A-Za-z0-9]{30,}|AIza[0-9A-Za-z_-]{35}|glpat-[A-Za-z0-9_-]{20,}|` +
		`sk_live_[A-Za-z0-9]{20,}|rk_live_[A-Za-z0-9]{20,}|bhk_[0-9a-f]{40})`),
		help: "API tokens with well-known prefixes (OpenAI, Anthropic, GitHub, Slack, Hugging Face, Google, GitLab, Stripe, ...)"},
	"password_assignment": {re: regexp.MustCompile(`(?i)(?:\b|_)(?:password|passwd|pwd|secret|api[_-]?key|access[_-]?token)\s*[:=]\s*["']?[^\s"']{6,}`),
		help: `credentials assigned in text or config ("password: hunter22", "API_KEY=...")`},
	"prompt_injection": {re: regexp.MustCompile(`(?i)\b(?:ignore|disregard|forget)\s+(?:all\s+|any\s+|the\s+)?(?:previous|prior|above|earlier)\s+(?:instructions|prompts|rules|messages)|` +
		`(?:reveal|print|show|repeat)\s+(?:your|the)\s+(?:system\s+prompt|hidden\s+instructions|initial\s+instructions)|` +
		`\byou are now (?:in )?(?:DAN|developer mode|jailbreak)`),
		help: "common prompt-injection and jailbreak phrasings"},
}

// Detectors lists the built-in detector names with what they find.
func Detectors() map[string]string {
	out := make(map[string]string, len(detectors))
	for k, d := range detectors {
		out[k] = d.help
	}
	return out
}

// DetectorNames lists the built-in detectors, sorted.
func DetectorNames() []string {
	out := make([]string, 0, len(detectors))
	for k := range detectors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Validate checks rule names, detectors, patterns, severities and actions.
func (c *Config) Validate() error {
	if c.ScanMaxBytes < 0 {
		return fmt.Errorf("policy.scan_max_bytes must be >= 0")
	}
	_, err := Compile(c)
	return err
}

// Policy is a compiled Config.
type Policy struct {
	maxBytes int
	rules    []compiledRule
}

type matcher struct {
	name string // detector name, "keyword" or "pattern"
	re   *regexp.Regexp
	ok   func(string) bool
}

type compiledRule struct {
	Rule
	matchers []matcher
}

// DefaultScanMaxBytes bounds the scanned prompt text when ScanMaxBytes is 0.
const DefaultScanMaxBytes = 1 << 20

// Compile prepares c's rules; a nil Policy (disabled) flags nothing.
func Compile(c *Config) (*Policy, error) {
	p := &Policy{maxBytes: c.ScanMaxBytes}
	if p.maxBytes == 0 {
		p.maxBytes = DefaultScanMaxBytes
	}
	seen := map[string]bool{}
	for i, r := range c.Rules {
		where := fmt.Sprintf("policy.rules[%d]", i)
		if r.Name == "" {
			return nil, fmt.Errorf("%s: name is required", where)
		}
		if seen[r.Name] {
			return nil, fmt.Errorf("%s: duplicate rule name %q", where, r.Name)
		}
		seen[r.Name] = true
		where = fmt.Sprintf("policy rule %q", r.Name)
		if r.Severity == "" {
			r.Severity = "medium"
		}
		if !contains(Severities, r.Severity) {
			return nil, fmt.Errorf("%s: severity %q (want %s)", where, r.Severity, strings.Join(Severities, ", "))
		}
		if r.Action == "" {
			r.Action = ActionFlag
		}
		if r.Action != ActionFlag && r.Action != ActionBlock {
			return nil, fmt.Errorf("%s: action %q (want flag or block)", where, r.Action)
		}
		cr := compiledRule{Rule: r}
		for _, d := range r.Detectors {
			det, ok := detectors[d]
			if !ok {
				return nil, fmt.Errorf("%s: unknown detector %q (want %s)", where, d, strings.Join(DetectorNames(), ", "))
			}
			cr.matchers = append(cr.matchers, matcher{name: d, re: det.re, ok: det.valid})
		}
		if re := keywordRegexp(r.Keywords); re != nil {
			cr.matchers = append(cr.matchers, matcher{name: "keyword", re: re})
		}
		for _, pat := range r.Patterns {
			re, err := regexp.Compile(pat)
			if err != nil {
				return nil, fmt.Errorf("%s: pattern %q: %w", where, pat, err)
			}
			cr.matchers = append(cr.matchers, matcher{name: "pattern", re: re})
		}
		if len(cr.matchers) == 0 {
			return nil, fmt.Errorf("%s: needs detectors, keywords or patterns", where)
		}
		p.rules = append(p.rules, cr)
	}
	return p, nil
}

// Scope is where a request goes: rules with Models/Clients/APIs lists apply only there.
type Scope struct {
	Model, Client, API string
}

func (r *compiledRule) applies(s Scope) bool {
	return (len(r.Models) == 0 || contains(r.Models, s.Model)) &&
		(len(r.Clients) == 0 || contains(r.Clients, s.Client)) &&
		(len(r.APIs) == 0 || contains(r.APIs, s.API))
}

// maxSampleMatches bounds how many matches per matcher are counted (the count saturates there).
const maxSampleMatches = 1000

// Scan returns one Flag per matching (rule, matcher), in rule order, and whether any of them blocks.
func (p *Policy) Scan(text string, s Scope) (flags []Flag, blocked bool) {
	if p == nil || len(p.rules) == 0 || text == "" {
		return nil, false
	}
	if len(text) > p.maxBytes {
		text = text[:p.maxBytes]
		for len(text) > 0 && !utf8.ValidString(text) { // never split a rune
			text = text[:len(text)-1]
		}
	}
	for i := range p.rules {
		r := &p.rules[i]
		if !r.applies(s) {
			continue
		}
		for _, m := range r.matchers {
			n, sample := 0, ""
			for _, loc := range m.re.FindAllStringIndex(text, maxSampleMatches) {
				hit := text[loc[0]:loc[1]]
				if m.ok != nil && !m.ok(hit) {
					continue
				}
				if n == 0 {
					sample = Redact(hit)
				}
				n++
			}
			if n == 0 {
				continue
			}
			flags = append(flags, Flag{Rule: r.Name, Detector: m.name, Severity: r.Severity, Action: r.Action, Count: n, Sample: sample})
			if r.Action == ActionBlock {
				blocked = true
			}
		}
	}
	return flags, blocked
}

// Redact keeps a match's shape without its content: the first two and last two characters of anything
// longer than eight, the rest masked; at most 24 characters, single line.
func Redact(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	rs := []rune(s)
	if len(rs) <= 4 {
		return strings.Repeat("*", len(rs))
	}
	keep := 1
	if len(rs) > 8 {
		keep = 2
	}
	mid := len(rs) - 2*keep
	if mid > 20 {
		mid = 20
	}
	return string(rs[:keep]) + strings.Repeat("*", mid) + string(rs[len(rs)-keep:])
}

func digits(s string) []int {
	var d []int
	for _, c := range s {
		if c >= '0' && c <= '9' {
			d = append(d, int(c-'0'))
		}
	}
	return d
}

// luhn accepts 13-19 digit numbers passing the Luhn checksum (and not all one digit).
func luhn(s string) bool {
	d := digits(s)
	if len(d) < 13 || len(d) > 19 {
		return false
	}
	same := true
	sum := 0
	for i := range d {
		v := d[len(d)-1-i]
		if i%2 == 1 {
			v *= 2
			if v > 9 {
				v -= 9
			}
		}
		sum += v
		if d[i] != d[0] {
			same = false
		}
	}
	return sum%10 == 0 && !same
}

// ssn rejects area 000, 666 and 900-999, group 00 and serial 0000.
func ssn(s string) bool {
	d := digits(s)
	area := d[0]*100 + d[1]*10 + d[2]
	group := d[3]*10 + d[4]
	serial := d[5]*1000 + d[6]*100 + d[7]*10 + d[8]
	return area != 0 && area != 666 && area < 900 && group != 0 && serial != 0
}

// iban verifies the mod-97 checksum.
func iban(s string) bool {
	s = strings.ReplaceAll(s, " ", "")
	if len(s) < 15 || len(s) > 34 {
		return false
	}
	s = s[4:] + s[:4]
	rem := 0
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A'+10)) % 97
		default:
			return false
		}
	}
	return rem == 1
}

// keywordRegexp matches any of words case-insensitively as whole words; nil when words is empty.
// A keyword's edge only needs a word boundary where it is a word character ("c++", "tl;dr").
func keywordRegexp(words []string) *regexp.Regexp {
	alts := make([]string, 0, len(words))
	for _, k := range words {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		alt := regexp.QuoteMeta(k)
		if isWord(k[0]) {
			alt = `\b` + alt
		}
		if isWord(k[len(k)-1]) {
			alt += `\b`
		}
		alts = append(alts, alt)
	}
	if len(alts) == 0 {
		return nil
	}
	return regexp.MustCompile(`(?i)(?:` + strings.Join(alts, "|") + `)`)
}

func isWord(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

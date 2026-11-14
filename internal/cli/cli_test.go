package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestExitCodes(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	cases := map[string]struct {
		args []string
		code int
	}{
		"ok":          {[]string{"version"}, 0},
		"usage":       {[]string{"run"}, 2},
		"unknown cmd": {[]string{"frobnicate"}, 2},
		"not found":   {[]string{"stop", "nope"}, 3},
		"bad flag":    {[]string{"ps", "--nope"}, 2},
		"no engine":   {[]string{"engine", "show", "nope"}, 3},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := Main(append([]string{"--json"}, c.args...)); got != c.code {
				t.Fatalf("exit %d, want %d", got, c.code)
			}
		})
	}
}

func TestSchemaCoversEveryCommand(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range schemas() {
		if seen[s.Command] {
			t.Fatalf("duplicate %s", s.Command)
		}
		seen[s.Command] = true
	}
	for _, want := range []string{"run", "ensure", "chat", "model add", "engine set", "usage", "requests", "mcp"} {
		if !seen[want] {
			t.Errorf("schema lacks %q", want)
		}
	}
	if seen["models"] || seen["engines"] {
		t.Error("aliases leaked into the schema")
	}
}

func TestToolArgv(t *testing.T) {
	var run mcpTool
	for _, tl := range tools(true) {
		if tl.Name == "emx_run" {
			run = tl
		}
	}
	if run.argv == nil {
		t.Fatal("no emx_run tool")
	}
	argv, err := run.argv(map[string]any{"args": []any{"m"}, "ctx": float64(4096), "flash-attn": true, "engine_args": []any{"--x", "1"}})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(argv, " ")
	if got != "run --ctx=4096 --flash-attn=true m -- --x 1" {
		t.Fatalf("argv %q", got)
	}
	if _, err := run.argv(map[string]any{"bogus": 1}); err == nil {
		t.Fatal("unknown argument accepted")
	}
}

// Without --allow-host an agent gets no input that chooses what the host runs or which files it reads.
func TestMCPToolsLeaveOutHostInputsByDefault(t *testing.T) {
	byName := map[string]mcpTool{}
	for _, tl := range tools(false) {
		byName[tl.Name] = tl
	}
	for _, name := range []string{"emx_engine_add", "emx_engine_set"} {
		if _, ok := byName[name]; ok {
			t.Errorf("%s is offered without --allow-host", name)
		}
	}
	props := func(tool string) map[string]any {
		tl, ok := byName[tool]
		if !ok {
			t.Fatalf("no %s tool", tool)
		}
		return tl.InputSchema["properties"].(map[string]any)
	}
	for tool, hidden := range map[string][]string{
		"emx_run":          {"engine_args", "trust-remote-code"},
		"emx_ensure":       {"engine_args", "trust-remote-code"},
		"emx_model_add":    {"engine_args", "var", "pre-arg", "trust-remote-code"},
		"emx_bench_run":    {"prompt-file"},
		"emx_bench_config": {"hardware"},
		"emx_policy_test":  {"file"},
	} {
		p := props(tool)
		for _, name := range hidden {
			if _, ok := p[name]; ok {
				t.Errorf("%s offers %q without --allow-host", tool, name)
			}
		}
	}
	if _, ok := props("emx_run")["ctx"]; !ok {
		t.Error("emx_run lost its ordinary flags")
	}
	for _, in := range []map[string]any{
		{"args": []any{"m"}, "engine_args": []any{"--x"}},
		{"args": []any{"m"}, "trust-remote-code": true},
		// The same inputs dressed as positionals, which are spliced into the command line.
		{"args": []any{"m", "--", "--directory", "/etc"}},
		{"args": []any{"--trust-remote-code", "m"}},
		{"args": []any{"m", "--var=image=x"}},
	} {
		if _, err := byName["emx_run"].argv(in); err == nil {
			t.Errorf("emx_run accepted %v without --allow-host", in)
		}
	}
	all := map[string]bool{}
	for _, tl := range tools(true) {
		all[tl.Name] = true
	}
	if !all["emx_engine_add"] || !all["emx_engine_set"] {
		t.Error("--allow-host does not offer the engine tools")
	}
}

func TestMCPSession(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"emx_version","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"emx_stop","arguments":{"args":["nope"]}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"nope"}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := serveMCP(context.Background(), strings.NewReader(in), &out, io.Discard, false); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	sc := bufio.NewScanner(&out)
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		byID[string(mustJSON(m["id"]))] = m
	}
	if len(byID) != 5 {
		t.Fatalf("want 5 responses (notification unanswered), got %d: %s", len(byID), out.String())
	}
	tools := byID["2"]["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 10 {
		t.Fatalf("only %d tools", len(tools))
	}
	ok := byID["3"]["result"].(map[string]any)
	if ok["isError"] != false || !strings.Contains(text(ok), `"spec"`) {
		t.Fatalf("version call: %v", ok)
	}
	bad := byID["4"]["result"].(map[string]any)
	if bad["isError"] != true || !strings.Contains(text(bad), `"code":"not_found"`) {
		t.Fatalf("stop call: %v", bad)
	}
	if byID["5"]["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Fatalf("unknown method: %v", byID["5"])
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func text(res map[string]any) string {
	return res["content"].([]any)[0].(map[string]any)["text"].(string)
}

// Text other people chose (a model name in a request, a name inside a model file) reaches the operator's
// terminal and spreadsheets as text: no control sequences, no extra rows, no formulas.
func TestOutputNeutralisesControlSequencesAndFormulas(t *testing.T) {
	var out bytes.Buffer
	e := &env{stdout: &out}
	e.table("MODEL\tERROR", [][]string{{"m\x1b[2J", "bad\nFAKE\trow \u009b1A"}})
	got := out.String()
	if strings.ContainsAny(got, "\x1b\u009b") || strings.Count(got, "\n") != 2 || !strings.Contains(got, `m\x1b[2J`) {
		t.Fatalf("table output: %q", got)
	}
	out.Reset()
	e.out(nil, func(w io.Writer) { fmt.Fprintf(w, "arch: %s\n", "llama\x07\x1b]0;title\x07") })
	if got := out.String(); strings.ContainsAny(got, "\x1b\x07") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("human output: %q", got)
	}
	out.Reset()
	e.csv = true
	e.table("CLIENT\tTOKENS", [][]string{{`=HYPERLINK("http://x","y")`, "-12"}, {"@cmd", "+3.5"}, {"-x", "7"}, {"nobody", "-"}})
	want := "CLIENT,TOKENS\n\"'=HYPERLINK(\"\"http://x\"\",\"\"y\"\")\",-12\n'@cmd,+3.5\n'-x,7\nnobody,-\n"
	if out.String() != want {
		t.Fatalf("csv:\n%s\nwant:\n%s", out.String(), want)
	}
}

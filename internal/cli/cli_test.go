package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestExitCodes(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
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
	for _, tl := range tools() {
		if tl.Name == "substrate_run" {
			run = tl
		}
	}
	if run.argv == nil {
		t.Fatal("no substrate_run tool")
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

func TestMCPSession(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"substrate_version","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"substrate_stop","arguments":{"args":["nope"]}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"nope"}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := serveMCP(context.Background(), strings.NewReader(in), &out, io.Discard); err != nil {
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

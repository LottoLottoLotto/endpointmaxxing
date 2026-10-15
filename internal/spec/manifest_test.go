package spec

import (
	"strings"
	"testing"
)

func TestBuiltinManifestsValid(t *testing.T) {
	ms, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("no builtin manifests")
	}
}

const minimal = `
spec: substrate.engine/v1
id: toy
name: Toy
binary: {candidates: [toy-server]}
launch: {command: ["{bin}", --port, "{port}"]}
health: {path: /health}
capabilities: {formats: [gguf], quantizations: ["*"], apis: [chat]}
`

func TestParseRejects(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"no port": {
			strings.Replace(minimal, `--port, "{port}"`, `--x`, 1), "must place {port}",
		},
		"unknown placeholder": {
			strings.Replace(minimal, `"{bin}"`, `"{binary}"`, 1), "unknown placeholder {binary}",
		},
		"unknown flag": {
			minimal + "flags: {context: [-c, \"{value}\"]}\n", "not a unified flag",
		},
		"valued flag without value": {
			minimal + "flags: {ctx: [-c]}\n", "must place {value}",
		},
		"unknown api": {
			strings.Replace(minimal, "apis: [chat]", "apis: [chat, speech]", 1), `"speech" unknown`,
		},
		"unknown field": {
			minimal + "lanuch: {}\n", "field lanuch not found",
		},
		"priority for unsupported format": {
			minimal + "routing: {priority: {safetensors: 5}}\n", "format not in capabilities.formats",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml), "test")
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestParseAcceptsShorthandAndEnvPlaceholders(t *testing.T) {
	y := minimal + `flags:
  ctx: [-c, "{value}"]
  gpus: {env: {CUDA_VISIBLE_DEVICES: "{value}"}}
  flash_attn: [-fa, "on"]
`
	y = strings.Replace(y, "health:", "vars: {p: \"{env.PATH}\"}\nhealth:", 1)
	m, err := Parse([]byte(y), "test")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Flags["ctx"].Args; len(got) != 2 || got[0] != "-c" {
		t.Fatalf("shorthand args: %v", got)
	}
}

func TestExpand(t *testing.T) {
	t.Setenv("SUBSTRATE_T", "/x")
	got, err := Expand("{a}:{env.SUBSTRATE_T}:{env.SUBSTRATE_UNSET}", map[string]string{"a": "1"})
	if err != nil || got != "1:/x:" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := Expand("{missing}", nil); err == nil {
		t.Fatal("unknown placeholder accepted")
	}
}

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
spec: emx.engine/v1
id: toy
name: Toy
binary: {candidates: [toy-server]}
launch: {command: ["{bin}", --port, "{port}"]}
health: {path: /health}
capabilities: {formats: [gguf], quantizations: ["*"], apis: [chat]}
`

const host = "host: {load: /admin/load, unload: /admin/unload, models: /admin/models}\n"

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
		"host without paths": {
			minimal + "host: {load: /admin/load}\n", "host.unload: required",
		},
		"host launch naming a model": {
			strings.Replace(minimal, `"{bin}"`, `"{bin}", -m, "{model_path}"`, 1) + host, "per-model placeholder {model_path}",
		},
		"host with per-model flags": {
			minimal + host + "flags: {ctx: [-c, \"{value}\"]}\n", "takes no per-model flags",
		},
		"api_key_env that is no variable name": {
			strings.Replace(minimal, `"{port}"]`, `"{port}"], api_key_env: "TOY KEY"`, 1), "launch.api_key_env",
		},
		"host with api_key_env": {
			strings.Replace(minimal, `"{port}"]`, `"{port}"], api_key_env: TOY_API_KEY`, 1) + host, "launch.api_key_env: a multi-model host",
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
	t.Setenv("EMX_T", "/x")
	got, err := Expand("{a}:{env.EMX_T}:{env.EMX_UNSET}", map[string]string{"a": "1"})
	if err != nil || got != "1:/x:" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := Expand("{missing}", nil); err == nil {
		t.Fatal("unknown placeholder accepted")
	}
}

// launch.api_key_env moves the engine API key from the command line to the engine's environment.
func TestAPIKeyEnvReplacesTheArgumentMapping(t *testing.T) {
	y := minimal + "flags: {api_key: [--api-key, \"{value}\"]}\n"
	m, err := Parse([]byte(y), "test")
	if err != nil {
		t.Fatal(err)
	}
	if fm, ok := m.FlagMapFor("api_key"); !ok || len(fm.Args) != 2 || len(fm.Env) != 0 {
		t.Fatalf("argv mapping: %+v %v", fm, ok)
	}
	m, err = Parse([]byte(strings.Replace(y, `"{port}"]`, `"{port}"], api_key_env: TOY_API_KEY`, 1)), "test")
	if err != nil {
		t.Fatal(err)
	}
	if fm, ok := m.FlagMapFor("api_key"); !ok || len(fm.Args) != 0 || fm.Env["TOY_API_KEY"] != "{value}" {
		t.Fatalf("env mapping: %+v %v", fm, ok)
	}
	if _, ok := m.FlagMapFor("ctx"); ok {
		t.Fatal("a flag the manifest does not map was reported as supported")
	}
}

// Flags that let their value pick code or files on the host are marked, so the settings API can refuse them.
func TestHostOnlyFlags(t *testing.T) {
	for _, f := range Unified {
		if want := f.Key == "trust_remote_code"; f.HostOnly != want {
			t.Errorf("%s: HostOnly = %v, want %v", f.Key, f.HostOnly, want)
		}
	}
}

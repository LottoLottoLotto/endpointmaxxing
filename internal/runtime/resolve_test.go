package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/spec"
)

func engine(t *testing.T, id string, ov config.EngineOverride) *Engine {
	t.Helper()
	ms, err := spec.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.ID == id {
			bin := filepath.Join(t.TempDir(), "bin", "server")
			os.MkdirAll(filepath.Dir(bin), 0o755)
			os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
			ov.Bin = bin
			e := Resolve(m, ov)
			if !e.Installed() {
				t.Fatalf("fake binary did not resolve: %v", e.Tried)
			}
			return e
		}
	}
	t.Fatalf("no builtin %s", id)
	return nil
}

func req(flags map[string]string) Request {
	return Request{
		Name: "m", Host: "0.0.0.0", Port: 9000, Flags: flags,
		Model: ModelRef{Source: "org/m", Path: "/models/m", Ref: "/models/m", Format: "safetensors"},
	}
}

func TestBuildTranslatesUnifiedFlags(t *testing.T) {
	e := engine(t, "vllm", config.EngineOverride{Args: []string{"--enforce-eager"}})
	r := req(map[string]string{"ctx": "4096", "kv_cache_type": "q8_0", "gpus": "1", "trust_remote_code": "false"})
	r.Args = []string{"--seed", "1"}
	p, err := e.Build(r)
	if err != nil {
		t.Fatal(err)
	}
	tail := p.Argv[len(p.Argv)-7:]
	want := []string{"--max-model-len", "4096", "--kv-cache-dtype", "fp8", "--enforce-eager", "--seed", "1"}
	if !reflect.DeepEqual(tail, want) {
		t.Fatalf("argv tail = %v, want %v", tail, want)
	}
	if p.Env["CUDA_VISIBLE_DEVICES"] != "1" {
		t.Fatalf("gpus not mapped to env: %v", p.Env)
	}
	if !strings.HasPrefix(p.Env["PATH"], filepath.Dir(e.Bin)+":") {
		t.Fatalf("venv bin not prepended to PATH: %q", p.Env["PATH"])
	}
	// A wildcard bind is probed on loopback.
	if p.HealthURL != "http://127.0.0.1:9000/health" {
		t.Fatalf("health url %s", p.HealthURL)
	}
}

func TestBuildUnsupportedFlag(t *testing.T) {
	e := engine(t, "llama.cpp", config.EngineOverride{})
	r := req(map[string]string{"tp": "2", "ctx": "8192"})
	if _, err := e.Build(r); err == nil || !strings.Contains(err.Error(), "--tp") {
		t.Fatalf("want unsupported --tp error, got %v", err)
	}
	r.Lenient = true
	p, err := e.Build(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 1 || strings.Contains(strings.Join(p.Argv, " "), "tensor") {
		t.Fatalf("lenient build: warnings %v argv %v", p.Warnings, p.Argv)
	}
}

func TestBuildNeedsLocalFileForPathEngines(t *testing.T) {
	e := engine(t, "llama.cpp", config.EngineOverride{})
	r := req(nil)
	r.Model.Path = ""
	if _, err := e.Build(r); err == nil || !strings.Contains(err.Error(), "local model file") {
		t.Fatalf("want local-file error, got %v", err)
	}
}

func TestStrataVarsOverride(t *testing.T) {
	e := engine(t, "strata", config.EngineOverride{Vars: map[string]string{"server": "tt/serve/server_tt.py"}})
	r := req(nil)
	r.Model.Format = "strata-config"
	p, err := e.Build(r)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(e.Bin)))
	if p.Argv[1] != filepath.Join(root, "tt/serve/server_tt.py") || p.Cwd != root {
		t.Fatalf("argv[1]=%s cwd=%s root=%s", p.Argv[1], p.Cwd, root)
	}
}

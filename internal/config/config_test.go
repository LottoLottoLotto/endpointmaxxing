package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentUpdatesKeepEveryModel(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := UpdateModels(func(m *Models) error {
				m.Put(Model{Name: fmt.Sprintf("m%02d", i), Source: "/x", Format: "gguf"})
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	m, err := LoadModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Models) != 20 {
		t.Fatalf("lost updates: %d of 20 models", len(m.Models))
	}
}

func TestStrictFiles(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	os.MkdirAll(filepath.Dir(ConfigPath()), 0o755)

	os.WriteFile(ConfigPath(), []byte("gateway:\n  prot: 8000\n"), 0o644)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "prot") {
		t.Fatalf("typo accepted: %v", err)
	}
	os.WriteFile(ConfigPath(), []byte("gateway:\n  port: 8000\n"), 0o644)
	c, err := Load()
	if err != nil || c.Gateway.Port != 8000 || !c.Gateway.Autostart || !c.Logging.Enabled {
		t.Fatalf("partial config must keep defaults: %+v %v", c, err)
	}

	os.WriteFile(ModelsPath(), []byte("models:\n  - {name: a, source: /x, format: gguf, flags: {ctx: big}}\n"), 0o644)
	if _, err := LoadModels(); err == nil || !strings.Contains(err.Error(), "not an integer") {
		t.Fatalf("bad flag value accepted: %v", err)
	}
	os.WriteFile(ModelsPath(), []byte("models:\n  - {name: a, source: /x, format: gguf, flags: {context: 4}}\n"), 0o644)
	if _, err := LoadModels(); err == nil || !strings.Contains(err.Error(), "not a unified flag") {
		t.Fatalf("unknown flag accepted: %v", err)
	}
	os.WriteFile(ModelsPath(), []byte("models:\n  - {name: a, source: /x, format: gguf, origin: not-a-repo}\n"), 0o644)
	if _, err := LoadModels(); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("bad origin accepted: %v", err)
	}
	os.WriteFile(ModelsPath(), []byte("models:\n  - {name: a, source: /x, format: gguf, origin: unsloth/Qwen3-0.6B-GGUF@50968a4468}\n"), 0o644)
	m, err := LoadModels()
	if err != nil {
		t.Fatal(err)
	}
	if repo, rev, err := m.Models[0].OriginRef(); repo != "unsloth/Qwen3-0.6B-GGUF" || rev != "50968a4468" || err != nil {
		t.Fatalf("origin: %q %q %v", repo, rev, err)
	}
}

func TestBodyLimits(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	os.MkdirAll(filepath.Dir(ConfigPath()), 0o755)
	if c, err := Load(); err != nil || c.Gateway.MaxBodyMB != 64 || c.Gateway.MaxBufferedMB != 1024 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	for yaml, ok := range map[string]bool{
		"gateway:\n  max_body_mb: 256\n":                      true,
		"gateway:\n  max_body_mb: 257\n":                      false, // over the hard bound
		"gateway:\n  max_body_mb: 0\n":                        false,
		"gateway:\n  max_body_mb: 8\n  max_buffered_mb: 4\n":  false, // no body of the allowed size would fit
		"gateway:\n  max_body_mb: 8\n  max_buffered_mb: 64\n": true,
	} {
		os.WriteFile(ConfigPath(), []byte(yaml), 0o644)
		if _, err := Load(); (err == nil) != ok {
			t.Errorf("%q: %v, want accepted=%v", yaml, err, ok)
		}
	}
}

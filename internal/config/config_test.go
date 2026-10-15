package config

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentUpdatesKeepEveryModel(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
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
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	os.MkdirAll(dirOf(ConfigPath()), 0o755)

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
}

func dirOf(p string) string { return p[:strings.LastIndexByte(p, '/')] }

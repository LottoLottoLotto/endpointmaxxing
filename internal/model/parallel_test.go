package model

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeclaredParallelOfAStrataConfig(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		body  string
		want  int
		known bool
	}{
		"one-at-a-time.json": {`{"exe":"strata","args":["-m","x"]}`, 1, true},
		"four.json":          {`{"exe":"strata","args":["-m","x"],"parallel":4}`, 4, true},
		"bad-value.json":     {`{"exe":"strata","args":["-m","x"],"parallel":"many"}`, 1, true},
		"other.json":         {`{"something":"else"}`, 0, false},
		"weights.gguf":       {`GGUF`, 0, false},
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if n, known := DeclaredParallel(p); n != tc.want || known != tc.known {
			t.Errorf("%s: %d known=%v, want %d known=%v", name, n, known, tc.want, tc.known)
		}
	}
}

func TestStrataConfigWhoseWeightsAreGoneIsMissing(t *testing.T) {
	dir := t.TempDir()
	weights := filepath.Join(dir, "w.gguf")
	os.WriteFile(weights, []byte("GGUF"), 0o600)
	cfg := filepath.Join(dir, "strata-x.json")
	os.WriteFile(cfg, []byte(`{"exe":"/bin/engine","args":["-m","`+filepath.ToSlash(weights)+`","--threads","8"]}`), 0o600)
	if MissingLocal(cfg) {
		t.Fatal("config with its weights in place reported missing")
	}
	os.Remove(weights)
	if !MissingLocal(cfg) {
		t.Fatal("config whose weights were deleted not reported missing")
	}
}

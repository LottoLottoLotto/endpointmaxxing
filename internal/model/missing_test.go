package model

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMissingLocalSeparatesGoneFilesFromRepoIDs(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "m.gguf")
	os.WriteFile(present, []byte("x"), 0o600)
	for source, want := range map[string]bool{
		present:                         false,
		filepath.Join(dir, "gone.gguf"): true,
		filepath.Join(dir, "gone-dir"):  true,
		"LiquidAI/LFM3-1.7B":            false, // a repo id downloads on first use
		"./org/name":                    true,  // relative path, not a repo id
	} {
		if got := MissingLocal(source); got != want {
			t.Errorf("MissingLocal(%q) = %v, want %v", source, got, want)
		}
	}
}

package model

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// A missing source is reported as the caller wrote it: the expanded path would disclose the environment.
func TestInspectMissingSourceIsReportedAsWritten(t *testing.T) {
	t.Setenv("EMX_T_SECRET", "hunter2")
	const source = "/nope/$EMX_T_SECRET/m.gguf"
	_, err := Inspect(source)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want a not-exist error, got %v", err)
	}
	if msg := err.Error(); strings.Contains(msg, "hunter2") || !strings.Contains(msg, source) {
		t.Fatalf("error must name the source as written: %s", msg)
	}
}

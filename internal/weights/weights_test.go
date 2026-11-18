package weights

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func ready(t *testing.T, tr *Tracker, src string) Report {
	t.Helper()
	r := tr.Fingerprint(src, nil)
	if r.Status != StatusReady {
		t.Fatalf("%s: %+v, want ready", src, r)
	}
	return r
}

// The id depends on the weights' content only: copies under other names have the same id, one changed
// byte gives another. Small files carry the SHA-1 git (and Hugging Face) reports for them.
func TestIDIsContentNotNames(t *testing.T) {
	dir := t.TempDir()
	weights := strings.Repeat("weights ", 1000)
	a := filepath.Join(dir, "a")
	write(t, filepath.Join(a, "model-00001-of-00002.safetensors"), weights)
	write(t, filepath.Join(a, "model-00002-of-00002.safetensors"), "second shard")
	config := write(t, filepath.Join(a, "config.json"), `{"architectures": ["Qwen3ForCausalLM"]}`)
	write(t, filepath.Join(a, "README.md"), "not part of it")
	write(t, filepath.Join(a, ".cache", "huggingface", "x.json"), "not part of it")
	b := filepath.Join(dir, "b")
	write(t, filepath.Join(b, "renamed.safetensors"), "second shard")
	write(t, filepath.Join(b, "sub", "other.safetensors"), weights)
	write(t, filepath.Join(b, "config.json"), `{"different": "config"}`)
	c := filepath.Join(dir, "c")
	write(t, filepath.Join(c, "model-00001-of-00002.safetensors"), "W"+weights[1:])
	write(t, filepath.Join(c, "model-00002-of-00002.safetensors"), "second shard")

	tr := New("")
	ra, rb, rc := ready(t, tr, a), ready(t, tr, b), ready(t, tr, c)
	if ra.ID != rb.ID {
		t.Errorf("same weights under other names: ids %s and %s", ra.ID, rb.ID)
	}
	if ra.ID == rc.ID {
		t.Errorf("one byte changed, same id %s", ra.ID)
	}
	var paths []string
	for _, f := range ra.Files {
		paths = append(paths, fmt.Sprintf("%s:%v", f.Path, f.Weight))
	}
	if got := strings.Join(paths, " "); got != "config.json:false model-00001-of-00002.safetensors:true model-00002-of-00002.safetensors:true" {
		t.Errorf("files: %s", got)
	}
	// The id is the sha256 of "<sha256> <bytes>\n" per weight file, sorted by sha256.
	var lines []string
	for _, content := range []string{weights, "second shard"} {
		sum := sha256.Sum256([]byte(content))
		lines = append(lines, fmt.Sprintf("%x %d\n", sum, len(content)))
	}
	if lines[0] > lines[1] {
		lines[0], lines[1] = lines[1], lines[0]
	}
	want := sha256.Sum256([]byte(lines[0] + lines[1]))
	if ra.ID != hex.EncodeToString(want[:]) {
		t.Errorf("id %s, want %x", ra.ID, want)
	}
	// A single-file source: the same weights, the same id.
	if r := ready(t, tr, filepath.Join(b, "sub", "other.safetensors")); len(r.Files) != 1 || r.Files[0].Path != "other.safetensors" {
		t.Errorf("single file: %+v", r)
	}

	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	out, err := exec.Command(git, "hash-object", config).Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range ra.Files {
		if f.Path == "config.json" && f.GitSHA1 != strings.TrimSpace(string(out)) {
			t.Errorf("config.json git_sha1 %s, git hash-object says %s", f.GitSHA1, out)
		}
	}
}

// File hashes are remembered on disk by path, size and modification time: another tracker (a restart)
// does not hash them again, and hashes a file again once it changed.
func TestCacheSkipsUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "state", "weights.json")
	m := write(t, filepath.Join(dir, "m.gguf"), "gguf weights")

	first := New(cache)
	r1 := ready(t, first, m)
	if n := first.hashed.Load(); n != 1 {
		t.Fatalf("first look hashed %d files, want 1", n)
	}
	ready(t, first, m)
	if n := first.hashed.Load(); n != 1 {
		t.Errorf("second look hashed again (%d)", n)
	}

	restarted := New(cache)
	if r := ready(t, restarted, m); r.ID != r1.ID || restarted.hashed.Load() != 0 {
		t.Errorf("after a restart: id %s (want %s), %d files hashed (want 0)", r.ID, r1.ID, restarted.hashed.Load())
	}

	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(m, later, later); err != nil {
		t.Fatal(err)
	}
	if r := ready(t, restarted, m); r.ID != r1.ID || restarted.hashed.Load() != 1 {
		t.Errorf("touched: id %s (want %s), %d files hashed (want 1)", r.ID, r1.ID, restarted.hashed.Load())
	}

	write(t, m, "other weights")
	os.Chtimes(m, later, later) // same time, other size
	if r := ready(t, restarted, m); r.ID == r1.ID || restarted.hashed.Load() != 2 {
		t.Errorf("rewritten: id %s (was %s), %d files hashed (want 2)", r.ID, r1.ID, restarted.hashed.Load())
	}
}

// A large file is hashed in the background: the first look says hashing, a later one ready.
func TestLargeFilesAreHashedInTheBackground(t *testing.T) {
	m := filepath.Join(t.TempDir(), "big.gguf")
	f, err := os.Create(m)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(inlineBelow) // sparse: fast to create, real to read
	f.Close()
	tr := New("")
	if r := tr.Fingerprint(m, nil); r.Status != StatusHashing || r.ID != "" {
		t.Fatalf("first look: %+v, want hashing", r)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		r := tr.Fingerprint(m, nil)
		if r.Status == StatusReady {
			if r.Files[0].GitSHA1 != "" || r.Files[0].Bytes != inlineBelow {
				t.Errorf("big file: %+v", r.Files[0])
			}
			break
		}
		if r.Status != StatusHashing || time.Now().After(deadline) {
			t.Fatalf("%+v", r)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for tr.busy() { // the job ends just after the hashes are stored
		time.Sleep(time.Millisecond)
	}
}

func (t *Tracker) busy() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, j := range t.jobs {
		if j.running {
			return true
		}
	}
	return false
}

// A source in the Hugging Face cache names its repo and commit; the files are read through the symlinks
// into blobs/ but keep their names. A repo id source is found in the cache.
func TestOriginFromHuggingFaceCache(t *testing.T) {
	hub := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hub)
	rev := strings.Repeat("ab", 20)
	base := filepath.Join(hub, "models--unsloth--Qwen3-0.6B-GGUF")
	write(t, filepath.Join(base, "blobs", "0123"), "q8 weights")
	snap := filepath.Join(base, "snapshots", rev)
	os.MkdirAll(snap, 0o755)
	if err := os.Symlink(filepath.Join("..", "..", "blobs", "0123"), filepath.Join(snap, "Qwen3-0.6B-Q8_0.gguf")); err != nil {
		t.Skip("no symlinks:", err)
	}
	write(t, filepath.Join(base, "refs", "main"), rev+"\n")

	tr := New("")
	want := Origin{Repo: "unsloth/Qwen3-0.6B-GGUF", Revision: rev}
	for _, src := range []string{filepath.Join(snap, "Qwen3-0.6B-Q8_0.gguf"), snap, "unsloth/Qwen3-0.6B-GGUF"} {
		r := ready(t, tr, src)
		if r.Origin == nil || *r.Origin != want {
			t.Errorf("%s: origin %+v", src, r.Origin)
		}
		if len(r.Files) != 1 || r.Files[0].Path != "Qwen3-0.6B-Q8_0.gguf" || r.Files[0].Bytes != int64(len("q8 weights")) {
			t.Errorf("%s: files %+v", src, r.Files)
		}
	}
	// A configured origin is reported as is.
	given := &Origin{Repo: "someone/else"}
	if r := tr.Fingerprint(snap, given); r.Origin != given {
		t.Errorf("configured origin: %+v", r.Origin)
	}
	if r := tr.Fingerprint("unsloth/not-downloaded", nil); r.Status != StatusError || r.Origin != nil {
		t.Errorf("repo not downloaded: %+v", r)
	}
}

// An unreadable file is an error that says which file, without the absolute path.
func TestUnreadableFileIsAnError(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes do not deny reading here")
	}
	dir := t.TempDir()
	m := write(t, filepath.Join(dir, "m", "model.safetensors"), "weights")
	os.Chmod(m, 0)
	r := New("").Fingerprint(filepath.Dir(m), nil)
	if r.Status != StatusError || !strings.Contains(r.Error, "model.safetensors") || strings.Contains(r.Error, dir) {
		t.Errorf("%+v", r)
	}
}

package audit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func write(t *testing.T, l *Log, n int, tag string) {
	t.Helper()
	for i := range n {
		l.Write(map[string]any{"type": "admin", "detail": fmt.Sprintf("%s %d %s", tag, i, strings.Repeat("x", 200))})
	}
}

func mustVerify(t *testing.T, p string) *Report {
	t.Helper()
	r, err := Verify(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestChainSurvivesRotationAndDetectsTampering(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	l := Open(p, 1, t.Logf)
	l.maxBytes = 4 << 10 // rotate every ~16 lines
	write(t, l, 60, "change")
	l.Close()
	if len(Rotated(p)) < 3 {
		t.Fatalf("expected rotations, got %v", Rotated(p))
	}
	if r := mustVerify(t, p); !r.OK || r.Lines != 60 || r.Anchored {
		t.Fatalf("intact chain: %+v", r)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("audit file mode %v", st.Mode().Perm())
	}

	mid := Rotated(p)[1]
	orig, _ := os.ReadFile(mid)
	for name, tamper := range map[string]func([]byte) []byte{
		"edit":   func(b []byte) []byte { return bytes.Replace(b, []byte("change 2"), []byte("chanGe 2"), 1) },
		"delete": func(b []byte) []byte { i := bytes.IndexByte(b, '\n'); return b[i+1:] },
		"insert": func(b []byte) []byte {
			i := bytes.IndexByte(b, '\n')
			return append(append(append([]byte{}, b[:i+1]...), []byte(`{"type":"admin","detail":"forged"}`+"\n")...), b[i+1:]...)
		},
	} {
		os.WriteFile(mid, tamper(append([]byte{}, orig...)), 0o600)
		if r := mustVerify(t, p); r.OK {
			t.Errorf("%s went undetected", name)
		}
		os.WriteFile(mid, orig, 0o600)
	}
	if r := mustVerify(t, p); !r.OK {
		t.Fatalf("restored chain: %+v", r)
	}

	// Retention removes the oldest file: what remains still verifies, anchored at its first line.
	if _, err := PruneRotated(p, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	l = Open(p, 1, t.Logf)
	write(t, l, 3, "after")
	l.Close()
	if r := mustVerify(t, p); !r.OK || !r.Anchored {
		t.Fatalf("after pruning rotated files: %+v", r)
	}
}

// Lines from before chaining existed stay valid; the chain starts after them.
func TestLegacyLinesThenChain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	os.WriteFile(p, []byte(`{"type":"request","id":"a"}`+"\n"+`{"type":"request","id":"b"}`+"\n"), 0o644)
	l := Open(p, 0, t.Logf)
	write(t, l, 2, "new")
	l.Close()
	r := mustVerify(t, p)
	if !r.OK || r.Unchained != 2 || r.Lines != 4 {
		t.Fatalf("%+v", r)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("an existing wider file was not tightened: %v", st.Mode().Perm())
	}
	// Editing the last legacy line breaks the first chained line's link.
	data, _ := os.ReadFile(p)
	os.WriteFile(p, bytes.Replace(data, []byte(`"id":"b"`), []byte(`"id":"B"`), 1), 0o600)
	if mustVerify(t, p).OK {
		t.Fatal("edit of the anchoring legacy line went undetected")
	}
}

// Writers in different processes (here: separate Logs) interleave without forking the chain.
func TestConcurrentWritersKeepOneChain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			l := Open(p, 1, t.Logf)
			l.maxBytes = 8 << 10
			write(t, l, 50, fmt.Sprintf("w%d", w))
			l.Close()
		}(w)
	}
	wg.Wait()
	if r := mustVerify(t, p); !r.OK || r.Lines != 200 {
		t.Fatalf("%+v", r)
	}
}

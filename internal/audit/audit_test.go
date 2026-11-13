package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LottoLottoLotto/substrate/internal/paths"
)

func open(t *testing.T, p string, maxMB int, keyFile string) *Log {
	t.Helper()
	l, err := Open(p, maxMB, keyFile, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func write(t *testing.T, l *Log, n int, tag string) {
	t.Helper()
	for i := range n {
		l.Write(map[string]any{"type": "admin", "detail": fmt.Sprintf("%s %d %s", tag, i, strings.Repeat("x", 200))})
	}
}

func mustVerify(t *testing.T, p string, c Check) *Report {
	t.Helper()
	r, err := Verify(p, c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// keyFile writes an audit key file as an operator would (mode 0600) and returns its path and the key.
func keyFile(t *testing.T) (string, []byte) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "audit.key")
	secret := strings.Repeat("k", MinKeyBytes)
	if err := os.WriteFile(p, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, []byte(secret)
}

func TestChainSurvivesRotationAndDetectsTampering(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	l := open(t, p, 1, "")
	l.maxBytes = 4 << 10 // rotate every dozen lines or so
	write(t, l, 60, "change")
	l.Close()
	if len(Rotated(p)) < 3 {
		t.Fatalf("expected rotations, got %v", Rotated(p))
	}
	r := mustVerify(t, p, Check{})
	if !r.OK || r.Lines != 60 || r.Anchored || r.Keyed || len(r.Warnings) != 0 {
		t.Fatalf("intact chain: %+v", r)
	}
	// The head anchor follows the active file through the rotations.
	if a, err := readHead(p); err != nil || a == nil || *a != r.Head || a.Count != 60 || a.File != "audit.jsonl" {
		t.Fatalf("head anchor %+v (%v), verified head %+v", a, err, r.Head)
	}
	for _, f := range []string{p, headPath(p)} {
		if st, err := os.Stat(f); err != nil {
			t.Error(err)
		} else if !paths.OwnerOnly(st.Mode()) {
			t.Errorf("%s: mode %v", f, st.Mode().Perm())
		}
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
		if r := mustVerify(t, p, Check{}); r.OK {
			t.Errorf("%s went undetected", name)
		}
		os.WriteFile(mid, orig, 0o600)
	}
	if r := mustVerify(t, p, Check{}); !r.OK {
		t.Fatalf("restored chain: %+v", r)
	}

	// Retention removes the oldest file: what remains still verifies, anchored at its first line.
	if _, err := PruneRotated(p, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	l = open(t, p, 1, "")
	write(t, l, 3, "after")
	l.Close()
	if r := mustVerify(t, p, Check{}); !r.OK || !r.Anchored || r.Head.Count != 63 {
		t.Fatalf("after pruning rotated files: %+v", r)
	}
}

// Lines from before chaining existed stay valid; the chain starts after them.
func TestLegacyLinesThenChain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	os.WriteFile(p, []byte(`{"type":"request","id":"a"}`+"\n"+`{"type":"request","id":"b"}`+"\n"), 0o644)
	l := open(t, p, 0, "")
	write(t, l, 2, "new")
	l.Close()
	r := mustVerify(t, p, Check{})
	if !r.OK || r.Unchained != 2 || r.Lines != 4 || r.Head.Count != 2 {
		t.Fatalf("%+v", r)
	}
	if st, _ := os.Stat(p); !paths.OwnerOnly(st.Mode()) {
		t.Errorf("an existing wider file was not tightened: %v", st.Mode().Perm())
	}
	// Editing the last legacy line breaks the first chained line's link.
	data, _ := os.ReadFile(p)
	os.WriteFile(p, bytes.Replace(data, []byte(`"id":"b"`), []byte(`"id":"B"`), 1), 0o600)
	if mustVerify(t, p, Check{}).OK {
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
			l := open(t, p, 1, "")
			l.maxBytes = 8 << 10
			write(t, l, 50, fmt.Sprintf("w%d", w))
			l.Close()
		}(w)
	}
	wg.Wait()
	if r := mustVerify(t, p, Check{}); !r.OK || r.Lines != 200 || r.Head.Count != 200 || len(r.Warnings) != 0 {
		t.Fatalf("%+v", r)
	}
}

// Lines cut off the end leave a chain that is consistent in itself: the head anchor still names the line
// the file ended with, and a later write does not mend the break.
func TestTailTruncationDetected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	l := open(t, p, 0, "")
	write(t, l, 10, "change")
	l.Close()
	if r := mustVerify(t, p, Check{}); !r.OK || r.Head.Count != 10 || len(r.Warnings) != 0 {
		t.Fatalf("intact chain: %+v", r)
	}
	data, _ := os.ReadFile(p)
	os.WriteFile(p, bytes.Join(bytes.SplitAfter(data, []byte("\n"))[:7], nil), 0o600)
	if r := mustVerify(t, p, Check{}); r.OK || !strings.Contains(r.Problem, "3 line(s) were removed from the tail") {
		t.Fatalf("truncated tail: %+v", r)
	}
	l = open(t, p, 0, "")
	write(t, l, 2, "after")
	l.Close()
	if r := mustVerify(t, p, Check{}); r.OK || !strings.Contains(r.Problem, "chain broken") {
		t.Fatalf("truncated tail after further writes: %+v", r)
	}
}

// A file from before head anchors (or one whose anchor was removed) verifies with a warning; the next
// write gives it an anchor.
func TestMissingAnchorIsAWarning(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	l := open(t, p, 0, "")
	write(t, l, 3, "change")
	l.Close()
	if err := os.Remove(headPath(p)); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, p, Check{}); !r.OK || len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "no head anchor") {
		t.Fatalf("without anchor: %+v", r)
	}
	l = open(t, p, 0, "")
	write(t, l, 1, "after")
	l.Close()
	if r := mustVerify(t, p, Check{}); !r.OK || len(r.Warnings) != 0 || r.Head.Count != 4 {
		t.Fatalf("after the next write: %+v", r)
	}
}

// A write cut off between the line and its anchor leaves the file one line ahead: that is not tampering,
// and the next writer carries on from the file.
func TestFileAheadOfAnchorIsAccepted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	l := open(t, p, 0, "")
	write(t, l, 3, "change")
	l.Close()
	anchor, err := os.ReadFile(headPath(p))
	if err != nil {
		t.Fatal(err)
	}
	l = open(t, p, 0, "")
	write(t, l, 1, "cut off")
	l.Close()
	os.WriteFile(headPath(p), anchor, 0o600)
	if r := mustVerify(t, p, Check{}); !r.OK || r.Head.Count != 4 {
		t.Fatalf("file one line ahead of its anchor: %+v", r)
	}
	l = open(t, p, 0, "")
	write(t, l, 1, "after")
	l.Close()
	if r := mustVerify(t, p, Check{}); !r.OK || r.Lines != 5 || r.Head.Count != 5 {
		t.Fatalf("after the next write: %+v", r)
	}
}

// A head kept elsewhere proves later that the chain still contains that line: a longer chain passes, a
// shorter or rewritten one does not, however consistent it is in itself.
func TestExpectedHead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	l := open(t, p, 0, "")
	write(t, l, 5, "change")
	l.Close()
	kept := mustVerify(t, p, Check{}).Head
	if kept.Count != 5 || kept.Hash == "" {
		t.Fatalf("head %+v", kept)
	}
	l = open(t, p, 0, "")
	write(t, l, 5, "more")
	l.Close()
	if r := mustVerify(t, p, Check{ExpectHead: kept.Hash, ExpectCount: kept.Count}); !r.OK || r.ExpectedAt != 5 || r.Head.Count != 10 {
		t.Fatalf("grown chain against the kept head: %+v", r)
	}
	if r := mustVerify(t, p, Check{ExpectHead: kept.Hash, ExpectCount: 4}); r.OK {
		t.Fatalf("kept head at another count: %+v", r)
	}
	if r := mustVerify(t, p, Check{ExpectCount: 11}); r.OK || !strings.Contains(r.Problem, "before the expected 11") {
		t.Fatalf("chain shorter than the expected count: %+v", r)
	}

	// The file and its anchor are replaced by a chain of other lines.
	os.Remove(p)
	os.Remove(headPath(p))
	l = open(t, p, 0, "")
	write(t, l, 10, "forged")
	l.Close()
	if r := mustVerify(t, p, Check{}); !r.OK || r.Head.Count != 10 {
		t.Fatalf("the replacement is not consistent in itself: %+v", r)
	}
	if r := mustVerify(t, p, Check{ExpectHead: kept.Hash, ExpectCount: kept.Count}); r.OK || !strings.Contains(r.Problem, "expected head") {
		t.Fatalf("replaced chain against the kept head: %+v", r)
	}
}

// With an audit key a file rewritten from scratch, every hash and the anchor recomputed, does not verify:
// its lines lack the macs only the key makes.
func TestRewriteWithoutKeyDetected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	kf, key := keyFile(t)
	l := open(t, p, 0, kf)
	write(t, l, 10, "change")
	l.Close()
	if r := mustVerify(t, p, Check{Key: key}); !r.OK || !r.Keyed || r.Head.Count != 10 || len(r.Warnings) != 0 {
		t.Fatalf("keyed chain: %+v", r)
	}
	if r := mustVerify(t, p, Check{}); r.OK || !strings.Contains(r.Problem, "no audit key") {
		t.Fatalf("keyed chain without its key: %+v", r)
	}
	if r := mustVerify(t, p, Check{Key: bytes.Repeat([]byte("x"), MinKeyBytes)}); r.OK || !strings.Contains(r.Problem, "mac does not match") {
		t.Fatalf("keyed chain with another key: %+v", r)
	}

	// The forger drops the first line, changes the others and chains them with plain hashes.
	data, _ := os.ReadFile(p)
	forger := &Log{path: p}
	var forged []byte
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n"))[1:] {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			t.Fatal(err)
		}
		delete(rec, "prev")
		delete(rec, "seq")
		delete(rec, "mac")
		rec["detail"] = "forged"
		body, _ := json.Marshal(rec)
		forger.seq++
		line, forger.last = forger.chainLine(forger.last, forger.seq, body)
		forged = append(append(forged, line...), '\n')
	}
	os.WriteFile(p, forged, 0o600)
	if err := forger.writeHead(); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, p, Check{}); !r.OK || r.Lines != 9 {
		t.Fatalf("the forgery is not consistent in itself: %+v", r)
	}
	if r := mustVerify(t, p, Check{Key: key}); r.OK || !strings.Contains(r.Problem, "not keyed") {
		t.Fatalf("rewritten file against the key: %+v", r)
	}
}

// A chain that began without a key carries on keyed once one is configured; until its first keyed line it
// does not pass for a keyed chain.
func TestKeyIntroducedOnExistingChain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	kf, key := keyFile(t)
	l := open(t, p, 0, "")
	write(t, l, 3, "plain")
	l.Close()
	if r := mustVerify(t, p, Check{Key: key}); r.OK || !strings.Contains(r.Problem, "not keyed") {
		t.Fatalf("unkeyed chain against a key: %+v", r)
	}
	l = open(t, p, 0, kf)
	write(t, l, 2, "keyed")
	l.Close()
	if r := mustVerify(t, p, Check{Key: key}); !r.OK || !r.Keyed || r.Lines != 5 || r.Head.Count != 5 {
		t.Fatalf("after the first keyed lines: %+v", r)
	}
}

// Lines written without the key after keyed ones (the key taken out of the config, or a forger appending)
// are a downgrade.
func TestKeyDowngradeDetected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	kf, key := keyFile(t)
	l := open(t, p, 0, kf)
	write(t, l, 3, "keyed")
	l.Close()
	l = open(t, p, 0, "")
	write(t, l, 2, "plain")
	l.Close()
	if r := mustVerify(t, p, Check{Key: key}); r.OK || !strings.Contains(r.Problem, "downgrade") {
		t.Fatalf("unkeyed lines after keyed ones: %+v", r)
	}
}

// A keyed chain runs across rotated files like a plain one, and its newest line, which no later line
// vouches for yet, is covered by its own mac.
func TestKeyedChainSurvivesRotation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	kf, key := keyFile(t)
	l := open(t, p, 1, kf)
	l.maxBytes = 4 << 10
	write(t, l, 40, "change")
	l.Close()
	if len(Rotated(p)) < 2 {
		t.Fatalf("expected rotations, got %v", Rotated(p))
	}
	r := mustVerify(t, p, Check{Key: key})
	if !r.OK || !r.Keyed || r.Lines != 40 || r.Head.Count != 40 || r.Head.File != "audit.jsonl" || len(r.Warnings) != 0 {
		t.Fatalf("keyed chain across rotations: %+v", r)
	}
	data, _ := os.ReadFile(p)
	os.WriteFile(p, bytes.Replace(data, []byte("change 39"), []byte("chanGe 39"), 1), 0o600)
	if r := mustVerify(t, p, Check{Key: key}); r.OK || !strings.Contains(r.Problem, "mac does not match") {
		t.Fatalf("edit of the newest keyed line: %+v", r)
	}
}

// The key is a secret: a file others can read, a short one and a missing one are all refused.
func TestKeyFileRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	kf, _ := keyFile(t)
	if !paths.Windows { // access is not in the mode bits there
		if err := os.Chmod(kf, 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(p, 0, kf, t.Logf); err == nil || !strings.Contains(err.Error(), "group or others") {
			t.Fatalf("key file readable by its group: %v", err)
		}
		if err := os.Chmod(kf, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(kf, []byte(strings.Repeat("k", MinKeyBytes-1)+"\n"), 0o600)
	if _, err := Open(p, 0, kf, t.Logf); err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Fatalf("short key: %v", err)
	}
	if _, err := Open(p, 0, filepath.Join(t.TempDir(), "missing.key"), t.Logf); err == nil {
		t.Fatal("missing key file was accepted")
	}
}

// Deleting the oldest lines leaves what retention leaves, a chain that starts in the middle: only lines
// retention recorded as removed may be missing.
func TestDeletedOldestLinesAreDetected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	l := open(t, p, 0, "")
	write(t, l, 10, "change")
	l.Close()
	whole := mustVerify(t, p, Check{})
	if !whole.OK || whole.First != 1 {
		t.Fatalf("intact chain: %+v", whole)
	}
	data, _ := os.ReadFile(p)
	cut := func(n int) { os.WriteFile(p, bytes.Join(bytes.SplitAfter(data, []byte("\n"))[n:], nil), 0o600) }

	cut(3)
	if r := mustVerify(t, p, Check{}); r.OK || !strings.Contains(r.Problem, "oldest lines were deleted") {
		t.Fatalf("three oldest lines deleted: %+v", r)
	}
	// Retention recorded that everything from record 4 was kept: that much may be missing, no more.
	os.WriteFile(firstPath(p), []byte(`{"first":4}`+"\n"), 0o600)
	if r := mustVerify(t, p, Check{}); !r.OK || r.First != 4 {
		t.Fatalf("lines removed by retention: %+v", r)
	}
	cut(5)
	if r := mustVerify(t, p, Check{}); r.OK || !strings.Contains(r.Problem, "2 line(s) were deleted from the start") {
		t.Fatalf("lines deleted beyond what retention removed: %+v", r)
	}
	// A first record noted elsewhere catches it even when the pruning record was rewritten too.
	os.WriteFile(firstPath(p), []byte(`{"first":6}`+"\n"), 0o600)
	if r := mustVerify(t, p, Check{ExpectFirst: 4}); r.OK || !strings.Contains(r.Problem, "after the expected 4") {
		t.Fatalf("expected first record: %+v", r)
	}
}

// Package audit writes the audit file: one JSON line per request, refused key, administrative change and
// alert. Lines are hash-chained (each carries "prev", the SHA-256 of the line before it, across rotated files),
// so an edited, inserted or deleted line breaks the chain and Verify finds it. Writers in several processes
// (the gateway, CLI commands) append under a file lock, so the chain stays linear.
package audit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lotto/substrate/internal/lock"
	"github.com/lotto/substrate/internal/paths"
)

// DefaultMaxMB is the rotation size when logging.audit_max_mb is unset.
const DefaultMaxMB = 100

// Path resolves logging.audit_file: relative paths are under the state directory; "" stays "".
func Path(p string) string {
	if p != "" && !filepath.IsAbs(p) {
		return filepath.Join(paths.State(), p)
	}
	return p
}

// Log appends chained lines to one audit file.
type Log struct {
	path     string
	maxBytes int64
	logf     func(format string, a ...any)

	mu   sync.Mutex
	f    *os.File
	size int64  // size of the file when last known
	last string // hash of its last line at that size
	errd bool
}

// Open returns a writer for path (already resolved); nil when path is "".
func Open(path string, maxMB int, logf func(string, ...any)) *Log {
	if path == "" {
		return nil
	}
	if maxMB <= 0 {
		maxMB = DefaultMaxMB
	}
	return &Log{path: path, maxBytes: int64(maxMB) << 20, logf: logf, size: -1}
}

// PathOf is the file the log writes.
func (l *Log) PathOf() string { return l.path }

func hashLine(line []byte) string {
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:])
}

// Write appends v (a JSON object) with its chain link.
func (l *Log) Write(v any) {
	if l == nil {
		return
	}
	body, err := json.Marshal(v)
	if err != nil || len(body) < 2 || body[0] != '{' {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := lock.With(l.path+".lock", func() error { return l.append(body) }); err != nil && !l.errd {
		l.logf("audit log %s: %v; audit lines are being dropped", l.path, err)
		l.errd = true
		return
	}
	l.errd = false
}

func (l *Log) append(body []byte) error {
	if err := l.open(); err != nil {
		return err
	}
	st, err := l.f.Stat()
	if err != nil {
		return err
	}
	prev := l.last
	if st.Size() != l.size { // another process wrote, or first use: read the chain head from disk
		if prev, err = l.head(st.Size()); err != nil {
			return err
		}
	}
	line := chainLine(prev, body)
	if st.Size() > 0 && st.Size()+int64(len(line))+1 > l.maxBytes {
		if err := l.rotate(); err != nil {
			return err
		}
		st = nil
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return err
	}
	l.last = hashLine(line)
	if st == nil {
		l.size = int64(len(line)) + 1
	} else {
		l.size = st.Size() + int64(len(line)) + 1
	}
	return nil
}

// chainLine inserts "prev" as the line's first field.
func chainLine(prev string, body []byte) []byte {
	var b bytes.Buffer
	b.WriteString(`{"prev":"`)
	b.WriteString(prev)
	b.WriteByte('"')
	if len(body) > 2 {
		b.WriteByte(',')
	}
	b.Write(body[1:])
	return b.Bytes()
}

// open (re)opens the file when it is not open or was moved away by an outside rotation.
func (l *Log) open() error {
	if l.f != nil {
		st, err1 := os.Stat(l.path)
		fst, err2 := l.f.Stat()
		if err1 == nil && err2 == nil && os.SameFile(st, fst) {
			return nil
		}
		l.f.Close()
		l.f, l.size = nil, -1
	}
	dir := filepath.Dir(l.path)
	var err error
	if strings.HasPrefix(dir, paths.State()) {
		err = paths.Private(paths.State(), dir)
	} else {
		err = os.MkdirAll(dir, 0o700)
	}
	if err != nil {
		return err
	}
	l.f, err = os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err == nil {
		err = os.Chmod(l.path, 0o600) // a file created by an older version may be wider
	}
	return err
}

// head is the hash of the file's last line, or of the newest rotated file's last line when it is empty.
func (l *Log) head(size int64) (string, error) {
	if size > 0 {
		return lastLineHash(l.path)
	}
	rotated := Rotated(l.path)
	if len(rotated) == 0 {
		return "", nil
	}
	return lastLineHash(rotated[len(rotated)-1])
}

func lastLineHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	// Read backwards in chunks until the last complete line is in hand.
	const chunk = 64 << 10
	end := st.Size()
	var buf []byte
	for off := end; off > 0; {
		n := int64(chunk)
		if off < n {
			n = off
		}
		off -= n
		part := make([]byte, n)
		if _, err := f.ReadAt(part, off); err != nil && err != io.EOF {
			return "", err
		}
		buf = append(part, buf...)
		trimmed := bytes.TrimRight(buf, "\n")
		if i := bytes.LastIndexByte(trimmed, '\n'); i >= 0 || off == 0 {
			return hashLine(trimmed[i+1:]), nil
		}
	}
	return "", nil
}

// rotate renames the file to <name>-<UTC time>.jsonl and opens a fresh one; the chain continues into it.
func (l *Log) rotate() error {
	l.f.Close()
	l.f = nil
	ext := filepath.Ext(l.path)
	dst := strings.TrimSuffix(l.path, ext) + "-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ext
	if err := os.Rename(l.path, dst); err != nil {
		return err
	}
	var err error
	l.f, err = os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	return err
}

// Close releases the file.
func (l *Log) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f, l.size = nil, -1
	}
}

// Rotated lists path's rotated files, oldest first.
func Rotated(path string) []string {
	ext := filepath.Ext(path)
	matches, _ := filepath.Glob(strings.TrimSuffix(path, ext) + "-*" + ext)
	sort.Strings(matches) // the UTC timestamp sorts chronologically
	return matches
}

// PruneRotated deletes rotated files last written before cutoff and returns them.
func PruneRotated(path string, cutoff time.Time) ([]string, error) {
	var gone []string
	for _, p := range Rotated(path) {
		st, err := os.Stat(p)
		if err != nil || !st.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(p); err != nil {
			return gone, err
		}
		gone = append(gone, p)
	}
	return gone, nil
}

// Report is the outcome of Verify.
type Report struct {
	Files []string `json:"files"`
	Lines int      `json:"lines"`
	// Unchained counts lines written before chaining existed (they precede the first chained line).
	Unchained int `json:"unchained"`
	// Anchored says the oldest retained line links to a file no longer present (pruned by retention).
	Anchored bool   `json:"anchored_to_pruned_file"`
	OK       bool   `json:"ok"`
	Problem  string `json:"problem,omitempty"`
}

// Verify walks the rotated files and the current one in order and checks every link.
func Verify(path string) (*Report, error) {
	files := append(Rotated(path), path)
	r := &Report{OK: true}
	prev, started := "", false
	for _, fp := range files {
		f, err := os.Open(fp)
		if os.IsNotExist(err) && fp == path {
			continue
		}
		if err != nil {
			return nil, err
		}
		r.Files = append(r.Files, fp)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		n := 0
		for sc.Scan() {
			n++
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			r.Lines++
			var link struct {
				Prev *string `json:"prev"`
			}
			if err := json.Unmarshal(line, &link); err != nil {
				r.OK, r.Problem = false, fmt.Sprintf("%s line %d: not JSON: %v", fp, n, err)
				f.Close()
				return r, nil
			}
			switch {
			case link.Prev == nil && !started:
				r.Unchained++
			case link.Prev == nil:
				r.OK, r.Problem = false, fmt.Sprintf("%s line %d: no chain link after chained lines (inserted line?)", fp, n)
			case !started && r.Unchained == 0 && *link.Prev != "":
				r.Anchored = true // the predecessor was pruned; this line anchors what remains
			case *link.Prev != prev:
				r.OK, r.Problem = false, fmt.Sprintf("%s line %d: chain broken (the line before it was changed, removed or reordered)", fp, n)
			}
			if !r.OK {
				f.Close()
				return r, nil
			}
			if link.Prev != nil {
				started = true
			}
			prev = hashLine(line)
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	return r, nil
}

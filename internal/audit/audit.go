// Package audit writes the audit file: one JSON line per request, refused key, administrative change and
// alert. Lines are hash-chained: each carries "prev", the chain hash of the line before it (across rotated
// files), and "seq", its number in the chain, so an edited, inserted or deleted line breaks the chain and
// Verify finds it. A line's chain hash is its SHA-256; with a key (gateway.audit_key_file) it is an
// HMAC-SHA256 the line also carries as "mac", so a file rewritten without the key does not verify. The
// newest line's number and chain hash (the head) are kept in <file>.head: a file that ends before that
// line, or without it, lost its tail. Writers in several processes (the gateway, CLI commands) append under
// a file lock, so the chain stays linear.
package audit

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LottoLottoLotto/substrate/internal/lock"
	"github.com/LottoLottoLotto/substrate/internal/paths"
)

// DefaultMaxMB is the rotation size when logging.audit_max_mb is unset.
const DefaultMaxMB = 100

// MinKeyBytes is the shortest secret gateway.audit_key_file may hold.
const MinKeyBytes = 32

// Path resolves logging.audit_file: relative paths are under the state directory; "" stays "".
func Path(p string) string {
	if p != "" && !filepath.IsAbs(p) {
		return filepath.Join(paths.State(), p)
	}
	return p
}

// KeyPath resolves gateway.audit_key_file: relative paths are under the config directory; "" stays "".
func KeyPath(p string) string {
	if p != "" && !filepath.IsAbs(p) {
		return filepath.Join(paths.Config(), p)
	}
	return p
}

// LoadKey reads the audit key from file (already resolved); nil when file is "". Surrounding whitespace is
// not part of the key. A file that group or others can access, or that holds fewer than MinKeyBytes bytes,
// is refused.
func LoadKey(file string) ([]byte, error) {
	if file == "" {
		return nil, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("audit key file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("audit key file: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("audit key file %s is accessible by group or others (mode %v); chmod 600 it", file, st.Mode().Perm())
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("audit key file: %w", err)
	}
	key := bytes.TrimSpace(data)
	if len(key) < MinKeyBytes {
		return nil, fmt.Errorf("audit key file %s holds %d bytes; at least %d are required (for example: openssl rand -hex 32)", file, len(key), MinKeyBytes)
	}
	return key, nil
}

// Head is where a chain ends: the newest line's sequence number and chain hash. The writer keeps the current
// one in <audit file>.head (the head anchor); kept somewhere else as well (a SIEM, a ticket), it later
// proves that nothing up to that line was removed or rewritten (Check.ExpectHead).
type Head struct {
	File  string `json:"file,omitempty"` // name of the file holding the line
	Count int64  `json:"count"`
	Hash  string `json:"hash"`
}

func headPath(path string) string { return path + ".head" }

// readHead returns the head anchor of the audit file at path; nil when there is none.
func readHead(path string) (*Head, error) {
	data, err := os.ReadFile(headPath(path))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var h Head
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// Log appends chained lines to one audit file.
type Log struct {
	path     string
	maxBytes int64
	mac      hash.Hash // HMAC-SHA256 under the audit key; nil for an unkeyed chain
	logf     func(format string, a ...any)

	mu   sync.Mutex
	f    *os.File
	size int64  // size of the file when last known
	last string // chain hash of its last line at that size
	seq  int64  // and that line's sequence number
	errd bool
}

// Open returns a writer for path (already resolved); nil when path is "". keyFile (already resolved; "" for
// an unkeyed chain) holds the audit key: see LoadKey for when it is refused.
func Open(path string, maxMB int, keyFile string, logf func(string, ...any)) (*Log, error) {
	if path == "" {
		return nil, nil
	}
	key, err := LoadKey(keyFile)
	if err != nil {
		return nil, err
	}
	if maxMB <= 0 {
		maxMB = DefaultMaxMB
	}
	l := &Log{path: path, maxBytes: int64(maxMB) << 20, logf: logf, size: -1}
	if key != nil {
		l.mac = hmac.New(sha256.New, key)
	}
	return l, nil
}

// PathOf is the file the log writes.
func (l *Log) PathOf() string { return l.path }

func hashLine(line []byte) string {
	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:])
}

// macLine is a keyed line's chain hash: the HMAC of the line as it reads without its "mac" field, given as
// its start (the "prev" and "seq" fields) and what follows them.
func macLine(mac hash.Hash, start, rest []byte) string {
	mac.Reset()
	mac.Write(start)
	mac.Write(rest)
	return hex.EncodeToString(mac.Sum(nil))
}

// Write appends v (a JSON object) with its chain fields.
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
	dropped := "; audit lines are being dropped"
	err = lock.With(l.path+".lock", func() error {
		if err := l.append(body); err != nil {
			return err
		}
		dropped = "" // the line is in the file; only its head anchor is not
		return l.writeHead()
	})
	if err != nil && !l.errd {
		l.logf("audit log %s: %v%s", l.path, err, dropped)
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
	if st.Size() != l.size { // another process wrote, or first use: read the chain head from disk
		if err := l.resync(st.Size()); err != nil {
			return err
		}
	}
	line, sum := l.chainLine(l.last, l.seq+1, body)
	if st.Size() > 0 && st.Size()+int64(len(line))+1 > l.maxBytes {
		if err := l.rotate(); err != nil {
			return err
		}
		st = nil
	}
	if _, err := l.f.Write(append(line, '\n')); err != nil {
		return err
	}
	l.last, l.seq = sum, l.seq+1
	if st == nil {
		l.size = int64(len(line)) + 1
	} else {
		l.size = st.Size() + int64(len(line)) + 1
	}
	return nil
}

// appendLink starts a line with its "prev" and "seq" fields.
func appendLink(b []byte, prev string, seq int64) []byte {
	b = append(b, `{"prev":"`...)
	b = append(b, prev...)
	b = append(b, `","seq":`...)
	return strconv.AppendInt(b, seq, 10)
}

// chainLine puts the chain fields first in the line ("prev", "seq" and, with a key, "mac") and returns the
// line with its chain hash. It overwrites body's opening brace.
func (l *Log) chainLine(prev string, seq int64, body []byte) ([]byte, string) {
	rest := body[1:] // an empty object: only the closing brace follows the chain fields
	if len(body) > 2 {
		body[0] = ','
		rest = body
	}
	line := appendLink(make([]byte, 0, len(body)+192), prev, seq)
	if l.mac == nil {
		line = append(line, rest...)
		return line, hashLine(line)
	}
	sum := macLine(l.mac, line, rest)
	line = append(line, `,"mac":"`...)
	line = append(line, sum...)
	line = append(line, '"')
	return append(line, rest...), sum
}

// writeHead records the chain head next to the file (written aside, then renamed over the old one, so a
// reader sees one or the other).
func (l *Log) writeHead() error {
	data, err := json.Marshal(Head{File: filepath.Base(l.path), Count: l.seq, Hash: l.last})
	if err == nil {
		tmp := headPath(l.path) + ".tmp"
		if err = os.WriteFile(tmp, append(data, '\n'), 0o600); err == nil {
			err = os.Rename(tmp, headPath(l.path))
		}
	}
	if err != nil {
		return fmt.Errorf("head anchor not written (substrate audit verify reports the file as ahead of it): %w", err)
	}
	return nil
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

// link is a line's chain fields.
type link struct {
	Prev *string `json:"prev"`
	Seq  *int64  `json:"seq"`
	Mac  *string `json:"mac"`
}

// resync reads the chain head from disk: the file's last line, or the newest rotated file's when it is empty.
// When the head anchor is not behind that line and names another one, the tail it recorded was removed or
// rewritten: the next line is then chained from the anchor, which leaves the break in the file for Verify
// instead of mending it. (A file ahead of its anchor only lost anchor writes.)
func (l *Log) resync(size int64) error {
	p := l.path
	if size == 0 {
		p = ""
		if rotated := Rotated(l.path); len(rotated) > 0 {
			p = rotated[len(rotated)-1]
		}
	}
	var last string
	var seq int64
	if p != "" {
		line, err := lastLine(p)
		if err != nil {
			return err
		}
		var lk link
		_ = json.Unmarshal(line, &lk) // a line that is not JSON has no number and chains by its SHA-256
		if lk.Seq != nil {
			seq = *lk.Seq
		}
		if lk.Mac != nil {
			last = *lk.Mac
		} else {
			last = hashLine(line)
		}
	}
	// An unreadable anchor is replaced like a missing one.
	if a, _ := readHead(l.path); a != nil && a.Count >= seq && a.Hash != last {
		l.logf("audit log %s: the file does not end with the line its head anchor names (record %d): lines were removed, rewritten or moved away; the next line is chained from the anchor (substrate audit verify)", l.path, a.Count)
		last, seq = a.Hash, a.Count
	}
	l.last, l.seq = last, seq
	return nil
}

// lastLine returns the last line of the file at path.
func lastLine(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
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
			return nil, err
		}
		buf = append(part, buf...)
		trimmed := bytes.TrimRight(buf, "\n")
		if i := bytes.LastIndexByte(trimmed, '\n'); i >= 0 || off == 0 {
			return trimmed[i+1:], nil
		}
	}
	return nil, nil
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

// Check is what Verify holds the chain to besides its own links.
type Check struct {
	// Key is the audit key (LoadKey); nil without one. With a key, the chain must end in keyed lines.
	Key []byte
	// ExpectHead is a head hash an earlier Verify reported: the chain must still contain that line.
	ExpectHead string
	// ExpectCount is the count reported with ExpectHead (0: not checked); without ExpectHead, a count the
	// chain must reach.
	ExpectCount int64
}

// Report is the outcome of Verify.
type Report struct {
	Files []string `json:"files"`
	Lines int      `json:"lines"`
	// Unchained counts lines written before chaining existed (they precede the first chained line).
	Unchained int `json:"unchained"`
	// Anchored says the oldest retained line links to a file no longer present (pruned by retention).
	Anchored bool `json:"anchored_to_pruned_file"`
	// Keyed says the chain ends in lines made with the audit key.
	Keyed bool `json:"keyed"`
	// Head is the chain's end; its count is 0 while no line carries a sequence number (older files).
	Head Head `json:"head"`
	// ExpectedAt is the count of the line Check.ExpectHead names, when the chain contains it.
	ExpectedAt int64 `json:"expected_head_count,omitempty"`
	// Warnings do not fail verification: the head anchor is missing or unreadable.
	Warnings []string `json:"warnings,omitempty"`
	OK       bool     `json:"ok"`
	Problem  string   `json:"problem,omitempty"`
}

// Verify walks the rotated files and the current one in order and checks every link, sequence number and
// mac, then that the chain contains the line its head anchor names and the one c expects.
func Verify(path string, c Check) (*Report, error) {
	// The anchor is read before the lines: a gateway writing meanwhile only takes the chain past it.
	anchor, anchorErr := readHead(path)
	files := append(Rotated(path), path)
	r := &Report{OK: true}
	fail := func(format string, a ...any) (*Report, error) {
		r.OK, r.Problem = false, fmt.Sprintf(format, a...)
		return r, nil
	}
	var mac hash.Hash
	if c.Key != nil {
		mac = hmac.New(sha256.New, c.Key)
	}
	var (
		prev                 string // chain hash of the line before
		seq                  int64  // sequence number of the last numbered line
		started, numbered    bool   // a chained, a numbered line was seen
		atAnchor, atExpected bool   // the line the anchor, the caller names was seen
		keyed                []byte // a keyed line's fields up to and including "mac"
	)
	// check takes the next line into the chain, or says why it does not fit.
	check := func(line []byte) string {
		var lk link
		if err := json.Unmarshal(line, &lk); err != nil {
			return fmt.Sprintf("not JSON: %v", err)
		}
		switch {
		case lk.Prev == nil && !started:
			r.Unchained++
		case lk.Prev == nil:
			return "no chain link after chained lines (inserted line?)"
		case !started && r.Unchained == 0 && *lk.Prev != "":
			r.Anchored = true // the predecessor was pruned; this line anchors what remains
		case *lk.Prev != prev:
			return "chain broken (the line before it was changed, removed or reordered)"
		}
		switch {
		case lk.Seq == nil && numbered:
			return "no sequence number after numbered lines (inserted line?)"
		case lk.Seq != nil && numbered && *lk.Seq != seq+1:
			return fmt.Sprintf("sequence number %d follows %d (lines were removed or reordered)", *lk.Seq, seq)
		}
		var sum string
		switch {
		case lk.Mac == nil && r.Keyed:
			return "not keyed after keyed lines (a downgrade: it was written without the audit key)"
		case lk.Mac == nil:
			sum = hashLine(line)
		case mac == nil:
			return "keyed line, but no audit key was given (gateway.audit_key_file, --key-file)"
		case lk.Prev == nil || lk.Seq == nil:
			return "keyed line without a chain link and sequence number"
		default:
			keyed = appendLink(keyed[:0], *lk.Prev, *lk.Seq)
			n := len(keyed)
			keyed = append(append(append(keyed, `,"mac":"`...), *lk.Mac...), '"')
			if !bytes.HasPrefix(line, keyed) || !hmac.Equal([]byte(macLine(mac, keyed[:n], line[len(keyed):])), []byte(*lk.Mac)) {
				return "mac does not match (the line was changed, or the audit key is not the one it was written with)"
			}
			sum, r.Keyed = *lk.Mac, true
		}
		if lk.Prev != nil {
			started = true
		}
		if lk.Seq != nil {
			numbered, seq = true, *lk.Seq
		}
		prev = sum
		if anchor != nil && seq == anchor.Count && sum == anchor.Hash {
			atAnchor = true
		}
		if c.ExpectHead != "" && sum == c.ExpectHead {
			if c.ExpectCount > 0 && seq != c.ExpectCount {
				return fmt.Sprintf("has the expected head hash but is record %d, not %d", seq, c.ExpectCount)
			}
			atExpected, r.ExpectedAt = true, seq
		}
		return ""
	}
	for _, fp := range files {
		f, err := os.Open(fp)
		if os.IsNotExist(err) && fp == path {
			continue
		}
		if err != nil {
			return nil, err
		}
		r.Files = append(r.Files, fp)
		base := filepath.Base(fp)
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
			if problem := check(line); problem != "" {
				f.Close()
				return fail("%s line %d: %s", fp, n, problem)
			}
			r.Head.File = base
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	r.Head.Count, r.Head.Hash = seq, prev
	if mac != nil && r.Lines > 0 && !r.Keyed {
		return fail("an audit key is configured but the newest line is not keyed: the file was rewritten without the key, or nothing was written since the key was configured")
	}
	switch {
	case anchorErr != nil:
		r.Warnings = append(r.Warnings, fmt.Sprintf("head anchor %s is unreadable (%v): a truncated tail would go unnoticed", headPath(path), anchorErr))
	case anchor == nil && r.Lines > 0:
		r.Warnings = append(r.Warnings, fmt.Sprintf("no head anchor %s (the file was last written by an older version, or the anchor was removed): a truncated tail would go unnoticed", headPath(path)))
	case anchor == nil, atAnchor:
	case seq < anchor.Count:
		return fail("the chain ends at record %d but its head anchor %s is at record %d: %d line(s) were removed from the tail, or an older copy of the file was restored",
			seq, headPath(path), anchor.Count, anchor.Count-seq)
	default:
		return fail("the chain does not contain the line its head anchor %s names (record %d, hash %s): the tail was rewritten", headPath(path), anchor.Count, anchor.Hash)
	}
	switch {
	case c.ExpectHead != "" && !atExpected:
		return fail("the expected head %s is not in the chain: lines were removed or rewritten since it was recorded (or its file was pruned)", c.ExpectHead)
	case c.ExpectHead == "" && seq < c.ExpectCount:
		return fail("the chain ends at record %d, before the expected %d: lines were removed from the tail", seq, c.ExpectCount)
	}
	return r, nil
}

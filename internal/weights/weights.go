// Package weights fingerprints a model's files, so that whoever is offered the model can tell which
// weights answer: the sha256 of every weight file (what Hugging Face publishes as a large file's lfs.oid),
// the git blob SHA-1 of every small file (a small file's oid), and an id that is equal for every copy of
// the same weights whatever their file names.
//
// Hashing a large model takes tens of seconds, so a Tracker hashes in the background, one file at a time,
// and remembers each file's hashes in the state directory by path, size and modification time: a restart
// does not hash again, a changed file is hashed again.
package weights

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/model"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/paths"
)

// Status of a fingerprint.
const (
	StatusReady   = "ready"   // every file is hashed: ID and Files are set
	StatusHashing = "hashing" // some file is being hashed (or waits its turn)
	StatusError   = "error"   // a file could not be read, or the source has no weight files: Error says why
)

const (
	gitSHA1Below = 10 << 20  // files smaller than this also get their git blob SHA-1
	tooLarge     = 100 << 30 // files larger than this are left out
	// inlineBelow: when what is left to hash is smaller than this, the caller waits for it (milliseconds)
	// instead of seeing "hashing".
	inlineBelow = 32 << 20
	// retryAfter is how long a failed fingerprint is reported before the files are tried again.
	retryAfter = time.Minute
)

// Origin is the Hugging Face repo, and the commit, the files came from.
type Origin struct {
	Repo     string `json:"repo"`
	Revision string `json:"revision,omitempty"`
}

// File is one file of a model.
type File struct {
	// Path is relative to the model's directory, with forward slashes; for a single-file source, its name.
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	// SHA256 is the hex sha256 of the content.
	SHA256 string `json:"sha256"`
	// GitSHA1 is the git blob SHA-1, sha1("blob <size>\x00" + content), for files under 10 MiB.
	GitSHA1 string `json:"git_sha1,omitempty"`
	// Weight is true for weight files (.gguf, .safetensors, .bin, .pt, .pth), which make up the ID.
	Weight bool `json:"weight"`
}

// Report is a model's fingerprint as /v1/models shows it (emx.weights).
type Report struct {
	Status string `json:"status"`
	// ID is the sha256 of the lines "<sha256> <bytes>\n" of the weight files, sorted by sha256 (ready only).
	ID     string  `json:"id,omitempty"`
	Files  []File  `json:"files,omitempty"`
	Origin *Origin `json:"origin,omitempty"`
	Error  string  `json:"error,omitempty"`
}

// Tracker fingerprints model sources in the background and remembers file hashes on disk.
type Tracker struct {
	cachePath string // "" keeps hashes in memory only

	mu     sync.Mutex
	loaded bool
	cache  map[string]cached // by resolved absolute path
	jobs   map[string]*job   // by the source's absolute path

	sem    chan struct{} // one background file hash at a time: the disk is the limit
	saveMu sync.Mutex
	hashed atomic.Int64 // files hashed (tests)
}

// cached is what is remembered of one file.
type cached struct {
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
	SHA256  string `json:"sha256"`
	GitSHA1 string `json:"git_sha1,omitempty"`
}

type cacheFile struct {
	Files map[string]cached `json:"files"`
}

// job is the hashing of one source's missing files.
type job struct {
	running bool
	sig     string // the files' paths, sizes and times when it started
	err     string
	at      time.Time // when it ended
}

// New returns a Tracker that keeps file hashes in cachePath (a JSON file in the state directory).
func New(cachePath string) *Tracker {
	return &Tracker{cachePath: cachePath, cache: map[string]cached{}, jobs: map[string]*job{}, sem: make(chan struct{}, 1)}
}

// file is one file of a source as found on disk.
type file struct {
	rel    string // reported path
	real   string // resolved absolute path: what is read, and the cache key
	size   int64
	mtime  int64
	weight bool
}

// source is a model source's files as found on disk.
type source struct {
	key    string // the absolute path, as configured
	files  []file
	origin *Origin
}

// Fingerprint reports source's fingerprint: ready when every file's hashes are known, else hashing (and
// the missing files are hashed in the background), or error. It never waits for a large file. origin,
// when not nil, is reported in place of the one the source's path shows.
func (t *Tracker) Fingerprint(src string, origin *Origin) Report {
	s, err := scan(src)
	if origin == nil {
		origin = s.origin
	}
	if err != nil {
		return Report{Status: StatusError, Error: err.Error(), Origin: origin}
	}
	t.mu.Lock()
	t.load()
	r, missing := t.report(s)
	if missing == nil {
		t.mu.Unlock()
		r.Origin = origin
		return r
	}
	sig := signature(missing)
	j := t.jobs[s.key]
	switch {
	case j != nil && j.running:
		t.mu.Unlock()
		return Report{Status: StatusHashing, Origin: origin}
	case j != nil && j.err != "" && j.sig == sig && time.Since(j.at) < retryAfter:
		t.mu.Unlock()
		return Report{Status: StatusError, Error: j.err, Origin: origin}
	}
	j = &job{running: true, sig: sig}
	t.jobs[s.key] = j
	t.mu.Unlock()
	var left int64
	for _, f := range missing {
		left += f.size
	}
	if left >= inlineBelow {
		go t.run(j, missing, true)
		return Report{Status: StatusHashing, Origin: origin}
	}
	t.run(j, missing, false)
	t.mu.Lock()
	defer t.mu.Unlock()
	r, missing = t.report(s)
	switch {
	case missing == nil:
	case j.err != "":
		r = Report{Status: StatusError, Error: j.err}
	default: // a file changed while it was read: the next call hashes it again
		r = Report{Status: StatusHashing}
	}
	r.Origin = origin
	return r
}

// report builds the ready report of s from the cache, or returns the files whose hashes are not known.
// t.mu is held.
func (t *Tracker) report(s source) (Report, []file) {
	var missing []file
	r := Report{Status: StatusReady, Files: make([]File, 0, len(s.files))}
	var lines []string
	for _, f := range s.files {
		c, ok := t.cache[f.real]
		if !ok || c.Size != f.size || c.MtimeNS != f.mtime {
			missing = append(missing, f)
			continue
		}
		r.Files = append(r.Files, File{Path: f.rel, Bytes: f.size, SHA256: c.SHA256, GitSHA1: c.GitSHA1, Weight: f.weight})
		if f.weight {
			lines = append(lines, c.SHA256+" "+strconv.FormatInt(f.size, 10)+"\n")
		}
	}
	if missing != nil {
		return Report{}, missing
	}
	if len(lines) == 0 {
		return Report{Status: StatusError, Error: "no weight files (.gguf, .safetensors, .bin, .pt, .pth)"}, nil
	}
	r.ID = weightsID(lines)
	return r, nil
}

// weightsID is the id of the lines "<sha256> <bytes>\n", one per weight file; it sorts lines.
func weightsID(lines []string) string {
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		io.WriteString(h, l)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func signature(files []file) string {
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "%s\x00%d\x00%d\n", f.real, f.size, f.mtime)
	}
	return b.String()
}

// run hashes files for job j; background runs take turns with each other.
func (t *Tracker) run(j *job, files []file, background bool) {
	var failed error
	for _, f := range files {
		if failed = t.hashOne(f, background); failed != nil {
			break
		}
	}
	t.mu.Lock()
	j.running, j.at = false, time.Now()
	if failed != nil {
		j.err = failed.Error()
	}
	t.mu.Unlock()
}

func (t *Tracker) hashOne(f file, background bool) error {
	if background {
		t.sem <- struct{}{}
		defer func() { <-t.sem }()
	}
	t.mu.Lock()
	c, ok := t.cache[f.real]
	t.mu.Unlock()
	if ok && c.Size == f.size && c.MtimeNS == f.mtime {
		return nil // another source with the same file got there first
	}
	sum, git, err := hashFile(f.real, f.size)
	if err != nil {
		return fmt.Errorf("%s: %w", f.rel, plain(err))
	}
	t.hashed.Add(1)
	if st, err := os.Stat(f.real); err != nil || st.Size() != f.size || st.ModTime().UnixNano() != f.mtime {
		return nil // changed while it was read: not remembered, so the next look hashes it again
	}
	t.mu.Lock()
	t.cache[f.real] = cached{Size: f.size, MtimeNS: f.mtime, SHA256: sum, GitSHA1: git}
	t.mu.Unlock()
	t.save()
	return nil
}

// hashFile returns the sha256 of the file at path, and its git blob SHA-1 when it is under 10 MiB.
func hashFile(path string, size int64) (sum, git string, err error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer fh.Close()
	s := sha256.New()
	var g hash.Hash
	w := io.Writer(s)
	if size < gitSHA1Below {
		g = sha1.New()
		fmt.Fprintf(g, "blob %d\x00", size)
		w = io.MultiWriter(s, g)
	}
	n, err := io.CopyBuffer(w, fh, make([]byte, 1<<20))
	if err != nil {
		return "", "", err
	}
	if n != size {
		return "", "", fmt.Errorf("size changed while reading (%d bytes, expected %d)", n, size)
	}
	sum = hex.EncodeToString(s.Sum(nil))
	if g != nil {
		git = hex.EncodeToString(g.Sum(nil))
	}
	return sum, git, nil
}

// plain drops the path from a file system error: the message is shown to API clients.
func plain(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// load reads the hash cache once, dropping files that are gone or changed. t.mu is held.
func (t *Tracker) load() {
	if t.loaded || t.cachePath == "" {
		return
	}
	t.loaded = true
	data, err := os.ReadFile(t.cachePath)
	if err != nil {
		return
	}
	var cf cacheFile
	if json.Unmarshal(data, &cf) != nil {
		return
	}
	for p, c := range cf.Files {
		if st, err := os.Stat(p); err == nil && st.Size() == c.Size && st.ModTime().UnixNano() == c.MtimeNS {
			t.cache[p] = c
		}
	}
}

// save writes the hash cache (best effort: without it a restart hashes again).
func (t *Tracker) save() {
	if t.cachePath == "" {
		return
	}
	t.saveMu.Lock()
	defer t.saveMu.Unlock()
	t.mu.Lock()
	data, err := json.Marshal(cacheFile{Files: t.cache})
	t.mu.Unlock()
	if err != nil {
		return
	}
	dir := filepath.Dir(t.cachePath)
	if paths.Private(dir) != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(t.cachePath)+".*.tmp")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr != nil || cerr != nil || paths.Replace(tmp.Name(), t.cachePath) != nil {
		os.Remove(tmp.Name())
	}
}

// hfCacheRe finds a Hugging Face cache snapshot in a path: models--<org>--<name>/snapshots/<commit>.
var hfCacheRe = regexp.MustCompile(`(?:^|/)models--([^/]+?)--([^/]+)/snapshots/([0-9a-f]{40})(?:/|$)`)

// cacheOrigin is the repo and commit of a path inside the Hugging Face cache, or nil.
func cacheOrigin(abs string) *Origin {
	m := hfCacheRe.FindStringSubmatch(filepath.ToSlash(abs))
	if m == nil {
		return nil
	}
	return &Origin{Repo: m[1] + "/" + m[2], Revision: m[3]}
}

// weightExt reports whether name is a weight file.
func weightExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".gguf", ".safetensors", ".bin", ".pt", ".pth":
		return true
	}
	return false
}

// listed reports whether a file of a directory source is part of its fingerprint: weights, configs,
// tokenizers and chat templates. READMEs, hidden files, images and code are not.
func listed(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(name, ".") || strings.HasPrefix(lower, "readme") {
		return false
	}
	if weightExt(name) {
		return true
	}
	switch filepath.Ext(lower) {
	case ".json", ".jinja", ".txt", ".model":
		return true
	}
	return strings.HasPrefix(lower, "tokenizer") || strings.HasPrefix(lower, "chat_template")
}

var shardRe = regexp.MustCompile(`^(.*)-\d{5}-of-(\d{5})\.gguf$`)

// scan finds src's files: a directory's listed files (recursively), a file with the other shards of a
// split GGUF, a Strata config with the GGUF files it names, or a Hugging Face repo id's cached snapshot.
func scan(src string) (source, error) {
	var s source
	p := paths.Expand(src)
	st, err := os.Stat(p)
	if err != nil {
		if !os.IsNotExist(err) || !model.IsRepoID(src) {
			// Name the source as written: the expanded path may hold values of environment variables.
			return s, fmt.Errorf("%s: %w", src, plain(err))
		}
		dir, rev, ok := model.HFSnapshot(src)
		if !ok {
			return s, fmt.Errorf("%s is not downloaded yet (its engine fetches it on first start)", src)
		}
		s.origin = &Origin{Repo: src, Revision: rev}
		p = dir
		if st, err = os.Stat(p); err != nil {
			return s, fmt.Errorf("%s: %w", src, plain(err))
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return s, err
	}
	s.key = abs
	if s.origin == nil {
		s.origin = cacheOrigin(abs)
	}
	add := func(rel, path string, weight bool) error {
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, plain(err))
		}
		fi, err := os.Stat(real)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, plain(err))
		}
		if fi.IsDir() || fi.Size() > tooLarge {
			return nil
		}
		s.files = append(s.files, file{rel: rel, real: real, size: fi.Size(), mtime: fi.ModTime().UnixNano(), weight: weight})
		return nil
	}
	if st.IsDir() {
		root, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return s, fmt.Errorf("%s: %w", src, plain(err))
		}
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return fmt.Errorf("%s: %w", src, plain(err))
			}
			if d.IsDir() {
				if path != root && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir // .cache (huggingface-cli download --local-dir), .git
				}
				return nil
			}
			if !listed(d.Name()) {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			return add(filepath.ToSlash(rel), path, weightExt(d.Name()))
		})
		sort.Slice(s.files, func(i, j int) bool { return s.files[i].rel < s.files[j].rel })
		return s, err
	}
	name := filepath.Base(abs)
	if strings.HasSuffix(strings.ToLower(name), ".json") {
		return s, scanStrata(&s, abs, add)
	}
	if m := shardRe.FindStringSubmatch(abs); m != nil {
		shards, _ := filepath.Glob(m[1] + "-*-of-" + m[2] + ".gguf")
		sort.Strings(shards)
		for _, sh := range shards {
			if err := add(filepath.Base(sh), sh, true); err != nil {
				return s, err
			}
		}
		return s, nil
	}
	return s, add(name, abs, weightExt(name))
}

// scanStrata lists a Strata config and the GGUF files its arguments name.
func scanStrata(s *source, abs string, add func(rel, path string, weight bool) error) error {
	data, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(abs), plain(err))
	}
	var c struct {
		Args []string `json:"args"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(abs), err)
	}
	if err := add(filepath.Base(abs), abs, false); err != nil {
		return err
	}
	for _, arg := range c.Args {
		if filepath.IsAbs(arg) && strings.HasSuffix(strings.ToLower(arg), ".gguf") {
			if err := add(filepath.Base(arg), arg, true); err != nil {
				return err
			}
		}
	}
	return nil
}

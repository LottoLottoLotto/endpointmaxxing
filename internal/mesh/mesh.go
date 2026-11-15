// Package mesh authenticates the calls gateways of one mesh make to each other. Every call carries an
// HMAC-SHA256, under the secret all machines of the mesh share, of what it asks for: method, path, time,
// sender, the client and team it acts for, and the SHA-256 of its body. A receiver accepts a call that is
// at most MaxSkew old, from a configured peer, that it has not seen before. Answers that carry state (what a
// peer serves, the keys a follower copies) are signed too, bound to the call they answer.
package mesh

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/paths"
)

// Headers of a mesh call.
const (
	HeaderFrom      = "X-Emx-Mesh-From"
	HeaderTime      = "X-Emx-Mesh-Time"
	HeaderClient    = "X-Emx-Mesh-Client"
	HeaderTeam      = "X-Emx-Mesh-Team"
	HeaderBody      = "X-Emx-Mesh-Body"
	HeaderNonce     = "X-Emx-Mesh-Nonce" // makes two identical calls in one second two different calls
	HeaderSignature = "X-Emx-Mesh-Signature"
	// HeaderReply signs an answer: HMAC of the call's signature and the answer's body.
	HeaderReply = "X-Emx-Mesh-Reply"
)

// MinSecretBytes is the shortest secret accepted.
const MinSecretBytes = 32

// MaxSkew is how far a call's time may be from the receiver's clock.
const MaxSkew = 60 * time.Second

// LoadSecret reads the mesh secret: owner-only, at least MinSecretBytes after trimming whitespace.
func LoadSecret(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, fmt.Errorf("mesh secret file: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("mesh secret file: %w", err)
	}
	if !paths.OwnerOnly(st.Mode()) {
		return nil, fmt.Errorf("mesh secret file %s is accessible by group or others (mode %v); chmod 600 it", file, st.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nil, fmt.Errorf("mesh secret file: %w", err)
	}
	s := bytes.TrimSpace(data)
	if len(s) < MinSecretBytes {
		return nil, fmt.Errorf("mesh secret file %s holds %d bytes; at least %d are required (openssl rand -hex 32), the same on every machine", file, len(s), MinSecretBytes)
	}
	return s, nil
}

func bodyHash(body []byte) string {
	s := sha256.Sum256(body)
	return hex.EncodeToString(s[:])
}

func mac(secret []byte, parts ...string) string {
	m := hmac.New(sha256.New, secret)
	for _, p := range parts {
		m.Write([]byte(p))
		m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))
}

func canonical(secret []byte, h http.Header, method, uri string) string {
	return mac(secret, "emx-mesh-v1", method, uri, h.Get(HeaderTime), h.Get(HeaderNonce), h.Get(HeaderFrom), h.Get(HeaderClient), h.Get(HeaderTeam), h.Get(HeaderBody))
}

// Sign makes h a signed call from node, acting for client and team, of method uri (path and query) with body.
func Sign(secret []byte, h http.Header, node, client, team, method, uri string, body []byte, now time.Time) {
	h.Set(HeaderFrom, node)
	h.Set(HeaderTime, strconv.FormatInt(now.Unix(), 10))
	nonce := make([]byte, 12)
	rand.Read(nonce)
	h.Set(HeaderNonce, hex.EncodeToString(nonce))
	setOrDel(h, HeaderClient, client)
	setOrDel(h, HeaderTeam, team)
	h.Set(HeaderBody, bodyHash(body))
	h.Set(HeaderSignature, canonical(secret, h, method, uri))
}

func setOrDel(h http.Header, k, v string) {
	if v == "" {
		h.Del(k)
	} else {
		h.Set(k, v)
	}
}

// IsCall reports whether r claims to be a mesh call.
func IsCall(h http.Header) bool { return h.Get(HeaderSignature) != "" }

// Call is a verified mesh call.
type Call struct {
	From, Client, Team string
	Signature          string
	bodyHash           string
}

// BodyMatches reports whether body is the one the call was signed with.
func (c *Call) BodyMatches(body []byte) bool {
	return hmac.Equal([]byte(bodyHash(body)), []byte(c.bodyHash))
}

// ErrRejected is a call that does not verify.
var ErrRejected = errors.New("mesh call rejected")

// Verifier checks calls and remembers the signatures it accepted, so none is accepted twice.
type Verifier struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// Verify checks a call's signature, time and sender (known reports whether a node is a configured peer).
func (v *Verifier) Verify(secret []byte, h http.Header, method, uri string, known func(string) bool, now time.Time) (*Call, error) {
	sig := h.Get(HeaderSignature)
	if len(secret) == 0 {
		return nil, fmt.Errorf("%w: this gateway is not in a mesh", ErrRejected)
	}
	if !hmac.Equal([]byte(sig), []byte(canonical(secret, h, method, uri))) {
		return nil, fmt.Errorf("%w: bad signature (is the mesh secret the same on both machines?)", ErrRejected)
	}
	ts, err := strconv.ParseInt(h.Get(HeaderTime), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: no time", ErrRejected)
	}
	if d := now.Sub(time.Unix(ts, 0)); d > MaxSkew || d < -MaxSkew {
		return nil, fmt.Errorf("%w: sent %s from this machine's clock (more than %s): check both clocks", ErrRejected, d.Round(time.Second), MaxSkew)
	}
	from := h.Get(HeaderFrom)
	if !known(from) {
		return nil, fmt.Errorf("%w: %q is not a peer in mesh.peers", ErrRejected, from)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen == nil {
		v.seen = map[string]time.Time{}
	}
	if _, dup := v.seen[sig]; dup {
		return nil, fmt.Errorf("%w: replayed", ErrRejected)
	}
	if len(v.seen) > 4096 {
		for s, t := range v.seen {
			if now.Sub(t) > 2*MaxSkew {
				delete(v.seen, s)
			}
		}
	}
	v.seen[sig] = now
	return &Call{From: from, Client: h.Get(HeaderClient), Team: h.Get(HeaderTeam), Signature: sig, bodyHash: h.Get(HeaderBody)}, nil
}

// SignReply signs an answer to the call with signature callSig.
func SignReply(secret []byte, h http.Header, callSig string, body []byte) {
	h.Set(HeaderReply, mac(secret, "emx-mesh-reply-v1", callSig, bodyHash(body)))
}

// VerifyReply checks an answer to a call this machine signed with callSig.
func VerifyReply(secret []byte, h http.Header, callSig string, body []byte) error {
	if !hmac.Equal([]byte(h.Get(HeaderReply)), []byte(mac(secret, "emx-mesh-reply-v1", callSig, bodyHash(body)))) {
		return fmt.Errorf("%w: the answer is not signed with the mesh secret", ErrRejected)
	}
	return nil
}

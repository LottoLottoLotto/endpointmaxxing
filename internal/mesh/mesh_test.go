package mesh

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func peers(n string) bool { return n == "b" }

func signed(t *testing.T, body []byte, now time.Time) http.Header {
	t.Helper()
	h := http.Header{}
	Sign(secret, h, "b", "alice", "ml", http.MethodPost, "/v1/chat/completions", body, now)
	return h
}

func TestCallVerifiesOnceAndCarriesWhoItActsFor(t *testing.T) {
	now := time.Now()
	body := []byte(`{"model":"m"}`)
	var v Verifier
	h := signed(t, body, now)
	c, err := v.Verify(secret, h, http.MethodPost, "/v1/chat/completions", peers, now)
	if err != nil || c.From != "b" || c.Client != "alice" || c.Team != "ml" || !c.BodyMatches(body) {
		t.Fatalf("verify: %v %+v", err, c)
	}
	if c.BodyMatches([]byte(`{"model":"other"}`)) {
		t.Fatal("a different body matched the signed one")
	}
	if _, err := v.Verify(secret, h, http.MethodPost, "/v1/chat/completions", peers, now); err == nil {
		t.Fatal("a replayed call was accepted")
	}
}

// Two identical requests in the same second (a client retrying, two users asking the same) are two calls.
func TestIdenticalCallsAreNotReplays(t *testing.T) {
	now := time.Now()
	var v Verifier
	for i := 0; i < 2; i++ {
		if _, err := v.Verify(secret, signed(t, []byte("same"), now), http.MethodPost, "/v1/chat/completions", peers, now); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

func TestCallsThatMustNotVerify(t *testing.T) {
	now := time.Now()
	cases := map[string]func(h http.Header) (http.Header, string, []byte, time.Time){
		"other path": func(h http.Header) (http.Header, string, []byte, time.Time) { return h, "/v1/messages", secret, now },
		"other secret": func(h http.Header) (http.Header, string, []byte, time.Time) {
			return h, "/v1/chat/completions", []byte("x" + string(secret)), now
		},
		"too old": func(h http.Header) (http.Header, string, []byte, time.Time) {
			return h, "/v1/chat/completions", secret, now.Add(2 * MaxSkew)
		},
		"client swapped": func(h http.Header) (http.Header, string, []byte, time.Time) {
			h.Set(HeaderClient, "admin")
			return h, "/v1/chat/completions", secret, now
		},
		"unknown sender": func(h http.Header) (http.Header, string, []byte, time.Time) {
			Sign(secret, h, "z", "", "", http.MethodPost, "/v1/chat/completions", nil, now)
			return h, "/v1/chat/completions", secret, now
		},
		"no mesh secret here": func(h http.Header) (http.Header, string, []byte, time.Time) {
			return h, "/v1/chat/completions", nil, now
		},
	}
	for name, mutate := range cases {
		var v Verifier
		h, uri, key, at := mutate(signed(t, nil, now))
		if _, err := v.Verify(key, h, http.MethodPost, uri, peers, at); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestReplyIsBoundToItsCallAndBody(t *testing.T) {
	h := http.Header{}
	SignReply(secret, h, "sig-1", []byte("state"))
	if err := VerifyReply(secret, h, "sig-1", []byte("state")); err != nil {
		t.Fatal(err)
	}
	if VerifyReply(secret, h, "sig-2", []byte("state")) == nil || VerifyReply(secret, h, "sig-1", []byte("forged")) == nil {
		t.Fatal("a reply verified for another call or body")
	}
}

func TestLoadSecretRefusesWeakOrShared(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	os.WriteFile(short, []byte("tooshort\n"), 0o600)
	if _, err := LoadSecret(short); err == nil {
		t.Error("a short secret was accepted")
	}
	good := filepath.Join(dir, "good")
	os.WriteFile(good, append(secret, '\n'), 0o600)
	if s, err := LoadSecret(good); err != nil || string(s) != string(secret) {
		t.Fatalf("good secret: %q %v", s, err)
	}
	if runtime.GOOS != "windows" { // owner-only is checked through the ACL there
		os.Chmod(good, 0o644)
		if _, err := LoadSecret(good); err == nil {
			t.Error("a secret readable by others was accepted")
		}
	}
}

package gateway

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lotto/substrate/internal/app"
	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/keys"
	"github.com/lotto/substrate/internal/paths"
	"github.com/lotto/substrate/internal/store"
)

func writeConfig(t *testing.T, yaml string) {
	t.Helper()
	if err := os.MkdirAll(paths.Config(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ConfigPath(), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
}

func chatBody(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
}

func requestsOf(t *testing.T, a *app.App, f store.Filter) []store.Request {
	t.Helper()
	db, err := a.DB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Requests(f)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestKeyLifecycleAndAuditorRole(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	backend := okBackend()
	defer backend.Close()
	instance(t, "m.fake", "m", backend.URL)
	past := time.Now().Add(-time.Hour)
	expired, _ := keys.Add(keys.Key{Name: "old", Role: keys.RoleUser, Expires: &past})
	disabled, _ := keys.Add(keys.Key{Name: "paused", Role: keys.RoleUser})
	if _, err := keys.Update("paused", func(k *keys.Key) error { k.Disabled = true; return nil }); err != nil {
		t.Fatal(err)
	}
	rotating, _ := keys.Add(keys.Key{Name: "svc", Role: keys.RoleUser})
	auditor, _ := keys.Add(keys.Key{Name: "audit", Role: keys.RoleAuditor})
	admin, _ := keys.Add(keys.Key{Name: "ops", Role: keys.RoleAdmin})
	g, srv := newGateway(t)

	if s := call(t, "POST", srv.URL+"/v1/chat/completions", expired, chatBody("m")); s != http.StatusUnauthorized {
		t.Fatalf("expired key: %d", s)
	}
	if s := call(t, "POST", srv.URL+"/v1/chat/completions", disabled, chatBody("m")); s != http.StatusUnauthorized {
		t.Fatalf("disabled key: %d", s)
	}

	fresh, k, err := keys.Rotate("svc", time.Hour)
	if err != nil || k.PrevExpires == nil {
		t.Fatalf("rotate: %+v %v", k, err)
	}
	for name, secret := range map[string]string{"old secret in grace": rotating, "new secret": fresh} {
		if s := call(t, "POST", srv.URL+"/v1/chat/completions", secret, chatBody("m")); s != http.StatusOK {
			t.Fatalf("%s: %d", name, s)
		}
	}
	if _, _, err := keys.Rotate("svc", 0); err != nil { // no grace: both earlier secrets stop working
		t.Fatal(err)
	}
	if s := call(t, "POST", srv.URL+"/v1/chat/completions", fresh, chatBody("m")); s != http.StatusUnauthorized {
		t.Fatalf("secret rotated out without grace: %d", s)
	}

	// Auditors read everything, but cannot send prompts or change anything.
	if s := call(t, "POST", srv.URL+"/v1/chat/completions", auditor, chatBody("m")); s != http.StatusForbidden {
		t.Fatalf("auditor inference: %d", s)
	}
	if s := call(t, "POST", srv.URL+"/substrate/v1/stop", auditor, `{"id":"m.fake"}`); s != http.StatusForbidden {
		t.Fatalf("auditor stop: %d", s)
	}
	for _, path := range []string{"/substrate/v1/report", "/substrate/v1/keys", "/substrate/v1/events", "/metrics"} {
		if s := call(t, "GET", srv.URL+path, auditor, ""); s != http.StatusOK {
			t.Fatalf("auditor GET %s: %d", path, s)
		}
	}
	if s := call(t, "POST", srv.URL+"/substrate/v1/reload", auditor, ""); s != http.StatusForbidden {
		t.Fatalf("auditor reload: %d", s)
	}
	if s := call(t, "POST", srv.URL+"/substrate/v1/reload", admin, ""); s != http.StatusOK {
		t.Fatalf("admin reload: %d", s)
	}
	g.Close()

	denied := requestsOf(t, g.App, store.Filter{ErrorKind: "auth_failed"})
	names := map[string]bool{}
	for _, r := range denied {
		names[r.Client] = true
	}
	// A secret rotated out without grace is simply unknown; expired and disabled keys are named.
	if !names["old"] || !names["paused"] {
		t.Fatalf("refused keys must be recorded under their names: %v", names)
	}
	infos, err := KeyInfos(mustDB(t, g.App))
	if err != nil {
		t.Fatal(err)
	}
	for _, ki := range infos {
		switch ki.Name {
		case "svc":
			if ki.LastUsed == nil {
				t.Errorf("svc served requests but has no last use")
			}
		case "old", "paused":
			if ki.LastUsed != nil || ki.Status == "active" {
				t.Errorf("%s: last used %v, status %s (refused requests are not use)", ki.Name, ki.LastUsed, ki.Status)
			}
		}
	}
}

func mustDB(t *testing.T, a *app.App) *store.DB {
	t.Helper()
	db, err := a.DB()
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestTeamBudgetsCostAndModelAccess(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	backend := okBackend() // every request: 5 prompt + 5 completion tokens
	defer backend.Close()
	instance(t, "m.fake", "m", backend.URL)
	writeConfig(t, `
pricing:
    currency: EUR
    models:
        m: {prompt: 1000, completion: 2000}   # 0.015 per request
teams:
    - name: tokens
      tokens_per_day: 15
    - name: money
      budget_per_month: 0.02
    - name: narrow
      models: [other]
`)
	tok, _ := keys.Add(keys.Key{Name: "t1", Role: keys.RoleUser, Team: "tokens"})
	mon, _ := keys.Add(keys.Key{Name: "m1", Role: keys.RoleUser, Team: "money"})
	nar, _ := keys.Add(keys.Key{Name: "n1", Role: keys.RoleUser, Team: "narrow"})
	g, srv := newGateway(t)

	seq := func(key string, want ...int) {
		t.Helper()
		for i, w := range want {
			if s := call(t, "POST", srv.URL+"/v1/chat/completions", key, chatBody("m")); s != w {
				t.Fatalf("request %d: status %d, want %d", i+1, s, w)
			}
		}
	}
	seq(tok, 200, 200, 429) // 10, then 20 tokens >= 15: the third is refused
	seq(mon, 200, 200, 429) // 0.015, then 0.030 >= 0.02
	if s := call(t, "POST", srv.URL+"/v1/chat/completions", nar, chatBody("m")); s != http.StatusForbidden {
		t.Fatalf("team model allowlist: %d", s)
	}
	g.Close()

	rows := requestsOf(t, g.App, store.Filter{Team: "money"})
	if len(rows) != 3 || rows[2].Cost == nil || *rows[2].Cost < 0.0149 || *rows[2].Cost > 0.0151 || rows[0].ErrorKind != "budget_exceeded" {
		t.Fatalf("money team requests: %+v", rows)
	}
	usage, err := mustDB(t, g.App).Usage("team", store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var moneyCost float64
	for _, u := range usage {
		if u.Key == "money" && u.Cost != nil {
			moneyCost = *u.Cost
		}
	}
	if moneyCost < 0.0299 || moneyCost > 0.0301 {
		t.Fatalf("usage by team: money cost %v", moneyCost)
	}
	deadline := time.Now().Add(2 * time.Second) // alerts are recorded off the request path
	for {
		evs, _ := mustDB(t, g.App).Events(store.Filter{})
		alerts := 0
		for _, e := range evs {
			if e.Kind == "budget_alert" {
				alerts++
			}
		}
		if alerts == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("budget alerts: %d, want one per team that crossed 80%%", alerts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	statuses, err := TeamStatuses(g.App.Config(), mustDB(t, g.App), nil)
	if err != nil || len(statuses) != 3 || statuses[0].Limits[0].Used != 20 {
		t.Fatalf("team statuses: %+v %v", statuses, err)
	}
}

func TestHotReload(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	backend := okBackend()
	defer backend.Close()
	instance(t, "m.fake", "m", backend.URL)
	writeConfig(t, "gateway:\n    max_queue: 64\n")
	a, err := app.Open()
	if err != nil {
		t.Fatal(err)
	}
	g := New(a, log.New(io.Discard, "", 0))
	defer g.Close()
	srv := httptest.NewServer(g.Handler())
	defer srv.Close()
	if s := call(t, "POST", srv.URL+"/v1/chat/completions", "", chatBody("m")); s != http.StatusOK {
		t.Fatalf("before reload: %d", s)
	}

	writeConfig(t, `
policy:
    enabled: true
    rules:
        - name: nohi
          action: block
          keywords: [hi]
`)
	if err := g.Reload(); err != nil {
		t.Fatal(err)
	}
	if s := call(t, "POST", srv.URL+"/v1/chat/completions", "", chatBody("m")); s != http.StatusBadRequest {
		t.Fatalf("new blocking rule after reload: %d", s)
	}

	writeConfig(t, "policy:\n    rules:\n        - name: broken\n          patterns: ['(']\n")
	if err := g.Reload(); err == nil {
		t.Fatal("an invalid config reloaded")
	}
	if s := call(t, "POST", srv.URL+"/v1/chat/completions", "", chatBody("m")); s != http.StatusBadRequest {
		t.Fatalf("an invalid config must keep the running rules: %d", s)
	}
	if st := g.reloadStatus(); st["failures"].(int64) != 1 || st["last_error"] == nil {
		t.Fatalf("reload status %v", st)
	}
}

// selfSigned writes a certificate for 127.0.0.1 to dir and returns the paths.
func selfSigned(t *testing.T, dir, serial string) (certFile, keyFile string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := new(big.Int).SetString(serial, 10)
	tmpl := &x509.Certificate{SerialNumber: n, Subject: pkix.Name{CommonName: "substrate.example"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"substrate.example"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(priv)
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(certFile+".tmp", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	os.Rename(certFile+".tmp", certFile)
	return certFile, keyFile
}

func TestTLSServesAndPicksUpRenewedCertificate(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := selfSigned(t, dir, "1")
	conf, err := TLSConfig(config.TLS{Cert: certFile, Key: keyFile}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", conf)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })}
	go srv.Serve(ln)
	defer srv.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	gw := &config.Gateway{Host: "127.0.0.1", Port: port, TLS: config.TLS{Cert: certFile, Key: keyFile}}

	// serial is the serial number of the certificate the server presents to a client trusting the files on disk.
	serial := func() (string, error) {
		base, client := Client(gw)
		if !strings.HasPrefix(base, "https://") {
			t.Fatalf("client base %s", base)
		}
		client.Transport.(*http.Transport).DisableKeepAlives = true
		resp, err := client.Get(base + "/health")
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		return resp.TLS.PeerCertificates[0].SerialNumber.String(), nil
	}
	if s, err := serial(); s != "1" {
		t.Fatalf("serving certificate %s: %v", s, err)
	}
	if resp, err := http.Get("http://" + ln.Addr().String() + "/health"); err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK || string(body) == "ok" {
			t.Fatal("plain HTTP reached the handler on the TLS port")
		}
	}

	time.Sleep(10 * time.Millisecond)
	selfSigned(t, dir, "2") // renewal in place
	future := time.Now().Add(time.Second)
	os.Chtimes(certFile, future, future)
	os.Chtimes(keyFile, future, future)
	// Until the server re-reads the files it still presents serial 1, which the client (now trusting the new
	// self-signed certificate) rejects; with a CA-signed certificate both would verify.
	deadline := time.Now().Add(certRecheck + 3*time.Second)
	for {
		if s, _ := serial(); s == "2" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the renewed certificate was not picked up")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

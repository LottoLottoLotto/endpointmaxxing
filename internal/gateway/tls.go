package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
)

// certReloader serves the certificate in cert/key, re-reading the files when either changes
// (checked at most every few seconds), so a renewed certificate is picked up without a restart.
type certReloader struct {
	certFile, keyFile string

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   [2]time.Time
	checked time.Time
	log     func(string, ...any)
}

const certRecheck = 5 * time.Second

func (c *certReloader) load() error {
	cs, err := os.Stat(c.certFile)
	if err != nil {
		return err
	}
	ks, err := os.Stat(c.keyFile)
	if err != nil {
		return err
	}
	stamp := [2]time.Time{cs.ModTime(), ks.ModTime()}
	if c.cert != nil && stamp == c.stamp {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return err
	}
	if c.cert != nil {
		c.log("tls: loaded the renewed certificate from %s", c.certFile)
	}
	c.cert, c.stamp = &cert, stamp
	return nil
}

func (c *certReloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.checked) > certRecheck {
		c.checked = time.Now()
		if err := c.load(); err != nil && c.cert != nil {
			c.log("tls: %v; keeping the current certificate", err) // a half-written renewal: retry later
		} else if err != nil {
			return nil, err
		}
	}
	return c.cert, nil
}

// TLSConfig builds the server TLS configuration from gateway.tls; it fails when the files are unusable.
func TLSConfig(t config.TLS, logf func(string, ...any)) (*tls.Config, error) {
	r := &certReloader{certFile: t.Cert, keyFile: t.Key, log: logf, checked: time.Now()}
	if err := r.load(); err != nil {
		return nil, fmt.Errorf("gateway.tls: %w", err)
	}
	c := &tls.Config{GetCertificate: r.get, MinVersion: tls.VersionTLS12}
	if t.MinVersion == "1.3" {
		c.MinVersion = tls.VersionTLS13
	}
	if t.ClientCA != "" {
		pem, err := os.ReadFile(t.ClientCA)
		if err != nil {
			return nil, fmt.Errorf("gateway.tls.client_ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("gateway.tls.client_ca: %s holds no PEM certificates", t.ClientCA)
		}
		c.ClientCAs, c.ClientAuth = pool, tls.RequireAndVerifyClientCert
	}
	return c, nil
}

// Client returns the gateway's base URL and an HTTP client for local commands (chat, reload) to reach it.
// With TLS the client trusts the system roots plus the gateway's own certificate (self-signed setups).
// Under mutual TLS, local commands have no client certificate and cannot use the gateway.
func Client(c *config.Gateway) (string, *http.Client) {
	host := c.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	base := "http://" + net.JoinHostPort(host, fmt.Sprint(c.Port))
	client := &http.Client{}
	if c.TLS.Enabled() {
		base = "https://" + net.JoinHostPort(host, fmt.Sprint(c.Port))
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		if pem, err := os.ReadFile(c.TLS.Cert); err == nil {
			pool.AppendCertsFromPEM(pem)
		}
		// The certificate names the public host; locally we connect by loopback address, so verify the chain
		// against the configured certificate rather than the name.
		client.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, InsecureSkipVerify: true,
			VerifyPeerCertificate: verifyChain(pool)}}
	}
	return base, client
}

// verifyChain checks the presented chain against roots, ignoring the host name.
func verifyChain(roots *x509.CertPool) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return fmt.Errorf("tls: no server certificate")
		}
		certs := make([]*x509.Certificate, len(raw))
		for i, b := range raw {
			c, err := x509.ParseCertificate(b)
			if err != nil {
				return err
			}
			certs[i] = c
		}
		inter := x509.NewCertPool()
		for _, c := range certs[1:] {
			inter.AddCert(c)
		}
		_, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter})
		return err
	}
}

package gateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/keys"
	"github.com/lotto/substrate/internal/store"
)

// errQuota is answered with 429.
var errQuota = errors.New("quota exceeded")

// principal is the authenticated caller.
type principal struct {
	name string // key name; "admin" for the master key; "" when the gateway is open
	role string
	key  *keys.Key // nil for the master key and open mode
}

func (p *principal) allows(model string) bool { return p.key == nil || p.key.Allows(model) }

// client is the usage attribution: the key name, or the self-reported client in open/master mode.
func (p *principal) client(r *http.Request) string {
	if p.key != nil {
		return p.name
	}
	return r.Header.Get("X-Substrate-Client")
}

type ctxKey struct{}

func principalOf(r *http.Request) *principal {
	if p, ok := r.Context().Value(ctxKey{}).(*principal); ok {
		return p
	}
	return &principal{role: keys.RoleAdmin}
}

// Route access levels.
const (
	roleAny     = iota // any authenticated caller
	roleAdmin          // admin keys only
	roleMetrics        // admin, or anyone when metrics_public
)

// authState caches keys.yaml and tracks per-key rate and token use.
type authState struct {
	mu      sync.Mutex
	file    *keys.File
	mtime   time.Time
	size    int64
	byHash  map[string]*keys.Key
	minute  map[string][]time.Time // request times in the last minute, per key
	tokens  map[string]tokenUse    // tokens used today, per key
	dbFunc  func() (*store.DB, error)
	loadErr error
}

type tokenUse struct {
	day     string
	used    int64
	checked time.Time
}

// reload re-reads keys.yaml when it changed.
func (a *authState) reload() error {
	st, err := os.Stat(keys.Path())
	if err != nil {
		if os.IsNotExist(err) {
			a.file, a.byHash = &keys.File{}, map[string]*keys.Key{}
			return nil
		}
		return err
	}
	if a.file != nil && st.ModTime().Equal(a.mtime) && st.Size() == a.size {
		return nil
	}
	f, err := keys.Load()
	if err != nil {
		return err
	}
	a.file, a.mtime, a.size = f, st.ModTime(), st.Size()
	a.byHash = map[string]*keys.Key{}
	for i := range f.Keys {
		a.byHash[f.Keys[i].Hash] = &f.Keys[i]
	}
	return nil
}

func secretOf(r *http.Request) string {
	if k := r.Header.Get("x-api-key"); k != "" {
		return k
	}
	if b, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(b)
	}
	return ""
}

// authenticate resolves the caller. With no master key and no keys.yaml entries the gateway is open.
func (g *Gateway) authenticate(r *http.Request) (*principal, error) {
	g.auth.mu.Lock()
	err := g.auth.reload()
	byHash := g.auth.byHash
	g.auth.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("keys.yaml: %w", err)
	}
	secret := secretOf(r)
	if g.APIKey != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(g.APIKey)) == 1 {
		return &principal{name: "admin", role: keys.RoleAdmin}, nil
	}
	if k := byHash[keys.Hash(secret)]; k != nil && secret != "" {
		return &principal{name: k.Name, role: k.Role, key: k}, nil
	}
	if g.APIKey == "" && len(byHash) == 0 {
		return &principal{role: keys.RoleAdmin}, nil // open (loopback-only, enforced by serve)
	}
	return nil, errs.New(errs.Forbidden, "invalid API key")
}

// authed wraps h with authentication and a role check.
func (g *Gateway) authed(level int, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if level == roleMetrics && g.Cfg.MetricsPublic {
			h(w, r)
			return
		}
		p, err := g.authenticate(r)
		if err != nil {
			status := http.StatusUnauthorized
			if errs.CodeOf(err) != errs.Forbidden {
				status = http.StatusInternalServerError
			}
			apiError(w, status, errType(status, err), err.Error())
			g.logDenied(r, status, err.Error())
			return
		}
		if (level == roleAdmin || level == roleMetrics) && p.role != keys.RoleAdmin {
			msg := fmt.Sprintf("key %s (role %s) may not use %s", p.name, p.role, r.URL.Path)
			apiError(w, http.StatusForbidden, "permission_error", msg)
			g.logDenied(r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)), http.StatusForbidden, msg)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	}
}

// logDenied records a request refused before reaching its handler (bad key, or a role too low for the route).
func (g *Gateway) logDenied(r *http.Request, status int, msg string) {
	now := time.Now()
	api, _ := apiForPath(r.URL.Path)
	rec := &store.Request{ID: newID(), TS: now.UnixMilli(), API: api, Path: r.URL.Path, UserAgent: r.UserAgent(),
		Remote: clientIP(r), Status: status, Error: msg, ErrorKind: gatewayErrorKind(status, nil)}
	if p, ok := r.Context().Value(ctxKey{}).(*principal); ok {
		rec.Client = p.name
	}
	if status == http.StatusInternalServerError {
		rec.ErrorKind = "gateway_error"
	}
	d := int64(0)
	rec.DurationMS = &d
	g.logRequest(rec)
}

func today() (string, time.Time) {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	return start.Format("2006-01-02"), start
}

// tokenRecheck bounds how stale the day's token total may be before re-reading the database.
const tokenRecheck = 30 * time.Second

// admit enforces the key's requests-per-minute and tokens-per-day limits.
func (a *authState) admit(p *principal) error {
	if p.key == nil {
		return nil
	}
	k := p.key
	a.mu.Lock()
	defer a.mu.Unlock()
	if k.RPM > 0 {
		if a.minute == nil {
			a.minute = map[string][]time.Time{}
		}
		cut := time.Now().Add(-time.Minute)
		recent := a.minute[k.Name][:0]
		for _, t := range a.minute[k.Name] {
			if t.After(cut) {
				recent = append(recent, t)
			}
		}
		if len(recent) >= k.RPM {
			a.minute[k.Name] = recent
			return errs.Wrap(errs.Capacity, fmt.Errorf("key %s: %d requests per minute: %w", k.Name, k.RPM, errQuota))
		}
		a.minute[k.Name] = append(recent, time.Now())
	}
	if k.TokensPerDay > 0 {
		if a.tokens == nil {
			a.tokens = map[string]tokenUse{}
		}
		day, start := today()
		u := a.tokens[k.Name]
		if u.day != day || time.Since(u.checked) > tokenRecheck {
			u = tokenUse{day: day, checked: time.Now()}
			if a.dbFunc != nil {
				if db, err := a.dbFunc(); err == nil {
					u.used, _ = db.TokensSince(k.Name, start)
				}
			}
			a.tokens[k.Name] = u
		}
		if u.used >= k.TokensPerDay {
			return errs.Wrap(errs.Capacity, fmt.Errorf("key %s: %d of %d tokens used today: %w", k.Name, u.used, k.TokensPerDay, errQuota))
		}
	}
	return nil
}

// charge adds a finished request's tokens to its key's running day total.
func (a *authState) charge(r *store.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.tokens[r.Client]
	if !ok {
		return
	}
	if r.PromptTokens != nil {
		u.used += *r.PromptTokens
	}
	if r.CompletionTokens != nil {
		u.used += *r.CompletionTokens
	}
	a.tokens[r.Client] = u
}

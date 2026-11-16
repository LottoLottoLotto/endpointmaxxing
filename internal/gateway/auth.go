package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/keys"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/mesh"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
)

// errQuota is answered with 429.
var errQuota = errors.New("quota exceeded")

// principal is the authenticated caller.
type principal struct {
	name string // key name; "admin" for the master key; "" when the gateway is open
	role string
	key  *keys.Key // nil for the master key and open mode
	open bool      // open mode: no master key and no keys, so nothing was checked
	// call is a request another machine of the mesh forwarded (role roleMeshCall): that machine checked the
	// client's key, model access and limits; name is the client it acts for.
	call *mesh.Call
}

// roleMeshCall is the role of a forwarded mesh call: inference only.
const roleMeshCall = "mesh"

// team is the key's team in cfg; nil without a key, without a team, or for a team not configured.
func (p *principal) team(cfg *config.Config) *config.Team {
	if p.key == nil || p.key.Team == "" {
		return nil
	}
	t, _ := cfg.Team(p.key.Team)
	return t
}

// allows reports whether the key, and its team, may use model.
func (p *principal) allows(model string, cfg *config.Config) bool {
	if p.key == nil {
		return true
	}
	t := p.team(cfg)
	return p.key.Allows(model) && (t == nil || t.Allows(model))
}

// client is the usage attribution: the key name, or the self-reported client in open/master mode.
func (p *principal) client(r *http.Request) string {
	if p.key != nil || p.call != nil {
		return p.name
	}
	return r.Header.Get("X-Emx-Client")
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
	roleInfer          // inference: user and admin keys (not auditors)
	roleRead           // read-only observability: admin and auditor keys
	roleAdmin          // admin keys only
	roleMetrics        // admin or auditor, or anyone when metrics_public
)

// permits reports whether role may use a route of level.
func permits(level int, role string) bool {
	switch level {
	case roleInfer:
		return role == keys.RoleUser || role == keys.RoleAdmin || role == roleMeshCall
	case roleAny:
		return role != roleMeshCall
	case roleRead, roleMetrics:
		return role == keys.RoleAdmin || role == keys.RoleAuditor
	case roleAdmin:
		return role == keys.RoleAdmin
	}
	return true
}

// authState caches keys.yaml and tracks per-key rate and token use.
type authState struct {
	mu     sync.Mutex
	file   *keys.File
	mtime  time.Time
	size   int64
	byHash map[string]*keys.Key   // current and (during a rotation grace period) previous secret hashes
	minute map[string][]time.Time // request times in the last minute, per key
	tokens map[string]tokenUse    // tokens used today, per key
	teams  map[string]*teamUse    // team totals for the day and month
	dbFunc func() (*store.DB, error)
	// peers returns what keys and teams used on the other machines of a mesh (nil outside one): limits are
	// for the whole mesh, whichever machine a request enters.
	peers func() (keysDay map[string]int64, teamsDay, teamsMonth map[string]store.TeamUse)
}

// peerUse is what keys and teams used on other machines of the mesh.
type peerUse struct {
	keysDay              map[string]int64
	teamsDay, teamsMonth map[string]store.TeamUse
}

func (a *authState) peerUse() peerUse {
	if a.peers == nil {
		return peerUse{}
	}
	kd, td, tm := a.peers()
	return peerUse{kd, td, tm}
}

// team is team's totals here plus on the other machines.
func (pu peerUse) team(name string, u *teamUse) *teamUse {
	c := *u
	c.dayTokens += pu.teamsDay[name].Tokens
	c.monthTokens += pu.teamsMonth[name].Tokens
	c.monthCost += pu.teamsMonth[name].Cost
	return &c
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
		k := &f.Keys[i]
		a.byHash[k.Hash] = k
		if k.PrevHash != "" {
			a.byHash[k.PrevHash] = k
		}
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

// errKeyRefused is a known key that may not be used now (expired, disabled, or an old secret past its grace).
type errKeyRefused struct{ name, why string }

func (e errKeyRefused) Error() string { return fmt.Sprintf("API key %s is %s", e.name, e.why) }

// authenticate resolves the caller. With no master key and no keys.yaml entries the gateway is open.
func (g *Gateway) authenticate(r *http.Request) (*principal, error) {
	g.auth.mu.Lock()
	err := g.auth.reload()
	byHash := g.auth.byHash
	g.auth.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("keys.yaml: %w", err)
	}
	if mesh.IsCall(r.Header) {
		m := g.meshCfg()
		call, err := g.mesh.verifier.Verify(g.meshSecret(), r.Header, r.Method, r.URL.RequestURI(), func(n string) bool { _, ok := m.Peer(n); return ok }, time.Now())
		if err != nil {
			return nil, errs.Wrap(errs.Forbidden, err)
		}
		role := roleMeshCall // forwarded inference
		if call.Role != "" { // a management call an admin, auditor or user made on the peer, relayed here
			if !slices.Contains(keys.Roles, call.Role) {
				return nil, errs.New(errs.Forbidden, "mesh call: unknown role %q", call.Role)
			}
			role = call.Role
		}
		return &principal{name: call.Client, role: role, call: call}, nil
	}
	secret := secretOf(r)
	if g.APIKey != "" && subtle.ConstantTimeCompare([]byte(secret), []byte(g.APIKey)) == 1 {
		return &principal{name: "admin", role: keys.RoleAdmin}, nil
	}
	hash := keys.Hash(secret)
	if k := byHash[hash]; k != nil && secret != "" {
		now := time.Now()
		switch {
		case !k.Matches(hash, now):
			return nil, errs.Wrap(errs.Forbidden, errKeyRefused{k.Name, "a rotated-out secret past its grace period"})
		case k.Status(now) != "active":
			return nil, errs.Wrap(errs.Forbidden, errKeyRefused{k.Name, k.Status(now)})
		}
		return &principal{name: k.Name, role: k.Role, key: k}, nil
	}
	if g.APIKey == "" && len(byHash) == 0 {
		if !g.AllowOpen { // decided when serve started: removing the last key must not open a networked gateway
			return nil, errs.New(errs.Forbidden, "no API keys exist and this gateway does not run open; create one (emx key add) or restart serve with a master key")
		}
		return &principal{role: keys.RoleAdmin, open: true}, nil
	}
	return nil, errs.New(errs.Forbidden, "invalid API key")
}

// authed wraps h with authentication and a role check.
func (g *Gateway) authed(level int, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if level == roleMetrics && g.gw().MetricsPublic {
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
			var refused errKeyRefused
			client := ""
			if errors.As(err, &refused) {
				client = refused.name
			}
			g.logDenied(r, client, status, err.Error())
			return
		}
		deny := func(status int, typ, msg string) {
			apiError(w, status, typ, msg)
			g.logDenied(r, p.name, status, msg)
		}
		if p.open {
			if msg := g.openModeRefusal(r); msg != "" {
				deny(http.StatusForbidden, "permission_error", msg)
				return
			}
		}
		if !permits(level, p.role) {
			deny(http.StatusForbidden, "permission_error", fmt.Sprintf("key %s (role %s) may not use %s", p.name, p.role, clip(r.URL.Path, maxStoredField)))
			return
		}
		if needsJSON(r) {
			deny(http.StatusUnsupportedMediaType, "invalid_request_error", "management and settings calls take a JSON body (Content-Type: application/json)")
			return
		}
		if p.call != nil && level != roleInfer { // inference checks its (large) body itself, against its budget
			body, err := io.ReadAll(io.LimitReader(r.Body, maxRelayBody+1))
			if err != nil || len(body) > maxRelayBody || !p.call.BodyMatches(body) {
				deny(http.StatusUnauthorized, "authentication_error", "mesh call: the body is not the one signed")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		// ?node= sends a management call to another machine of the mesh; a relayed call is never relayed again.
		if node := r.URL.Query().Get("node"); node != "" && level != roleInfer && node != g.meshCfg().Node {
			if p.call != nil {
				deny(http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("this is %s, not %s: a relayed call is not relayed again", g.meshCfg().Node, node))
				return
			}
			g.relay(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)), node)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	}
}

// logDenied records a request refused before reaching its handler (bad, expired or disabled key, a role
// too low for the route, or a request open mode does not answer); client is the key's name when it is known.
// Refused keys (401) cost an attacker nothing to produce: only maxStoredDenials a minute are stored, the
// rest are counted in emx_auth_denials_unstored_total.
func (g *Gateway) logDenied(r *http.Request, client string, status int, msg string) {
	now := time.Now()
	if status == http.StatusUnauthorized && !g.denials.allow(now) {
		g.metrics.denialUnstored()
		return
	}
	api, _ := apiForPath(r.URL.Path)
	rec := &store.Request{ID: newID(), TS: now.UnixMilli(), API: api, Path: clip(r.URL.Path, maxStoredField),
		UserAgent: clip(r.UserAgent(), maxStoredField), Remote: clientIP(r), Client: client, Status: status,
		Error: clip(msg, maxStoredError), ErrorKind: gatewayErrorKind(status, nil)}
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

// admit enforces the key's requests-per-minute and tokens-per-day limits. It returns what the key may
// still use (for rate-limit headers) and, when it refuses, how long until the request could succeed.
func (a *authState) admit(p *principal) (quota, time.Duration, error) {
	q := quota{tokLimit: -1}
	if p.key == nil {
		return q, 0, nil
	}
	k := p.key
	elsewhere := a.peerUse().keysDay[k.Name]
	a.mu.Lock()
	defer a.mu.Unlock()
	if k.RPM > 0 {
		if a.minute == nil {
			a.minute = map[string][]time.Time{}
		}
		now := time.Now()
		cut := now.Add(-time.Minute)
		recent := a.minute[k.Name][:0]
		for _, t := range a.minute[k.Name] {
			if t.After(cut) {
				recent = append(recent, t)
			}
		}
		q.rpmLimit = k.RPM
		if len(recent) >= k.RPM {
			a.minute[k.Name] = recent
			wait := recent[0].Add(time.Minute).Sub(now) // the oldest request in the window leaves it
			return q, wait, errs.Wrap(errs.Capacity, fmt.Errorf("key %s: %d requests per minute: %w", k.Name, k.RPM, errQuota))
		}
		recent = append(recent, now)
		a.minute[k.Name] = recent
		q.rpmRemaining, q.rpmReset = k.RPM-len(recent), recent[0].Add(time.Minute).Sub(now)
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
		used := u.used + elsewhere
		q.tighter(k.TokensPerDay, used, untilNextDay())
		if used >= k.TokensPerDay {
			return q, untilNextDay(), errs.Wrap(errs.Capacity, fmt.Errorf("key %s: %d of %d tokens used today: %w", k.Name, used, k.TokensPerDay, errQuota))
		}
	}
	return q, 0, nil
}

// charge adds a finished request's tokens to its key's day total and its team's totals, and returns
// the team budget alerts it crossed.
func (a *authState) charge(r *store.Request, team *config.Team) []budgetAlert {
	pu := a.peerUse()
	a.mu.Lock()
	defer a.mu.Unlock()
	if u, ok := a.tokens[r.Client]; ok {
		if r.PromptTokens != nil {
			u.used += *r.PromptTokens
		}
		if r.CompletionTokens != nil {
			u.used += *r.CompletionTokens
		}
		a.tokens[r.Client] = u
	}
	return a.chargeTeam(r, team, pu)
}

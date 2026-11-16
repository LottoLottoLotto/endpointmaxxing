package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/keys"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/mesh"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/runtime"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
)

// A mesh joins gateways on several machines into one endpoint. Each gateway asks its peers every
// mesh.interval what they serve (GET /emx/v1/mesh/state); a request for a model this machine does not have
// ready, or has busier than a peer has it, goes to a peer that has it running, or else to one that can
// start it. The machine a request enters authenticates it, applies its key's and team's limits (counting
// what the key and team used on every machine), and meters it; the machine that serves it records it too,
// marked with where it came from, and never forwards it again. One machine holds the keys, teams, prices
// and aliases (mesh.keys_from); the others copy them (GET /emx/v1/mesh/shared) and refuse to change them.

// meshEngine marks the stand-in instance of a forward to a peer.
const meshEngine = "mesh"

// peerView is what this gateway knows of one peer.
type peerView struct {
	Name     string     `json:"name"`
	URL      string     `json:"url"`
	Up       bool       `json:"up"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
	Error    string     `json:"error,omitempty"`
	RTTMS    int64      `json:"rtt_ms,omitempty"`
	Version  string     `json:"version,omitempty"`
	Models   []string   `json:"models"`
	state    *peerState
	fails    int
}

// peerState is a gateway's answer to GET /emx/v1/mesh/state.
type peerState struct {
	Node    string        `json:"node"`
	Version string        `json:"version"`
	Models  []*modelEntry `json:"models"`
	// Serving says, per model, how it can be served there: what routing compares.
	Serving map[string]servingState `json:"serving"`
	// Usage is what clients sent to that machine today and this month, for limits mesh-wide.
	Usage meshUsage `json:"usage"`
}

type servingState struct {
	Ready  bool     `json:"ready"` // an instance answers now
	APIs   []string `json:"apis,omitempty"`
	Active int      `json:"active"`
	Queued int      `json:"queued"`
	Serves int      `json:"serves"` // requests it works on at once (at least 1)
	// Speed is the model's measured decode speed there (tok/s over the last 30 days; 0: not measured).
	Speed float64 `json:"speed,omitempty"`
}

type meshUsage struct {
	Day        string                   `json:"day"`
	Month      string                   `json:"month"`
	KeysDay    map[string]int64         `json:"keys_day"`
	TeamsDay   map[string]store.TeamUse `json:"teams_day"`
	TeamsMonth map[string]store.TeamUse `json:"teams_month"`
}

// sharedCopy is the answer to GET /emx/v1/mesh/shared.
type sharedCopy struct {
	Keys   string        `json:"keys"` // keys.yaml: names, roles, limits and secret hashes, no secret
	Shared config.Shared `json:"shared"`
}

type meshState struct {
	mu       sync.Mutex
	peers    map[string]*peerView
	secret   []byte
	client   *http.Client
	clientOf string // the TLS settings client was made for
	verifier mesh.Verifier
	// usage is this machine's own entered usage, cached for an interval.
	usage   meshUsage
	usageAt time.Time
	// sync is the last copy of keys and shared settings (followers).
	syncAt  *time.Time
	syncErr string
	synced  []byte
	// speeds caches this machine's measured decode speed per model (routing).
	speeds  map[string]float64
	speedAt time.Time
	wake    chan struct{}
}

func (g *Gateway) meshCfg() config.Mesh { return g.App.Config().Mesh }

// meshSecret returns the current secret, re-read when the file changes (nil outside a mesh).
func (g *Gateway) meshSecret() []byte {
	g.mesh.mu.Lock()
	defer g.mesh.mu.Unlock()
	return g.mesh.secret
}

// meshRefresh re-reads the secret and rebuilds the peer client when the settings changed.
func (g *Gateway) meshRefresh() error {
	m := g.meshCfg()
	if !m.Enabled() {
		g.mesh.mu.Lock()
		g.mesh.secret, g.mesh.peers = nil, nil
		g.mesh.mu.Unlock()
		return nil
	}
	secret, err := mesh.LoadSecret(m.Secret())
	tlsKey := fmt.Sprint(m.CAFile, "|", g.gw().TLS.Cert, "|", g.gw().TLS.Key, "|", g.gw().UpstreamHeaderTimeout)
	g.mesh.mu.Lock()
	defer g.mesh.mu.Unlock()
	if err != nil {
		return err // keep the secret loaded before
	}
	g.mesh.secret = secret
	if g.mesh.client == nil || g.mesh.clientOf != tlsKey {
		c, err := meshClient(m.CAFile, g.gw().TLS, time.Duration(g.gw().UpstreamHeaderTimeout))
		if err != nil {
			return err
		}
		g.mesh.client, g.mesh.clientOf = c, tlsKey
	}
	if g.mesh.peers == nil {
		g.mesh.peers = map[string]*peerView{}
	}
	for name := range g.mesh.peers {
		if _, ok := m.Peer(name); !ok {
			delete(g.mesh.peers, name)
		}
	}
	for _, p := range m.Peers {
		v := g.mesh.peers[p.Name]
		if v == nil || v.URL != strings.TrimSuffix(p.URL, "/") {
			g.mesh.peers[p.Name] = &peerView{Name: p.Name, URL: strings.TrimSuffix(p.URL, "/"), Models: []string{}}
		}
	}
	return nil
}

// meshClient trusts the system's roots and ca_file, and presents this gateway's own certificate (for peers
// that require client certificates).
func meshClient(caFile string, t config.TLS, headerTimeout time.Duration) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("mesh.ca_file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("mesh.ca_file %s holds no PEM certificate", caFile)
		}
	}
	conf := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if t.Cert != "" && t.Key != "" {
		if cert, err := tls.LoadX509KeyPair(t.Cert, t.Key); err == nil {
			conf.Certificates = []tls.Certificate{cert}
		}
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       conf,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: headerTimeout, // as for local engines: the peer may be starting the model
	}}, nil
}

// meshLoop polls the peers and, on a follower, copies keys and shared settings, until ctx ends.
func (g *Gateway) meshLoop(ctx context.Context) {
	for {
		if err := g.meshRefresh(); err != nil {
			g.Log.Printf("mesh: %v", err)
		}
		if g.meshCfg().Enabled() {
			g.pollPeers(ctx)
			if g.meshCfg().Follower() {
				g.syncShared(ctx)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(g.meshCfg().Every()):
		case <-g.mesh.wakeCh():
		}
	}
}

// wakeCh is signalled when the machine that holds the keys says they changed.
func (s *meshState) wakeCh() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	return s.wake
}

// meshSyncNow answers the key holder's note that keys or shared settings changed: copy them now.
func (g *Gateway) meshSyncNow(*http.Request) (any, error) {
	select {
	case g.mesh.wakeCh() <- struct{}{}:
	default:
	}
	return map[string]bool{"ok": true}, nil
}

// tellFollowers asks the other machines to copy the keys and shared settings now, after a change here.
func (g *Gateway) tellFollowers() {
	m := g.meshCfg()
	if !m.Enabled() || m.Follower() {
		return
	}
	for _, p := range g.upPeers() {
		go func() {
			var out map[string]bool
			if err := g.meshCall(context.Background(), http.MethodPost, p.URL, "/emx/v1/mesh/sync", &out); err != nil {
				g.Log.Printf("mesh: telling %s the keys changed: %v (it copies them within %s)", p.Name, err, m.Every())
			}
		}()
	}
}

// meshGet makes a signed GET to peer and checks the signed answer.
func (g *Gateway) meshGet(ctx context.Context, peerURL, path string, out any) error {
	return g.meshCall(ctx, http.MethodGet, peerURL, path, out)
}

// meshCall makes a signed call without a body to peer and checks the signed answer.
func (g *Gateway) meshCall(ctx context.Context, method, peerURL, path string, out any) error {
	g.mesh.mu.Lock()
	secret, client := g.mesh.secret, g.mesh.client
	g.mesh.mu.Unlock()
	if secret == nil || client == nil {
		return errors.New("no mesh secret loaded")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, peerURL+path, nil)
	if err != nil {
		return err
	}
	mesh.Sign(secret, req.Header, g.meshCfg().Node, mesh.As{}, method, path, nil, time.Now())
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, upstreamError(body))
	}
	if err := mesh.VerifyReply(secret, resp.Header, req.Header.Get(mesh.HeaderSignature), body); err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func (g *Gateway) pollPeers(ctx context.Context) {
	g.mesh.mu.Lock()
	peers := make([]*peerView, 0, len(g.mesh.peers))
	for _, p := range g.mesh.peers {
		peers = append(peers, &peerView{Name: p.Name, URL: p.URL})
	}
	g.mesh.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var st peerState
			t0 := time.Now()
			err := g.meshGet(ctx, p.URL, "/emx/v1/mesh/state", &st)
			if err == nil && st.Node != p.Name {
				err = fmt.Errorf("the gateway at %s calls itself %q, not %q", p.URL, st.Node, p.Name)
			}
			g.mesh.mu.Lock()
			defer g.mesh.mu.Unlock()
			v := g.mesh.peers[p.Name]
			if v == nil || v.URL != p.URL {
				return // removed or changed meanwhile
			}
			if err != nil {
				if v.Up || v.Error != err.Error() {
					g.Log.Printf("mesh: peer %s (%s): %v", p.Name, p.URL, err)
				}
				v.Up, v.Error = false, err.Error()
				v.fails++
				return
			}
			if !v.Up {
				g.Log.Printf("mesh: peer %s (%s) is up: %d model(s)", p.Name, p.URL, len(st.Models))
			}
			now := time.Now()
			v.Up, v.Error, v.fails, v.LastSeen, v.RTTMS, v.Version = true, "", 0, &now, time.Since(t0).Milliseconds(), st.Version
			v.state = &st
			v.Models = v.Models[:0]
			for _, m := range st.Models {
				v.Models = append(v.Models, m.ID)
			}
		}()
	}
	wg.Wait()
}

// peerDown marks a peer that failed a forward as down until its next good answer.
func (g *Gateway) peerDown(name string, err error) {
	g.mesh.mu.Lock()
	defer g.mesh.mu.Unlock()
	if v := g.mesh.peers[name]; v != nil {
		v.Up, v.Error = false, "forwarding: "+err.Error()
		v.fails++
	}
}

// upPeers returns the peers that answered last time, with their state.
func (g *Gateway) upPeers() []*peerView {
	g.mesh.mu.Lock()
	defer g.mesh.mu.Unlock()
	var out []*peerView
	for _, v := range g.mesh.peers {
		if v.Up && v.state != nil {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// syncShared copies keys, teams, prices and aliases from the machine that holds them.
func (g *Gateway) syncShared(ctx context.Context) {
	m := g.meshCfg()
	p, _ := m.Peer(m.KeysFrom)
	var c sharedCopy
	err := g.meshGet(ctx, strings.TrimSuffix(p.URL, "/"), "/emx/v1/mesh/shared", &c)
	var changed bool
	if err == nil {
		data, _ := json.Marshal(c)
		g.mesh.mu.Lock()
		changed = !bytes.Equal(data, g.mesh.synced)
		g.mesh.mu.Unlock()
		if changed {
			if err = c.Shared.Validate(); err == nil {
				if err = keys.Replace([]byte(c.Keys)); err == nil {
					err = config.SaveShared(c.Shared)
				}
			}
			if err == nil {
				g.mesh.mu.Lock()
				g.mesh.synced = data
				g.mesh.mu.Unlock()
			}
		}
	}
	g.mesh.mu.Lock()
	prev := g.mesh.syncErr
	if err != nil {
		g.mesh.syncErr = err.Error()
	} else {
		now := time.Now()
		g.mesh.syncAt, g.mesh.syncErr = &now, ""
	}
	g.mesh.mu.Unlock()
	switch {
	case err != nil && prev != err.Error():
		g.Log.Printf("mesh: copying keys and settings from %s: %v (keeping the last copy)", m.KeysFrom, err)
	case err == nil && changed:
		g.Log.Printf("mesh: keys, teams, prices and aliases copied from %s", m.KeysFrom)
		g.Reload()
	}
}

// meshOnly wraps a handler only other machines of the mesh may call; it signs the answer.
func (g *Gateway) meshOnly(h func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m := g.meshCfg()
		secret := g.meshSecret()
		call, err := g.mesh.verifier.Verify(secret, r.Header, r.Method, r.URL.RequestURI(), func(n string) bool { _, ok := m.Peer(n); return ok }, time.Now())
		if err != nil {
			apiError(w, http.StatusUnauthorized, "authentication_error", err.Error())
			g.logDenied(r, r.Header.Get(mesh.HeaderFrom), http.StatusUnauthorized, err.Error())
			return
		}
		out, err := h(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		data, err := json.Marshal(out)
		if err != nil {
			writeErr(w, err)
			return
		}
		mesh.SignReply(secret, w.Header(), call.Signature, data)
		w.Header().Set("Content-Type", "application/json")
		w.Write(data)
	}
}

// meshStateOf answers what this machine serves.
func (g *Gateway) meshStateOf(r *http.Request) (any, error) {
	byName := g.modelEntries(r.Context(), func(string) bool { return true }, false)
	st := peerState{Node: g.meshCfg().Node, Version: g.Version, Serving: map[string]servingState{}, Usage: g.enteredUsage()}
	for name, e := range byName {
		st.Models = append(st.Models, e)
		s := servingState{APIs: apisOf(e)}
		insts, _ := e.EMX["instances"].([]map[string]any)
		for _, in := range insts {
			s.Ready = s.Ready || in["status"] == runtime.StatusReady
		}
		if c, ok := e.EMX["concurrency"].(map[string]any); ok {
			s.Active, _ = c["active"].(int)
			s.Queued, _ = c["queued"].(int)
			s.Serves, _ = c["serves"].(int)
		}
		s.Serves = max(s.Serves, 1)
		s.Speed, _ = e.EMX["decode_tok_per_sec"].(float64)
		st.Serving[name] = s
	}
	sort.Slice(st.Models, func(i, j int) bool { return st.Models[i].ID < st.Models[j].ID })
	return st, nil
}

func apisOf(e *modelEntry) []string {
	a, _ := e.EMX["apis"].([]string)
	return a
}

// meshShared answers a follower's copy request: only the machine that holds the keys does.
func (g *Gateway) meshShared(*http.Request) (any, error) {
	m := g.meshCfg()
	if m.Follower() {
		return nil, config.ErrManagedElsewhere{Node: m.KeysFrom}
	}
	data, err := keys.Raw()
	if err != nil {
		return nil, err
	}
	return sharedCopy{Keys: string(data), Shared: config.SharedOf(g.App.Config())}, nil
}

// enteredUsage is what clients sent to this machine today and this month (cached for a second).
func (g *Gateway) enteredUsage() meshUsage {
	g.mesh.mu.Lock()
	defer g.mesh.mu.Unlock()
	day, dayStart := today()
	month, monthStart := thisMonth()
	if g.mesh.usage.Day == day && g.mesh.usage.Month == month && time.Since(g.mesh.usageAt) < time.Second {
		return g.mesh.usage
	}
	u := meshUsage{Day: day, Month: month}
	if db, err := g.App.DB(); err == nil && g.App.Config().Logging.Enabled {
		u.KeysDay, u.TeamsDay, _ = db.Entered(dayStart)
		_, u.TeamsMonth, _ = db.Entered(monthStart)
	}
	g.mesh.usage, g.mesh.usageAt = u, time.Now()
	return u
}

// peerUsage sums what clients used on the other machines today and this month (current periods only).
func (g *Gateway) peerUsage() (keysDay map[string]int64, teamsDay, teamsMonth map[string]store.TeamUse) {
	keysDay, teamsDay, teamsMonth = map[string]int64{}, map[string]store.TeamUse{}, map[string]store.TeamUse{}
	day, _ := today()
	month, _ := thisMonth()
	for _, p := range g.upPeers() {
		u := p.state.Usage
		if u.Day == day {
			for k, n := range u.KeysDay {
				keysDay[k] += n
			}
			for t, x := range u.TeamsDay {
				teamsDay[t] = store.TeamUse{Tokens: teamsDay[t].Tokens + x.Tokens, Cost: teamsDay[t].Cost + x.Cost}
			}
		}
		if u.Month == month {
			for t, x := range u.TeamsMonth {
				teamsMonth[t] = store.TeamUse{Tokens: teamsMonth[t].Tokens + x.Tokens, Cost: teamsMonth[t].Cost + x.Cost}
			}
		}
	}
	return keysDay, teamsDay, teamsMonth
}

// meshStatus is GET /emx/v1/mesh: this machine's view of the mesh.
func (g *Gateway) meshStatus(w http.ResponseWriter, r *http.Request) {
	m := g.meshCfg()
	out := map[string]any{"enabled": m.Enabled()}
	if !m.Enabled() {
		writeJSON(w, http.StatusOK, out)
		return
	}
	g.mesh.mu.Lock()
	peers := make([]peerView, 0, len(g.mesh.peers))
	for _, p := range g.mesh.peers {
		peers = append(peers, *p)
	}
	keysFrom := m.KeysFrom
	if keysFrom == "" {
		keysFrom = m.Node
	}
	out["node"], out["keys_from"], out["follower"], out["version"] = m.Node, keysFrom, m.Follower(), g.Version
	if m.Follower() {
		out["keys_copied_at"], out["keys_copy_error"] = g.mesh.syncAt, g.mesh.syncErr
	}
	g.mesh.mu.Unlock()
	sort.Slice(peers, func(i, j int) bool { return peers[i].Name < peers[j].Name })
	out["peers"] = peers
	local := g.modelEntries(r.Context(), func(string) bool { return true }, false)
	names := make([]string, 0, len(local))
	for n := range local {
		names = append(names, n)
	}
	sort.Strings(names)
	out["models"] = names
	writeJSON(w, http.StatusOK, out)
}

// addPeerModels adds the peers' models to a listing: a model on several machines is one entry, its
// emx.nodes naming them.
func (g *Gateway) addPeerModels(byName map[string]*modelEntry, allows func(string) bool) {
	for _, p := range g.upPeers() {
		for _, pe := range p.state.Models {
			if pe == nil || !allows(pe.ID) {
				continue
			}
			if e, ok := byName[pe.ID]; ok {
				nodes, _ := e.EMX["nodes"].([]string)
				e.EMX["nodes"] = append(nodes, p.Name)
				insts, _ := e.EMX["instances"].([]map[string]any)
				e.EMX["instances"] = append(insts, peerInstances(p.Name, pe)...)
				if e.MaxModelLen == 0 {
					e.setContext(pe.MaxModelLen)
				}
				continue
			}
			c := *pe
			c.EMX = make(map[string]any, len(pe.EMX)+1)
			for k, v := range pe.EMX {
				c.EMX[k] = v
			}
			c.EMX["nodes"] = []string{p.Name}
			delete(c.EMX, "registered") // not in this machine's registry: it cannot be removed or stopped from here
			c.EMX["instances"] = append([]map[string]any{}, peerInstances(p.Name, pe)...)
			c.Limits = nil
			byName[pe.ID] = &c
		}
	}
}

// meshTargets decides whether a request for model goes to other machines: nil keeps it here. It prefers,
// in order: this machine when it has the model ready and no peer that has it ready would serve it sooner; a
// peer that has it ready (soonest first); this machine when it can start it; a peer that can start it.
//
// "Sooner" compares the rounds of requests ahead (running and waiting, per slot) divided by the model's
// measured decode speed on each machine, so a fast GPU is preferred over a CPU that is just as idle. When a
// machine has no measured speed yet, only the requests ahead per slot are compared.
func (g *Gateway) meshTargets(ctx context.Context, model, api string) []*runtime.Instance {
	if !g.meshCfg().Enabled() {
		return nil
	}
	var ready, startable []routeCand
	for _, p := range g.upPeers() {
		s, ok := p.state.Serving[model]
		if !ok || (len(s.APIs) > 0 && api != "" && !slices.Contains(s.APIs, api)) {
			continue
		}
		if !s.Ready {
			startable = append(startable, routeCand{name: p.Name})
			continue
		}
		g.activeMu.Lock()
		sent := g.active[meshID(p.Name, model)] // forwarded since its last answer, or still in flight
		g.activeMu.Unlock()
		ready = append(ready, routeCand{name: p.Name, busy: max(s.Active+s.Queued, sent), serves: max(s.Serves, 1), speed: s.Speed})
	}
	out := func(cs ...[]routeCand) []*runtime.Instance {
		var ins []*runtime.Instance
		for _, list := range cs {
			for _, c := range list {
				ins = append(ins, g.peerInstance(c.name, model))
			}
		}
		return ins
	}
	if local, _ := g.ready(ctx, model, api); len(local) > 0 {
		busy, slots := g.localBusy(ctx, model, local)
		here := routeCand{busy: busy, serves: slots, speed: g.localSpeed(model)}
		sortCands(ready, here)
		if len(ready) > 0 && ready[0].cost(useSpeed(ready, here)) < here.cost(useSpeed(ready, here)) {
			return out(ready)
		}
		return nil
	}
	sortCands(ready)
	if len(ready) > 0 {
		return out(ready, startable)
	}
	if m, ok := g.App.RegisteredModel(model); ok && g.autostart() && g.App.Offer(m).Unavailable == "" {
		return nil
	}
	return out(startable)
}

// routeCand is one machine that could serve a request.
type routeCand struct {
	name         string
	busy, serves int     // requests running and waiting; requests it works on at once
	speed        float64 // measured decode tok/s (0: not measured)
}

// cost is how long the request would take there, relatively: rounds of requests ahead (this one included)
// over the speed; or, without speeds, the requests ahead per slot.
func (c routeCand) cost(bySpeed bool) float64 {
	if bySpeed {
		return float64(c.busy/c.serves+1) / c.speed
	}
	return float64(c.busy) / float64(c.serves)
}

// useSpeed reports whether every candidate has a measured speed.
func useSpeed(cs []routeCand, more ...routeCand) bool {
	for _, c := range append(append([]routeCand{}, cs...), more...) {
		if c.speed <= 0 {
			return false
		}
	}
	return true
}

func sortCands(cs []routeCand, more ...routeCand) {
	bySpeed := useSpeed(cs, more...)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].cost(bySpeed) < cs[j].cost(bySpeed) })
}

func meshID(peer, model string) string { return "mesh:" + peer + "/" + model }

// peerInstance is the stand-in instance a forward to peer goes through.
func (g *Gateway) peerInstance(peer, model string) *runtime.Instance {
	g.mesh.mu.Lock()
	defer g.mesh.mu.Unlock()
	u := ""
	if v := g.mesh.peers[peer]; v != nil {
		u = v.URL
	}
	return &runtime.Instance{ID: meshID(peer, model), Name: model, Engine: meshEngine, BaseURL: u, Owner: peer}
}

// localBusy is the requests running and waiting for model on this machine, and the slots serving it.
func (g *Gateway) localBusy(ctx context.Context, model string, ins []*runtime.Instance) (busy, slots int) {
	for _, in := range ins {
		g.activeMu.Lock()
		busy += g.active[in.ID]
		g.activeMu.Unlock()
		slots += max(g.instanceReport(ctx, in).slots, 1)
	}
	if lim := g.maxConcurrency(model); lim > 0 && lim < slots {
		slots = lim
	}
	return busy + g.lim.queued()[model], max(slots, 1)
}

// localSpeed is model's measured decode speed here over the last 30 days (0 when not measured), refreshed
// once a minute.
func (g *Gateway) localSpeed(model string) float64 {
	g.mesh.mu.Lock()
	defer g.mesh.mu.Unlock()
	if time.Since(g.mesh.speedAt) > time.Minute {
		g.mesh.speeds, g.mesh.speedAt = map[string]float64{}, time.Now()
		if db, err := g.App.DB(); err == nil && g.App.Config().Logging.Enabled {
			if rows, err := db.Usage("model", store.Filter{Since: time.Now().Add(-historyWindow)}); err == nil {
				for _, r := range rows {
					if r.DecodeTokPerSec != nil {
						g.mesh.speeds[r.Key] = *r.DecodeTokPerSec
					}
				}
			}
		}
	}
	return g.mesh.speeds[model]
}

// peerInstances are a peer's instances of a model, named node/id and marked with their node.
func peerInstances(node string, pe *modelEntry) []map[string]any {
	var out []map[string]any
	list, _ := pe.EMX["instances"].([]any)
	for _, x := range list {
		if in, ok := x.(map[string]any); ok {
			ci := map[string]any{"node": node}
			for k, v := range in {
				ci[k] = v
			}
			ci["id"] = fmt.Sprintf("%s/%v", node, in["id"])
			out = append(out, ci)
		}
	}
	return out
}

// maxRelayBody bounds a management call relayed between machines.
const maxRelayBody = 1 << 20

// relayTimeout bounds a relayed management call (model starts excepted).
const relayTimeout = time.Minute

// relay sends a management call to peer node as a signed call acting for the caller (with its role), and
// passes the answer back. The peer checks the role again and records the change as made via this machine.
func (g *Gateway) relay(w http.ResponseWriter, r *http.Request, node string) {
	m := g.meshCfg()
	peer, ok := m.Peer(node)
	if !m.Enabled() || !ok {
		apiError(w, http.StatusNotFound, "not_found_error", fmt.Sprintf("no mesh peer called %q (see GET /emx/v1/mesh)", node))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRelayBody+1))
	if err != nil || len(body) > maxRelayBody {
		apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "a call relayed to another machine is at most 1 MiB")
		return
	}
	g.mesh.mu.Lock()
	secret, client := g.mesh.secret, g.mesh.client
	g.mesh.mu.Unlock()
	if secret == nil || client == nil {
		apiError(w, http.StatusServiceUnavailable, "api_error", "mesh: no secret loaded")
		return
	}
	// A peer that accepts the connection and never answers must not hold the caller: a minute, except for
	// starting a model, which may take as long as the load does.
	ctx := r.Context()
	if !strings.HasSuffix(r.URL.Path, "/ensure") {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, relayTimeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, strings.TrimSuffix(peer.URL, "/")+r.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		writeErr(w, err)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	p := principalOf(r)
	team := ""
	if p.key != nil {
		team = p.key.Team
	}
	mesh.Sign(secret, req.Header, m.Node, mesh.As{Client: p.name, Team: team, Role: p.role}, r.Method, r.URL.RequestURI(), body, time.Now())
	resp, err := client.Do(req)
	if err != nil {
		g.peerDown(node, err)
		apiError(w, http.StatusBadGateway, "api_error", fmt.Sprintf("mesh peer %s: %v", node, err))
		return
	}
	defer resp.Body.Close()
	for _, k := range []string{"Content-Type", "Retry-After", "Cache-Control"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set("X-Emx-Node", node)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

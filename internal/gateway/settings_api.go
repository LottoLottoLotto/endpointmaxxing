package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/keys"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/model"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/settings"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/spec"
)

// maxSettingsBody bounds a settings request body.
const maxSettingsBody = 64 << 10

// settingsRoutes registers the settings API: reads for admins and auditors, changes for admin keys only.
func (g *Gateway) settingsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /emx/v1/models", g.authed(roleRead, g.listRegistry))
	mux.HandleFunc("POST /emx/v1/models", g.settingsWrite(g.addModel))
	mux.HandleFunc("DELETE /emx/v1/models/{name}", g.settingsWrite(g.removeModel))

	mux.HandleFunc("POST /emx/v1/keys", g.settingsWrite(g.addKey))
	mux.HandleFunc("PATCH /emx/v1/keys/{name}", g.settingsWrite(g.patchKey))
	mux.HandleFunc("POST /emx/v1/keys/{name}/rotate", g.settingsWrite(g.rotateKey))
	mux.HandleFunc("POST /emx/v1/keys/{name}/disable", g.settingsWrite(g.toggleKey(true)))
	mux.HandleFunc("POST /emx/v1/keys/{name}/enable", g.settingsWrite(g.toggleKey(false)))
	mux.HandleFunc("DELETE /emx/v1/keys/{name}", g.settingsWrite(g.removeKey))

	mux.HandleFunc("PUT /emx/v1/teams/{name}", g.settingsWrite(g.putTeam))
	mux.HandleFunc("DELETE /emx/v1/teams/{name}", g.settingsWrite(g.removeTeam))

	mux.HandleFunc("GET /emx/v1/prices", g.authed(roleRead, g.listPrices))
	mux.HandleFunc("PUT /emx/v1/prices/{pattern}", g.settingsWrite(g.putPrice))
	mux.HandleFunc("DELETE /emx/v1/prices/{pattern}", g.settingsWrite(g.removePrice))

	mux.HandleFunc("GET /emx/v1/aliases", g.authed(roleRead, g.listAliases))
	mux.HandleFunc("PUT /emx/v1/aliases/{alias}", g.settingsWrite(g.putAlias))
	mux.HandleFunc("DELETE /emx/v1/aliases/{alias}", g.settingsWrite(g.removeAlias))

	mux.HandleFunc("DELETE /emx/v1/clients/{name}/data", g.settingsWrite(g.eraseClient))
}

// settingsWrite guards a settings change. On top of an admin key it needs the settings API enabled
// (gateway.disable_settings_api), an HTTPS or loopback connection (requests and answers carry key secrets),
// and a body of at most maxSettingsBody bytes (authed has checked that it is declared as JSON); answers
// are never cached.
func (g *Gateway) settingsWrite(h http.HandlerFunc) http.HandlerFunc {
	return g.authed(roleAdmin, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		deny := func(status int, typ, msg string) {
			apiError(w, status, typ, msg)
			g.logDenied(r, principalOf(r).name, status, msg)
		}
		if g.gw().DisableSettingsAPI {
			deny(http.StatusForbidden, "permission_error", "the settings API is disabled (gateway.disable_settings_api); change settings with the CLI on the gateway host")
			return
		}
		if r.TLS == nil && !loopback(clientIP(r)) {
			deny(http.StatusForbidden, "permission_error", "settings changes need HTTPS (gateway.tls) or a loopback connection: they carry key secrets")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxSettingsBody)
		// A mesh follower takes changes to keys, teams, prices and aliases to the machine that holds them.
		if m := g.meshCfg(); m.Follower() && principalOf(r).call == nil && sharedSetting(r.URL.Path) {
			g.relay(w, r, m.KeysFrom)
			go g.syncShared(context.WithoutCancel(r.Context())) // copy the change back now, not at the next interval
			return
		}
		h(w, r)
		if sharedSetting(r.URL.Path) {
			g.tellFollowers() // they copy it now rather than at their next interval
		}
	})
}

func loopback(ip string) bool {
	p := net.ParseIP(ip)
	return p != nil && p.IsLoopback()
}

// decodeBody reads a strict JSON object into v (unknown fields are errors, so a typo is not silently ignored);
// an empty body leaves v as it is.
func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return errs.New(errs.Usage, "request body: %v", err)
	}
	if dec.More() {
		return errs.New(errs.Usage, "request body: one JSON object expected")
	}
	return nil
}

// changed records a settings change made through the API (with where it came from) and answers it.
func (g *Gateway) changed(w http.ResponseWriter, r *http.Request, status int, res settings.Result, body map[string]any) {
	g.admin(r, fmt.Sprintf("%s [api from %s]", res.Detail, clientIP(r)))
	if len(res.Warnings) > 0 {
		body["warnings"] = res.Warnings
	}
	writeJSON(w, status, body)
}

// reloadNow applies a config change at once rather than at the watcher's next look (an invalid file keeps
// the running config and is logged).
func (g *Gateway) reloadNow() {
	if err := g.Reload(); err != nil {
		g.Log.Printf("settings: reload after change: %v", err)
	}
}

// ownKey refuses changes that would lock the caller out with its own key: disabling, deleting or demoting it.
func ownKey(r *http.Request, name string) error {
	if p := principalOf(r); p.key != nil && p.key.Name == name {
		return errs.New(errs.Conflict, "key %s is the key making this request; change it with another admin key or the CLI", name)
	}
	return nil
}

// ---- keys ----

func (g *Gateway) addKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string   `json:"name"`
		Role            string   `json:"role"`
		Team            string   `json:"team"`
		Models          []string `json:"models"`
		RPM             int      `json:"rpm"`
		TokensPerDay    int64    `json:"tokens_per_day"`
		MaxOutputTokens int      `json:"max_output_tokens"`
		Delegate        bool     `json:"delegate"`
		Expires         string   `json:"expires"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Role == "" {
		req.Role = keys.RoleUser
	}
	exp, err := settings.ParseExpiry(req.Expires)
	if err != nil {
		writeErr(w, err)
		return
	}
	k := keys.Key{Name: req.Name, Role: req.Role, Team: req.Team, Models: req.Models, RPM: req.RPM, TokensPerDay: req.TokensPerDay,
		MaxOutputTokens: req.MaxOutputTokens, Delegate: req.Delegate, Expires: exp}
	secret, res, err := settings.AddKey(g.App.Config(), k)
	if err != nil {
		writeErr(w, err)
		return
	}
	g.changed(w, r, http.StatusCreated, res, map[string]any{"name": k.Name, "role": k.Role, "team": k.Team, "models": k.Models,
		"rpm": k.RPM, "tokens_per_day": k.TokensPerDay, "max_output_tokens": k.MaxOutputTokens, "delegate": k.Delegate, "expires": k.Expires, "secret": secret})
}

func (g *Gateway) patchKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Role            *string   `json:"role"`
		Team            *string   `json:"team"`
		Models          *[]string `json:"models"`
		RPM             *int      `json:"rpm"`
		TokensPerDay    *int64    `json:"tokens_per_day"`
		MaxOutputTokens *int      `json:"max_output_tokens"`
		Delegate        *bool     `json:"delegate"`
		Expires         *string   `json:"expires"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Role != nil && *req.Role != keys.RoleAdmin {
		if err := ownKey(r, name); err != nil {
			writeErr(w, err)
			return
		}
	}
	p := settings.KeyPatch{Role: req.Role, Team: req.Team, Models: req.Models, RPM: req.RPM, TokensPerDay: req.TokensPerDay,
		MaxOutputTokens: req.MaxOutputTokens, Delegate: req.Delegate}
	if req.Expires != nil {
		exp, err := settings.ParseExpiry(*req.Expires)
		if err != nil {
			writeErr(w, err)
			return
		}
		p.ExpiresSet, p.Expires = true, exp
	}
	k, res, err := settings.SetKey(g.App.Config(), name, p)
	if err != nil {
		writeErr(w, err)
		return
	}
	g.changed(w, r, http.StatusOK, res, map[string]any{"key": KeyInfo{Key: k, Status: k.Status(time.Now())}})
}

func (g *Gateway) rotateKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Grace *string `json:"grace"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	grace := 24 * time.Hour
	if req.Grace != nil {
		var err error
		if grace, err = time.ParseDuration(*req.Grace); err != nil {
			writeErr(w, errs.New(errs.Usage, "grace: %v", err))
			return
		}
	}
	secret, k, res, err := settings.RotateKey(r.PathValue("name"), grace)
	if err != nil {
		writeErr(w, err)
		return
	}
	g.changed(w, r, http.StatusOK, res, map[string]any{"name": k.Name, "secret": secret, "old_secret_valid_until": k.PrevExpires})
}

func (g *Gateway) toggleKey(disable bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if disable {
			if err := ownKey(r, name); err != nil {
				writeErr(w, err)
				return
			}
		}
		res, err := settings.SetKeysDisabled([]string{name}, disable)
		if err != nil {
			writeErr(w, err)
			return
		}
		g.changed(w, r, http.StatusOK, res, map[string]any{"name": name, "disabled": disable})
	}
}

func (g *Gateway) removeKey(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := ownKey(r, name); err != nil {
		writeErr(w, err)
		return
	}
	res, err := settings.RemoveKeys([]string{name})
	if err != nil {
		writeErr(w, err)
		return
	}
	g.changed(w, r, http.StatusOK, res, map[string]any{"removed": name})
}

// ---- teams ----

func (g *Gateway) putTeam(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TokensPerDay   *int64    `json:"tokens_per_day"`
		TokensPerMonth *int64    `json:"tokens_per_month"`
		BudgetPerMonth *float64  `json:"budget_per_month"`
		AlertAt        *float64  `json:"alert_at"`
		Models         *[]string `json:"models"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	t, res, err := settings.SetTeam(r.PathValue("name"), settings.TeamPatch{TokensPerDay: req.TokensPerDay,
		TokensPerMonth: req.TokensPerMonth, BudgetPerMonth: req.BudgetPerMonth, AlertAt: req.AlertAt, Models: req.Models})
	if err != nil {
		writeErr(w, err)
		return
	}
	g.reloadNow()
	g.changed(w, r, http.StatusOK, res, map[string]any{"team": t})
}

func (g *Gateway) removeTeam(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	res, err := settings.RemoveTeam(name)
	if err != nil {
		writeErr(w, err)
		return
	}
	g.reloadNow()
	g.changed(w, r, http.StatusOK, res, map[string]any{"removed": name})
}

// ---- prices ----

func (g *Gateway) listPrices(w http.ResponseWriter, r *http.Request) {
	p := g.App.Config().Pricing
	writeJSON(w, http.StatusOK, map[string]any{"currency": p.CurrencyOrDefault(), "default": p.Default, "models": p.Models})
}

func (g *Gateway) putPrice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Prompt     *float64 `json:"prompt"`
		Completion *float64 `json:"completion"`
		Cached     *float64 `json:"cached"`
		Currency   string   `json:"currency"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Prompt == nil || req.Completion == nil {
		writeErr(w, errs.New(errs.Usage, `body must give "prompt" and "completion" (per million tokens), optionally "cached" and "currency"`))
		return
	}
	pattern := r.PathValue("pattern")
	p := config.Price{Prompt: *req.Prompt, Completion: *req.Completion, Cached: req.Cached}
	res, err := settings.SetPrice(pattern, p, req.Currency)
	if err != nil {
		writeErr(w, err)
		return
	}
	g.reloadNow()
	g.changed(w, r, http.StatusOK, res, map[string]any{"model": pattern, "price": p})
}

func (g *Gateway) removePrice(w http.ResponseWriter, r *http.Request) {
	pattern := r.PathValue("pattern")
	res, err := settings.RemovePrice(pattern)
	if err != nil {
		writeErr(w, err)
		return
	}
	g.reloadNow()
	g.changed(w, r, http.StatusOK, res, map[string]any{"removed": pattern})
}

// ---- aliases ----

func (g *Gateway) listAliases(w http.ResponseWriter, r *http.Request) {
	al := g.App.Config().Aliases
	if al == nil {
		al = map[string]string{}
	}
	writeJSON(w, http.StatusOK, al)
}

func (g *Gateway) putAlias(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string `json:"model"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	alias := r.PathValue("alias")
	res, err := settings.SetAlias(g.App, alias, req.Model)
	if err != nil {
		writeErr(w, err)
		return
	}
	g.reloadNow()
	g.changed(w, r, http.StatusOK, res, map[string]any{"alias": alias, "model": req.Model})
}

func (g *Gateway) removeAlias(w http.ResponseWriter, r *http.Request) {
	alias := r.PathValue("alias")
	res, err := settings.RemoveAlias(alias)
	if err != nil {
		writeErr(w, err)
		return
	}
	g.reloadNow()
	g.changed(w, r, http.StatusOK, res, map[string]any{"removed": alias})
}

// ---- model registry ----

// registryEntry is a registered model as the settings API reports it.
type registryEntry struct {
	config.Model
	SourceMissing bool `json:"source_missing,omitempty"`
}

func (g *Gateway) listRegistry(w http.ResponseWriter, r *http.Request) {
	g.reloadNow() // see models added since
	ms := g.App.RegisteredModels()
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	out := make([]registryEntry, len(ms))
	for i, m := range ms {
		out[i] = registryEntry{Model: m.Redacted(), SourceMissing: model.MissingLocal(m.Source)} // no engine key: auditors read this
	}
	writeJSON(w, http.StatusOK, out)
}

// addModel registers a model. Engine arguments, placeholders (vars), container pre-args and host-only
// flags (trust_remote_code) are not accepted here: they reach engine command lines and container options or
// let a model run code on the host, so only the CLI sets them. Nor is "$" in a source: environment
// variables are expanded for the CLI only, never for a caller who would read the host's environment with them.
func (g *Gateway) addModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name           string            `json:"name"`
		Source         string            `json:"source"`
		Engine         string            `json:"engine"`
		Force          bool              `json:"force"`
		Flags          map[string]string `json:"flags"`
		VRAMMiB        int64             `json:"vram_mib"`
		IdleUnload     string            `json:"idle_unload"`
		MaxConcurrency int               `json:"max_concurrency"`
		Devices        int               `json:"devices"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if req.Name == "" || req.Source == "" {
		writeErr(w, errs.New(errs.Usage, `body must give "name" and "source" (a local path or an org/repo id)`))
		return
	}
	if strings.Contains(req.Source, "$") {
		writeErr(w, errs.New(errs.Usage, `source %q: "$" is not accepted over the API (environment variables are expanded only by `+
			"`emx model add` on the host)", req.Source))
		return
	}
	for _, f := range spec.Unified {
		if _, set := req.Flags[f.Key]; set && f.HostOnly {
			writeErr(w, errs.New(errs.Usage, "flags.%s is not accepted over the API: it lets a model run code or read files on the host; "+
				"set it there with `emx model add ... --%s`", f.Key, f.CLI))
			return
		}
	}
	var idle time.Duration
	if req.IdleUnload != "" {
		var err error
		if idle, err = time.ParseDuration(req.IdleUnload); err != nil {
			writeErr(w, errs.New(errs.Usage, "idle_unload: %v", err))
			return
		}
	}
	added, res, err := settings.AddModel(g.App, settings.ModelSpec{Name: req.Name, Source: req.Source, Engine: req.Engine,
		Force: req.Force, Flags: req.Flags, VRAMMiB: req.VRAMMiB, IdleUnload: idle, MaxConcurrency: req.MaxConcurrency, Devices: req.Devices})
	if err != nil {
		writeErr(w, err)
		return
	}
	g.reloadNow()
	g.changed(w, r, http.StatusCreated, res, map[string]any{"model": added.Model.Redacted(), "engine": added.Engine, "route": added.Route})
}

func (g *Gateway) removeModel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	res, err := settings.RemoveModels([]string{name})
	if err != nil {
		writeErr(w, err)
		return
	}
	g.reloadNow()
	g.changed(w, r, http.StatusOK, res, map[string]any{"removed": name})
}

// ---- data ----

// eraseClient deletes every request record and body of one client (an erasure request); the admin trail
// records that it happened.
func (g *Gateway) eraseClient(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	db, err := g.App.DB()
	if err != nil {
		writeErr(w, err)
		return
	}
	res, n, err := settings.EraseClient(db, name)
	if err != nil {
		writeErr(w, err)
		return
	}
	g.changed(w, r, http.StatusOK, res, map[string]any{"client": name, "deleted": n})
}

// sharedSetting reports whether path changes keys, teams, prices or aliases (what a mesh shares).
func sharedSetting(path string) bool {
	for _, p := range []string{"/emx/v1/keys", "/emx/v1/teams/", "/emx/v1/prices/", "/emx/v1/aliases/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

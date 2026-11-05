package gateway

import (
	"embed"
	"io/fs"
	"net/http"
)

// The admin UI is a static client of the management API: it holds no data and has no routes of its own,
// so everything a person can do there an agent can do over /substrate/v1 with the same key and role.
//
//go:embed ui
var uiFiles embed.FS

// uiHandler serves the admin UI under /admin/. The page asks for an API key and sends it as a bearer
// token on every API call, so there is no session cookie to forge across sites.
func uiHandler() http.Handler {
	sub, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	files := http.StripPrefix("/admin/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; "+
			"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

// whoami reports the caller's key name, role and team, so a client (the admin UI, or an agent) can tell
// what it may do before trying.
func (g *Gateway) whoami(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	out := map[string]any{"name": p.name, "role": p.role, "open": p.name == "" && p.key == nil}
	if p.key != nil && p.key.Team != "" {
		out["team"] = p.key.Team
	}
	writeJSON(w, http.StatusOK, out)
}

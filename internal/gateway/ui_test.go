package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/LottoLottoLotto/substrate/internal/keys"
)

func TestWhoamiReportsRoleAndTeam(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	user, _ := keys.Add(keys.Key{Name: "alice", Role: keys.RoleUser, Team: "research"})
	auditor, _ := keys.Add(keys.Key{Name: "audit", Role: keys.RoleAuditor})
	g, srv := newGateway(t)
	g.APIKey = "master"

	for _, tc := range []struct{ key, name, role, team string }{
		{user, "alice", keys.RoleUser, "research"},
		{auditor, "audit", keys.RoleAuditor, ""},
		{"master", "admin", keys.RoleAdmin, ""},
	} {
		req, _ := http.NewRequest("GET", srv.URL+"/substrate/v1/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+tc.key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Name, Role, Team string
			Open             bool
		}
		json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || got.Name != tc.name || got.Role != tc.role || got.Team != tc.team || got.Open {
			t.Errorf("%s: %d %+v", tc.name, resp.StatusCode, got)
		}
	}
	if s := call(t, "GET", srv.URL+"/substrate/v1/whoami", "wrong", ""); s != http.StatusUnauthorized {
		t.Fatalf("unknown key: %d", s)
	}
}

// The UI's files are public (they hold no data); everything it shows still needs a key.
func TestAdminUIServedWithoutDataOrFraming(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	keys.Add(keys.Key{Name: "ops", Role: keys.RoleAdmin})
	_, srv := newGateway(t)

	resp, err := http.Get(srv.URL + "/admin") // redirected to /admin/
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/admin/" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("/admin: %d %s %s", resp.StatusCode, resp.Request.URL.Path, resp.Header.Get("Content-Type"))
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP: %q", csp)
	}
	for _, f := range []string{"app.js", "app.css"} {
		if s := call(t, "GET", srv.URL+"/admin/"+f, "", ""); s != http.StatusOK {
			t.Errorf("%s: %d", f, s)
		}
	}
	if s := call(t, "GET", srv.URL+"/substrate/v1/report", "", ""); s != http.StatusUnauthorized {
		t.Fatalf("report without a key: %d", s)
	}
}

// Clients (the admin UI, agents) iterate these answers: no match must be [], not null.
func TestEmptyListsAreArrays(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	admin, _ := keys.Add(keys.Key{Name: "ops", Role: keys.RoleAdmin})
	_, srv := newGateway(t)
	for _, p := range []string{"/substrate/v1/requests?client=nobody", "/substrate/v1/events?kind=none", "/substrate/v1/usage?by=model"} {
		req, _ := http.NewRequest("GET", srv.URL+p, nil)
		req.Header.Set("Authorization", "Bearer "+admin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.TrimSpace(string(body)) != "[]" {
			t.Errorf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
}

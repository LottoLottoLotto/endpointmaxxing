package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lotto/substrate/internal/paths"
	"github.com/lotto/substrate/internal/runtime"
)

// A multi-model host serves exactly the models it holds now: requests for them go to it
// under their own names, /v1/models lists them (not the host), and others are not routed there.
func TestHostServesTheModelsItHolds(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	held := []string{"a"}
	var served []string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
		case "/admin/models":
			list := []map[string]string{}
			for _, n := range held {
				list = append(list, map[string]string{"name": n})
			}
			json.NewEncoder(w).Encode(list)
		default:
			var body struct{ Model string }
			json.NewDecoder(r.Body).Decode(&body)
			served = append(served, body.Model)
			w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		}
	}))
	defer backend.Close()
	now := time.Now()
	in := runtime.Instance{ID: "tt-multihost", Name: "tt-multihost", Engine: "tt-multihost", Hosting: true,
		PID: os.Getpid(), StartTick: selfStartTick(t), BaseURL: backend.URL, HealthURL: backend.URL + "/health",
		APIs: []string{"chat"}, ReadyAt: &now, Owner: "cli"}
	data, _ := json.Marshal(in)
	dir := filepath.Join(paths.State(), "instances")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, in.ID+".json"), data, 0o600)

	_, srv := newGateway(t)
	if code := call(t, "POST", srv.URL+"/v1/chat/completions", "", `{"model":"a","messages":[]}`); code != 200 {
		t.Fatalf("held model: status %d", code)
	}
	if len(served) != 1 || served[0] != "a" {
		t.Fatalf("host got %v, want one request for a", served)
	}
	// Not held (and not registered, so nothing may load it): never sent to the host.
	if code := call(t, "POST", srv.URL+"/v1/chat/completions", "", `{"model":"b","messages":[]}`); code != 404 {
		t.Fatalf("model the host does not hold: status %d, want 404", code)
	}
	if len(served) != 1 {
		t.Fatalf("host got %v", served)
	}

	held = []string{"a", "b"}
	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct{ Data []struct{ ID string } }
	json.NewDecoder(resp.Body).Decode(&list)
	var ids []string
	for _, m := range list.Data {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "a,b" {
		t.Fatalf("/v1/models = %v, want the hosted models a,b", ids)
	}
}

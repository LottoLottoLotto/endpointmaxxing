package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/csv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/app"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/keys"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/paths"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/store"
	_ "modernc.org/sqlite"
)

func openApp(t *testing.T) *app.App {
	t.Helper()
	a, err := app.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

func TestBackupHoldsConfigKeysAndAConsistentDatabase(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	if _, err := config.Update(func(c *config.Config) error { c.Aliases = map[string]string{"x": "y"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Add(keys.Key{Name: "alice", Role: keys.RoleUser}); err != nil {
		t.Fatal(err)
	}
	a := openApp(t)
	db, err := a.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InsertRequest(&store.Request{ID: "req_1", TS: time.Now().UnixMilli(), Model: "m", Status: 200}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	files, err := backup(a, out, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(out); !paths.OwnerOnly(st.Mode()) {
		t.Fatalf("archive mode %v: it holds key hashes and possibly prompts", st.Mode().Perm())
	}
	f, _ := os.Open(out)
	defer f.Close()
	gz, _ := gzip.NewReader(f)
	tr := tar.NewReader(gz)
	got := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name], _ = io.ReadAll(tr)
	}
	for _, name := range []string{"config/config.yaml", "config/keys.yaml", "state/usage.db"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("archive lacks %s (has %v)", name, files)
		}
	}
	snap := filepath.Join(t.TempDir(), "usage.db")
	os.WriteFile(snap, got["state/usage.db"], 0o600)
	sdb, err := sql.Open("sqlite", snap)
	if err != nil {
		t.Fatal(err)
	}
	defer sdb.Close()
	var n int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM requests WHERE id = 'req_1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("snapshot rows: %d %v", n, err)
	}
	if _, err := backup(a, out, true, false); err == nil {
		t.Fatal("backup overwrote an existing archive")
	}
}

func TestSecurityFindings(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	if _, err := config.Update(func(c *config.Config) error {
		c.Gateway.Host = "0.0.0.0"
		c.Logging.Bodies = true
		c.Teams = []config.Team{{Name: "r", BudgetPerMonth: 10}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(3 * 24 * time.Hour)
	keys.Add(keys.Key{Name: "forever", Role: keys.RoleUser})
	keys.Add(keys.Key{Name: "soon", Role: keys.RoleUser, Expires: &soon, Team: "ghost"})
	found := map[string]string{}
	for _, f := range securityFindings(openApp(t)) {
		found[f.Check] += f.Detail + "; "
	}
	for check, want := range map[string]string{
		"plain_http": "0.0.0.0", "bodies_kept_forever": "", "key_no_expiry": "forever", "key_expiring": "soon",
		"key_unknown_team": "ghost", "budget_without_prices": "team r",
	} {
		if d, ok := found[check]; !ok || !strings.Contains(d, want) {
			t.Errorf("finding %s: %q (all: %v)", check, d, found)
		}
	}
	if strings.Contains(found["key_no_expiry"], "soon") {
		t.Errorf("a key with an expiry was reported as never expiring")
	}
}

func TestCSVTables(t *testing.T) {
	var buf bytes.Buffer
	e := &env{stdout: &buf, csv: true}
	e.table("NAME\tCOST", [][]string{{"team, research", "1.50"}, {`say "hi"`, "-"}})
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[1][0] != "team, research" || rows[2][0] != `say "hi"` || rows[0][1] != "COST" {
		t.Fatalf("csv rows %q", rows)
	}
}

func TestAdminChangesAreRecorded(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	t.Setenv("EMX_ACTOR", "carol")
	if _, err := config.Update(func(c *config.Config) error { c.Logging.AuditFile = "audit.jsonl"; return nil }); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	e := &env{stdout: &out, stderr: io.Discard, json: true, owner: "cli"}
	defer e.close()
	for _, args := range [][]string{{"key", "add", "bob", "--team", "r"}, {"key", "rotate", "bob"}, {"alias", "set", "chat", "m"}} {
		if err := execute(e, args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	a, _ := e.app()
	db, _ := a.DB()
	evs, err := db.Events(store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var details []string
	for _, ev := range evs {
		if ev.Kind == "admin" && ev.Owner == "carol" {
			details = append(details, ev.Detail)
		}
	}
	joined := strings.Join(details, "\n")
	for _, want := range []string{"key add bob", "key rotate bob", "alias set chat -> m"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("admin events %q lack %q", joined, want)
		}
	}
	audit, _ := os.ReadFile(filepath.Join(os.Getenv("EMX_HOME"), "state", "audit.jsonl"))
	if strings.Count(string(audit), `"type":"admin"`) != 3 || strings.Contains(string(audit), "sk-emx-") {
		t.Fatalf("audit log: %s", audit)
	}
}

// A backup never touches a file that was already there: the previous archive survives a repeated command.
func TestBackupLeavesAnExistingFileAlone(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	a := openApp(t)
	out := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := os.WriteFile(out, []byte("yesterday's backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backup(a, out, true, false); errs.CodeOf(err) != errs.Conflict {
		t.Fatalf("backup over an existing file: %v", err)
	}
	if data, _ := os.ReadFile(out); string(data) != "yesterday's backup" {
		t.Fatalf("the existing file was changed or removed: %q", data)
	}
}

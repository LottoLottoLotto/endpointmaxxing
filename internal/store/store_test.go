package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/LottoLottoLotto/substrate/internal/paths"
)

func i64(v int64) *int64 { return &v }

func open(t *testing.T) *DB {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestReopenKeepsSchema(t *testing.T) {
	db := open(t)
	db.Close()
	db2, err := Open()
	if err != nil {
		t.Fatalf("reopening a migrated db: %v", err)
	}
	db2.Close()
}

func TestUsageAggregation(t *testing.T) {
	db := open(t)
	now := time.Now()
	rows := []*Request{
		// Streamed: 11 tokens, TTFT 100ms, total 1100ms -> 10 tokens over 1s of decode.
		{ID: "a", TS: now.UnixMilli(), Model: "m1", Engine: "vllm", Status: 200, Stream: true,
			PromptTokens: i64(100), CompletionTokens: i64(11), CachedTokens: i64(50), TTFTMS: i64(100), DurationMS: i64(1100)},
		{ID: "b", TS: now.UnixMilli(), Model: "m1", Engine: "vllm", Status: 200, PromptTokens: i64(10), CompletionTokens: i64(5), DurationMS: i64(500)},
		{ID: "c", TS: now.UnixMilli(), Model: "m1", Engine: "vllm", Status: 502, Error: "boom"},
		{ID: "d", TS: now.UnixMilli(), Model: "m2", Engine: "llama.cpp", Status: 200}, // unmetered
		{ID: "old", TS: now.Add(-48 * time.Hour).UnixMilli(), Model: "m1", Engine: "vllm", Status: 200, CompletionTokens: i64(1000)},
	}
	for _, r := range rows {
		if err := db.InsertRequest(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.Usage("model", Filter{Since: now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "m1" {
		t.Fatalf("groups: %+v", got)
	}
	m1 := got[0]
	if m1.Requests != 3 || m1.Errors != 1 || m1.PromptTokens != 110 || m1.CompletionTokens != 16 || m1.CachedTokens != 50 {
		t.Fatalf("m1: %+v", m1)
	}
	if m1.DecodeTokPerSec == nil || *m1.DecodeTokPerSec != 10 {
		t.Fatalf("decode tok/s: %v", m1.DecodeTokPerSec)
	}
	if got[1].UnmeteredReqs != 1 {
		t.Fatalf("m2 unmetered: %+v", got[1])
	}

	n, err := db.Prune(now.Add(-24*time.Hour), time.Time{})
	if err != nil || n["requests"] != 1 {
		t.Fatalf("prune: %v %v", n, err)
	}
}

func TestBodiesAndFilters(t *testing.T) {
	db := open(t)
	r := &Request{ID: "x", TS: time.Now().UnixMilli(), Model: "m", Client: "agent-1", Status: 200,
		RequestBody: []byte(`{"q":1}`), ResponseBody: []byte(`{"a":2}`)}
	if err := db.InsertRequest(r); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertRequest(&Request{ID: "y", TS: time.Now().UnixMilli(), Model: "m", Client: "agent-2"}); err != nil {
		t.Fatal(err)
	}
	got, err := db.Requests(Filter{Client: "agent-1"})
	if err != nil || len(got) != 1 || got[0].ID != "x" {
		t.Fatalf("client filter: %+v %v", got, err)
	}
	req, resp, _, ok, err := db.Bodies("x")
	if err != nil || !ok || string(req) != `{"q":1}` || string(resp) != `{"a":2}` {
		t.Fatalf("bodies: %q %q %v %v", req, resp, ok, err)
	}
	if _, _, _, ok, _ := db.Bodies("y"); ok {
		t.Fatal("bodies for a request that captured none")
	}
	if err := db.InsertRequest(r); err == nil {
		t.Fatal("duplicate request id accepted")
	}
}

// A client's own request id is stored beside the gateway's: repeating it never drops a row.
func TestClientRequestIDIsNotUnique(t *testing.T) {
	db := open(t)
	for _, id := range []string{"req_1", "req_2"} {
		if err := db.InsertRequest(&Request{ID: id, ClientRequestID: "same", TS: time.Now().UnixMilli(), Model: "m"}); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	if err := db.InsertRequest(&Request{ID: "req_3", TS: time.Now().UnixMilli(), Model: "m"}); err != nil {
		t.Fatal(err)
	}
	got, err := db.Requests(Filter{ID: "same"})
	if err != nil || len(got) != 2 || got[0].ClientRequestID != "same" {
		t.Fatalf("by client id: %+v %v", got, err)
	}
	got, err = db.Requests(Filter{ID: "req_3"})
	if err != nil || len(got) != 1 || got[0].ClientRequestID != "" {
		t.Fatalf("by gateway id: %+v %v", got, err)
	}
	data, _ := json.Marshal(got[0])
	if bytes.Contains(data, []byte("client_request_id")) {
		t.Fatalf("an absent client id must be omitted: %s", data)
	}
}

// The admin trail outlives usage records: each has its own cutoff.
func TestPruneKeepsTheAdminTrailUntilItsOwnCutoff(t *testing.T) {
	db := open(t)
	old := time.Now().Add(-48 * time.Hour).UnixMilli()
	for _, e := range []*Event{{TS: old, Kind: "exited"}, {TS: old, Kind: "admin", Owner: "ops", Detail: "key add x"}} {
		if err := db.InsertEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	cut := time.Now().Add(-24 * time.Hour)
	if _, err := db.Prune(cut, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if ev, _ := db.Events(Filter{}); len(ev) != 1 || ev[0].Kind != "admin" {
		t.Fatalf("usage prune touched the admin trail: %+v", ev)
	}
	if n, err := db.Prune(time.Time{}, cut); err != nil || n["admin_events"] != 1 {
		t.Fatalf("audit prune: %v %v", n, err)
	}
}

// Erasing a client removes its records and bodies from the database file and the WAL, not just from queries,
// and leaves other clients alone.
func TestEraseClientLeavesNoCopyOnDisk(t *testing.T) {
	db := open(t)
	secret := "ERASE-ME-7f3c1b0d9a"
	for i, c := range []string{"alice", "bob"} {
		r := &Request{ID: fmt.Sprintf("r%d", i), TS: time.Now().UnixMilli(), Model: "m", Client: c, Status: 200,
			RequestBody: []byte(`{"prompt":"` + secret + c + `"}`)}
		if err := db.InsertRequest(r); err != nil {
			t.Fatal(err)
		}
	}
	n, err := db.EraseClient("alice")
	if err != nil || n["requests"] != 1 || n["bodies"] != 1 {
		t.Fatalf("erase: %v %v", n, err)
	}
	if rows, _ := db.Requests(Filter{}); len(rows) != 1 || rows[0].Client != "bob" {
		t.Fatalf("remaining: %+v", rows)
	}
	for _, p := range []string{Path(), Path() + "-wal"} {
		data, _ := os.ReadFile(p)
		if bytes.Contains(data, []byte(secret+"alice")) {
			t.Errorf("%s still holds the erased body", p)
		}
	}
}

func TestDatabaseIsPrivate(t *testing.T) {
	t.Setenv("SUBSTRATE_HOME", t.TempDir())
	os.MkdirAll(paths.State(), 0o755)
	os.WriteFile(Path(), nil, 0o644) // an existing database from an older version
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InsertEvent(&Event{TS: 1, Kind: "x"}); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{paths.State(): 0o700, Path(): 0o600, Path() + "-wal": 0o600} {
		st, err := os.Stat(p)
		if err != nil || (!paths.Windows && st.Mode().Perm() != want) {
			t.Errorf("%s: %v %v, want %v", p, st.Mode().Perm(), err, want)
		}
	}
}

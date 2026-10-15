package store

import (
	"testing"
	"time"
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

	n, err := db.Prune(now.Add(-24 * time.Hour))
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

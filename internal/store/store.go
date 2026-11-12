// Package store records engine and token use in state/usage.db (SQLite, WAL):
// every gateway request, lifecycle events, and periodic GPU/process samples.
// The CLI and the gateway write concurrently; SQLite's locking arbitrates.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/LottoLottoLotto/substrate/internal/paths"
	"github.com/LottoLottoLotto/substrate/internal/policy"
	_ "modernc.org/sqlite"
)

// Path is the usage database.
func Path() string { return filepath.Join(paths.State(), "usage.db") }

// migrations[i] moves the schema from user_version i to i+1.
var migrations = []string{
	`CREATE TABLE requests (
		id TEXT PRIMARY KEY,
		ts INTEGER NOT NULL,            -- unix ms, request received
		model TEXT NOT NULL DEFAULT '',
		instance TEXT NOT NULL DEFAULT '',
		engine TEXT NOT NULL DEFAULT '',
		api TEXT NOT NULL DEFAULT '',
		path TEXT NOT NULL DEFAULT '',
		stream INTEGER NOT NULL DEFAULT 0,
		status INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		client TEXT NOT NULL DEFAULT '',
		user_agent TEXT NOT NULL DEFAULT '',
		remote TEXT NOT NULL DEFAULT '',
		prompt_tokens INTEGER,
		completion_tokens INTEGER,
		cached_tokens INTEGER,
		reasoning_tokens INTEGER,
		usage_source TEXT NOT NULL DEFAULT '',
		finish_reason TEXT NOT NULL DEFAULT '',
		autostarted INTEGER NOT NULL DEFAULT 0,
		queue_ms INTEGER,               -- waiting for a ready instance (incl. autostart)
		ttft_ms INTEGER,                -- first generated content (streams)
		duration_ms INTEGER,            -- request received to last byte
		req_bytes INTEGER,
		resp_bytes INTEGER
	);
	CREATE INDEX requests_ts ON requests(ts);
	CREATE INDEX requests_model ON requests(model, ts);
	CREATE INDEX requests_engine ON requests(engine, ts);
	CREATE TABLE bodies (
		request_id TEXT PRIMARY KEY,
		request BLOB,
		response BLOB,
		truncated INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE events (
		id INTEGER PRIMARY KEY,
		ts INTEGER NOT NULL,
		kind TEXT NOT NULL,             -- start, ready, start_failed, stop, exited
		instance TEXT NOT NULL DEFAULT '',
		engine TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		owner TEXT NOT NULL DEFAULT '',
		pid INTEGER,
		port INTEGER,
		duration_ms INTEGER,            -- load time (ready) or uptime (stop, exited)
		detail TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX events_ts ON events(ts);
	CREATE TABLE gpu_samples (
		ts INTEGER NOT NULL,
		gpu INTEGER NOT NULL,
		name TEXT NOT NULL DEFAULT '',
		mem_used_mib INTEGER,
		mem_total_mib INTEGER,
		util_pct INTEGER,
		power_w REAL
	);
	CREATE INDEX gpu_samples_ts ON gpu_samples(ts);
	CREATE TABLE instance_samples (
		ts INTEGER NOT NULL,
		instance TEXT NOT NULL,
		engine TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		gpu_mem_mib INTEGER,
		rss_mib INTEGER,
		processes INTEGER
	);
	CREATE INDEX instance_samples_ts ON instance_samples(ts);`,
	// 2: token quotas sum a client's day.
	`CREATE INDEX requests_client ON requests(client, ts);`,
	// 3: prompt policy, classification and error kinds.
	`ALTER TABLE requests ADD COLUMN category TEXT NOT NULL DEFAULT '';
	ALTER TABLE requests ADD COLUMN flags TEXT NOT NULL DEFAULT '';         -- JSON array of policy.Flag
	ALTER TABLE requests ADD COLUMN flag_severity TEXT NOT NULL DEFAULT ''; -- highest flag severity
	ALTER TABLE requests ADD COLUMN error_kind TEXT NOT NULL DEFAULT '';
	CREATE INDEX requests_category ON requests(category, ts);
	CREATE INDEX requests_flag_severity ON requests(flag_severity, ts) WHERE flag_severity != '';
	CREATE INDEX requests_error_kind ON requests(error_kind, ts) WHERE error_kind != '';`,
	// 4: teams and cost.
	`ALTER TABLE requests ADD COLUMN team TEXT NOT NULL DEFAULT '';
	ALTER TABLE requests ADD COLUMN cost REAL;                              -- in pricing.currency; NULL when unpriced
	CREATE INDEX requests_team ON requests(team, ts) WHERE team != '';`,
	// 5: benchmark runs (substrate bench).
	`CREATE TABLE benchmarks (
		id TEXT PRIMARY KEY,
		ts INTEGER NOT NULL,            -- unix ms, run started
		model TEXT NOT NULL,
		engine TEXT NOT NULL DEFAULT '',
		instance TEXT NOT NULL DEFAULT '',
		via TEXT NOT NULL DEFAULT '',    -- direct or gateway
		concurrency INTEGER NOT NULL DEFAULT 1,
		prompt TEXT NOT NULL DEFAULT '', -- canonical prompt id, or file:<path>
		tok_s_out REAL,
		tok_s_prefill REAL,
		tok_s_total REAL,
		ttft_ms REAL,
		prompt_tokens INTEGER,
		output_tokens INTEGER,
		payload TEXT NOT NULL DEFAULT '', -- path of the LocalMaxxing payload JSON
		status TEXT NOT NULL,             -- measured, failed, validated, submitted
		verified INTEGER,                 -- server verdict (dry-run or submit); NULL before validation
		issues TEXT NOT NULL DEFAULT '',  -- verification issues, "; "-joined
		submission_id TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX benchmarks_model ON benchmarks(model, ts);`,
	// 6: the client's own request id (X-Request-Id); requests.id is always generated by the gateway.
	`ALTER TABLE requests ADD COLUMN client_request_id TEXT;
	CREATE INDEX requests_client_request_id ON requests(client_request_id) WHERE client_request_id IS NOT NULL;`,
}

// DB is an open usage database.
type DB struct{ *sql.DB }

// Open opens (creating and migrating) the usage database. The database and its WAL files are private to the
// owner (an existing database with wider permissions is tightened), and secure_delete overwrites deleted
// records instead of leaving them in free pages.
func Open() (*DB, error) {
	if err := paths.Private(paths.State()); err != nil {
		return nil, err
	}
	if f, err := os.OpenFile(Path(), os.O_RDONLY|os.O_CREATE, 0o600); err == nil {
		f.Close() // created owner-only: SQLite gives its -wal and -shm files the database's permissions
	}
	if err := privateFiles(); err != nil {
		return nil, err
	}
	dsn := "file:" + Path() + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)" +
		"&_pragma=secure_delete(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("%s: %w", Path(), err)
	}
	return &DB{db}, nil
}

// privateFiles makes the database and its WAL files owner-only.
func privateFiles() error {
	for _, p := range []string{Path(), Path() + "-wal", Path() + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func migrate(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this substrate (%d)", v, len(migrations))
	}
	for ; v < len(migrations); v++ {
		if _, err := tx.Exec(migrations[v]); err != nil {
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return err
	}
	return tx.Commit()
}

// Request is one proxied request.
type Request struct {
	ID string `json:"id"`
	// ClientRequestID is the X-Request-Id the client sent, if any (not unique: clients may repeat it).
	ClientRequestID  string `json:"client_request_id,omitempty"`
	TS               int64  `json:"ts"`
	Model            string `json:"model"`
	Instance         string `json:"instance"`
	Engine           string `json:"engine"`
	API              string `json:"api"`
	Path             string `json:"path"`
	Stream           bool   `json:"stream"`
	Status           int    `json:"status"`
	Error            string `json:"error,omitempty"`
	Client           string `json:"client,omitempty"`
	UserAgent        string `json:"user_agent,omitempty"`
	Remote           string `json:"remote,omitempty"`
	PromptTokens     *int64 `json:"prompt_tokens"`
	CompletionTokens *int64 `json:"completion_tokens"`
	CachedTokens     *int64 `json:"cached_tokens"`
	ReasoningTokens  *int64 `json:"reasoning_tokens"`
	UsageSource      string `json:"usage_source,omitempty"`
	FinishReason     string `json:"finish_reason,omitempty"`
	Autostarted      bool   `json:"autostarted"`
	QueueMS          *int64 `json:"queue_ms"`
	TTFTMS           *int64 `json:"ttft_ms"`
	DurationMS       *int64 `json:"duration_ms"`
	ReqBytes         int64  `json:"req_bytes"`
	RespBytes        int64  `json:"resp_bytes"`
	// Category is the task class of the last user turn ("" when classification is off or there was no text).
	Category string `json:"category,omitempty"`
	// Flags are the policy rules the prompt matched; FlagSeverity is the highest of them.
	Flags        []policy.Flag `json:"flags,omitempty"`
	FlagSeverity string        `json:"flag_severity,omitempty"`
	// ErrorKind classifies failures (see ErrorKinds); "" for successful requests.
	ErrorKind string `json:"error_kind,omitempty"`
	// Team is the API key's team when the request was made; Cost prices its tokens (nil when unpriced).
	Team string   `json:"team,omitempty"`
	Cost *float64 `json:"cost"`

	// Bodies, when captured, are stored in the bodies table.
	RequestBody   []byte `json:"-"`
	ResponseBody  []byte `json:"-"`
	BodyTruncated bool   `json:"-"`
}

// ErrorKinds describes the error_kind values the gateway records.
var ErrorKinds = map[string]string{
	"auth_failed":         "missing or invalid API key (401)",
	"forbidden":           "the key lacks the role or model access (403)",
	"rate_limited":        "requests-per-minute, daily token quota or queue limit (429)",
	"budget_exceeded":     "the key's team used up a token or cost budget (429)",
	"policy_blocked":      "the prompt matched a blocking policy rule (400)",
	"bad_request":         "unreadable, oversized or non-JSON body (400/413)",
	"model_not_found":     "no registered model or running instance by that name (404)",
	"no_capacity":         "the model could not be placed or started in time (503)",
	"engine_start_failed": "the engine process failed to start (502)",
	"engine_unreachable":  "the engine could not be reached or dropped the connection (502)",
	"upstream_timeout":    "the engine sent nothing for upstream_idle_timeout (502)",
	"engine_error":        "the engine answered with a 5xx",
	"engine_rejected":     "the engine answered with a 4xx (bad parameters, context too long, ...)",
	"client_disconnected": "the client went away before the response finished (499)",
	"gateway_error":       "any other gateway failure",
}

const requestCols = `id, ts, model, instance, engine, api, path, stream, status, error, client, user_agent, remote,
	prompt_tokens, completion_tokens, cached_tokens, reasoning_tokens, usage_source, finish_reason, autostarted,
	queue_ms, ttft_ms, duration_ms, req_bytes, resp_bytes, category, flags, flag_severity, error_kind, team, cost,
	client_request_id`

// InsertRequest records r (and its bodies when present).
func (db *DB) InsertRequest(r *Request) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	flags := ""
	if len(r.Flags) > 0 {
		data, err := json.Marshal(r.Flags)
		if err != nil {
			return err
		}
		flags = string(data)
	}
	_, err = tx.Exec(`INSERT INTO requests (`+requestCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.TS, r.Model, r.Instance, r.Engine, r.API, r.Path, r.Stream, r.Status, r.Error, r.Client, r.UserAgent, r.Remote,
		r.PromptTokens, r.CompletionTokens, r.CachedTokens, r.ReasoningTokens, r.UsageSource, r.FinishReason, r.Autostarted,
		r.QueueMS, r.TTFTMS, r.DurationMS, r.ReqBytes, r.RespBytes, r.Category, flags, r.FlagSeverity, r.ErrorKind, r.Team, r.Cost,
		sql.NullString{String: r.ClientRequestID, Valid: r.ClientRequestID != ""})
	if err != nil {
		return err
	}
	if r.RequestBody != nil || r.ResponseBody != nil {
		if _, err := tx.Exec(`INSERT INTO bodies (request_id, request, response, truncated) VALUES (?,?,?,?)`,
			r.ID, r.RequestBody, r.ResponseBody, r.BodyTruncated); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Event is one lifecycle event.
type Event struct {
	ID         int64  `json:"id"`
	TS         int64  `json:"ts"`
	Kind       string `json:"kind"`
	Instance   string `json:"instance"`
	Engine     string `json:"engine"`
	Model      string `json:"model"`
	Owner      string `json:"owner,omitempty"`
	PID        *int64 `json:"pid"`
	Port       *int64 `json:"port"`
	DurationMS *int64 `json:"duration_ms"`
	Detail     string `json:"detail,omitempty"`
}

// InsertEvent records e (TS defaults to now).
func (db *DB) InsertEvent(e *Event) error {
	if e.TS == 0 {
		e.TS = time.Now().UnixMilli()
	}
	_, err := db.Exec(`INSERT INTO events (ts, kind, instance, engine, model, owner, pid, port, duration_ms, detail)
		VALUES (?,?,?,?,?,?,?,?,?,?)`, e.TS, e.Kind, e.Instance, e.Engine, e.Model, e.Owner, e.PID, e.Port, e.DurationMS, e.Detail)
	return err
}

// GPUSample is one GPU reading.
type GPUSample struct {
	TS          int64   `json:"ts"`
	GPU         int     `json:"gpu"`
	Name        string  `json:"name"`
	MemUsedMiB  int64   `json:"mem_used_mib"`
	MemTotalMiB int64   `json:"mem_total_mib"`
	UtilPct     int64   `json:"util_pct"`
	PowerW      float64 `json:"power_w"`
}

// InstanceSample is one instance resource reading.
type InstanceSample struct {
	TS        int64  `json:"ts"`
	Instance  string `json:"instance"`
	Engine    string `json:"engine"`
	Model     string `json:"model"`
	GPUMemMiB int64  `json:"gpu_mem_mib"`
	RSSMiB    int64  `json:"rss_mib"`
	Processes int64  `json:"processes"`
}

// InsertSamples records one sampling round.
func (db *DB) InsertSamples(gs []GPUSample, is []InstanceSample) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, g := range gs {
		if _, err := tx.Exec(`INSERT INTO gpu_samples (ts, gpu, name, mem_used_mib, mem_total_mib, util_pct, power_w) VALUES (?,?,?,?,?,?,?)`,
			g.TS, g.GPU, g.Name, g.MemUsedMiB, g.MemTotalMiB, g.UtilPct, g.PowerW); err != nil {
			return err
		}
	}
	for _, s := range is {
		if _, err := tx.Exec(`INSERT INTO instance_samples (ts, instance, engine, model, gpu_mem_mib, rss_mib, processes) VALUES (?,?,?,?,?,?,?)`,
			s.TS, s.Instance, s.Engine, s.Model, s.GPUMemMiB, s.RSSMiB, s.Processes); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Prune deletes usage records (requests, bodies, lifecycle events, samples) older than before and admin-trail
// events older than auditBefore; a zero time leaves that part alone. It returns how many rows went per table.
// With secure_delete the rows are overwritten, and the WAL is truncated so no copy of them stays there either.
func (db *DB) Prune(before, auditBefore time.Time) (map[string]int64, error) {
	type stmt struct {
		table, sql string
		cutoff     time.Time
	}
	stmts := []stmt{
		{"bodies", `DELETE FROM bodies WHERE request_id IN (SELECT id FROM requests WHERE ts < ?)`, before},
		{"requests", `DELETE FROM requests WHERE ts < ?`, before},
		{"events", `DELETE FROM events WHERE ts < ? AND kind != 'admin'`, before},
		{"gpu_samples", `DELETE FROM gpu_samples WHERE ts < ?`, before},
		{"instance_samples", `DELETE FROM instance_samples WHERE ts < ?`, before},
		{"admin_events", `DELETE FROM events WHERE ts < ? AND kind = 'admin'`, auditBefore},
	}
	out := map[string]int64{}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, s := range stmts {
		if s.cutoff.IsZero() {
			continue
		}
		res, err := tx.Exec(s.sql, s.cutoff.UnixMilli())
		if err != nil {
			return nil, err
		}
		out[s.table], _ = res.RowsAffected()
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, db.truncateWAL()
}

// EraseClient deletes every request record (and stored body) of one client (an API key name, or a client's
// self-reported name in open mode), for a data-subject erasure request. The admin trail and the audit file
// are kept: they are the record that the erasure happened.
func (db *DB) EraseClient(client string) (map[string]int64, error) {
	if client == "" {
		return nil, fmt.Errorf("erase: a client name is required")
	}
	out := map[string]int64{}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, s := range []struct{ table, sql string }{
		{"bodies", `DELETE FROM bodies WHERE request_id IN (SELECT id FROM requests WHERE client = ?)`},
		{"requests", `DELETE FROM requests WHERE client = ?`},
	} {
		res, err := tx.Exec(s.sql, client)
		if err != nil {
			return nil, err
		}
		out[s.table], _ = res.RowsAffected()
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, db.truncateWAL()
}

// truncateWAL checkpoints the WAL into the database and empties it, so deleted rows survive in neither.
func (db *DB) truncateWAL() error {
	_, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// Filter narrows queries.
type Filter struct {
	Since    time.Time
	Until    time.Time
	Model    string
	Engine   string
	Instance string
	Client   string
	Team     string
	ID       string // requests only
	Kind     string // events only
	Limit    int

	// Requests only:
	Category  string
	ErrorKind string
	// Errors keeps failed requests (status >= 400 or an error).
	Errors bool
	// Flagged keeps requests whose highest flag severity is at least MinSeverity (any flag when empty).
	Flagged     bool
	MinSeverity string
	// Rule keeps requests flagged by this policy rule.
	Rule string
}

// requestExtras are the Filter fields that apply to the requests table.
var requestExtras = []string{"model", "engine", "instance", "client", "team", "id", "category", "error_kind", "errors", "flagged", "rule"}

func (f Filter) where(tsCol string, extra ...string) (string, []any) {
	var conds []string
	var args []any
	if !f.Since.IsZero() {
		conds, args = append(conds, tsCol+" >= ?"), append(args, f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		conds, args = append(conds, tsCol+" < ?"), append(args, f.Until.UnixMilli())
	}
	add := func(col, v string) {
		if v != "" {
			conds, args = append(conds, col+" = ?"), append(args, v)
		}
	}
	for _, c := range extra {
		switch c {
		case "model":
			add("model", f.Model)
		case "engine":
			add("engine", f.Engine)
		case "instance":
			add("instance", f.Instance)
		case "client":
			add("client", f.Client)
		case "team":
			add("team", f.Team)
		case "id":
			if f.ID != "" { // the gateway's id, or the id the client sent
				conds, args = append(conds, "(id = ? OR client_request_id = ?)"), append(args, f.ID, f.ID)
			}
		case "kind":
			add("kind", f.Kind)
		case "category":
			add("category", f.Category)
		case "error_kind":
			add("error_kind", f.ErrorKind)
		case "errors":
			if f.Errors {
				conds = append(conds, "(status >= 400 OR error != '')")
			}
		case "flagged":
			if f.Flagged || f.MinSeverity != "" {
				sev := policy.Severities
				for i, s := range sev {
					if s == f.MinSeverity {
						sev = sev[i:]
					}
				}
				conds = append(conds, "flag_severity IN ("+strings.TrimSuffix(strings.Repeat("?,", len(sev)), ",")+")")
				for _, s := range sev {
					args = append(args, s)
				}
			}
		case "rule":
			if f.Rule != "" {
				conds = append(conds, "flags != '' AND EXISTS (SELECT 1 FROM json_each(requests.flags) WHERE json_extract(value, '$.rule') = ?)")
				args = append(args, f.Rule)
			}
		}
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func limit(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" LIMIT %d", n)
}

// Requests lists requests, newest first.
func (db *DB) Requests(f Filter) ([]Request, error) {
	w, args := f.where("ts", requestExtras...)
	rows, err := db.Query(`SELECT `+requestCols+` FROM requests`+w+` ORDER BY ts DESC`+limit(f.Limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Request{} // an empty result is [] in JSON, not null
	for rows.Next() {
		var r Request
		var flags string
		var clientID sql.NullString
		if err := rows.Scan(&r.ID, &r.TS, &r.Model, &r.Instance, &r.Engine, &r.API, &r.Path, &r.Stream, &r.Status, &r.Error,
			&r.Client, &r.UserAgent, &r.Remote, &r.PromptTokens, &r.CompletionTokens, &r.CachedTokens, &r.ReasoningTokens,
			&r.UsageSource, &r.FinishReason, &r.Autostarted, &r.QueueMS, &r.TTFTMS, &r.DurationMS, &r.ReqBytes, &r.RespBytes,
			&r.Category, &flags, &r.FlagSeverity, &r.ErrorKind, &r.Team, &r.Cost, &clientID); err != nil {
			return nil, err
		}
		r.ClientRequestID = clientID.String
		if flags != "" {
			if err := json.Unmarshal([]byte(flags), &r.Flags); err != nil {
				return nil, fmt.Errorf("request %s: flags: %w", r.ID, err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Bodies returns the captured bodies of request id.
func (db *DB) Bodies(id string) (req, resp []byte, truncated, ok bool, err error) {
	err = db.QueryRow(`SELECT request, response, truncated FROM bodies WHERE request_id = ?`, id).Scan(&req, &resp, &truncated)
	if err == sql.ErrNoRows {
		return nil, nil, false, false, nil
	}
	return req, resp, truncated, err == nil, err
}

// Events lists lifecycle events, newest first.
func (db *DB) Events(f Filter) ([]Event, error) {
	w, args := f.where("ts", "model", "engine", "instance", "kind")
	rows, err := db.Query(`SELECT id, ts, kind, instance, engine, model, owner, pid, port, duration_ms, detail FROM events`+w+
		` ORDER BY ts DESC, id DESC`+limit(f.Limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.Instance, &e.Engine, &e.Model, &e.Owner, &e.PID, &e.Port, &e.DurationMS, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Group is an aggregation dimension for Usage.
var Groups = map[string]string{
	"model":         "model",
	"engine":        "engine",
	"instance":      "instance",
	"client":        "client",
	"team":          "team",
	"api":           "api",
	"category":      "category",
	"error_kind":    "error_kind",
	"flag_severity": "flag_severity",
	"status":        "CAST(status AS TEXT)",
	"day":           "strftime('%Y-%m-%d', ts/1000, 'unixepoch', 'localtime')",
	"hour":          "strftime('%Y-%m-%d %H:00', ts/1000, 'unixepoch', 'localtime')",
}

// UsageRow is one aggregated group.
type UsageRow struct {
	Key              string   `json:"key"`
	Requests         int64    `json:"requests"`
	Errors           int64    `json:"errors"`
	Flagged          int64    `json:"flagged"` // requests matching any policy rule
	Blocked          int64    `json:"blocked"` // refused by a blocking rule
	Models           int64    `json:"models"`  // distinct models used
	Clients          int64    `json:"clients"` // distinct clients
	Cost             *float64 `json:"cost"`    // sum over priced requests; nil when none was priced
	PromptTokens     int64    `json:"prompt_tokens"`
	CompletionTokens int64    `json:"completion_tokens"`
	CachedTokens     int64    `json:"cached_tokens"`
	ReasoningTokens  int64    `json:"reasoning_tokens"`
	UnmeteredReqs    int64    `json:"unmetered_requests"` // no token counts from the engine
	AvgTTFTMS        *float64 `json:"avg_ttft_ms"`
	AvgDurationMS    *float64 `json:"avg_duration_ms"`
	// DecodeTokPerSec is completion tokens over generation time (duration minus TTFT) for metered streams.
	DecodeTokPerSec *float64 `json:"decode_tok_per_sec"`
}

// Usage aggregates requests by group (a Groups key; "" totals everything).
func (db *DB) Usage(group string, f Filter) ([]UsageRow, error) {
	key := "'total'"
	if group != "" {
		var ok bool
		if key, ok = Groups[group]; !ok {
			return nil, fmt.Errorf("unknown group %q", group)
		}
	}
	w, args := f.where("ts", requestExtras...)
	q := `SELECT ` + key + ` AS k, COUNT(*),
		SUM(CASE WHEN status >= 400 OR error != '' THEN 1 ELSE 0 END),
		SUM(CASE WHEN flag_severity != '' THEN 1 ELSE 0 END), SUM(CASE WHEN error_kind = 'policy_blocked' THEN 1 ELSE 0 END),
		COUNT(DISTINCT NULLIF(model, '')), COUNT(DISTINCT NULLIF(client, '')), SUM(cost),
		COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(cached_tokens), 0), COALESCE(SUM(reasoning_tokens), 0),
		SUM(CASE WHEN status < 400 AND completion_tokens IS NULL THEN 1 ELSE 0 END),
		AVG(ttft_ms), AVG(duration_ms),
		SUM(CASE WHEN stream AND ttft_ms IS NOT NULL AND completion_tokens > 1 AND duration_ms > ttft_ms THEN completion_tokens - 1 END) * 1000.0 /
		NULLIF(SUM(CASE WHEN stream AND ttft_ms IS NOT NULL AND completion_tokens > 1 AND duration_ms > ttft_ms THEN duration_ms - ttft_ms END), 0)
		FROM requests` + w + ` GROUP BY k ORDER BY k`
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageRow{}
	for rows.Next() {
		var u UsageRow
		if err := rows.Scan(&u.Key, &u.Requests, &u.Errors, &u.Flagged, &u.Blocked, &u.Models, &u.Clients, &u.Cost, &u.PromptTokens,
			&u.CompletionTokens, &u.CachedTokens, &u.ReasoningTokens, &u.UnmeteredReqs, &u.AvgTTFTMS, &u.AvgDurationMS, &u.DecodeTokPerSec); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// FlagRow aggregates policy matches of one rule and detector.
type FlagRow struct {
	Rule     string `json:"rule"`
	Detector string `json:"detector"`
	Severity string `json:"severity"`
	Action   string `json:"action"`
	Requests int64  `json:"requests"` // requests with at least one match
	Matches  int64  `json:"matches"`
	Clients  int64  `json:"clients"` // distinct clients that sent them
	LastTS   int64  `json:"last_ts"`
}

// FlagStats aggregates the flags of requests matching f, most frequent first.
func (db *DB) FlagStats(f Filter) ([]FlagRow, error) {
	w, args := f.where("ts", requestExtras...)
	if w == "" {
		w = " WHERE flags != ''"
	} else {
		w += " AND flags != ''"
	}
	rows, err := db.Query(`SELECT json_extract(j.value, '$.rule'), json_extract(j.value, '$.detector'),
		json_extract(j.value, '$.severity'), json_extract(j.value, '$.action'),
		COUNT(DISTINCT requests.id), SUM(json_extract(j.value, '$.count')), COUNT(DISTINCT NULLIF(client, '')), MAX(ts)
		FROM requests, json_each(requests.flags) AS j`+w+`
		GROUP BY 1, 2, 3, 4 ORDER BY 5 DESC, 1, 2`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FlagRow
	for rows.Next() {
		var r FlagRow
		if err := rows.Scan(&r.Rule, &r.Detector, &r.Severity, &r.Action, &r.Requests, &r.Matches, &r.Clients, &r.LastTS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ErrorRow aggregates failed requests of one kind.
type ErrorRow struct {
	Kind        string `json:"kind"`
	Count       int64  `json:"count"`
	Models      int64  `json:"models"`
	Clients     int64  `json:"clients"`
	LastTS      int64  `json:"last_ts"`
	LastError   string `json:"last_error"`
	LastModel   string `json:"last_model"`
	LastRequest string `json:"last_request"`
}

// ErrorStats aggregates failed requests matching f by error kind, most frequent first. Requests recorded
// before error kinds existed count as "unclassified".
func (db *DB) ErrorStats(f Filter) ([]ErrorRow, error) {
	f.Errors = true
	w, args := f.where("ts", requestExtras...)
	rows, err := db.Query(`SELECT k, n, models, clients, last_ts,
			(SELECT error FROM requests r2 WHERE r2.ts = last_ts AND COALESCE(NULLIF(r2.error_kind, ''), 'unclassified') = k AND (r2.status >= 400 OR r2.error != '') LIMIT 1),
			(SELECT model FROM requests r2 WHERE r2.ts = last_ts AND COALESCE(NULLIF(r2.error_kind, ''), 'unclassified') = k AND (r2.status >= 400 OR r2.error != '') LIMIT 1),
			(SELECT id FROM requests r2 WHERE r2.ts = last_ts AND COALESCE(NULLIF(r2.error_kind, ''), 'unclassified') = k AND (r2.status >= 400 OR r2.error != '') LIMIT 1)
		FROM (SELECT COALESCE(NULLIF(error_kind, ''), 'unclassified') AS k, COUNT(*) AS n,
			COUNT(DISTINCT NULLIF(model, '')) AS models, COUNT(DISTINCT NULLIF(client, '')) AS clients, MAX(ts) AS last_ts
			FROM requests`+w+` GROUP BY k)
		ORDER BY n DESC, k`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ErrorRow
	for rows.Next() {
		var r ErrorRow
		var lastErr, lastModel, lastID sql.NullString
		if err := rows.Scan(&r.Kind, &r.Count, &r.Models, &r.Clients, &r.LastTS, &lastErr, &lastModel, &lastID); err != nil {
			return nil, err
		}
		r.LastError, r.LastModel, r.LastRequest = lastErr.String, lastModel.String, lastID.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// TeamUse is a team's metered tokens and cost over a period.
type TeamUse struct {
	Tokens int64
	Cost   float64
}

// TeamSince sums the tokens and cost recorded for team since t.
func (db *DB) TeamSince(team string, since time.Time) (TeamUse, error) {
	var u TeamUse
	err := db.QueryRow(`SELECT COALESCE(SUM(COALESCE(prompt_tokens,0) + COALESCE(completion_tokens,0)), 0), COALESCE(SUM(cost), 0)
		FROM requests WHERE team = ? AND ts >= ?`, team, since.UnixMilli()).Scan(&u.Tokens, &u.Cost)
	return u, err
}

// LastUsed returns each client's latest successfully authenticated request (unix ms): for API keys, when the key was last used.
func (db *DB) LastUsed() (map[string]int64, error) {
	rows, err := db.Query(`SELECT client, MAX(ts) FROM requests WHERE client != '' AND error_kind NOT IN ('auth_failed', 'forbidden') GROUP BY client`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var c string
		var ts int64
		if err := rows.Scan(&c, &ts); err != nil {
			return nil, err
		}
		out[c] = ts
	}
	return out, rows.Err()
}

// TokensSince sums prompt and completion tokens recorded for client since t.
func (db *DB) TokensSince(client string, since time.Time) (int64, error) {
	var n int64
	err := db.QueryRow(`SELECT COALESCE(SUM(COALESCE(prompt_tokens,0) + COALESCE(completion_tokens,0)), 0)
		FROM requests WHERE client = ? AND ts >= ?`, client, since.UnixMilli()).Scan(&n)
	return n, err
}

// LastLoad returns how long model last took to become ready (0 when never recorded).
func (db *DB) LastLoad(model string) (time.Duration, error) {
	var ms sql.NullInt64
	err := db.QueryRow(`SELECT duration_ms FROM events WHERE model = ? AND kind = 'ready' ORDER BY ts DESC LIMIT 1`, model).Scan(&ms)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return time.Duration(ms.Int64) * time.Millisecond, err
}

// PeakGPU returns the largest GPU footprint (MiB, all GPUs) sampled for model on engine since t; 0 when none.
func (db *DB) PeakGPU(model, engine string, since time.Time) (int64, error) {
	var peak sql.NullInt64
	err := db.QueryRow(`SELECT MAX(gpu_mem_mib) FROM instance_samples WHERE model = ? AND engine = ? AND ts >= ?`,
		model, engine, since.UnixMilli()).Scan(&peak)
	return peak.Int64, err
}

// InstanceSamples lists instance samples, newest first.
func (db *DB) InstanceSamples(f Filter) ([]InstanceSample, error) {
	w, args := f.where("ts", "model", "engine", "instance")
	rows, err := db.Query(`SELECT ts, instance, engine, model, gpu_mem_mib, rss_mib, processes FROM instance_samples`+w+
		` ORDER BY ts DESC`+limit(f.Limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InstanceSample{}
	for rows.Next() {
		var s InstanceSample
		if err := rows.Scan(&s.TS, &s.Instance, &s.Engine, &s.Model, &s.GPUMemMiB, &s.RSSMiB, &s.Processes); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GPUSamples lists GPU samples, newest first.
func (db *DB) GPUSamples(f Filter) ([]GPUSample, error) {
	w, args := f.where("ts")
	rows, err := db.Query(`SELECT ts, gpu, name, mem_used_mib, mem_total_mib, util_pct, power_w FROM gpu_samples`+w+
		` ORDER BY ts DESC, gpu`+limit(f.Limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []GPUSample{}
	for rows.Next() {
		var g GPUSample
		if err := rows.Scan(&g.TS, &g.GPU, &g.Name, &g.MemUsedMiB, &g.MemTotalMiB, &g.UtilPct, &g.PowerW); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

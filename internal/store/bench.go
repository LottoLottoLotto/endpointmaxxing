package store

import (
	"database/sql"
	"fmt"
)

// Benchmark is one measured configuration of `emx bench run` (one concurrency level).
type Benchmark struct {
	ID           string   `json:"id"`
	TS           int64    `json:"ts"`
	Model        string   `json:"model"`
	Engine       string   `json:"engine"`
	Instance     string   `json:"instance"`
	Via          string   `json:"via"`
	Concurrency  int      `json:"concurrency"`
	Prompt       string   `json:"prompt"`
	TokSOut      *float64 `json:"tok_s_out"`
	TokSPrefill  *float64 `json:"tok_s_prefill"`
	TokSTotal    *float64 `json:"tok_s_total"`
	TTFTMS       *float64 `json:"ttft_ms"`
	PromptTokens *int64   `json:"prompt_tokens"`
	OutputTokens *int64   `json:"output_tokens"`
	Payload      string   `json:"payload"`
	Status       string   `json:"status"`
	Verified     *bool    `json:"verified"`
	Issues       string   `json:"issues,omitempty"`
	SubmissionID string   `json:"submission_id,omitempty"`
	Error        string   `json:"error,omitempty"`
}

const benchCols = `id, ts, model, engine, instance, via, concurrency, prompt, tok_s_out, tok_s_prefill, tok_s_total, ttft_ms,
	prompt_tokens, output_tokens, payload, status, verified, issues, submission_id, error`

// SaveBenchmark inserts or replaces b.
func (db *DB) SaveBenchmark(b *Benchmark) error {
	_, err := db.Exec(`INSERT OR REPLACE INTO benchmarks (`+benchCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.ID, b.TS, b.Model, b.Engine, b.Instance, b.Via, b.Concurrency, b.Prompt, b.TokSOut, b.TokSPrefill, b.TokSTotal, b.TTFTMS,
		b.PromptTokens, b.OutputTokens, b.Payload, b.Status, b.Verified, b.Issues, b.SubmissionID, b.Error)
	return err
}

// Benchmarks lists runs, newest first (model "" = all; limit 0 = all).
func (db *DB) Benchmarks(model string, limit int) ([]Benchmark, error) {
	q, args := `SELECT `+benchCols+` FROM benchmarks`, []any{}
	if model != "" {
		q, args = q+` WHERE model = ?`, append(args, model)
	}
	q += ` ORDER BY ts DESC, id DESC`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Benchmark
	for rows.Next() {
		var b Benchmark
		if err := rows.Scan(&b.ID, &b.TS, &b.Model, &b.Engine, &b.Instance, &b.Via, &b.Concurrency, &b.Prompt, &b.TokSOut,
			&b.TokSPrefill, &b.TokSTotal, &b.TTFTMS, &b.PromptTokens, &b.OutputTokens, &b.Payload, &b.Status, &b.Verified,
			&b.Issues, &b.SubmissionID, &b.Error); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Benchmark returns run id (ok false when absent).
func (db *DB) Benchmark(id string) (Benchmark, bool, error) {
	var b Benchmark
	err := db.QueryRow(`SELECT `+benchCols+` FROM benchmarks WHERE id = ?`, id).Scan(&b.ID, &b.TS, &b.Model, &b.Engine,
		&b.Instance, &b.Via, &b.Concurrency, &b.Prompt, &b.TokSOut, &b.TokSPrefill, &b.TokSTotal, &b.TTFTMS, &b.PromptTokens,
		&b.OutputTokens, &b.Payload, &b.Status, &b.Verified, &b.Issues, &b.SubmissionID, &b.Error)
	if err == sql.ErrNoRows {
		return b, false, nil
	}
	return b, err == nil, err
}

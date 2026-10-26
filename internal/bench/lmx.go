package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// LMX runs the LocalMaxxing CLI.
type LMX struct {
	Bin string
	// Log receives lmx's progress (stderr); nil discards it.
	Log io.Writer
}

// FindLMX locates lmx: $SUBSTRATE_LMX, then PATH.
func FindLMX() (*LMX, error) {
	if p := os.Getenv("SUBSTRATE_LMX"); p != "" {
		return &LMX{Bin: p}, nil
	}
	p, err := exec.LookPath("lmx")
	if err != nil {
		return nil, fmt.Errorf("lmx (the LocalMaxxing CLI) is not on PATH: install it (https://github.com/LottoLottoLotto/localmaxxing-cli) or set SUBSTRATE_LMX")
	}
	return &LMX{Bin: p}, nil
}

// run executes lmx with --json --quiet and decodes its stdout into out (when not nil).
func (l *LMX) run(ctx context.Context, out any, args ...string) error {
	cmd := exec.CommandContext(ctx, l.Bin, append(args, "--json", "--quiet")...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if l.Log != nil {
		cmd.Stderr = io.MultiWriter(&stderr, l.Log)
	}
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(stdout.Bytes(), &e) == nil && (e.Error != "" || e.Message != "") {
			msg = strings.TrimSpace(e.Message + " " + e.Error)
		}
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return fmt.Errorf("lmx %s: %v: %s", args[0]+" "+args[1], err, lastLines(msg, 6))
	}
	if out != nil {
		if err := json.Unmarshal(stdout.Bytes(), out); err != nil {
			return fmt.Errorf("lmx %s %s: unreadable output: %v", args[0], args[1], err)
		}
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Run is one measurement.
type Run struct {
	BaseURL     string
	ServedModel string
	APIKey      string
	Concurrency int
	// CanonicalPrompt is a LocalMaxxing prompt id (verified runs); PromptFile replaces it.
	CanonicalPrompt string
	PromptFile      string
	MaxTokens       int
	Iterations      int
	Warmup          int
	Notes           string
	HardwareFile    string
	Out             string
}

// Measure runs `lmx speed-test run` against the endpoint and writes the LocalMaxxing payload to r.Out.
func (l *LMX) Measure(ctx context.Context, m *Meta, r Run) (map[string]any, error) {
	args := []string{"speed-test", "run", m.EngineName, "--mode", "remote", "--base-url", r.BaseURL,
		"--served-model", r.ServedModel, "--hf-id", m.HFID, "--quantization", m.Quantization,
		"--hardware", r.HardwareFile, "--concurrency", strconv.Itoa(r.Concurrency), "--out", r.Out}
	add := func(flag, value string) {
		if value != "" {
			args = append(args, flag, value)
		}
	}
	add("--model-revision", m.Revision)
	add("--backend", m.Backend)
	add("--engine-version", m.EngineVersion)
	add("--engine-repository", m.EngineRepository)
	add("--engine-commit", m.EngineCommit)
	add("--model-api-key", r.APIKey)
	add("--notes", r.Notes)
	if r.PromptFile != "" {
		add("--prompt-file", r.PromptFile)
	} else {
		add("--canonical-prompt", r.CanonicalPrompt)
	}
	for flag, v := range map[string]int{"--max-tokens": r.MaxTokens, "--iterations": r.Iterations, "--warmup": r.Warmup, "--context-length": m.ContextLength} {
		if v > 0 {
			add(flag, strconv.Itoa(v))
		}
	}
	if s := m.Spec; s != nil && s.Method != "" {
		args = append(args, "--spec-decoding", "--spec-method", s.Method)
		if s.NumTokens > 0 {
			add("--spec-num-tokens", strconv.Itoa(s.NumTokens))
		}
		add("--spec-draft-model", s.DraftModel)
	}
	if err := l.run(ctx, nil, args...); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(r.Out)
	if err != nil {
		return nil, fmt.Errorf("lmx wrote no payload: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("payload %s: %w", r.Out, err)
	}
	return payload, nil
}

// Verdict is the server's view of a payload.
type Verdict struct {
	Verified bool
	Issues   []string
	// SubmissionID is set after a submit.
	SubmissionID string
	Status       string
}

// Validate asks LocalMaxxing to check payload (no upload).
func (l *LMX) Validate(ctx context.Context, payload string) (*Verdict, error) {
	var out struct {
		Valid        bool `json:"valid"`
		Verification struct {
			Verified bool     `json:"verified"`
			Issues   []string `json:"issues"`
		} `json:"verification"`
	}
	if err := l.run(ctx, &out, "speed-test", "dry-run", payload); err != nil {
		return nil, err
	}
	if !out.Valid {
		return nil, fmt.Errorf("LocalMaxxing rejected the payload as invalid")
	}
	return &Verdict{Verified: out.Verification.Verified, Issues: out.Verification.Issues}, nil
}

// Submit uploads payload.
func (l *LMX) Submit(ctx context.Context, payload string) (*Verdict, error) {
	var out struct {
		ID                 string   `json:"id"`
		Status             string   `json:"status"`
		VerifiedRun        bool     `json:"verifiedRun"`
		VerificationIssues []string `json:"verificationIssues"`
	}
	if err := l.run(ctx, &out, "speed-test", "submit", payload); err != nil {
		return nil, err
	}
	if out.ID == "" {
		return nil, fmt.Errorf("LocalMaxxing returned no submission id")
	}
	return &Verdict{Verified: out.VerifiedRun, Issues: out.VerificationIssues, SubmissionID: out.ID, Status: out.Status}, nil
}

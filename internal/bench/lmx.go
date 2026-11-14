package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
)

// LMX runs the LocalMaxxing CLI.
type LMX struct {
	Bin string
	// Log receives lmx's progress (stderr); nil discards it.
	Log io.Writer
}

// FindLMX locates lmx: $EMX_LMX, then PATH.
func FindLMX() (*LMX, error) {
	if p := os.Getenv("EMX_LMX"); p != "" {
		return &LMX{Bin: p}, nil
	}
	p, err := exec.LookPath("lmx")
	if err != nil {
		return nil, fmt.Errorf("lmx (the LocalMaxxing CLI) is not on PATH: install it (https://github.com/LottoLottoLotto/localmaxxing-cli) or set EMX_LMX")
	}
	return &LMX{Bin: p}, nil
}

// run executes lmx with --json --quiet and decodes its stdout into out (when not nil).
func (l *LMX) run(ctx context.Context, out any, args ...string) error {
	stdout, err := l.runRaw(ctx, nil, args...)
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(stdout, out); err != nil {
			return fmt.Errorf("lmx %s %s: unreadable output: %v", args[0], args[1], err)
		}
	}
	return nil
}

// runRaw executes lmx with --json --quiet and returns its stdout, also when it fails. env adds variables
// to lmx's environment.
func (l *LMX) runRaw(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, l.Bin, append(args, "--json", "--quiet")...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
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
		return stdout.Bytes(), fmt.Errorf("lmx %s: %v: %s", strings.Join(args[:min(2, len(args))], " "), err, lastLines(msg, 6))
	}
	return stdout.Bytes(), nil
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
	// KeyInArgs accepts passing APIKey on lmx's command line when this lmx cannot take it from its
	// environment (every user of the host can read a process's arguments).
	KeyInArgs   bool
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
	// RunsDir is where lmx keeps its own copy of the run; absolute, so a later submit from any working
	// directory finds it to record the receipt.
	RunsDir string
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
	add("--runs-dir", r.RunsDir)
	add("--model-revision", m.Revision)
	add("--backend", m.Backend)
	add("--engine-version", m.EngineVersion)
	add("--engine-repository", m.EngineRepository)
	add("--engine-commit", m.EngineCommit)
	var env []string
	if r.APIKey != "" {
		switch {
		case l.keyFromEnv(ctx):
			env = []string{modelKeyEnv + "=" + r.APIKey}
		case r.KeyInArgs:
			add("--model-api-key", r.APIKey)
		default:
			return nil, fmt.Errorf("this lmx takes the model's API key only on its command line, where every user of this host can read it: update lmx, or accept that with --key-in-args")
		}
	}
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
	if _, err := l.runRaw(ctx, env, args...); err != nil {
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

// modelKeyEnv is the variable a recent lmx reads the model endpoint's key from.
const modelKeyEnv = "LMX_MODEL_API_KEY"

// keyFromEnv reports whether this lmx reads the model key from its environment (`lmx version --json`
// lists the feature); older ones take it only as an argument.
func (l *LMX) keyFromEnv(ctx context.Context) bool {
	out, err := l.runRaw(ctx, nil, "version")
	if err != nil {
		return false
	}
	var v struct {
		Features []string `json:"features"`
	}
	return json.Unmarshal(out, &v) == nil && slices.Contains(v.Features, "model-api-key-env")
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

// submitResponse is LocalMaxxing's answer to an upload.
type submitResponse struct {
	ID                 string   `json:"id"`
	Status             string   `json:"status"`
	VerifiedRun        bool     `json:"verifiedRun"`
	VerificationIssues []string `json:"verificationIssues"`
}

// Submit uploads payload. lmx prints the server's response before recording the receipt in the run files;
// when only that bookkeeping fails, the upload happened, so the response is returned rather than an error
// (retrying would upload the run twice).
func (l *LMX) Submit(ctx context.Context, payload string) (*Verdict, error) {
	stdout, err := l.runRaw(ctx, nil, "speed-test", "submit", payload)
	var out submitResponse
	dec := json.NewDecoder(bytes.NewReader(stdout))
	for {
		var r submitResponse
		if dec.Decode(&r) != nil {
			break
		}
		if r.ID != "" {
			out = r
			break
		}
	}
	if out.ID == "" {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("LocalMaxxing returned no submission id")
	}
	return &Verdict{Verified: out.VerifiedRun, Issues: out.VerificationIssues, SubmissionID: out.ID, Status: out.Status}, nil
}

// Receipt reports the submission lmx already recorded in payload (its "submissionId"), if any: such a run
// was uploaded even if emx did not learn of it.
func Receipt(payload string) (id, status string) {
	data, err := os.ReadFile(payload)
	if err != nil {
		return "", ""
	}
	var p struct {
		SubmissionID     string `json:"submissionId"`
		SubmissionStatus string `json:"submissionStatus"`
	}
	if json.Unmarshal(data, &p) != nil {
		return "", ""
	}
	return p.SubmissionID, p.SubmissionStatus
}

package bench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LottoLottoLotto/substrate/internal/config"
	"github.com/LottoLottoLotto/substrate/internal/paths"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestGitProvenanceNamesTheRemoteThatHasTheCommit(t *testing.T) {
	root := t.TempDir()
	upstream, fork, work := filepath.Join(root, "upstream.git"), filepath.Join(root, "fork.git"), filepath.Join(root, "work")
	git(t, root, "init", "-q", "--bare", upstream)
	git(t, root, "init", "-q", "--bare", fork)
	git(t, root, "init", "-q", work)
	os.WriteFile(filepath.Join(work, "f"), []byte("a"), 0o644)
	git(t, work, "add", "f")
	git(t, work, "commit", "-qm", "a")
	git(t, work, "remote", "add", "origin", upstream)
	git(t, work, "remote", "add", "fork", "git@github.com:me/engine.git")
	git(t, work, "push", "-q", "origin", "HEAD:refs/heads/main")
	git(t, work, "fetch", "-q", "origin")
	os.WriteFile(filepath.Join(work, "f"), []byte("b"), 0o644)
	git(t, work, "commit", "-qam", "b") // a local commit no remote has
	head := git(t, work, "rev-parse", "HEAD")
	sub := filepath.Join(work, "venv", "bin")
	os.MkdirAll(sub, 0o755)

	if repo, commit := gitProvenance(sub); commit != head || repo != "" {
		t.Fatalf("unpublished HEAD: repo %q commit %q", repo, commit)
	}
	git(t, work, "remote", "set-url", "fork", fork)
	git(t, work, "push", "-q", "fork", "HEAD:refs/heads/feature")
	git(t, work, "fetch", "-q", "fork")
	if repo, commit := gitProvenance(sub); commit != head || repo != strings.TrimSuffix(fork, ".git") {
		t.Fatalf("published to fork: repo %q commit %q (want %q)", repo, commit, fork)
	}
	if got := normalizeRepoURL("git@github.com:me/engine.git"); got != "https://github.com/me/engine" {
		t.Fatalf("normalizeRepoURL: %s", got)
	}
}

// fakeLMX writes a shell script that records its arguments and answers like lmx.
func fakeLMX(t *testing.T, verified bool) (*LMX, string) {
	t.Helper()
	if paths.Windows {
		t.Skip("the fake lmx is a shell script")
	}
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	v := "false"
	if verified {
		v = "true"
	}
	script := `#!/bin/sh
echo "$@" >> ` + argsFile + `
case "$1 $2" in
"speed-test run")
  while [ $# -gt 0 ]; do [ "$1" = --out ] && out=$2; shift; done
  echo '{"tokSOut": 27.5, "tokSPrefill": 159.6, "ttftMs": 2181, "promptTokens": 348, "outputTokens": 1024}' > "$out"
  echo '{}' ;;
"speed-test dry-run") echo '{"valid": true, "verification": {"verified": ` + v + `, "issues": ["x"]}}' ;;
"speed-test submit") echo '{"id": "run_1", "status": "APPROVED", "verifiedRun": ` + v + `, "verificationIssues": []}' ;;
esac
`
	bin := filepath.Join(dir, "lmx")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &LMX{Bin: bin}, argsFile
}

func TestLMXArgumentsAndVerdicts(t *testing.T) {
	l, argsFile := fakeLMX(t, true)
	out := filepath.Join(t.TempDir(), "p.json")
	m := &Meta{EngineName: "strata", HFID: "org/m", Quantization: "IQ3_S", Backend: "tt-metal", EngineCommit: "abc1234",
		ContextLength: 32768, Spec: &config.BenchSpec{Method: "MTP", NumTokens: 3}}
	payload, err := l.Measure(context.Background(), m, Run{BaseURL: "http://h:1/v1", ServedModel: "m", Concurrency: 1,
		CanonicalPrompt: "reasoning-v1", MaxTokens: 1024, HardwareFile: "hw.json", Out: out})
	if err != nil || payload["tokSOut"] != 27.5 {
		t.Fatalf("measure: %v %v", payload, err)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"speed-test run strata --mode remote --base-url http://h:1/v1", "--hf-id org/m", "--quantization IQ3_S",
		"--canonical-prompt reasoning-v1", "--spec-decoding --spec-method MTP", "--spec-num-tokens 3", "--engine-commit abc1234",
		"--backend tt-metal", "--context-length 32768", "--json --quiet"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("lmx arguments lack %q:\n%s", want, args)
		}
	}
	v, err := l.Validate(context.Background(), out)
	if err != nil || !v.Verified || v.Issues[0] != "x" {
		t.Fatalf("validate: %+v %v", v, err)
	}
	s, err := l.Submit(context.Background(), out)
	if err != nil || s.SubmissionID != "run_1" || !s.Verified {
		t.Fatalf("submit: %+v %v", s, err)
	}
}

// lmx prints the server's response, then fails to record the receipt in its run files (exit 1): the run was
// uploaded, so Submit must report it rather than fail and invite a duplicate upload.
func TestSubmitSucceedsWhenOnlyTheReceiptWriteFails(t *testing.T) {
	if paths.Windows {
		t.Skip("the fake lmx is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "lmx")
	script := `#!/bin/sh
echo '{"id": "run_9", "status": "APPROVED", "verifiedRun": true, "verificationIssues": []}'
echo '{"code": "submission_receipt_write_failed", "message": "Submission succeeded, but its receipt could not be saved."}'
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := (&LMX{Bin: bin}).Submit(context.Background(), "p.json")
	if err != nil || s.SubmissionID != "run_9" || !s.Verified {
		t.Fatalf("submit: %+v %v", s, err)
	}

	// A failure before any upload still fails.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '{\"code\": \"unauthorized\", \"message\": \"bad key\"}'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if s, err := (&LMX{Bin: bin}).Submit(context.Background(), "p.json"); err == nil {
		t.Fatalf("an upload that never happened succeeded: %+v", s)
	}
}

func TestReceiptReadsTheSubmissionLMXRecorded(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.json")
	os.WriteFile(p, []byte(`{"tokSOut": 1, "submissionId": "cm123", "submissionStatus": "approved"}`), 0o600)
	if id, st := Receipt(p); id != "cm123" || st != "approved" {
		t.Fatalf("receipt: %q %q", id, st)
	}
	os.WriteFile(p, []byte(`{"tokSOut": 1}`), 0o600)
	if id, _ := Receipt(p); id != "" {
		t.Fatalf("unsubmitted payload has receipt %q", id)
	}
}

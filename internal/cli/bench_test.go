package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LottoLottoLotto/substrate/internal/config"
	"github.com/LottoLottoLotto/substrate/internal/paths"
	"github.com/LottoLottoLotto/substrate/internal/runtime"
	"github.com/LottoLottoLotto/substrate/internal/store"
)

// fakeLMXScript answers like lmx; verified decides the server's verdict. It returns the arguments file.
func fakeLMXScript(t *testing.T, dir string, verified bool) string {
	t.Helper()
	v := map[bool]string{true: "true", false: "false"}[verified]
	args := filepath.Join(dir, "args")
	script := `#!/bin/sh
echo "$@" >> ` + args + `
case "$1 $2" in
"speed-test run")
  while [ $# -gt 0 ]; do [ "$1" = --out ] && out=$2; shift; done
  echo '{"tokSOut": 27.5, "tokSPrefill": 159.6, "ttftMs": 2181, "promptTokens": 348, "outputTokens": 1024}' > "$out"
  echo '{}' ;;
"speed-test dry-run") echo '{"valid": true, "verification": {"verified": ` + v + `, "issues": ["acceptance stats inconsistent"]}}' ;;
"speed-test submit") echo '{"id": "run_42", "status": "APPROVED", "verifiedRun": ` + v + `, "verificationIssues": []}' ;;
esac
`
	bin := filepath.Join(dir, "lmx")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBSTRATE_LMX", bin)
	return args
}

func selfStart(t *testing.T) uint64 {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Skip("needs /proc")
	}
	s := string(data)
	var tick uint64
	json.Unmarshal([]byte(strings.Fields(s[strings.LastIndexByte(s, ')')+1:])[19]), &tick)
	return tick
}

func TestBenchRunUploadsOnlyVerifiedRuns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SUBSTRATE_HOME", home)
	t.Setenv("SUBSTRATE_ACTOR", "tester")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	cfgFile := filepath.Join(home, "strata-m.json")
	os.WriteFile(cfgFile, []byte(`{"exe": "x", "args": ["y"], "model_name": "m-iq3_s"}`), 0o644)
	hwFile := filepath.Join(home, "hw.json")
	os.WriteFile(hwFile, []byte(`{"hwClass": "DISCRETE_GPU", "gpuName": "Tenstorrent P150", "gpuCount": 1, "vramGb": 32}`), 0o644)
	if err := config.UpdateModels(func(ms *config.Models) error {
		ms.Put(config.Model{Name: "m", Source: cfgFile, Format: "strata-config", Engine: "strata", Quant: "IQ3_S",
			Bench: &config.BenchMeta{HFID: "org/m", Hardware: hwFile, Spec: &config.BenchSpec{Method: "MTP", NumTokens: 3}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	in := runtime.Instance{ID: "m.strata", Name: "m", Engine: "strata", PID: os.Getpid(), StartTick: selfStart(t),
		BaseURL: backend.URL, HealthURL: backend.URL + "/health", APIs: []string{"chat"}, ReadyAt: &now, Owner: "cli"}
	data, _ := json.Marshal(in)
	os.MkdirAll(filepath.Join(paths.State(), "instances"), 0o755)
	os.WriteFile(filepath.Join(paths.State(), "instances", "m.strata.json"), data, 0o600)

	argsFile := fakeLMXScript(t, t.TempDir(), false)
	var out bytes.Buffer
	e := &env{ctx: context.Background(), stdout: &out, stderr: io.Discard, json: true, owner: "cli"}
	defer e.close()
	if err := execute(e, []string{"bench", "run", "m", "--concurrency", "1,4", "--submit"}); err != nil {
		t.Fatal(err)
	}
	var res struct {
		Runs []store.Benchmark `json:"runs"`
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || len(res.Runs) != 2 {
		t.Fatalf("bench run output: %s (%v)", out.String(), err)
	}
	first := res.Runs[0]
	if first.SubmissionID != "" || first.Status != "validated" || first.Verified == nil || *first.Verified ||
		!strings.Contains(first.Error, "not uploaded") || *first.TokSOut != 27.5 || *first.OutputTokens != 1024 {
		t.Fatalf("an unverified run must be validated but not uploaded: %+v", first)
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"--served-model m", "--concurrency 4", "--spec-method MTP", "--hardware " + filepath.Join(paths.State(), "benchmarks"),
		"--canonical-prompt reasoning-v1", "Served by substrate"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("lmx arguments lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(string(args), "speed-test submit") {
		t.Fatal("an unverified run was uploaded")
	}

	// Once the server would verify it, bench submit uploads the recorded run and records the change.
	fakeLMXScript(t, t.TempDir(), true)
	out.Reset()
	if err := execute(e, []string{"bench", "submit", first.ID}); err != nil {
		t.Fatal(err)
	}
	a, _ := e.app()
	db, _ := a.DB()
	got, _, _ := db.Benchmark(first.ID)
	if got.SubmissionID != "run_42" || got.Status != "submitted" || got.Verified == nil || !*got.Verified || got.Error != "" {
		t.Fatalf("after submit: %+v", got)
	}
	if err := execute(e, []string{"bench", "submit", first.ID}); err == nil {
		t.Fatal("a run was uploaded twice")
	}
	evs, _ := db.Events(store.Filter{})
	found := false
	for _, ev := range evs {
		found = found || (ev.Kind == "admin" && ev.Owner == "tester" && strings.Contains(ev.Detail, "run_42"))
	}
	if !found {
		t.Fatal("the upload is not in the admin trail")
	}
}

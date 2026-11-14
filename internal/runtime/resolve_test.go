package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/paths"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/spec"
)

func engine(t *testing.T, id string, ov config.EngineOverride) *Engine {
	t.Helper()
	ms, err := spec.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		if m.ID == id {
			bin := filepath.Join(t.TempDir(), "bin", "server.exe") // the suffix makes it executable on Windows
			os.MkdirAll(filepath.Dir(bin), 0o755)
			os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
			ov.Bin = bin
			e := Resolve(m, ov)
			if !e.Installed() {
				t.Fatalf("fake binary did not resolve: %v", e.Tried)
			}
			return e
		}
	}
	t.Fatalf("no builtin %s", id)
	return nil
}

func req(flags map[string]string) Request {
	return Request{
		Name: "m", Host: "0.0.0.0", Port: 9000, Flags: flags,
		Model: ModelRef{Source: "org/m", Path: "/models/m", Ref: "/models/m", Format: "safetensors"},
	}
}

func TestBuildTranslatesUnifiedFlags(t *testing.T) {
	e := engine(t, "vllm", config.EngineOverride{Args: []string{"--enforce-eager"}})
	r := req(map[string]string{"ctx": "4096", "kv_cache_type": "q8_0", "gpus": "1", "trust_remote_code": "false"})
	r.Args = []string{"--seed", "1"}
	p, err := e.Build(r)
	if err != nil {
		t.Fatal(err)
	}
	tail := p.Argv[len(p.Argv)-7:]
	want := []string{"--max-model-len", "4096", "--kv-cache-dtype", "fp8", "--enforce-eager", "--seed", "1"}
	if !reflect.DeepEqual(tail, want) {
		t.Fatalf("argv tail = %v, want %v", tail, want)
	}
	if p.Env["CUDA_VISIBLE_DEVICES"] != "1" {
		t.Fatalf("gpus not mapped to env: %v", p.Env)
	}
	if !strings.HasPrefix(p.Env["PATH"], filepath.Dir(e.Bin)+":") {
		t.Fatalf("venv bin not prepended to PATH: %q", p.Env["PATH"])
	}
	// A wildcard bind is probed on loopback.
	if p.HealthURL != "http://127.0.0.1:9000/health" {
		t.Fatalf("health url %s", p.HealthURL)
	}
}

func TestBuildUnsupportedFlag(t *testing.T) {
	e := engine(t, "llama.cpp", config.EngineOverride{})
	r := req(map[string]string{"tp": "2", "ctx": "8192"})
	if _, err := e.Build(r); err == nil || !strings.Contains(err.Error(), "--tp") {
		t.Fatalf("want unsupported --tp error, got %v", err)
	}
	r.Lenient = true
	p, err := e.Build(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Warnings) != 1 || strings.Contains(strings.Join(p.Argv, " "), "tensor") {
		t.Fatalf("lenient build: warnings %v argv %v", p.Warnings, p.Argv)
	}
}

func TestBuildNeedsLocalFileForPathEngines(t *testing.T) {
	e := engine(t, "llama.cpp", config.EngineOverride{})
	r := req(nil)
	r.Model.Path = ""
	if _, err := e.Build(r); err == nil || !strings.Contains(err.Error(), "local model file") {
		t.Fatalf("want local-file error, got %v", err)
	}
}

func TestMemoryOverrideIsPerInstall(t *testing.T) {
	e := engine(t, "strata", config.EngineOverride{Memory: &spec.Memory{Mode: "none"}})
	if e.MemoryMode() != "none" {
		t.Fatalf("override not applied: %s", e.MemoryMode())
	}
	// The builtin manifest itself is untouched (other installs keep exclusive).
	if e2 := engine(t, "strata", config.EngineOverride{}); e2.MemoryMode() != "exclusive" {
		t.Fatalf("override leaked into the shared manifest: %s", e2.MemoryMode())
	}
}

func TestTTVLLMPlanPlacesPreArgsVarsAndChips(t *testing.T) {
	e := engine(t, "tt-vllm", config.EngineOverride{})
	r := req(map[string]string{"ctx": "8192"})
	r.ID, r.Devices = "lfm.tt-vllm", []int{2, 3}
	r.PreArgs = []string{"-e", "LFM2_SPECULATE=1"}
	r.Args = []string{"--max-num-seqs", "1"}
	if _, err := e.Build(r); err == nil || !strings.Contains(err.Error(), "vars.image") {
		t.Fatalf("missing image: %v", err)
	}
	r.Vars = map[string]string{"image": "lfm25-p150:dev-m12", "mesh": "P150x2"}
	p, err := e.Build(r)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(p.Argv, " ")
	for _, want := range []string{
		"--name emx-lfm.tt-vllm",
		"-e TT_VISIBLE_DEVICES=2,3 -e VLLM_API_KEY -e LFM2_SPECULATE=1 --entrypoint", // pre-args sit before the image
		"lfm25-p150:dev-m12 -m vllm.entrypoints.openai.api_server",                   // an image tag survives path cleaning
		"MESH_DEVICE=P150x2",
		"--served-model-name m",
		"--max-model-len 8192 --max-num-seqs 1",
	} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv lacks %q:\n%s", want, argv)
		}
	}
	if p.Accelerator != spec.AccelTenstorrent || len(p.Cleanup) != 1 || p.Cleanup[0][3] != "emx-lfm.tt-vllm" {
		t.Fatalf("plan %+v", p)
	}
}

// The multi-model host launches without a model: its required vars come from the
// engine's config, its server files are shipped to {assets} and mounted into the container.
func TestMultiHostPlanShipsItsServer(t *testing.T) {
	if paths.Windows {
		t.Skip("a Linux-only engine: the manifest's container paths are POSIX paths")
	}
	t.Setenv("EMX_HOME", t.TempDir())
	r := Request{ID: "tt-multihost", Name: "tt-multihost", Host: "127.0.0.1", Port: 9000, Devices: []int{0}}
	if _, err := engine(t, "tt-multihost", config.EngineOverride{}).Build(r); err == nil ||
		!strings.Contains(err.Error(), "emx engine set tt-multihost --var image=") {
		t.Fatalf("missing image: %v", err)
	}
	e := engine(t, "tt-multihost", config.EngineOverride{Vars: map[string]string{"image": "tt:dev", "models": "/srv/models"}})
	p, err := e.Build(r)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(p.Argv, " ")
	for _, want := range []string{"-v /srv/models:/srv/models:ro", "-v " + p.Assets + ":/opt/emx:ro",
		"TT_VISIBLE_DEVICES=0", "tt:dev -m tt_multihost"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv lacks %q:\n%s", want, argv)
		}
	}
	if !p.Hosting || p.Assets == "" {
		t.Fatalf("plan %+v", p)
	}
	if err := spec.SyncAssets(p.Engine, p.Assets); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Assets, "tt_multihost", "__main__.py")); err != nil {
		t.Fatalf("server not shipped: %v", err)
	}
}

func TestStrataVarsOverride(t *testing.T) {
	if paths.Windows {
		t.Skip("compares POSIX paths")
	}
	e := engine(t, "strata", config.EngineOverride{Vars: map[string]string{"server": "tt/serve/server_tt.py"}})
	r := req(nil)
	r.Model.Format = "strata-config"
	p, err := e.Build(r)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(e.Bin)))
	if p.Argv[1] != filepath.Join(root, "tt/serve/server_tt.py") || p.Cwd != root {
		t.Fatalf("argv[1]=%s cwd=%s root=%s", p.Argv[1], p.Cwd, root)
	}
}

// An instance id is a file name: a reference that leaves the instances directory, or a file that records
// another id, is no instance, so a planted record is never loaded and its cleanup commands never run.
func TestLookupsStayInsideTheInstancesDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EMX_HOME", home)
	marker := filepath.Join(home, "cleanup-ran")
	plant := func(path, id string) {
		t.Helper()
		data, _ := json.Marshal(Instance{ID: id, Name: "evil", Cleanup: [][]string{{"touch", marker}}})
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plant(filepath.Join(paths.State(), "evil.json"), "../evil")   // outside the directory, reached with ../
	plant(filepath.Join(instancesDir(), "alias.json"), "../evil") // inside it, but recording another id
	for _, ref := range []string{"../evil", "../../state/evil", "alias", "evil", ""} {
		if _, err := Get(ref); errs.CodeOf(err) != errs.NotFound {
			t.Errorf("Get(%q): want not_found, got %v", ref, err)
		}
		in, err := Find(ref)
		if errs.CodeOf(err) != errs.NotFound {
			t.Errorf("Find(%q): want not_found, got %+v, %v", ref, in, err)
		}
		if in != nil {
			runCleanup(in.Cleanup, in) // what stopping it would do
		}
	}
	if all, _ := List(); len(all) != 0 {
		t.Errorf("List returned planted records: %+v", all[0])
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a planted record's cleanup command ran")
	}
	if _, err := Start(&Plan{}, "../evil", "cli"); errs.CodeOf(err) != errs.Usage {
		t.Fatalf("Start with an id outside the directory: %v", err)
	}
}

// What others see of an instance holds no credential: env values are masked (names stay), and so are
// the values of secret-looking arguments.
func TestRedactedMasksEnvValuesAndSecretArguments(t *testing.T) {
	in := &Instance{APIKey: "sk-engine",
		Argv: []string{"server", "--api-key", "sk-engine", "--hf-token=hf_argv", "-e", "HF_TOKEN=hf_docker", "--port", "9000"},
		Env:  map[string]string{"HF_TOKEN": "hf_env", "LLAMA_API_KEY": "sk-engine", "CUDA_VISIBLE_DEVICES": "0"}}
	r := in.Redacted()
	wantArgv := []string{"server", "--api-key", "***", "--hf-token=***", "-e", "HF_TOKEN=***", "--port", "9000"}
	if !reflect.DeepEqual(r.Argv, wantArgv) {
		t.Fatalf("argv = %v, want %v", r.Argv, wantArgv)
	}
	wantEnv := map[string]string{"HF_TOKEN": "***", "LLAMA_API_KEY": "***", "CUDA_VISIBLE_DEVICES": "***"}
	if !reflect.DeepEqual(r.Env, wantEnv) || r.APIKey != "" {
		t.Fatalf("env = %v, api key %q", r.Env, r.APIKey)
	}
	if in.Env["HF_TOKEN"] != "hf_env" || in.Argv[2] != "sk-engine" {
		t.Fatalf("Redacted changed the instance itself: %+v", in)
	}
}

// Engines that read their API key from the environment get it there: the process list shows no key.
func TestAPIKeyTravelsInTheEnvironmentWhereTheEngineReadsIt(t *testing.T) {
	for id, env := range map[string]string{"vllm": "VLLM_API_KEY", "llama.cpp": "LLAMA_API_KEY", "buun-llama-cpp": "LLAMA_API_KEY"} {
		p, err := engine(t, id, config.EngineOverride{}).Build(req(map[string]string{"api_key": "sk-engine"}))
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if p.Env[env] != "sk-engine" || strings.Contains(strings.Join(p.Argv, " "), "sk-engine") {
			t.Fatalf("%s: key must be in $%s only: env %v argv %v", id, env, p.Env, p.Argv)
		}
	}
	if paths.Windows {
		return // sglang is detected by running its Python, and the fake is a shell script
	}
	// SGLang only takes its key as an argument, so its manifest starts it through a line of Python that
	// reads the key from the environment: the launch command itself carries no key.
	p, err := engine(t, "sglang", config.EngineOverride{}).Build(req(map[string]string{"api_key": "sk-engine"}))
	if err != nil {
		t.Fatal(err)
	}
	if p.Env["SGLANG_API_KEY"] != "sk-engine" || strings.Contains(strings.Join(p.Argv, " "), "sk-engine") {
		t.Fatalf("sglang: key must be in $SGLANG_API_KEY only: env %v argv %v", p.Env, p.Argv)
	}
}

// Secrets an engine prints into its own log (SGLang logs its arguments) are replaced where log text leaves
// the file, and in the file once the engine has stopped.
func TestLogTextAndStoppedLogsCarryNoSecrets(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "engine.log")
	in := &Instance{ID: "m.e", Log: logPath, APIKey: "sk-engine-123456", Env: map[string]string{"HF_TOKEN": "hf_abcdefgh", "CUDA_VISIBLE_DEVICES": "0"}}
	text := "args: {'api_key': 'sk-engine-123456', 'port': 0}\ntoken hf_abcdefgh used on device 0\n"
	if err := os.WriteFile(logPath, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	got := in.Scrub(Tail(logPath, 10))
	if strings.Contains(got, "sk-engine-123456") || strings.Contains(got, "hf_abcdefgh") || !strings.Contains(got, "device 0") {
		t.Fatalf("scrubbed tail: %q", got)
	}
	in.scrubLog()
	data, _ := os.ReadFile(logPath)
	if strings.Contains(string(data), "sk-engine-123456") || strings.Contains(string(data), "hf_abcdefgh") || !strings.Contains(string(data), "'port': 0") {
		t.Fatalf("log file after the engine stopped: %q", data)
	}
	if st, _ := os.Stat(logPath); !paths.OwnerOnly(st.Mode()) {
		t.Fatalf("scrubbed log mode %v", st.Mode().Perm())
	}
}

func TestPlanRedactedMasksKeysWhereverTheyTravel(t *testing.T) {
	p := &Plan{Argv: []string{"server", "--api-key", "sk-engine-123456", "--port", "1"}, apiKey: "sk-engine-123456",
		Env: map[string]string{"LLAMA_API_KEY": "sk-engine-123456", "HF_TOKEN": "hf_abcdefgh", "CUDA_VISIBLE_DEVICES": "0"}}
	r := p.Redacted(false)
	if s := strings.Join(r.Argv, " "); strings.Contains(s, "sk-engine") || !strings.Contains(s, "--port 1") {
		t.Fatalf("argv %q", s)
	}
	if r.Env["LLAMA_API_KEY"] != "***" || r.Env["HF_TOKEN"] != "***" || r.Env["CUDA_VISIBLE_DEVICES"] != "0" {
		t.Fatalf("env %v", r.Env)
	}
	if all := p.Redacted(true); all.Env["CUDA_VISIBLE_DEVICES"] != "***" {
		t.Fatalf("for an agent every value is masked: %v", all.Env)
	}
	if p.Env["HF_TOKEN"] != "hf_abcdefgh" {
		t.Fatal("the plan itself was changed: the launch needs the real values")
	}
}

// The gateway's monitor restarts a model whose engine died. An engine that died because somebody stopped it
// (emx stop from another process, which removes the record when it is done) is not such a case: NoteExit
// says so, having waited for that stop to finish.
func TestNoteExitTellsACrashFromAStop(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	dead := &Instance{ID: "m.e", Name: "m", Engine: "e", PID: 1 << 30, StartTick: 1, StartedAt: time.Now(), Log: filepath.Join(t.TempDir(), "x.log")}
	if err := dead.save(); err != nil {
		t.Fatal(err)
	}
	seen := *dead // what a monitor read before looking closer
	if !seen.NoteExit() {
		t.Fatal("a crashed engine's exit was not noted")
	}
	again := *dead
	if again.NoteExit() {
		t.Fatal("the same exit was noted twice")
	}
	stopped := &Instance{ID: "n.e", Name: "n", Engine: "e", PID: 1 << 30, StartTick: 1, StartedAt: time.Now()}
	if err := stopped.save(); err != nil {
		t.Fatal(err)
	}
	seen = *stopped
	os.Remove(stopped.path()) // the stop finished and removed the record
	if seen.NoteExit() {
		t.Fatal("an engine that was stopped on purpose was reported as an exit to react to")
	}
}

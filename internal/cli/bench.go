package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lotto/substrate/internal/app"
	"github.com/lotto/substrate/internal/bench"
	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/gateway"
	"github.com/lotto/substrate/internal/paths"
	"github.com/lotto/substrate/internal/runtime"
	"github.com/lotto/substrate/internal/store"
)

func cmdBench() *command {
	return &command{name: "bench", summary: "benchmark served models and publish verified runs to LocalMaxxing (via lmx)",
		sub: []*command{benchRunCmd(), benchConfigCmd(), benchListCmd(), benchShowCmd(), benchSubmitCmd()}}
}

// benchDir holds hardware descriptions and LocalMaxxing payloads.
func benchDir() string { return filepath.Join(paths.State(), "benchmarks") }

func benchModel(a *app.App, ref string) (config.Model, error) {
	name := ref
	if target, ok := a.Config().Alias(ref); ok {
		name = target
	}
	m, ok := a.RegisteredModel(name)
	if !ok {
		return m, errs.New(errs.NotFound, "no registered model %q (substrate model add)", ref)
	}
	return m, nil
}

// runningInstance is the model's ready instance, or nil.
func runningInstance(name string) *runtime.Instance {
	ins, _ := runtime.List()
	for _, in := range ins {
		if in.Name == name && in.Alive() && in.ReadyAt != nil {
			return in
		}
	}
	return nil
}

func parseLevels(s string) ([]int, error) {
	var out []int
	for _, f := range splitList(s) {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 {
			return nil, errs.New(errs.Usage, "--concurrency %q: want positive integers, e.g. 1,4,16", s)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		out = []int{1}
	}
	return out, nil
}

func f64(m map[string]any, k string) *float64 {
	if v, ok := m[k].(float64); ok {
		return &v
	}
	return nil
}

func i64(m map[string]any, k string) *int64 {
	if v, ok := m[k].(float64); ok {
		n := int64(v)
		return &n
	}
	return nil
}

func benchRunCmd() *command {
	return &command{name: "run", synopsis: "<model> [--concurrency 1,4,16] [--canonical-prompt reasoning-v1 | --prompt-file F] [--submit]",
		summary: "measure a model through its substrate instance with lmx; --submit uploads to LocalMaxxing", tool: true,
		build: func(fs *flag.FlagSet) action {
			levels := fs.String("concurrency", "1", "comma-separated concurrent-request levels; one run each (verified runs need 1)")
			canonical := fs.String("canonical-prompt", "reasoning-v1", "LocalMaxxing canonical prompt id (lmx context get canonicalPrompts)")
			promptFile := fs.String("prompt-file", "", "measure on this prompt instead (never verified)")
			maxTokens := fs.Int("max-tokens", 1024, "output tokens per request (canonical prompts need at least their minimum, 256 for reasoning-v1)")
			iterations := fs.Int("iterations", 3, "timed iterations per level; the median is reported")
			warmup := fs.Int("warmup", 1, "untimed warmup requests per level")
			via := fs.String("via", "direct", "direct (the engine's own endpoint) or gateway (through substrate serve: metered, policies and caps apply)")
			notes := fs.String("notes", "", "added to the submission notes")
			validate := fs.Bool("validate", false, "ask LocalMaxxing to check each run (no upload)")
			submit := fs.Bool("submit", false, "validate, then upload each run")
			force := fs.Bool("force", false, "upload even when LocalMaxxing would not mark the run verified, or metadata is incomplete")
			timeout := fs.Duration("start-timeout", 30*time.Minute, "how long to wait for the model to start")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("bench run", pos, 1, 1); err != nil {
					return err
				}
				lv, err := parseLevels(*levels)
				if err != nil {
					return err
				}
				if *via != "direct" && *via != "gateway" {
					return errs.New(errs.Usage, "bench run: --via %q (want direct or gateway)", *via)
				}
				lmx, err := bench.FindLMX()
				if err != nil {
					return errs.Wrap(errs.NotFound, err)
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				m, err := benchModel(a, pos[0])
				if err != nil {
					return err
				}
				e.progress("ensuring %s is ready...", m.Name)
				res, err := a.Ensure(e.ctx, m.Name, app.LaunchOpts{Owner: e.owner}, *timeout)
				if err != nil {
					return err
				}
				in := res.Instance
				meta := bench.Resolve(a, m, in)
				if (meta.HFID == "" || meta.Quantization == "") || (len(meta.Problems) > 0 && (*submit && !*force)) {
					return errs.New(errs.Usage, "bench run %s: %s", m.Name, strings.Join(meta.Problems, "; "))
				}
				for _, p := range meta.Problems {
					e.warn("%s", p)
				}
				warnBusy(e, a, in)

				run := bench.Run{CanonicalPrompt: *canonical, PromptFile: *promptFile, MaxTokens: *maxTokens,
					Iterations: *iterations, Warmup: *warmup, BaseURL: in.BaseURL, ServedModel: res.Model, APIKey: in.APIKey,
					RunsDir: filepath.Join(benchDir(), "lmx-runs")}
				if *via == "gateway" {
					run.BaseURL, _ = gateway.Client(&a.Config().Gateway)
					run.ServedModel, run.APIKey = m.Name, os.Getenv("SUBSTRATE_API_KEY")
				}
				parts := []string{fmt.Sprintf("Served by substrate %s (instance %s, measured %s).", Version, in.ID, map[string]string{
					"direct": "on the engine's endpoint", "gateway": "through the substrate gateway"}[*via])}
				for _, n := range []string{*notes, meta.Notes} {
					if n = strings.TrimSpace(n); n != "" {
						parts = append([]string{n}, parts...)
					}
				}
				run.Notes = strings.Join(parts, " ")
				prompt := "canonical:" + *canonical
				if *promptFile != "" {
					prompt = "file:" + *promptFile
				}

				if err := os.MkdirAll(benchDir(), 0o700); err != nil {
					return err
				}
				db, err := a.DB()
				if err != nil {
					return err
				}
				var results []store.Benchmark
				for _, c := range lv {
					id := fmt.Sprintf("bench_%s_%s_c%d", time.Now().Format("20060102-150405"), m.Name, c)
					hwFile := filepath.Join(benchDir(), id+".hardware.json")
					data, _ := json.MarshalIndent(meta.Hardware, "", "  ")
					if err := os.WriteFile(hwFile, data, 0o600); err != nil {
						return err
					}
					run.Concurrency, run.HardwareFile, run.Out = c, hwFile, filepath.Join(benchDir(), id+".json")
					rec := store.Benchmark{ID: id, TS: time.Now().UnixMilli(), Model: m.Name, Engine: in.Engine, Instance: in.ID,
						Via: *via, Concurrency: c, Prompt: prompt, Payload: run.Out, Status: "measured"}
					e.progress("measuring %s at concurrency %d (%d iterations)...", m.Name, c, *iterations)
					payload, err := lmx.Measure(e.ctx, meta, run)
					if err != nil {
						rec.Status, rec.Error = "failed", err.Error()
					} else {
						rec.TokSOut, rec.TokSPrefill, rec.TokSTotal, rec.TTFTMS = f64(payload, "tokSOut"), f64(payload, "tokSPrefill"),
							f64(payload, "tokSTotal"), f64(payload, "ttftMs")
						rec.PromptTokens, rec.OutputTokens = i64(payload, "promptTokens"), i64(payload, "outputTokens")
						if *validate || *submit {
							publish(e, a, lmx, &rec, *submit, *force)
						}
					}
					if err := db.SaveBenchmark(&rec); err != nil {
						return err
					}
					results = append(results, rec)
				}
				return e.out(map[string]any{"meta": meta, "runs": results}, func(w io.Writer) { printBenchmarks(e, results) })
			}
		}}
}

// publish validates rec's payload with LocalMaxxing and, when asked, uploads it (only verified runs unless force).
// A payload lmx already recorded as submitted is adopted rather than uploaded again.
func publish(e *env, a *app.App, lmx *bench.LMX, rec *store.Benchmark, submit, force bool) {
	if id, status := bench.Receipt(rec.Payload); id != "" && submit {
		rec.Status, rec.SubmissionID = "submitted", id
		e.recordAdmin(fmt.Sprintf("bench submit %s: already uploaded as LocalMaxxing run %s (%s); recorded", rec.ID, id, status))
		return
	}
	v, err := lmx.Validate(e.ctx, rec.Payload)
	if err != nil {
		rec.Error = err.Error()
		return
	}
	rec.Status, rec.Verified, rec.Issues = "validated", &v.Verified, strings.Join(v.Issues, "; ")
	if !submit {
		return
	}
	if !v.Verified && !force {
		rec.Error = "not uploaded: LocalMaxxing would not mark it verified (" + rec.Issues + "); --force uploads anyway"
		return
	}
	s, err := lmx.Submit(e.ctx, rec.Payload)
	if err != nil {
		rec.Error = err.Error()
		return
	}
	rec.Status, rec.SubmissionID, rec.Verified, rec.Issues = "submitted", s.SubmissionID, &s.Verified, strings.Join(s.Issues, "; ")
	e.recordAdmin(fmt.Sprintf("bench submit %s: LocalMaxxing run %s (verified %v)", rec.ID, s.SubmissionID, s.Verified))
}

// warnBusy notes other traffic on the instance (through the gateway), which would skew the measurement.
func warnBusy(e *env, a *app.App, in *runtime.Instance) {
	base, client := gateway.Client(&a.Config().Gateway)
	client.Timeout = 2 * time.Second
	req, _ := http.NewRequestWithContext(e.ctx, http.MethodGet, base+"/substrate/v1/instances", nil)
	if key := os.Getenv("SUBSTRATE_API_KEY"); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var list []struct {
		Instance struct {
			ID string `json:"id"`
		} `json:"instance"`
		Active int `json:"active_requests"`
	}
	if json.NewDecoder(resp.Body).Decode(&list) != nil {
		return
	}
	for _, x := range list {
		if x.Instance.ID == in.ID && x.Active > 0 {
			e.warn("%s is serving %d gateway request(s) right now: they share the engine with the benchmark", in.ID, x.Active)
		}
	}
}

func printBenchmarks(e *env, rs []store.Benchmark) {
	if len(rs) == 0 {
		fmt.Fprintln(e.stdout, "no benchmark runs (substrate bench run <model>)")
		return
	}
	rows := make([][]string, len(rs))
	for i, r := range rs {
		verified := "-"
		if r.Verified != nil {
			verified = yesno(*r.Verified)
		}
		note := r.Error
		if note == "" {
			note = r.Issues
		}
		rows[i] = []string{r.ID, time.UnixMilli(r.TS).Format("2006-01-02 15:04"), r.Model, r.Engine, fmt.Sprint(r.Concurrency),
			optf(r.TokSOut, ""), optf(r.TokSPrefill, ""), optf(r.TTFTMS, "ms"), opti(r.OutputTokens), r.Status, verified,
			dash(r.SubmissionID), dash(oneLine(note, 70))}
	}
	e.table("ID\tTIME\tMODEL\tENGINE\tCONC\tTOK/S OUT\tTOK/S PREFILL\tTTFT\tOUT TOKENS\tSTATUS\tVERIFIED\tSUBMISSION\tNOTE", rows)
}

func benchListCmd() *command {
	return &command{name: "list", synopsis: "[--model M] [--limit N]", summary: "recorded benchmark runs", tool: true,
		build: func(fs *flag.FlagSet) action {
			model := fs.String("model", "", "only this model")
			limit := fs.Int("limit", 50, "max rows (0 = all)")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("bench list", pos, 0, 0); err != nil {
					return err
				}
				db, err := e.db()
				if err != nil {
					return err
				}
				rs, err := db.Benchmarks(*model, *limit)
				if err != nil {
					return err
				}
				if rs == nil {
					rs = []store.Benchmark{}
				}
				return e.out(rs, func(w io.Writer) { printBenchmarks(e, rs) })
			}
		}}
}

func benchShowCmd() *command {
	return &command{name: "show", synopsis: "<id>", summary: "one benchmark run with its LocalMaxxing payload", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("bench show", pos, 1, 1); err != nil {
					return err
				}
				db, err := e.db()
				if err != nil {
					return err
				}
				r, ok, err := db.Benchmark(pos[0])
				if err != nil {
					return err
				}
				if !ok {
					return errs.New(errs.NotFound, "no benchmark %q (substrate bench list)", pos[0])
				}
				var payload any
				if data, err := os.ReadFile(r.Payload); err == nil {
					json.Unmarshal(data, &payload)
				}
				return e.out(map[string]any{"run": r, "payload": payload}, func(w io.Writer) {
					data, _ := json.MarshalIndent(r, "", "  ")
					fmt.Fprintf(w, "%s\n\npayload: %s\n", data, r.Payload)
				})
			}
		}}
}

func benchSubmitCmd() *command {
	return &command{name: "submit", synopsis: "<id>... [--force]", summary: "validate recorded runs with LocalMaxxing and upload them",
		build: func(fs *flag.FlagSet) action {
			force := fs.Bool("force", false, "upload runs LocalMaxxing would not mark verified")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("bench submit", pos, 1, -1); err != nil {
					return err
				}
				lmx, err := bench.FindLMX()
				if err != nil {
					return errs.Wrap(errs.NotFound, err)
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				db, err := a.DB()
				if err != nil {
					return err
				}
				var out []store.Benchmark
				for _, id := range pos {
					r, ok, err := db.Benchmark(id)
					if err != nil {
						return err
					}
					if !ok {
						return errs.New(errs.NotFound, "no benchmark %q", id)
					}
					if r.SubmissionID != "" {
						return errs.New(errs.Conflict, "%s was already uploaded as LocalMaxxing run %s", id, r.SubmissionID)
					}
					if r.Status == "failed" {
						return errs.New(errs.Usage, "%s failed to measure: %s", id, r.Error)
					}
					r.Error = ""
					publish(e, a, lmx, &r, true, *force)
					if err := db.SaveBenchmark(&r); err != nil {
						return err
					}
					out = append(out, r)
				}
				return e.out(out, func(w io.Writer) { printBenchmarks(e, out) })
			}
		}}
}

func benchConfigCmd() *command {
	return &command{name: "config", synopsis: "<model> [--hf-id org/repo] [--quantization Q] [--spec-method MTP --spec-num-tokens 3] [...]",
		summary: "set or show what submissions say about a model (unset values are detected)", tool: true,
		build: func(fs *flag.FlagSet) action {
			s := map[string]*string{}
			for _, f := range []struct{ name, help string }{
				{"hf-id", "Hugging Face repo of the weights"}, {"revision", "model revision"},
				{"quantization", "quantization label (Q4_K_M, IQ3_S, FP8, ...)"},
				{"engine-name", "LocalMaxxing engine name (llama.cpp, vllm, sglang, strata, custom, ...)"},
				{"engine-version", "engine release"}, {"engine-repository", "engine source repository URL"},
				{"engine-commit", "engine git commit"}, {"backend", "cuda, rocm, tt-metal, cpu, ..."},
				{"spec-method", `speculative decoding method (MTP, EAGLE, DFlash, ngram, ...; "-" clears)`},
				{"spec-draft-model", "draft model id"}, {"hardware", "JSON file replacing the detected hardware"},
				{"notes", "notes added to every submission"},
			} {
				s[f.name] = fs.String(f.name, "", f.help)
			}
			specTokens := fs.Int("spec-num-tokens", -1, "draft tokens proposed per verification step")
			ctxLen := fs.Int("context-length", -1, "context length the engine serves")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("bench config", pos, 1, 1); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				m, err := benchModel(a, pos[0])
				if err != nil {
					return err
				}
				changed := *specTokens >= 0 || *ctxLen >= 0
				for _, v := range s {
					changed = changed || *v != ""
				}
				if changed {
					if p := *s["hardware"]; p != "" {
						abs, err := filepath.Abs(p)
						if err != nil {
							return err
						}
						*s["hardware"] = abs
					}
					err := config.UpdateModels(func(ms *config.Models) error {
						x, ok := ms.Get(m.Name)
						if !ok {
							return errs.New(errs.NotFound, "no registered model %q", m.Name)
						}
						if x.Bench == nil {
							x.Bench = &config.BenchMeta{}
						}
						b := x.Bench
						for name, dst := range map[string]*string{"hf-id": &b.HFID, "revision": &b.Revision, "quantization": &b.Quantization,
							"engine-name": &b.EngineName, "engine-version": &b.EngineVersion, "engine-repository": &b.EngineRepository,
							"engine-commit": &b.EngineCommit, "backend": &b.Backend, "hardware": &b.Hardware, "notes": &b.Notes} {
							switch v := *s[name]; v {
							case "":
							case "-":
								*dst = ""
							default:
								*dst = v
							}
						}
						switch method := *s["spec-method"]; {
						case method == "-":
							b.Spec = nil
						case method != "" || *specTokens >= 0 || *s["spec-draft-model"] != "":
							if b.Spec == nil {
								b.Spec = &config.BenchSpec{}
							}
							if method != "" {
								b.Spec.Method = method
							}
							if *specTokens >= 0 {
								b.Spec.NumTokens = *specTokens
							}
							if d := *s["spec-draft-model"]; d != "" {
								b.Spec.DraftModel = d
							}
						}
						if *ctxLen >= 0 {
							b.ContextLength = *ctxLen
						}
						ms.Put(*x)
						return nil
					})
					if err != nil {
						return err
					}
					if err := a.Refresh(); err != nil {
						return err
					}
					m, _ = benchModel(a, pos[0])
					data, _ := json.Marshal(m.Bench)
					e.recordAdmin(fmt.Sprintf("bench config %s: %s", m.Name, data))
				}
				in := runningInstance(m.Name)
				detected := in != nil
				if in == nil {
					in = &runtime.Instance{Engine: m.Engine}
				}
				meta := bench.Resolve(a, m, in)
				return e.out(map[string]any{"meta": meta, "instance_running": detected}, func(w io.Writer) {
					rows := [][]string{}
					add := func(field, value string) {
						src := meta.Sources[field]
						if value == "" {
							value, src = "-", "-"
						}
						rows = append(rows, []string{field, value, src})
					}
					add("hf_id", meta.HFID)
					add("quantization", meta.Quantization)
					add("engine_name", meta.EngineName)
					add("engine_version", meta.EngineVersion)
					add("engine_repository", meta.EngineRepository)
					add("engine_commit", meta.EngineCommit)
					add("backend", meta.Backend)
					if meta.ContextLength > 0 {
						add("context_length", fmt.Sprint(meta.ContextLength))
					}
					if meta.Spec != nil {
						rows = append(rows, []string{"spec", fmt.Sprintf("%s, %d tokens, draft %s", meta.Spec.Method, meta.Spec.NumTokens, dash(meta.Spec.DraftModel)), "config"})
					}
					hw, _ := json.Marshal(meta.Hardware)
					add("hardware", string(hw))
					e.table("FIELD\tVALUE\tSOURCE", rows)
					if !detected {
						fmt.Fprintf(w, "\n%s is not running: backend, version and hardware are detected when bench run starts it.\n", m.Name)
					}
					for _, p := range meta.Problems {
						fmt.Fprintf(w, "problem: %s\n", p)
					}
				})
			}
		}}
}

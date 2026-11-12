package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/LottoLottoLotto/substrate/internal/config"
	"github.com/LottoLottoLotto/substrate/internal/errs"
	"github.com/LottoLottoLotto/substrate/internal/gateway"
	"github.com/LottoLottoLotto/substrate/internal/gpu"
	"github.com/LottoLottoLotto/substrate/internal/keys"
	"github.com/LottoLottoLotto/substrate/internal/model"
	"github.com/LottoLottoLotto/substrate/internal/paths"
	"github.com/LottoLottoLotto/substrate/internal/route"
	"github.com/LottoLottoLotto/substrate/internal/runtime"
	"github.com/LottoLottoLotto/substrate/internal/settings"
	"github.com/LottoLottoLotto/substrate/internal/spec"
	"github.com/LottoLottoLotto/substrate/internal/store"
	"github.com/LottoLottoLotto/substrate/internal/tenstorrent"
)

// ---- model ----

func cmdModel() *command {
	return &command{name: "model", summary: "manage the model registry",
		sub: []*command{modelListCmd(), modelAddCmd(), modelRmCmd(), modelInspectCmd()}}
}

func modelListCmd() *command {
	return &command{name: "list", summary: "list registered models", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("model list", pos, 0, 0); err != nil {
					return err
				}
				ms, err := config.LoadModels()
				if err != nil {
					return err
				}
				return e.out(ms.Models, func(w io.Writer) {
					if len(ms.Models) == 0 {
						fmt.Fprintln(w, "no models (substrate model add <name> <path|org/repo>)")
						return
					}
					rows := make([][]string, len(ms.Models))
					for i, m := range ms.Models {
						rows[i] = []string{m.Name, m.Format, dash(m.Arch), dash(m.Quant), dash(m.Engine), m.Source}
					}
					e.table("NAME\tFORMAT\tARCH\tQUANT\tENGINE\tSOURCE", rows)
				})
			}
		}}
}

func modelAddCmd() *command {
	return &command{name: "add", synopsis: "<name> <path|org/repo> [--engine ID] [unified flags] [-- engine args]",
		summary: "register a model (inspects format and quantization)", tool: true, passthrough: true,
		build: func(fs *flag.FlagSet) action {
			engine := fs.String("engine", "", "pin the model to this engine")
			force := fs.Bool("force", false, "replace an existing entry")
			vram := fs.Int64("vram-mib", 0, "declare the model's total GPU memory need (overrides estimates and measurements)")
			idle := fs.Duration("idle-unload", 0, "unload after this long without requests (overrides gateway.idle_unload)")
			maxConc := fs.Int("max-concurrency", 0, "in-flight request limit for this model (overrides gateway.max_concurrency)")
			devices := fs.Int("devices", 0, "accelerator chips one instance owns (Tenstorrent; overrides the engine's default)")
			var vars, preArgs multi
			fs.Var(&vars, "var", "manifest placeholder for this model, KEY=VALUE (repeatable; e.g. image=lfm25-p150:dev-m12)")
			fs.Var(&preArgs, "pre-arg", "argument inserted at the engine's {pre_args} slot (repeatable; needs --engine; e.g. -e X=1)")
			flags := map[string]string{}
			addUnified(fs, flags)
			return func(e *env, pos, extra []string) error {
				if err := wantArgs("model add", pos, 2, 2); err != nil {
					return err
				}
				name, source := pos[0], pos[1]
				if len(preArgs) > 0 && *engine == "" {
					return errs.New(errs.Usage, "model add: --pre-arg needs --engine (pre-args are engine-specific)")
				}
				vm, err := kv(vars, "--var")
				if err != nil {
					return err
				}
				if len(extra) > 0 && *engine == "" {
					return errs.New(errs.Usage, "model add: engine args after -- need --engine (they are engine-specific)")
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				added, res, err := settings.AddModel(a, settings.ModelSpec{Name: name, Source: source, Engine: *engine, Force: *force,
					Flags: flags, VRAMMiB: *vram, IdleUnload: *idle, MaxConcurrency: *maxConc, Devices: *devices,
					Vars: vm, PreArgs: preArgs, Args: extra})
				if err != nil {
					return err
				}
				e.applied(res)
				m := added.Model
				return e.out(added, func(w io.Writer) {
					fmt.Fprintf(w, "added %s (%s %s %s)", name, m.Format, dash(m.Arch), dash(m.Quant))
					if added.Engine != "" {
						fmt.Fprintf(w, " -> %s", added.Engine)
					}
					fmt.Fprintln(w)
				})
			}
		}}
}

func modelRmCmd() *command {
	return &command{name: "rm", synopsis: "<name>...", summary: "remove models from the registry", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("model rm", pos, 1, -1); err != nil {
					return err
				}
				res, err := settings.RemoveModels(pos)
				if err != nil {
					return err
				}
				e.applied(res)
				return e.out(map[string]any{"removed": pos}, func(w io.Writer) { fmt.Fprintf(w, "removed %s\n", strings.Join(pos, ", ")) })
			}
		}}
}

func modelInspectCmd() *command {
	return &command{name: "inspect", synopsis: "<name|path|org/repo>", summary: "show a model's format, quantization and engine ranking", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("model inspect", pos, 1, 1); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				t, err := a.ResolveModel(pos[0])
				if err != nil {
					return err
				}
				cands := a.Route(t)
				return e.out(map[string]any{"info": t.Info, "registered": t.Registered, "route": cands}, func(w io.Writer) {
					i := t.Info
					fmt.Fprintf(w, "source:  %s\nformat:  %s\narch:    %s\nquant:   %s\nsize:    %s\n", i.Source, i.Format, dash(i.Arch), dash(i.Quant), human(i.Bytes))
					if i.Path != "" {
						fmt.Fprintf(w, "path:    %s\n", i.Path)
					}
					fmt.Fprintln(w)
					printRoute(e, cands)
				})
			}
		}}
}

// ---- route ----

func cmdRoute() *command {
	return &command{name: "route", synopsis: "<model|path|org/repo>", summary: "rank the engines able to serve a model", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("route", pos, 1, 1); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				t, err := a.ResolveModel(pos[0])
				if err != nil {
					return err
				}
				cands := a.Route(t)
				pick, perr := route.Pick(cands)
				res := map[string]any{"model": t.Model.Name, "format": t.Info.Format, "quant": t.Info.Quant, "engine": pick, "candidates": cands}
				if perr != nil {
					res["error"] = perr.Error()
				}
				if err := e.out(res, func(w io.Writer) { printRoute(e, cands) }); err != nil {
					return err
				}
				return perr
			}
		}}
}

func printRoute(e *env, cands []route.Candidate) {
	rows := make([][]string, len(cands))
	for i, c := range cands {
		mark := ""
		if i == 0 && c.Compatible && c.Installed {
			mark = "*"
		}
		rows[i] = []string{mark + c.Engine, yesno(c.Compatible), yesno(c.Installed), fmt.Sprint(c.Score), strings.Join(c.Reasons, "; ")}
	}
	e.table("ENGINE\tCOMPATIBLE\tINSTALLED\tSCORE\tNOTES", rows)
}

// ---- engine ----

func cmdEngine() *command {
	return &command{name: "engine", summary: "manage engines",
		sub: []*command{engineListCmd(), engineShowCmd(), engineInstallCmd(), engineValidateCmd(), engineAddCmd(), engineSetCmd()}}
}

type engineRow struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Installed bool           `json:"installed"`
	Bin       string         `json:"bin,omitempty"`
	Version   string         `json:"version,omitempty"`
	Source    string         `json:"source"`
	Formats   []string       `json:"formats"`
	APIs      []string       `json:"apis"`
	Features  []string       `json:"features,omitempty"`
	Tried     []string       `json:"tried,omitempty"`
	Manifest  *spec.Manifest `json:"manifest,omitempty"`
}

func engineListCmd() *command {
	return &command{name: "list", synopsis: "[--versions]", summary: "list engines and whether they are installed", tool: true,
		build: func(fs *flag.FlagSet) action {
			versions := fs.Bool("versions", false, "run each engine's version command (slower)")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("engine list", pos, 0, 0); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				var rows []engineRow
				for _, m := range a.Engines.All() {
					en, _ := a.Engine(m.ID)
					r := engineRow{ID: m.ID, Name: m.Name, Installed: en.Installed(), Bin: en.Bin, Source: m.Source,
						Formats: m.Capabilities.Formats, APIs: m.Capabilities.APIs, Features: m.Capabilities.Features}
					if *versions {
						r.Version = en.Version()
					}
					rows = append(rows, r)
				}
				return e.out(rows, func(w io.Writer) {
					t := make([][]string, len(rows))
					for i, r := range rows {
						t[i] = []string{r.ID, yesno(r.Installed), strings.Join(r.Formats, ","), dash(r.Version), dash(r.Bin)}
					}
					e.table("ENGINE\tINSTALLED\tFORMATS\tVERSION\tBINARY", t)
				})
			}
		}}
}

func engineShowCmd() *command {
	return &command{name: "show", synopsis: "<id> [--raw]", summary: "show an engine's manifest and install state", tool: true,
		build: func(fs *flag.FlagSet) action {
			raw := fs.Bool("raw", false, "print the manifest YAML")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("engine show", pos, 1, 1); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				en, err := a.Engine(pos[0])
				if err != nil {
					return err
				}
				if *raw {
					var data []byte
					if en.Source == "builtin" {
						data, _ = spec.BuiltinYAML(en.ID)
					} else if data, err = os.ReadFile(en.Source); err != nil {
						return err
					}
					if e.json {
						return e.out(map[string]string{"id": en.ID, "source": en.Source, "yaml": string(data)}, nil)
					}
					_, err := e.stdout.Write(data)
					return err
				}
				r := engineRow{ID: en.ID, Name: en.Name, Installed: en.Installed(), Bin: en.Bin, Version: en.Version(),
					Source: en.Source, Formats: en.Capabilities.Formats, APIs: en.Capabilities.APIs,
					Features: en.Capabilities.Features, Tried: en.Tried, Manifest: en.Manifest}
				return e.out(r, func(w io.Writer) {
					fmt.Fprintf(w, "%s: %s\n", en.ID, en.Name)
					if en.Description != "" {
						fmt.Fprintf(w, "  %s\n", en.Description)
					}
					fmt.Fprintf(w, "manifest:  %s\nhomepage:  %s\ninstalled: %s\n", en.Source, dash(en.Homepage), yesno(r.Installed))
					if r.Installed {
						fmt.Fprintf(w, "binary:    %s\nversion:   %s\n", en.Bin, dash(r.Version))
					} else {
						fmt.Fprintf(w, "tried:     %s\n", strings.Join(en.Tried, ", "))
					}
					fmt.Fprintf(w, "formats:   %s\nquants:    %s\napis:      %s\nfeatures:  %s\n",
						strings.Join(en.Capabilities.Formats, ", "), strings.Join(en.Capabilities.Quantizations, ", "),
						strings.Join(en.Capabilities.APIs, ", "), strings.Join(en.Capabilities.Features, ", "))
					var fl []string
					for _, f := range spec.Unified {
						if _, ok := en.Flags[f.Key]; ok {
							fl = append(fl, "--"+f.CLI)
						}
					}
					fmt.Fprintf(w, "flags:     %s\nlaunch:    %s\n", strings.Join(fl, " "), strings.Join(en.Launch.Command, " "))
				})
			}
		}}
}

func engineInstallCmd() *command {
	return &command{name: "install", synopsis: "<id> [--dry-run]", summary: "run an engine's install steps",
		build: func(fs *flag.FlagSet) action {
			dry := fs.Bool("dry-run", false, "print the steps without running them")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("engine install", pos, 1, 1); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				en, err := a.Engine(pos[0])
				if err != nil {
					return err
				}
				if len(en.Install.Steps) == 0 {
					return errs.New(errs.Unsupported, "%s declares no install steps; see %s", en.ID, dash(en.Homepage))
				}
				steps, err := spec.ExpandAll(en.Install.Steps, map[string]string{"home": paths.Home(), "data": paths.Data(), "state": paths.State()})
				if err != nil {
					return err
				}
				if *dry {
					return e.out(map[string]any{"engine": en.ID, "notes": en.Install.Notes, "steps": steps}, func(w io.Writer) {
						if en.Install.Notes != "" {
							fmt.Fprintf(w, "# %s\n", en.Install.Notes)
						}
						for _, s := range steps {
							fmt.Fprintln(w, s)
						}
					})
				}
				if err := os.MkdirAll(paths.Data(), 0o755); err != nil {
					return err
				}
				for i, s := range steps {
					fmt.Fprintf(e.stderr, "[%d/%d] %s\n", i+1, len(steps), s)
					cmd := exec.CommandContext(e.ctx, "sh", "-c", s)
					cmd.Dir = paths.Data()
					cmd.Stdin, cmd.Stdout, cmd.Stderr = e.stdin, e.stderr, e.stderr
					if err := cmd.Run(); err != nil {
						return errs.New(errs.EngineFailed, "install step %d failed: %w", i+1, err)
					}
				}
				re, err := a.Reresolve(en.ID)
				if err != nil {
					return err
				}
				if !re.Installed() {
					return errs.New(errs.EngineFailed, "install steps finished, but no binary resolves (tried: %s)", strings.Join(re.Tried, ", "))
				}
				return e.out(map[string]any{"engine": en.ID, "bin": re.Bin}, func(w io.Writer) { fmt.Fprintf(w, "%s installed: %s\n", en.ID, re.Bin) })
			}
		}}
}

func engineValidateCmd() *command {
	return &command{name: "validate", synopsis: "<manifest.yaml>...", summary: "check manifests against the engine spec", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("engine validate", pos, 1, -1); err != nil {
					return err
				}
				type result struct {
					File  string `json:"file"`
					ID    string `json:"id,omitempty"`
					Valid bool   `json:"valid"`
					Error string `json:"error,omitempty"`
				}
				var res []result
				failed := false
				for _, f := range pos {
					data, err := os.ReadFile(f)
					r := result{File: f}
					if err == nil {
						var m *spec.Manifest
						if m, err = spec.Parse(data, f); err == nil {
							r.ID, r.Valid = m.ID, true
						}
					}
					if err != nil {
						r.Error, failed = err.Error(), true
					}
					res = append(res, r)
				}
				if err := e.out(res, func(w io.Writer) {
					for _, r := range res {
						if r.Valid {
							fmt.Fprintf(w, "%s: valid (%s, %s)\n", r.File, r.ID, spec.Version)
						} else {
							fmt.Fprintln(w, r.Error)
						}
					}
				}); err != nil {
					return err
				}
				if failed {
					return errs.New(errs.Usage, "invalid manifest")
				}
				return nil
			}
		}}
}

func engineAddCmd() *command {
	return &command{name: "add", synopsis: "<manifest.yaml> [--force]", summary: "install a manifest into the user engines dir", tool: true,
		build: func(fs *flag.FlagSet) action {
			force := fs.Bool("force", false, "replace an existing user manifest with the same id")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("engine add", pos, 1, 1); err != nil {
					return err
				}
				data, err := os.ReadFile(pos[0])
				if err != nil {
					return errs.Wrap(errs.NotFound, err)
				}
				m, err := spec.Parse(data, pos[0])
				if err != nil {
					return errs.Wrap(errs.Usage, err)
				}
				dst := filepath.Join(config.EnginesDir(), m.ID+".yaml")
				if _, err := os.Stat(dst); err == nil && !*force {
					return errs.New(errs.Conflict, "%s exists (use --force)", dst)
				}
				if err := os.MkdirAll(config.EnginesDir(), 0o755); err != nil {
					return err
				}
				tmp := dst + ".tmp"
				if err := os.WriteFile(tmp, data, 0o644); err != nil {
					return err
				}
				if err := os.Rename(tmp, dst); err != nil {
					return err
				}
				_, builtin := spec.BuiltinYAML(m.ID)
				return e.out(map[string]any{"engine": m.ID, "path": dst, "overrides_builtin": builtin}, func(w io.Writer) {
					fmt.Fprintf(w, "added %s -> %s", m.ID, dst)
					if builtin {
						fmt.Fprint(w, " (replaces the builtin manifest)")
					}
					fmt.Fprintln(w)
				})
			}
		}}
}

func engineSetCmd() *command {
	return &command{name: "set", synopsis: "<id> [--bin PATH] [--var K=V]... [--env K=V]... [--arg A]... [--clear]",
		summary: "set local overrides for an engine (binary, vars, env, args)", tool: true,
		build: func(fs *flag.FlagSet) action {
			bin := fs.String("bin", "", "entry point binary (skips discovery)")
			var vars, envs, eargs multi
			fs.Var(&vars, "var", "override a manifest var (repeatable)")
			fs.Var(&envs, "env", "extra environment for launches (repeatable)")
			fs.Var(&eargs, "arg", "extra argument appended to every launch (repeatable)")
			clear := fs.Bool("clear", false, "remove all overrides for the engine first")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("engine set", pos, 1, 1); err != nil {
					return err
				}
				id := pos[0]
				a, err := e.app()
				if err != nil {
					return err
				}
				if _, ok := a.Engines.Get(id); !ok {
					return errs.New(errs.NotFound, "unknown engine %q", id)
				}
				vm, err := kv(vars, "--var")
				if err != nil {
					return err
				}
				em, err := kv(envs, "--env")
				if err != nil {
					return err
				}
				var ov config.EngineOverride
				_, err = config.Update(func(c *config.Config) error {
					if c.Engines == nil {
						c.Engines = map[string]config.EngineOverride{}
					}
					ov = c.Engines[id]
					if *clear {
						ov = config.EngineOverride{}
					}
					if *bin != "" {
						ov.Bin = paths.Expand(*bin)
					}
					for k, v := range vm {
						if ov.Vars == nil {
							ov.Vars = map[string]string{}
						}
						ov.Vars[k] = v
					}
					for k, v := range em {
						if ov.Env == nil {
							ov.Env = map[string]string{}
						}
						ov.Env[k] = v
					}
					ov.Args = append(ov.Args, eargs...)
					if ov.Bin == "" && len(ov.Vars) == 0 && len(ov.Env) == 0 && len(ov.Args) == 0 {
						delete(c.Engines, id)
					} else {
						c.Engines[id] = ov
					}
					return nil
				})
				if err != nil {
					return err
				}
				if err := a.Refresh(); err != nil {
					return err
				}
				re, err := a.Reresolve(id)
				if err != nil {
					return err
				}
				e.recordAdmin(fmt.Sprintf("engine set %s (bin %s)", id, dash(re.Bin)))
				return e.out(map[string]any{"engine": id, "override": ov, "installed": re.Installed(), "bin": re.Bin}, func(w io.Writer) {
					fmt.Fprintf(w, "%s: installed=%s %s\n", id, yesno(re.Installed()), re.Bin)
				})
			}
		}}
}

// ---- flags ----

func cmdFlags() *command {
	return &command{name: "flags", summary: "list unified flags and how each engine maps them", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("flags", pos, 0, 0); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				type row struct {
					spec.UnifiedFlag
					Engines map[string]spec.FlagMap `json:"engines"`
				}
				var rows []row
				ids := a.Engines.IDs()
				for _, f := range spec.Unified {
					r := row{UnifiedFlag: f, Engines: map[string]spec.FlagMap{}}
					for _, id := range ids {
						m, _ := a.Engines.Get(id)
						if fm, ok := m.Flags[f.Key]; ok {
							r.Engines[id] = fm
						}
					}
					rows = append(rows, r)
				}
				return e.out(rows, func(w io.Writer) {
					t := make([][]string, len(rows))
					for i, r := range rows {
						t[i] = []string{"--" + r.CLI, string(r.Type)}
						for _, id := range ids {
							fm, ok := r.Engines[id]
							switch {
							case !ok:
								t[i] = append(t[i], "-")
							case len(fm.Args) > 0:
								t[i] = append(t[i], strings.Join(fm.Args, " "))
							default:
								var env []string
								for _, k := range sortedKeys(fm.Env) {
									env = append(env, k+"="+fm.Env[k])
								}
								t[i] = append(t[i], strings.Join(env, " "))
							}
						}
					}
					e.table("FLAG\tTYPE\t"+strings.Join(ids, "\t"), t)
				})
			}
		}}
}

// ---- doctor ----

func cmdDoctor() *command {
	return &command{name: "doctor", summary: "check engines, GPUs, config, the usage database and security settings", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("doctor", pos, 0, 0); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				gs, gerr := gpu.List()
				var engines []engineRow
				for _, m := range a.Engines.All() {
					en, _ := a.Engine(m.ID)
					engines = append(engines, engineRow{ID: m.ID, Name: m.Name, Installed: en.Installed(), Bin: en.Bin,
						Version: en.Version(), Source: m.Source, Tried: en.Tried})
				}
				var manifestErrs []string
				for _, err := range a.Engines.Errors {
					manifestErrs = append(manifestErrs, err.Error())
				}
				dbStatus := "disabled"
				if a.Config().Logging.Enabled {
					if _, err := a.DB(); err != nil {
						dbStatus = "error: " + err.Error()
					} else {
						dbStatus = "ok"
					}
				}
				type missingModel struct {
					Name   string `json:"name"`
					Source string `json:"source"`
				}
				missing := []missingModel{}
				for _, m := range a.RegisteredModels() {
					if model.MissingLocal(m.Source) {
						missing = append(missing, missingModel{m.Name, m.Source})
					}
				}
				res := map[string]any{
					"version": Version, "spec": spec.Version,
					"paths": map[string]string{"config": config.ConfigPath(), "models": config.ModelsPath(), "engines": config.EnginesDir(),
						"state": paths.State(), "data": paths.Data(), "usage_db": store.Path()},
					"gpus": gs, "engines": engines, "manifest_errors": manifestErrs, "models": len(a.RegisteredModels()),
					"missing_models": missing, "usage_db": dbStatus, "logging": a.Config().Logging,
				}
				type chip struct {
					Index int    `json:"index"`
					Lock  string `json:"lock"`
					Held  bool   `json:"lock_held"`
					Users []int  `json:"users"`
				}
				chips := []chip{}
				for _, n := range tenstorrent.Chips() {
					chips = append(chips, chip{Index: n, Lock: runtime.TT.LockPath(n), Held: runtime.TT.Held(n), Users: tenstorrent.Users(n)})
				}
				res["tenstorrent"] = chips
				findings := securityFindings(a)
				res["security"] = findings
				if gerr != nil {
					res["gpu_error"] = gerr.Error()
				}
				return e.out(res, func(w io.Writer) {
					fmt.Fprintf(w, "substrate %s (%s)\nconfig:   %s\nstate:    %s\ndata:     %s\nusage db: %s (%s)\n\n",
						Version, spec.Version, filepath.Dir(config.ConfigPath()), paths.State(), paths.Data(), store.Path(), dbStatus)
					switch {
					case gerr != nil:
						fmt.Fprintf(w, "GPUs: nvidia-smi failed: %v\n", gerr)
					case len(gs) == 0:
						fmt.Fprintln(w, "GPUs: none found (nvidia-smi absent)")
					default:
						for _, g := range gs {
							fmt.Fprintf(w, "GPU %d: %s, %d/%d MiB used\n", g.Index, g.Name, g.MemUsedMiB, g.MemTotalMiB)
						}
					}
					for _, c := range chips {
						state := "free"
						if c.Held || len(c.Users) > 0 {
							state = fmt.Sprintf("busy (lock held=%v, open by pids %v)", c.Held, c.Users)
						}
						fmt.Fprintf(w, "Tenstorrent chip %d: %s; lock %s\n", c.Index, state, c.Lock)
					}
					fmt.Fprintln(w)
					for _, r := range engines {
						if r.Installed {
							fmt.Fprintf(w, "ok   %-15s %s  %s\n", r.ID, r.Bin, r.Version)
						} else {
							fmt.Fprintf(w, "--   %-15s not found (tried %s)\n", r.ID, strings.Join(r.Tried, ", "))
						}
					}
					for _, m := range manifestErrs {
						fmt.Fprintf(w, "\nmanifest error: %s\n", m)
					}
					fmt.Fprintf(w, "\n%d registered model(s)\n", len(a.RegisteredModels()))
					for _, m := range missing {
						fmt.Fprintf(w, "missing  %s: %s does not exist (restore it, or substrate model rm %s)\n", m.Name, m.Source, m.Name)
					}
					printFindings(w, findings)
				})
			}
		}}
}

// ---- serve ----

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func cmdServe() *command {
	return &command{name: "serve", synopsis: "[--host H] [--port P] [--tls-cert F --tls-key F [--tls-client-ca F]] [--placement memory|exclusive|off] [--no-autostart] [--api-key-file F]",
		summary: "run the unified OpenAI/Anthropic-compatible gateway (placement, limits, keys, metering, /metrics)",
		build: func(fs *flag.FlagSet) action {
			host := fs.String("host", "", "listen address (default: config gateway.host)")
			port := fs.Int("port", 0, "listen port (default: config gateway.port)")
			noAuto := fs.Bool("no-autostart", false, "never start models on demand")
			placementMode := fs.String("placement", "", "memory, exclusive or off (default: config gateway.placement)")
			keyFile := fs.String("api-key-file", "", "file holding the master (admin) key, mode 0600 (default: $SUBSTRATE_API_KEY); never pass a key as an argument, other users can read the command line")
			insecure := fs.Bool("insecure", false, "allow a non-loopback address without any API key")
			tlsCert := fs.String("tls-cert", "", "serve HTTPS with this PEM certificate (default: config gateway.tls.cert)")
			tlsKey := fs.String("tls-key", "", "PEM private key for --tls-cert (default: config gateway.tls.key)")
			tlsCA := fs.String("tls-client-ca", "", "require client certificates signed by these PEM CAs (mutual TLS)")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("serve", pos, 0, 0); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				switch *placementMode {
				case "", config.PlacementMemory, config.PlacementExclusive, config.PlacementOff:
				default:
					return errs.New(errs.Usage, "serve: --placement %q (want memory, exclusive or off)", *placementMode)
				}
				// Command-line settings win over config.yaml, including after a hot reload.
				a.SetOverlay(func(c *config.Config) {
					if *placementMode != "" {
						c.Gateway.Placement = *placementMode
					}
					if *tlsCert != "" || *tlsKey != "" {
						c.Gateway.TLS.Cert, c.Gateway.TLS.Key = *tlsCert, *tlsKey
					}
					if *tlsCA != "" {
						c.Gateway.TLS.ClientCA = *tlsCA
					}
				})
				g := a.Config().Gateway
				if *host == "" {
					*host = g.Host
				}
				if *port == 0 {
					*port = g.Port
				}
				key, err := masterKey(*keyFile)
				if err != nil {
					return err
				}
				ks, err := keys.Load()
				if err != nil {
					return err
				}
				if !isLoopback(*host) && key == "" && len(ks.Keys) == 0 && !*insecure {
					return errs.New(errs.Usage, "serve: %s is reachable from other machines; create keys (substrate key add) or set a master key ($SUBSTRATE_API_KEY or --api-key-file), or pass --insecure", *host)
				}
				logger := log.New(e.stderr, "substrate: ", log.LstdFlags)
				a.Warn = logger.Printf
				var tlsConf *tls.Config
				if g.TLS.Enabled() {
					if err := g.TLS.Validate(); err != nil {
						return errs.Wrap(errs.Usage, err)
					}
					if tlsConf, err = gateway.TLSConfig(g.TLS, logger.Printf); err != nil {
						return errs.Wrap(errs.Usage, err)
					}
				} else if !isLoopback(*host) {
					logger.Printf("warning: serving plain HTTP on %s: prompts and API keys cross the network unencrypted (set gateway.tls or --tls-cert/--tls-key)", *host)
				}
				if _, err := auditKey(a.Config(), ""); err != nil {
					return err
				}
				gw := gateway.New(a, logger)
				defer gw.Close()
				gw.NoAutostart, gw.APIKey = *noAuto, key

				ln, err := net.Listen("tcp", net.JoinHostPort(*host, fmt.Sprint(*port)))
				if err != nil {
					return errs.Wrap(errs.Conflict, err)
				}
				scheme := "http"
				if tlsConf != nil {
					ln, scheme = tls.NewListener(ln, tlsConf), "https"
				}
				srv := &http.Server{Handler: gw.Handler(), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 5 * time.Minute}
				ctx, stop := signal.NotifyContext(e.ctx, os.Interrupt, syscall.SIGTERM)
				defer stop()
				hup := make(chan os.Signal, 1)
				signal.Notify(hup, syscall.SIGHUP)
				defer signal.Stop(hup)
				go func() {
					for {
						select {
						case <-ctx.Done():
							return
						case <-hup:
							gw.Reload()
						}
					}
				}()
				gw.Run(ctx)
				mtls := ""
				if g.TLS.ClientCA != "" {
					mtls = " mtls=on"
				}
				logger.Printf("gateway on %s://%s/v1 (placement=%s autostart=%v keys=%d logging=%v bodies=%v%s; config reloads on change or SIGHUP)",
					scheme, ln.Addr(), a.Config().Gateway.Placement, g.Autostart && !*noAuto, len(ks.Keys), a.Config().Logging.Enabled,
					a.Config().Logging.Bodies, mtls)
				drained := make(chan struct{})
				go func() {
					defer close(drained)
					<-ctx.Done()
					logger.Printf("shutting down: waiting up to 30s for in-flight requests")
					sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					srv.Shutdown(sctx)
				}()
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				<-drained // handlers finished: safe to flush the usage log
				if hijacked := srv.Close(); hijacked != nil {
					logger.Printf("close: %v", hijacked)
				}
				logger.Printf("gateway stopped; engine instances keep running (substrate ps / stop)")
				return nil
			}
		}}
}

// masterKey reads the gateway's master key from a file (which must not be readable by others) or, without
// one, from $SUBSTRATE_API_KEY.
func masterKey(file string) (string, error) {
	if file == "" {
		return os.Getenv("SUBSTRATE_API_KEY"), nil
	}
	st, err := os.Stat(file)
	if err != nil {
		return "", errs.Wrap(errs.NotFound, err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", errs.New(errs.Usage, "--api-key-file %s is readable by other users (mode %v); chmod 600 it", file, st.Mode().Perm())
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", errs.New(errs.Usage, "--api-key-file %s is empty", file)
	}
	return key, nil
}

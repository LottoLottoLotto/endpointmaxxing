package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/LottoLottoLotto/substrate/internal/app"
	"github.com/LottoLottoLotto/substrate/internal/config"
	"github.com/LottoLottoLotto/substrate/internal/errs"
	"github.com/LottoLottoLotto/substrate/internal/gateway"
	"github.com/LottoLottoLotto/substrate/internal/meter"
	"github.com/LottoLottoLotto/substrate/internal/runtime"
	"github.com/LottoLottoLotto/substrate/internal/store"
	"github.com/LottoLottoLotto/substrate/internal/tenstorrent"
)

// launchFlags are shared by run and ensure.
type launchFlags struct {
	engine, name, id, host *string
	port                   *int
	lenient, force         *bool
	timeout                *time.Duration
	flags                  map[string]string
}

func addLaunchFlags(fs *flag.FlagSet) *launchFlags {
	l := &launchFlags{
		engine:  fs.String("engine", "", "engine id (default: best compatible installed engine)"),
		name:    fs.String("name", "", "served model name (default: the model's name)"),
		id:      fs.String("id", "", "instance id (default: <name>.<engine>)"),
		host:    fs.String("host", "127.0.0.1", "listen address"),
		port:    fs.Int("port", 0, "listen port (default: a free port)"),
		lenient: fs.Bool("lenient", false, "drop unified flags the engine cannot express instead of failing"),
		timeout: fs.Duration("timeout", 0, "readiness timeout (default: the engine manifest's)"),
		force:   fs.Bool("force", false, "skip the GPU memory check and placement"),
		flags:   map[string]string{},
	}
	addUnified(fs, l.flags)
	return l
}

func (l *launchFlags) opts(e *env, extra []string) app.LaunchOpts {
	return app.LaunchOpts{Engine: *l.engine, Name: *l.name, ID: *l.id, Host: *l.host, Port: *l.port,
		Flags: l.flags, Args: extra, Lenient: *l.lenient, Owner: e.owner, Force: *l.force}
}

// interruptible returns a context cancelled by Ctrl-C (CLI only).
func (e *env) interruptible() (context.Context, func()) {
	if e.owner != "cli" {
		return e.ctx, func() {}
	}
	return signal.NotifyContext(e.ctx, os.Interrupt)
}

func cmdRun() *command {
	return &command{name: "run", synopsis: "<model|path|org/repo> [flags] [-- engine args]",
		summary: "start a model on an engine (routes automatically)", tool: true, passthrough: true,
		build: func(fs *flag.FlagSet) action {
			l := addLaunchFlags(fs)
			dry := fs.Bool("dry-run", false, "print the launch plan without starting")
			noWait := fs.Bool("no-wait", false, "return once the process starts, without waiting for readiness")
			return func(e *env, pos, extra []string) error {
				if err := wantArgs("run", pos, 1, 1); err != nil {
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
				opts := l.opts(e, extra)
				if *dry {
					plan, iid, err := a.PlanPlaced(t, opts)
					if err != nil {
						return err
					}
					// The plan is for reading and pasting: the key is masked wherever the engine takes it.
					plan.Argv = runtime.Redact(plan.Argv, l.flags["api_key"])
					if key := l.flags["api_key"]; key != "" {
						for k, v := range plan.Env {
							if v == key {
								plan.Env[k] = "***"
							}
						}
					}
					return e.out(map[string]any{"id": iid, "plan": plan}, func(w io.Writer) { printPlan(w, iid, plan) })
				}
				if a.HostEngine(t, opts.Engine) != nil {
					return ensure(e, a, pos[0], opts, *l.timeout) // hosted models are loaded, not launched
				}
				in, plan, err := a.Launch(t, opts)
				if err != nil {
					return err
				}
				for _, w := range plan.Warnings {
					e.warn("%s", w)
				}
				e.progress("started %s (pid %d) on %s; log: %s", in.ID, in.PID, in.BaseURL, in.Log)
				status := runtime.StatusStarting
				if !*noWait {
					ctx, stop := e.interruptible()
					defer stop()
					e.progress("waiting for %s ...", in.HealthURL)
					if err := a.AwaitReady(ctx, in, *l.timeout); err != nil {
						if ctx.Err() != nil {
							return fmt.Errorf("interrupted; %s keeps loading (substrate stop %s)", in.ID, in.ID)
						}
						return err
					}
					status = runtime.StatusReady
				}
				return e.out(instanceView(in, status), func(w io.Writer) {
					fmt.Fprintf(w, "%s %s: %s/v1 (model %q, engine %s)\n", in.ID, status, in.BaseURL, in.Name, in.Engine)
				})
			}
		}}
}

func cmdEnsure() *command {
	return &command{name: "ensure", synopsis: "<model|path|org/repo> [flags] [-- engine args]",
		summary: "idempotently get a ready instance (reuse a running one or start it)", tool: true, passthrough: true,
		build: func(fs *flag.FlagSet) action {
			l := addLaunchFlags(fs)
			return func(e *env, pos, extra []string) error {
				if err := wantArgs("ensure", pos, 1, 1); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				return ensure(e, a, pos[0], l.opts(e, extra), *l.timeout)
			}
		}}
}

// ensure runs App.Ensure and reports the instance serving ref.
func ensure(e *env, a *app.App, ref string, o app.LaunchOpts, timeout time.Duration) error {
	ctx, stop := e.interruptible()
	defer stop()
	res, err := a.Ensure(ctx, ref, o, timeout)
	if err != nil {
		return err
	}
	v := instanceView(res.Instance, runtime.StatusReady)
	return e.out(map[string]any{"instance": v, "model": res.Model, "started": res.Started, "loaded": res.Loaded}, func(w io.Writer) {
		verb := "reused"
		switch {
		case res.Started:
			verb = "started"
		case res.Loaded:
			verb = "loaded " + res.Model + " into"
		}
		fmt.Fprintf(w, "%s %s: %s (model %q, engine %s)\n", verb, v.ID, v.APIURL, res.Model, v.Engine)
	})
}

func printPlan(w io.Writer, id string, p *runtime.Plan) {
	model := fmt.Sprintf("%s (%s)", p.Model, p.Format)
	if p.Hosting {
		model = "none: a multi-model host, models are loaded into it on first use"
	}
	fmt.Fprintf(w, "instance: %s\nengine:   %s\nmodel:    %s\nendpoint: %s/v1\n", id, p.Engine, model, p.BaseURL)
	if p.Cwd != "" {
		fmt.Fprintf(w, "cwd:      %s\n", p.Cwd)
	}
	for _, k := range sortedKeys(p.Env) {
		if k != "PATH" {
			fmt.Fprintf(w, "env:      %s=%s\n", k, p.Env[k])
		}
	}
	fmt.Fprintf(w, "command:  %s\n", runtime.ShellJoin(p.Argv))
	if p.Placement != "" {
		fmt.Fprintf(w, "placement: %s\n", p.Placement)
	}
	for _, x := range p.Warnings {
		fmt.Fprintf(w, "warning:  %s\n", x)
	}
}

// view is the JSON shape of an instance in ps/run/ensure/health output (API key redacted).
type view struct {
	*runtime.Instance
	Status string `json:"status"`
	APIURL string `json:"api_url"`
	// Hosted lists the models a multi-model host holds (ps only).
	Hosted []string `json:"hosted,omitempty"`
}

func instanceView(in *runtime.Instance, status string) view {
	return view{Instance: in.Redacted(), Status: status, APIURL: in.BaseURL + "/v1"}
}

func cmdPs() *command {
	return &command{name: "ps", synopsis: "[--prune]", summary: "list instances", tool: true,
		build: func(fs *flag.FlagSet) action {
			prune := fs.Bool("prune", false, "forget instances whose process exited")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("ps", pos, 0, 0); err != nil {
					return err
				}
				a, err := e.app() // installs the event recorder
				if err != nil {
					return err
				}
				ins, err := runtime.List()
				if err != nil {
					return err
				}
				views := []view{}
				for _, in := range ins {
					st := in.Status(e.ctx)
					if st == runtime.StatusExited {
						if *prune {
							if err := in.Forget(); err != nil {
								return err
							}
							continue
						}
						in.NoteExit()
					}
					v := instanceView(in, st)
					if in.Hosting && st == runtime.StatusReady {
						if eng, err := a.Engine(in.Engine); err == nil && eng.Host != nil {
							v.Hosted, _ = runtime.HostedModels(e.ctx, in, eng.Host)
						}
					}
					views = append(views, v)
				}
				return e.out(views, func(w io.Writer) {
					if len(views) == 0 {
						fmt.Fprintln(w, "no instances")
						return
					}
					rows := make([][]string, len(views))
					for i, v := range views {
						where := "-"
						switch {
						case len(v.Devices) > 0:
							where = "tt:" + tenstorrent.Join(v.Devices)
						case len(v.GPUs) > 0:
							where = "gpu:" + tenstorrent.Join(v.GPUs)
						}
						name := v.Name
						if v.Hosting {
							name = "host: " + strings.Join(v.Hosted, ",")
						}
						rows[i] = []string{v.ID, name, v.Engine, v.Status, where, fmt.Sprint(v.PID), v.APIURL, v.Owner,
							time.Since(v.StartedAt).Round(time.Second).String()}
					}
					e.table("ID\tMODEL\tENGINE\tSTATUS\tDEVICES\tPID\tENDPOINT\tOWNER\tUP", rows)
				})
			}
		}}
}

func cmdStop() *command {
	return &command{name: "stop", synopsis: "<instance|model>... | --all", summary: "stop instances (a model in a multi-model host is unloaded)", tool: true,
		build: func(fs *flag.FlagSet) action {
			all := fs.Bool("all", false, "stop every instance")
			return func(e *env, pos, _ []string) error {
				a, err := e.app()
				if err != nil {
					return err
				}
				var targets []*runtime.Instance
				unloaded := []string{}
				if *all {
					if targets, err = runtime.List(); err != nil {
						return err
					}
				} else {
					if len(pos) == 0 {
						return errs.New(errs.Usage, "stop: name instances or pass --all")
					}
					for _, ref := range pos {
						in, err := runtime.Find(ref)
						if errs.CodeOf(err) == errs.NotFound {
							if host := hostOf(e.ctx, a, ref); host != nil {
								if err := a.UnloadHosted(e.ctx, host.Engine, ref); err != nil {
									return err
								}
								unloaded = append(unloaded, ref)
								continue
							}
						}
						if err != nil {
							return err
						}
						targets = append(targets, in)
					}
				}
				stopped := []string{}
				for _, in := range targets {
					e.progress("stopping %s (pid %d)", in.ID, in.PID)
					if err := in.Stop(); err != nil {
						return fmt.Errorf("stop %s: %w", in.ID, err)
					}
					stopped = append(stopped, in.ID)
				}
				return e.out(map[string]any{"stopped": stopped, "unloaded": unloaded}, func(w io.Writer) {
					for _, m := range unloaded {
						fmt.Fprintf(w, "unloaded %s\n", m)
					}
					if len(stopped) == 0 && len(unloaded) == 0 {
						fmt.Fprintln(w, "nothing to stop")
					}
				})
			}
		}}
}

// hostOf returns the running multi-model host that holds model, or nil.
func hostOf(ctx context.Context, a *app.App, model string) *runtime.Instance {
	ins, _ := runtime.List()
	for _, in := range ins {
		if !in.Hosting || !in.Alive() {
			continue
		}
		if eng, err := a.Engine(in.Engine); err == nil && eng.Host != nil {
			if names, err := runtime.HostedModels(ctx, in, eng.Host); err == nil && slices.Contains(names, model) {
				return in
			}
		}
	}
	return nil
}

func cmdLogs() *command {
	return &command{name: "logs", synopsis: "<instance|model> [-f] [-n N]", summary: "show an instance's log (kept after stop)", tool: true,
		build: func(fs *flag.FlagSet) action {
			follow := fs.Bool("f", false, "follow (CLI only)")
			n := fs.Int("n", 50, "lines to show")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("logs", pos, 1, 1); err != nil {
					return err
				}
				in, err := runtime.Find(pos[0])
				if err != nil {
					// The record is gone after stop; the log stays. Only a well-formed id names a log: anything
					// else (../) would be a path to some other file.
					if !config.NameRe.MatchString(pos[0]) {
						return err
					}
					path := runtime.LogPath(pos[0])
					if _, serr := os.Stat(path); serr != nil {
						return err
					}
					in = &runtime.Instance{ID: pos[0], Log: path}
				}
				if e.json || e.owner != "cli" {
					return e.out(map[string]string{"id": in.ID, "log": in.Log, "tail": runtime.Tail(in.Log, *n)}, nil)
				}
				if t := runtime.Tail(in.Log, *n); t != "" {
					fmt.Fprintln(e.stdout, t)
				}
				if !*follow {
					return nil
				}
				f, err := os.Open(in.Log)
				if err != nil {
					return err
				}
				defer f.Close()
				if _, err := f.Seek(0, io.SeekEnd); err != nil {
					return err
				}
				ctx, stop := e.interruptible()
				defer stop()
				buf := make([]byte, 64<<10)
				for ctx.Err() == nil {
					k, _ := f.Read(buf)
					if k > 0 {
						e.stdout.Write(buf[:k])
						continue
					}
					if in.PID > 0 && !in.Alive() {
						return nil
					}
					select {
					case <-ctx.Done():
					case <-time.After(250 * time.Millisecond):
					}
				}
				return nil
			}
		}}
}

func cmdHealth() *command {
	return &command{name: "health", synopsis: "<instance|model> [--wait DURATION]", summary: "probe an instance (exit 0 only when ready)", tool: true,
		build: func(fs *flag.FlagSet) action {
			wait := fs.Duration("wait", 0, "wait up to this long for readiness")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("health", pos, 1, 1); err != nil {
					return err
				}
				in, err := runtime.Find(pos[0])
				if err != nil {
					return err
				}
				if *wait > 0 {
					if err := in.WaitReady(e.ctx, *wait); err != nil {
						return err
					}
				}
				st := in.Status(e.ctx)
				if err := e.out(instanceView(in, st), func(w io.Writer) { fmt.Fprintf(w, "%s: %s\n", in.ID, st) }); err != nil {
					return err
				}
				switch st {
				case runtime.StatusExited:
					return errs.New(errs.EngineFailed, "%s exited", in.ID)
				case runtime.StatusStarting:
					return errs.New(errs.Timeout, "%s is not ready yet", in.ID)
				}
				return nil
			}
		}}
}

// chatResult is chat's JSON output.
type chatResult struct {
	Model     string         `json:"model"`
	Instance  string         `json:"instance"`
	Engine    string         `json:"engine"`
	Via       string         `json:"via"` // gateway or direct
	RequestID string         `json:"request_id"`
	Content   string         `json:"content"`
	Reasoning string         `json:"reasoning,omitempty"`
	Finish    string         `json:"finish_reason,omitempty"`
	Usage     map[string]any `json:"usage,omitempty"`
	TookMS    int64          `json:"took_ms"`
}

func cmdChat() *command {
	return &command{name: "chat", synopsis: "<model> [prompt...] (prompt from stdin when omitted)",
		summary: "send one chat message (through the gateway when it runs, else directly; always metered)", tool: true,
		build: func(fs *flag.FlagSet) action {
			system := fs.String("system", "", "system prompt")
			maxTokens := fs.Int("max-tokens", 512, "reply token limit")
			temp := fs.Float64("temperature", -1, "sampling temperature (default: the engine's)")
			via := fs.String("via", "auto", "auto (gateway if running), gateway, or direct")
			noThink := fs.Bool("no-think", false, "ask the chat template to skip reasoning (chat_template_kwargs.enable_thinking=false)")
			timeout := fs.Duration("timeout", 10*time.Minute, "overall timeout, including starting the model")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("chat", pos, 1, -1); err != nil {
					return err
				}
				prompt := strings.Join(pos[1:], " ")
				if prompt == "" && e.stdin != nil {
					data, err := io.ReadAll(io.LimitReader(e.stdin, 64<<20))
					if err != nil {
						return err
					}
					prompt = strings.TrimSpace(string(data))
				}
				if prompt == "" {
					return errs.New(errs.Usage, "chat: empty prompt")
				}
				if *via != "auto" && *via != "gateway" && *via != "direct" {
					return errs.New(errs.Usage, "chat: --via must be auto, gateway or direct")
				}
				msgs := []map[string]string{}
				if *system != "" {
					msgs = append(msgs, map[string]string{"role": "system", "content": *system})
				}
				msgs = append(msgs, map[string]string{"role": "user", "content": prompt})
				body := map[string]any{"model": pos[0], "messages": msgs, "max_tokens": *maxTokens}
				if *temp >= 0 {
					body["temperature"] = *temp
				}
				if *noThink {
					body["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
				}
				ctx, cancel := context.WithTimeout(e.ctx, *timeout)
				defer cancel()
				res, err := chat(ctx, e, pos[0], body, *via)
				if err != nil {
					return err
				}
				return e.out(res, func(w io.Writer) {
					if res.Content == "" && res.Reasoning != "" {
						fmt.Fprintln(w, res.Reasoning)
						return
					}
					fmt.Fprintln(w, res.Content)
				})
			}
		}}
}

var chatClient = &http.Client{}

func chat(ctx context.Context, e *env, model string, body map[string]any, via string) (*chatResult, error) {
	a, err := e.app()
	if err != nil {
		return nil, err
	}
	gw := a.Config().Gateway
	gwBase, gwClient := gateway.Client(&gw)
	useGateway := via == "gateway" || (via == "auto" && gatewayUp(ctx, gwBase, gwClient))
	res := &chatResult{Model: model}
	start := time.Now()
	var url, key string
	var in *runtime.Instance
	client := chatClient
	if useGateway {
		res.Via, url, key, client = "gateway", gwBase+"/v1/chat/completions", os.Getenv("SUBSTRATE_API_KEY"), gwClient
	} else {
		r, err := a.Ensure(ctx, model, app.LaunchOpts{Owner: e.owner}, 0)
		if err != nil {
			return nil, err
		}
		in = r.Instance
		body["model"] = r.Model
		res.Via, url, key = "direct", in.BaseURL+"/v1/chat/completions", in.APIKey
		res.Instance, res.Engine = in.ID, in.Engine
	}
	payload, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Substrate-Client", "substrate-"+e.owner)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	sent := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, errs.Wrap(errs.EngineFailed, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	res.TookMS = time.Since(start).Milliseconds()
	if useGateway {
		res.RequestID = resp.Header.Get("X-Substrate-Request-Id")
		res.Instance, res.Engine = resp.Header.Get("X-Substrate-Instance"), resp.Header.Get("X-Substrate-Engine")
	} else {
		res.RequestID = logDirect(a, e, in, payload, data, resp.StatusCode, start, sent)
	}
	if resp.StatusCode >= 400 {
		code := errs.EngineFailed
		if resp.StatusCode == 404 {
			code = errs.NotFound
		} else if resp.StatusCode < 500 {
			code = errs.Unsupported
		}
		return nil, errs.New(code, "%s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("engine reply is not JSON: %w", err)
	}
	if len(out.Choices) > 0 {
		c := out.Choices[0]
		res.Content, res.Finish = c.Message.Content, c.FinishReason
		res.Reasoning = c.Message.ReasoningContent
		if res.Reasoning == "" {
			res.Reasoning = c.Message.Reasoning
		}
	}
	res.Usage = out.Usage
	return res, nil
}

// logDirect meters a request that bypassed the gateway, so usage stays complete.
func logDirect(a *app.App, e *env, in *runtime.Instance, reqBody, respBody []byte, status int, start, sent time.Time) string {
	if !a.Config().Logging.Enabled {
		return ""
	}
	m := meter.New(start, false, len(respBody)+1)
	m.Write(respBody)
	u := m.Finish()
	d := time.Since(start).Milliseconds()
	q := sent.Sub(start).Milliseconds()
	r := &store.Request{ID: fmt.Sprintf("req_cli_%d", time.Now().UnixNano()), TS: start.UnixMilli(), Model: in.Name,
		Instance: in.ID, Engine: in.Engine, API: "chat", Path: "/v1/chat/completions", Status: status,
		Client: "substrate-" + e.owner, PromptTokens: u.Prompt, CompletionTokens: u.Completion, CachedTokens: u.Cached,
		ReasoningTokens: u.Reasoning, UsageSource: u.Source, FinishReason: u.Finish, QueueMS: &q, DurationMS: &d,
		ReqBytes: int64(len(reqBody)), RespBytes: int64(len(respBody))}
	switch {
	case status >= 500:
		r.Error, r.ErrorKind = strings.TrimSpace(string(respBody)), "engine_error"
	case status >= 400:
		r.Error, r.ErrorKind = strings.TrimSpace(string(respBody)), "engine_rejected"
	}
	// Direct requests are labelled like gateway traffic; they are never blocked (the operator sent them).
	if insp, err := gateway.NewInspector(a); err == nil {
		insp.Inspect(r, reqBody)
	}
	if c, ok := a.Config().Pricing.Cost(r.Model, r.PromptTokens, r.CompletionTokens, r.CachedTokens); ok {
		r.Cost = &c
	}
	if a.Config().Logging.Bodies || (a.Config().Logging.FlaggedBodies && len(r.Flags) > 0) {
		max := a.Config().Logging.BodyMaxBytes
		r.RequestBody, r.ResponseBody = clip(reqBody, max), clip(respBody, max)
		r.BodyTruncated = len(reqBody) > max || len(respBody) > max
	}
	db, err := a.DB()
	if err == nil {
		err = db.InsertRequest(r)
	}
	if err != nil {
		e.warn("usage log: %v", err)
	}
	return r.ID
}

func clip(b []byte, max int) []byte {
	if len(b) > max {
		return b[:max]
	}
	return b
}

func gatewayUp(ctx context.Context, base string, client *http.Client) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

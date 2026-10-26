package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/gateway"
	"github.com/lotto/substrate/internal/policy"
	"github.com/lotto/substrate/internal/store"
)

func cmdFlagged() *command {
	return &command{name: "flagged", synopsis: "[--since 7d] [--min-severity high] [filters]",
		summary: "prompt policy matches by rule and detector (use `requests --flagged` for the requests)", tool: true,
		build: func(fs *flag.FlagSet) action {
			ff := addFilterFlags(fs, "7d", -1).addRequestFilters(fs)
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("flagged", pos, 0, 0); err != nil {
					return err
				}
				f, err := ff.filter()
				if err != nil {
					return err
				}
				db, err := e.db()
				if err != nil {
					return err
				}
				rows, err := db.FlagStats(f)
				if err != nil {
					return err
				}
				if rows == nil {
					rows = []store.FlagRow{}
				}
				return e.out(rows, func(w io.Writer) {
					if len(rows) == 0 {
						fmt.Fprintln(w, "no flagged requests in this window")
						return
					}
					t := make([][]string, len(rows))
					for i, r := range rows {
						t[i] = []string{r.Rule, r.Detector, r.Severity, r.Action, fmt.Sprint(r.Requests), fmt.Sprint(r.Matches),
							fmt.Sprint(r.Clients), ts(r.LastTS)}
					}
					e.table("RULE\tDETECTOR\tSEVERITY\tACTION\tREQUESTS\tMATCHES\tCLIENTS\tLAST", t)
				})
			}
		}}
}

func cmdErrorStats() *command {
	return &command{name: "errors", synopsis: "[--since 7d] [--kinds] [filters]",
		summary: "failed requests by error kind, with the latest message of each", tool: true,
		build: func(fs *flag.FlagSet) action {
			kinds := fs.Bool("kinds", false, "list the error kinds and what they mean")
			ff := addFilterFlags(fs, "7d", -1).addRequestFilters(fs)
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("errors", pos, 0, 0); err != nil {
					return err
				}
				if *kinds {
					names := make([]string, 0, len(store.ErrorKinds))
					for k := range store.ErrorKinds {
						names = append(names, k)
					}
					sort.Strings(names)
					return e.out(store.ErrorKinds, func(w io.Writer) {
						t := make([][]string, len(names))
						for i, k := range names {
							t[i] = []string{k, store.ErrorKinds[k]}
						}
						e.table("KIND\tMEANING", t)
					})
				}
				f, err := ff.filter()
				if err != nil {
					return err
				}
				db, err := e.db()
				if err != nil {
					return err
				}
				rows, err := db.ErrorStats(f)
				if err != nil {
					return err
				}
				if rows == nil {
					rows = []store.ErrorRow{}
				}
				return e.out(rows, func(w io.Writer) {
					if len(rows) == 0 {
						fmt.Fprintln(w, "no failed requests in this window")
						return
					}
					t := make([][]string, len(rows))
					for i, r := range rows {
						t[i] = []string{r.Kind, fmt.Sprint(r.Count), fmt.Sprint(r.Models), fmt.Sprint(r.Clients), ts(r.LastTS),
							dash(r.LastModel), dash(r.LastRequest), dash(oneLine(r.LastError, 80))}
					}
					e.table("KIND\tCOUNT\tMODELS\tCLIENTS\tLAST\tLAST MODEL\tLAST REQUEST\tLAST ERROR", t)
				})
			}
		}}
}

func cmdReport() *command {
	return &command{name: "report", synopsis: "[--since 24h] [filters]",
		summary: "summary of traffic: totals, models, clients, prompt categories, policy flags and errors", tool: true,
		build: func(fs *flag.FlagSet) action {
			ff := addFilterFlags(fs, "24h", -1).addRequestFilters(fs)
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("report", pos, 0, 0); err != nil {
					return err
				}
				f, err := ff.filter()
				if err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				db, err := e.db()
				if err != nil {
					return err
				}
				r, err := gateway.BuildReport(a, db, f)
				if err != nil {
					return err
				}
				return e.out(r, func(w io.Writer) { printReport(e, w, r, *ff.since) })
			}
		}}
}

func printReport(e *env, w io.Writer, r *gateway.Report, since string) {
	window := "all time"
	if since != "" {
		window = "last " + since
	}
	t := r.Totals
	fmt.Fprintf(w, "Window: %s\n", window)
	fmt.Fprintf(w, "Requests: %d (%d failed, %d flagged, %d blocked) from %d clients\n", t.Requests, t.Errors, t.Flagged, t.Blocked, t.Clients)
	fmt.Fprintf(w, "Tokens: %d prompt, %d completion (%d cached, %d reasoning); %d requests unmetered\n",
		t.PromptTokens, t.CompletionTokens, t.CachedTokens, t.ReasoningTokens, t.UnmeteredReqs)
	fmt.Fprintf(w, "Models: %d registered, %d running (%d ready), %d used in the window\n",
		r.Models.Registered, r.Models.Running, r.Models.Ready, r.Models.Used)
	usage := func(title string, rows []store.UsageRow) {
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s\n", title)
		tab := make([][]string, len(rows))
		for i, u := range rows {
			tab[i] = []string{dash(u.Key), fmt.Sprint(u.Requests), fmt.Sprint(u.Errors), fmt.Sprint(u.Flagged),
				fmt.Sprint(u.PromptTokens), fmt.Sprint(u.CompletionTokens), optf(u.AvgDurationMS, "ms"), optf(u.DecodeTokPerSec, "")}
		}
		e.table("KEY\tREQS\tERRS\tFLAGGED\tPROMPT\tCOMPLETION\tAVG TIME\tDECODE TOK/S", tab)
	}
	usage("By model", r.ByModel)
	usage("By client", r.ByClient)
	usage("By prompt category", r.ByCategory)
	if len(r.Flags) > 0 {
		fmt.Fprintln(w, "\nPolicy flags")
		tab := make([][]string, len(r.Flags))
		for i, f := range r.Flags {
			tab[i] = []string{f.Rule, f.Detector, f.Severity, f.Action, fmt.Sprint(f.Requests), fmt.Sprint(f.Clients)}
		}
		e.table("RULE\tDETECTOR\tSEVERITY\tACTION\tREQUESTS\tCLIENTS", tab)
	}
	if len(r.Errors) > 0 {
		fmt.Fprintln(w, "\nErrors")
		tab := make([][]string, len(r.Errors))
		for i, x := range r.Errors {
			tab[i] = []string{x.Kind, fmt.Sprint(x.Count), dash(x.LastModel), dash(oneLine(x.LastError, 80))}
		}
		e.table("KIND\tCOUNT\tLAST MODEL\tLAST ERROR", tab)
	}
}

func cmdPolicy() *command {
	return &command{name: "policy", summary: "prompt policy rules and detectors",
		sub: []*command{policyShowCmd(), policyTestCmd()}}
}

func policyShowCmd() *command {
	return &command{name: "show", summary: "the configured rules, categories and built-in detectors",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("policy show", pos, 0, 0); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				cats := a.Cfg.Classify.Categories
				if len(cats) == 0 {
					cats = policy.DefaultCategories
				}
				out := map[string]any{"policy": a.Cfg.Policy, "classify": a.Cfg.Classify, "categories": cats,
					"detectors": policy.Detectors()}
				return e.out(out, func(w io.Writer) {
					fmt.Fprintf(w, "policy: %s\n", onOff(a.Cfg.Policy.Enabled))
					t := make([][]string, len(a.Cfg.Policy.Rules))
					for i, r := range a.Cfg.Policy.Rules {
						var what []string
						what = append(what, r.Detectors...)
						if len(r.Keywords) > 0 {
							what = append(what, fmt.Sprintf("%d keywords", len(r.Keywords)))
						}
						if len(r.Patterns) > 0 {
							what = append(what, fmt.Sprintf("%d patterns", len(r.Patterns)))
						}
						scope := []string{}
						for _, s := range []struct {
							k  string
							vs []string
						}{{"models", r.Models}, {"clients", r.Clients}, {"apis", r.APIs}} {
							if len(s.vs) > 0 {
								scope = append(scope, s.k+"="+strings.Join(s.vs, ","))
							}
						}
						t[i] = []string{r.Name, orDefault(r.Severity, "medium"), orDefault(r.Action, policy.ActionFlag),
							strings.Join(what, ", "), dash(strings.Join(scope, " "))}
					}
					e.table("RULE\tSEVERITY\tACTION\tMATCHES\tSCOPE", t)
					fmt.Fprintf(w, "\nclassify: %s (default category %q)\n", onOff(a.Cfg.Classify.Enabled), orDefault(a.Cfg.Classify.Default, "general"))
					names := make([]string, len(cats))
					for i, c := range cats {
						names[i] = c.Name
					}
					fmt.Fprintf(w, "categories: %s\n\ndetectors:\n", strings.Join(names, ", "))
					dt := [][]string{}
					for _, d := range policy.DetectorNames() {
						dt = append(dt, []string{d, policy.Detectors()[d]})
					}
					e.table("DETECTOR\tFINDS", dt)
				})
			}
		}}
}

func policyTestCmd() *command {
	return &command{name: "test", synopsis: "<text> | --file prompt.txt | --body request.json [--model M --client C]",
		summary: "run the policy and classifier over a prompt, without sending it anywhere", tool: true,
		build: func(fs *flag.FlagSet) action {
			file := fs.String("file", "", "read the prompt text from a file (- for stdin)")
			bodyFile := fs.String("body", "", "read a whole chat/completions/messages request body (JSON) from a file")
			model := fs.String("model", "", "model the request would go to (for rules scoped to models)")
			client := fs.String("client", "", "client the request would come from (for rules scoped to clients)")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("policy test", pos, 0, 1); err != nil {
					return err
				}
				var body []byte
				switch {
				case *bodyFile != "":
					data, err := readFileOrStdin(*bodyFile)
					if err != nil {
						return err
					}
					if !json.Valid(data) {
						return errs.New(errs.Usage, "policy test: %s is not JSON", *bodyFile)
					}
					body = data
				case *file != "" || len(pos) == 1:
					text := ""
					if len(pos) == 1 {
						text = pos[0]
					} else {
						data, err := readFileOrStdin(*file)
						if err != nil {
							return err
						}
						text = string(data)
					}
					body, _ = json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": text}}})
				default:
					return errs.New(errs.Usage, "policy test: give the prompt text, --file or --body")
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				insp, err := gateway.NewInspector(a)
				if err != nil {
					return err
				}
				rec := &store.Request{Model: *model, Client: *client, API: "chat"}
				blocked := insp.Inspect(rec, body)
				out := map[string]any{"category": rec.Category, "flags": rec.Flags, "flag_severity": rec.FlagSeverity, "blocked": blocked}
				if rec.Flags == nil {
					out["flags"] = []policy.Flag{}
				}
				return e.out(out, func(w io.Writer) {
					fmt.Fprintf(w, "category: %s\n", dash(rec.Category))
					if len(rec.Flags) == 0 {
						fmt.Fprintln(w, "flags: none")
						return
					}
					t := make([][]string, len(rec.Flags))
					for i, f := range rec.Flags {
						t[i] = []string{f.Rule, f.Detector, f.Severity, f.Action, fmt.Sprint(f.Count), f.Sample}
					}
					e.table("RULE\tDETECTOR\tSEVERITY\tACTION\tMATCHES\tSAMPLE", t)
					if blocked {
						fmt.Fprintln(w, "\nthe gateway would refuse this request (400, content policy)")
					}
				})
			}
		}}
}

func readFileOrStdin(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

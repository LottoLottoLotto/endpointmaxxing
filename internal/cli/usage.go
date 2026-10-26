package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/gateway"
	"github.com/lotto/substrate/internal/policy"
	"github.com/lotto/substrate/internal/store"
)

// filterFlags are shared by the usage-database commands.
type filterFlags struct {
	since, until, model, engine, instance, client *string
	limit                                         *int
	// request-only filters (nil unless addRequestFilters registered them)
	category, errorKind, minSeverity, rule *string
	errors, flagged                        *bool
}

func addFilterFlags(fs *flag.FlagSet, defSince string, defLimit int) *filterFlags {
	f := &filterFlags{
		since:    fs.String("since", defSince, "start: duration back (90m, 24h, 7d) or date (2026-10-01); empty = all"),
		until:    fs.String("until", "", "end: duration back or date"),
		model:    fs.String("model", "", "only this model"),
		engine:   fs.String("engine", "", "only this engine"),
		instance: fs.String("instance", "", "only this instance"),
		client:   fs.String("client", "", "only this client (X-Substrate-Client header or request user)"),
	}
	if defLimit >= 0 {
		f.limit = fs.Int("limit", defLimit, "max rows (0 = all)")
	}
	return f
}

// addRequestFilters registers the filters that apply to recorded requests (usage, requests, flags, errors, report).
func (f *filterFlags) addRequestFilters(fs *flag.FlagSet) *filterFlags {
	f.category = fs.String("category", "", "only this prompt category")
	f.errorKind = fs.String("error-kind", "", "only this error kind (see `substrate errors --kinds`)")
	f.errors = fs.Bool("errors", false, "only failed requests")
	f.flagged = fs.Bool("flagged", false, "only requests that matched a policy rule")
	f.minSeverity = fs.String("min-severity", "", "only flagged requests at or above this severity: "+strings.Join(policy.Severities, ", "))
	f.rule = fs.String("rule", "", "only requests flagged by this policy rule")
	return f
}

func (f *filterFlags) filter() (store.Filter, error) {
	out := store.Filter{Model: *f.model, Engine: *f.engine, Instance: *f.instance, Client: *f.client}
	var err error
	if *f.since != "" {
		if out.Since, err = gateway.ParseSince(*f.since); err != nil {
			return out, err
		}
	}
	if *f.until != "" {
		if out.Until, err = gateway.ParseSince(*f.until); err != nil {
			return out, err
		}
	}
	if f.limit != nil {
		out.Limit = *f.limit
	}
	if f.category != nil {
		out.Category, out.ErrorKind, out.Rule = *f.category, *f.errorKind, *f.rule
		out.Errors, out.Flagged, out.MinSeverity = *f.errors, *f.flagged, *f.minSeverity
		if out.MinSeverity != "" && !slices.Contains(policy.Severities, out.MinSeverity) {
			return out, errs.New(errs.Usage, "--min-severity %q: want %s", out.MinSeverity, strings.Join(policy.Severities, ", "))
		}
	}
	return out, nil
}

func (e *env) db() (*store.DB, error) {
	a, err := e.app()
	if err != nil {
		return nil, err
	}
	return a.DB()
}

func ms(v *int64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%dms", *v)
}

func optf(v *float64, unit string) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%s", *v, unit)
}

func opti(v *int64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprint(*v)
}

func ts(msec int64) string { return time.UnixMilli(msec).Format("2006-01-02 15:04:05") }

// oneLine keeps the first line of s, cut to max runes, for table cells.
func oneLine(s string, max int) string {
	s, _, _ = strings.Cut(s, "\n")
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "..."
	}
	return s
}
func cmdUsage() *command {
	groups := make([]string, 0, len(store.Groups))
	for g := range store.Groups {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	return &command{name: "usage", synopsis: "[--by model|client|category|error_kind|day|...] [--since 24h] [filters]",
		summary: "token and request totals from the usage database", tool: true,
		build: func(fs *flag.FlagSet) action {
			by := fs.String("by", "model", "group by: "+strings.Join(groups, ", ")+", or total")
			ff := addFilterFlags(fs, "24h", -1).addRequestFilters(fs)
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("usage", pos, 0, 0); err != nil {
					return err
				}
				f, err := ff.filter()
				if err != nil {
					return err
				}
				group := *by
				if group == "total" {
					group = ""
				} else if _, ok := store.Groups[group]; !ok {
					return errs.New(errs.Usage, "usage: --by %q (want %s or total)", group, strings.Join(groups, ", "))
				}
				db, err := e.db()
				if err != nil {
					return err
				}
				rows, err := db.Usage(group, f)
				if err != nil {
					return err
				}
				if rows == nil {
					rows = []store.UsageRow{}
				}
				return e.out(map[string]any{"by": *by, "since": *ff.since, "rows": rows}, func(w io.Writer) {
					if len(rows) == 0 {
						fmt.Fprintln(w, "no requests recorded in this window (only traffic through `substrate serve` or `substrate chat` is metered)")
						return
					}
					t := make([][]string, len(rows))
					for i, r := range rows {
						key := r.Key
						if key == "" {
							key = "(none)"
						}
						t[i] = []string{key, fmt.Sprint(r.Requests), fmt.Sprint(r.Errors), fmt.Sprint(r.Flagged), fmt.Sprint(r.PromptTokens),
							fmt.Sprint(r.CompletionTokens), fmt.Sprint(r.CachedTokens), fmt.Sprint(r.UnmeteredReqs),
							optf(r.AvgTTFTMS, "ms"), optf(r.AvgDurationMS, "ms"), optf(r.DecodeTokPerSec, "")}
					}
					e.table(strings.ToUpper(*by)+"\tREQS\tERRS\tFLAGGED\tPROMPT\tCOMPLETION\tCACHED\tUNMETERED\tAVG TTFT\tAVG TIME\tDECODE TOK/S", t)
				})
			}
		}}
}

func cmdRequests() *command {
	return &command{name: "requests", synopsis: "[--id ID] [--since 24h] [filters]",
		summary: "list recorded requests (or show one, with bodies when captured)", tool: true,
		build: func(fs *flag.FlagSet) action {
			id := fs.String("id", "", "show one request, with its bodies when captured")
			ff := addFilterFlags(fs, "", 50).addRequestFilters(fs)
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("requests", pos, 0, 0); err != nil {
					return err
				}
				db, err := e.db()
				if err != nil {
					return err
				}
				if *id != "" {
					return showRequest(e, db, *id)
				}
				f, err := ff.filter()
				if err != nil {
					return err
				}
				rows, err := db.Requests(f)
				if err != nil {
					return err
				}
				if rows == nil {
					rows = []store.Request{}
				}
				return e.out(rows, func(w io.Writer) {
					t := make([][]string, len(rows))
					for i, r := range rows {
						t[i] = []string{ts(r.TS), r.ID, r.Model, dash(r.Engine), r.API, fmt.Sprint(r.Status), opti(r.PromptTokens),
							opti(r.CompletionTokens), ms(r.TTFTMS), ms(r.DurationMS), dash(r.Client), dash(r.Category), dash(flagSummary(r)),
							dash(oneLine(r.Error, 70))}
					}
					e.table("TIME\tID\tMODEL\tENGINE\tAPI\tSTATUS\tPROMPT\tCOMPL\tTTFT\tTIME\tCLIENT\tCATEGORY\tFLAGS\tERROR", t)
				})
			}
		}}
}

// flagSummary is "<severity>:<rule>,<rule>" for a flagged request.
func flagSummary(r store.Request) string {
	if len(r.Flags) == 0 {
		return ""
	}
	var rules []string
	for _, f := range r.Flags {
		if !slices.Contains(rules, f.Rule) {
			rules = append(rules, f.Rule)
		}
	}
	return r.FlagSeverity + ":" + strings.Join(rules, ",")
}

func showRequest(e *env, db *store.DB, id string) error {
	rows, err := db.Requests(store.Filter{ID: id, Limit: 1})
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return errs.New(errs.NotFound, "no request %q", id)
	}
	r := &rows[0]
	req, resp, trunc, ok, err := db.Bodies(id)
	if err != nil {
		return err
	}
	out := map[string]any{"request": r}
	if ok {
		out["bodies"] = map[string]any{"request": rawOrString(req), "response": rawOrString(resp), "truncated": trunc}
	}
	return e.out(out, func(w io.Writer) {
		data, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintln(w, string(data))
		if ok {
			fmt.Fprintf(w, "\n--- request body ---\n%s\n--- response body ---\n%s\n", req, resp)
			if trunc {
				fmt.Fprintln(w, "(truncated)")
			}
		} else {
			fmt.Fprintln(w, "\n(bodies not captured: set logging.bodies: true in config.yaml)")
		}
	})
}

// rawOrString embeds JSON bodies as JSON and anything else (SSE, truncated JSON) as a string.
func rawOrString(b []byte) any {
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	return string(b)
}

func cmdEvents() *command {
	return &command{name: "events", synopsis: "[--since 7d] [filters]",
		summary: "engine lifecycle events: start, ready (load time), start_failed, stop, exited", tool: true,
		build: func(fs *flag.FlagSet) action {
			ff := addFilterFlags(fs, "", 50)
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("events", pos, 0, 0); err != nil {
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
				rows, err := db.Events(f)
				if err != nil {
					return err
				}
				if rows == nil {
					rows = []store.Event{}
				}
				return e.out(rows, func(w io.Writer) {
					t := make([][]string, len(rows))
					for i, r := range rows {
						detail := strings.ReplaceAll(r.Detail, "\n", " | ")
						if len(detail) > 100 {
							detail = detail[:100] + "..."
						}
						t[i] = []string{ts(r.TS), r.Kind, r.Instance, dash(r.Owner), ms(r.DurationMS), dash(detail)}
					}
					e.table("TIME\tEVENT\tINSTANCE\tOWNER\tDURATION\tDETAIL", t)
				})
			}
		}}
}

func cmdSamples() *command {
	return &command{name: "samples", synopsis: "[--gpu] [--since 1h] [filters]",
		summary: "GPU and per-instance memory samples taken by the gateway", tool: true,
		build: func(fs *flag.FlagSet) action {
			gpus := fs.Bool("gpu", false, "show per-GPU samples instead of per-instance")
			ff := addFilterFlags(fs, "1h", 100)
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("samples", pos, 0, 0); err != nil {
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
				if *gpus {
					rows, err := db.GPUSamples(f)
					if err != nil {
						return err
					}
					if rows == nil {
						rows = []store.GPUSample{}
					}
					return e.out(rows, func(w io.Writer) {
						t := make([][]string, len(rows))
						for i, r := range rows {
							t[i] = []string{ts(r.TS), fmt.Sprint(r.GPU), r.Name, fmt.Sprintf("%d/%d", r.MemUsedMiB, r.MemTotalMiB),
								fmt.Sprintf("%d%%", r.UtilPct), fmt.Sprintf("%.0fW", r.PowerW)}
						}
						e.table("TIME\tGPU\tNAME\tMEM MiB\tUTIL\tPOWER", t)
					})
				}
				rows, err := db.InstanceSamples(f)
				if err != nil {
					return err
				}
				if rows == nil {
					rows = []store.InstanceSample{}
				}
				return e.out(rows, func(w io.Writer) {
					t := make([][]string, len(rows))
					for i, r := range rows {
						t[i] = []string{ts(r.TS), r.Instance, r.Engine, fmt.Sprint(r.GPUMemMiB), fmt.Sprint(r.RSSMiB), fmt.Sprint(r.Processes)}
					}
					e.table("TIME\tINSTANCE\tENGINE\tGPU MiB\tRSS MiB\tPROCS", t)
				})
			}
		}}
}

func cmdPrune() *command {
	return &command{name: "prune", synopsis: "--older-than 90d", summary: "delete usage records older than a cutoff",
		build: func(fs *flag.FlagSet) action {
			older := fs.String("older-than", "", "cutoff: duration back (90d, 720h) or date")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("prune", pos, 0, 0); err != nil {
					return err
				}
				if *older == "" {
					return errs.New(errs.Usage, "prune: --older-than is required")
				}
				before, err := gateway.ParseSince(*older)
				if err != nil {
					return err
				}
				db, err := e.db()
				if err != nil {
					return err
				}
				n, err := db.Prune(before)
				if err != nil {
					return err
				}
				return e.out(map[string]any{"before": before, "deleted": n}, func(w io.Writer) {
					fmt.Fprintf(w, "deleted before %s: %v\n", before.Format(time.RFC3339), n)
				})
			}
		}}
}

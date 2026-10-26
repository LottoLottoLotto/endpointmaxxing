package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/gateway"
	"github.com/lotto/substrate/internal/keys"
)

// ---- team ----

func cmdTeam() *command {
	return &command{name: "team", summary: "teams: shared token and cost budgets and model access for groups of API keys",
		sub: []*command{teamListCmd(), teamSetCmd(), teamRmCmd()}}
}

func teamListCmd() *command {
	return &command{name: "list", summary: "teams with their limits and use this day and month", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("team list", pos, 0, 0); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				db, _ := e.db()
				perTeam := map[string]int{}
				if f, err := keys.Load(); err == nil {
					for _, k := range f.Keys {
						if k.Team != "" {
							perTeam[k.Team]++
						}
					}
				}
				teams, err := gateway.TeamStatuses(a.Config(), db, perTeam)
				if err != nil {
					return err
				}
				cur := a.Config().Pricing.CurrencyOrDefault()
				return e.out(teams, func(w io.Writer) {
					if len(teams) == 0 {
						fmt.Fprintln(w, "no teams (substrate team set <name> --budget-per-month 500 ...)")
						return
					}
					rows := [][]string{}
					for _, t := range teams {
						models := dash(strings.Join(t.Config.Models, ","))
						if len(t.Limits) == 0 {
							rows = append(rows, []string{t.Name, fmt.Sprint(t.Keys), models, "-", "-", "-"})
						}
						for _, l := range t.Limits {
							used := fmt.Sprintf("%.0f / %.0f", l.Used, l.Max)
							if l.Limit == "budget_per_month" {
								used = fmt.Sprintf("%.2f / %.2f %s", l.Used, l.Max, cur)
							}
							rows = append(rows, []string{t.Name, fmt.Sprint(t.Keys), models, l.Limit, used, fmt.Sprintf("%.0f%%", 100*l.Ratio)})
						}
					}
					e.table("TEAM\tKEYS\tMODELS\tLIMIT\tUSED\tSHARE", rows)
				})
			}
		}}
}

func teamSetCmd() *command {
	return &command{name: "set", synopsis: "<name> [--tokens-per-day N] [--tokens-per-month N] [--budget-per-month X] [--alert-at 0.8] [--models a,b*]",
		summary: "create or change a team in config.yaml (the running gateway picks it up)",
		build: func(fs *flag.FlagSet) action {
			tpd := fs.Int64("tokens-per-day", -1, "team tokens per calendar day (0 unlimited)")
			tpm := fs.Int64("tokens-per-month", -1, "team tokens per calendar month (0 unlimited)")
			bpm := fs.Float64("budget-per-month", -1, "team cost per calendar month in pricing.currency (0 unlimited; needs prices)")
			alert := fs.Float64("alert-at", -1, "share of a limit that raises a budget alert (default 0.8)")
			models := fs.String("models", "", `comma-separated model globs the team may use ("-" for all)`)
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("team set", pos, 1, 1); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				t := config.Team{Name: pos[0]}
				if cur, ok := a.Config().Team(pos[0]); ok {
					t = *cur
				}
				if *tpd >= 0 {
					t.TokensPerDay = *tpd
				}
				if *tpm >= 0 {
					t.TokensPerMonth = *tpm
				}
				if *bpm >= 0 {
					t.BudgetPerMonth = *bpm
				}
				if *alert >= 0 {
					t.AlertAt = *alert
				}
				switch *models {
				case "":
				case "-":
					t.Models = nil
				default:
					t.Models = splitList(*models)
				}
				if _, err := config.UpdateTeam(t); err != nil {
					return errs.Wrap(errs.Usage, err)
				}
				if t.BudgetPerMonth > 0 && a.Config().Pricing.Default == nil && len(a.Config().Pricing.Models) == 0 {
					e.warn("team %s has a cost budget but no model has a price, so nothing is charged against it (substrate price set ...)", t.Name)
				}
				e.recordAdmin(fmt.Sprintf("team set %s (tokens/day %d, tokens/month %d, budget/month %g, alert at %g, models %s)",
					t.Name, t.TokensPerDay, t.TokensPerMonth, t.BudgetPerMonth, t.AlertFraction(), dash(strings.Join(t.Models, ","))))
				return e.out(t, func(w io.Writer) { fmt.Fprintf(w, "team %s saved\n", t.Name) })
			}
		}}
}

func teamRmCmd() *command {
	return &command{name: "rm", synopsis: "<name>", summary: "remove a team (its keys keep the label but lose the budgets)",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("team rm", pos, 1, 1); err != nil {
					return err
				}
				ok, err := config.RemoveTeam(pos[0])
				if err != nil {
					return err
				}
				if !ok {
					return errs.New(errs.NotFound, "no team %q", pos[0])
				}
				e.recordAdmin("team rm " + pos[0])
				return e.out(map[string]any{"removed": pos[0]}, func(w io.Writer) { fmt.Fprintf(w, "removed team %s\n", pos[0]) })
			}
		}}
}

// ---- price ----

func cmdPrice() *command {
	return &command{name: "price", summary: "prices per million tokens, for cost accounting and team budgets",
		sub: []*command{priceListCmd(), priceSetCmd(), priceRmCmd()}}
}

func priceListCmd() *command {
	return &command{name: "list", summary: "model prices (per million tokens)", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("price list", pos, 0, 0); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				p := a.Config().Pricing
				return e.out(p, func(w io.Writer) {
					rows := [][]string{}
					names := make([]string, 0, len(p.Models))
					for m := range p.Models {
						names = append(names, m)
					}
					sort.Strings(names)
					row := func(name string, pr config.Price) []string {
						cached := "= prompt"
						if pr.Cached != nil {
							cached = fmt.Sprintf("%g", *pr.Cached)
						}
						return []string{name, fmt.Sprintf("%g", pr.Prompt), cached, fmt.Sprintf("%g", pr.Completion)}
					}
					for _, m := range names {
						rows = append(rows, row(m, p.Models[m]))
					}
					if p.Default != nil {
						rows = append(rows, row("(default)", *p.Default))
					}
					if len(rows) == 0 {
						fmt.Fprintln(w, "no prices: requests are not costed (substrate price set <model|glob|default> --prompt X --completion Y)")
						return
					}
					fmt.Fprintf(w, "%s per million tokens\n", p.CurrencyOrDefault())
					e.table("MODEL\tPROMPT\tCACHED\tCOMPLETION", rows)
				})
			}
		}}
}

func priceSetCmd() *command {
	return &command{name: "set", synopsis: "<model|glob|default> --prompt X --completion Y [--cached Z] [--currency EUR]",
		summary: "price a model (per million tokens); applies to requests from now on",
		build: func(fs *flag.FlagSet) action {
			prompt := fs.Float64("prompt", -1, "price per million prompt tokens")
			completion := fs.Float64("completion", -1, "price per million completion tokens")
			cached := fs.Float64("cached", -1, "price per million cached prompt tokens (default: the prompt price)")
			currency := fs.String("currency", "", "currency label for all prices (default USD)")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("price set", pos, 1, 1); err != nil {
					return err
				}
				if *prompt < 0 || *completion < 0 {
					return errs.New(errs.Usage, "price set: --prompt and --completion are required (>= 0)")
				}
				p := &config.Price{Prompt: *prompt, Completion: *completion}
				if *cached >= 0 {
					p.Cached = cached
				}
				if _, err := config.SetPrice(pos[0], p); err != nil {
					return errs.Wrap(errs.Usage, err)
				}
				if *currency != "" {
					if _, err := config.Update(func(c *config.Config) error { c.Pricing.Currency = *currency; return nil }); err != nil {
						return err
					}
				}
				e.recordAdmin(fmt.Sprintf("price set %s (prompt %g, completion %g, cached %s per million tokens)", pos[0], p.Prompt, p.Completion,
					cachedText(p.Cached)))
				return e.out(map[string]any{"model": pos[0], "price": p}, func(w io.Writer) { fmt.Fprintf(w, "priced %s\n", pos[0]) })
			}
		}}
}

func priceRmCmd() *command {
	return &command{name: "rm", synopsis: "<model|glob|default>", summary: "remove a price",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("price rm", pos, 1, 1); err != nil {
					return err
				}
				if _, err := config.SetPrice(pos[0], nil); err != nil {
					return err
				}
				e.recordAdmin("price rm " + pos[0])
				return e.out(map[string]any{"removed": pos[0]}, func(w io.Writer) { fmt.Fprintf(w, "removed the price of %s\n", pos[0]) })
			}
		}}
}

// ---- reload ----

func cmdReload() *command {
	return &command{name: "reload", summary: "make the running gateway re-read config.yaml and models.yaml now (it also does on every change)",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("reload", pos, 0, 0); err != nil {
					return err
				}
				if _, err := config.Load(); err != nil { // fail here, with the file's error, before asking the gateway
					return errs.Wrap(errs.Usage, err)
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				base, client := gateway.Client(&a.Config().Gateway)
				req, _ := http.NewRequestWithContext(e.ctx, http.MethodPost, base+"/substrate/v1/reload", nil)
				if key := os.Getenv("SUBSTRATE_API_KEY"); key != "" {
					req.Header.Set("Authorization", "Bearer "+key)
				}
				resp, err := client.Do(req)
				if err != nil {
					return errs.New(errs.NotFound, "no gateway answering at %s: %v", base, err)
				}
				defer resp.Body.Close()
				var out map[string]any
				json.NewDecoder(resp.Body).Decode(&out)
				if resp.StatusCode != http.StatusOK {
					return errs.New(errs.Usage, "reload: %s: %v", resp.Status, out["error"])
				}
				return e.out(out, func(w io.Writer) { fmt.Fprintf(w, "gateway at %s reloaded its configuration\n", base) })
			}
		}}
}

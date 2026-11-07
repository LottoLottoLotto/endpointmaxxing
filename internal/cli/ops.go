package cli

import (
	"archive/tar"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/lotto/substrate/internal/app"
	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/keys"
	"github.com/lotto/substrate/internal/paths"
	"github.com/lotto/substrate/internal/settings"
	"github.com/lotto/substrate/internal/store"
)

// ---- alias ----

func cmdAlias() *command {
	return &command{name: "alias", summary: "stable model names for clients (chat-default -> a real model); switch without touching clients",
		sub: []*command{aliasListCmd(), aliasSetCmd(), aliasRmCmd()}}
}

func aliasListCmd() *command {
	return &command{name: "list", summary: "model aliases", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("alias list", pos, 0, 0); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				al := a.Config().Aliases
				if al == nil {
					al = map[string]string{}
				}
				return e.out(al, func(w io.Writer) {
					if len(al) == 0 {
						fmt.Fprintln(w, "no aliases (substrate alias set <name> <model>)")
						return
					}
					names := make([]string, 0, len(al))
					for n := range al {
						names = append(names, n)
					}
					sort.Strings(names)
					rows := make([][]string, len(names))
					for i, n := range names {
						rows[i] = []string{n, al[n]}
					}
					e.table("ALIAS\tMODEL", rows)
				})
			}
		}}
}

func aliasSetCmd() *command {
	return &command{name: "set", synopsis: "<alias> <model>", summary: "point an alias at a model (the running gateway switches within seconds)",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("alias set", pos, 2, 2); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				res, err := settings.SetAlias(a, pos[0], pos[1])
				if err != nil {
					return err
				}
				e.applied(res)
				return e.out(map[string]string{"alias": pos[0], "model": pos[1]}, func(w io.Writer) { fmt.Fprintf(w, "%s -> %s\n", pos[0], pos[1]) })
			}
		}}
}

func aliasRmCmd() *command {
	return &command{name: "rm", synopsis: "<alias>", summary: "remove an alias",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("alias rm", pos, 1, 1); err != nil {
					return err
				}
				res, err := settings.RemoveAlias(pos[0])
				if err != nil {
					return err
				}
				e.applied(res)
				return e.out(map[string]string{"removed": pos[0]}, func(w io.Writer) { fmt.Fprintf(w, "removed alias %s\n", pos[0]) })
			}
		}}
}

// ---- backup ----

func cmdBackup() *command {
	return &command{name: "backup", synopsis: "[--out substrate-backup.tar.gz] [--audit]",
		summary: "archive config, models, keys, engine manifests and a consistent copy of the usage database",
		build: func(fs *flag.FlagSet) action {
			out := fs.String("out", "", "archive path (default substrate-backup-<time>.tar.gz in the current directory)")
			withAudit := fs.Bool("audit", false, "include logging.audit_file")
			noDB := fs.Bool("no-db", false, "leave out the usage database")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("backup", pos, 0, 0); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				if *out == "" {
					*out = "substrate-backup-" + time.Now().Format("20060102-150405") + ".tar.gz"
				}
				files, err := backup(a, *out, !*noDB, *withAudit)
				if err != nil {
					os.Remove(*out)
					return err
				}
				return e.out(map[string]any{"archive": *out, "files": files}, func(w io.Writer) {
					fmt.Fprintf(w, "wrote %s (%d files). Restore by stopping the gateway and extracting into %s: config/ to %s, state/ to %s.\n",
						*out, len(files), "a scratch directory", filepath.Dir(config.ConfigPath()), paths.State())
				})
			}
		}}
}

// backup writes a tar.gz with config/ (config.yaml, models.yaml, keys.yaml, engines/) and state/ (usage.db
// snapshot taken with VACUUM INTO, audit log). Keys are stored hashed, but the archive still holds them and
// any captured prompts: it is written mode 0600.
func backup(a *app.App, out string, withDB, withAudit bool) ([]string, error) {
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	var names []string
	add := func(src, name string) error {
		st, err := os.Stat(src)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		r, err := os.Open(src)
		if err != nil {
			return err
		}
		defer r.Close()
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: st.Size(), ModTime: st.ModTime()}); err != nil {
			return err
		}
		if _, err := io.Copy(tw, r); err != nil {
			return err
		}
		names = append(names, name)
		return nil
	}
	for _, p := range []string{config.ConfigPath(), config.ModelsPath(), keys.Path()} {
		if err := add(p, "config/"+filepath.Base(p)); err != nil {
			return nil, err
		}
	}
	err = filepath.WalkDir(config.EnginesDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(config.EnginesDir(), p)
		return add(p, "config/engines/"+filepath.ToSlash(rel))
	})
	if err != nil {
		return nil, err
	}
	if withDB {
		if _, err := os.Stat(store.Path()); err == nil {
			db, err := a.DB()
			if err != nil {
				return nil, err
			}
			tmp, err := os.CreateTemp(paths.State(), "usage-backup-*.db")
			if err != nil {
				return nil, err
			}
			tmp.Close()
			os.Remove(tmp.Name()) // VACUUM INTO wants a new file
			defer os.Remove(tmp.Name())
			if _, err := db.Exec(`VACUUM INTO ?`, tmp.Name()); err != nil {
				return nil, fmt.Errorf("snapshotting the usage database: %w", err)
			}
			if err := add(tmp.Name(), "state/usage.db"); err != nil {
				return nil, err
			}
		}
	}
	if withAudit && a.Config().Logging.AuditFile != "" {
		p := a.Config().Logging.AuditFile
		if !filepath.IsAbs(p) {
			p = filepath.Join(paths.State(), p)
		}
		if err := add(p, "state/"+filepath.Base(p)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return names, f.Close()
}

// ---- security checks (doctor) ----

// securityFinding is one doctor warning.
type securityFinding struct {
	Check  string `json:"check"`
	Detail string `json:"detail"`
}

// securityFindings checks the setup against common deployment mistakes.
func securityFindings(a *app.App) []securityFinding {
	c := a.Config()
	var out []securityFinding
	add := func(check, f string, args ...any) { out = append(out, securityFinding{check, fmt.Sprintf(f, args...)}) }
	exposed := !isLoopback(c.Gateway.Host) && c.Gateway.Host != ""
	if exposed && !c.Gateway.TLS.Enabled() {
		add("plain_http", "gateway.host %s is reachable from other machines over plain HTTP: set gateway.tls", c.Gateway.Host)
	}
	if c.Gateway.MetricsPublic && exposed {
		add("metrics_public", "metrics_public exposes model, team and key names to anyone who can reach %s", c.Gateway.Host)
	}
	if c.Logging.Bodies && c.Logging.RetentionDays == 0 {
		add("bodies_kept_forever", "logging.bodies stores every prompt and response with no logging.retention_days")
	}
	if c.Logging.FlaggedBodies && c.Logging.RetentionDays == 0 {
		add("flagged_bodies_kept_forever", "logging.flagged_bodies keeps flagged prompts (credentials, personal data) with no retention_days")
	}
	if !c.Logging.Enabled {
		add("no_usage_log", "logging.enabled is off: no request records, audit trail or budgets")
	}
	f, err := keys.Load()
	if err != nil {
		add("keys", "keys.yaml: %v", err)
		return out
	}
	if exposed && len(f.Keys) == 0 {
		add("open_gateway", "gateway.host %s with no API keys: only --api-key or --insecure would let it start", c.Gateway.Host)
	}
	var last map[string]int64
	if db, err := a.DB(); err == nil {
		last, _ = db.LastUsed()
	}
	now := time.Now()
	admins := 0
	for _, k := range f.Keys {
		if k.Status(now) != "active" {
			continue
		}
		if k.Role == keys.RoleAdmin {
			admins++
		}
		if k.Expires == nil {
			add("key_no_expiry", "key %s (%s) never expires: substrate key set %s --expires 90d", k.Name, k.Role, k.Name)
		} else if k.Expires.Sub(now) < 14*24*time.Hour {
			add("key_expiring", "key %s expires %s", k.Name, k.Expires.Local().Format("2006-01-02"))
		}
		used, ok := last[k.Name]
		idleSince := k.Created
		if ok {
			idleSince = time.UnixMilli(used)
		}
		if now.Sub(idleSince) > 90*24*time.Hour {
			add("key_unused", "key %s unused for %d days: substrate key disable %s", k.Name, int(now.Sub(idleSince).Hours()/24), k.Name)
		}
		if k.Team != "" {
			if _, ok := c.Team(k.Team); !ok {
				add("key_unknown_team", "key %s is in team %s, which config.yaml does not define (no budget applies)", k.Name, k.Team)
			}
		}
	}
	if admins > 3 {
		add("many_admins", "%d active admin keys; use auditor keys for read-only access", admins)
	}
	for _, t := range c.Teams {
		if t.BudgetPerMonth > 0 && c.Pricing.Default == nil && len(c.Pricing.Models) == 0 {
			add("budget_without_prices", "team %s has a cost budget, but no model is priced", t.Name)
		}
	}
	for alias, target := range c.Aliases {
		if _, ok := a.RegisteredModel(target); !ok {
			add("alias_target", "alias %s points at %s, which is not a registered model", alias, target)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Check < out[j].Check })
	return out
}

// printFindings is doctor's security section.
func printFindings(w io.Writer, fs []securityFinding) {
	if len(fs) == 0 {
		fmt.Fprintln(w, "\nsecurity: no findings")
		return
	}
	fmt.Fprintf(w, "\nsecurity: %d finding(s)\n", len(fs))
	for _, f := range fs {
		fmt.Fprintf(w, "  %-28s %s\n", f.Check, f.Detail)
	}
}

package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/gateway"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/keys"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/paths"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/settings"
)

// ---- key ----

func cmdKey() *command {
	return &command{name: "key", summary: "manage gateway API keys (roles, teams, model access, quotas, expiry, rotation)",
		sub: []*command{keyAddCmd(), keyListCmd(), keySetCmd(), keyRotateCmd(), keyToggleCmd("disable", true), keyToggleCmd("enable", false), keyRmCmd()}}
}

func splitList(s string) []string {
	var out []string
	for _, m := range strings.Split(s, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	return out
}

func keyAddCmd() *command {
	return &command{name: "add", synopsis: "<name> [--role user|auditor|admin] [--team T] [--models a,b*] [--rpm N] [--tokens-per-day N] [--expires 90d]",
		summary: "create a key (printed once)",
		build: func(fs *flag.FlagSet) action {
			role := fs.String("role", keys.RoleUser, "user (inference, own usage), auditor (read-only: usage, logs, reports; no inference) or admin (everything)")
			team := fs.String("team", "", "team whose budgets and model access apply (config.yaml teams; emx team set)")
			models := fs.String("models", "", "comma-separated model names or globs the key may use (default all)")
			rpm := fs.Int("rpm", 0, "requests per minute (0 unlimited)")
			tpd := fs.Int64("tokens-per-day", 0, "prompt+completion tokens per calendar day (0 unlimited)")
			maxOut := fs.Int("max-output-tokens", 0, "cap (and default) for each request's output tokens (0: only gateway.max_output_tokens)")
			delegate := fs.Bool("delegate", false, "the key acts for clients of its own (X-Emx-Client): each is recorded and gets its own prompt-cache domain (e.g. a rental host agent's renters)")
			cacheShared := fs.Bool("cache-shared", false, "all of the key's requests share one prompt cache under gateway.cache_isolation (clients that trust each other)")
			expires := fs.String("expires", "", "when the key stops working: 90d, 720h, 2027-01-31 (default never)")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("key add", pos, 1, 1); err != nil {
					return err
				}
				exp, err := settings.ParseExpiry(*expires)
				if err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				k := keys.Key{Name: pos[0], Role: *role, Team: *team, Models: splitList(*models), RPM: *rpm, TokensPerDay: *tpd,
					MaxOutputTokens: *maxOut, Delegate: *delegate, CacheShared: *cacheShared, Expires: exp}
				secret, res, err := settings.AddKey(a.Config(), k)
				if err != nil {
					return err
				}
				e.applied(res)
				return e.out(map[string]any{"name": k.Name, "role": k.Role, "team": k.Team, "key": secret, "models": k.Models, "rpm": k.RPM,
					"tokens_per_day": k.TokensPerDay, "expires": k.Expires}, func(w io.Writer) {
					fmt.Fprintf(w, "%s\n", secret)
					fmt.Fprintf(e.stderr, "key %s (%s) created; it is shown only now. Clients send it as Authorization: Bearer <key> or x-api-key.\n", k.Name, k.Role)
				})
			}
		}}
}

func keyListCmd() *command {
	return &command{name: "list", summary: "list keys with status, expiry and last use (secrets are never shown)",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("key list", pos, 0, 0); err != nil {
					return err
				}
				db, _ := e.db() // without a usage database keys still list, without last use
				infos, err := gateway.KeyInfos(db)
				if err != nil {
					return err
				}
				return e.out(infos, func(w io.Writer) {
					if len(infos) == 0 {
						fmt.Fprintln(w, "no keys: the gateway is open on loopback (emx key add <name>)")
						return
					}
					rows := make([][]string, len(infos))
					soon := time.Now().Add(14 * 24 * time.Hour)
					for i, k := range infos {
						status := k.Status
						if status == "active" && k.Expires != nil && k.Expires.Before(soon) {
							status = "expires soon"
						}
						if k.PrevExpires != nil && time.Now().Before(*k.PrevExpires) {
							status += " (old secret valid until " + k.PrevExpires.Local().Format("2006-01-02 15:04") + ")"
						}
						exp, last := "never", "never"
						if k.Expires != nil {
							exp = k.Expires.Local().Format(time.DateOnly)
						}
						if k.LastUsed != nil {
							last = k.LastUsed.Local().Format("2006-01-02 15:04")
						}
						rows[i] = []string{k.Name, k.Role, dash(k.Team), k.Prefix + "...", dash(strings.Join(k.Models, ",")), limit(int64(k.RPM)),
							limit(k.TokensPerDay), k.Created.Local().Format(time.DateOnly), exp, last, status}
					}
					e.table("NAME\tROLE\tTEAM\tKEY\tMODELS\tRPM\tTOKENS/DAY\tCREATED\tEXPIRES\tLAST USED\tSTATUS", rows)
				})
			}
		}}
}

func keySetCmd() *command {
	return &command{name: "set", synopsis: "<name> [--role R] [--team T] [--models a,b*] [--rpm N] [--tokens-per-day N] [--max-output-tokens N] [--expires 90d|never]",
		summary: "change a key's role, team, model access, limits or expiry (the secret stays)",
		build: func(fs *flag.FlagSet) action {
			role := fs.String("role", "", "user, auditor or admin")
			team := fs.String("team", "", `team ("-" for none)`)
			models := fs.String("models", "", `comma-separated model globs ("-" for all)`)
			rpm := fs.Int("rpm", -1, "requests per minute (0 unlimited)")
			tpd := fs.Int64("tokens-per-day", -1, "tokens per calendar day (0 unlimited)")
			maxOut := fs.Int("max-output-tokens", -1, "cap (and default) for each request's output tokens (0: only the gateway's)")
			delegate := fs.String("delegate", "", "on or off: the key acts for clients of its own (X-Emx-Client)")
			cacheShared := fs.String("cache-shared", "", "on or off: all of the key's requests share one prompt cache")
			expires := fs.String("expires", "", "90d, 720h, 2027-01-31 or never")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("key set", pos, 1, 1); err != nil {
					return err
				}
				p := settings.KeyPatch{}
				if *expires != "" {
					exp, err := settings.ParseExpiry(*expires)
					if err != nil {
						return err
					}
					p.ExpiresSet, p.Expires = true, exp
				}
				if *role != "" {
					p.Role = role
				}
				switch *team {
				case "":
				case "-":
					p.Team = new(string)
				default:
					p.Team = team
				}
				switch *models {
				case "":
				case "-":
					p.Models = &[]string{}
				default:
					ms := splitList(*models)
					p.Models = &ms
				}
				if *rpm >= 0 {
					p.RPM = rpm
				}
				if *tpd >= 0 {
					p.TokensPerDay = tpd
				}
				if *maxOut >= 0 {
					p.MaxOutputTokens = maxOut
				}
				switch *delegate {
				case "":
				case "on", "off":
					on := *delegate == "on"
					p.Delegate = &on
				default:
					return errs.New(errs.Usage, "key set: --delegate %q (want on or off)", *delegate)
				}
				switch *cacheShared {
				case "":
				case "on", "off":
					on := *cacheShared == "on"
					p.CacheShared = &on
				default:
					return errs.New(errs.Usage, "key set: --cache-shared %q (want on or off)", *cacheShared)
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				k, res, err := settings.SetKey(a.Config(), pos[0], p)
				if err != nil {
					return err
				}
				e.applied(res)
				return e.out(k, func(w io.Writer) { fmt.Fprintf(w, "updated key %s\n", k.Name) })
			}
		}}
}

func keyRotateCmd() *command {
	return &command{name: "rotate", synopsis: "<name> [--grace 24h]",
		summary: "replace a key's secret (printed once); the old one keeps working for --grace",
		build: func(fs *flag.FlagSet) action {
			grace := fs.Duration("grace", 24*time.Hour, "how long the old secret keeps working (0 revokes it now)")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("key rotate", pos, 1, 1); err != nil {
					return err
				}
				secret, k, res, err := settings.RotateKey(pos[0], *grace)
				if err != nil {
					return err
				}
				e.applied(res)
				return e.out(map[string]any{"name": k.Name, "key": secret, "old_secret_valid_until": k.PrevExpires}, func(w io.Writer) {
					fmt.Fprintf(w, "%s\n", secret)
					if k.PrevExpires != nil {
						fmt.Fprintf(e.stderr, "key %s rotated; the old secret works until %s. The new one is shown only now.\n", k.Name, k.PrevExpires.Local().Format("2006-01-02 15:04"))
					} else {
						fmt.Fprintf(e.stderr, "key %s rotated; the old secret no longer works. The new one is shown only now.\n", k.Name)
					}
				})
			}
		}}
}

func keyToggleCmd(name string, disable bool) *command {
	summary := "re-enable disabled keys"
	if disable {
		summary = "refuse keys immediately, keeping them and their history (enable restores them)"
	}
	return &command{name: name, synopsis: "<name>...", summary: summary,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("key "+name, pos, 1, -1); err != nil {
					return err
				}
				res, err := settings.SetKeysDisabled(pos, disable)
				if err != nil {
					return err
				}
				e.applied(res)
				if disable {
					warnNoUsableKeys(e)
				}
				return e.out(map[string]any{name + "d": pos}, func(w io.Writer) { fmt.Fprintf(w, "%sd %s\n", name, strings.Join(pos, ", ")) })
			}
		}}
}

func limit(n int64) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprint(n)
}

// warnNoUsableKeys tells the operator on stderr when a change left no key that authenticates.
func warnNoUsableKeys(e *env) {
	f, err := keys.Load()
	if err != nil {
		return
	}
	now := time.Now()
	for i := range f.Keys {
		if f.Keys[i].Status(now) == "active" {
			return
		}
	}
	if len(f.Keys) == 0 {
		fmt.Fprintln(e.stderr, "warning: that was the last API key. A gateway serving on a loopback address (or started with --insecure) now runs open: every caller is admin. Any other gateway refuses every request except the master key's until a key exists.")
		return
	}
	fmt.Fprintln(e.stderr, "warning: no usable API key is left (the others are disabled or expired): the gateway refuses every request except the master key's.")
}

func keyRmCmd() *command {
	return &command{name: "rm", synopsis: "<name>...", summary: "revoke keys (takes effect on the next request)",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("key rm", pos, 1, -1); err != nil {
					return err
				}
				res, err := settings.RemoveKeys(pos)
				if err != nil {
					return err
				}
				e.applied(res)
				warnNoUsableKeys(e)
				return e.out(map[string]any{"removed": pos}, func(w io.Writer) { fmt.Fprintf(w, "removed %s\n", strings.Join(pos, ", ")) })
			}
		}}
}

// ---- service ----

func cmdService() *command {
	return &command{name: "service", summary: "run the gateway as a systemd user service",
		sub: []*command{serviceInstallCmd(), serviceUninstallCmd(), serviceStatusCmd()}}
}

func unitPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		dir = filepath.Join(paths.Home(), ".config")
	}
	return filepath.Join(dir, "systemd", "user", "emx.service")
}

func systemctl(e *env, args ...string) error {
	cmd := exec.CommandContext(e.ctx, "systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout, cmd.Stderr = e.stderr, e.stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl --user %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// unit renders the service file. KillMode=process stops only the gateway on
// restart: engines live in their own sessions and keep serving across upgrades.
func unit(bin string, serveArgs []string, env []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[Unit]
Description=emx inference gateway
After=network-online.target

[Service]
Type=simple
ExecStart=%s serve%s
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=3
KillMode=process
TimeoutStopSec=45
`, bin, prefixSpace(strings.Join(serveArgs, " ")))
	for _, kv := range env {
		fmt.Fprintf(&b, "Environment=%q\n", kv)
	}
	b.WriteString("\n[Install]\nWantedBy=default.target\n")
	return b.String()
}

func prefixSpace(s string) string {
	if s == "" {
		return ""
	}
	return " " + s
}

func serviceInstallCmd() *command {
	return &command{name: "install", synopsis: "[--enable] [--env K=V]... [-- serve args]",
		summary: "write ~/.config/systemd/user/emx.service (PATH captured from this shell)", passthrough: true,
		build: func(fs *flag.FlagSet) action {
			enable := fs.Bool("enable", false, "also daemon-reload, enable and start it now")
			var envs multi
			fs.Var(&envs, "env", "extra Environment= line, KEY=VALUE (repeatable). Not for the master key: a value typed here is on this command line; give the gateway a key file instead (-- --api-key-file F)")
			return func(e *env, pos, extra []string) error {
				if err := wantArgs("service install", pos, 0, 0); err != nil {
					return err
				}
				if _, err := kv(envs, "--env"); err != nil {
					return err
				}
				bin, err := os.Executable()
				if err != nil {
					return err
				}
				if bin, err = filepath.EvalSymlinks(bin); err != nil {
					return err
				}
				// Engines are found through PATH (llama-server, vllm, ...): pin the PATH of this shell.
				vars := append([]string{"PATH=" + os.Getenv("PATH")}, envs...)
				if h := os.Getenv("EMX_HOME"); h != "" {
					vars = append(vars, "EMX_HOME="+h)
				}
				text := unit(bin, extra, vars)
				path := unitPath()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return err
				}
				if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
					return err
				}
				if *enable {
					if err := systemctl(e, "daemon-reload"); err != nil {
						return err
					}
					if err := systemctl(e, "enable", "--now", "emx.service"); err != nil {
						return err
					}
				}
				return e.out(map[string]any{"unit": path, "enabled": *enable}, func(w io.Writer) {
					fmt.Fprintf(w, "wrote %s\n", path)
					if !*enable {
						fmt.Fprintln(w, "start it: systemctl --user daemon-reload && systemctl --user enable --now emx")
					}
					if !lingering(e) {
						fmt.Fprintln(w, "keep it running after logout: loginctl enable-linger $USER")
					}
				})
			}
		}}
}

func serviceUninstallCmd() *command {
	return &command{name: "uninstall", summary: "stop, disable and remove the service (engines keep running)",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("service uninstall", pos, 0, 0); err != nil {
					return err
				}
				path := unitPath()
				if _, err := os.Stat(path); err != nil {
					return errs.New(errs.NotFound, "no service installed at %s", path)
				}
				_ = systemctl(e, "disable", "--now", "emx.service")
				if err := os.Remove(path); err != nil {
					return err
				}
				_ = systemctl(e, "daemon-reload")
				return e.out(map[string]any{"removed": path}, func(w io.Writer) { fmt.Fprintf(w, "removed %s\n", path) })
			}
		}}
}

func serviceStatusCmd() *command {
	return &command{name: "status", summary: "show the service state",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("service status", pos, 0, 0); err != nil {
					return err
				}
				out, _ := exec.CommandContext(e.ctx, "systemctl", "--user", "show", "emx.service",
					"--property=LoadState,ActiveState,SubState,MainPID,ExecMainStartTimestamp").Output()
				props := map[string]string{"unit": unitPath()}
				for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
					if k, v, ok := strings.Cut(l, "="); ok {
						props[k] = v
					}
				}
				return e.out(props, func(w io.Writer) {
					fmt.Fprintf(w, "%s: %s/%s (pid %s, since %s)\n", props["unit"], dash(props["ActiveState"]), dash(props["SubState"]),
						dash(props["MainPID"]), dash(props["ExecMainStartTimestamp"]))
				})
			}
		}}
}

// lingering reports whether systemd keeps this user's services running without a login session.
func lingering(e *env) bool {
	out, err := exec.CommandContext(e.ctx, "loginctl", "show-user", strconv.Itoa(os.Getuid()), "-p", "Linger", "--value").Output()
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}

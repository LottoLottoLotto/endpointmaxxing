package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lotto/substrate/internal/errs"
	"github.com/lotto/substrate/internal/keys"
	"github.com/lotto/substrate/internal/paths"
)

// ---- key ----

func cmdKey() *command {
	return &command{name: "key", summary: "manage per-client gateway API keys (roles, model access, quotas)",
		sub: []*command{keyAddCmd(), keyListCmd(), keyRmCmd()}}
}

func keyAddCmd() *command {
	return &command{name: "add", synopsis: "<name> [--role user|admin] [--models a,b*] [--rpm N] [--tokens-per-day N]",
		summary: "create a key (printed once)",
		build: func(fs *flag.FlagSet) action {
			role := fs.String("role", keys.RoleUser, "user (inference, own usage) or admin (everything)")
			models := fs.String("models", "", "comma-separated model names or globs the key may use (default all)")
			rpm := fs.Int("rpm", 0, "requests per minute (0 unlimited)")
			tpd := fs.Int64("tokens-per-day", 0, "prompt+completion tokens per calendar day (0 unlimited)")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("key add", pos, 1, 1); err != nil {
					return err
				}
				if *rpm < 0 || *tpd < 0 {
					return errs.New(errs.Usage, "key add: --rpm and --tokens-per-day must be >= 0")
				}
				k := keys.Key{Name: pos[0], Role: *role, RPM: *rpm, TokensPerDay: *tpd}
				for _, m := range strings.Split(*models, ",") {
					if m = strings.TrimSpace(m); m != "" {
						k.Models = append(k.Models, m)
					}
				}
				secret, err := keys.Add(k)
				if err != nil {
					if strings.Contains(err.Error(), "exists") {
						return errs.Wrap(errs.Conflict, err)
					}
					return errs.Wrap(errs.Usage, err)
				}
				return e.out(map[string]any{"name": k.Name, "role": k.Role, "key": secret, "models": k.Models, "rpm": k.RPM,
					"tokens_per_day": k.TokensPerDay}, func(w io.Writer) {
					fmt.Fprintf(w, "%s\n", secret)
					fmt.Fprintf(e.stderr, "key %s (%s) created; it is shown only now. Clients send it as Authorization: Bearer <key> or x-api-key.\n", k.Name, k.Role)
				})
			}
		}}
}

func keyListCmd() *command {
	return &command{name: "list", summary: "list keys (secrets are never shown)",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("key list", pos, 0, 0); err != nil {
					return err
				}
				f, err := keys.Load()
				if err != nil {
					return err
				}
				if f.Keys == nil {
					f.Keys = []keys.Key{}
				}
				return e.out(f.Keys, func(w io.Writer) {
					if len(f.Keys) == 0 {
						fmt.Fprintln(w, "no keys: the gateway is open on loopback (substrate key add <name>)")
						return
					}
					rows := make([][]string, len(f.Keys))
					for i, k := range f.Keys {
						rows[i] = []string{k.Name, k.Role, k.Prefix + "...", dash(strings.Join(k.Models, ",")), limit(int64(k.RPM)),
							limit(k.TokensPerDay), k.Created.Local().Format(time.DateOnly)}
					}
					e.table("NAME\tROLE\tKEY\tMODELS\tRPM\tTOKENS/DAY\tCREATED", rows)
				})
			}
		}}
}

func limit(n int64) string {
	if n == 0 {
		return "-"
	}
	return fmt.Sprint(n)
}

func keyRmCmd() *command {
	return &command{name: "rm", synopsis: "<name>...", summary: "revoke keys (takes effect on the next request)",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("key rm", pos, 1, -1); err != nil {
					return err
				}
				for _, n := range pos {
					ok, err := keys.Remove(n)
					if err != nil {
						return err
					}
					if !ok {
						return errs.New(errs.NotFound, "no key %q", n)
					}
				}
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
	return filepath.Join(dir, "systemd", "user", "substrate.service")
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
Description=substrate inference gateway
After=network-online.target

[Service]
Type=simple
ExecStart=%s serve%s
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
		summary: "write ~/.config/systemd/user/substrate.service (PATH captured from this shell)", passthrough: true,
		build: func(fs *flag.FlagSet) action {
			enable := fs.Bool("enable", false, "also daemon-reload, enable and start it now")
			var envs multi
			fs.Var(&envs, "env", "extra Environment= line, e.g. SUBSTRATE_API_KEY=... (repeatable)")
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
				if h := os.Getenv("SUBSTRATE_HOME"); h != "" {
					vars = append(vars, "SUBSTRATE_HOME="+h)
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
					if err := systemctl(e, "enable", "--now", "substrate.service"); err != nil {
						return err
					}
				}
				return e.out(map[string]any{"unit": path, "enabled": *enable}, func(w io.Writer) {
					fmt.Fprintf(w, "wrote %s\n", path)
					if !*enable {
						fmt.Fprintln(w, "start it: systemctl --user daemon-reload && systemctl --user enable --now substrate")
					}
					fmt.Fprintln(w, "keep it running after logout: loginctl enable-linger $USER")
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
				_ = systemctl(e, "disable", "--now", "substrate.service")
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
				out, _ := exec.CommandContext(e.ctx, "systemctl", "--user", "show", "substrate.service",
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

// Package cli implements the substrate command line and the MCP server that
// exposes the same commands as tools.
package cli

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/LottoLottoLotto/substrate/internal/app"
	"github.com/LottoLottoLotto/substrate/internal/errs"
	"github.com/LottoLottoLotto/substrate/internal/spec"
)

// Version is set at build time with -ldflags "-X github.com/LottoLottoLotto/substrate/internal/cli.Version=...".
var Version = "dev"

// env is what every command receives.
type env struct {
	ctx  context.Context
	json bool
	// csv prints tables as CSV (header row, then one row per line) for spreadsheets and finance tools.
	csv    bool
	stdout io.Writer
	stderr io.Writer
	// stdin is nil when commands must not read it (MCP).
	stdin io.Reader
	// owner tags instances this invocation starts ("cli" or "mcp").
	owner string

	a *app.App
}

// app opens the substrate setup once per invocation.
func (e *env) app() (*app.App, error) {
	if e.a != nil {
		return e.a, nil
	}
	a, err := app.Open()
	if err != nil {
		return nil, err
	}
	a.Warn = e.warn
	for _, err := range a.Engines.Errors {
		e.warn("skipping manifest: %v", err)
	}
	e.a = a
	return a, nil
}

func (e *env) close() {
	if e.a != nil {
		e.a.Close()
	}
}

// out prints v as JSON in --json mode, else calls human.
func (e *env) out(v any, human func(w io.Writer)) error {
	if e.json || human == nil {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	human(plain{e.stdout})
	return nil
}

// plain is where human-readable output goes: what substrate prints includes text other people chose (model
// names and paths of requests, client names, user agents, error messages, names inside model files), and a
// terminal would obey control sequences in it (clear the screen, rewrite earlier lines). Control characters
// other than newline and tab are shown as \xNN instead.
type plain struct{ w io.Writer }

func (p plain) Write(b []byte) (int, error) {
	clean := true
	for _, c := range b {
		if c < 0x20 && c != '\n' && c != '\t' || c == 0x7f {
			clean = false
			break
		}
	}
	// U+0080..U+009F (C1 controls, among them a one-character CSI) are the bytes C2 80..9F in UTF-8.
	if clean && !bytes.Contains(b, []byte{0xc2}) {
		return p.w.Write(b)
	}
	out := make([]byte, 0, len(b)+16)
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch {
		case c < 0x20 && c != '\n' && c != '\t' || c == 0x7f:
			out = fmt.Appendf(out, `\x%02x`, c)
		case c == 0xc2 && i+1 < len(b) && b[i+1] >= 0x80 && b[i+1] <= 0x9f:
			out = fmt.Appendf(out, `\u%04x`, b[i+1])
			i++
		default:
			out = append(out, c)
		}
	}
	if _, err := p.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

// csvCell keeps a spreadsheet from running a cell as a formula: text that starts like one gets a leading
// apostrophe. Numbers (also negative ones) stay as they are.
func csvCell(s string) string {
	if s == "" || !strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return s
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return s
	}
	return "'" + s
}

func (e *env) table(header string, rows [][]string) {
	if e.csv {
		cw := csv.NewWriter(e.stdout)
		cw.Write(strings.Split(header, "\t"))
		for _, r := range rows {
			cells := make([]string, len(r))
			for i, c := range r {
				cells[i] = csvCell(c)
			}
			cw.Write(cells)
		}
		cw.Flush()
		return
	}
	tw := tabwriter.NewWriter(plain{e.stdout}, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, header)
	oneLine := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ") // a cell cannot start a row or a column
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = oneLine.Replace(c)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	tw.Flush()
}

// warn writes to stderr (never pollutes --json stdout).
func (e *env) warn(f string, a ...any) { fmt.Fprintf(e.stderr, "substrate: "+f+"\n", a...) }

// progress writes human progress lines to stderr, suppressed in --json mode.
func (e *env) progress(f string, a ...any) {
	if !e.json {
		fmt.Fprintf(e.stderr, f+"\n", a...)
	}
}

// action runs a command after flag parsing.
type action func(e *env, pos, extra []string) error

// command is one CLI command (or a group of subcommands).
type command struct {
	name     string
	synopsis string // arguments after the command name
	summary  string
	// build registers the command's flags and returns its action; nil for groups.
	build func(fs *flag.FlagSet) action
	sub   []*command
	// tool exposes the command over MCP.
	tool bool
	// host marks a command whose inputs choose programs this host runs (an engine manifest, an engine's
	// binary, environment and arguments). MCP offers it only with `substrate mcp --allow-host`.
	host bool
	// passthrough means args after -- are engine arguments.
	passthrough bool
	alias       bool
}

func commands() []*command {
	return []*command{
		cmdRun(), cmdEnsure(), cmdPs(), cmdStop(), cmdLogs(), cmdHealth(), cmdChat(),
		cmdServe(), cmdMCP(),
		cmdRoute(), cmdModel(), aliasOf("models", "list registered models", modelListCmd()),
		cmdEngine(), aliasOf("engines", "list engines", engineListCmd()),
		cmdUsage(), cmdRequests(), cmdReport(), cmdFlagged(), cmdErrorStats(), cmdPolicy(), cmdEvents(), cmdSamples(), cmdPrune(), cmdAudit(),
		cmdKey(), cmdTeam(), cmdPrice(), cmdAlias(), cmdReload(), cmdBackup(), cmdBench(), cmdService(),
		cmdFlags(), cmdDoctor(), cmdVersion(), cmdCommands(),
	}
}

// aliasOf exposes target under a top-level name; aliases stay out of the schema.
func aliasOf(name, summary string, target *command) *command {
	return &command{name: name, synopsis: target.synopsis, summary: summary, build: target.build, alias: true}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "substrate: one CLI and one endpoint for every inference engine.\n\nUsage: substrate [--json | --csv] <command> [args]\n\nCommands:\n")
	for _, c := range commands() {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
		for _, s := range c.sub {
			fmt.Fprintf(w, "    %-14s %s\n", s.name, s.summary)
		}
	}
	fmt.Fprintf(w, `
--json (anywhere before --) prints machine-readable output; errors become {"error","code"}.
--csv prints tables as CSV (usage, requests, report, team list, key list, flagged, errors, ...).
Exit codes: 0 ok, 1 error, 2 usage, 3 not_found, 4 timeout, 5 engine_failed, 6 conflict, 7 unsupported.
'substrate commands --json' describes every command and flag; 'substrate mcp' serves them as MCP tools.
`)
}

// resolve finds the command addressed by args and returns it with the remaining args.
func resolve(args []string) (*command, []string, []string, error) {
	cs := commands()
	var path []string
	var c *command
	for len(args) > 0 {
		var next *command
		for _, x := range cs {
			if x.name == args[0] {
				next = x
			}
		}
		if next == nil {
			break
		}
		c, path, args, cs = next, append(path, next.name), args[1:], next.sub
		if c.build != nil {
			break
		}
	}
	if c == nil {
		if len(args) == 0 {
			return nil, nil, nil, errs.New(errs.Usage, "no command")
		}
		return nil, nil, nil, errs.New(errs.Usage, "unknown command %q", args[0])
	}
	if c.build == nil {
		names := make([]string, len(c.sub))
		for i, s := range c.sub {
			names[i] = s.name
		}
		if len(args) == 0 {
			return nil, nil, nil, errs.New(errs.Usage, "%s: want a subcommand (%s)", c.name, strings.Join(names, ", "))
		}
		return nil, nil, nil, errs.New(errs.Usage, "%s: unknown subcommand %q (%s)", c.name, args[0], strings.Join(names, ", "))
	}
	return c, path, args, nil
}

// execute runs one command line in e and returns its error.
func execute(e *env, args []string) error {
	c, path, rest, err := resolve(args)
	if err != nil {
		return err
	}
	fs := newFlags(strings.Join(path, " "), c.synopsis)
	run := c.build(fs)
	pos, extra, err := parse(fs, rest, e.stdout)
	if err != nil {
		return err
	}
	if len(extra) > 0 && !c.passthrough {
		return errs.New(errs.Usage, "%s takes no engine arguments after --", strings.Join(path, " "))
	}
	return run(e, pos, extra)
}

// Main runs the CLI and returns the exit code.
func Main(args []string) int {
	e := &env{ctx: context.Background(), stdout: os.Stdout, stderr: os.Stderr, stdin: os.Stdin, owner: "cli"}
	defer e.close()
	var rest []string
	for i, a := range args {
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if a == "--json" || a == "-json" {
			e.json = true
			continue
		}
		if a == "--csv" || a == "-csv" {
			e.csv = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) == 0 || rest[0] == "-h" || rest[0] == "--help" || rest[0] == "help" {
		usage(e.stdout)
		return 0
	}
	err := execute(e, rest)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	code := errs.CodeOf(err)
	if e.json {
		json.NewEncoder(e.stdout).Encode(map[string]string{"error": err.Error(), "code": string(code)})
	} else {
		e.warn("%v", err)
		if code == errs.Usage {
			fmt.Fprintln(e.stderr, "run 'substrate help' or 'substrate <command> -h'")
		}
	}
	return errs.Exit[code]
}

// parse parses flags interspersed with positionals; args after "--" are returned as passthrough.
func parse(fs *flag.FlagSet, args []string, help io.Writer) (pos, passthrough []string, err error) {
	for i, a := range args {
		if a == "--" {
			passthrough = args[i+1:]
			args = args[:i]
			break
		}
	}
	fs.SetOutput(io.Discard)
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fs.SetOutput(help)
				fs.Usage()
				return nil, nil, flag.ErrHelp
			}
			return nil, nil, errs.New(errs.Usage, "%s: %v", fs.Name(), err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, passthrough, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func newFlags(name, synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: substrate %s %s\n\n", name, synopsis)
		fs.PrintDefaults()
	}
	return fs
}

func wantArgs(name string, pos []string, min, max int) error {
	if len(pos) < min || (max >= 0 && len(pos) > max) {
		switch {
		case min == max:
			return errs.New(errs.Usage, "%s: want %d argument(s), got %d", name, min, len(pos))
		case max < 0:
			return errs.New(errs.Usage, "%s: want at least %d argument(s)", name, min)
		}
		return errs.New(errs.Usage, "%s: want %d to %d arguments, got %d", name, min, max, len(pos))
	}
	return nil
}

// multi is a repeatable string flag.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

// unifiedValue collects one unified flag into a shared map.
type unifiedValue struct {
	f   spec.UnifiedFlag
	dst map[string]string
}

func (u unifiedValue) String() string { return "" }
func (u unifiedValue) Set(v string) error {
	n, err := u.f.Normalize(v)
	if err != nil {
		return err
	}
	u.dst[u.f.Key] = n
	return nil
}
func (u unifiedValue) IsBoolFlag() bool { return u.f.Type == spec.TypeBool }

// addUnified registers every unified flag on fs, collecting into dst by manifest key.
func addUnified(fs *flag.FlagSet, dst map[string]string) {
	for _, f := range spec.Unified {
		fs.Var(unifiedValue{f, dst}, f.CLI, fmt.Sprintf("[unified %s] %s", f.Type, f.Help))
	}
}

func kv(pairs []string, what string) (map[string]string, error) {
	out := map[string]string{}
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, errs.New(errs.Usage, "%s %q: want KEY=VALUE", what, p)
		}
		out[k] = v
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func human(n int64) string {
	if n <= 0 {
		return "-"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// ---- schema ----

// FlagSchema describes one flag for agents.
type FlagSchema struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // boolean, integer, number, string, duration, string[]
	Default string `json:"default,omitempty"`
	Usage   string `json:"usage"`
	// HostOnly marks an input that chooses what this host runs or which of its files are read; MCP offers
	// it only with `substrate mcp --allow-host`.
	HostOnly bool `json:"host_only,omitempty"`
}

// hostFlags are the flags, on whichever command has them, whose value chooses what this host runs or which
// of its files are read: manifest placeholders and container arguments, and paths of files to read.
// Unified flags carry their own mark (spec.UnifiedFlag.HostOnly).
var hostFlags = map[string]bool{"var": true, "pre-arg": true, "prompt-file": true, "hardware": true, "file": true, "key-file": true}

func hostOnlyFlag(f *flag.Flag) bool {
	if u, ok := f.Value.(unifiedValue); ok {
		return u.f.HostOnly
	}
	return hostFlags[f.Name]
}

// CommandSchema describes one runnable command.
type CommandSchema struct {
	Command    string       `json:"command"`
	Synopsis   string       `json:"synopsis"`
	Summary    string       `json:"summary"`
	Flags      []FlagSchema `json:"flags"`
	EngineArgs bool         `json:"engine_args"` // accepts engine arguments after --
	// HostOnly marks a command whose inputs choose programs this host runs.
	HostOnly    bool   `json:"host_only,omitempty"`
	MCPTool     string `json:"mcp_tool,omitempty"`
	exampleArgs []string
}

func flagType(f *flag.Flag) string {
	switch v := f.Value.(type) {
	case unifiedValue:
		switch v.f.Type {
		case spec.TypeBool:
			return "boolean"
		case spec.TypeInt:
			return "integer"
		case spec.TypeFloat:
			return "number"
		}
		return "string"
	case *multi:
		return "string[]"
	}
	switch fmt.Sprintf("%T", f.Value) {
	case "*flag.boolValue":
		return "boolean"
	case "*flag.intValue", "*flag.int64Value":
		return "integer"
	case "*flag.float64Value":
		return "number"
	case "*flag.durationValue":
		return "duration"
	}
	return "string"
}

func toolName(path string) string { return "substrate_" + strings.ReplaceAll(path, " ", "_") }

// schemas describes every runnable command (aliases excluded).
func schemas() []CommandSchema {
	var out []CommandSchema
	var walk func(prefix string, cs []*command)
	walk = func(prefix string, cs []*command) {
		for _, c := range cs {
			path := strings.TrimSpace(prefix + " " + c.name)
			if c.build == nil {
				walk(path, c.sub)
				continue
			}
			if c.alias {
				continue // aliases
			}
			fs := newFlags(path, c.synopsis)
			c.build(fs)
			s := CommandSchema{Command: path, Synopsis: c.synopsis, Summary: c.summary, Flags: []FlagSchema{}, EngineArgs: c.passthrough, HostOnly: c.host}
			fs.VisitAll(func(f *flag.Flag) {
				s.Flags = append(s.Flags, FlagSchema{Name: f.Name, Type: flagType(f), Default: f.DefValue, Usage: f.Usage, HostOnly: hostOnlyFlag(f)})
			})
			if c.tool {
				s.MCPTool = toolName(path)
			}
			out = append(out, s)
		}
	}
	walk("", commands())
	return out
}

func cmdCommands() *command {
	return &command{name: "commands", summary: "describe every command and flag (use --json)",
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("commands", pos, 0, 0); err != nil {
					return err
				}
				s := schemas()
				return e.out(map[string]any{"version": Version, "spec": spec.Version, "exit_codes": errs.Exit, "commands": s},
					func(w io.Writer) {
						for _, c := range s {
							fmt.Fprintf(w, "substrate %s %s\n    %s\n", c.Command, c.Synopsis, c.Summary)
						}
					})
			}
		}}
}

func cmdVersion() *command {
	return &command{name: "version", summary: "print the version", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, _, _ []string) error {
				return e.out(map[string]string{"version": Version, "spec": spec.Version}, func(w io.Writer) {
					fmt.Fprintf(w, "substrate %s (%s)\n", Version, spec.Version)
				})
			}
		}}
}

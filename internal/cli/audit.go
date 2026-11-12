package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/LottoLottoLotto/substrate/internal/audit"
	"github.com/LottoLottoLotto/substrate/internal/config"
	"github.com/LottoLottoLotto/substrate/internal/errs"
)

func cmdAudit() *command {
	return &command{name: "audit", summary: "the hash-chained audit file (logging.audit_file)", sub: []*command{auditVerifyCmd()}}
}

// auditKey reads the audit key from file, by default the configured gateway.audit_key_file (nil when
// neither is set). A file that is missing, accessible by group or others, or too short is an error.
func auditKey(c *config.Config, file string) ([]byte, error) {
	if file == "" {
		file = audit.KeyPath(c.Gateway.AuditKeyFile)
	}
	key, err := audit.LoadKey(file)
	return key, errs.Wrap(errs.Usage, err)
}

func auditVerifyCmd() *command {
	return &command{name: "verify", synopsis: "[--key-file F] [--expect-head HASH [--expect-count N]]",
		summary: "check that no audit line was changed, inserted, removed or reordered and that the file still ends at its recorded head (exit 6 otherwise)", tool: true,
		build: func(fs *flag.FlagSet) action {
			keyFile := fs.String("key-file", "", "file holding the audit key of a keyed chain, mode 0600 (default: config gateway.audit_key_file)")
			expectHead := fs.String("expect-head", "", "head hash printed by an earlier verify and kept elsewhere: fail unless the chain still contains that line (nothing up to it was removed or rewritten)")
			expectCount := fs.Int64("expect-count", 0, "head count printed with that hash: fail unless the line has this number; without --expect-head, fail if the chain ends before it")
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("audit verify", pos, 0, 0); err != nil {
					return err
				}
				if *expectCount < 0 {
					return errs.New(errs.Usage, "audit verify: --expect-count must be >= 0")
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				p := audit.Path(a.Config().Logging.AuditFile)
				if p == "" {
					return errs.New(errs.Usage, "audit verify: logging.audit_file is not set")
				}
				key, err := auditKey(a.Config(), *keyFile)
				if err != nil {
					return err
				}
				r, err := audit.Verify(p, audit.Check{Key: key, ExpectHead: strings.ToLower(strings.TrimSpace(*expectHead)), ExpectCount: *expectCount})
				if err != nil {
					return err
				}
				if err := e.out(r, func(w io.Writer) {
					fmt.Fprintf(w, "%d line(s) in %d file(s)", r.Lines, len(r.Files))
					if r.Unchained > 0 {
						fmt.Fprintf(w, "; the first %d predate chaining", r.Unchained)
					}
					if r.Anchored {
						fmt.Fprint(w, "; older files were pruned, the oldest retained line anchors the chain")
					}
					fmt.Fprintln(w)
					for _, warning := range r.Warnings {
						fmt.Fprintf(w, "warning: %s\n", warning)
					}
					if !r.OK {
						return
					}
					if r.Keyed {
						fmt.Fprintln(w, "chain intact (keyed)")
					} else {
						fmt.Fprintln(w, "chain intact")
					}
					if *expectHead != "" {
						fmt.Fprintf(w, "expected head: count %d, %d line(s) written since\n", r.ExpectedAt, r.Head.Count-r.ExpectedAt)
					}
					if r.Lines > 0 {
						fmt.Fprintf(w, "head: count %d hash %s\n", r.Head.Count, r.Head.Hash)
					}
				}); err != nil {
					return err
				}
				if !r.OK {
					return errs.New(errs.Conflict, "audit chain broken: %s", r.Problem)
				}
				return nil
			}
		}}
}

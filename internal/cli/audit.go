package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/lotto/substrate/internal/audit"
	"github.com/lotto/substrate/internal/errs"
)

func cmdAudit() *command {
	return &command{name: "audit", summary: "the hash-chained audit file (logging.audit_file)", sub: []*command{auditVerifyCmd()}}
}

func auditVerifyCmd() *command {
	return &command{name: "verify", summary: "check that no audit line was changed, inserted, removed or reordered (exit 6 if one was)", tool: true,
		build: func(fs *flag.FlagSet) action {
			return func(e *env, pos, _ []string) error {
				if err := wantArgs("audit verify", pos, 0, 0); err != nil {
					return err
				}
				a, err := e.app()
				if err != nil {
					return err
				}
				p := audit.Path(a.Config().Logging.AuditFile)
				if p == "" {
					return errs.New(errs.Usage, "audit verify: logging.audit_file is not set")
				}
				r, err := audit.Verify(p)
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
					if r.OK {
						fmt.Fprintln(w, "chain intact")
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

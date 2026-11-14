package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/audit"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/paths"
)

// audit verify reports the chain's head, holds the chain to a head kept elsewhere (--expect-head,
// --expect-count) and reads the audit key from gateway.audit_key_file or --key-file.
func TestAuditVerifyHeadAndKey(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	t.Setenv("EMX_ACTOR", "carol")
	keyFile := filepath.Join(t.TempDir(), "audit.key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("k", audit.MinKeyBytes)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Update(func(c *config.Config) error {
		c.Logging.AuditFile, c.Gateway.AuditKeyFile = "audit.jsonl", keyFile
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	e := &env{stdout: &out, stderr: io.Discard, json: true, owner: "cli"}
	defer e.close()
	if err := execute(e, []string{"alias", "set", "chat", "m"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := execute(e, []string{"audit", "verify"}); err != nil {
		t.Fatal(err)
	}
	var r audit.Report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatalf("%v: %s", err, out.Bytes())
	}
	if !r.OK || !r.Keyed || r.Head.Count != 1 || r.Head.Hash == "" {
		t.Fatalf("report %+v", r)
	}
	if err := execute(e, []string{"audit", "verify", "--expect-head", r.Head.Hash, "--expect-count", "1"}); err != nil {
		t.Fatalf("the head just reported: %v", err)
	}
	if err := execute(e, []string{"audit", "verify", "--expect-head", strings.Repeat("0", 64)}); errs.CodeOf(err) != errs.Conflict {
		t.Fatalf("a head the chain does not contain: %v", err)
	}
	if err := execute(e, []string{"audit", "verify", "--expect-count", "2"}); errs.CodeOf(err) != errs.Conflict {
		t.Fatalf("a count the chain does not reach: %v", err)
	}

	// Another key does not verify the chain; a key file others can read is not used at all.
	other := filepath.Join(t.TempDir(), "other.key")
	if err := os.WriteFile(other, []byte(strings.Repeat("x", audit.MinKeyBytes)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := execute(e, []string{"audit", "verify", "--key-file", other}); errs.CodeOf(err) != errs.Conflict {
		t.Fatalf("another key: %v", err)
	}
	if paths.Windows {
		return // access is not in the mode bits there
	}
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := execute(e, []string{"audit", "verify"}); errs.CodeOf(err) != errs.Usage {
		t.Fatalf("key file readable by others: %v", err)
	}
}

package runtime

import (
	"bytes"
	"os"
	"strings"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/paths"
)

// Engines write what they like to their logs, and some print their own arguments and settings, the API key
// among them (SGLang does). The log is private to the owner, but its lines travel: `emx logs` and its MCP
// tool show them, and the tail of a failed start is put into the error a client receives and into the
// event records auditors read. So secrets are replaced wherever log text leaves the file, and in the file
// itself once the engine has stopped and no longer writes to it.

// secretsOf lists the values to hide: the engine's API key and the values of secret-looking environment
// variables (HF_TOKEN, ...). Very short values are left alone: replacing them would garble the text.
func secretsOf(apiKey string, env map[string]string) []string {
	var out []string
	if len(apiKey) >= 4 {
		out = append(out, apiKey)
	}
	for name, v := range env {
		if len(v) >= 6 && secretName(name) {
			out = append(out, v)
		}
	}
	return out
}

func scrub(text, apiKey string, env map[string]string) string {
	for _, s := range secretsOf(apiKey, env) {
		text = strings.ReplaceAll(text, s, "***")
	}
	return text
}

// Scrub replaces the instance's secrets in text taken from its log.
func (in *Instance) Scrub(text string) string { return scrub(text, in.APIKey, in.Env) }

// maxScrubLog bounds the log that is rewritten in memory.
const maxScrubLog = 512 << 20

// scrubLog replaces the instance's secrets in its log file. Called once the engine has stopped: nothing
// writes to the file any more, and afterwards the record that knows the secrets is gone.
func (in *Instance) scrubLog() {
	secrets := secretsOf(in.APIKey, in.Env)
	st, err := os.Stat(in.Log)
	if len(secrets) == 0 || err != nil || st.Size() > maxScrubLog {
		return
	}
	data, err := os.ReadFile(in.Log)
	if err != nil {
		return
	}
	clean := data
	for _, s := range secrets {
		clean = bytes.ReplaceAll(clean, []byte(s), []byte("***"))
	}
	if bytes.Equal(clean, data) {
		return
	}
	tmp := in.Log + ".tmp"
	if os.WriteFile(tmp, clean, 0o600) == nil {
		if paths.Replace(tmp, in.Log) != nil {
			os.Remove(tmp)
		}
	}
}

// Redacted returns a copy of the plan safe to print: the API key and the values of secret-looking options
// and environment variables are masked. With allEnv every environment value is (for output that leaves the
// operator's terminal).
func (p *Plan) Redacted(allEnv bool) *Plan {
	c := *p
	c.Argv = redactArgv(Redact(p.Argv, p.apiKey))
	if len(p.Env) > 0 {
		c.Env = make(map[string]string, len(p.Env))
		for k, v := range p.Env {
			if allEnv || secretName(k) || (p.apiKey != "" && v == p.apiKey) {
				v = "***"
			}
			c.Env[k] = v
		}
	}
	return &c
}

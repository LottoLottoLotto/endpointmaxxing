package spec

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

// Signals are the stop signals a manifest may name.
var Signals = map[string]syscall.Signal{
	"SIGTERM": syscall.SIGTERM,
	"SIGINT":  syscall.SIGINT,
	"SIGQUIT": syscall.SIGQUIT,
	"SIGHUP":  syscall.SIGHUP,
}

// Expand replaces every {key} in s with vars[key]; {env.NAME} is the
// environment variable NAME (empty when unset). An unknown key is an error.
func Expand(s string, vars map[string]string) (string, error) {
	var err error
	out := placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		k := m[1 : len(m)-1]
		if name, ok := strings.CutPrefix(k, "env."); ok {
			return os.Getenv(name)
		}
		v, ok := vars[k]
		if !ok && err == nil {
			err = fmt.Errorf("unknown placeholder %s in %q", m, s)
		}
		return v
	})
	return out, err
}

// Placeholders lists the keys referenced in s ({name} -> name), excluding {env.*}.
func Placeholders(s string) []string {
	var out []string
	for _, m := range placeholderRe.FindAllStringSubmatch(s, -1) {
		if !strings.HasPrefix(m[1], "env.") {
			out = append(out, m[1])
		}
	}
	return out
}

// ExpandAll expands every element of xs.
func ExpandAll(xs []string, vars map[string]string) ([]string, error) {
	out := make([]string, len(xs))
	for i, x := range xs {
		v, err := Expand(x, vars)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

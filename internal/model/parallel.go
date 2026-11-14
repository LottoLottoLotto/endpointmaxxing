package model

import (
	"encoding/json"
	"os"
	"strings"
)

// DeclaredParallel returns how many requests the model at source is configured to serve at the same time,
// when its own files decide that: a Strata config serves one request at a time unless it sets "parallel".
// known is false for every other kind of model, where the engine's launch flags decide.
func DeclaredParallel(source string) (n int, known bool) {
	if !strings.HasSuffix(source, ".json") {
		return 0, false
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return 0, false
	}
	var c struct {
		Exe      string   `json:"exe"`
		Args     []string `json:"args"`
		Parallel any      `json:"parallel"`
	}
	if json.Unmarshal(data, &c) != nil || c.Exe == "" || len(c.Args) == 0 {
		return 0, false // not a Strata config
	}
	if p, ok := c.Parallel.(float64); ok && p >= 2 && p == float64(int(p)) {
		return int(p), true
	}
	return 1, true
}

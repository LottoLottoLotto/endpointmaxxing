// Package route ranks the engines able to serve a model.
package route

import (
	"fmt"
	"sort"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/errs"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/spec"
)

// Model is what routing needs to know about a model.
type Model struct {
	Format string
	Quant  string
	// Engine pins the model to one engine.
	Engine string
}

// Candidate is one engine's verdict for a model.
type Candidate struct {
	Engine     string   `json:"engine"`
	Compatible bool     `json:"compatible"`
	Installed  bool     `json:"installed"`
	Score      int      `json:"score"`
	Reasons    []string `json:"reasons,omitempty"`
}

// Rank scores every engine for m; the best usable candidate comes first.
// installed reports whether an engine's binary resolves. prefer maps a format
// to engine ids in preference order (config.yaml `prefer`).
func Rank(m Model, engines []*spec.Manifest, installed func(id string) bool, prefer map[string][]string) []Candidate {
	out := make([]Candidate, 0, len(engines))
	for _, e := range engines {
		c := Candidate{Engine: e.ID, Compatible: true, Installed: installed(e.ID)}
		if m.Engine != "" && m.Engine != e.ID {
			c.Compatible = false
			c.Reasons = append(c.Reasons, "model is pinned to "+m.Engine)
		}
		if !e.HasFormat(m.Format) {
			c.Compatible = false
			c.Reasons = append(c.Reasons, fmt.Sprintf("does not load %s", m.Format))
		}
		// GGUF file types are self-describing to every GGUF engine; quantization
		// lists only constrain checkpoint quantization methods.
		if m.Format == "safetensors" && !e.AcceptsQuant(m.Quant) {
			c.Compatible = false
			c.Reasons = append(c.Reasons, fmt.Sprintf("does not support %s quantization", m.Quant))
		}
		if !c.Installed {
			c.Reasons = append(c.Reasons, "not installed")
		}
		c.Score = e.Routing.Priority[m.Format]
		for i, id := range prefer[m.Format] {
			if id == e.ID {
				c.Score = 1000 - i
				c.Reasons = append(c.Reasons, fmt.Sprintf("preferred #%d for %s", i+1, m.Format))
			}
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.usable() != b.usable() {
			return a.usable()
		}
		if a.Compatible != b.Compatible {
			return a.Compatible
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.Engine < b.Engine
	})
	return out
}

func (c Candidate) usable() bool { return c.Compatible && c.Installed }

// Pick returns the best usable engine, or an error naming why none is.
func Pick(cands []Candidate) (string, error) {
	if len(cands) > 0 && cands[0].usable() {
		return cands[0].Engine, nil
	}
	var compat []string
	for _, c := range cands {
		if c.Compatible {
			compat = append(compat, c.Engine)
		}
	}
	if len(compat) > 0 {
		return "", errs.New(errs.NotFound, "no compatible engine is installed (compatible: %v; see `emx engine install`)", compat)
	}
	return "", errs.New(errs.Unsupported, "no engine can serve this model (see `emx route` for reasons)")
}

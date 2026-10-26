package config

import (
	"math"
	"testing"
)

func TestPricingLookupAndCost(t *testing.T) {
	half := 0.5
	p := Pricing{
		Default: &Price{Prompt: 1, Completion: 1},
		Models: map[string]Price{
			"qwen*":          {Prompt: 2, Completion: 4},
			"qwen3.8-27b*":   {Prompt: 3, Completion: 6, Cached: &half},
			"qwen3.8-27b-tt": {Prompt: 10, Completion: 20},
		},
	}
	n := func(v int64) *int64 { return &v }
	cases := []struct {
		model                     string
		prompt, completion, cache *int64
		want                      float64
		priced                    bool
	}{
		{"qwen3.8-27b-tt", n(1_000_000), n(1_000_000), nil, 30, true},             // exact name beats globs
		{"qwen3.8-27b-8u", n(1_000_000), n(0), n(400_000), 0.6*3 + 0.4*0.5, true}, // longest glob; cached at its own price
		{"qwen3-0.6b", n(500_000), n(0), n(500_000), 1, true},                     // no cached price: cached at the prompt price
		{"llama", n(1_000_000), n(1_000_000), nil, 2, true},                       // default
		{"llama", nil, nil, nil, 0, false},                                        // nothing metered
	}
	for _, c := range cases {
		got, ok := p.Cost(c.model, c.prompt, c.completion, c.cache)
		if ok != c.priced || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s: cost %v priced %v, want %v %v", c.model, got, ok, c.want, c.priced)
		}
	}
	if _, ok := (&Pricing{}).Cost("x", n(1), n(1), nil); ok {
		t.Error("a model without a price (and no default) was costed")
	}
}

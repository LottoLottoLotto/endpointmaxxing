package spec

import (
	"fmt"
	"strconv"
	"strings"
)

// FlagType is the value type of a unified flag.
type FlagType string

const (
	TypeInt    FlagType = "int"
	TypeFloat  FlagType = "float"
	TypeString FlagType = "string"
	TypeBool   FlagType = "bool"
)

// UnifiedFlag is one engine-independent launch option. Manifests translate it
// into engine arguments and environment through their `flags` table.
type UnifiedFlag struct {
	Key  string   `json:"key"`  // manifest key, e.g. gpu_layers
	CLI  string   `json:"cli"`  // CLI flag, e.g. gpu-layers
	Type FlagType `json:"type"` //
	Help string   `json:"help"`
}

// Unified is the complete unified flag vocabulary, in display order.
var Unified = []UnifiedFlag{
	{"ctx", "ctx", TypeInt, "context length in tokens"},
	{"gpus", "gpus", TypeString, "GPU indices to use, comma separated (as nvidia-smi numbers them)"},
	{"tp", "tp", TypeInt, "tensor-parallel degree"},
	{"gpu_layers", "gpu-layers", TypeString, "layers to offload to the GPU (number or \"all\")"},
	{"parallel", "parallel", TypeInt, "concurrent sequences / server slots"},
	{"mem_fraction", "mem-fraction", TypeFloat, "fraction of GPU memory the engine may claim (0-1)"},
	{"kv_cache_type", "kv-cache", TypeString, "KV cache type: f16, bf16, fp8, q8_0, q4_0 (engine-specific values pass through)"},
	{"quantization", "quant", TypeString, "weight quantization method to force (engine-specific names)"},
	{"dtype", "dtype", TypeString, "activation/weight dtype: auto, float16, bfloat16"},
	{"api_key", "api-key", TypeString, "API key the engine requires from clients"},
	{"trust_remote_code", "trust-remote-code", TypeBool, "allow model repositories to run custom code"},
	{"flash_attn", "flash-attn", TypeBool, "force flash attention on"},
	{"embeddings", "embeddings", TypeBool, "serve the model as an embedding model"},
}

// FlagByKey returns the unified flag with the manifest key k.
func FlagByKey(k string) (UnifiedFlag, bool) {
	for _, f := range Unified {
		if f.Key == k {
			return f, true
		}
	}
	return UnifiedFlag{}, false
}

// FlagByCLI returns the unified flag with the CLI name n (without dashes).
func FlagByCLI(n string) (UnifiedFlag, bool) {
	for _, f := range Unified {
		if f.CLI == n {
			return f, true
		}
	}
	return UnifiedFlag{}, false
}

// Normalize checks v against the flag's type and returns its canonical text.
func (f UnifiedFlag) Normalize(v string) (string, error) {
	v = strings.TrimSpace(v)
	switch f.Type {
	case TypeInt:
		if _, err := strconv.Atoi(v); err != nil {
			return "", fmt.Errorf("--%s: %q is not an integer", f.CLI, v)
		}
	case TypeFloat:
		x, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return "", fmt.Errorf("--%s: %q is not a number", f.CLI, v)
		}
		if f.Key == "mem_fraction" && (x <= 0 || x > 1) {
			return "", fmt.Errorf("--%s: %v is outside (0, 1]", f.CLI, x)
		}
	case TypeBool:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return "", fmt.Errorf("--%s: %q is not true/false", f.CLI, v)
		}
		return strconv.FormatBool(b), nil
	case TypeString:
		if v == "" {
			return "", fmt.Errorf("--%s: empty value", f.CLI)
		}
	}
	return v, nil
}

// Package model identifies a model's format, architecture and quantization
// from its files, so routing can match it against engine capabilities.
package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/LottoLottoLotto/substrate/internal/paths"
)

// Info is what substrate knows about a model source.
type Info struct {
	Source string `json:"source"`
	// Path is the local file or directory engines load ("" for an uncached HF repo).
	Path string `json:"path,omitempty"`
	// Ref is what path-or-repo engines (vLLM, SGLang) are given: Path, or the repo id.
	Ref    string `json:"ref"`
	Format string `json:"format"`
	Arch   string `json:"arch,omitempty"`
	// Quant is the GGUF file type (Q4_K_M, ...) or the HF quantization method (fp8, gptq, ...).
	Quant string `json:"quant,omitempty"`
	Bytes int64  `json:"bytes,omitempty"`
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// MissingLocal reports whether source names a local file or directory that does not exist (a Hugging Face
// repo id is never missing here: it downloads on first use).
func MissingLocal(source string) bool {
	_, err := os.Stat(paths.Expand(source))
	return os.IsNotExist(err) && !(repoRe.MatchString(source) && !strings.HasPrefix(source, "."))
}

// Inspect identifies source: a .gguf file, a directory of GGUF shards or of a
// Hugging Face checkpoint, a Strata config, or a Hugging Face repo id.
func Inspect(source string) (*Info, error) {
	info, err := inspect(source)
	if info != nil {
		// These come out of the model's own files and are shown in terminals and stored in models.yaml.
		info.Arch, info.Quant = label(info.Arch), label(info.Quant)
	}
	return info, err
}

// label makes text taken from a model file safe to print and store: printable characters only, at most 64.
func label(s string) string {
	out := make([]rune, 0, min(len(s), 64))
	for _, r := range s {
		if len(out) == 64 {
			break
		}
		if unicode.IsPrint(r) {
			out = append(out, r)
		}
	}
	return string(out)
}

func inspect(source string) (*Info, error) {
	p := paths.Expand(source)
	st, err := os.Stat(p)
	if err != nil {
		if os.IsNotExist(err) && repoRe.MatchString(source) && !strings.HasPrefix(source, ".") {
			return inspectRepo(source)
		}
		// Name the source as the caller wrote it: the expanded path may hold values of environment variables.
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return nil, fmt.Errorf("%s: %w", source, pe.Err)
		}
		return nil, err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		return inspectDir(source, abs)
	}
	switch {
	case strings.HasSuffix(strings.ToLower(abs), ".gguf"):
		return inspectGGUF(source, abs)
	case strings.HasSuffix(abs, ".json"):
		return inspectStrata(source, abs)
	}
	return nil, fmt.Errorf("%s: unrecognized model file (want .gguf, a checkpoint directory, a strata-*.json config or an org/name repo id)", source)
}

func inspectGGUF(source, path string) (*Info, error) {
	h, err := ReadGGUFHeader(path)
	if err != nil {
		return nil, err
	}
	info := &Info{Source: source, Path: path, Ref: path, Format: "gguf", Arch: h.Arch, Quant: h.FileType}
	// Split models: the first shard is what engines load; size counts all shards.
	if m := shardRe.FindStringSubmatch(path); m != nil {
		shards, _ := filepath.Glob(m[1] + "-*-of-" + m[2] + ".gguf")
		for _, s := range shards {
			info.Bytes += fileSize(s)
		}
	} else {
		info.Bytes = fileSize(path)
	}
	return info, nil
}

var shardRe = regexp.MustCompile(`^(.*)-\d{5}-of-(\d{5})\.gguf$`)

func inspectDir(source, dir string) (*Info, error) {
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		return inspectCheckpoint(source, dir, dir)
	}
	ggufs, _ := filepath.Glob(filepath.Join(dir, "*.gguf"))
	var main []string
	for _, g := range ggufs {
		base := strings.ToLower(filepath.Base(g))
		// Vision projectors and MTP/draft heads ride along with the main model.
		if strings.Contains(base, "mmproj") || strings.Contains(base, "mtp-only") {
			continue
		}
		if m := shardRe.FindStringSubmatch(g); m != nil && !strings.HasSuffix(g, "-00001-of-"+m[2]+".gguf") {
			continue
		}
		main = append(main, g)
	}
	sort.Strings(main)
	switch len(main) {
	case 0:
		return nil, fmt.Errorf("%s: no config.json and no .gguf model in directory", source)
	case 1:
		info, err := inspectGGUF(source, main[0])
		if info != nil {
			info.Source = source
		}
		return info, err
	}
	names := make([]string, len(main))
	for i, g := range main {
		names[i] = filepath.Base(g)
	}
	return nil, fmt.Errorf("%s: several GGUF models (%s); pass the file", source, strings.Join(names, ", "))
}

// hfConfig is the part of a Hugging Face config.json routing needs.
type hfConfig struct {
	Architectures []string `json:"architectures"`
	ModelType     string   `json:"model_type"`
	Quant         *struct {
		Method string `json:"quant_method"`
	} `json:"quantization_config"`
	// MLX checkpoints describe quantization as {"bits", "group_size"} under "quantization".
	MLXQuant *struct {
		Bits int `json:"bits"`
	} `json:"quantization"`
}

func inspectCheckpoint(source, dir, ref string) (*Info, error) {
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, err
	}
	var c hfConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s/config.json: %w", dir, err)
	}
	info := &Info{Source: source, Path: dir, Ref: ref, Format: "safetensors"}
	if len(c.Architectures) > 0 {
		info.Arch = c.Architectures[0]
	} else {
		info.Arch = c.ModelType
	}
	switch {
	case c.Quant != nil && c.Quant.Method != "":
		info.Quant = strings.ToLower(c.Quant.Method)
	case c.MLXQuant != nil:
		info.Quant = "mlx"
	default:
		q, err := os.ReadFile(filepath.Join(dir, "quantize_config.json"))
		if err != nil {
			break
		}
		// AutoGPTQ checkpoints predating quantization_config.
		var qc struct {
			Method string `json:"quant_method"`
		}
		if json.Unmarshal(q, &qc) == nil && qc.Method != "" {
			info.Quant = strings.ToLower(qc.Method)
		} else {
			info.Quant = "gptq"
		}
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.safetensors"))
	if len(files) == 0 {
		bins, _ := filepath.Glob(filepath.Join(dir, "*.bin"))
		files = bins
	}
	for _, f := range files {
		info.Bytes += fileSize(f)
	}
	return info, nil
}

// inspectRepo resolves a Hugging Face repo id from the local HF cache when present.
func inspectRepo(repo string) (*Info, error) {
	hub := os.Getenv("HF_HUB_CACHE")
	if hub == "" {
		hfHome := os.Getenv("HF_HOME")
		if hfHome == "" {
			hfHome = filepath.Join(paths.Home(), ".cache", "huggingface")
		}
		hub = filepath.Join(hfHome, "hub")
	}
	base := filepath.Join(hub, "models--"+strings.ReplaceAll(repo, "/", "--"))
	if ref, err := os.ReadFile(filepath.Join(base, "refs", "main")); err == nil {
		snap := filepath.Join(base, "snapshots", strings.TrimSpace(string(ref)))
		if _, err := os.Stat(filepath.Join(snap, "config.json")); err == nil {
			// Engines resolve the repo id against the same cache themselves.
			return inspectCheckpoint(repo, snap, repo)
		}
	}
	// Not downloaded: the engine fetches it; format is the HF default.
	return &Info{Source: repo, Ref: repo, Format: "safetensors"}, nil
}

// strataConfig is the part of a Strata strata-<model>.json routing needs.
type strataConfig struct {
	Exe       string   `json:"exe"`
	Args      []string `json:"args"`
	ModelName string   `json:"model_name"`
}

func inspectStrata(source, path string) (*Info, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c strataConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if c.Exe == "" || len(c.Args) == 0 {
		return nil, fmt.Errorf("%s: JSON is not a Strata config (no exe/args)", source)
	}
	info := &Info{Source: source, Path: path, Ref: path, Format: "strata-config", Arch: "qwen3.8-flash-next"}
	if i := strings.LastIndex(c.ModelName, "-"); i > 0 {
		info.Quant = strings.ToUpper(c.ModelName[i+1:])
	}
	return info, nil
}

func fileSize(p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return st.Size()
}

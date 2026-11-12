// Package bench measures models substrate serves and publishes the results to LocalMaxxing. Measurement,
// verification evidence and upload are lmx's job (the LocalMaxxing CLI); this package works out what lmx
// needs to know about the model, engine and hardware from substrate's registry and running instances.
package bench

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/LottoLottoLotto/substrate/internal/app"
	"github.com/LottoLottoLotto/substrate/internal/config"
	"github.com/LottoLottoLotto/substrate/internal/gpu"
	"github.com/LottoLottoLotto/substrate/internal/runtime"
)

// Meta is everything a submission says about the setup, with where each value came from.
type Meta struct {
	Model            string            `json:"model"`
	HFID             string            `json:"hf_id"`
	Revision         string            `json:"revision,omitempty"`
	Quantization     string            `json:"quantization"`
	EngineID         string            `json:"engine_id"`
	EngineName       string            `json:"engine_name"`
	EngineVersion    string            `json:"engine_version,omitempty"`
	EngineRepository string            `json:"engine_repository,omitempty"`
	EngineCommit     string            `json:"engine_commit,omitempty"`
	Backend          string            `json:"backend,omitempty"`
	ContextLength    int               `json:"context_length,omitempty"`
	Spec             *config.BenchSpec `json:"spec,omitempty"`
	Hardware         map[string]any    `json:"hardware"`
	Notes            string            `json:"notes,omitempty"`
	// Sources maps a field to "config" (bench metadata), "registry", "detected" or "engine".
	Sources map[string]string `json:"sources"`
	// Problems are missing or doubtful values; a submission with any is likely refused or unverifiable.
	Problems []string `json:"problems,omitempty"`
}

// lmxEngineNames maps substrate engine ids to LocalMaxxing engine names.
var lmxEngineNames = map[string]string{
	"llama.cpp":      "llama.cpp",
	"buun-llama-cpp": "llama.cpp",
	"vllm":           "vllm",
	"tt-vllm":        "vllm",
	"sglang":         "sglang",
	"strata":         "strata",
}

var hfRepoRe = regexp.MustCompile(`^[A-Za-z0-9][\w.-]*/[\w.-]+$`)

// Resolve works out a model's submission metadata: bench metadata from models.yaml first, then the registry,
// then what the engine and the running instance reveal.
func Resolve(a *app.App, m config.Model, in *runtime.Instance) *Meta {
	b := config.BenchMeta{}
	if m.Bench != nil {
		b = *m.Bench
	}
	meta := &Meta{Model: m.Name, EngineID: in.Engine, Sources: map[string]string{}}
	pick := func(field, configured, fallback, fallbackSource string) string {
		if configured != "" {
			meta.Sources[field] = "config"
			return configured
		}
		if fallback != "" {
			meta.Sources[field] = fallbackSource
		}
		return fallback
	}
	hf := ""
	if hfRepoRe.MatchString(m.Source) && !strings.HasPrefix(m.Source, ".") {
		if _, err := os.Stat(m.Source); err != nil { // a repo id, not a relative path
			hf = m.Source
		}
	}
	meta.HFID = pick("hf_id", b.HFID, hf, "registry")
	meta.Revision = b.Revision
	meta.Quantization = pick("quantization", b.Quantization, m.Quant, "registry")

	name := lmxEngineNames[in.Engine]
	if name == "" {
		name = "custom"
	}
	meta.EngineName = pick("engine_name", b.EngineName, name, "detected")
	var version, repo, commit string
	if en, err := a.Engine(in.Engine); err == nil && en.Installed() {
		if in.Engine != "strata" { // lmx reads a Strata server's version from /props
			version = en.Version()
		}
		repo, commit = gitProvenance(filepath.Dir(en.Bin))
	}
	meta.EngineVersion = pick("engine_version", b.EngineVersion, version, "engine")
	meta.EngineRepository = pick("engine_repository", b.EngineRepository, repo, "detected")
	meta.EngineCommit = pick("engine_commit", b.EngineCommit, commit, "detected")

	backend := ""
	switch {
	case in.Accelerator == "tenstorrent" || len(in.Devices) > 0:
		backend = "tt-metal"
	case len(in.GPUs) > 0:
		backend = "cuda"
	}
	meta.Backend = pick("backend", b.Backend, backend, "detected")
	meta.ContextLength = b.ContextLength
	if meta.ContextLength > 0 {
		meta.Sources["context_length"] = "config"
	}
	meta.Spec, meta.Notes = b.Spec, b.Notes

	if b.Hardware != "" {
		data, err := os.ReadFile(b.Hardware)
		if err == nil {
			err = json.Unmarshal(data, &meta.Hardware)
		}
		if err != nil {
			meta.Problems = append(meta.Problems, fmt.Sprintf("bench.hardware %s: %v", b.Hardware, err))
		}
		meta.Sources["hardware"] = "config"
	} else {
		hw, problems := DetectHardware(in)
		meta.Hardware, meta.Problems = hw, append(meta.Problems, problems...)
		meta.Sources["hardware"] = "detected"
	}

	if meta.HFID == "" {
		meta.Problems = append(meta.Problems, fmt.Sprintf("no Hugging Face id: substrate bench config %s --hf-id org/repo", m.Name))
	}
	if meta.Quantization == "" {
		meta.Problems = append(meta.Problems, fmt.Sprintf("no quantization label: substrate bench config %s --quantization Q4_K_M", m.Name))
	}
	if meta.EngineCommit != "" && meta.Sources["engine_commit"] == "detected" && meta.EngineRepository == "" {
		meta.Problems = append(meta.Problems, "the engine checkout's commit is on no remote branch: publish it or set --engine-commit/--engine-repository")
	}
	return meta
}

// gitProvenance finds the git checkout holding dir and returns the remote repository URL whose branches
// contain HEAD, and HEAD itself; repo is empty when HEAD is on no remote branch (unpublished).
func gitProvenance(dir string) (repo, commit string) {
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	commit = git("rev-parse", "HEAD")
	if commit == "" {
		return "", ""
	}
	for _, line := range strings.Split(git("branch", "-r", "--contains", "HEAD"), "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" || strings.Contains(ref, "->") {
			continue
		}
		remote, _, _ := strings.Cut(ref, "/")
		if url := git("remote", "get-url", remote); url != "" {
			return normalizeRepoURL(url), commit
		}
	}
	return "", commit
}

// normalizeRepoURL turns git@host:org/repo.git into https://host/org/repo.
func normalizeRepoURL(u string) string {
	if rest, ok := strings.CutPrefix(u, "git@"); ok {
		host, path, _ := strings.Cut(rest, ":")
		u = "https://" + host + "/" + path
	}
	return strings.TrimSuffix(u, ".git")
}

// ttBoards names Tenstorrent cards (sysfs tt_card_type) as LocalMaxxing does, with their memory in GB.
var ttBoards = map[string]struct {
	name string
	gb   float64
}{
	"p150a": {"Tenstorrent P150", 32}, "p150b": {"Tenstorrent P150", 32}, "p150c": {"Tenstorrent P150", 32},
	"p100a": {"Tenstorrent P100", 28}, "p300": {"Tenstorrent P300", 64},
	"n150": {"Tenstorrent N150", 12}, "n300": {"Tenstorrent N300", 24},
}

// DetectHardware describes the host and the accelerators in shape LocalMaxxing expects: hwClass, gpuName,
// gpuCount, vramGb, cpu, ramGb, os. Only the devices the instance uses are counted.
func DetectHardware(in *runtime.Instance) (map[string]any, []string) {
	var problems []string
	hw := map[string]any{"hwClass": "DISCRETE_GPU", "cpu": cpuName(), "os": osName()}
	if gb := ramGB(); gb > 0 {
		hw["ramGb"] = gb
	}
	switch {
	case in.Accelerator == "tenstorrent" || len(in.Devices) > 0:
		names := map[string]int{}
		var mem float64
		for _, chip := range in.Devices {
			raw, err := os.ReadFile(fmt.Sprintf("/sys/class/tenstorrent/tenstorrent!%d/tt_card_type", chip))
			if err != nil {
				problems = append(problems, fmt.Sprintf("Tenstorrent chip %d: board type unknown (%v)", chip, err))
				continue
			}
			typ := strings.TrimSpace(string(raw))
			b, ok := ttBoards[typ]
			if !ok {
				b.name = "Tenstorrent " + strings.ToUpper(typ)
				problems = append(problems, fmt.Sprintf("Tenstorrent board %q is not in substrate's table: check the name and memory", typ))
			}
			names[b.name]++
			mem += b.gb
		}
		setGPUs(hw, names, mem, &problems)
	case len(in.GPUs) > 0:
		gs, err := gpu.List()
		if err != nil {
			problems = append(problems, fmt.Sprintf("nvidia-smi: %v", err))
			break
		}
		names := map[string]int{}
		var mem float64
		for _, idx := range in.GPUs {
			for _, g := range gs {
				if g.Index == idx {
					names[g.Name]++
					mem += math.Round(float64(g.MemTotalMiB) / 1024)
				}
			}
		}
		setGPUs(hw, names, mem, &problems)
	default:
		hw["hwClass"] = "CPU"
		problems = append(problems, "the instance uses no GPU or Tenstorrent chip that substrate knows of: set bench.hardware")
	}
	return hw, problems
}

func setGPUs(hw map[string]any, names map[string]int, mem float64, problems *[]string) {
	total := 0
	for name, n := range names {
		hw["gpuName"] = name
		total += n
	}
	if len(names) > 1 {
		*problems = append(*problems, "mixed accelerator models: LocalMaxxing takes one gpuName; set bench.hardware")
	}
	if total > 0 {
		hw["gpuCount"] = total
		hw["vramGb"] = mem / float64(total)
	}
}

var coreSuffix = regexp.MustCompile(`\s+\d+-Core Processor$|\s+Processor$|\(R\)|\(TM\)|\s+CPU @.*$`)

func cpuName() string {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.Join(strings.Fields(coreSuffix.ReplaceAllString(strings.TrimSpace(v), "")), " ")
		}
	}
	return ""
}

// ramGB is installed memory as sold: MemTotal rounded up to a multiple of 8 GB (the kernel reserves some).
func ramGB() int {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var kb int64
		if _, err := fmt.Sscanf(sc.Text(), "MemTotal: %d kB", &kb); err == nil {
			gb := float64(kb) / (1 << 20)
			return int(math.Ceil(gb/8) * 8)
		}
	}
	return 0
}

func osName() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "Linux"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return "Linux"
}

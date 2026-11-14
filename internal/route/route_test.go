package route

import (
	"testing"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/spec"
)

func manifests(t *testing.T) []*spec.Manifest {
	ms, err := spec.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	return ms
}

func all(string) bool { return true }

func TestRankByPriority(t *testing.T) {
	ms := manifests(t)
	if got, _ := Pick(Rank(Model{Format: "safetensors"}, ms, all, nil)); got != "vllm" {
		t.Fatalf("safetensors -> %s", got)
	}
	if got, _ := Pick(Rank(Model{Format: "gguf"}, ms, all, nil)); got != "llama.cpp" {
		t.Fatalf("gguf -> %s", got)
	}
}

func TestPreferAndInstalled(t *testing.T) {
	ms := manifests(t)
	prefer := map[string][]string{"gguf": {"buun-llama-cpp"}}
	if got, _ := Pick(Rank(Model{Format: "gguf"}, ms, all, prefer)); got != "buun-llama-cpp" {
		t.Fatalf("preferred -> %s", got)
	}
	onlyLlama := func(id string) bool { return id == "llama.cpp" }
	if got, _ := Pick(Rank(Model{Format: "gguf"}, ms, onlyLlama, prefer)); got != "llama.cpp" {
		t.Fatalf("preferred engine not installed -> %s", got)
	}
}

func TestQuantAndPin(t *testing.T) {
	ms := manifests(t)
	// No serving engine takes an exotic checkpoint quantization; buun takes any.
	if got, _ := Pick(Rank(Model{Format: "safetensors", Quant: "exl3"}, ms, all, nil)); got != "buun-llama-cpp" {
		t.Fatalf("exl3 -> %s", got)
	}
	none := func(string) bool { return false }
	if _, err := Pick(Rank(Model{Format: "gguf"}, ms, none, nil)); err == nil {
		t.Fatal("picked an uninstalled engine")
	}
	if got, _ := Pick(Rank(Model{Format: "safetensors", Engine: "sglang"}, ms, all, nil)); got != "sglang" {
		t.Fatalf("pinned -> %s", got)
	}
	if _, err := Pick(Rank(Model{Format: "gguf", Engine: "strata"}, ms, all, nil)); err == nil {
		t.Fatal("pinned to an engine that cannot load the format")
	}
}

package model

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeGGUF writes a GGUF v3 header with the given metadata, in order.
func writeGGUF(t *testing.T, path string, kvs ...any) {
	t.Helper()
	var b bytes.Buffer
	le := func(v any) { binary.Write(&b, binary.LittleEndian, v) }
	str := func(s string) { le(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	le(uint32(3))
	le(uint64(0))
	le(uint64(len(kvs) / 2))
	for i := 0; i < len(kvs); i += 2 {
		str(kvs[i].(string))
		switch v := kvs[i+1].(type) {
		case string:
			le(uint32(gString))
			str(v)
		case uint32:
			le(uint32(gU32))
			le(v)
		case []string:
			le(uint32(gArray))
			le(uint32(gString))
			le(uint64(len(v)))
			for _, s := range v {
				str(s)
			}
		}
	}
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadGGUFHeaderSkipsArrays(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.gguf")
	writeGGUF(t, p,
		"tokenizer.ggml.tokens", []string{"a", "bb", "ccc"},
		"general.architecture", "qwen3",
		"general.file_type", uint32(15),
	)
	h, err := ReadGGUFHeader(p)
	if err != nil {
		t.Fatal(err)
	}
	if h.Arch != "qwen3" || h.FileType != "Q4_K_M" {
		t.Fatalf("got %+v", h)
	}
}

func TestReadGGUFRejectsNonGGUF(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.gguf")
	os.WriteFile(p, []byte("not a model"), 0o644)
	if _, err := ReadGGUFHeader(p); err == nil {
		t.Fatal("accepted non-GGUF file")
	}
}

func TestInspectDirPicksFirstShardAndSkipsProjector(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"m-00001-of-00002.gguf", "m-00002-of-00002.gguf", "mmproj-F16.gguf"} {
		writeGGUF(t, filepath.Join(d, n), "general.architecture", "llama", "general.file_type", uint32(7))
	}
	info, err := Inspect(d)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(info.Path) != "m-00001-of-00002.gguf" || info.Quant != "Q8_0" {
		t.Fatalf("got %+v", info)
	}
}

func TestInspectCheckpointQuant(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "config.json"),
		[]byte(`{"architectures":["Qwen3ForCausalLM"],"quantization_config":{"quant_method":"GPTQ"}}`), 0o644)
	info, err := Inspect(d)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "safetensors" || info.Arch != "Qwen3ForCausalLM" || info.Quant != "gptq" {
		t.Fatalf("got %+v", info)
	}
}

func TestInspectUncachedRepo(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	info, err := Inspect("org/model-x")
	if err != nil {
		t.Fatal(err)
	}
	if info.Path != "" || info.Ref != "org/model-x" || info.Format != "safetensors" {
		t.Fatalf("got %+v", info)
	}
}

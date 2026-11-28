package model

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSafetensors writes a safetensors container whose header metadata is meta (nil: none).
func writeSafetensors(t *testing.T, path string, meta map[string]string) {
	t.Helper()
	header := map[string]any{"t": map[string]any{"dtype": "U8", "shape": []int{4}, "data_offsets": []int{0, 4}}}
	if meta != nil {
		header["__metadata__"] = meta
	}
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	b := binary.LittleEndian.AppendUint64(nil, uint64(len(h)))
	b = append(append(b, h...), 1, 2, 3, 4)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInspectPackedFlareCore(t *testing.T) {
	p := filepath.Join(t.TempDir(), "qwen3.8-27b-tp1.safetensors")
	writeSafetensors(t, p, map[string]string{"format": "flare"})
	info, err := Inspect(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "flare-core" || info.Path != p || info.Ref != p || info.Bytes != fileSize(p) {
		t.Fatalf("got %+v", info)
	}
}

func TestInspectFlareCoreDirectory(t *testing.T) {
	d := t.TempDir()
	writeSafetensors(t, filepath.Join(d, "model.safetensors"), map[string]string{"format": "flare-payload"})
	writeSafetensors(t, filepath.Join(d, "weights", "layer-000.safetensors"), map[string]string{"format": "flare-payload"})
	writeSafetensors(t, filepath.Join(d, "deployments", "gb202-tp1", "rank-0", "graphs.safetensors"), map[string]string{"format": "flare-payload"})
	info, err := Inspect(d)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "flare-core" || info.Path != d || info.Bytes != 3*fileSize(filepath.Join(d, "model.safetensors")) {
		t.Fatalf("got %+v", info)
	}
}

// A lone .safetensors file that is not a Hyperflare core (a checkpoint shard) is refused with a pointer to
// the checkpoint directory, not routed to an engine that cannot load it.
func TestInspectLoneCheckpointShardIsRefused(t *testing.T) {
	d := t.TempDir()
	plain := filepath.Join(d, "model-00001-of-00002.safetensors")
	writeSafetensors(t, plain, map[string]string{"format": "pt"})
	if _, err := Inspect(plain); err == nil || !strings.Contains(err.Error(), "checkpoint directory") {
		t.Fatalf("got %v", err)
	}
	junk := filepath.Join(d, "junk.safetensors")
	os.WriteFile(junk, []byte("not a container"), 0o644)
	if _, err := Inspect(junk); err == nil || !strings.Contains(err.Error(), "not a safetensors file") {
		t.Fatalf("got %v", err)
	}
}

// A checkpoint directory stays a checkpoint even with a model.safetensors: config.json decides first.
func TestCheckpointWithModelSafetensorsIsNotACore(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"architectures":["Qwen3ForCausalLM"]}`), 0o644)
	writeSafetensors(t, filepath.Join(d, "model.safetensors"), map[string]string{"format": "pt"})
	info, err := Inspect(d)
	if err != nil {
		t.Fatal(err)
	}
	if info.Format != "safetensors" {
		t.Fatalf("got %+v", info)
	}
}

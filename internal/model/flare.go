package model

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A Hyperflare runtime core (https://github.com/NVIDIA/hyperflare-inference) is a prepared model: weights,
// compiled kernels and captured graphs, in safetensors containers whose header metadata names the format.
// It comes in two forms, both published on Hugging Face as is:
//
//   - packed: one .safetensors file, metadata format "flare";
//   - directory: model.safetensors (metadata format "flare-payload", holding the manifest) beside
//     weights/ and deployments/ files.
//
// Either is what `flare run` takes, so either is the model's path.

// flarePrimary is the file of a directory-form core that `flare run` is given the directory for.
const flarePrimary = "model.safetensors"

// maxSafetensorsHeader bounds the JSON header read from a .safetensors file (cores' are a few KiB).
const maxSafetensorsHeader = 64 << 20

// safetensorsFormat returns the "format" entry of a .safetensors file's header metadata ("" when it has
// none). err is set when the file is not a safetensors container.
func safetensorsFormat(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var n uint64
	if err := binary.Read(f, binary.LittleEndian, &n); err != nil {
		return "", fmt.Errorf("not a safetensors file: %w", err)
	}
	if n < 2 || n > maxSafetensorsHeader {
		return "", fmt.Errorf("not a safetensors file (header of %d bytes)", n)
	}
	header := make([]byte, n)
	if _, err := io.ReadFull(f, header); err != nil {
		return "", fmt.Errorf("not a safetensors file: %w", err)
	}
	var h struct {
		Metadata map[string]string `json:"__metadata__"`
	}
	if err := json.Unmarshal(header, &h); err != nil {
		return "", fmt.Errorf("not a safetensors file: %w", err)
	}
	return h.Metadata["format"], nil
}

// isFlareFormat reports whether a safetensors format entry is a Hyperflare core's.
func isFlareFormat(format string) bool {
	return format == "flare" || format == "flare-payload"
}

// isFlareCoreDir reports whether dir is a directory-form Hyperflare core.
func isFlareCoreDir(dir string) bool {
	format, err := safetensorsFormat(filepath.Join(dir, flarePrimary))
	return err == nil && isFlareFormat(format)
}

// inspectFlareFile identifies a lone .safetensors file: a packed Hyperflare core is the only one emx serves
// (a checkpoint is its directory, config.json included).
func inspectFlareFile(source, path string) (*Info, error) {
	format, err := safetensorsFormat(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if !isFlareFormat(format) {
		return nil, fmt.Errorf("%s: a single .safetensors file is not a model emx can serve; pass the checkpoint directory (with its config.json)", source)
	}
	return flareInfo(source, path, fileSize(path)), nil
}

// inspectFlareDir identifies a directory-form Hyperflare core; its size is that of all of its files.
func inspectFlareDir(source, dir string) (*Info, error) {
	var size int64
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && p != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".safetensors") {
			size += fileSize(p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return flareInfo(source, dir, size), nil
}

func flareInfo(source, path string, size int64) *Info {
	return &Info{Source: source, Path: path, Ref: path, Format: "flare-core", Bytes: size}
}

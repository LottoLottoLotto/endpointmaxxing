package model

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// DeclaresVision reports whether the checkpoint at source says it takes images: a Hugging Face checkpoint
// directory whose config.json has a vision section. known is false for anything else (a GGUF file takes
// images only with a separate projector, which the file does not say).
func DeclaresVision(source string) (images, known bool) {
	data, err := os.ReadFile(filepath.Join(source, "config.json"))
	if err != nil {
		return false, false
	}
	var cfg map[string]json.RawMessage
	if json.Unmarshal(data, &cfg) != nil {
		return false, false
	}
	if _, ok := cfg["vision_config"]; !ok {
		return false, true
	}
	// A checkpoint exported without its vision tower keeps the section and says so.
	return strings.TrimSpace(string(cfg["language_model_only"])) != "true", true
}

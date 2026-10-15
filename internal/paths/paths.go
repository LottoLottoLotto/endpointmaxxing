// Package paths resolves substrate's config, state and data directories.
//
// SUBSTRATE_HOME, when set, roots all three (config/, state/, data/) so a
// test or a second installation never touches the user's real files.
package paths

import (
	"os"
	"path/filepath"
	"strings"
)

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "/"
	}
	return h
}

func xdg(env, fallback, sub string) string {
	if root := os.Getenv("SUBSTRATE_HOME"); root != "" {
		return filepath.Join(root, sub)
	}
	if v := os.Getenv(env); v != "" {
		return filepath.Join(v, "substrate")
	}
	return filepath.Join(home(), fallback, "substrate")
}

// Config holds config.yaml, models.yaml and engines/*.yaml.
func Config() string { return xdg("XDG_CONFIG_HOME", ".config", "config") }

// State holds instances/*.json and logs/*.log.
func State() string { return xdg("XDG_STATE_HOME", ".local/state", "state") }

// Data holds engine installs created by `substrate engine install`.
func Data() string { return xdg("XDG_DATA_HOME", ".local/share", "data") }

func Home() string { return home() }

// Expand resolves a leading ~ and $VARS.
func Expand(p string) string {
	if p == "~" {
		return home()
	}
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(home(), p[2:])
	}
	return os.ExpandEnv(p)
}

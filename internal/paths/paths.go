// Package paths resolves substrate's config, state and data directories.
//
// SUBSTRATE_HOME, when set, roots all three (config/, state/, data/) so a
// test or a second installation never touches the user's real files.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
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

// Private creates each directory (parents included) readable by the owner only, and tightens ones that
// exist with wider permissions: they hold keys, prompts, request records and engine logs. Pass the
// substrate root (State or Config) before its subdirectories so the root is tightened too.
func Private(dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// OwnerOnly reports whether mode grants nothing to group or others. Windows does not express access in
// mode bits (files under the user profile are protected by its ACL), so there it is always true.
func OwnerOnly(mode os.FileMode) bool {
	return runtime.GOOS == "windows" || mode.Perm()&0o077 == 0
}

// Replace renames tmp over dst, the last step of an atomic write. On Windows a rename fails while another
// process has dst open (a reader, for the instant it reads); it is retried for up to a second.
func Replace(tmp, dst string) error {
	err := os.Rename(tmp, dst)
	for i := 0; err != nil && runtime.GOOS == "windows" && i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		err = os.Rename(tmp, dst)
	}
	return err
}

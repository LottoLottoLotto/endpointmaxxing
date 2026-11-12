package spec

import (
	"bytes"
	"embed"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/LottoLottoLotto/substrate/internal/paths"
)

// Builtin engines may ship files (e.g. the server a container runs) in assets/<id>/.
// The {assets} placeholder names the directory they are written to before each launch.
//
//go:embed all:assets
var assets embed.FS

// HasAssets reports whether builtin engine id ships files.
func HasAssets(id string) bool {
	_, err := fs.Stat(assets, path.Join("assets", id))
	return err == nil
}

// SyncAssets writes builtin engine id's files under dir, rewriting only files whose
// content changed (a running engine keeps reading unchanged files) and removing none.
func SyncAssets(id, dir string) error {
	root := path.Join("assets", id)
	return fs.WalkDir(assets, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		dst := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := assets.ReadFile(p)
		if err != nil {
			return err
		}
		if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, data) {
			return nil
		}
		// Files are world-readable: a container may run them as another user.
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return err
		}
		return paths.Replace(tmp, dst)
	})
}

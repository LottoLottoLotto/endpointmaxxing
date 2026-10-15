// Package keys stores per-client gateway API keys (config/keys.yaml, mode 0600).
// Only SHA-256 hashes are stored; a key is shown once, when created.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/lotto/substrate/internal/config"
	"github.com/lotto/substrate/internal/lock"
	"github.com/lotto/substrate/internal/paths"
	"gopkg.in/yaml.v3"
)

// Roles.
const (
	RoleUser  = "user"  // inference, /v1/models, its own usage
	RoleAdmin = "admin" // everything, including the management API and /metrics
)

// Key is one client.
type Key struct {
	Name string `yaml:"name" json:"name"`
	// Hash is hex SHA-256 of the secret.
	Hash string `yaml:"hash" json:"-"`
	// Prefix is the secret's first characters, for recognizing it.
	Prefix string `yaml:"prefix" json:"prefix"`
	Role   string `yaml:"role" json:"role"`
	// Models are path.Match globs of model names the key may use (empty = all).
	Models []string `yaml:"models,omitempty" json:"models,omitempty"`
	// RPM caps requests per minute (0 = unlimited).
	RPM int `yaml:"rpm,omitempty" json:"rpm,omitempty"`
	// TokensPerDay caps prompt+completion tokens per local calendar day (0 = unlimited).
	TokensPerDay int64     `yaml:"tokens_per_day,omitempty" json:"tokens_per_day,omitempty"`
	Created      time.Time `yaml:"created" json:"created"`
}

// Allows reports whether the key may use model.
func (k *Key) Allows(model string) bool {
	if len(k.Models) == 0 {
		return true
	}
	for _, g := range k.Models {
		if ok, _ := path.Match(g, model); ok {
			return true
		}
	}
	return false
}

// File is keys.yaml.
type File struct {
	Keys []Key `yaml:"keys"`
}

// Path is the key store.
func Path() string { return filepath.Join(paths.Config(), "keys.yaml") }

// Hash returns the stored form of a secret.
func Hash(secret string) string {
	s := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(s[:])
}

// Load reads keys.yaml (empty when absent).
func Load() (*File, error) {
	var f File
	data, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return &f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", Path(), err)
	}
	return &f, nil
}

func (f *File) save() error {
	sort.Slice(f.Keys, func(i, j int) bool { return f.Keys[i].Name < f.Keys[j].Name })
	data, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(Path()), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", Path(), os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path())
}

func lockPath() string { return filepath.Join(paths.Config(), ".keys.lock") }

// Add creates a key and returns its secret (the only time it is visible).
func Add(k Key) (string, error) {
	if !config.NameRe.MatchString(k.Name) {
		return "", fmt.Errorf("key name %q must match %s", k.Name, config.NameRe)
	}
	if k.Role != RoleUser && k.Role != RoleAdmin {
		return "", fmt.Errorf("role %q (want user or admin)", k.Role)
	}
	for _, g := range k.Models {
		if _, err := path.Match(g, ""); err != nil {
			return "", fmt.Errorf("model pattern %q: %w", g, err)
		}
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	secret := "sk-sub-" + hex.EncodeToString(b[:])
	k.Hash, k.Prefix, k.Created = Hash(secret), secret[:14], time.Now().UTC()
	err := lock.With(lockPath(), func() error {
		f, err := Load()
		if err != nil {
			return err
		}
		for _, x := range f.Keys {
			if x.Name == k.Name {
				return fmt.Errorf("key %s exists (remove it first)", k.Name)
			}
		}
		f.Keys = append(f.Keys, k)
		return f.save()
	})
	return secret, err
}

// Remove deletes the key called name; false when absent.
func Remove(name string) (bool, error) {
	found := false
	err := lock.With(lockPath(), func() error {
		f, err := Load()
		if err != nil {
			return err
		}
		for i := range f.Keys {
			if f.Keys[i].Name == name {
				f.Keys = append(f.Keys[:i], f.Keys[i+1:]...)
				found = true
				return f.save()
			}
		}
		return nil
	})
	return found, err
}

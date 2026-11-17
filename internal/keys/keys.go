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

	"github.com/LottoLottoLotto/endpointmaxxing/internal/config"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/lock"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/paths"
	"gopkg.in/yaml.v3"
)

// Roles.
const (
	RoleUser    = "user"    // inference, /v1/models, its own usage
	RoleAuditor = "auditor" // read-only: usage, requests, events, flags, errors, reports, keys and /metrics; no inference
	RoleAdmin   = "admin"   // everything, including the management API and /metrics
)

// Roles lists the valid roles.
var Roles = []string{RoleUser, RoleAuditor, RoleAdmin}

// Key is one client.
type Key struct {
	Name string `yaml:"name" json:"name"`
	// Hash is hex SHA-256 of the secret.
	Hash string `yaml:"hash" json:"-"`
	// Prefix is the secret's first characters, for recognizing it.
	Prefix string `yaml:"prefix" json:"prefix"`
	Role   string `yaml:"role" json:"role"`
	// Team attributes the key's usage to a team (config.yaml teams) and its budgets.
	Team string `yaml:"team,omitempty" json:"team,omitempty"`
	// Models are path.Match globs of model names the key may use (empty = all).
	Models []string `yaml:"models,omitempty" json:"models,omitempty"`
	// RPM caps requests per minute (0 = unlimited).
	RPM int `yaml:"rpm,omitempty" json:"rpm,omitempty"`
	// MaxOutputTokens caps each request's output tokens (0 = only gateway.max_output_tokens applies).
	MaxOutputTokens int `yaml:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
	// TokensPerDay caps prompt+completion tokens per local calendar day (0 = unlimited).
	TokensPerDay int64     `yaml:"tokens_per_day,omitempty" json:"tokens_per_day,omitempty"`
	Created      time.Time `yaml:"created" json:"created"`
	// Expires, when set, is when the key stops working.
	Expires *time.Time `yaml:"expires,omitempty" json:"expires,omitempty"`
	// Delegate lets the key act for clients of its own (the renters behind a rental host agent, the users
	// behind an app): a request may name one in X-Emx-Client. It is recorded as the request's sub-client and
	// gets its own prompt-cache domain (gateway.cache_isolation); the key's limits still cover them all.
	Delegate bool `yaml:"delegate,omitempty" json:"delegate,omitempty"`
	// CacheShared makes all of the key's requests (and the clients it acts for) one prompt-cache domain
	// under gateway.cache_isolation: for clients that trust each other, such as one team's private rental.
	CacheShared bool `yaml:"cache_shared,omitempty" json:"cache_shared,omitempty"`
	// Disabled keys are refused (kept for their history; enable to restore).
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	// Rotated is when the secret was last replaced; PrevHash keeps the old secret working until PrevExpires.
	Rotated     *time.Time `yaml:"rotated,omitempty" json:"rotated,omitempty"`
	PrevHash    string     `yaml:"prev_hash,omitempty" json:"-"`
	PrevExpires *time.Time `yaml:"prev_expires,omitempty" json:"prev_expires,omitempty"`
}

// Status is "active", "disabled" or "expired" at now.
func (k *Key) Status(now time.Time) string {
	switch {
	case k.Disabled:
		return "disabled"
	case k.Expires != nil && !now.Before(*k.Expires):
		return "expired"
	}
	return "active"
}

// Matches reports whether hash is the key's secret, or its previous secret within the rotation grace period.
func (k *Key) Matches(hash string, now time.Time) bool {
	if hash == k.Hash {
		return true
	}
	return k.PrevHash != "" && hash == k.PrevHash && k.PrevExpires != nil && now.Before(*k.PrevExpires)
}

// Validate checks the name, role and model patterns.
func (k *Key) Validate() error {
	if !config.NameRe.MatchString(k.Name) {
		return fmt.Errorf("key name %q must match %s", k.Name, config.NameRe)
	}
	valid := false
	for _, r := range Roles {
		valid = valid || k.Role == r
	}
	if !valid {
		return fmt.Errorf("role %q (want user, auditor or admin)", k.Role)
	}
	if k.Team != "" && !config.NameRe.MatchString(k.Team) {
		return fmt.Errorf("team name %q must match %s", k.Team, config.NameRe)
	}
	if k.RPM < 0 || k.TokensPerDay < 0 || k.MaxOutputTokens < 0 {
		return fmt.Errorf("rpm, tokens_per_day and max_output_tokens must be >= 0")
	}
	for _, g := range k.Models {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("model pattern %q: %w", g, err)
		}
	}
	return nil
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
	if err := paths.Private(filepath.Dir(Path())); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", Path(), os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return paths.Replace(tmp, Path())
}

func lockPath() string { return filepath.Join(paths.Config(), ".keys.lock") }

func newSecret() (secret, hash, prefix string, err error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", "", err
	}
	secret = "sk-emx-" + hex.EncodeToString(b[:])
	return secret, Hash(secret), secret[:14], nil
}

// Add creates a key and returns its secret (the only time it is visible).
func Add(k Key) (string, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	secret, hash, prefix, err := newSecret()
	if err != nil {
		return "", err
	}
	k.Hash, k.Prefix, k.Created = hash, prefix, time.Now().UTC()
	err = lock.With(lockPath(), func() error {
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

// ErrNotFound reports an unknown key name.
var ErrNotFound = errors.New("no such key")

// Update applies fn to the key called name and saves it (fn's error aborts).
func Update(name string, fn func(*Key) error) (Key, error) {
	var out Key
	err := lock.With(lockPath(), func() error {
		f, err := Load()
		if err != nil {
			return err
		}
		for i := range f.Keys {
			if f.Keys[i].Name != name {
				continue
			}
			if err := fn(&f.Keys[i]); err != nil {
				return err
			}
			if err := f.Keys[i].Validate(); err != nil {
				return err
			}
			out = f.Keys[i]
			return f.save()
		}
		return fmt.Errorf("key %s: %w", name, ErrNotFound)
	})
	return out, err
}

// Rotate gives the key a new secret and returns it. The old secret keeps working for grace (0 = revoked now).
func Rotate(name string, grace time.Duration) (string, Key, error) {
	secret, hash, prefix, err := newSecret()
	if err != nil {
		return "", Key{}, err
	}
	k, err := Update(name, func(k *Key) error {
		now := time.Now().UTC()
		k.PrevHash, k.PrevExpires = "", nil
		if grace > 0 {
			until := now.Add(grace)
			k.PrevHash, k.PrevExpires = k.Hash, &until
		}
		k.Hash, k.Prefix, k.Rotated = hash, prefix, &now
		return nil
	})
	return secret, k, err
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

// Replace makes data (a keys.yaml received from the mesh machine that holds the keys) the key store, after
// checking every entry. Only secret hashes travel: the file holds no secret.
func Replace(data []byte) error {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("keys from the mesh: %w", err)
	}
	for i := range f.Keys {
		if err := f.Keys[i].Validate(); err != nil {
			return fmt.Errorf("keys from the mesh: %w", err)
		}
		if _, err := hex.DecodeString(f.Keys[i].Hash); err != nil || len(f.Keys[i].Hash) != 64 {
			return fmt.Errorf("keys from the mesh: key %s has no valid secret hash", f.Keys[i].Name)
		}
	}
	return lock.With(lockPath(), f.save)
}

// Raw returns keys.yaml as stored (nil when absent).
func Raw() ([]byte, error) {
	data, err := os.ReadFile(Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return data, err
}

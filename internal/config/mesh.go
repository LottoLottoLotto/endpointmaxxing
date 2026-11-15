package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/LottoLottoLotto/endpointmaxxing/internal/paths"
	"github.com/LottoLottoLotto/endpointmaxxing/internal/spec"
)

// Mesh joins this gateway with gateways on other machines into one endpoint: a client may send any request
// to any of them, and it is served by whichever machine has the model, started there when it is stopped.
// Machines prove they belong with a shared secret (HMAC-SHA256 of every call between them), and one of them
// holds the API keys, teams, prices and aliases; the others copy them from it.
type Mesh struct {
	// Node is this machine's name in the mesh (shown on requests and models it serves).
	Node string `yaml:"node,omitempty" json:"node,omitempty"`
	// SecretFile holds the secret every machine of the mesh shares: at least 32 bytes, owner-only (relative
	// paths are under the config directory).
	SecretFile string `yaml:"secret_file,omitempty" json:"secret_file,omitempty"`
	// Peers are the other machines.
	Peers []MeshPeer `yaml:"peers,omitempty" json:"peers,omitempty"`
	// KeysFrom names the machine that holds the API keys, teams, prices and aliases: this node's own name
	// (or empty) makes it that machine; a peer's name makes this one copy them from that peer.
	KeysFrom string `yaml:"keys_from,omitempty" json:"keys_from,omitempty"`
	// CAFile verifies peers' HTTPS certificates (PEM) besides the system's roots: for a private CA.
	CAFile string `yaml:"ca_file,omitempty" json:"ca_file,omitempty"`
	// Interval is how often each peer is asked what it serves (default 5s).
	Interval spec.Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
}

// MeshPeer is another machine of the mesh.
type MeshPeer struct {
	Name string `yaml:"name" json:"name"`
	// URL is the peer's gateway (https://host:7777).
	URL string `yaml:"url" json:"url"`
}

// Enabled reports whether this gateway is part of a mesh.
func (m Mesh) Enabled() bool { return len(m.Peers) > 0 }

// Follower reports whether this machine copies keys, teams, prices and aliases from a peer.
func (m Mesh) Follower() bool { return m.Enabled() && m.KeysFrom != "" && m.KeysFrom != m.Node }

// Every is the peer polling interval.
func (m Mesh) Every() time.Duration {
	if m.Interval > 0 {
		return time.Duration(m.Interval)
	}
	return 5 * time.Second
}

// Secret resolves SecretFile.
func (m Mesh) Secret() string {
	if m.SecretFile != "" && !filepath.IsAbs(m.SecretFile) {
		return filepath.Join(paths.Config(), m.SecretFile)
	}
	return m.SecretFile
}

// Peer returns the peer called name.
func (m Mesh) Peer(name string) (MeshPeer, bool) {
	for _, p := range m.Peers {
		if p.Name == name {
			return p, true
		}
	}
	return MeshPeer{}, false
}

func (m *Mesh) validate() error {
	if !m.Enabled() {
		return nil
	}
	if !NameRe.MatchString(m.Node) {
		return fmt.Errorf("mesh.node %q must be set and match %s", m.Node, NameRe)
	}
	if m.SecretFile == "" {
		return errors.New("mesh.secret_file must be set: a file of at least 32 random bytes every machine of the mesh shares (openssl rand -hex 32)")
	}
	seen := map[string]bool{m.Node: true}
	for i, p := range m.Peers {
		if !NameRe.MatchString(p.Name) {
			return fmt.Errorf("mesh.peers[%d]: name %q must match %s", i, p.Name, NameRe)
		}
		if seen[p.Name] {
			return fmt.Errorf("mesh.peers[%d]: name %q is used twice (or is this node's name)", i, p.Name)
		}
		seen[p.Name] = true
		u, err := url.Parse(p.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
			return fmt.Errorf("mesh.peers[%d]: url %q must be http(s)://host:port of the peer's gateway", i, p.URL)
		}
	}
	if m.KeysFrom != "" && !seen[m.KeysFrom] {
		return fmt.Errorf("mesh.keys_from %q is neither this node (%s) nor one of its peers", m.KeysFrom, m.Node)
	}
	if m.Interval < 0 {
		return errors.New("mesh.interval must be positive")
	}
	return nil
}

// Shared is what a mesh follower copies from the machine that holds keys (keys.yaml travels as it is).
type Shared struct {
	Teams   []Team            `yaml:"teams,omitempty" json:"teams,omitempty"`
	Pricing Pricing           `yaml:"pricing,omitempty" json:"pricing,omitempty"`
	Aliases map[string]string `yaml:"aliases,omitempty" json:"aliases,omitempty"`
}

// SharedPath is a follower's copy of the shared settings.
func SharedPath() string { return filepath.Join(paths.Config(), "mesh-shared.yaml") }

// SharedOf returns the shared part of c.
func SharedOf(c *Config) Shared {
	return Shared{Teams: c.Teams, Pricing: c.Pricing, Aliases: c.Aliases}
}

// Validate checks a copy received from the mesh before it is saved.
func (s *Shared) Validate() error {
	c := Default()
	c.Teams, c.Pricing, c.Aliases = s.Teams, s.Pricing, s.Aliases
	for _, check := range []func() error{c.Pricing.validate, func() error { return validateTeams(c.Teams) },
		func() error { return validateAliases(c.Aliases) }} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// SaveShared writes a follower's copy.
func SaveShared(s Shared) error {
	if err := s.Validate(); err != nil {
		return err
	}
	return writeYAML(SharedPath(), s)
}

// overlayShared replaces a follower's teams, prices and aliases with its copy (none until the first one).
func (c *Config) overlayShared() error {
	if !c.Mesh.Follower() {
		return nil
	}
	c.Teams, c.Pricing, c.Aliases = nil, Pricing{}, nil
	data, err := os.ReadFile(SharedPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var s Shared
	if err := decodeStrict(SharedPath(), data, &s); err != nil {
		return err
	}
	c.Teams, c.Pricing, c.Aliases = s.Teams, s.Pricing, s.Aliases
	return nil
}

// ErrManagedElsewhere refuses a change to keys, teams, prices or aliases on a mesh follower.
type ErrManagedElsewhere struct{ Node string }

func (e ErrManagedElsewhere) Error() string {
	return fmt.Sprintf("API keys, teams, prices and aliases of this mesh are managed on %s (mesh.keys_from): make the change there", e.Node)
}

// ManagedElsewhere returns ErrManagedElsewhere on a mesh follower.
func ManagedElsewhere() error {
	c, err := loadRaw()
	if err != nil {
		return err
	}
	if c.Mesh.Follower() {
		return ErrManagedElsewhere{c.Mesh.KeysFrom}
	}
	return nil
}

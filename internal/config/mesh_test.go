package config

import (
	"os"
	"strings"
	"testing"
)

func TestMeshValidation(t *testing.T) {
	ok := Mesh{Node: "a", SecretFile: "mesh.secret", KeysFrom: "b", Peers: []MeshPeer{{Name: "b", URL: "https://b:7777"}}}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(m *Mesh){
		"no node":           func(m *Mesh) { m.Node = "" },
		"no secret":         func(m *Mesh) { m.SecretFile = "" },
		"peer named as me":  func(m *Mesh) { m.Peers[0].Name = "a" },
		"url with path":     func(m *Mesh) { m.Peers[0].URL = "https://b:7777/v1" },
		"not http":          func(m *Mesh) { m.Peers[0].URL = "ftp://b" },
		"keys from no one":  func(m *Mesh) { m.KeysFrom = "zed" },
		"peer listed twice": func(m *Mesh) { m.Peers = append(m.Peers, m.Peers[0]) },
	}
	for name, mutate := range bad {
		m := ok
		m.Peers = append([]MeshPeer(nil), ok.Peers...)
		mutate(&m)
		if err := m.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A follower's teams, prices and aliases are its copy of the key holder's; changing other settings must not
// write that copy into its own config.yaml (a later change of keys_from would make it look local).
func TestFollowerSeesTheCopyButNeverSavesIt(t *testing.T) {
	t.Setenv("EMX_HOME", t.TempDir())
	os.MkdirAll(os.Getenv("EMX_HOME")+"/config", 0o700)
	if _, err := Update(func(c *Config) error {
		c.Aliases = map[string]string{"mine": "m1"}
		c.Mesh = Mesh{Node: "b", SecretFile: "s", KeysFrom: "a", Peers: []MeshPeer{{Name: "a", URL: "http://a:7777"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := SaveShared(Shared{Aliases: map[string]string{"chat": "big"}}); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if err != nil || c.Aliases["chat"] != "big" || c.Aliases["mine"] != "" {
		t.Fatalf("follower sees %v (%v)", c.Aliases, err)
	}
	if _, err := Update(func(c *Config) error { c.Gateway.Port = 7778; return nil }); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(ConfigPath())
	if strings.Contains(string(data), "big") {
		t.Fatalf("the copy was written into config.yaml:\n%s", data)
	}
	if err := ManagedElsewhere(); err == nil || !strings.Contains(err.Error(), "managed on a") {
		t.Fatalf("ManagedElsewhere: %v", err)
	}
}

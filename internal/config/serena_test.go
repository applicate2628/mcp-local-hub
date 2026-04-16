package config

import (
	"os"
	"testing"
)

func TestSerenaManifestParses(t *testing.T) {
	f, err := os.Open("../../servers/serena/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := ParseManifest(f)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Name != "serena" {
		t.Errorf("Name = %q", m.Name)
	}
	if len(m.Daemons) != 3 {
		t.Errorf("len(Daemons) = %d, want 3", len(m.Daemons))
	}
	if len(m.ClientBindings) != 4 {
		t.Errorf("len(ClientBindings) = %d, want 4", len(m.ClientBindings))
	}
}

func TestPortsRegistryValid(t *testing.T) {
	f, err := os.Open("../../configs/ports.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := ParsePortRegistry(f); err != nil {
		t.Fatalf("ParsePortRegistry: %v", err)
	}
}

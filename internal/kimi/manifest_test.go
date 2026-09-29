package kimi

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"mcp-local-hub/internal/config"
)

func TestKimiManifestAndCanonicalPortRegistryAgree(t *testing.T) {
	manifestPath := filepath.Join("..", "..", "servers", "kimi", "manifest.yaml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read Kimi manifest: %v", err)
	}
	manifest, err := config.ParseManifest(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse Kimi manifest: %v", err)
	}
	if manifest.Name != "kimi" || manifest.Command != "mcphub" || !reflect.DeepEqual(manifest.BaseArgs, []string{"kimi"}) || len(manifest.Daemons) != 1 || manifest.Daemons[0].Name != "default" || manifest.Daemons[0].Port != 9142 {
		t.Fatalf("manifest = %+v", manifest)
	}
	registryFile, err := os.Open(filepath.Join("..", "..", "configs", "ports.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer registryFile.Close()
	registry, err := config.ParsePortRegistry(registryFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range registry.Global {
		if entry.Server == "kimi" && entry.Daemon == "default" && entry.Port == 9142 {
			return
		}
	}
	t.Fatal("configs/ports.yaml has no kimi/default=9142 owner")
}

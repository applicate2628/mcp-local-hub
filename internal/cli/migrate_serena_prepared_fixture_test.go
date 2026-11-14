package cli

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/config"
)

// Preserve the scenario's legacy or dynamic-pool shape while using the exact
// package command and pinned source from this binary's embedded catalog. The
// seeded bytes remain the driver's source and the unchanged-manifest oracle.
func seedPreparedSerenaManifestForTest(t *testing.T, manifestDir, shapeYAML string) string {
	t.Helper()
	shape, err := config.ParseManifest(strings.NewReader(shapeYAML))
	if err != nil {
		t.Fatalf("parse Serena migration fixture shape: %v", err)
	}
	raw, err := api.NewAPI().CatalogManifestGet("serena")
	if err != nil {
		t.Fatalf("read embedded Serena package source: %v", err)
	}
	catalog, err := config.ParseManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse embedded Serena package source: %v", err)
	}
	shape.Command = catalog.Command
	shape.BaseArgs = append([]string(nil), catalog.BaseArgs...)
	if err := shape.Validate(); err != nil {
		t.Fatalf("validate prepared Serena migration fixture: %v", err)
	}
	payload, err := yaml.Marshal(shape)
	if err != nil {
		t.Fatalf("encode prepared Serena migration fixture: %v", err)
	}
	return seedSerenaManifest(t, manifestDir, string(payload))
}

package cli

import (
	"os"
	"reflect"
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
	if _, err := config.ParseManifest(strings.NewReader(shapeYAML)); err != nil {
		t.Fatalf("parse Serena migration fixture shape: %v", err)
	}
	// Keep declaration presence: encoding ServerManifest would introduce empty
	// keys such as url, which are forbidden for a native-http manifest.
	var shape map[string]any
	if err := yaml.Unmarshal([]byte(shapeYAML), &shape); err != nil {
		t.Fatalf("decode Serena migration fixture declarations: %v", err)
	}
	raw, err := api.NewAPI().CatalogManifestGet("serena")
	if err != nil {
		t.Fatalf("read embedded Serena package source: %v", err)
	}
	catalog, err := config.ParseManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse embedded Serena package source: %v", err)
	}
	shape["command"] = catalog.Command
	shape["base_args"] = append([]string(nil), catalog.BaseArgs...)
	payload, err := yaml.Marshal(shape)
	if err != nil {
		t.Fatalf("encode prepared Serena migration fixture: %v", err)
	}
	if _, err := config.ParseManifest(strings.NewReader(string(payload))); err != nil {
		t.Fatalf("validate serialized prepared Serena migration fixture: %v", err)
	}
	return seedSerenaManifest(t, manifestDir, string(payload))
}

func TestPreparedSerenaManifestKeepsNativeShape(t *testing.T) {
	catalogRaw, err := api.NewAPI().CatalogManifestGet("serena")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := config.ParseManifest(strings.NewReader(catalogRaw))
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"legacy": legacy2DaemonManifestYAML, "dynamic": alreadyMigratedManifestYAML} {
		t.Run(name, func(t *testing.T) {
			before, err := config.ParseManifest(strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			beforeState, err := detectSerenaSourceState(before)
			if err != nil {
				t.Fatal(err)
			}
			path := seedPreparedSerenaManifestForTest(t, t.TempDir(), body)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			after, err := config.ParseManifest(strings.NewReader(string(raw)))
			if err != nil {
				t.Fatalf("serialized native fixture is invalid: %v", err)
			}
			if after.Command != catalog.Command || !reflect.DeepEqual(after.BaseArgs, catalog.BaseArgs) {
				t.Fatal("fixture does not use the canonical package command and pinned source")
			}
			afterState, err := detectSerenaSourceState(after)
			if err != nil || afterState != beforeState {
				t.Fatalf("fixture migration shape changed: before=%v after=%v err=%v", beforeState, afterState, err)
			}
			var original, seeded map[string]any
			if err := yaml.Unmarshal([]byte(body), &original); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal(raw, &seeded); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"command", "base_args"} {
				delete(original, key)
				delete(seeded, key)
			}
			if !reflect.DeepEqual(original, seeded) {
				t.Fatalf("fixture changed declarations beyond its package command: before=%#v after=%#v", original, seeded)
			}
		})
	}
}

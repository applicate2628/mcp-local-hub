package api

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

func TestSerenaBuildEnvironment_CatalogUnbufferedIsRuntimeOnly(t *testing.T) {
	raw, err := NewAPI().CatalogManifestGet(SerenaServerName)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := config.ParseManifest(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	value, declared := manifest.Env["PYTHONUNBUFFERED"]
	if !declared || value != "1" {
		t.Fatalf("embedded Serena runtime logging declaration = %q, present = %v; want 1", value, declared)
	}

	for _, key := range []string{"PYTHONUNBUFFERED", "pythonunbuffered"} {
		if IsSerenaPreparationBuildDeclaration(key) {
			t.Errorf("%s must remain a runtime declaration, outside package preparation", key)
		}
	}
	kind, normalized, err := normalizeSerenaBuildValue("PYTHONUNBUFFERED", value)
	if err != nil || kind != "runtime-only" || normalized != "" {
		t.Fatalf("runtime logging declaration normalized to (%q, %q, %v), want (runtime-only, empty, nil)", kind, normalized, err)
	}
}

func TestSerenaBuildEnvironment_PythonDeclarationBoundary(t *testing.T) {
	tests := []struct {
		name, key, value, wantKind, wantValue, wantError string
		wantBuild                                        bool
	}{
		{name: "unknown-python", key: "PYTHONUNKNOWN", value: "1", wantBuild: true, wantError: "SERENA_BUILD_INPUT_UNSUPPORTED"},
		{name: "unbuffered-lookalike", key: "PYTHONUNBUFFERED_EXTRA", value: "1", wantBuild: true, wantError: "SERENA_BUILD_INPUT_UNSUPPORTED"},
		{name: "python-path", key: "PYTHONPATH", value: "unqualified", wantBuild: true, wantError: "SERENA_BUILD_IMPORT_OVERRIDE_REFUSED"},
		{name: "python-home", key: "PYTHONHOME", value: "unqualified", wantBuild: true, wantError: "SERENA_BUILD_IMPORT_OVERRIDE_REFUSED"},
		{name: "virtual-environment", key: "VIRTUAL_ENV", value: "unqualified", wantBuild: true, wantError: "SERENA_BUILD_IMPORT_OVERRIDE_REFUSED"},
		{name: "bytecode-policy", key: "PYTHONDONTWRITEBYTECODE", value: "1", wantBuild: true, wantKind: "policy", wantValue: "1"},
		{name: "bytecode-policy-conflict", key: "PYTHONDONTWRITEBYTECODE", value: "0", wantBuild: true, wantError: "SERENA_BUILD_BYTECODE_POLICY_CONFLICT"},
		{name: "unbuffered-nul", key: "PYTHONUNBUFFERED", value: "1\x00", wantError: "SERENA_BUILD_VALUE_INVALID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, key := range []string{tt.key, strings.ToLower(tt.key)} {
				if got := IsSerenaPreparationBuildDeclaration(key); got != tt.wantBuild {
					t.Errorf("build declaration %s = %v, want %v", key, got, tt.wantBuild)
				}
			}
			kind, normalized, err := normalizeSerenaBuildValue(tt.key, tt.value)
			gotError := ""
			if err != nil {
				gotError = err.Error()
			}
			if kind != tt.wantKind || normalized != tt.wantValue || gotError != tt.wantError {
				t.Fatalf("normalized (%q, %q, %q), want (%q, %q, %q)", kind, normalized, gotError, tt.wantKind, tt.wantValue, tt.wantError)
			}
		})
	}
}

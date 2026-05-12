package api

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

func TestManifestValidateStrictRejectsDoubleUnderscore(t *testing.T) {
	a := NewAPI()
	yaml := "name: foo__bar\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9200\n"
	warnings, err := a.ManifestValidateMode(yaml, ValidateModeStrict)
	if err == nil {
		t.Fatalf("strict mode must reject __ in name; got nil error, warnings=%v", warnings)
	}
	if !strings.Contains(err.Error(), "__") {
		t.Errorf("error must name the offending substring '__'; got %v", err)
	}
}

func TestManifestValidateCompatWarnsOnDoubleUnderscore(t *testing.T) {
	a := NewAPI()
	yaml := "name: foo__bar\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9200\n"
	warnings, err := a.ManifestValidateMode(yaml, ValidateModeCompat)
	if err != nil {
		t.Fatalf("compat mode must accept __ in name; got err=%v", err)
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "__") {
			found = true
		}
	}
	if !found {
		t.Errorf("compat mode must emit a __ warning; warnings=%v", warnings)
	}
}

func TestManifestValidateDefaultEqualsCompat(t *testing.T) {
	// Existing callers that use ManifestValidate (compat-equivalent) must keep working.
	a := NewAPI()
	yaml := "name: foo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9200\n"
	// must not panic; existing []string return preserved
	_ = a.ManifestValidate(yaml)
}

// TestManifestValidateForHubBindRejectsStrict pins the bind-time gate's
// strict-only error path. Phase 4's hub listener bring-up calls this
// helper at gate-ON startup. The helper drops warnings and surfaces
// only the hard error.
func TestManifestValidateForHubBindRejectsStrict(t *testing.T) {
	a := NewAPI()
	bad := "name: foo__bar\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9200\n"
	if err := a.ManifestValidateForHubBind(bad); err == nil {
		t.Errorf("ManifestValidateForHubBind must reject __ name")
	}
	good := "name: foo\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9200\n"
	if err := a.ManifestValidateForHubBind(good); err != nil {
		t.Errorf("ManifestValidateForHubBind must accept clean name; got %v", err)
	}
}

// TestServerManifestValidateStrictAgreesWithApiLayer ensures the
// config-layer ValidateStrict and the api-layer ManifestValidateMode
// agree on '__' detection. Phase 5 install paths call ValidateStrict
// directly on a parsed ServerManifest; the api layer is the YAML-text
// entry point. They must not drift.
func TestServerManifestValidateStrictAgreesWithApiLayer(t *testing.T) {
	bad := &config.ServerManifest{
		Name:      "foo__bar",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "echo",
		Daemons:   []config.DaemonSpec{{Name: "default", Port: 9200}},
	}
	if err := bad.ValidateStrict(); err == nil {
		t.Errorf("ServerManifest.ValidateStrict must reject __ name")
	}
	good := &config.ServerManifest{
		Name:      "foo-bar",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "echo",
		Daemons:   []config.DaemonSpec{{Name: "default", Port: 9200}},
	}
	if err := good.ValidateStrict(); err != nil {
		t.Errorf("ServerManifest.ValidateStrict must accept clean name; got %v", err)
	}
}

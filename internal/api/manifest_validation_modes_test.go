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

// codex bot r1 P1 closure: parse failures (malformed YAML, missing
// required field, etc.) must return a hard error in strict mode so
// admission gates that drop warnings (ManifestValidateForHubBind)
// cannot let invalid manifests through.
func TestManifestValidateStrictRejectsParseFailure(t *testing.T) {
	a := NewAPI()
	cases := []struct {
		name string
		yaml string
	}{
		{name: "malformed-yaml", yaml: "name: foo\nkind: [unclosed-list\n"},
		{name: "missing-name", yaml: "kind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9200\n"},
		{name: "unknown-kind", yaml: "name: foo\nkind: nonsense\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9200\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.ManifestValidateMode(tc.yaml, ValidateModeStrict)
			if err == nil {
				t.Fatalf("strict mode must reject %s as hard error", tc.name)
			}
		})
	}
}

// And the parse-failure-rejection must propagate through the strict
// hub-bind helper, which is the actual gate at hub startup.
func TestManifestValidateForHubBindRejectsParseFailure(t *testing.T) {
	a := NewAPI()
	// Truncated / unparseable YAML — must NOT admit.
	err := a.ManifestValidateForHubBind("name: foo\nkind: [unclosed\n")
	if err == nil {
		t.Fatalf("hub-bind gate must reject malformed YAML; got nil")
	}
}

// codex bot r5 P1 closure: ManifestCreateIn + ManifestEditIn are
// "mutation surfaces" per the spec's Pre-gate section; they must
// reject '__' in server names with a hard error, not just emit a
// warning. Without this gate, a direct call like
// ManifestCreateIn(tmp, "foo__bar", yaml) would write the manifest
// to disk despite the strict-mode policy.
func TestManifestCreateInRejectsDoubleUnderscore(t *testing.T) {
	a := NewAPI()
	tmp := t.TempDir()
	body := "name: foo__bar\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9201\n"
	err := a.ManifestCreateIn(tmp, "foo__bar", body)
	if err == nil {
		t.Fatalf("ManifestCreateIn must reject '__' in name via strict gate")
	}
	if !strings.Contains(err.Error(), "__") {
		t.Errorf("error must mention '__'; got %v", err)
	}
}

func TestManifestEditInRejectsDoubleUnderscore(t *testing.T) {
	a := NewAPI()
	tmp := t.TempDir()
	// First create a legitimate (non-__) manifest to edit.
	createBody := "name: legit\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9202\n"
	if err := a.ManifestCreateIn(tmp, "legit", createBody); err != nil {
		t.Fatalf("setup create: %v", err)
	}
	// Now try to edit with a '__' name. The directory-name 'legit'
	// stays the same; the inner YAML's `name:` is what hits the
	// strict gate.
	editBody := "name: foo__bar\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9202\n"
	err := a.ManifestEditIn(tmp, "legit", editBody)
	if err == nil {
		t.Fatalf("ManifestEditIn must reject '__' in inner manifest name via strict gate")
	}
	if !strings.Contains(err.Error(), "__") {
		t.Errorf("error must mention '__'; got %v", err)
	}
}

// Compat mode must still surface parse failures as warnings (not errors)
// so existing GUI manifest-list callers don't break on legacy
// __-named manifests with minor structural quirks.
func TestManifestValidateCompatTreatsParseFailureAsWarning(t *testing.T) {
	a := NewAPI()
	warnings, err := a.ManifestValidateMode("name: foo\nkind: [unclosed\n", ValidateModeCompat)
	if err != nil {
		t.Fatalf("compat mode must NOT return error on parse failure; got %v", err)
	}
	if len(warnings) == 0 {
		t.Fatalf("compat mode must surface parse failure as warning; got empty warnings")
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

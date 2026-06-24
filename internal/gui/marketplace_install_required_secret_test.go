package gui

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// redirectStateToEmptyVaultHome points the vault / state path env vars at a
// fresh temp dir so api.AdmissionCheck's required-secret probe sees a GENUINELY
// ABSENT vault (OpenVaultOptional → nil, nil → the required key is unresolvable).
// It mirrors the api-package setupAdmissionParityTest env redirection, but the
// gui test package cannot reach api's unexported preflight stubs, so it sets only
// the path env vars (the required-secret finding fires regardless of the other
// host probes — they only ADD findings, and RequiredSecretAdmission filters to
// the required-secret one).
func redirectStateToEmptyVaultHome(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"Local", "Roaming", "Home", "State", "Config"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "Local"))
	t.Setenv("APPDATA", filepath.Join(root, "Roaming"))
	t.Setenv("USERPROFILE", filepath.Join(root, "Home"))
	t.Setenv("HOME", filepath.Join(root, "Home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "State"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "Config"))
}

// TestMarketplaceInstall_HubRequiredSecretBlocksBeforeManifestCreate is the
// GUI-hub-path mirror of the api-side TestRequiredSecretGate_BlocksInstallWithEmptyVault
// (codex finding 2). A one-click HUB install of a suno-style stdio entry whose
// required_secrets key is UNSET in the vault must be REFUSED by the
// required-secret gate that now runs BEFORE ManifestCreate — so NO manifest file
// is left behind. Previously the gate ran only inside Install→Preflight, which
// fires AFTER ManifestCreate had already persisted the manifest to disk.
//
// The fakes prove the no-write invariant: ManifestCreate sets creator.name only
// when called, and Install sets installer.called only when called — both must
// stay zero/false. The response is 412 with REQUIRED_SECRET_MISSING.
func TestMarketplaceInstall_HubRequiredSecretBlocksBeforeManifestCreate(t *testing.T) {
	redirectStateToEmptyVaultHome(t) // vault genuinely absent → required key unresolvable

	entry := &api.MarketplaceEntry{
		ID:              "suno",
		Name:            "Suno",
		Transport:       "stdio",
		Command:         "uvx",
		Args:            []string{"--from", "mcp-suno", "mcp-suno"},
		Env:             map[string]string{"ACEDATACLOUD_API_TOKEN": "secret:acedata_api_token"},
		RequiredSecrets: []string{"acedata_api_token"},
	}
	loader := &fakeMarketplaceEntryLoader{entry: entry, found: true}
	picker := &fakeGlobalPortPicker{port: 9314}
	creator := &fakeManifestCreator{}
	installer := &fakeInstaller{}
	s := newMarketplaceInstallTestServer(loader, picker, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, creator, installer)

	rec := postInstall(t, s, `{"id":"suno","mode":"hub"}`, "same-origin")

	// 412 Precondition Failed (NOT 409 NAME_CONFLICT) — the required-secret
	// precondition is unmet.
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412 (required-secret unset blocks), body=%q", rec.Code, rec.Body.String())
	}
	// No manifest written, no install attempted — the gate runs BEFORE ManifestCreate.
	if creator.name != "" {
		t.Fatalf("ManifestCreate was reached (name=%q) — a blocked required-secret install must leave NO manifest behind", creator.name)
	}
	if installer.called {
		t.Fatal("Install was reached for a required-secret-blocked install — gate ran too late")
	}
	// The error code names the distinct precondition so the frontend can render
	// its own "set the secret first" message.
	if body := rec.Body.String(); !strings.Contains(body, "REQUIRED_SECRET_MISSING") {
		t.Errorf("body missing REQUIRED_SECRET_MISSING code: %q", body)
	}
	// Redaction posture: the Reason names the KEY, never a value (there is no value
	// to leak here, but the key must be present so the operator knows what to set).
	if body := rec.Body.String(); !strings.Contains(body, "acedata_api_token") {
		t.Errorf("body should name the missing required-secret key: %q", body)
	}
}

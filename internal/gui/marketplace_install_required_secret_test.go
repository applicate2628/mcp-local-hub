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

// TestMarketplaceInstall_HubUnreadableVaultBlocksBeforeManifestCreate covers the
// codex finding-1 edge: an EMPTY-BUT-UNREADABLE vault (the secrets.age file
// EXISTS but cannot be opened — here because no valid .age-key is present) must
// ALSO block the one-click HUB install BEFORE ManifestCreate. In that state
// AdmissionCheck emits the non-optional "secrets-vault-readable" finding and
// DELIBERATELY SKIPS the per-key "required-secret" loop (it cannot tell which
// keys are present), so a gate filtering to "required-secret" alone would see
// nothing and let ManifestCreate persist the manifest before the real
// Install→Preflight gate re-detects the unreadable vault — leaving a manifest
// behind. RequiredSecretAdmission now surfaces the secret-vault-readable block
// too, so this 412s with NO manifest written.
func TestMarketplaceInstall_HubUnreadableVaultBlocksBeforeManifestCreate(t *testing.T) {
	redirectStateToEmptyVaultHome(t)
	// Plant a present-but-unreadable vault: a non-empty secrets.age with NO valid
	// .age-key alongside it. OpenVaultOptional stats secrets.age (exists) → calls
	// OpenVault → read-identity fails → returns (nil, "vault exists but
	// unreadable"), the exact state that fires "secrets-vault-readable". This is
	// cross-platform (corrupt content, not a DACL/permission flip).
	dataDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "mcp-local-hub")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "secrets.age"), []byte("not-a-valid-age-vault\n"), 0o600); err != nil {
		t.Fatalf("plant unreadable vault: %v", err)
	}

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
	picker := &fakeGlobalPortPicker{port: 9315}
	creator := &fakeManifestCreator{}
	installer := &fakeInstaller{}
	s := newMarketplaceInstallTestServer(loader, picker, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, creator, installer)

	rec := postInstall(t, s, `{"id":"suno","mode":"hub"}`, "same-origin")

	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412 (unreadable vault blocks), body=%q", rec.Code, rec.Body.String())
	}
	if creator.name != "" {
		t.Fatalf("ManifestCreate was reached (name=%q) — an unreadable-vault-blocked install must leave NO manifest behind", creator.name)
	}
	if installer.called {
		t.Fatal("Install was reached for an unreadable-vault-blocked install — gate ran too late")
	}
	if body := rec.Body.String(); !strings.Contains(body, "REQUIRED_SECRET_MISSING") {
		t.Errorf("body missing REQUIRED_SECRET_MISSING code: %q", body)
	}
	// codex finding 1 — the 412 body must use the REDACTED generic vault message,
	// NOT the raw OpenVaultOptional error, which embeds the absolute vault/key FILE
	// PATH (DefaultKeyPath/DefaultVaultPath) and, on corp hosts, the AD username in
	// C:\Users\<name>\.... The redacted message names the vault problem + the
	// Secrets screen, with no path.
	body := rec.Body.String()
	if !strings.Contains(body, "secrets vault could not be read") {
		t.Errorf("body should carry the redacted vault message; got %q", body)
	}
	// The temp data dir path (and the raw-error fragments that carry it) must NOT
	// leak into the HTTP body.
	if strings.Contains(body, dataDir) {
		t.Errorf("412 body LEAKS the vault directory path %q: %q", dataDir, body)
	}
	for _, leak := range []string{"secrets.age", "read identity", "vault exists but unreadable", "uses secret refs"} {
		if strings.Contains(body, leak) {
			t.Errorf("412 body leaks raw vault-error fragment %q: %q", leak, body)
		}
	}
}

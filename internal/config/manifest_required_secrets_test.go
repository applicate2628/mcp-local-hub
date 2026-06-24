package config

import (
	"strings"
	"testing"
)

// manifest_required_secrets_test.go — the manifest-side required_secrets
// integrity gate (codex finding 3). The catalog authoring guard already asserts
// each required_secrets key backs a `secret:<key>` env ref on a MarketplaceEntry;
// these tests pin the SAME rule on a persisted / hand-edited ServerManifest via
// Validate(), so a typo key (one that happens to exist in the vault) cannot pass
// the install gate while the real env secret stays missing → crash-loop.

// requiredSecretStdioManifest is a minimal valid stdio-bridge manifest carrying a
// secret: env ref. Tests vary only RequiredSecrets/Env so the gate is isolated.
func requiredSecretStdioManifest() *ServerManifest {
	return &ServerManifest{
		Name:      "suno",
		Kind:      KindGlobal,
		Transport: TransportStdioBridge,
		Command:   "uvx",
		Daemons:   []DaemonSpec{{Name: "default", Port: 9314}},
		Env:       map[string]string{"ACEDATACLOUD_API_TOKEN": "secret:acedata_api_token"},
	}
}

// A required_secrets key that DOES back a secret:<key> env ref validates cleanly.
func TestValidate_RequiredSecrets_BackedKeyAccepted(t *testing.T) {
	m := requiredSecretStdioManifest()
	m.RequiredSecrets = []string{"acedata_api_token"}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate rejected a backed required_secrets key: %v", err)
	}
}

// A required_secrets key with NO matching secret:<key> env ref is rejected — the
// typo/stale-key case that would gate the install on a phantom credential the
// daemon never reads (or pass while the real env secret stays missing).
func TestValidate_RequiredSecrets_UnbackedKeyRejected(t *testing.T) {
	m := requiredSecretStdioManifest()
	m.RequiredSecrets = []string{"typo_key"} // env backs acedata_api_token, not typo_key
	err := m.Validate()
	if err == nil {
		t.Fatal("Validate accepted an unbacked required_secrets key; want reject")
	}
	if !strings.Contains(err.Error(), "typo_key") || !strings.Contains(err.Error(), "no matching secret:") {
		t.Fatalf("error %q must name the unbacked key + the missing secret: ref", err)
	}
}

// An empty required_secrets entry is rejected so a stray "" cannot become a
// permanently-unblockable phantom requirement (mirrors the catalog guard).
func TestValidate_RequiredSecrets_EmptyKeyRejected(t *testing.T) {
	m := requiredSecretStdioManifest()
	m.RequiredSecrets = []string{""}
	err := m.Validate()
	if err == nil {
		t.Fatal("Validate accepted an empty required_secrets key; want reject")
	}
	if !strings.Contains(err.Error(), "empty key") {
		t.Fatalf("error %q must report the empty required_secrets key", err)
	}
}

// The reverse direction is intentionally NOT enforced: a secret: env ref WITHOUT
// a required_secrets entry stays the default optional-secret posture (the whole
// point of the opt-in gate) — Validate must NOT require required_secrets to list
// every secret env ref.
func TestValidate_RequiredSecrets_OptionalSecretEnvRefNotForced(t *testing.T) {
	m := requiredSecretStdioManifest()
	// Env has a secret: ref but RequiredSecrets is empty → must still validate.
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate rejected an optional (unlisted) secret env ref: %v", err)
	}
}

// ADDITIVE: a manifest with no required_secrets at all skips the gate entirely
// (every existing manifest), so its presence does not change validation.
func TestValidate_RequiredSecrets_AbsentIsNoOp(t *testing.T) {
	m := requiredSecretStdioManifest()
	m.Env = nil // no env, no required_secrets
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate rejected a manifest with no required_secrets: %v", err)
	}
}

// The gate round-trips through ParseManifest: a backed key parses, an unbacked
// key is rejected at parse (Validate is called by ParseManifest).
func TestValidate_RequiredSecrets_ParseManifestRejectsUnbacked(t *testing.T) {
	unbacked := `
name: suno
kind: global
transport: stdio-bridge
command: uvx
required_secrets: [typo_key]
env:
  ACEDATACLOUD_API_TOKEN: secret:acedata_api_token
daemons:
  - name: default
    port: 9314
`
	_, err := ParseManifest(strings.NewReader(unbacked))
	if err == nil {
		t.Fatal("ParseManifest accepted an unbacked required_secrets key; want reject")
	}
	if !strings.Contains(err.Error(), "typo_key") {
		t.Fatalf("error %q must name the unbacked key", err)
	}
}

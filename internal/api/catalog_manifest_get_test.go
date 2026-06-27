package api

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCatalogManifestGet_EmbeddedNameReturnsSecretRefNotLiteral is the
// secret-safe-source half of the D2 r2 security core. CatalogManifestGet for
// an EMBEDDED server (wolfram) must return the EMBED YAML — whose sensitive
// env is published as a `secret:` ref, never a resolved literal — proving the
// cold-re-enable Re-add prefill draws from the shipped manifest, not a live
// client config or a disk manifest.
func TestCatalogManifestGet_EmbeddedNameReturnsSecretRefNotLiteral(t *testing.T) {
	a := NewAPI()
	yaml, err := a.CatalogManifestGet("wolfram")
	if err != nil {
		t.Fatalf("CatalogManifestGet(wolfram): unexpected error %v", err)
	}
	if !strings.Contains(yaml, "secret:wolfram_app_id") {
		t.Errorf("embed YAML must carry the secret: ref; got:\n%s", yaml)
	}
	// The embed manifest must NOT carry a resolved literal app id. A
	// catalog-prefill that echoed a literal would defeat D2's whole reason
	// to exist. The shipped manifest only ever holds the placeholder.
	if strings.Contains(yaml, "secret:wolfram_app_id") && strings.Contains(strings.ToLower(yaml), "literal") {
		t.Errorf("embed YAML unexpectedly contains a literal marker:\n%s", yaml)
	}
}

// TestCatalogManifestGet_DiskOnlyNameIsNotEmbedded is THE AUTHORITATIVE
// disk-literal-not-sourced falsifier at the api layer. It seeds a manifest
// that exists ONLY on disk (via the MCPHUB_MANIFEST_DIR_OVERRIDE test seam)
// carrying a LITERAL secret in env, then asserts CatalogManifestGet returns
// ErrManifestNotEmbedded for it.
//
// This proves the membership gate runs BEFORE the loader: embeddedManifestNames
// reads the embed FS directly and does NOT consult the override, so a disk-only
// name is excluded from the embed set regardless of the override. The 404 fires
// first, so loadManifestYAMLEmbedFirst's override/disk read is structurally
// unreachable for this path — the literal in the disk manifest is NEVER sourced.
func TestCatalogManifestGet_DiskOnlyNameIsNotEmbedded(t *testing.T) {
	dir := t.TempDir()
	// A disk-only manifest with a LITERAL secret in env — the worst case the
	// membership gate must refuse to source.
	srv := "diskonlysrv"
	if err := os.MkdirAll(filepath.Join(dir, srv), 0o755); err != nil {
		t.Fatal(err)
	}
	const literal = "sk-LITERAL-do-not-source"
	yamlBytes := "name: diskonlysrv\nkind: global\ntransport: stdio-bridge\ncommand: node\n" +
		"env:\n  MY_TOKEN: '" + literal + "'\ndaemons:\n  - name: default\n    port: 9299\n"
	if err := os.WriteFile(filepath.Join(dir, srv, "manifest.yaml"), []byte(yamlBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	// The override bypasses the embed FS in loadManifestYAMLEmbedFirst — but
	// embeddedManifestNames (the gate) ignores it. So the gate still says
	// "diskonlysrv is not embedded" and returns BEFORE the loader runs.
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	a := NewAPI()
	yaml, err := a.CatalogManifestGet(srv)
	if !errors.Is(err, ErrManifestNotEmbedded) {
		t.Fatalf("CatalogManifestGet(%q): err = %v, want ErrManifestNotEmbedded", srv, err)
	}
	if yaml != "" {
		t.Errorf("CatalogManifestGet(%q): yaml must be empty on the membership-gate miss, got %q", srv, yaml)
	}
	if strings.Contains(yaml, literal) {
		t.Fatalf("SECURITY VIOLATION: the disk literal %q was sourced through CatalogManifestGet", literal)
	}
}

// TestCatalogManifestGet_OverrideDoesNotSourceDiskLiteralForUnembeddedName is
// the belt-and-suspenders form: even with the override pointing at a dir that
// shadows a real embedded name's path, an UNEMBEDDED name there is still gated
// out. (The embedded-name case is covered by the secret-ref test above, which
// runs WITHOUT the override so it exercises the genuine embed branch.)
func TestCatalogManifestGet_OverrideDoesNotSourceDiskLiteralForUnembeddedName(t *testing.T) {
	dir := t.TempDir()
	// Sanity: the embed set must NOT contain our synthetic disk-only name, or
	// the falsifier is vacuous.
	for _, n := range embeddedManifestNames() {
		if n == "ghostonly" {
			t.Fatalf("test invariant broken: %q unexpectedly in the embed set", "ghostonly")
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "ghostonly"), 0o755); err != nil {
		t.Fatal(err)
	}
	const literal = "sk-GHOST-literal"
	body := "name: ghostonly\nkind: global\ntransport: stdio-bridge\ncommand: node\nenv:\n  T: '" + literal + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "ghostonly", "manifest.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	a := NewAPI()
	yaml, err := a.CatalogManifestGet("ghostonly")
	if !errors.Is(err, ErrManifestNotEmbedded) {
		t.Fatalf("err = %v, want ErrManifestNotEmbedded", err)
	}
	if strings.Contains(yaml, literal) {
		t.Fatalf("SECURITY VIOLATION: disk literal sourced for unembedded name")
	}
}

// TestCatalogManifestGet_EmbeddedNameWithOverrideStillReadsEmbed is the D2 r3
// FINDING 1 falsifier. It points MCPHUB_MANIFEST_DIR_OVERRIDE at a temp dir
// holding wolfram/manifest.yaml with a LITERAL secret, then asserts
// CatalogManifestGet("wolfram") returns the EMBED YAML (the secret: ref), NOT
// the disk literal.
//
// This is the case the r2 code could NOT defend: wolfram IS in the embed set,
// so the membership gate passes, and the OLD path (loadManifestYAMLEmbedFirst)
// would hit the override branch (manifest_source.go:77-79) and read the disk
// literal for an embedded name. The r3 fix reads the embed FS DIRECTLY
// (fs.ReadFile(servers.Manifests, ...)) after the gate, with no override/disk
// branch — so the endpoint is TRULY embed-only and secret-safe by construction.
func TestCatalogManifestGet_EmbeddedNameWithOverrideStillReadsEmbed(t *testing.T) {
	dir := t.TempDir()
	// Sanity: wolfram MUST be in the embed set, or this falsifier is vacuous.
	embedded := false
	for _, n := range embeddedManifestNames() {
		if n == "wolfram" {
			embedded = true
			break
		}
	}
	if !embedded {
		t.Fatalf("test invariant broken: %q is not in the embed set", "wolfram")
	}
	// A DISK wolfram manifest carrying a LITERAL secret — exactly what the
	// override branch of loadManifestYAMLEmbedFirst would have sourced for an
	// embedded name before the r3 direct-embed-read fix.
	if err := os.MkdirAll(filepath.Join(dir, "wolfram"), 0o755); err != nil {
		t.Fatal(err)
	}
	const literal = "sk-WOLFRAM-LITERAL-do-not-source"
	body := "name: wolfram\nkind: global\ntransport: stdio-bridge\ncommand: node\n" +
		"env:\n  WOLFRAM_LLM_APP_ID: '" + literal + "'\ndaemons:\n  - name: default\n    port: 9298\n"
	if err := os.WriteFile(filepath.Join(dir, "wolfram", "manifest.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)

	a := NewAPI()
	yaml, err := a.CatalogManifestGet("wolfram")
	if err != nil {
		t.Fatalf("CatalogManifestGet(wolfram): unexpected error %v", err)
	}
	// MUST be the embed YAML (the secret: ref), proving the read ignored the
	// override entirely.
	if !strings.Contains(yaml, "secret:wolfram_app_id") {
		t.Errorf("expected the EMBED YAML (secret: ref) despite the override; got:\n%s", yaml)
	}
	// MUST NOT be the disk literal — the whole point of the r3 fix.
	if strings.Contains(yaml, literal) {
		t.Fatalf("SECURITY VIOLATION: the disk literal %q was sourced through CatalogManifestGet despite the embedded name — the override leaked", literal)
	}
}

// TestCatalogManifestGet_RejectsPathTraversal asserts the name gate runs
// FIRST — a traversal name is refused at checkManifestName (a non-nil error
// that is NOT ErrManifestNotEmbedded), so it cannot drive a pre-validation
// filesystem probe through the loader.
func TestCatalogManifestGet_RejectsPathTraversal(t *testing.T) {
	a := NewAPI()
	for _, bad := range []string{"../escape", "../../etc", "/abs/path", "name/with/slash", ".."} {
		yaml, err := a.CatalogManifestGet(bad)
		if err == nil {
			t.Errorf("CatalogManifestGet(%q): expected an error, got nil (yaml=%q)", bad, yaml)
			continue
		}
		// A traversal name fails the name gate, NOT the membership gate — it
		// never reaches embeddedManifestNames / the loader.
		if errors.Is(err, ErrManifestNotEmbedded) {
			t.Errorf("CatalogManifestGet(%q): expected a name-validation error, got ErrManifestNotEmbedded", bad)
		}
		if yaml != "" {
			t.Errorf("CatalogManifestGet(%q): yaml must be empty on error, got %q", bad, yaml)
		}
	}
}

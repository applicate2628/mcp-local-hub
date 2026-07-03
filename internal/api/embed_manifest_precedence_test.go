package api

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/servers"
)

// Tests for work-items/decisions/2026-07-03-embed-vs-disk-manifest-precedence.md
// (Option B — embed always wins; the collision is LOUD at the write gate).

// Test 1 — Create refused for embedded name: the error names the collision +
// suggests a rename, wraps ErrManifestNameEmbedded, and NO file is written (the
// refusal fires BEFORE the disk write).
func TestManifestCreateIn_RefusesEmbeddedName(t *testing.T) {
	a := NewAPI()
	dir := t.TempDir()
	// "wolfram" is a shipped/embedded server. NO override → isEmbeddedManifestName true.
	yaml := "name: wolfram\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9401\n"
	err := a.ManifestCreateIn(dir, "wolfram", yaml)
	if err == nil {
		t.Fatal("ManifestCreateIn must refuse a shipped/embedded server name")
	}
	if !errors.Is(err, ErrManifestNameEmbedded) {
		t.Errorf("err = %v, want wrapped ErrManifestNameEmbedded", err)
	}
	if !strings.Contains(err.Error(), "wolfram") || !strings.Contains(err.Error(), "wolfram-custom") {
		t.Errorf("error must name the collision + suggest a rename; got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "wolfram", "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("a manifest was written despite the refusal (stat err = %v)", statErr)
	}
}

// Test 2 — Create accepted for a non-embedded name: the dev/custom-server flow
// is intact.
func TestManifestCreateIn_AcceptsNonEmbeddedName(t *testing.T) {
	a := NewAPI()
	dir := t.TempDir()
	yaml := "name: mycustomsrv\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9402\n"
	if err := a.ManifestCreateIn(dir, "mycustomsrv", yaml); err != nil {
		t.Fatalf("create of a non-embedded name must succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "mycustomsrv", "manifest.yaml")); err != nil {
		t.Fatalf("manifest not written: %v", err)
	}
}

// Test 3 — Test-override symmetry: with MCPHUB_MANIFEST_DIR_OVERRIDE set the
// loader bypasses the embed FS, so isEmbeddedManifestName is false even for a
// shipped name and the create is allowed (hermetic tests are unaffected).
func TestManifestCreateIn_EmbeddedNameAllowedUnderTestOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", dir)
	if isEmbeddedManifestName("wolfram") {
		t.Fatal("isEmbeddedManifestName must be false under MCPHUB_MANIFEST_DIR_OVERRIDE (embed FS bypassed)")
	}
	a := NewAPI()
	yaml := "name: wolfram\nkind: global\ntransport: stdio-bridge\ncommand: echo\ndaemons:\n  - name: default\n    port: 9403\n"
	if err := a.ManifestCreateIn(dir, "wolfram", yaml); err != nil {
		t.Fatalf("under the test override, creating a shipped name must be allowed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "wolfram", "manifest.yaml")); err != nil {
		t.Fatalf("manifest not written under override: %v", err)
	}
}

// Test 4 — CatalogManifestGet byte-identical after the membership-loop dedup.
// The dedup replaced the inline loop with embeddedManifestNamesContains (the
// OVERRIDE-INDEPENDENT core), so every embedded name still resolves to the
// direct embed bytes and a non-embedded name still returns ErrManifestNotEmbedded.
func TestCatalogManifestGet_ByteIdenticalAfterMembershipDedup(t *testing.T) {
	a := NewAPI()
	names := embeddedManifestNames()
	if len(names) == 0 {
		t.Fatal("no embedded manifests — test is vacuous")
	}
	for _, n := range names {
		if !embeddedManifestNamesContains(n) {
			t.Errorf("embeddedManifestNamesContains(%q) = false, want true", n)
		}
		got, err := a.CatalogManifestGet(n)
		if err != nil {
			t.Errorf("CatalogManifestGet(%q): unexpected error %v", n, err)
			continue
		}
		want, err := fs.ReadFile(servers.Manifests, n+"/manifest.yaml")
		if err != nil {
			t.Fatalf("read embed %q: %v", n, err)
		}
		if got != string(want) {
			t.Errorf("CatalogManifestGet(%q) not byte-identical to the direct embed read", n)
		}
	}
	if embeddedManifestNamesContains("ghost-not-embedded") {
		t.Fatal("test invariant broken: synthetic name unexpectedly in the embed set")
	}
	if _, err := a.CatalogManifestGet("ghost-not-embedded"); !errors.Is(err, ErrManifestNotEmbedded) {
		t.Errorf("CatalogManifestGet(non-embedded): err = %v, want ErrManifestNotEmbedded", err)
	}
}

// Test 5 — pre-existing colliding disk file: embeddedDiskShadowWarning fires for
// a shipped name shadowed on disk and NEVER touches the operator's file. (The
// "embed installs" half of the scenario is the read-precedence regression, Test
// 8.)
func TestEmbeddedDiskShadowWarning_FiresForShippedNameAndLeavesFileUntouched(t *testing.T) {
	dir := t.TempDir()
	const body = "name: wolfram\nkind: global\ntransport: stdio-bridge\ncommand: node\nenv:\n  WOLFRAM_LLM_APP_ID: 'sk-USER-EDIT'\ndaemons:\n  - name: default\n    port: 9404\n"
	seed := filepath.Join(dir, "wolfram", "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(seed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seed, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	warn := embeddedDiskShadowWarning("wolfram", dir)
	if warn == "" {
		t.Fatal("expected a warn for a shipped name shadowed by a disk manifest")
	}
	if !strings.Contains(warn, "wolfram") || !strings.Contains(warn, "ignored") {
		t.Errorf("warn must name the shadowed shipped server + say it is ignored; got %q", warn)
	}
	// NEVER delete the user's file — the warn is non-destructive.
	after, err := os.ReadFile(seed)
	if err != nil {
		t.Fatalf("the operator's disk manifest must be left untouched: %v", err)
	}
	if string(after) != body {
		t.Errorf("disk manifest bytes changed after the warn")
	}
	// A NON-embedded name is not warned even with a disk file present.
	other := filepath.Join(dir, "mycustomsrv", "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("name: mycustomsrv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := embeddedDiskShadowWarning("mycustomsrv", dir); w != "" {
		t.Errorf("non-embedded name must not warn; got %q", w)
	}
	// A shipped name with NO disk file present → no warn.
	if w := embeddedDiskShadowWarning("wolfram", t.TempDir()); w != "" {
		t.Errorf("shipped name without a disk shadow must not warn; got %q", w)
	}
}

// Test 8 — Regression: embed bytes still win the read for a shipped name. The
// embed-first loader serves the embed bytes directly; the disk-fallback branch
// is reached ONLY on an embed MISS, so a same-named disk manifest is never
// consulted for a shipped name (byte-identical to the direct embed read proves
// the fallback was not taken).
func TestLoadManifestYAMLEmbedFirst_EmbedWinsForShippedNameRegression(t *testing.T) {
	const name = "wolfram"
	want, err := fs.ReadFile(servers.Manifests, name+"/manifest.yaml")
	if err != nil {
		t.Fatalf("read embed: %v", err)
	}
	got, err := loadManifestYAMLEmbedFirst(name)
	if err != nil {
		t.Fatalf("loadManifestYAMLEmbedFirst(%q): %v", name, err)
	}
	if string(got) != string(want) {
		t.Errorf("embed-first read for a shipped name diverged from the direct embed bytes")
	}
	if !isEmbeddedManifestName(name) {
		t.Errorf("isEmbeddedManifestName(%q) = false, want true (no override)", name)
	}
}

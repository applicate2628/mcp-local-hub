package api

import (
	"bytes"
	"errors"
	"io/fs"
	"log"
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

// F2 (Fable pre-bot review of 27bcb3b9) — identical-bytes suppression: a disk
// copy byte-equal to the embed manifest changes nothing (the embed read serving
// instead of the disk read is unobservable), so it must NOT warn — otherwise a
// dev checkout beside the built binary warns for every shipped server on every
// install --all / scan. A differing copy still warns (the truthful case).
func TestEmbeddedDiskShadowWarning_IdenticalBytesSuppressed(t *testing.T) {
	embedBytes, err := fs.ReadFile(servers.Manifests, "wolfram/manifest.yaml")
	if err != nil {
		t.Fatalf("read embed: %v", err)
	}
	// Identical copy → no warn.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "wolfram"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wolfram", "manifest.yaml"), embedBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if w := embeddedDiskShadowWarning("wolfram", dir); w != "" {
		t.Errorf("identical-bytes disk copy must not warn (pure noise); got %q", w)
	}
	// A differing copy (trailing operator edit) → the warn stays.
	edited := append(append([]byte{}, embedBytes...), []byte("\n# operator edit\n")...)
	if err := os.WriteFile(filepath.Join(dir, "wolfram", "manifest.yaml"), edited, 0o644); err != nil {
		t.Fatal(err)
	}
	if w := embeddedDiskShadowWarning("wolfram", dir); w == "" {
		t.Error("differing disk copy must still warn")
	}
}

// F3a (Fable pre-bot review) — emit-site test for (*API).Install: the warn line
// actually reaches the install writer, and the embed manifest is what the flow
// consumes. The seeded disk shadow is deliberately UNPARSEABLE: if the install
// path (incorrectly) read the disk file instead of the embed,
// parseManifestForName would fail with a parse error — its absence proves
// embed-wins held. Install runs DryRun (installPlanCore: "DRY RUN → print the
// plan and return", no mutation, no intent) with state redirected to a temp
// dir; the warn is emitted before preflight, so the assertion holds regardless
// of the host's admission-check outcome (tolerated: e.g. a missing command on
// PATH).
func TestInstall_EmitsEmbeddedDiskShadowWarnToWriter(t *testing.T) {
	tmpState := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmpState)
	t.Setenv("XDG_DATA_HOME", tmpState)
	t.Setenv("XDG_STATE_HOME", tmpState)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Differing AND unparseable — the embed-wins discriminator (see doc above).
	if err := os.WriteFile(filepath.Join(dir, "memory", "manifest.yaml"), []byte("{{{ not yaml [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := installShadowWarnDir
	installShadowWarnDir = func() string { return dir }
	t.Cleanup(func() { installShadowWarnDir = prev })

	var buf bytes.Buffer
	err := NewAPI().Install(InstallOpts{Server: "memory", DryRun: true, Writer: &buf})
	if !strings.Contains(buf.String(), `disk manifest for shipped server "memory" is ignored`) {
		t.Fatalf("install writer output missing the shadow warn; output:\n%s", buf.String())
	}
	if err != nil && strings.Contains(err.Error(), "parse") {
		t.Fatalf("install error looks like a disk-manifest parse failure — embed-wins violated: %v", err)
	}
}

// F3b (Fable pre-bot review) — emit-site test for the InstallAllWithOpts lane:
// installUsingEmbedFirst IS the per-server entry InstallAllWithOpts drives for
// every embedded name (install.go:308) and owns that lane's warn emit, so it is
// exercised DIRECTLY with the one shadowed server. A full InstallAllWithOpts
// dry-run was measured at ~8s of scheduler/port probing PER embedded server on
// a live Windows host (16 servers → >2 min wall + a per-server exec-spawn
// storm) for zero additional emit-site coverage — the loop driver contains no
// warn logic of its own. Deleting the emit in installUsingEmbedFirst fails
// this test.
func TestInstallUsingEmbedFirst_EmitsEmbeddedDiskShadowWarnToWriter(t *testing.T) {
	tmpState := t.TempDir()
	t.Setenv("LOCALAPPDATA", tmpState)
	t.Setenv("XDG_DATA_HOME", tmpState)
	t.Setenv("XDG_STATE_HOME", tmpState)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Differing AND unparseable — same embed-wins discriminator as the Install
	// emit test above.
	if err := os.WriteFile(filepath.Join(dir, "memory", "manifest.yaml"), []byte("{{{ not yaml [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := installShadowWarnDir
	installShadowWarnDir = func() string { return dir }
	t.Cleanup(func() { installShadowWarnDir = prev })

	var buf bytes.Buffer
	err := NewAPI().installUsingEmbedFirst(InstallOpts{Server: "memory", DryRun: true, Writer: &buf})
	if !strings.Contains(buf.String(), `disk manifest for shipped server "memory" is ignored`) {
		t.Fatalf("bulk-install per-server writer output missing the shadow warn; output:\n%s", buf.String())
	}
	if err != nil && strings.Contains(err.Error(), "parse") {
		t.Fatalf("install error looks like a disk-manifest parse failure — embed-wins violated: %v", err)
	}
	// A non-shadowed shipped server stays silent through the same entry (the F2
	// anti-spam posture): no disk file under the seam dir → no warn line.
	var quiet bytes.Buffer
	_ = NewAPI().installUsingEmbedFirst(InstallOpts{Server: "fetch", DryRun: true, Writer: &quiet})
	if strings.Contains(quiet.String(), "is ignored; the shipped built-in manifest") {
		t.Errorf("non-shadowed server must not warn; output:\n%s", quiet.String())
	}
}

// F3c (Fable pre-bot review) — emit-site test for scan's readManifestNames on
// the EMBED-FIRST path (dir == ""): the warn reaches the log sink for a
// differing colliding file under the embed-first shadow dir, and the name still
// lands in the scanned set (warn, never drop). Seeded via the scanShadowWarnDir
// seam (defaultManifestDir is exe-derived, not seedable).
func TestReadManifestNames_EmbedFirstPathLogsEmbeddedDiskShadowWarn(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "wolfram"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A DIFFERING copy of the shipped wolfram manifest (append a comment) so the
	// byte-compare in embeddedDiskShadowWarning does not suppress it.
	embed, err := fs.ReadFile(servers.Manifests, "wolfram/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wolfram", "manifest.yaml"), append(embed, []byte("\n# operator edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	prevDir := scanShadowWarnDir
	scanShadowWarnDir = func() string { return dir }
	t.Cleanup(func() { scanShadowWarnDir = prevDir })

	var logBuf bytes.Buffer
	prevLog := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prevLog) })

	// dir == "" → embed-first path (listManifestNamesEmbedFirst yields wolfram).
	names, err := readManifestNames("")
	if err != nil {
		t.Fatalf("readManifestNames: %v", err)
	}
	if !names["wolfram"] {
		t.Fatal("wolfram missing from the scanned name set — the warn must never drop the row")
	}
	if !strings.Contains(logBuf.String(), `disk manifest for shipped server "wolfram" is ignored`) {
		t.Fatalf("scan log sink missing the embed-first shadow warn; log:\n%s", logBuf.String())
	}
}

// TestReadManifestNames_ExplicitDirDoesNotWarnShadow (bot PR #494 P3) — an
// EXPLICIT dir scan reads dir/name/manifest.yaml DIRECTLY (loadManifestForServer
// non-empty-dir branch, migrate.go), so the disk manifest is actually USED, not
// shadowed by embed — the "ignored" warn must NOT fire in that mode.
func TestReadManifestNames_ExplicitDirDoesNotWarnShadow(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "name: memory\nkind: global\ntransport: stdio-bridge\ncommand: node\ndaemons:\n  - name: default\n    port: 9410\n"
	if err := os.WriteFile(filepath.Join(dir, "memory", "manifest.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	names, err := readManifestNames(dir)
	if err != nil {
		t.Fatalf("readManifestNames: %v", err)
	}
	if !names["memory"] {
		t.Fatal("memory missing from the scanned name set")
	}
	if strings.Contains(logBuf.String(), "is ignored") {
		t.Fatalf("explicit-dir scan emitted a FALSE shadow warn (the disk manifest is actually used in that mode); log:\n%s", logBuf.String())
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

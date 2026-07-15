package api

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"mcp-local-hub/servers"
)

// deAdoptManifestReadDir is the filesystem seam for the de-adopt-only strict
// manifest enumeration. Production uses os.ReadDir; executor tests replace it
// to reproduce a non-ENOENT directory-listing failure deterministically.
var deAdoptManifestReadDir = os.ReadDir

// manifestDirForTests is a test-only override consulted by
// ScanManifestEnv and the embed-aware helpers when set via
// MCPHUB_MANIFEST_DIR_OVERRIDE. When the override is non-empty the
// embed FS is bypassed entirely; tests get the test directory's
// manifests with no leakage from the binary's shipped set
// (which include `secret:` refs from wolfram, paper-search-mcp).
func manifestDirForTests() string {
	return os.Getenv("MCPHUB_MANIFEST_DIR_OVERRIDE")
}

// Manifest-source abstraction.
//
// Before this file existed, read-side API calls (ManifestList,
// ManifestGet, Install, scan) resolved manifests through defaultManifestDir()
// — a heuristic that searches for a `servers/` directory next to the
// running binary. The daemon (cli/daemon.go) read manifests from the
// servers.Manifests embed instead. Two sources of truth → split-brain:
// canonical ~/.local/bin/mcphub.exe saw 0 servers from disk when invoked
// from %TEMP% even though 10 were embedded in the binary.
//
// Fix: all read paths go through embeddedManifestNames /
// loadManifestYAMLEmbedFirst, which prefer the embed FS (the source of
// truth shipped with the binary) and fall back to disk only when the
// embed is empty (dev-checkout dev-flow without a rebuild).
//
// Write paths (ManifestCreate / ManifestEdit / ManifestDelete) continue
// to use disk. Editing the embedded FS at runtime is impossible; write
// ops are a dev-workflow feature and documented as such.

// embeddedManifestNames returns the sorted list of server names that
// have a manifest.yaml baked into the binary.
func embeddedManifestNames() []string {
	entries, err := fs.ReadDir(servers.Manifests, ".")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// embed.FS contains only paths declared in //go:embed, so every
		// subdirectory here is guaranteed to have a manifest.yaml.
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// embeddedManifestNamesContains reports RAW membership in the binary's embed
// FS — the OVERRIDE-INDEPENDENT core of the embed-name question. It reads the
// real embed set directly (embeddedManifestNames ignores
// MCPHUB_MANIFEST_DIR_OVERRIDE), so it answers "is this exact name baked into
// servers.Manifests?" regardless of any test seam.
//
// CatalogManifestGet dedups its inline membership loop onto this core: that
// endpoint is DELIBERATELY override-independent (it reads the embed FS directly
// for secret-safety, so a disk manifest with a literal secret is unreachable),
// so it must NOT go through the override-symmetric isEmbeddedManifestName below.
func embeddedManifestNamesContains(name string) bool {
	for _, n := range embeddedManifestNames() {
		if n == name {
			return true
		}
	}
	return false
}

// isEmbeddedManifestName is the WRITE-GATE / warn / readiness / marketplace
// membership predicate: it mirrors loadManifestYAMLEmbedFirst's embed branch
// EXACTLY, INCLUDING the test-override symmetry. When
// MCPHUB_MANIFEST_DIR_OVERRIDE is set the loader bypasses the embed FS and
// reads the override dir (manifest_source.go:77-79), so for the purposes of
// "would a disk create/read shadow a SHIPPED manifest?" the answer is false —
// there is no shipped manifest in play under the override. That keeps the
// predicate byte-symmetric with the read path it guards, so hermetic tests
// that seed a temp manifest dir via the override are unaffected by the
// embed-name write refusal.
//
// It is layered on the override-independent core embeddedManifestNamesContains,
// so the raw membership loop is single-owner; only the override symmetry is
// added here (that is the sole difference from the CatalogManifestGet core).
func isEmbeddedManifestName(name string) bool {
	if manifestDirForTests() != "" {
		return false
	}
	return embeddedManifestNamesContains(name)
}

// IsEmbeddedManifestName is the exported gate for callers outside this package
// (the internal/gui readiness Save-&-Install dry-run mirror) that must reflect
// the same embed-membership decision the ManifestCreateIn write gate applies.
// (The marketplace handler does NOT use this predicate — it maps the write
// gate's ErrManifestNameEmbedded refusal via errors.Is instead.) It mirrors
// the CheckManifestName/checkManifestName exported-wrapper idiom so the
// predicate stays single-owner across package boundaries.
func IsEmbeddedManifestName(name string) bool {
	return isEmbeddedManifestName(name)
}

// embeddedDiskShadowWarning returns a human-readable warn line when `name` is a
// shipped (embedded) server AND a same-named manifest.yaml ALSO exists on disk
// under manifestDir — the disk-fallback location the embed-first loader
// (loadManifestYAMLEmbedFirst) consults only on an embed MISS. Because the
// embed read wins for a shipped name, that disk manifest is silently ignored at
// install: the operator's edits never take effect. The warn says so and points
// at the rename remedy.
//
// This is the READ-side surface for PRE-EXISTING collisions (new ones are
// refused at the ManifestCreateIn write gate); it NEVER deletes the user's
// file. Empty string == no collision (name not embedded — which includes the
// test-override case via isEmbeddedManifestName — or no disk file), so callers
// emit nothing. Single owner of the collision predicate + message; each
// consumer routes the returned string to its own sink (install → writer,
// scan → log).
//
// IDENTICAL-BYTES SUPPRESSION: a disk copy whose bytes EQUAL the embed
// manifest changes nothing — the embed read serving instead of the disk read
// is unobservable — so warning about it is pure noise. The dominant producer
// of identical copies is a dev checkout / built binary sitting next to the
// source `servers/` tree, where defaultManifestDir() resolves to the source
// tree and EVERY shipped server would otherwise warn on every install --all
// and every scan. Only a DIFFERING disk copy (the operator actually edited
// something the embed read silently discards) warns.
func embeddedDiskShadowWarning(name, manifestDir string) string {
	if !isEmbeddedManifestName(name) {
		return ""
	}
	if manifestDir == "" {
		return ""
	}
	// isEmbeddedManifestName(name) == true implies `name` literally matched an
	// embed FS directory entry, so it is join-safe by construction (no
	// separators / traversal). Read the disk shadow directly: any read failure
	// (IsNotExist, permission) means there is no observable shadow to warn
	// about — same no-warn outcome the earlier stat-based check produced.
	diskBytes, err := os.ReadFile(filepath.Join(manifestDir, name, "manifest.yaml"))
	if err != nil {
		return ""
	}
	// Identical bytes → pure noise, skip (see header). An embed read failure
	// cannot really happen for a name proven in the embed set; if it somehow
	// does, fall through to the truthful warn (conservative).
	if embedBytes, err := fs.ReadFile(servers.Manifests, name+"/manifest.yaml"); err == nil && bytes.Equal(diskBytes, embedBytes) {
		return ""
	}
	return fmt.Sprintf(
		"disk manifest for shipped server %q is ignored; the shipped built-in manifest is what installs — rename your copy (e.g. %q) to install a customized version",
		name, name+"-custom",
	)
}

// loadManifestYAMLEmbedFirst returns the raw YAML bytes for the named
// server. Consults the embed FS first; on miss (server not in the
// binary's shipped set), falls back to the on-disk dev-checkout path.
//
// checkManifestName runs at the loader boundary so a bad server name
// cannot drive a pre-validation filesystem probe (existence check,
// special-file open, etc.) via callers that validated only after the
// raw read. Production wrappers ManifestGet/ManifestEdit/etc. already
// gate on the same regex, so the redundant call here is cheap; the
// guard exists for the direct callers (install, uninstall, scan,
// status_enrich, secrets_scan, migrate) that compose `name` straight
// into the loader without their own pre-load check.
func loadManifestYAMLEmbedFirst(name string) ([]byte, error) {
	if err := checkManifestName(name); err != nil {
		return nil, err
	}
	if dir := manifestDirForTests(); dir != "" {
		// Test-only override: skip the embed FS entirely.
		return os.ReadFile(filepath.Join(dir, name, "manifest.yaml"))
	}
	if data, err := fs.ReadFile(servers.Manifests, name+"/manifest.yaml"); err == nil {
		return data, nil
	}
	// Disk fallback for dev flow (e.g. brand-new manifest not yet compiled in).
	path := filepath.Join(defaultManifestDir(), name, "manifest.yaml")
	return os.ReadFile(path)
}

// listManifestNamesEmbedFirst returns the set of available server
// names, unioning embed and disk so a dev-added manifest still shows
// up before a rebuild.
func listManifestNamesEmbedFirst() ([]string, error) {
	if dir := manifestDirForTests(); dir != "" {
		// Test-only override: skip the embed FS entirely so tests get
		// only the manifests they explicitly seed.
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		var names []string
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "manifest.yaml")); err == nil {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		return names, nil
	}
	// Production path: union embed + disk.
	seen := map[string]bool{}
	for _, n := range embeddedManifestNames() {
		seen[n] = true
	}
	// Union with disk so dev-created manifests appear before they are
	// compiled into the binary.
	entries, err := os.ReadDir(defaultManifestDir())
	if err != nil && !os.IsNotExist(err) {
		// Disk read failure is non-fatal — return what we have from embed.
		// The common case on an installed binary with no source tree is
		// that defaultManifestDir() does not exist.
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(defaultManifestDir(), e.Name(), "manifest.yaml")); err == nil {
			seen[e.Name()] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// listManifestNamesEmbedFirstStrict is the fail-closed manifest enumerator for
// de-adopt's routed-secret scan. Unlike the install-facing forgiving lister, a
// non-ENOENT disk listing failure is fatal because an incomplete manifest set
// could make a shared routed secret appear unique. Every directory name is
// included without a manifest.yaml stat gate so a later load failure reaches
// de-adopt's existing conservative preserve-all-candidates branch.
func listManifestNamesEmbedFirstStrict() ([]string, error) {
	dir := manifestDirForTests()
	seen := map[string]bool{}
	if dir == "" {
		for _, name := range embeddedManifestNames() {
			seen[name] = true
		}
		dir = defaultManifestDir()
	}

	entries, err := deAdoptManifestReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			seen[entry.Name()] = true
		}
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

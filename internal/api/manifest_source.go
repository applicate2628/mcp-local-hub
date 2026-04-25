package api

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"mcp-local-hub/servers"
)

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

// loadManifestYAMLEmbedFirst returns the raw YAML bytes for the named
// server. Consults the embed FS first; on miss (server not in the
// binary's shipped set), falls back to the on-disk dev-checkout path.
func loadManifestYAMLEmbedFirst(name string) ([]byte, error) {
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

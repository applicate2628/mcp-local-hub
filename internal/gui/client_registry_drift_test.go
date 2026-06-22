package gui

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// TestRoutingTSClientListMirrorsBackendRegistry is the cross-language drift
// guard for the Servers-matrix client universe. The frontend routing module
// (internal/gui/frontend/src/lib/routing.ts) declares CORE_CLIENTS (the seven
// always-shown columns) and NON_CORE_CLIENTS (every other supported client,
// detection-gated). Their union MUST equal clients.SupportedClientNames(); if
// a backend client is added without mirroring it in NON_CORE_CLIENTS, that
// client's matrix column, column-toggle entry, binding-editor row, and
// Backups group silently disappear from the GUI even though scan/routing
// surface it dynamically. CORE_CLIENTS must additionally be the first seven
// registry entries, in order (the matrix renders them unconditionally).
//
// This is the test the routing.ts comment names. It parses the TS source
// rather than importing it (Go cannot import TS), keying on the two exported
// array literals. The DETECTION universe in routing.ts is derived live from
// the scan presence map, so a missing NON_CORE_CLIENTS entry does not break
// detection — but it DOES break the static authorities listed above, which is
// what this guard protects.
func TestRoutingTSClientListMirrorsBackendRegistry(t *testing.T) {
	path := filepath.Join("frontend", "src", "lib", "routing.ts")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(data)

	core := extractTSStringArray(t, src, "CORE_CLIENTS")
	nonCore := extractTSStringArray(t, src, "NON_CORE_CLIENTS")

	registry := clients.SupportedClientNames()
	if len(registry) < len(core) {
		t.Fatalf("registry has %d clients, fewer than CORE_CLIENTS (%d)", len(registry), len(core))
	}

	// CORE_CLIENTS must be exactly the first len(core) registry entries, in order.
	for i, name := range core {
		if registry[i] != name {
			t.Errorf("CORE_CLIENTS[%d] = %q, but SupportedClientNames()[%d] = %q (core columns must be the leading registry entries in order)", i, name, i, registry[i])
		}
	}

	// CORE ++ NON_CORE must equal the registry as a set (no missing, no extra).
	tsSet := map[string]bool{}
	for _, n := range core {
		tsSet[n] = true
	}
	for _, n := range nonCore {
		if tsSet[n] {
			t.Errorf("client %q appears in both CORE_CLIENTS and NON_CORE_CLIENTS", n)
		}
		tsSet[n] = true
	}
	registrySet := map[string]bool{}
	for _, n := range registry {
		registrySet[n] = true
	}
	for _, n := range registry {
		if !tsSet[n] {
			t.Errorf("backend client %q is missing from routing.ts (add it to NON_CORE_CLIENTS) — its GUI column/toggle/binding-row/backups-group would be invisible", n)
		}
	}
	for n := range tsSet {
		if !registrySet[n] {
			t.Errorf("routing.ts lists client %q that is not in clients.SupportedClientNames() (stale/removed client)", n)
		}
	}
}

// TestMatrixScannableUniverseMatchesBackendCapabilities is the drift guard for
// the Servers-matrix SCANNABLE universe. The matrix now shows a non-core
// column ONLY for a SCANNABLE client (one with a clientScanners() parser), and
// the frontend derives "scannable" live from api.ClientCapabilities() carried
// on the scan result. This test pins the two ends together:
//
//  1. Every scannable client (the matrix's potential column universe) is
//     declared in routing.ts (CORE_CLIENTS ∪ NON_CORE_CLIENTS), so a
//     scannable client always has its column-toggle/binding-row/Backups-group
//     authority entry — a scannable client missing from the static list would
//     be a half-wired column.
//  2. Every client routing.ts declares is a real SupportedClientNames() id
//     (already pinned above) AND its capability entry exists, so the runtime
//     scannable gate has a value to read for each declared client.
//
// Because the runtime scannable decision reads api.ClientCapabilities()
// (single owner, == clientScanners() keys), a backend that registers a new
// parser surfaces the column automatically; this guard ensures the static TS
// authorities don't fall behind that scannable set.
func TestMatrixScannableUniverseMatchesBackendCapabilities(t *testing.T) {
	path := filepath.Join("frontend", "src", "lib", "routing.ts")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(data)

	tsSet := map[string]bool{}
	for _, n := range extractTSStringArray(t, src, "CORE_CLIENTS") {
		tsSet[n] = true
	}
	for _, n := range extractTSStringArray(t, src, "NON_CORE_CLIENTS") {
		tsSet[n] = true
	}

	caps := api.ClientCapabilities()
	if len(caps) == 0 {
		t.Fatal("api.ClientCapabilities() returned an empty map")
	}

	scannableCount := 0
	for name, cap := range caps {
		if !cap.Scannable {
			continue
		}
		scannableCount++
		if !tsSet[name] {
			t.Errorf("scannable client %q (has a clientScanners() parser) is missing from routing.ts CORE/NON_CORE — its Servers column authorities would be unwired", name)
		}
	}
	// Sanity: the scannable set must be a strict, non-trivial subset — at
	// least the 7 core clients (all have parsers), but fewer than the full
	// registry (Finding 3: the no-scanner clients are excluded). A regression
	// that made every client scannable (or none) would defeat the gate.
	if scannableCount < len(extractTSStringArray(t, src, "CORE_CLIENTS")) {
		t.Errorf("scannable client count = %d, expected at least the core clients (all core clients have parsers)", scannableCount)
	}
	if scannableCount >= len(caps) {
		t.Errorf("scannable client count = %d equals the full registry %d — the no-scanner exclusion (Finding 3) is not in effect", scannableCount, len(caps))
	}
}

// extractTSStringArray pulls the string literals out of an exported TS array
// declaration of the form:
//
//	export const NAME = [
//	  "a",
//	  "b", // comment
//	] as const;
//
// It locates `export const NAME = [` then reads every double-quoted token up
// to the closing `]`. Order is preserved. Fails the test if the declaration
// is absent or empty so a rename/removal cannot make this guard vacuous.
func extractTSStringArray(t *testing.T, src, name string) []string {
	t.Helper()
	startRe := regexp.MustCompile(`export const ` + regexp.QuoteMeta(name) + `\s*=\s*\[`)
	loc := startRe.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("could not find `export const %s = [` in routing.ts", name)
	}
	rest := src[loc[1]:]
	end := indexOfRune(rest, ']')
	if end < 0 {
		t.Fatalf("unterminated array literal for %s in routing.ts", name)
	}
	body := rest[:end]
	tokenRe := regexp.MustCompile(`"([^"]+)"`)
	matches := tokenRe.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatalf("%s array literal is empty in routing.ts", name)
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// indexOfRune returns the byte index of the first occurrence of r in s, or -1.
// The array bodies contain no nested ']' (string tokens only), so a plain
// first-']' scan correctly delimits the literal.
func indexOfRune(s string, r rune) int {
	for i, c := range s {
		if c == r {
			return i
		}
	}
	return -1
}

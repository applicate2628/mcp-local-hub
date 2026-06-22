package gui

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

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

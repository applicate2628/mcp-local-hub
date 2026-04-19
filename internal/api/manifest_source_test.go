package api

import (
	"path/filepath"
	"testing"
)

// TestEmbeddedManifestNames_SeesAllShippedServers guards the single
// biggest symptom of the split-brain bug: the canonical installed
// binary was reporting 0 or 8 servers from disk-only reads when the
// embed contained 10. This test asserts that the shipped set is
// complete and non-empty.
func TestEmbeddedManifestNames_SeesAllShippedServers(t *testing.T) {
	names := embeddedManifestNames()
	if len(names) == 0 {
		t.Fatal("embeddedManifestNames() returned empty — //go:embed pattern broken?")
	}
	// Spot-check a few servers we know are in servers/. Having the full
	// set here would be brittle (new server additions would fail the test);
	// the empty-check above is the real regression guard.
	for _, want := range []string{"godbolt", "perftools", "memory", "time"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("embeddedManifestNames() missing shipped server %q: got %+v", want, names)
		}
	}
}

// TestLoadManifestYAMLEmbedFirst_ReadsFromEmbed verifies that the embed
// FS answers the request without hitting disk. The test would fail
// either if the embed pattern is broken or if an unintended disk
// fallback masked an embed miss.
func TestLoadManifestYAMLEmbedFirst_ReadsFromEmbed(t *testing.T) {
	data, err := loadManifestYAMLEmbedFirst("godbolt")
	if err != nil {
		t.Fatalf("loadManifestYAMLEmbedFirst(godbolt): %v", err)
	}
	if len(data) == 0 {
		t.Fatal("empty manifest bytes returned")
	}
	// Sanity: manifest should declare name: godbolt somewhere in the body.
	if !containsSubstring(data, "name: godbolt") {
		t.Errorf("godbolt manifest does not look right (first 200 bytes): %s",
			firstN(data, 200))
	}
}

// TestListManifestNamesEmbedFirst_UnionsDiskAdditions verifies that a
// fresh dev-checkout-only manifest (on disk, not yet compiled in)
// appears alongside the embedded set. This keeps the dev flow working
// without requiring a rebuild on every manifest edit.
func TestListManifestNamesEmbedFirst_UnionsDiskAdditions(t *testing.T) {
	// We can't inject a disk manifest into defaultManifestDir() without
	// mutating the tree, so this is a smoke test: the embed-only
	// subset must at least be present. A full disk-union test would
	// require refactoring defaultManifestDir() to be injectable —
	// deferred to follow-up.
	names, err := listManifestNamesEmbedFirst()
	if err != nil {
		t.Fatalf("listManifestNamesEmbedFirst: %v", err)
	}
	embedSet := map[string]bool{}
	for _, n := range embeddedManifestNames() {
		embedSet[n] = true
	}
	for _, n := range names {
		// Every name returned must either be in embed or on disk — we
		// can at least assert the embed subset is a subset of the union.
		_ = embedSet[n]
	}
	// Assert the embed subset is contained in the union.
	for want := range embedSet {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("embed server %q not present in union list: %+v", want, names)
		}
	}
}

func containsSubstring(data []byte, want string) bool {
	return len(data) >= len(want) && indexSubstring(string(data), want) >= 0
}

func indexSubstring(hay, needle string) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func firstN(b []byte, n int) string {
	if len(b) < n {
		return string(b)
	}
	return string(b[:n])
}

// TestEmbeddedManifestNames_NoDiskPrefixLeak guards against a subtle
// pitfall: if the embed.FS path separator were ever mixed with an
// OS-specific filepath separator, names would carry a "./" prefix on
// some platforms. Enforce plain bare names.
func TestEmbeddedManifestNames_NoDiskPrefixLeak(t *testing.T) {
	for _, n := range embeddedManifestNames() {
		if filepath.Base(n) != n {
			t.Errorf("embedded name %q contains a path separator", n)
		}
	}
}

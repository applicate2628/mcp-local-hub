package api

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// docsOnlyCatalogIDs are the S4 manual-install POINTER rows the v2 catalog carries
// in its SEPARATE top-level docs_only[] array — servers the hub never installs
// (immature, git-clone-only, macOS-only, archived upstream, in-app-add-on/workbench
// install, paid-edition requirement, or a LAN-bind risk), so they are discoverable
// but install-inert by construction. They live OUT of entries[] so a released v1-only
// client (which knows only stdio/native-http/http) ignores the unknown top-level
// docs_only key instead of rejecting the WHOLE catalog on an unknown entry transport
// (bot #446 P1). The vendor-breadth wave-2b batch appends 10 more pointers (office +
// 3D/CAD + creative + flashcards), each license-confirmed via gh and docs-only for a
// stated reason (very-low maturity, archived upstream, in-app add-on/workbench load,
// paid Studio edition, or an archived/ambiguously-licensed dependency).
var docsOnlyCatalogIDs = []string{
	"comsol", "solidworks", "autocad", "guitarpro", "flstudio",
	"bitwig", "midi", "logicpro", "cubase",
	// wave-2b additions.
	"mathematica", "word", "powerpoint", "blender", "freecad",
	"fusion360", "rhino", "davinci", "audacity", "anki",
}

// TestDocsOnlyPointerText_EmitsPointerNotManifest pins the S4 pointer text: a
// DocsOnlyEntry projects onto a human-readable POINTER block — homepage + readme +
// the verbatim manual_install steps — and NEVER a YAML manifest. It must carry NO
// manifest keys so it can't be piped into `mcphub manifest create`.
func TestDocsOnlyPointerText_EmitsPointerNotManifest(t *testing.T) {
	d := &DocsOnlyEntry{
		ID:            "cubase",
		Name:          "Cubase MCP server (docs-only)",
		Summary:       "Drive Cubase from an AI client.",
		Homepage:      "https://github.com/hedidjs/cubase-mcp",
		ReadmeURL:     "https://raw.githubusercontent.com/hedidjs/cubase-mcp/main/README.md",
		ManualInstall: "git clone the repo and configure a virtual-MIDI port in Cubase.",
	}
	got, err := DocsOnlyPointerText(d)
	if err != nil {
		t.Fatalf("DocsOnlyPointerText: %v", err)
	}
	for _, want := range []string{
		"DOCS-ONLY pointer",
		"https://github.com/hedidjs/cubase-mcp",
		"https://raw.githubusercontent.com/hedidjs/cubase-mcp/main/README.md",
		"git clone the repo and configure a virtual-MIDI port in Cubase.",
		"Manual setup steps:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("docs-only pointer missing %q\n---\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"kind: global",
		"transport:",
		"command:",
		"base_args:",
		"daemons:",
		"client_bindings:",
		"port:",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("docs-only pointer must NOT contain manifest key %q (never a draftable manifest)\n---\n%s", forbidden, got)
		}
	}
}

// TestDocsOnlyPointerText_RejectsHostileRunes proves the pointer text routes every
// interpolated field through the SAME untrusted-rune owner the stdio/http draft arms
// use, so a hostile registry cannot inject a terminal escape / control byte into the
// operator's stdout / HTTP body via the manual_install (or any other) field.
func TestDocsOnlyPointerText_RejectsHostileRunes(t *testing.T) {
	base := func() *DocsOnlyEntry {
		return &DocsOnlyEntry{ID: "evil", Name: "Evil", Summary: "ok", Homepage: "https://example.com"}
	}
	// An ESC (U+001B) in manual_install — a classic terminal-escape injection.
	d := base()
	d.ManualInstall = "step1\x1b[2Jclobber"
	if _, err := DocsOnlyPointerText(d); err == nil {
		t.Fatal("DocsOnlyPointerText accepted a manual_install carrying an ESC control byte; want refusal")
	}
	// A bidi override (U+202E) in summary — Trojan-Source class.
	d2 := base()
	d2.Summary = "good‮dab"
	if _, err := DocsOnlyPointerText(d2); err == nil {
		t.Fatal("DocsOnlyPointerText accepted a summary carrying a bidi-override rune; want refusal")
	}
}

// TestValidateDocsOnlyEntry_Shape pins the S4 docs_only validation: a docs_only row
// MUST carry id + name + homepage + summary; the id must pass the name gate. The
// type itself has no transport/command/args/url fields, so install-field carriage is
// unrepresentable rather than checked.
func TestValidateDocsOnlyEntry_Shape(t *testing.T) {
	ok := &DocsOnlyEntry{ID: "ok-row", Name: "OK", Summary: "why docs-only", Homepage: "https://example.com"}
	if err := validateDocsOnlyEntry(ok); err != nil {
		t.Fatalf("valid docs_only row rejected: %v", err)
	}
	cases := []struct {
		name    string
		mutate  func(d *DocsOnlyEntry)
		wantSub string
	}{
		{"missing id", func(d *DocsOnlyEntry) { d.ID = "" }, "missing id"},
		{"missing name", func(d *DocsOnlyEntry) { d.Name = "" }, "missing name"},
		{"missing homepage", func(d *DocsOnlyEntry) { d.Homepage = "" }, "homepage"},
		{"missing summary", func(d *DocsOnlyEntry) { d.Summary = "" }, "summary"},
		{"bad id name", func(d *DocsOnlyEntry) { d.ID = "Bad Name!" }, "manifest-name gate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &DocsOnlyEntry{ID: "row", Name: "Row", Summary: "s", Homepage: "https://example.com"}
			tc.mutate(d)
			err := validateDocsOnlyEntry(d)
			if err == nil {
				t.Fatalf("validateDocsOnlyEntry accepted invalid row (%s); want rejection", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q, want it to mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestDocsOnlyRequiresSchemaV2 proves the docs_only TOP-LEVEL key is gated to
// schema_version 2: a v1 catalog carrying docs_only (even present-empty) is rejected
// on the fetch path. This is the forward-compat-CORRECT gate (it REPLACES the prior
// per-entry transport gate, bot #446 P1) — a v1 catalog must never carry docs_only.
func TestDocsOnlyRequiresSchemaV2(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "v1 with a populated docs_only row",
			raw:  `{"schema_version":"1","entries":[],"docs_only":[{"id":"x","name":"X","homepage":"https://example.com","summary":"s"}]}`,
		},
		{
			// present-empty docs_only on a v1 catalog still breaks an older v1-only
			// DisallowUnknownFields client on the bare key, so it must be rejected too.
			name: "v1 with a present-empty docs_only array",
			raw:  `{"schema_version":"1","entries":[],"docs_only":[]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMarketplaceCatalog([]byte(tc.raw))
			if err == nil {
				t.Fatalf("v1 catalog carrying docs_only was accepted (%s); want rejection (the key is v2-only)", tc.name)
			}
			if !strings.Contains(err.Error(), "schema_version") || !strings.Contains(err.Error(), "docs_only") {
				t.Fatalf("error = %q, want it to mention both docs_only and schema_version", err.Error())
			}
		})
	}

	// A v2 catalog with the same docs_only row IS accepted.
	v2raw := `{"schema_version":"2","entries":[],"docs_only":[{"id":"x","name":"X","homepage":"https://example.com","summary":"s"}]}`
	cat, err := ParseMarketplaceCatalog([]byte(v2raw))
	if err != nil {
		t.Fatalf("v2 catalog with a docs_only row was rejected; want acceptance: %v", err)
	}
	if len(cat.DocsOnly) != 1 || cat.DocsOnly[0].ID != "x" {
		t.Fatalf("v2 docs_only row not parsed: %#v", cat.DocsOnly)
	}
}

// TestDocsOnly_IdCollisionAcrossArraysRejected proves the docs_only id namespace is
// shared with entries[]: an id present in BOTH arrays (or twice in docs_only) is a
// duplicate-id rejection, so the GUI never renders two cards / the install loader is
// never ambiguous.
func TestDocsOnly_IdCollisionAcrossArraysRejected(t *testing.T) {
	raw := `{"schema_version":"2","entries":[{"id":"dup","name":"E","transport":"stdio","command":"uvx","args":["x"]}],"docs_only":[{"id":"dup","name":"D","homepage":"https://example.com","summary":"s"}]}`
	_, err := ParseMarketplaceCatalog([]byte(raw))
	if err == nil {
		t.Fatal("catalog with an id in BOTH entries[] and docs_only[] was accepted; want duplicate-id rejection")
	}
	if !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("error = %q, want it to mention duplicate id", err.Error())
	}
}

// ---------------------------------------------------------------------------
// FORWARD-COMPAT PROOF (the load-bearing P1 test, bot #446)
// ---------------------------------------------------------------------------

// TestForwardCompat_DeployedParserIgnoresDocsOnlyTopKey is THE load-bearing proof
// the P1 demands: a released client whose parser is on the DEPLOYED fetch path
// (DisallowUnknownFields OFF) loads a v2 catalog that carries the NEW top-level
// docs_only[] array WITHOUT error, and its entries[] parse clean — i.e. publishing
// docs_only rows does NOT break already-shipped clients. The companion negative
// proves the OLD (rejected) shape: a docs-only row INSIDE entries[] (the wrong
// abstraction) makes ANY parser that knows only stdio/native-http/http reject the
// WHOLE catalog (this is what the move fixed).
func TestForwardCompat_DeployedParserIgnoresDocsOnlyTopKey(t *testing.T) {
	// The CORRECT shape: docs_only as a separate top-level key. The deployed fetch
	// decode leaves DisallowUnknownFields OFF, so an OLDER client that has no
	// DocsOnly field on its struct simply IGNORES the unknown top-level key — the
	// catalog (and its entries[]) load clean. We model the deployed decode with
	// ParseMarketplaceCatalog, which is exactly that path.
	correct := `{
	  "schema_version": "2",
	  "entries": [
	    {"id":"filesystem","name":"Filesystem","transport":"stdio","command":"npx","args":["-y","@modelcontextprotocol/server-filesystem"]}
	  ],
	  "docs_only": [
	    {"id":"cubase","name":"Cubase (docs-only)","homepage":"https://github.com/hedidjs/cubase-mcp","summary":"manual-install pointer","manual_install":"git clone and configure virtual-MIDI"}
	  ]
	}`
	cat, err := ParseMarketplaceCatalog([]byte(correct))
	if err != nil {
		t.Fatalf("DEPLOYED parser rejected a v2 catalog carrying the new top-level docs_only key; the P1 forward-compat property is BROKEN: %v", err)
	}
	if len(cat.Entries) != 1 || cat.Entries[0].ID != "filesystem" {
		t.Fatalf("entries[] did not load clean alongside docs_only: %#v", cat.Entries)
	}

	// Model an OLD client whose struct has NO DocsOnly field but otherwise matches
	// the deployed decode (DisallowUnknownFields OFF): the unknown top-level
	// docs_only key is IGNORED and entries[] still parse. This is the literal
	// "released client doesn't break" proof.
	type oldMarketplaceEntry struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Transport string `json:"transport"`
	}
	type oldMarketplaceCatalog struct {
		SchemaVersion string                `json:"schema_version"`
		Entries       []oldMarketplaceEntry `json:"entries"`
		// NOTE: deliberately NO docs_only field — this models a pre-S4 client.
	}
	var oldCat oldMarketplaceCatalog
	// json.Unmarshal (no DisallowUnknownFields) ignores unknown keys — exactly the
	// deployed fetch posture (parseMarketplaceCatalog leaves it off).
	if err := json.Unmarshal([]byte(correct), &oldCat); err != nil {
		t.Fatalf("an OLD client (no docs_only field, DisallowUnknownFields OFF) failed to parse the v2 catalog; forward-compat BROKEN: %v", err)
	}
	if oldCat.SchemaVersion != "2" || len(oldCat.Entries) != 1 || oldCat.Entries[0].ID != "filesystem" {
		t.Fatalf("OLD client did not load entries[] clean: %#v", oldCat)
	}

	// NEGATIVE control — the WRONG (old) shape that the move FIXED: a docs-only row
	// INSIDE entries[]. ANY parser that validates entry transports (every shipped
	// client + ours) rejects the WHOLE catalog on the unknown transport, so the
	// marketplace would be EMPTY for all shipped clients. Prove our parser rejects
	// it, which is the same failure a released client hits.
	wrong := `{
	  "schema_version": "2",
	  "entries": [
	    {"id":"filesystem","name":"Filesystem","transport":"stdio","command":"npx","args":["-y","x"]},
	    {"id":"cubase","name":"Cubase","transport":"docs-only","homepage":"https://example.com","summary":"s"}
	  ]
	}`
	if _, err := ParseMarketplaceCatalog([]byte(wrong)); err == nil {
		t.Fatal("the WRONG shape (docs-only INSIDE entries[]) was accepted; it must be rejected (an unknown entry transport breaks every shipped client) — this is exactly what moving to docs_only[] fixed")
	}
}

// TestV2DocsOnlyRows_PresentInPointerArray is the S4 catalog-data acceptance: the v2
// catalog carries the 9 docs_only POINTER rows in the SEPARATE top-level docs_only[]
// array (NOT entries[]); each carries id/name/homepage/summary/manual_install, the
// confirmed license, and NO install fields (unrepresentable on the type). It also
// proves none of the 9 ids leaked into entries[].
func TestV2DocsOnlyRows_PresentInPointerArray(t *testing.T) {
	raw, err := os.ReadFile(v2CatalogPath())
	if err != nil {
		t.Fatalf("v2 catalog not readable: %v", err)
	}
	cat, err := ParseMarketplaceCatalog(raw)
	if err != nil {
		t.Fatalf("v2 catalog failed to parse: %v", err)
	}
	docsByID := map[string]*DocsOnlyEntry{}
	for i := range cat.DocsOnly {
		docsByID[cat.DocsOnly[i].ID] = &cat.DocsOnly[i]
	}
	entryIDs := map[string]bool{}
	for i := range cat.Entries {
		entryIDs[cat.Entries[i].ID] = true
	}
	wantLicense := map[string]string{
		"comsol": "MIT", "solidworks": "MIT", "autocad": "MIT",
		"guitarpro": "ISC", "flstudio": "MIT", "bitwig": "MIT",
		"midi": "MIT", "logicpro": "MIT", "cubase": "MIT",
		// wave-2b additions (license-confirmed via `gh api repos/<r>/license`).
		"mathematica": "MIT", "word": "MIT", "powerpoint": "MIT",
		"blender": "MIT", "freecad": "MIT", "fusion360": "MIT",
		"rhino": "MIT", "davinci": "MIT", "audacity": "Apache-2.0",
		"anki": "MIT",
	}
	if len(cat.DocsOnly) != len(docsOnlyCatalogIDs) {
		t.Fatalf("docs_only count = %d, want %d", len(cat.DocsOnly), len(docsOnlyCatalogIDs))
	}
	for _, id := range docsOnlyCatalogIDs {
		id := id
		t.Run(id, func(t *testing.T) {
			d, ok := docsByID[id]
			if !ok {
				t.Fatalf("v2 catalog missing docs_only row %q", id)
			}
			if entryIDs[id] {
				t.Fatalf("docs_only id %q ALSO appears in entries[] — a pointer must not be installable", id)
			}
			if strings.TrimSpace(d.Homepage) == "" || strings.TrimSpace(d.Summary) == "" {
				t.Fatalf("docs_only row %q missing homepage/summary", id)
			}
			if strings.TrimSpace(d.ManualInstall) == "" {
				t.Fatalf("docs_only row %q missing manual_install setup steps", id)
			}
			if want := wantLicense[id]; d.License != want {
				t.Fatalf("docs_only row %q license = %q, want %q", id, d.License, want)
			}
			if !strings.HasPrefix(d.ReadmeURL, "https://") {
				t.Errorf("docs_only row %q readme_url %q is not an https link", id, d.ReadmeURL)
			}
			// generate → pointer (never a manifest).
			got, err := DocsOnlyPointerText(d)
			if err != nil {
				t.Fatalf("pointer text for %q: %v", id, err)
			}
			if !strings.Contains(got, "DOCS-ONLY pointer") {
				t.Fatalf("row %q pointer text malformed\n---\n%s", id, got)
			}
		})
	}
}

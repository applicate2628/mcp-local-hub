package api

import (
	"os"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// docsOnlyCatalogIDs are the 9 S4 manual-install pointer rows the v2 catalog
// appends after the installable rows. Each is transport:"docs-only" — a server
// the hub never installs (immature, git-clone-only, macOS-only, or a LAN-bind
// risk), so it is discoverable but install-inert by construction.
var docsOnlyCatalogIDs = []string{
	"comsol", "solidworks", "autocad", "guitarpro", "flstudio",
	"bitwig", "midi", "logicpro", "cubase",
}

// TestGenerateDraftManifest_DocsOnlyEmitsPointerNotManifest pins the S4 generate
// arm: a transport:"docs-only" entry projects onto a human-readable POINTER TEXT
// block — homepage + readme + the verbatim manual_install steps — and NEVER a YAML
// manifest. It must carry NO manifest keys (name:/kind:/transport:/command:/
// daemons:/client_bindings:) so it can't be piped into `mcphub manifest create`.
func TestGenerateDraftManifest_DocsOnlyEmitsPointerNotManifest(t *testing.T) {
	e := &MarketplaceEntry{
		ID:            "cubase",
		Name:          "Cubase MCP server (docs-only)",
		Summary:       "Drive Cubase from an AI client.",
		Homepage:      "https://github.com/hedidjs/cubase-mcp",
		ReadmeURL:     "https://raw.githubusercontent.com/hedidjs/cubase-mcp/main/README.md",
		Transport:     "docs-only",
		ManualInstall: "git clone the repo and configure a virtual-MIDI port in Cubase.",
	}
	got, warns, err := GenerateDraftManifest(e, GenerateOpts{})
	if err != nil {
		t.Fatalf("GenerateDraftManifest(docs-only): %v", err)
	}
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	// The pointer carries the human-facing payload.
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
	// It is NOT a manifest: none of the YAML manifest keys may appear. A docs-only
	// row producing a manifest would let a caller install something the row exists
	// precisely to keep manual-only.
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
			t.Errorf("docs-only pointer must NOT contain manifest key %q (it must never be a draftable manifest)\n---\n%s", forbidden, got)
		}
	}
}

// TestGenerateDocsOnlyPointer_RejectsHostileRunes proves the docs-only arm routes
// every interpolated field through the SAME untrusted-rune owner the stdio/http
// arms use, so a hostile registry cannot inject a terminal escape / control byte
// into the operator's stdout via the new manual_install (or any other) field.
func TestGenerateDocsOnlyPointer_RejectsHostileRunes(t *testing.T) {
	base := func() *MarketplaceEntry {
		return &MarketplaceEntry{
			ID:        "evil",
			Name:      "Evil",
			Summary:   "ok",
			Homepage:  "https://example.com",
			Transport: "docs-only",
		}
	}
	// An ESC (U+001B) in manual_install — a classic terminal-escape injection.
	e := base()
	e.ManualInstall = "step1\x1b[2Jclobber"
	if _, _, err := GenerateDraftManifest(e, GenerateOpts{}); err == nil {
		t.Fatal("docs-only generate accepted a manual_install carrying an ESC control byte; want refusal")
	}
	// A bidi override (U+202E) in summary — Trojan-Source class.
	e2 := base()
	e2.Summary = "good‮dab"
	if _, _, err := GenerateDraftManifest(e2, GenerateOpts{}); err == nil {
		t.Fatal("docs-only generate accepted a summary carrying a bidi-override rune; want refusal")
	}
}

// TestValidateMarketplaceEntry_DocsOnlyShape pins the S4 schema validation: a
// docs-only row MUST carry homepage + summary and MUST NOT carry command/args/url.
// A row that violates either is rejected at catalog parse, so a docs-only row can
// never carry install fields a future code path might honor.
func TestValidateMarketplaceEntry_DocsOnlyShape(t *testing.T) {
	ok := &MarketplaceEntry{
		ID:        "ok-row",
		Name:      "OK",
		Summary:   "what it does and why docs-only",
		Homepage:  "https://example.com",
		Transport: "docs-only",
	}
	if err := validateMarketplaceEntry(ok, MarketplaceCatalogSchemaVersionV2, nil); err != nil {
		t.Fatalf("valid docs-only row rejected: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(e *MarketplaceEntry)
		wantSub string
	}{
		{
			name:    "missing homepage",
			mutate:  func(e *MarketplaceEntry) { e.Homepage = "" },
			wantSub: "homepage",
		},
		{
			name:    "missing summary",
			mutate:  func(e *MarketplaceEntry) { e.Summary = "" },
			wantSub: "summary",
		},
		{
			name:    "carries command",
			mutate:  func(e *MarketplaceEntry) { e.Command = "uvx" },
			wantSub: "command/args",
		},
		{
			name:    "carries args",
			mutate:  func(e *MarketplaceEntry) { e.Args = []string{"x"} },
			wantSub: "command/args",
		},
		{
			name:    "carries url",
			mutate:  func(e *MarketplaceEntry) { e.URL = "https://example.com/mcp" },
			wantSub: "url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &MarketplaceEntry{
				ID:        "row",
				Name:      "Row",
				Summary:   "s",
				Homepage:  "https://example.com",
				Transport: "docs-only",
			}
			tc.mutate(e)
			err := validateMarketplaceEntry(e, MarketplaceCatalogSchemaVersionV2, nil)
			if err == nil {
				t.Fatalf("validateMarketplaceEntry accepted invalid docs-only row (%s); want rejection", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q, want it to mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestDocsOnlyRequiresSchemaV2 proves the manual_install key is gated to
// schema_version 2 (newCatalogFieldKeys): a v1 catalog carrying manual_install is
// rejected on the fetch path (key-presence gate), so the additive S4 field can
// never reach an older v1-only client.
func TestDocsOnlyRequiresSchemaV2(t *testing.T) {
	raw := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"docs-only","homepage":"https://example.com","summary":"s","manual_install":"do the thing"}]}`
	_, err := ParseMarketplaceCatalog([]byte(raw))
	if err == nil {
		t.Fatal("v1 catalog carrying manual_install was accepted; want rejection (the key is v2-only)")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("error = %q, want it to mention schema_version", err.Error())
	}
}

// TestDocsOnlyTransportRequiresSchemaV2 proves the docs-only TRANSPORT itself is
// v2-only — distinct from the manual_install KEY gate above (bot #446 P2). A v1
// catalog with a docs-only row that carries NO manual_install key (so the
// key-presence gate does NOT fire) must STILL be rejected, because an older v1-only
// client knows only stdio/native-http/http and rejects the whole catalog on the
// unknown transport — accepting it here was a contract split. A v2 catalog with the
// same row is accepted.
func TestDocsOnlyTransportRequiresSchemaV2(t *testing.T) {
	// v1 docs-only row WITHOUT manual_install (the key-presence gate is silent here):
	// the transport gate must still reject it.
	v1raw := `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"docs-only","homepage":"https://example.com","summary":"s"}]}`
	_, err := ParseMarketplaceCatalog([]byte(v1raw))
	if err == nil {
		t.Fatal("v1 catalog with a docs-only transport row (no manual_install) was accepted; want rejection (the transport is v2-only)")
	}
	if !strings.Contains(err.Error(), "schema_version") || !strings.Contains(err.Error(), "docs-only") {
		t.Fatalf("error = %q, want it to mention both the docs-only transport and schema_version", err.Error())
	}

	// Same row under schema_version 2 is accepted.
	v2raw := `{"schema_version":"2","entries":[{"id":"x","name":"X","transport":"docs-only","homepage":"https://example.com","summary":"s"}]}`
	cat, err := ParseMarketplaceCatalog([]byte(v2raw))
	if err != nil {
		t.Fatalf("v2 catalog with a docs-only row was rejected; want acceptance: %v", err)
	}
	if len(cat.Entries) != 1 || cat.Entries[0].Transport != "docs-only" {
		t.Fatalf("v2 docs-only row not parsed: %#v", cat.Entries)
	}
}

// TestV2DocsOnlyRows_PresentInertAndPointerOnly is the S4 catalog-data acceptance:
// the v2 catalog carries the 9 docs-only pointer rows; each is transport:"docs-only"
// with homepage + summary + manual_install and NO command/args/url, and each
// projects through GenerateDraftManifest to a pointer (never a manifest). It also
// pins each row's confirmed license.
func TestV2DocsOnlyRows_PresentInertAndPointerOnly(t *testing.T) {
	byID := v2CatalogByID(t)
	wantLicense := map[string]string{
		"comsol": "MIT", "solidworks": "MIT", "autocad": "MIT",
		"guitarpro": "ISC", "flstudio": "MIT", "bitwig": "MIT",
		"midi": "MIT", "logicpro": "MIT", "cubase": "MIT",
	}
	for _, id := range docsOnlyCatalogIDs {
		id := id
		t.Run(id, func(t *testing.T) {
			e, ok := byID[id]
			if !ok {
				t.Fatalf("v2 catalog missing docs-only row %q", id)
			}
			if e.Transport != "docs-only" {
				t.Fatalf("row %q transport = %q, want docs-only", id, e.Transport)
			}
			if strings.TrimSpace(e.Homepage) == "" || strings.TrimSpace(e.Summary) == "" {
				t.Fatalf("docs-only row %q missing homepage/summary", id)
			}
			if strings.TrimSpace(e.ManualInstall) == "" {
				t.Fatalf("docs-only row %q missing manual_install setup steps", id)
			}
			// Install-inert by construction.
			if e.Command != "" || len(e.Args) > 0 || e.URL != "" {
				t.Fatalf("docs-only row %q carries install fields (cmd=%q args=%v url=%q) — must be inert", id, e.Command, e.Args, e.URL)
			}
			// No install gate fields — a docs-only row is neither availability-gated nor
			// vendored (it is not installed at all).
			if e.Availability != "" || e.InstallProbe != nil || e.VendoredSource != nil || len(e.RequiredSecrets) > 0 {
				t.Fatalf("docs-only row %q carries an install gate (av=%q probe=%#v vs=%#v secrets=%v) — pointless on a non-installable row", id, e.Availability, e.InstallProbe, e.VendoredSource, e.RequiredSecrets)
			}
			if want := wantLicense[id]; e.License != want {
				t.Fatalf("docs-only row %q license = %q, want %q", id, e.License, want)
			}
			// generate → pointer (never a manifest).
			got, _, err := GenerateDraftManifest(e, GenerateOpts{})
			if err != nil {
				t.Fatalf("generate pointer for %q: %v", id, err)
			}
			if !strings.Contains(got, "DOCS-ONLY pointer") {
				t.Fatalf("row %q generate did not produce a docs-only pointer\n---\n%s", id, got)
			}
			if strings.Contains(got, "kind: global") || strings.Contains(got, "daemons:") {
				t.Fatalf("row %q generate produced manifest keys (must be pointer-only)\n---\n%s", id, got)
			}
		})
	}
	// Sanity: the docs-only rows do NOT collide with any installable id, and the
	// readme_url is a raw https link for each.
	for _, id := range docsOnlyCatalogIDs {
		e := byID[id]
		if e == nil {
			continue
		}
		if !strings.HasPrefix(e.ReadmeURL, "https://") {
			t.Errorf("docs-only row %q readme_url %q is not an https link", id, e.ReadmeURL)
		}
	}
}

// TestV2DocsOnlyRows_BrowseProbeStateReady confirms a docs-only row classifies as
// browse-ready (not greyed) — the frontend keys on transport==="docs-only" to
// suppress install, so the probe state must not also grey it. A docs-only row has
// no availability gate, so MarketplaceEntryBrowseProbeState returns ready.
func TestV2DocsOnlyRows_BrowseProbeStateReady(t *testing.T) {
	byID := v2CatalogByID(t)
	for _, id := range docsOnlyCatalogIDs {
		e := byID[id]
		if e == nil {
			t.Fatalf("v2 catalog missing docs-only row %q", id)
		}
		if got := MarketplaceEntryBrowseProbeState(e); got != ProbeBrowseReady {
			t.Fatalf("docs-only row %q browse probe state = %q, want %q (no availability gate)", id, got, ProbeBrowseReady)
		}
	}
}

// TestV2Catalog_DocsOnlyAppendedAfterInstallableRows guards that the docs-only
// rows are APPENDED after the installable rows (frozen v1 prefix + v2 installable
// rows stay first), so a docs-only insertion never shifts an existing row. It reads
// the file directly and asserts the first docs-only row appears only after every
// non-docs-only row.
func TestV2Catalog_DocsOnlyAppendedAfterInstallableRows(t *testing.T) {
	raw, err := os.ReadFile(v2CatalogPath())
	if err != nil {
		t.Fatalf("v2 catalog not readable: %v", err)
	}
	cat, err := ParseMarketplaceCatalog(raw)
	if err != nil {
		t.Fatalf("v2 catalog failed to parse: %v", err)
	}
	firstDocsOnly := -1
	lastInstallable := -1
	for i := range cat.Entries {
		if cat.Entries[i].Transport == "docs-only" {
			if firstDocsOnly == -1 {
				firstDocsOnly = i
			}
		} else {
			lastInstallable = i
		}
	}
	if firstDocsOnly == -1 {
		t.Fatal("v2 catalog has no docs-only rows")
	}
	if firstDocsOnly < lastInstallable {
		t.Fatalf("a docs-only row at index %d precedes an installable row at index %d — docs-only rows must be appended after all installable rows", firstDocsOnly, lastInstallable)
	}
	// And the config availability constant is still imported (keeps the import
	// honest if the rest of the test ever drops its last use).
	_ = config.AvailabilityDisabledUntilProbe
}

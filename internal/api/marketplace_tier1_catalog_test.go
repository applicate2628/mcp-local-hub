package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// tier1CatalogIDs are the Tier-1 desktop-app rows the v2 catalog appends after the
// verbatim v1 entries. The first batch was excel/ableton/codex-mcp-server; matlab
// (official MathWorks Go binary, v0.11.0) and ansys (official ansys/pymapdl-mcp,
// v0.2.1, FastMCP/stdio) extend it. Two rows were DROPPED before merge:
//   - cst — bbl21/CST_MCP is a CLI toolkit, not an MCP server
//     (work-items/bugs/2026-06-24-cst-not-an-mcp-server.md).
//   - mathcad — ${workspaceFolder} freezes to CWD for a kind:global daemon
//     (category error), the server artifact is unprobed+absent (repo unpackaged →
//     crash-loop), and the license is pending
//     (work-items/backlog/2026-06-24-mathcad-mcp-row-deferred.md).
var tier1CatalogIDs = []string{"excel", "ableton", "codex-mcp-server", "matlab", "ansys"}

// v1CatalogPath / v2CatalogPath: internal/api/ test cwd → repo root is two levels up.
func v1CatalogPath() string { return filepath.Join("..", "..", "marketplace", "v1", "catalog.json") }
func v2CatalogPath() string { return filepath.Join("..", "..", "marketplace", "v2", "catalog.json") }

// TestParseV2Catalog_ParsesAsSchema2WithTier1Rows is the primary v2 acceptance:
// the new marketplace/v2/catalog.json parses cleanly through the SAME
// ParseMarketplaceCatalog gate (no Go gate change in this PR — Tier-0 already
// taught it to accept "2" and allow the D-2/D-3 keys), declares schema_version
// "2", and carries the three Tier-1 desktop-app rows on TOP of the 14 verbatim v1
// entries. Each Tier-1 row is inert (disabled-until-probe) with a non-empty
// install_probe, and each forked row carries a pinned vendored_source.
func TestParseV2Catalog_ParsesAsSchema2WithTier1Rows(t *testing.T) {
	raw, err := os.ReadFile(v2CatalogPath())
	if err != nil {
		t.Fatalf("v2 catalog not readable at %s: %v", v2CatalogPath(), err)
	}
	cat, err := ParseMarketplaceCatalog(raw)
	if err != nil {
		t.Fatalf("v2 catalog failed to parse: %v", err)
	}
	if cat.SchemaVersion != MarketplaceCatalogSchemaVersionV2 {
		t.Fatalf("v2 catalog schema_version = %q, want %q", cat.SchemaVersion, MarketplaceCatalogSchemaVersionV2)
	}

	// Entry count: the v1 entries are copied VERBATIM, then the Tier-1 rows are
	// appended. Derive the expected count from the v1 file so this stays correct
	// if v1 ever grows before being frozen (it is frozen now, but the derivation
	// is the robust assertion).
	v1raw, err := os.ReadFile(v1CatalogPath())
	if err != nil {
		t.Fatalf("v1 catalog not readable at %s: %v", v1CatalogPath(), err)
	}
	v1cat, err := ParseMarketplaceCatalog(v1raw)
	if err != nil {
		t.Fatalf("v1 catalog failed to parse: %v", err)
	}
	wantCount := len(v1cat.Entries) + len(tier1CatalogIDs)
	if len(cat.Entries) != wantCount {
		t.Fatalf("v2 catalog entry count = %d, want %d (v1 %d + %d Tier-1)",
			len(cat.Entries), wantCount, len(v1cat.Entries), len(tier1CatalogIDs))
	}

	// Every v1 entry id must still be present (verbatim copy), in order, before
	// the Tier-1 rows.
	byID := map[string]*MarketplaceEntry{}
	for i := range cat.Entries {
		byID[cat.Entries[i].ID] = &cat.Entries[i]
	}
	for _, v1e := range v1cat.Entries {
		if _, ok := byID[v1e.ID]; !ok {
			t.Fatalf("v2 catalog dropped v1 entry %q", v1e.ID)
		}
	}

	// Each Tier-1 row: present, inert, with a non-empty install_probe.
	for _, id := range tier1CatalogIDs {
		e, ok := byID[id]
		if !ok {
			t.Fatalf("v2 catalog missing Tier-1 row %q", id)
		}
		if e.Availability != config.AvailabilityDisabledUntilProbe {
			t.Fatalf("Tier-1 row %q availability = %q, want %q", id, e.Availability, config.AvailabilityDisabledUntilProbe)
		}
		if e.InstallProbe == nil || (len(e.InstallProbe.Binaries) == 0 && len(e.InstallProbe.Files) == 0) {
			t.Fatalf("Tier-1 row %q has no install_probe (binaries or files)", id)
		}
	}

	// The two FORK rows carry a pinned vendored_source; the OFFICIAL rows (codex,
	// matlab, ansys) carry NONE — they are first-party servers, not community forks,
	// so their pin lives in the upstream-pinned readme_url / args (matlab release
	// v0.11.0 binary; ansys uvx ==0.2.1 PyPI pin), not a vendored_source block.
	for _, id := range []string{"excel", "ableton"} {
		e := byID[id]
		if e.VendoredSource == nil || strings.TrimSpace(e.VendoredSource.PinnedRef) == "" {
			t.Fatalf("fork row %q is missing a pinned vendored_source", id)
		}
		if config.IsMovingGitRef(e.VendoredSource.PinnedRef) {
			t.Fatalf("fork row %q pinned_ref %q is a moving ref (must be a SHA or tag)", id, e.VendoredSource.PinnedRef)
		}
	}
	for _, id := range []string{"codex-mcp-server", "matlab", "ansys"} {
		if e := byID[id]; e.VendoredSource != nil {
			t.Fatalf("official row %q must NOT carry vendored_source (it is not a fork): %#v", id, e.VendoredSource)
		}
	}
}

// TestV1CatalogFrozen_ParsesAsSchema1WithNoNewKeys is the FREEZE guard: the
// repointed default URL now serves v2, but marketplace/v1/catalog.json MUST stay
// frozen so an OLDER released client hard-coded to the v1 URL still resolves. It
// declares schema_version "1" and carries ZERO D-2/D-3 keys (a v1 catalog can
// never carry them — the forward-compat gate would reject it, and an older
// v1-only DisallowUnknownFields decoder would choke on the bare key).
func TestV1CatalogFrozen_ParsesAsSchema1WithNoNewKeys(t *testing.T) {
	raw, err := os.ReadFile(v1CatalogPath())
	if err != nil {
		t.Fatalf("v1 catalog not readable at %s: %v", v1CatalogPath(), err)
	}
	cat, err := ParseMarketplaceCatalog(raw)
	if err != nil {
		t.Fatalf("frozen v1 catalog failed to parse: %v", err)
	}
	if cat.SchemaVersion != MarketplaceCatalogSchemaVersion {
		t.Fatalf("v1 catalog schema_version = %q, want %q (freeze guard)", cat.SchemaVersion, MarketplaceCatalogSchemaVersion)
	}
	for _, e := range cat.Entries {
		if e.VendoredSource != nil || e.Availability != "" || e.InstallProbe != nil {
			t.Fatalf("FREEZE VIOLATION: v1 entry %q carries a D-2/D-3 key (must live in v2 only): vs=%#v av=%q probe=%#v",
				e.ID, e.VendoredSource, e.Availability, e.InstallProbe)
		}
	}
	// Belt-and-suspenders: assert the raw bytes hold none of the new keys, so a
	// future edit that re-introduces a present-empty/null key (which the typed
	// decode would silently zero) still trips this guard.
	for _, key := range []string{`"vendored_source"`, `"availability"`, `"install_probe"`} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("FREEZE VIOLATION: v1 catalog raw bytes contain %s — the D-2/D-3 keys belong in v2 only", key)
		}
	}
}

// TestV2Tier1Rows_GenerateThenCreateDryRun is the architect-required MIRROR GATE:
// for EACH Tier-1 row, project it through GenerateDraftManifest (the exact
// `mcphub marketplace generate <id>` path), substitute a real port for the
// placeholder 0 (the operator's required edit), then ParseManifest (which runs
// Validate()). A clean draft + a clean Validate() proves the row is gate-clean —
// it would be accepted by `mcphub manifest create`. This is fully in-process: no
// daemon spawn, no state-dir write, so it is state-safe with no env redirection.
func TestV2Tier1Rows_GenerateThenCreateDryRun(t *testing.T) {
	raw, err := os.ReadFile(v2CatalogPath())
	if err != nil {
		t.Fatalf("v2 catalog not readable at %s: %v", v2CatalogPath(), err)
	}
	cat, err := ParseMarketplaceCatalog(raw)
	if err != nil {
		t.Fatalf("v2 catalog failed to parse: %v", err)
	}
	byID := map[string]*MarketplaceEntry{}
	for i := range cat.Entries {
		byID[cat.Entries[i].ID] = &cat.Entries[i]
	}

	// A fixed absolute workspace for any ${workspaceFolder} placeholder a row's
	// args carry (Validate accepts the expanded shape; a missing file is a
	// readiness row, not a Validate failure). t.TempDir is absolute on every OS.
	ws := t.TempDir()

	for _, id := range tier1CatalogIDs {
		id := id
		t.Run(id, func(t *testing.T) {
			e, ok := byID[id]
			if !ok {
				t.Fatalf("v2 catalog missing Tier-1 row %q", id)
			}
			draft, warns, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: ws})
			if err != nil {
				t.Fatalf("generate draft for %q: %v (warnings=%v)", id, err, warns)
			}
			// The operator's required edit: pick a real port for the placeholder 0.
			parseReady := strings.Replace(draft, "port: 0", "port: 9311", 1)
			m, err := config.ParseManifest(strings.NewReader(parseReady))
			if err != nil {
				t.Fatalf("row %q drafted manifest failed ParseManifest+Validate (mirror gate): %v\n---\n%s", id, err, parseReady)
			}
			// The D-3 availability gate survives projection into the manifest for
			// every Tier-1 row (so the post-install readiness gate is not blind).
			if m.Availability != config.AvailabilityDisabledUntilProbe {
				t.Fatalf("row %q drafted manifest lost availability gate: av=%q", id, m.Availability)
			}
			if m.InstallProbe == nil || (len(m.InstallProbe.Binaries) == 0 && len(m.InstallProbe.Files) == 0) {
				t.Fatalf("row %q drafted manifest lost install_probe", id)
			}
			// The two fork rows project the pinned vendored_source; the official rows
			// (codex, matlab, ansys) do not.
			switch id {
			case "excel", "ableton":
				if m.VendoredSource == nil || strings.TrimSpace(m.VendoredSource.PinnedRef) == "" {
					t.Fatalf("fork row %q drafted manifest lost the pinned vendored_source: %#v", id, m.VendoredSource)
				}
			case "codex-mcp-server", "matlab", "ansys":
				if m.VendoredSource != nil {
					t.Fatalf("official row %q drafted manifest gained a vendored_source: %#v", id, m.VendoredSource)
				}
			}
		})
	}
}

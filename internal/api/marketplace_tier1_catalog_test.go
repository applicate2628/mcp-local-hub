package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// gitSHA40 matches a full 40-hex git object name (the immutable-pin shape the
// ableton row uses after the provenance fix). A bare PyPI version like "2.2.0"
// does NOT match, so this also guards against a silent revert to the prior
// hijacked-PyPI-name pin.
var gitSHA40 = regexp.MustCompile(`^[0-9a-f]{40}$`)

// tier1CatalogIDs are the Tier-1 desktop-app rows the v2 catalog appends after the
// verbatim v1 entries. The first batch was excel/ableton/codex-mcp-server; matlab
// (official MathWorks Go binary, v0.11.0) and ansys (official ansys/pymapdl-mcp,
// v0.2.1, FastMCP/stdio) extend it, and kicad (community fork oaslananka/kicad-mcp,
// PyPI kicad-mcp-pro v3.14.1, uvx --from, MIT) is the one clean one-click EDA row.
// onshape (community server altendky/onshape-mcp, npm onshape-mcp@0.4.0 via npx,
// Apache-2.0, SLSA-provenance-attested) is the §9 cloud-CAD breadth row — like kicad
// it is a community fork with a pinned vendored_source, but it gates on the npx
// launcher (cloud server, no host app to detect) rather than a host-binary glob.
// reaper (community fork bonfire-systems/reaper-mcp, PyPI reaper-mcp-server v0.1.1,
// uvx --from, MIT) is the music-breadth DAW row — like kicad it is a uvx-launched
// community fork with a pinned vendored_source and a host-app file glob
// (REAPER's reaper.exe). Its MCP server is stdio, but the python-reapy distant API
// it requires binds 0.0.0.0 inside REAPER (LAN-reachable once enabled — SAME risk
// class as the Ableton row; the row's summary carries the firewall/trusted-network
// caution + a destructive-write warning).
// Two rows were DROPPED before merge:
//   - cst — bbl21/CST_MCP is a CLI toolkit, not an MCP server
//     (work-items/bugs/2026-06-24-cst-not-an-mcp-server.md).
//   - mathcad — ${workspaceFolder} freezes to CWD for a kind:global daemon
//     (category error), the server artifact is unprobed+absent (repo unpackaged →
//     crash-loop), and the license is pending
//     (work-items/backlog/2026-06-24-mathcad-mcp-row-deferred.md).
//
// Five further EDA/CAD rows are DEFERRED (real MCP servers, but each needs a manual
// git-clone+build that vendored_source.install_cmd does not execute — see
// work-items/backlog/2026-06-24-tier3-manual-clone-mcps.md).
// The vendor-breadth wave-2a rows extend the disabled-until-probe set with five more
// uvx/npx-launched servers that gate on the launcher (and, for photoshop, a host-app
// glob + an arch matrix). Each is verified to launch on a clean cache and carries a
// pinned vendored_source:
//   - grafana   — official grafana/mcp-grafana (Apache-2.0), PyPI mcp-grafana==0.17.0
//     (a Python wheel bundling the per-platform Go server binary); stdio is the Go
//     binary's default and the row passes -t stdio to pin it (the SSE/streamable-http
//     modes are opt-in and bind localhost:8000, not 0.0.0.0). The Grafana
//     service-account token is the OPTIONAL-secret posture (server starts without it).
//   - photoshop — community loonghao/photoshop-python-api-mcp-server (MIT), PyPI
//     photoshop-mcp-server==0.1.11; Windows-only COM (win32com) + stdio (no socket),
//     so the probe ARCH-GATES on platforms:[windows/amd64] plus a host Photoshop glob.
//   - zotero    — community kujenga/zotero-mcp (MIT), PyPI zotero-mcp==0.1.6; stdio,
//     ZOTERO_LOCAL=true (literal) makes it a CLIENT to Zotero desktop's own loopback
//     local API (http://localhost:23119, verified in pyzotero). No secret.
//   - jupyter   — community datalayer/jupyter-mcp-server (BSD-3-Clause), PyPI
//     jupyter-mcp-server==1.0.2; the CLI default is stdio and the row passes
//     --transport stdio to PIN it AWAY from the streamable-http mode (host=0.0.0.0,
//     opt-in, NOT used). The Jupyter token is the OPTIONAL-secret posture.
//   - rmcp      — community finite-sample/rmcp (MIT, the PyPI provenance repo), PyPI
//     rmcp==0.8.1; the row runs the `start` subcommand (stdio) to PIN it AWAY from
//     `serve-http --host 0.0.0.0` (opt-in, NOT used). No secret; drives R via Rscript.
var tier1CatalogIDs = []string{"excel", "ableton", "codex-mcp-server", "matlab", "ansys", "kicad", "onshape", "reaper", "grafana", "photoshop", "zotero", "jupyter", "rmcp"}

// tierMusicLocalCatalogIDs are the local-stdio music rows that gate the install on
// a REQUIRED vault secret (required_secrets) rather than a host-app install_probe.
// suno (mcp-suno via uvx) hard-exits on startup without its AceDataCloud token, so
// the row carries required_secrets: [acedata_api_token] and is BLOCKED at install
// until the token is set — the opt-in install gate shipped alongside this row. These
// rows are READY (not disabled-until-probe): the gate is the secret, not a probe.
var tierMusicLocalCatalogIDs = []string{"suno"}

// tierSecretGatedVendoredCatalogIDs are the wave-2a data/BI rows that, like suno, gate
// the install on a REQUIRED vault secret (they hard-exit on startup without auth —
// verified by a clean-cache launch), but UNLIKE suno are pinned community/official
// servers carrying a vendored_source (npm packages run via npx). They are READY (the
// secret is the gate, not a host-app probe) and carry NO install_probe:
//   - tableau  — official tableau/tableau-mcp (Apache-2.0), npm @tableau/mcp-server
//     @2.18.1; hard-exits without SERVER/PAT_NAME/PAT_VALUE, so required_secrets is
//     [tableau_pat_name, tableau_pat_value] (the PAT name+value stored in the vault).
//   - metabase — community easecloudio/mcp-metabase-server (MIT), npm
//     @easecloudio/mcp-metabase-server@1.3.0; StdioServerTransport only (no port,
//     loopback-clean); hard-exits without METABASE_URL, so required_secrets is
//     [metabase_api_key].
var tierSecretGatedVendoredCatalogIDs = []string{"tableau", "metabase"}

// tierVendorWave2bCatalogIDs are the vendor-breadth wave-2b clean rows. Each is a
// uvx-launched community fork carrying a pinned vendored_source, verified to launch
// (build + entry-point resolve) on a CLEAN uv cache, with its in-app bridge bound to
// loopback (or no socket at all). They split across two install-gate postures:
//   - obsidian — community MarkusPfundstein/mcp-obsidian (MIT), PyPI mcp-obsidian
//     ==0.2.2; stdio + a CLIENT to the 'Local REST API' Obsidian plugin at
//     https://127.0.0.1:27124 (loopback, verified). READY (NOT disabled-until-probe):
//     the gate is the REQUIRED secret — mcp-obsidian HARD-EXITS without OBSIDIAN_API_KEY
//     (verified on a clean-cache launch), so required_secrets:[obsidian_api_key].
//   - logseq   — community ergut/mcp-logseq (MIT), PyPI mcp-logseq==1.8.0; stdio
//     (the row PINS --transport stdio; the http mode DEFAULTS to --host 127.0.0.1 and
//     needs an explicit --insecure to leave loopback, verified). READY: the gate is
//     the REQUIRED secret — mcp-logseq HARD-EXITS without LOGSEQ_API_TOKEN, so
//     required_secrets:[logseq_token]. LOGSEQ_API_URL has a localhost default, so it
//     stays an editable literal (not a required secret).
//   - origin-pro — community youngminsw/Origin-Pro-MCP (MIT), PyPI origin-pro-mcp
//     ==0.1.0; stdio + Windows COM (win32com Dispatch 'Origin.ApplicationSI'), NO
//     socket (loopback-clean by construction, like photoshop). disabled-until-probe:
//     ARCH-GATES on platforms:[windows/amd64] (COM is Windows-only) + a host OriginLab
//     Origin install glob + uvx. NO secret. LOW maturity (Alpha, ~21 stars) — caveat
//     in the summary, but a clean-cache launch is verified.
var tierVendorWave2bCatalogIDs = []string{"obsidian", "logseq", "origin-pro"}

// tierResearchCatalogIDs are the research/academic-search rows appended AFTER the
// wave-2b batch. Both are READY one-click rows (no install_probe, no vendored_source,
// no required_secrets), so they extend ONLY the entries[] count sum below — they are
// NOT part of the disabled-until-probe tier1/wave-2a install_probe ordering loops:
//   - scholar-search — community silung/scholar-search-mcp (Python, MIT), PyPI
//     scholar-search-mcp, run via `uv run --with scholar-search-mcp`. A local-stdio
//     CLIENT to the Semantic Scholar API, which works keyless at the shared public
//     rate limit, so it is a clean ready row like the git/fetch v1 entries. No secret.
//   - consensus — REMOTE HTTP row (transport:http, like context7) pointing at the
//     provider-hosted endpoint https://mcp.consensus.app/mcp. READY, no auth for the
//     free tier. As a transport:http row it carries NO command/args and NO
//     required_secrets (a remote endpoint's credentials live in its OAuth/headers).
var tierResearchCatalogIDs = []string{"scholar-search", "consensus"}

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
	// The docs-only POINTER rows live in the SEPARATE top-level docs_only[] array
	// (S4, bot #446 P1), NOT entries[], so the entries[] count is v1 + Tier-1 +
	// music-local only. docs_only count is asserted separately below.
	wantCount := len(v1cat.Entries) + len(tier1CatalogIDs) + len(tierMusicLocalCatalogIDs) + len(tierSecretGatedVendoredCatalogIDs) + len(tierVendorWave2bCatalogIDs) + len(tierResearchCatalogIDs)
	if len(cat.Entries) != wantCount {
		t.Fatalf("v2 catalog entry count = %d, want %d (v1 %d + %d Tier-1 + %d music-local + %d secret-gated-vendored + %d wave-2b + %d research; docs-only rows are in docs_only[], not entries[])",
			len(cat.Entries), wantCount, len(v1cat.Entries), len(tier1CatalogIDs), len(tierMusicLocalCatalogIDs), len(tierSecretGatedVendoredCatalogIDs), len(tierVendorWave2bCatalogIDs), len(tierResearchCatalogIDs))
	}
	if len(cat.DocsOnly) != len(docsOnlyCatalogIDs) {
		t.Fatalf("v2 catalog docs_only count = %d, want %d", len(cat.DocsOnly), len(docsOnlyCatalogIDs))
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

	// FROZEN-PREFIX INVARIANT (codex-bot PR #429 P2): the v2 catalog must keep the
	// 14 v1 entries copied VERBATIM IN ORDER at indices 0..len(v1)-1, BEFORE any
	// v2-only addition. Inserting a v2 row inside the v1 prefix (suno was wrongly
	// inserted at index 6, between context7 and qt-docs) shifts the frozen entries
	// down and breaks the byte-for-byte v1→v2 prefix copy. Assert positionally, not
	// just by set membership.
	for i, v1e := range v1cat.Entries {
		if cat.Entries[i].ID != v1e.ID {
			t.Fatalf("FROZEN-PREFIX VIOLATION: v2 entry[%d] = %q, want the v1 entry %q (the 14 v1 rows must stay verbatim in order before all v2-only additions; a v2 row was inserted inside the frozen prefix)",
				i, cat.Entries[i].ID, v1e.ID)
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
		if e.InstallProbe == nil || (len(e.InstallProbe.Binaries) == 0 && len(e.InstallProbe.Files) == 0 && len(e.InstallProbe.FileGlobs) == 0) {
			t.Fatalf("Tier-1 row %q has no install_probe (binaries, files, or file_globs)", id)
		}
	}

	// The FORK rows carry a pinned vendored_source; the OFFICIAL rows (codex,
	// matlab, ansys) carry NONE — they are first-party servers, not community forks,
	// so their pin lives in the upstream-pinned readme_url / args (matlab release
	// v0.11.0 binary; ansys uvx ==0.2.1 PyPI pin), not a vendored_source block.
	// kicad is a community fork (oaslananka/kicad-mcp) published to PyPI, so it pins
	// via vendored_source like excel/ableton. onshape is a community server
	// (altendky/onshape-mcp) published to npm, pinned via vendored_source to the
	// v0.4.0 tag SHA. reaper is a community fork (bonfire-systems/reaper-mcp)
	// published to PyPI (reaper-mcp-server), pinned via vendored_source to the
	// v0.1.1 tag SHA.
	for _, id := range []string{"excel", "ableton", "kicad", "onshape", "reaper", "grafana", "photoshop", "zotero", "jupyter", "rmcp", "tableau", "metabase", "obsidian", "logseq", "origin-pro"} {
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

// --- codex catalog finding 4 / architect path A: fetch-tolerance + author-strict ---

// TestParseCatalog_FetchToleratesUnknownAdditiveField proves the FETCH decode is
// forward-compatible: a v2 catalog carrying a FUTURE additive field the current
// build does not know about parses cleanly (the unknown key is ignored by the
// typed decode) instead of rejecting the whole catalog. This is what keeps an
// already-deployed older client non-broken when a new v2-additive field ships.
func TestParseCatalog_FetchToleratesUnknownAdditiveField(t *testing.T) {
	raw := `{
  "schema_version": "2",
  "entries": [
    {
      "id": "future-thing",
      "name": "Future Thing",
      "transport": "stdio",
      "command": "uvx",
      "args": ["x"],
      "future_additive_field_v3": {"some": "value"}
    }
  ]
}`
	cat, err := ParseMarketplaceCatalog([]byte(raw))
	if err != nil {
		t.Fatalf("fetch decode rejected a v2 catalog with an unknown additive field; want forward-compat tolerance: %v", err)
	}
	if len(cat.Entries) != 1 || cat.Entries[0].ID != "future-thing" {
		t.Fatalf("entry not parsed despite the unknown field: %#v", cat.Entries)
	}
}

// TestParseCatalog_FetchStructuralGuardsSurvive proves the structural guards
// dropping DisallowUnknownFields did NOT weaken: trailing bytes, a bad
// schema_version, a duplicate id, and a v1 catalog carrying a v2 key are all still
// rejected on the FETCH path.
func TestParseCatalog_FetchStructuralGuardsSurvive(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantSub string
	}{
		{
			name:    "trailing bytes",
			raw:     `{"schema_version":"2","entries":[]}  {"x":1}`,
			wantSub: "trailing bytes",
		},
		{
			name:    "bad schema_version",
			raw:     `{"schema_version":"99","entries":[]}`,
			wantSub: "schema_version",
		},
		{
			name:    "duplicate id",
			raw:     `{"schema_version":"2","entries":[{"id":"dup","name":"A","transport":"stdio","command":"uvx","args":["x"]},{"id":"dup","name":"B","transport":"stdio","command":"uvx","args":["x"]}]}`,
			wantSub: "duplicate id",
		},
		{
			name:    "v1 catalog carrying a v2 key (key-presence gate)",
			raw:     `{"schema_version":"1","entries":[{"id":"x","name":"X","transport":"stdio","command":"uvx","args":["x"],"availability":""}]}`,
			wantSub: "schema_version",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseMarketplaceCatalog([]byte(tc.raw))
			if err == nil {
				t.Fatalf("ParseMarketplaceCatalog accepted %s; want rejection", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error = %q, want it to mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestParseCatalogStrict_RealCatalogsPass proves the author-side strict decode
// (DisallowUnknownFields) accepts the repo's OWN catalogs — so the strict gate is
// a real, passing assertion the in-repo catalog.json must keep satisfying, not a
// vacuous one.
func TestParseCatalogStrict_RealCatalogsPass(t *testing.T) {
	for _, p := range []string{v1CatalogPath(), v2CatalogPath()} {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("catalog not readable at %s: %v", p, err)
		}
		if _, err := ParseMarketplaceCatalogStrict(raw); err != nil {
			t.Fatalf("the repo's own catalog %s failed the strict author decode (an unknown/typo key crept in): %v", p, err)
		}
	}
}

// TestParseCatalogStrict_RejectsTypoKey proves the author-strict decode catches a
// typo'd field name — the protection the fetch path deliberately gives up. The
// fetch path tolerates the same typo'd-key bytes (it cannot tell a typo from a
// future additive field), so this guard lives ONLY on the author side.
func TestParseCatalogStrict_RejectsTypoKey(t *testing.T) {
	raw := `{
  "schema_version": "2",
  "entries": [
    {
      "id": "typo-row",
      "name": "Typo Row",
      "transport": "stdio",
      "command": "uvx",
      "args": ["x"],
      "instal_probe": {"binaries": ["go"]}
    }
  ]
}`
	if _, err := ParseMarketplaceCatalogStrict([]byte(raw)); err == nil {
		t.Fatal("strict author decode accepted a typo'd key (instal_probe); want rejection")
	}
	// The SAME bytes must be TOLERATED on the fetch path (forward-compat): the
	// typo is indistinguishable from a future additive field at fetch time.
	if _, err := ParseMarketplaceCatalog([]byte(raw)); err != nil {
		t.Fatalf("fetch decode rejected the unknown key; the author-strict guard must be the ONLY rejector: %v", err)
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
			if m.InstallProbe == nil || (len(m.InstallProbe.Binaries) == 0 && len(m.InstallProbe.Files) == 0 && len(m.InstallProbe.FileGlobs) == 0) {
				t.Fatalf("row %q drafted manifest lost install_probe", id)
			}
			// The fork rows project the pinned vendored_source; the official rows
			// (codex, matlab, ansys) do not.
			switch id {
			case "excel", "ableton", "kicad", "onshape", "reaper", "grafana", "photoshop", "zotero", "jupyter", "rmcp":
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

// v2CatalogByID is a small helper for the catalog-data assertions below.
func v2CatalogByID(t *testing.T) map[string]*MarketplaceEntry {
	t.Helper()
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
	return byID
}

// TestV2MatlabRow_ProbeRequiresMatlabOnPATH is TEST #6 — the codex catalog finding 2
// fix. The matlab-mcp-server discovers MATLAB via the `matlab` launcher on PATH by
// default (README v0.11.0: "By default, the server tries to find the first MATLAB on
// the system PATH"; setup step 1 is "add it to the system PATH"), and the row sets no
// args/env (no --matlab-root / MW_MCP_SERVER_MATLAB_ROOT), so PATH is the ONLY
// discovery. The probe MUST therefore require `matlab` on PATH so it AGREES with the
// server's discovery — a MATLAB-installed-but-not-on-PATH host must NOT pass the
// probe and then have the server fail to find MATLAB. The redundant
// Program-Files-path file-glob is DROPPED (matlab-on-PATH is the authoritative
// signal). So the probe is exactly binaries: [matlab-mcp-server, matlab] and carries
// NO files[] / file_globs[].
func TestV2MatlabRow_ProbeRequiresMatlabOnPATH(t *testing.T) {
	e := v2CatalogByID(t)["matlab"]
	if e == nil {
		t.Fatalf("v2 catalog missing the matlab row")
	}
	if e.InstallProbe == nil {
		t.Fatalf("matlab row has no install_probe")
	}
	p := e.InstallProbe
	// Both binaries required: the server binary AND the `matlab` launcher (PATH discovery).
	has := func(s []string, want string) bool {
		for _, v := range s {
			if v == want {
				return true
			}
		}
		return false
	}
	if !has(p.Binaries, "matlab-mcp-server") {
		t.Fatalf("matlab probe binaries %v missing the server binary matlab-mcp-server", p.Binaries)
	}
	if !has(p.Binaries, "matlab") {
		t.Fatalf("matlab probe binaries %v MUST include `matlab` so the probe matches the server's PATH discovery (finding 2)", p.Binaries)
	}
	// The redundant file-glob is dropped — matlab-on-PATH is the authoritative signal.
	if len(p.Files) != 0 || len(p.FileGlobs) != 0 {
		t.Fatalf("matlab probe should carry NO files[]/file_globs[] (matlab-on-PATH is authoritative): files=%v file_globs=%v", p.Files, p.FileGlobs)
	}
	// Cross-check the probe AGREES with the server's discovery: the row declares no
	// args/env, so PATH is the only MATLAB discovery — the probe requires `matlab` on
	// PATH, so it cannot pass where the server would fail to find MATLAB.
	// Args MAY carry non-discovery flags (e.g. --disable-telemetry=true for privacy),
	// but MUST NOT pass --matlab-root, which would move MATLAB discovery off PATH and
	// invalidate the matlab-on-PATH probe requirement above.
	for _, a := range e.Args {
		if has([]string{a}, "--matlab-root") || (len(a) >= 13 && a[:13] == "--matlab-root") {
			t.Fatalf("matlab row passes %q — discovery is no longer PATH-only; the probe's matlab-on-PATH requirement must be revisited", a)
		}
	}
	// Telemetry-off is REQUIRED for the curated row (privacy parity with the ableton row).
	if !has(e.Args, "--disable-telemetry=true") {
		t.Fatalf("matlab row args %v MUST include --disable-telemetry=true (curated-row privacy default; matlab-mcp-server data-collection is on by default)", e.Args)
	}
	if _, ok := e.Env["MW_MCP_SERVER_MATLAB_ROOT"]; ok {
		t.Fatalf("matlab row sets MW_MCP_SERVER_MATLAB_ROOT — discovery is no longer PATH-only; the probe's matlab-on-PATH requirement must be revisited")
	}
}

// TestV2Tier1Rows_GlobsLiveInFileGlobsNotFiles asserts the catalog-data migration:
// every version-agnostic glob pattern in the Tier-1 rows lives in the OPT-IN
// file_globs[] field, NEVER files[] (which is now exact-stat-only). excel + ableton +
// ansys + kicad + reaper carry their patterns in file_globs[]; matlab carries none
// (binaries-only); codex carries binaries-only. No Tier-1 row uses files[] (none
// needs a literal path), and no file_globs[] entry is missing its absolute-path shape.
func TestV2Tier1Rows_GlobsLiveInFileGlobsNotFiles(t *testing.T) {
	byID := v2CatalogByID(t)
	// Rows whose probe carries a glob pattern → must be in file_globs[]. photoshop
	// adds the year-versioned Adobe Photoshop host-exe glob (wave-2a).
	wantGlobRows := map[string]bool{"excel": true, "ableton": true, "ansys": true, "kicad": true, "reaper": true, "photoshop": true}
	for id, e := range byID {
		if e.InstallProbe == nil {
			continue
		}
		p := e.InstallProbe
		// No Tier-1 row should use files[] (no literal-path probe in this batch). The
		// wave-2a uvx/npx rows (grafana/photoshop/zotero/jupyter/rmcp) gate on the
		// launcher (+ photoshop's host-exe glob), never a literal files[] path.
		if id == "excel" || id == "ableton" || id == "codex-mcp-server" || id == "matlab" || id == "ansys" || id == "kicad" || id == "reaper" ||
			id == "grafana" || id == "photoshop" || id == "zotero" || id == "jupyter" || id == "rmcp" {
			if len(p.Files) != 0 {
				t.Fatalf("Tier-1 row %q uses files[] %v — version globs belong in file_globs[]; files[] is exact-stat-only", id, p.Files)
			}
		}
		if wantGlobRows[id] {
			if len(p.FileGlobs) == 0 {
				t.Fatalf("Tier-1 glob row %q has no file_globs[] (its version-agnostic pattern was lost)", id)
			}
			for _, g := range p.FileGlobs {
				if !config.IsAbsolutePathShape(g) {
					t.Fatalf("Tier-1 row %q file_globs entry %q is not absolute-path-shaped", id, g)
				}
			}
		}
	}
}

// TestV2AbletonRow_GitPinnedProvenanceMatchesInstalledArtifact locks the
// Ableton row's provenance-consistency invariant. The catalog must execute the
// same immutable, reviewed source it advertises in vendored_source and
// readme_url. The row now installs the reviewed loopback-safe fork
// applicate2628/ableton-mcp-loopback (security fork of ahujasid/ableton-mcp,
// MIT): full upstream tool parity, but the in-Live Remote Script binds 127.0.0.1
// as a hard constant, fixing upstream's HOST=0.0.0.0 LAN exposure. It must still
// NOT install from the earlier build-fix fork applicate2628/ableton-mcp-extended,
// which was never security-reviewed for this row.
func TestV2AbletonRow_GitPinnedProvenanceMatchesInstalledArtifact(t *testing.T) {
	e := v2CatalogByID(t)["ableton"]
	if e == nil {
		t.Fatalf("v2 catalog missing the ableton row")
	}

	const wantRepo = "https://github.com/applicate2628/ableton-mcp-loopback"
	const forbiddenFork = "https://github.com/applicate2628/ableton-mcp-extended"

	vs := e.VendoredSource
	if vs == nil {
		t.Fatalf("ableton row lost its vendored_source")
	}
	if vs.Repo != wantRepo {
		t.Fatalf("ableton vendored_source.repo = %q, want trusted upstream %q", vs.Repo, wantRepo)
	}
	sha := strings.TrimSpace(vs.PinnedRef)
	if !gitSHA40.MatchString(sha) {
		t.Fatalf("ableton vendored_source.pinned_ref = %q, want a 40-hex git SHA", sha)
	}
	if config.IsMovingGitRef(vs.PinnedRef) {
		t.Fatalf("ableton pinned_ref %q is a moving ref (must be an immutable SHA)", vs.PinnedRef)
	}

	wantGitFrom := "git+" + wantRepo + "@" + sha
	joinedArgs := strings.Join(e.Args, " ")
	if !strings.Contains(joinedArgs, wantGitFrom) {
		t.Fatalf("ableton args %v do not install from the pinned upstream git source %q", e.Args, wantGitFrom)
	}
	if strings.Contains(joinedArgs, forbiddenFork) {
		t.Fatalf("ableton args %v still install from the untrusted personal fork %q", e.Args, forbiddenFork)
	}
	for label, cmd := range map[string]string{"install_cmd": vs.InstallCmd, "run_cmd": vs.RunCmd} {
		if !strings.Contains(cmd, wantGitFrom) {
			t.Fatalf("ableton vendored_source.%s = %q does not reference the pinned upstream git source %q", label, cmd, wantGitFrom)
		}
		if strings.Contains(cmd, forbiddenFork) {
			t.Fatalf("ableton vendored_source.%s = %q still references the untrusted personal fork %q", label, cmd, forbiddenFork)
		}
	}
}

// TestV2SunoRow_RequiredSecretGate pins the music-local suno row's shape: it is a
// READY local-stdio row (NOT disabled-until-probe) whose install gate is a REQUIRED
// vault secret. The row MUST declare required_secrets: [acedata_api_token] and back
// it with the matching env ref ACEDATACLOUD_API_TOKEN: secret:acedata_api_token, so
// the catalog authoring guard (validateCatalogVendoredAndAvailability) accepts it and
// the projected manifest's AdmissionCheck install gate blocks a token-less install
// (the mcp-suno server hard-exits without the token). It carries NO install_probe /
// availability gate (the secret is the gate) and NO vendored_source (it is an
// official PyPI package run via uvx, like the ansys row).
func TestV2SunoRow_RequiredSecretGate(t *testing.T) {
	e := v2CatalogByID(t)["suno"]
	if e == nil {
		t.Fatalf("v2 catalog missing the suno music-local row")
	}
	// READY (the secret is the gate, not a host-app probe). Absent availability ==
	// ready; an inert availability would wrongly grey it behind a host-app probe.
	if e.Availability != "" && e.Availability != "ready" {
		t.Fatalf("suno availability = %q, want ready/empty (the install gate is the required secret, not a probe)", e.Availability)
	}
	if e.InstallProbe != nil {
		t.Fatalf("suno carries an install_probe %#v — its gate is required_secrets, not a host-app probe", e.InstallProbe)
	}
	if e.VendoredSource != nil {
		t.Fatalf("suno carries a vendored_source %#v — it is an official PyPI package run via uvx, not a community fork", e.VendoredSource)
	}
	if e.Transport != "stdio" {
		t.Fatalf("suno transport = %q, want stdio (local mcp-suno over uvx)", e.Transport)
	}
	// required_secrets present and matched by a secret: env ref (the authoring guard).
	if len(e.RequiredSecrets) != 1 || e.RequiredSecrets[0] != "acedata_api_token" {
		t.Fatalf("suno required_secrets = %v, want [acedata_api_token]", e.RequiredSecrets)
	}
	if got := e.Env["ACEDATACLOUD_API_TOKEN"]; got != "secret:acedata_api_token" {
		t.Fatalf("suno env ACEDATACLOUD_API_TOKEN = %q, want secret:acedata_api_token (required_secrets must back an env secret ref)", got)
	}

	// MIRROR GATE: generate→`manifest create` dry-run must project required_secrets +
	// the secret env ref into a schema-valid manifest, so the post-install AdmissionCheck
	// re-sees the gate. (Port-0 placeholder is the operator's required edit.)
	draft, warns, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: t.TempDir()})
	if err != nil {
		t.Fatalf("generate draft for suno: %v (warnings=%v)", err, warns)
	}
	for _, want := range []string{"required_secrets:", "acedata_api_token", "secret:acedata_api_token"} {
		if !strings.Contains(draft, want) {
			t.Fatalf("suno draft missing %q\n---\n%s", want, draft)
		}
	}
	parseReady := strings.Replace(draft, "port: 0", "port: 9314", 1)
	m, err := config.ParseManifest(strings.NewReader(parseReady))
	if err != nil {
		t.Fatalf("suno drafted manifest failed ParseManifest+Validate (mirror gate): %v\n---\n%s", err, parseReady)
	}
	if len(m.RequiredSecrets) != 1 || m.RequiredSecrets[0] != "acedata_api_token" {
		t.Fatalf("suno drafted manifest lost required_secrets: %v", m.RequiredSecrets)
	}
	if got := m.Env["ACEDATACLOUD_API_TOKEN"]; got != "secret:acedata_api_token" {
		t.Fatalf("suno drafted manifest lost the secret env ref: %q", got)
	}
}

// TestV2OnshapeRow_NpxProbeAndOptionalSecretEnv pins the §9 cloud-CAD onshape row's
// shape. It is a DISABLED-UNTIL-PROBE local-stdio row whose probe gates on the npx
// launcher (cloud server — no host CAD app to detect), pinned to the npm
// onshape-mcp@0.4.0 package via a vendored_source SHA. The Onshape API-key env vars
// are wired as `secret:` vault refs but are deliberately NOT required_secrets: the
// server's `auto` auth mode supports an OAuth browser flow as an alternative to
// static API keys and does not hard-exit without them (per the server's
// authentication.md OAuthPending state), so the keys are the optional-secret posture
// (like paper-search's unpaywall_email), not a blocking install gate. The generate
// dry-run must keep the env values as VERBATIM secret refs (no resolved plaintext).
func TestV2OnshapeRow_NpxProbeAndOptionalSecretEnv(t *testing.T) {
	e := v2CatalogByID(t)["onshape"]
	if e == nil {
		t.Fatalf("v2 catalog missing the onshape row")
	}
	if e.Transport != "stdio" {
		t.Fatalf("onshape transport = %q, want stdio (npx onshape-mcp over stdio)", e.Transport)
	}
	if e.License != "Apache-2.0" {
		t.Fatalf("onshape license = %q, want Apache-2.0", e.License)
	}
	// Inert: gated behind the npx probe (cloud — no host-app glob).
	if e.Availability != config.AvailabilityDisabledUntilProbe {
		t.Fatalf("onshape availability = %q, want %q", e.Availability, config.AvailabilityDisabledUntilProbe)
	}
	if e.InstallProbe == nil {
		t.Fatalf("onshape row has no install_probe")
	}
	has := func(s []string, want string) bool {
		for _, v := range s {
			if v == want {
				return true
			}
		}
		return false
	}
	if !has(e.InstallProbe.Binaries, "npx") {
		t.Fatalf("onshape probe binaries %v MUST gate on the npx launcher (cloud server, no host app)", e.InstallProbe.Binaries)
	}
	// codex-bot #441 P2 (529): the probe MUST also require `node`, not just the
	// `npx` shim — a host with an npx shim on PATH but a missing/broken node
	// runtime would pass a `npx`-only catalog probe, persist the manifest, and
	// only fail later at the runtimeBehindLauncher("npx") preflight inside
	// Install (after the manifest write). Gating on `node` here blocks the row
	// at AvailabilityAdmissionEntry, before any manifest write.
	if !has(e.InstallProbe.Binaries, "node") {
		t.Fatalf("onshape probe binaries %v MUST also gate on the node runtime (an npx shim without node passes a npx-only probe then fails post-manifest-write)", e.InstallProbe.Binaries)
	}
	// CLOUD: no host-app file glob — the probe gates only on the npx/node launcher.
	if len(e.InstallProbe.Files) != 0 || len(e.InstallProbe.FileGlobs) != 0 {
		t.Fatalf("onshape probe must carry NO files[]/file_globs[] (cloud server, no host CAD app): files=%v file_globs=%v", e.InstallProbe.Files, e.InstallProbe.FileGlobs)
	}
	// ARCH GATE: the @onshape-mcp npm ships win32-x64 + linux/darwin (x64+arm64)
	// platform packages but NOT win32-arm64, so the probe arch-gates on exactly that
	// matrix. On a win-arm64 host the row stays inert instead of false-installing and
	// then failing at daemon spawn. windows/arm64 MUST be absent.
	wantPlatforms := map[string]bool{
		"windows/amd64": true, "linux/amd64": true, "linux/arm64": true,
		"darwin/amd64": true, "darwin/arm64": true,
	}
	if len(e.InstallProbe.Platforms) != len(wantPlatforms) {
		t.Fatalf("onshape probe platforms = %v, want exactly %d entries (the @onshape-mcp platform matrix)", e.InstallProbe.Platforms, len(wantPlatforms))
	}
	for _, p := range e.InstallProbe.Platforms {
		if !wantPlatforms[p] {
			t.Fatalf("onshape probe carries unexpected platform %q (want only %v)", p, wantPlatforms)
		}
	}
	if has(e.InstallProbe.Platforms, "windows/arm64") {
		t.Fatalf("onshape probe MUST NOT list windows/arm64 (the @onshape-mcp npm ships no win32-arm64 package); platforms=%v", e.InstallProbe.Platforms)
	}
	// Command + version pin: npx --yes onshape-mcp@0.4.0.
	if e.Command != "npx" {
		t.Fatalf("onshape command = %q, want npx", e.Command)
	}
	if !has(e.Args, "onshape-mcp@0.4.0") {
		t.Fatalf("onshape args %v must pin onshape-mcp@0.4.0", e.Args)
	}
	// vendored_source pinned to a 40-hex SHA (the v0.4.0 tag commit).
	vs := e.VendoredSource
	if vs == nil {
		t.Fatalf("onshape row lost its vendored_source")
	}
	if !gitSHA40.MatchString(strings.TrimSpace(vs.PinnedRef)) {
		t.Fatalf("onshape vendored_source.pinned_ref = %q, want a 40-hex git SHA (the v0.4.0 tag commit)", vs.PinnedRef)
	}
	if config.IsMovingGitRef(vs.PinnedRef) {
		t.Fatalf("onshape pinned_ref %q is a moving ref (must be an immutable SHA)", vs.PinnedRef)
	}
	// API-key env wired as secret: refs.
	if got := e.Env["ONSHAPE_MCP_AUTH__ACCESS_KEY"]; got != "secret:onshape_access_key" {
		t.Fatalf("onshape env ONSHAPE_MCP_AUTH__ACCESS_KEY = %q, want secret:onshape_access_key", got)
	}
	if got := e.Env["ONSHAPE_MCP_AUTH__SECRET_KEY"]; got != "secret:onshape_secret_key" {
		t.Fatalf("onshape env ONSHAPE_MCP_AUTH__SECRET_KEY = %q, want secret:onshape_secret_key", got)
	}
	// NOT a required_secrets gate (OAuth is a valid alternative; server does not
	// hard-exit without API keys).
	if len(e.RequiredSecrets) != 0 {
		t.Fatalf("onshape required_secrets = %v, want none (OAuth is a valid alternative; the keys are optional-secret posture)", e.RequiredSecrets)
	}

	// MIRROR GATE: generate→`manifest create` dry-run keeps the env as VERBATIM
	// secret refs (no resolved plaintext) and projects the npx probe + vendored pin
	// into a schema-valid manifest.
	draft, warns, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: t.TempDir()})
	if err != nil {
		t.Fatalf("generate draft for onshape: %v (warnings=%v)", err, warns)
	}
	for _, want := range []string{"secret:onshape_access_key", "secret:onshape_secret_key", "onshape-mcp@0.4.0"} {
		if !strings.Contains(draft, want) {
			t.Fatalf("onshape draft missing %q\n---\n%s", want, draft)
		}
	}
	parseReady := strings.Replace(draft, "port: 0", "port: 9315", 1)
	m, err := config.ParseManifest(strings.NewReader(parseReady))
	if err != nil {
		t.Fatalf("onshape drafted manifest failed ParseManifest+Validate (mirror gate): %v\n---\n%s", err, parseReady)
	}
	if got := m.Env["ONSHAPE_MCP_AUTH__ACCESS_KEY"]; got != "secret:onshape_access_key" {
		t.Fatalf("onshape drafted manifest env ACCESS_KEY = %q, want verbatim secret ref", got)
	}
	if got := m.Env["ONSHAPE_MCP_AUTH__SECRET_KEY"]; got != "secret:onshape_secret_key" {
		t.Fatalf("onshape drafted manifest env SECRET_KEY = %q, want verbatim secret ref", got)
	}
	if len(m.RequiredSecrets) != 0 {
		t.Fatalf("onshape drafted manifest gained required_secrets %v (must stay optional-secret)", m.RequiredSecrets)
	}
	if m.VendoredSource == nil || strings.TrimSpace(m.VendoredSource.PinnedRef) == "" {
		t.Fatalf("onshape drafted manifest lost the pinned vendored_source: %#v", m.VendoredSource)
	}
	// MIRROR GATE: the platforms[] arch matrix survives generate→manifest create so
	// the post-install readiness gate re-evaluates the same arch allowlist.
	if m.InstallProbe == nil || len(m.InstallProbe.Platforms) != len(wantPlatforms) {
		t.Fatalf("onshape drafted manifest dropped the install_probe.platforms matrix: %#v", m.InstallProbe)
	}
	for _, p := range m.InstallProbe.Platforms {
		if !wantPlatforms[p] {
			t.Fatalf("onshape drafted manifest carries unexpected platform %q", p)
		}
	}
}

// --- vendor-breadth wave-2a row shape locks ---

// hasStr is a tiny membership helper for the wave-2a row assertions.
func hasStr(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// TestV2SecretGatedVendoredRows_RequiredSecretGate pins the tableau + metabase rows:
// each is a READY local-stdio row (NOT disabled-until-probe) whose install gate is a
// REQUIRED vault secret backing a secret: env ref, carries a pinned vendored_source
// (npm package via npx), and projects required_secrets + the secret env refs into a
// schema-valid manifest (the post-install AdmissionCheck re-sees the gate). Both
// servers hard-exit on startup without their auth env (verified by a clean-cache
// launch), which is exactly why they take the required_secrets posture rather than
// onshape's optional-secret one.
func TestV2SecretGatedVendoredRows_RequiredSecretGate(t *testing.T) {
	byID := v2CatalogByID(t)
	cases := []struct {
		id        string
		license   string
		secrets   []string
		secretEnv map[string]string // env key -> expected secret: ref
		pinArg    string            // a version-pinned npm arg the row must carry
	}{
		{
			// The Tableau instance URL is routed through required_secrets (no
			// required-non-secret-config mechanism exists), so SERVER is a
			// secret: ref AND in required_secrets — the install BLOCKS until the
			// operator sets the real URL, never shipping the placeholder default.
			id:      "tableau",
			license: "Apache-2.0",
			secrets: []string{"tableau_server_url", "tableau_pat_name", "tableau_pat_value"},
			secretEnv: map[string]string{
				"SERVER":    "secret:tableau_server_url",
				"PAT_NAME":  "secret:tableau_pat_name",
				"PAT_VALUE": "secret:tableau_pat_value",
			},
			pinArg: "@tableau/mcp-server@2.18.1",
		},
		{
			// Same required-URL gate for metabase: METABASE_URL is a secret: ref
			// AND in required_secrets so the placeholder URL can never ship as the
			// live default.
			id:      "metabase",
			license: "MIT",
			secrets: []string{"metabase_url", "metabase_api_key"},
			secretEnv: map[string]string{
				"METABASE_URL":     "secret:metabase_url",
				"METABASE_API_KEY": "secret:metabase_api_key",
			},
			pinArg: "@easecloudio/mcp-metabase-server@1.3.0",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.id, func(t *testing.T) {
			e := byID[tc.id]
			if e == nil {
				t.Fatalf("v2 catalog missing the %s row", tc.id)
			}
			if e.Transport != "stdio" {
				t.Fatalf("%s transport = %q, want stdio", tc.id, e.Transport)
			}
			if e.License != tc.license {
				t.Fatalf("%s license = %q, want %q", tc.id, e.License, tc.license)
			}
			// READY (the secret is the gate, not a host-app probe). An inert
			// availability would wrongly grey it behind a non-existent host-app probe.
			if e.Availability != "" && e.Availability != "ready" {
				t.Fatalf("%s availability = %q, want ready/empty (the install gate is the required secret)", tc.id, e.Availability)
			}
			if e.InstallProbe != nil {
				t.Fatalf("%s carries an install_probe %#v — its gate is required_secrets, not a host-app probe", tc.id, e.InstallProbe)
			}
			// required_secrets present, in order, each backed by its secret: env ref.
			if len(e.RequiredSecrets) != len(tc.secrets) {
				t.Fatalf("%s required_secrets = %v, want %v", tc.id, e.RequiredSecrets, tc.secrets)
			}
			for i, want := range tc.secrets {
				if e.RequiredSecrets[i] != want {
					t.Fatalf("%s required_secrets[%d] = %q, want %q", tc.id, i, e.RequiredSecrets[i], want)
				}
			}
			for envKey, wantRef := range tc.secretEnv {
				if got := e.Env[envKey]; got != wantRef {
					t.Fatalf("%s env %s = %q, want %q (required_secrets must back a secret: env ref)", tc.id, envKey, got, wantRef)
				}
			}
			// Version-pinned npm arg present (no floating @latest).
			if !hasStr(e.Args, tc.pinArg) {
				t.Fatalf("%s args %v must pin %q", tc.id, e.Args, tc.pinArg)
			}
			// Pinned vendored_source (immutable SHA — the npm version's tag commit).
			vs := e.VendoredSource
			if vs == nil || !gitSHA40.MatchString(strings.TrimSpace(vs.PinnedRef)) {
				t.Fatalf("%s vendored_source.pinned_ref = %#v, want a 40-hex git SHA", tc.id, vs)
			}
			if config.IsMovingGitRef(vs.PinnedRef) {
				t.Fatalf("%s pinned_ref %q is a moving ref", tc.id, vs.PinnedRef)
			}

			// MIRROR GATE: generate→`manifest create` dry-run projects required_secrets +
			// the VERBATIM secret env refs (no resolved plaintext) into a schema-valid
			// manifest.
			draft, warns, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: t.TempDir()})
			if err != nil {
				t.Fatalf("generate draft for %s: %v (warnings=%v)", tc.id, err, warns)
			}
			wants := append([]string{"required_secrets:"}, tc.secrets...)
			for _, ref := range tc.secretEnv {
				wants = append(wants, ref)
			}
			for _, want := range wants {
				if !strings.Contains(draft, want) {
					t.Fatalf("%s draft missing %q\n---\n%s", tc.id, want, draft)
				}
			}
			parseReady := strings.Replace(draft, "port: 0", "port: 9320", 1)
			m, err := config.ParseManifest(strings.NewReader(parseReady))
			if err != nil {
				t.Fatalf("%s drafted manifest failed ParseManifest+Validate (mirror gate): %v\n---\n%s", tc.id, err, parseReady)
			}
			if len(m.RequiredSecrets) != len(tc.secrets) {
				t.Fatalf("%s drafted manifest lost required_secrets: %v", tc.id, m.RequiredSecrets)
			}
			for envKey, wantRef := range tc.secretEnv {
				if got := m.Env[envKey]; got != wantRef {
					t.Fatalf("%s drafted manifest env %s = %q, want verbatim secret ref %q", tc.id, envKey, got, wantRef)
				}
			}
			if m.VendoredSource == nil || strings.TrimSpace(m.VendoredSource.PinnedRef) == "" {
				t.Fatalf("%s drafted manifest lost the pinned vendored_source: %#v", tc.id, m.VendoredSource)
			}
		})
	}
}

// TestV2DataRows_NoPlaceholderURLDefaultAndReadOnlyToolMode locks the bot #448 P2
// config-correctness fixes for the tableau + metabase rows, which the GUI one-click
// path writes VERBATIM with no operator-edit step:
//
//   findings 2+3 — the instance URL must NOT ship as a placeholder http(s):// literal
//     default (that would silently target a sample URL on every install). Since there
//     is no required-non-secret-config mechanism, the URL is routed through the
//     required_secrets gate as a secret: env ref, so a one-click install BLOCKS until
//     the operator supplies the real URL. The URL env value must therefore be a
//     `secret:` ref AND the key must be in required_secrets — never a bare URL.
//   finding 1 — metabase must default to the upstream NON-destructive tool set
//     (TOOL_MODE=read); omitting it would default upstream to `all` (create/update/
//     delete). The operator opts into the destructive surface explicitly.
func TestV2DataRows_NoPlaceholderURLDefaultAndReadOnlyToolMode(t *testing.T) {
	byID := v2CatalogByID(t)

	// The URL env key per row + the required_secrets key it must be gated by.
	urlGate := map[string]struct{ envKey, secretKey string }{
		"tableau":  {"SERVER", "tableau_server_url"},
		"metabase": {"METABASE_URL", "metabase_url"},
	}
	for id, g := range urlGate {
		e := byID[id]
		if e == nil {
			t.Fatalf("v2 catalog missing the %s row", id)
		}
		got := e.Env[g.envKey]
		// MUST be a secret: ref, never a bare http(s) placeholder default.
		if !strings.HasPrefix(got, "secret:") {
			t.Fatalf("%s env %s = %q — the instance URL must be a secret: ref (required-URL gate), not a placeholder literal that ships as the live default", id, g.envKey, got)
		}
		if strings.HasPrefix(got, "secret:http") || strings.Contains(got, "://") {
			t.Fatalf("%s env %s = %q looks like a bare URL leaked into the value", id, g.envKey, got)
		}
		// AND must be in required_secrets so the one-click install BLOCKS until set.
		if !hasStr(e.RequiredSecrets, g.secretKey) {
			t.Fatalf("%s required_secrets %v must include %q so the install blocks until the real URL is supplied (no placeholder default)", id, e.RequiredSecrets, g.secretKey)
		}
		// Belt-and-suspenders: no env value anywhere is a bare http(s) literal.
		for k, v := range e.Env {
			if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
				t.Fatalf("%s env %s = %q ships a placeholder URL literal as the live default; route it through the required_secrets gate instead", id, k, v)
			}
		}
	}

	// finding 1: metabase ships read-only (TOOL_MODE=read), never the destructive
	// upstream `all` default.
	mb := byID["metabase"]
	if got := mb.Env["TOOL_MODE"]; got != "read" {
		t.Fatalf("metabase env TOOL_MODE = %q, want \"read\" (non-destructive default; omitting it defaults upstream to `all` which can create/update/delete)", got)
	}
}

// TestV2TableauRow_NodeEngineNoteInSummary pins bot #448 finding 5: the tableau row's
// npm package declares engines.node >= 22.7.5 and the probe cannot version-check node,
// so the summary MUST loudly document the requirement (the failure on an older node is
// a clear spawn-time version error, not silent — a summary note is the agreed mitigation,
// no node-version probe field is added).
func TestV2TableauRow_NodeEngineNoteInSummary(t *testing.T) {
	e := v2CatalogByID(t)["tableau"]
	if e == nil {
		t.Fatalf("v2 catalog missing the tableau row")
	}
	if !strings.Contains(e.Summary, "22.7.5") {
		t.Fatalf("tableau summary must document the node >= 22.7.5 requirement (engines.node from @tableau/mcp-server@2.18.1); summary=%q", e.Summary)
	}
}

// TestV2PhotoshopRow_WindowsArchGate pins the photoshop row's Windows-only COM shape:
// disabled-until-probe gated on uvx + a host Adobe Photoshop host-exe glob + an arch
// matrix of exactly [windows/amd64] (win32com COM is Windows-only). On any non-Windows
// host the row stays inert. The row carries NO secret and NO PS_VERSION (v0.1.11
// auto-detects via COM). The arch matrix must survive generate→manifest
// create so the post-install readiness gate re-evaluates the same allowlist.
func TestV2PhotoshopRow_WindowsArchGate(t *testing.T) {
	e := v2CatalogByID(t)["photoshop"]
	if e == nil {
		t.Fatalf("v2 catalog missing the photoshop row")
	}
	if e.Availability != config.AvailabilityDisabledUntilProbe {
		t.Fatalf("photoshop availability = %q, want %q", e.Availability, config.AvailabilityDisabledUntilProbe)
	}
	p := e.InstallProbe
	if p == nil {
		t.Fatalf("photoshop row has no install_probe")
	}
	if !hasStr(p.Binaries, "uvx") {
		t.Fatalf("photoshop probe binaries %v must gate on uvx", p.Binaries)
	}
	if len(p.FileGlobs) == 0 {
		t.Fatalf("photoshop probe must carry a host Photoshop file_globs entry")
	}
	for _, g := range p.FileGlobs {
		if !config.IsAbsolutePathShape(g) {
			t.Fatalf("photoshop file_globs entry %q is not absolute-path-shaped", g)
		}
	}
	// Arch matrix is EXACTLY windows/amd64 (COM is Windows-only).
	if len(p.Platforms) != 1 || p.Platforms[0] != "windows/amd64" {
		t.Fatalf("photoshop probe platforms = %v, want exactly [windows/amd64] (win32com COM is Windows-only)", p.Platforms)
	}
	// No secret on this row, and no env at all.
	if len(e.RequiredSecrets) != 0 {
		t.Fatalf("photoshop required_secrets = %v, want none", e.RequiredSecrets)
	}
	for _, v := range e.Env {
		if strings.HasPrefix(v, "secret:") {
			t.Fatalf("photoshop env carries a secret ref %q — the row has no secret", v)
		}
	}
	// PROBE↔PS_VERSION ALIGNMENT (bot #448 finding 4): v0.1.11's COM adapter builds
	// Session()/Application() with NO version argument (auto-detects the installed
	// Photoshop via the COM ProgID) — it does NOT read PS_VERSION. A hardcoded
	// PS_VERSION=2024 would be inert AND would imply a version pin that does not
	// happen, falsely conflicting with the version-agnostic `Adobe Photoshop *`
	// glob. So the row MUST NOT set PS_VERSION; the glob stays version-agnostic so
	// it matches the auto-detect behavior for ANY installed year.
	if _, ok := e.Env["PS_VERSION"]; ok {
		t.Fatalf("photoshop must NOT set PS_VERSION (inert in v0.1.11, which auto-detects via COM; a hardcoded year falsely conflicts with the version-agnostic glob): env=%v", e.Env)
	}
	// The glob must stay version-agnostic (not narrowed to a single year), matching
	// the auto-detect behavior — assert it does not pin a 4-digit year segment.
	for _, g := range p.FileGlobs {
		if strings.Contains(g, "Adobe Photoshop 20") {
			t.Fatalf("photoshop file_globs %q narrows to a specific year; must stay version-agnostic (Adobe Photoshop *) to match COM auto-detect", g)
		}
	}
	// MIRROR GATE: the arch matrix survives generate→manifest create.
	draft, warns, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: t.TempDir()})
	if err != nil {
		t.Fatalf("generate draft for photoshop: %v (warnings=%v)", err, warns)
	}
	parseReady := strings.Replace(draft, "port: 0", "port: 9321", 1)
	m, err := config.ParseManifest(strings.NewReader(parseReady))
	if err != nil {
		t.Fatalf("photoshop drafted manifest failed ParseManifest+Validate (mirror gate): %v\n---\n%s", err, parseReady)
	}
	if m.InstallProbe == nil || len(m.InstallProbe.Platforms) != 1 || m.InstallProbe.Platforms[0] != "windows/amd64" {
		t.Fatalf("photoshop drafted manifest dropped/changed the [windows/amd64] arch gate: %#v", m.InstallProbe)
	}
}

// TestV2OptionalSecretRows_NotRequiredGate pins the grafana + jupyter rows' optional-
// secret posture: each is disabled-until-probe (gated on uvx), wires its server token
// as a secret: env ref, but is deliberately NOT a required_secrets install gate — the
// server STARTS without the token (verified by a clean-cache launch: grafana enforces
// auth per-tool-call, jupyter connects to Jupyter lazily), so a one-click install is
// not hard-blocked. This is the onshape optional-secret posture, not the suno one.
func TestV2OptionalSecretRows_NotRequiredGate(t *testing.T) {
	byID := v2CatalogByID(t)
	cases := []struct {
		id        string
		secretEnv map[string]string
	}{
		{id: "grafana", secretEnv: map[string]string{"GRAFANA_SERVICE_ACCOUNT_TOKEN": "secret:grafana_service_account_token"}},
		{id: "jupyter", secretEnv: map[string]string{"JUPYTER_TOKEN": "secret:jupyter_token"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.id, func(t *testing.T) {
			e := byID[tc.id]
			if e == nil {
				t.Fatalf("v2 catalog missing the %s row", tc.id)
			}
			if e.Availability != config.AvailabilityDisabledUntilProbe {
				t.Fatalf("%s availability = %q, want %q", tc.id, e.Availability, config.AvailabilityDisabledUntilProbe)
			}
			if e.InstallProbe == nil || !hasStr(e.InstallProbe.Binaries, "uvx") {
				t.Fatalf("%s probe must gate on uvx: %#v", tc.id, e.InstallProbe)
			}
			// The token is wired as a secret: ref...
			for envKey, wantRef := range tc.secretEnv {
				if got := e.Env[envKey]; got != wantRef {
					t.Fatalf("%s env %s = %q, want %q", tc.id, envKey, got, wantRef)
				}
			}
			// ...but is NOT a required_secrets install gate (optional-secret posture).
			if len(e.RequiredSecrets) != 0 {
				t.Fatalf("%s required_secrets = %v, want none (server starts without the token — optional-secret posture)", tc.id, e.RequiredSecrets)
			}
		})
	}
}

// TestV2StdioPinnedRows_PinAwayFrom0000 locks the loopback-discipline invariant for
// the two rows whose upstream ships an opt-in HTTP transport that binds 0.0.0.0: the
// row's args must PIN the stdio entry so a one-click install never silently opens a
// LAN listener. jupyter passes --transport stdio (the streamable-http mode binds
// host=0.0.0.0); rmcp runs the `start` subcommand (the serve-http mode binds
// --host 0.0.0.0). Neither row may carry the http-mode token in its args.
func TestV2StdioPinnedRows_PinAwayFrom0000(t *testing.T) {
	byID := v2CatalogByID(t)

	jup := byID["jupyter"]
	if jup == nil {
		t.Fatalf("v2 catalog missing the jupyter row")
	}
	if !hasStr(jup.Args, "stdio") || !hasStr(jup.Args, "--transport") {
		t.Fatalf("jupyter args %v must pin --transport stdio (the streamable-http mode binds 0.0.0.0)", jup.Args)
	}
	if hasStr(jup.Args, "streamable-http") {
		t.Fatalf("jupyter args %v must NOT select the 0.0.0.0 streamable-http transport", jup.Args)
	}

	r := byID["rmcp"]
	if r == nil {
		t.Fatalf("v2 catalog missing the rmcp row")
	}
	if !hasStr(r.Args, "start") {
		t.Fatalf("rmcp args %v must run the `start` (stdio) subcommand (serve-http binds --host 0.0.0.0)", r.Args)
	}
	if hasStr(r.Args, "serve-http") {
		t.Fatalf("rmcp args %v must NOT select the 0.0.0.0 serve-http subcommand", r.Args)
	}
}

// --- vendor-breadth wave-2b row shape locks ---

// TestV2Wave2bSecretGatedRows_RequiredSecretGate pins the obsidian + logseq rows: each
// is a READY local-stdio row (NOT disabled-until-probe) whose install gate is a REQUIRED
// vault secret backing a secret: env ref, carries a pinned vendored_source, and projects
// required_secrets + the secret env ref into a schema-valid manifest (the post-install
// AdmissionCheck re-sees the gate). Both servers HARD-EXIT on startup without their token
// (verified by a clean-cache launch), which is exactly why they take the required_secrets
// posture rather than an install_probe. logseq additionally pins --transport stdio
// (the http mode defaults to 127.0.0.1 and needs --insecure to leave loopback), and its
// LOGSEQ_API_URL stays an editable localhost-default literal (not a required secret).
func TestV2Wave2bSecretGatedRows_RequiredSecretGate(t *testing.T) {
	byID := v2CatalogByID(t)
	cases := []struct {
		id        string
		secrets   []string
		secretEnv map[string]string // env key -> expected secret: ref
		pinArg    string            // a version-pinned arg the row must carry
	}{
		{
			id:        "obsidian",
			secrets:   []string{"obsidian_api_key"},
			secretEnv: map[string]string{"OBSIDIAN_API_KEY": "secret:obsidian_api_key"},
			pinArg:    "mcp-obsidian==0.2.2",
		},
		{
			id:        "logseq",
			secrets:   []string{"logseq_token"},
			secretEnv: map[string]string{"LOGSEQ_API_TOKEN": "secret:logseq_token"},
			pinArg:    "mcp-logseq==1.8.0",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.id, func(t *testing.T) {
			e := byID[tc.id]
			if e == nil {
				t.Fatalf("v2 catalog missing the %s row", tc.id)
			}
			if e.Transport != "stdio" {
				t.Fatalf("%s transport = %q, want stdio", tc.id, e.Transport)
			}
			if e.License != "MIT" {
				t.Fatalf("%s license = %q, want MIT", tc.id, e.License)
			}
			// READY (the secret is the gate, not a host-app probe).
			if e.Availability != "" && e.Availability != "ready" {
				t.Fatalf("%s availability = %q, want ready/empty (the install gate is the required secret)", tc.id, e.Availability)
			}
			if e.InstallProbe != nil {
				t.Fatalf("%s carries an install_probe %#v — its gate is required_secrets, not a host-app probe", tc.id, e.InstallProbe)
			}
			// required_secrets present, in order, each backed by its secret: env ref.
			if len(e.RequiredSecrets) != len(tc.secrets) {
				t.Fatalf("%s required_secrets = %v, want %v", tc.id, e.RequiredSecrets, tc.secrets)
			}
			for i, want := range tc.secrets {
				if e.RequiredSecrets[i] != want {
					t.Fatalf("%s required_secrets[%d] = %q, want %q", tc.id, i, e.RequiredSecrets[i], want)
				}
			}
			for envKey, wantRef := range tc.secretEnv {
				if got := e.Env[envKey]; got != wantRef {
					t.Fatalf("%s env %s = %q, want %q (required_secrets must back a secret: env ref)", tc.id, envKey, got, wantRef)
				}
			}
			if !hasStr(e.Args, tc.pinArg) {
				t.Fatalf("%s args %v must pin %q", tc.id, e.Args, tc.pinArg)
			}
			// Pinned vendored_source (SHA or tag — neither a moving ref).
			vs := e.VendoredSource
			if vs == nil || strings.TrimSpace(vs.PinnedRef) == "" {
				t.Fatalf("%s vendored_source.pinned_ref = %#v, want a pinned SHA/tag", tc.id, vs)
			}
			if config.IsMovingGitRef(vs.PinnedRef) {
				t.Fatalf("%s pinned_ref %q is a moving ref", tc.id, vs.PinnedRef)
			}

			// MIRROR GATE: generate→`manifest create` dry-run projects required_secrets +
			// the VERBATIM secret env ref (no resolved plaintext) into a schema-valid manifest.
			draft, warns, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: t.TempDir()})
			if err != nil {
				t.Fatalf("generate draft for %s: %v (warnings=%v)", tc.id, err, warns)
			}
			wants := append([]string{"required_secrets:"}, tc.secrets...)
			for _, ref := range tc.secretEnv {
				wants = append(wants, ref)
			}
			for _, want := range wants {
				if !strings.Contains(draft, want) {
					t.Fatalf("%s draft missing %q\n---\n%s", tc.id, want, draft)
				}
			}
			parseReady := strings.Replace(draft, "port: 0", "port: 9330", 1)
			m, err := config.ParseManifest(strings.NewReader(parseReady))
			if err != nil {
				t.Fatalf("%s drafted manifest failed ParseManifest+Validate (mirror gate): %v\n---\n%s", tc.id, err, parseReady)
			}
			if len(m.RequiredSecrets) != len(tc.secrets) {
				t.Fatalf("%s drafted manifest lost required_secrets: %v", tc.id, m.RequiredSecrets)
			}
			for envKey, wantRef := range tc.secretEnv {
				if got := m.Env[envKey]; got != wantRef {
					t.Fatalf("%s drafted manifest env %s = %q, want verbatim secret ref %q", tc.id, envKey, got, wantRef)
				}
			}
			if m.VendoredSource == nil || strings.TrimSpace(m.VendoredSource.PinnedRef) == "" {
				t.Fatalf("%s drafted manifest lost the pinned vendored_source: %#v", tc.id, m.VendoredSource)
			}
		})
	}

	// logseq-specific loopback discipline: the row must PIN --transport stdio (the http
	// mode defaults to 127.0.0.1 but needs --insecure to bind non-loopback; pinning stdio
	// avoids the question entirely), and must NOT carry the http transport token.
	lg := byID["logseq"]
	if !hasStr(lg.Args, "stdio") || !hasStr(lg.Args, "--transport") {
		t.Fatalf("logseq args %v must pin --transport stdio (the http mode can leave loopback via --insecure)", lg.Args)
	}
	if hasStr(lg.Args, "http") {
		t.Fatalf("logseq args %v must NOT select the http transport", lg.Args)
	}
	// LOGSEQ_API_URL stays an editable localhost-default literal (has a default, so it is
	// NOT routed through required_secrets), consistent with grafana/jupyter localhost URLs.
	if got := lg.Env["LOGSEQ_API_URL"]; !strings.HasPrefix(got, "http://localhost") {
		t.Fatalf("logseq env LOGSEQ_API_URL = %q, want a localhost default literal (it has a default, so it is an editable literal not a secret)", got)
	}
	if hasStr(lg.RequiredSecrets, "logseq_api_url") {
		t.Fatalf("logseq required_secrets %v must NOT gate the localhost-default URL", lg.RequiredSecrets)
	}
}

// TestV2OriginProRow_WindowsArchGate pins the origin-pro row's Windows-only COM shape:
// disabled-until-probe gated on uvx + a host OriginLab Origin host-exe glob + an arch
// matrix of exactly [windows/amd64] (win32com COM is Windows-only). It is loopback-clean
// by construction (stdio + COM, no socket), carries NO secret, and pins origin-pro-mcp
// ==0.1.0. The arch matrix must survive generate→manifest create so the post-install
// readiness gate re-evaluates the same allowlist.
func TestV2OriginProRow_WindowsArchGate(t *testing.T) {
	e := v2CatalogByID(t)["origin-pro"]
	if e == nil {
		t.Fatalf("v2 catalog missing the origin-pro row")
	}
	if e.Transport != "stdio" {
		t.Fatalf("origin-pro transport = %q, want stdio (stdio + Windows COM, no socket)", e.Transport)
	}
	if e.License != "MIT" {
		t.Fatalf("origin-pro license = %q, want MIT", e.License)
	}
	if e.Availability != config.AvailabilityDisabledUntilProbe {
		t.Fatalf("origin-pro availability = %q, want %q", e.Availability, config.AvailabilityDisabledUntilProbe)
	}
	p := e.InstallProbe
	if p == nil {
		t.Fatalf("origin-pro row has no install_probe")
	}
	if !hasStr(p.Binaries, "uvx") {
		t.Fatalf("origin-pro probe binaries %v must gate on uvx", p.Binaries)
	}
	if len(p.FileGlobs) == 0 {
		t.Fatalf("origin-pro probe must carry a host OriginLab Origin file_globs entry")
	}
	for _, g := range p.FileGlobs {
		if !config.IsAbsolutePathShape(g) {
			t.Fatalf("origin-pro file_globs entry %q is not absolute-path-shaped", g)
		}
	}
	// Arch matrix is EXACTLY windows/amd64 (win32com COM is Windows-only).
	if len(p.Platforms) != 1 || p.Platforms[0] != "windows/amd64" {
		t.Fatalf("origin-pro probe platforms = %v, want exactly [windows/amd64] (win32com COM is Windows-only)", p.Platforms)
	}
	// No secret on this row.
	if len(e.RequiredSecrets) != 0 {
		t.Fatalf("origin-pro required_secrets = %v, want none", e.RequiredSecrets)
	}
	for _, v := range e.Env {
		if strings.HasPrefix(v, "secret:") {
			t.Fatalf("origin-pro env carries a secret ref %q — the row has no secret", v)
		}
	}
	if !hasStr(e.Args, "origin-pro-mcp==0.1.0") {
		t.Fatalf("origin-pro args %v must pin origin-pro-mcp==0.1.0", e.Args)
	}
	// MIRROR GATE: the arch matrix survives generate→manifest create.
	draft, warns, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: t.TempDir()})
	if err != nil {
		t.Fatalf("generate draft for origin-pro: %v (warnings=%v)", err, warns)
	}
	parseReady := strings.Replace(draft, "port: 0", "port: 9331", 1)
	m, err := config.ParseManifest(strings.NewReader(parseReady))
	if err != nil {
		t.Fatalf("origin-pro drafted manifest failed ParseManifest+Validate (mirror gate): %v\n---\n%s", err, parseReady)
	}
	if m.Availability != config.AvailabilityDisabledUntilProbe {
		t.Fatalf("origin-pro drafted manifest lost availability gate: av=%q", m.Availability)
	}
	if m.InstallProbe == nil || len(m.InstallProbe.Platforms) != 1 || m.InstallProbe.Platforms[0] != "windows/amd64" {
		t.Fatalf("origin-pro drafted manifest dropped/changed the [windows/amd64] arch gate: %#v", m.InstallProbe)
	}
	if m.VendoredSource == nil || strings.TrimSpace(m.VendoredSource.PinnedRef) == "" {
		t.Fatalf("origin-pro drafted manifest lost the pinned vendored_source: %#v", m.VendoredSource)
	}
}

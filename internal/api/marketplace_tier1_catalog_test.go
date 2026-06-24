package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"
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
var tier1CatalogIDs = []string{"excel", "ableton", "codex-mcp-server", "matlab", "ansys", "kicad"}

// tierMusicLocalCatalogIDs are the v2-only LOCAL-stdio music rows appended LAST,
// after the Tier-1 desktop-app rows (so the frozen 14-entry v1 prefix at indices
// 0-13 stays byte-identical to marketplace/v1/catalog.json). suno was originally a
// remote-http row, but a hosted remote endpoint cannot REQUIRE an operator
// credential at install: generateRemoteHTTPDraft emits only name/kind/transport/url
// (no headers), so a one-click install yields a server returning 401 on every
// request (codex-bot PR #429 P1). It was switched to the official mcp-suno PyPI
// package run locally over stdio with uvx, carrying an `env` secret ref
// (ACEDATACLOUD_API_TOKEN = secret:acedata_api_token) that the readiness gate
// surfaces as a concrete per-key install prompt AND that the mcp-suno package
// itself hard-requires (it exit(1)s on startup when the token is unset). These
// rows are NOT folded into tier1CatalogIDs because they carry NO vendored_source
// (mcp-suno is an official AceDataCloud package, not a community fork — like
// codex-mcp-server/matlab/ansys) and NO install_probe (uvx fetches the package on
// demand; there is no host-installed artifact to gate on). They ARE stdio rows, so
// they share the stdio draft shape (command/args/env, port-0 daemon) the remote
// rows lack.
var tierMusicLocalCatalogIDs = []string{"suno"}

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
	wantCount := len(v1cat.Entries) + len(tier1CatalogIDs) + len(tierMusicLocalCatalogIDs)
	if len(cat.Entries) != wantCount {
		t.Fatalf("v2 catalog entry count = %d, want %d (v1 %d + %d Tier-1 + %d music-local)",
			len(cat.Entries), wantCount, len(v1cat.Entries), len(tier1CatalogIDs), len(tierMusicLocalCatalogIDs))
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
	// via vendored_source like excel/ableton.
	for _, id := range []string{"excel", "ableton", "kicad"} {
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
			if m.InstallProbe == nil || (len(m.InstallProbe.Binaries) == 0 && len(m.InstallProbe.Files) == 0 && len(m.InstallProbe.FileGlobs) == 0) {
				t.Fatalf("row %q drafted manifest lost install_probe", id)
			}
			// The fork rows project the pinned vendored_source; the official rows
			// (codex, matlab, ansys) do not.
			switch id {
			case "excel", "ableton", "kicad":
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

// TestV2MusicLocalRows_GenerateThenCreateDryRun is the MIRROR GATE for the v2-only
// LOCAL-stdio music rows: for each row, project it through GenerateDraftManifest
// (the `mcphub marketplace generate <id>` path → the transport=stdio branch →
// generateCommandDraft), substitute a real port for the placeholder 0 (the
// operator's required edit), then ParseManifest+Validate the drafted stdio
// manifest. A clean draft + clean Validate proves the row is gate-clean (it would
// be accepted by `mcphub manifest create`). Fully in-process: no daemon spawn, no
// state-dir write — state-safe with no env redirection.
//
// The CREDENTIAL GATE is the point of the switch from remote-http (codex-bot PR
// #429 P1): a stdio `env` secret ref (`ACEDATACLOUD_API_TOKEN = secret:<key>`)
// survives the draft verbatim (the expander only touches ${...} placeholders, and a
// bare `secret:<key>` value has none), and HasSecretRef on the drafted manifest's
// env confirms the readiness gate will surface a concrete per-key install prompt for
// it — exactly the operator-facing credential requirement a hosted remote endpoint
// (which materializes NO headers in generateRemoteHTTPDraft) cannot provide.
func TestV2MusicLocalRows_GenerateThenCreateDryRun(t *testing.T) {
	byID := v2CatalogByID(t)
	ws := t.TempDir()
	for _, id := range tierMusicLocalCatalogIDs {
		id := id
		t.Run(id, func(t *testing.T) {
			e, ok := byID[id]
			if !ok {
				t.Fatalf("v2 catalog missing music-local row %q", id)
			}
			// Shape contract: local-stdio row carries a command (no remote url), and —
			// unlike a remote endpoint — at least one `env` secret ref so the install
			// REQUIRES a credential (the bot's exact P1 ask). It carries NO
			// vendored_source (mcp-suno is an official AceDataCloud package, not a
			// community fork) and NO install_probe (uvx fetches on demand).
			if e.Transport != "stdio" {
				t.Fatalf("music-local row %q transport = %q, want \"stdio\"", id, e.Transport)
			}
			if strings.TrimSpace(e.URL) != "" {
				t.Fatalf("music-local row %q must NOT carry a url (it is a local stdio server): %q", id, e.URL)
			}
			if strings.TrimSpace(e.Command) == "" {
				t.Fatalf("music-local row %q has an empty command", id)
			}
			if e.VendoredSource != nil {
				t.Fatalf("music-local row %q must NOT carry vendored_source (official package, not a community fork): %#v", id, e.VendoredSource)
			}
			if e.InstallProbe != nil {
				t.Fatalf("music-local row %q must NOT carry install_probe (uvx fetches the package on demand): %#v", id, e.InstallProbe)
			}
			// CREDENTIAL GATE: the catalog row must carry an env secret ref so the
			// install surfaces a required-credential prompt. A remote-http row could not
			// (generateRemoteHTTPDraft emits no headers).
			if !secrets.HasSecretRef(e.Env) {
				t.Fatalf("music-local row %q carries no `env` secret: ref — the credential gate is the whole point of the local switch (PR #429 P1): env=%#v", id, e.Env)
			}
			// The env value must be the bare `secret:<key>` form (like paper-search-mcp),
			// NOT a ${secret:KEY} placeholder (that form is for remote url/headers).
			for k, v := range e.Env {
				if strings.HasPrefix(v, "secret:") && strings.Contains(v, "${") {
					t.Fatalf("music-local row %q env[%q] = %q mixes the ${...} placeholder form into a bare secret: env ref", id, k, v)
				}
			}

			draft, warns, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: ws})
			if err != nil {
				t.Fatalf("generate stdio draft for %q: %v (warnings=%v)", id, err, warns)
			}
			// The operator's required edit: pick a real port for the placeholder 0.
			parseReady := strings.Replace(draft, "port: 0", "port: 9312", 1)
			m, err := config.ParseManifest(strings.NewReader(parseReady))
			if err != nil {
				t.Fatalf("row %q drafted stdio manifest failed ParseManifest+Validate (mirror gate): %v\n---\n%s", id, err, parseReady)
			}
			if m.Transport != config.TransportStdioBridge {
				t.Fatalf("row %q drafted manifest transport = %q, want %q", id, m.Transport, config.TransportStdioBridge)
			}
			// The secret ref must survive into the drafted manifest's env so the
			// readiness gate (HasSecretRef) surfaces the per-key install prompt.
			if !secrets.HasSecretRef(m.Env) {
				t.Fatalf("row %q drafted manifest lost the env secret: ref — the readiness credential prompt would never fire: env=%#v", id, m.Env)
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
// ansys + kicad carry their patterns in file_globs[]; matlab carries none
// (binaries-only); codex carries binaries-only. No Tier-1 row uses files[] (none
// needs a literal path), and no file_globs[] entry is missing its absolute-path shape.
func TestV2Tier1Rows_GlobsLiveInFileGlobsNotFiles(t *testing.T) {
	byID := v2CatalogByID(t)
	// Rows whose probe carries a glob pattern → must be in file_globs[].
	wantGlobRows := map[string]bool{"excel": true, "ableton": true, "ansys": true, "kicad": true}
	for id, e := range byID {
		if e.InstallProbe == nil {
			continue
		}
		p := e.InstallProbe
		// No Tier-1 row should use files[] (no literal-path probe in this batch).
		if id == "excel" || id == "ableton" || id == "codex-mcp-server" || id == "matlab" || id == "ansys" || id == "kicad" {
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

// TestV2AbletonRow_GitPinnedProvenanceMatchesInstalledArtifact is the codex-bot P1
// closure ("ableton must install from an installable source"). The ableton row MUST
// install from mcphub's build-fixed fork applicate2628/ableton-mcp-extended at an
// immutable git SHA. Upstream uisato/ableton-mcp-extended's pyproject mis-declared
// the optional `AbletonMCP_UDP` package at the wrong path, so the setuptools source
// build failed and `uvx --from git+...uisato...` could not install; our fork drops
// that optional package (not imported by MCP_Server/server.py) so the install builds.
// The row must NOT install from the PyPI distribution `ableton-mcp-extended`, which is
// a DIFFERENT codebase (IMNMV's branch) whose project URLs point at
// github.com/IMNMV/ableton-mcp and which depends on the nonexistent `audio2llm`
// package (install fails). This test pins the provenance-consistency invariant the
// bot flagged: the vetted source (vendored_source.repo + readme_url), the executed
// command (args git+ URL + vendored_source.install_cmd/run_cmd), and the SHA pin all
// agree, so the code users run is exactly the repo/readme/license being presented.
func TestV2AbletonRow_GitPinnedProvenanceMatchesInstalledArtifact(t *testing.T) {
	e := v2CatalogByID(t)["ableton"]
	if e == nil {
		t.Fatalf("v2 catalog missing the ableton row")
	}

	const wantRepo = "https://github.com/applicate2628/ableton-mcp-extended"

	vs := e.VendoredSource
	if vs == nil {
		t.Fatalf("ableton row lost its vendored_source")
	}
	if vs.Repo != wantRepo {
		t.Fatalf("ableton vendored_source.repo = %q, want %q (must be mcphub's build-fixed fork, not upstream uisato whose pyproject fails to build, and not the hijacked PyPI source)", vs.Repo, wantRepo)
	}
	// The pin must be a 40-hex git SHA (immutable), not the prior bare PyPI version
	// "2.2.0" that resolved to IMNMV's broken/audio2llm-missing package.
	sha := strings.TrimSpace(vs.PinnedRef)
	if !gitSHA40.MatchString(sha) {
		t.Fatalf("ableton vendored_source.pinned_ref = %q, want a 40-hex git SHA (a bare PyPI version like 2.2.0 is a provenance/install hazard)", sha)
	}
	// Defense-in-depth: the same gate the catalog/manifest validators use must agree
	// the SHA is immutable (not a moving branch).
	if config.IsMovingGitRef(vs.PinnedRef) {
		t.Fatalf("ableton pinned_ref %q is a moving ref (must be an immutable SHA)", vs.PinnedRef)
	}

	// The EXECUTED artifact must come from that exact git source + SHA. The PyPI name
	// `ableton-mcp-extended==<ver>` in --from would silently pull IMNMV's package, so
	// the args MUST carry the git+<repo>@<sha> form pointing at our build-fixed fork.
	wantGitFrom := "git+" + wantRepo + "@" + sha
	joinedArgs := strings.Join(e.Args, " ")
	if !strings.Contains(joinedArgs, wantGitFrom) {
		t.Fatalf("ableton args %v do not install from the pinned git source %q (a PyPI-name --from would pull the wrong IMNMV codebase)", e.Args, wantGitFrom)
	}
	// Guard against the prior PyPI-name pin lingering anywhere in the executed args.
	if strings.Contains(joinedArgs, "ableton-mcp-extended==") {
		t.Fatalf("ableton args %v still carry a PyPI-version --from pin (==<ver>); must be the git+<repo>@<sha> form", e.Args)
	}
	// vendored_source.install_cmd / run_cmd must reflect the same git+SHA source so the
	// advertised reproduction command matches what `command`+`args` actually run.
	for label, cmd := range map[string]string{"install_cmd": vs.InstallCmd, "run_cmd": vs.RunCmd} {
		if !strings.Contains(cmd, wantGitFrom) {
			t.Fatalf("ableton vendored_source.%s = %q does not reference the pinned git source %q", label, cmd, wantGitFrom)
		}
	}
}

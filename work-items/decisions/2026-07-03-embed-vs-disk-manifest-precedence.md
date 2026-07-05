# Embed-vs-disk manifest precedence for shipped server names

- **status:** accepted
- **date:** 2026-07-03
- **author:** $architect (opus) recommendation, accepted by main-conversation gate
- **closes deferral in:** `work-items/bugs/2026-06-28-embed-first-install-shadows-disk-manifest.md`

## Decision

**Option B — embed always wins; make the collision LOUD at the WRITE gate.**

For a server name present in the binary's embed set:

1. **Read precedence UNCHANGED:** `loadManifestYAMLEmbedFirst` stays embed-first. All 17 read consumers (install, uninstall, scan, status_enrich, secrets_scan, migrate, lsp, catalog, readiness, register, …) keep byte-identical behavior.
2. **New single-owner membership predicate:** `isEmbeddedManifestName(name) bool` in `internal/api/manifest_source.go`, mirroring the read helper's embed branch EXACTLY including test-override symmetry (returns false when `MCPHUB_MANIFEST_DIR_OVERRIDE` is set — embed FS bypassed). `CatalogManifestGet`'s inline membership loop dedups onto it.
3. **Write-gate refusal:** `ManifestCreateIn` hard-refuses creating a disk manifest under an embedded name BEFORE the disk-stat guard, with a message naming the collision + suggesting a rename (`%q-custom`). One refusal covers every create surface via the `ManifestCreate` funnel (GUI AddServer + marketplace one-click).
4. **Warn surface for pre-existing collisions:** install/scan emit a warn ("disk manifest for shipped server %q is ignored…"). NEVER auto-delete the user's file. `ManifestEditIn` is NOT refused (would trap the user with an uneditable file) — warn only.
5. **GUI mirrors:** `readiness.go` dry-run reflects the refusal (calls the gate, does not re-derive); `marketplace_install.go` handles a colliding catalog name gracefully (route to Install of the embedded manifest or friendly 400 — not an opaque 500).

## Why not A (disk overrides embed) / C (opt-in override)

- Embed-as-source-of-truth is LOAD-BEARING for secret-safety: `CatalogManifestGet` (manifest.go:333) is deliberately embed-only so "a disk manifest with literal secrets is structurally unreachable" for embedded names. A would reverse that posture and break npm upgrade safety (stale disk file silently shadows a shipped fix). C adds a permanent override surface with the same risks, opt-in, for a workflow that has NEVER worked (re-creating a shipped server on disk was already silently ignored — B makes an already-broken path fail honestly).
- B has the smallest blast radius: zero read-path change; one predicate + one refusal + additive warnings.

## Change-surface contract (binding on the implementation)

- **Touch:** `manifest.go` (ManifestCreateIn refusal; CatalogManifestGet dedup — behavior-preserving), `manifest_source.go` (predicate), `install.go` + `scan.go readManifestNames` (warn), `gui/readiness.go` (mirror), `gui/marketplace_install.go` (graceful path).
- **Protected:** `loadManifestYAMLEmbedFirst` read branches, `embeddedManifestNames`, all 17 read consumers, persisted state, wire contracts; no auto-deletion of user files.

## Test list (from the architect memo — implementer executes all 8)

1. Create refused for embedded name (error names collision, no file written).
2. Create accepted for non-embedded name (dev flow intact).
3. Test-override symmetry (`MCPHUB_MANIFEST_DIR_OVERRIDE` set → predicate false → create allowed).
4. `CatalogManifestGet` byte-identical after dedup.
5. Install with pre-existing colliding disk file → embed installs + warn emitted, file untouched.
6. GUI readiness dry-run reports the refusal.
7. Marketplace one-click with colliding name → graceful, not 500.
8. Regression: embed bytes still win the read for an embedded name with a disk file present.

---
severity: low
context: adjacent-finding
closed: 2026-06-28
---

- **status:** fixed
- **fixed-by:** PR #450 (`784f9475`) - marketplace registry URL single-owned in `internal/api`.
- **HEAD reconciliation (2026-07-09):** Verified against master `63b6a008`; see `triage-2026-07-09.md` for code/test evidence.

## Resolution (2026-06-28)

Fixed by moving the canonical literal to `internal/api`
(`api.DefaultMarketplaceRegistryURL`, in `marketplace_catalog.go` next to the
schema-version consts it owns). `internal/cli` and `internal/gui` BOTH already
import `internal/api` for `LoadMarketplaceCatalog`, so each now re-exports from
it (`cli.DefaultMarketplaceRegistryURL = api.DefaultMarketplaceRegistryURL`;
`gui.defaultMarketplaceRegistryURL = api.DefaultMarketplaceRegistryURL`). This is
cleaner than the suggested new `internal/marketplaceurl` leaf — no new package,
the shared lower layer is the single owner, and the GUI still never imports the
CLI. `grep` confirms the URL literal now appears exactly once. One bump point;
the drift footgun is gone. (The suggested-fix note below is superseded.)

# Default marketplace-registry URL is duplicated across cli + gui (must bump together)

## Finding (P3 — adjacent finding, NOT fixed in the Tier-1 catalog PR)

The default marketplace-registry URL is a string constant duplicated in two places:

- `internal/cli/marketplace.go` — `const DefaultMarketplaceRegistryURL`
- `internal/gui/marketplace.go` — `const defaultMarketplaceRegistryURL`

They are deliberately NOT shared because the GUI layer must not import the CLI command
package (a layering boundary — see the comment on the GUI const). But that means any change
to the canonical catalog URL (e.g. the v1 → v2 bump done in the Tier-1 catalog PR) MUST edit
BOTH consts by hand; missing one leaves the GUI and CLI pointed at different catalogs — a
silent drift the build does not catch.

This was surfaced while repointing both consts v1 → v2 for the Tier-1 desktop-app catalog
rows. Both were bumped correctly in that PR (grep confirms zero `/v1/catalog.json` left in
`internal/cli` + `internal/gui`), and a NOTE comment was added on the GUI const naming this
bug. The DUPLICATION ITSELF is the adjacent finding.

## Why not fixed here

Per the backend-engineer adjacent-findings protocol, this is outside the approved change
surface (the Tier-1 catalog rows + the v2 repoint). De-duplicating the const requires a new
shared neutral-leaf package both `internal/cli` and `internal/gui` can import without
violating the layering boundary — an architecture decision for the orchestrator/architect,
not an implementer judgment call inside this PR.

## Suggested fix (for whoever picks this up)

Introduce a tiny neutral leaf package (e.g. `internal/marketplaceurl`) that both `internal/cli`
and `internal/gui` import, holding the single `DefaultRegistryURL` const. That removes the
"bump both by hand" footgun without creating an upward cli ← gui dependency. Add a test that
asserts the two call sites resolve to the same value if the const is kept duplicated for now.

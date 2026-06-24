---
status: open
severity: low
context: adjacent-finding
---

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

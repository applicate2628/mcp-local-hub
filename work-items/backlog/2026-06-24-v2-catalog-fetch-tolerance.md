---
status: open
severity: medium
context: adjacent-finding
defer: true
---

# Make `ParseMarketplaceCatalog` tolerate unknown fields on the FETCH/cache path (strict at authoring/CI)

## Summary (codex PR #430 finding 1 — the CLASS fix)

`ParseMarketplaceCatalog` (`internal/api/marketplace_catalog.go:154`) decodes the
fetched catalog with `DisallowUnknownFields` on the CLIENT fetch path. That makes
an OLDER released binary reject the WHOLE catalog the moment a NEW additive v2
field appears in `catalog.json` — even though the binary only needs the fields it
already understands. The bot raised this against the new `required_secrets` field
added in PR #430, but it is NOT specific to that field: every additive v2 field
(`vendored_source`, `availability`, `install_probe` from #424, and now
`required_secrets`) shares the exact same fragility, because they all funnel
through the one strict decoder.

## Why PR #430 does NOT change the decoder (architect verdict — VERIFIED)

The concern is THEORETICAL for currently-deployed clients:

- Every deployed npm release (`v0.4.6` / `v0.4.7` / `v0.4.8`) defaults to the v1
  registry URL, and **v1 is FROZEN** (`marketplace/v1/catalog.json`,
  `schema_version: "1"`, zero additive keys). A v1-default binary never fetches
  the v2 catalog, so it never sees `required_secrets` (or any v2 field).
- The v1 → v2 default flip (`#426` / commit `d6ad7b3d`) is in **no release tag**,
  so no deployed binary fetches v2 yet.
- `required_secrets` is gated to schema_version 2 via the SAME
  `newCatalogFieldKeys` forward-compat mechanism (`marketplace_catalog.go:188`) as
  the already-merged #424 fields, so a frozen v1 catalog can never carry it, and
  the per-key v1-gate (`newCatalogFieldsRequireV2`) keeps a v1 client from ever
  being offered the key.

So `required_secrets` is correctly gated as-is; the decode is left UNCHANGED in
PR #430. This work-item tracks the general fetch-tolerance class fix, which
**becomes relevant the moment a v2-default binary is shipped in a release tag**.

## The class fix (path A — for whoever picks this up)

In `ParseMarketplaceCatalog`, drop `DisallowUnknownFields` on the FETCH/cache
decode so a forward-compatible client ignores fields it does not know, while
KEEPING:

- the trailing-byte / single-document rejection (`marketplace_catalog.go:161-167`)
  — a garbage tail is still a hard parse error;
- the `schema_version` validation;
- the duplicate-id rejection;
- the `newCatalogFieldKeys` v1-gate (`newCatalogFieldsRequireV2` /
  `catalogEntryNewKeyPresence`) — a v1 catalog carrying a v2 key still fails, so
  the freeze contract holds.

Strictness (reject genuinely-unknown/misspelled keys) moves to the
**authoring/CI** path instead — a separate strict-decode validator run in CI
against `marketplace/v2/catalog.json` so a typo'd field is caught at authoring,
not silently dropped on every client. It is ONE function (the fetch decoder) plus
a CI validator.

This affects #424's three fields (`vendored_source`, `availability`,
`install_probe`) EQUALLY — the fragility was introduced by the strict-fetch
decoder, NOT by `required_secrets`, which merely follows the established additive
pattern.

## Why it's deferred / out of scope for PR #430

- No deployed client breaks today (all releases default to v1; the v2 flip is in
  no tag — verified).
- Loosening the fetch decoder is a **trust-boundary change**: the catalog body is
  an untrusted remote input, so relaxing field-strictness on it must be reviewed
  for what an unknown-but-now-ignored field could mask (e.g. a silently-dropped
  security-relevant gate field). **`$security-reviewer` is MANDATORY** on the
  separate PR that lands path A.

## Related

- `work-items/bugs/2026-06-24-marketplace-url-duplication.md` — same default-URL
  /catalog-version surface (the duplicated `DefaultMarketplaceRegistryURL` const).
- `work-items/backlog/2026-06-24-stdio-env-secret-blocking-gate.md` — the
  precursor that PR #430 implements (`required_secrets` install gate).

## Disposition

DEFER until a v2-default binary is scheduled for a release tag. Then implement
path A (fetch-tolerant decode + CI-strict authoring validator) as its own
security-reviewed PR.

---
status: proposed
date: 2026-06-24
---

# Required-secret install gate (`required_secrets` opt-in blocking finding)

## Context

mcphub's default secret posture is **optional**: an unset `secret:<key>` env ref
is OMITTED at daemon spawn (`secrets.ResolveMapBestEffort`,
`internal/secrets/resolver.go:101`), so a server whose secret is not set still
spawns — with that env var unset — and reports its own "missing API key" instead
of mcphub failing the spawn cryptically. This is load-bearing for rows like
`paper-search-mcp` (Unpaywall email) and `wolfram` (app id): the server is
usable without the secret, just degraded.

Some servers, however, **hard-exit on startup** when a required credential is
unset. The `suno` row (`uvx --from mcp-suno==2026.6.18.0 mcp-suno`, env
`ACEDATACLOUD_API_TOKEN: secret:acedata_api_token`) exits 1 immediately when the
token is missing. Under the optional posture a one-click install with no token
would write the manifest + supervisor-intent + client configs, then the daemon
would **crash-loop** — a confusing failure that looks like a daemon bug, not a
missing-credential problem. The symptom ("daemon keeps restarting") misdirects
diagnosis away from the real cause (no token set).

## Decision

Add an **opt-in** `required_secrets []string` field (a list of vault KEYS) to
both `config.ServerManifest` and the catalog `MarketplaceEntry`. A key listed
there is a **REQUIRED install gate**: when it is not resolvable in the vault, the
install is BLOCKED instead of spawning a crash-looping daemon.

The block is enforced as a new **blocking (non-optional) AdmissionCheck finding**
(`required-secret`), a SIBLING of the existing per-finding admission checks
(`internal/api/admission_check.go`). A blocking finding makes
`containsNonOptional(admission)` true → `Preflight` returns an `AdmissionError`
→ Install aborts **before any manifest / supervisor-intent / client-config
write**. The finding's `Reason` names the KEY ONLY, never a value (redaction
posture, matching the readiness secret rows).

The readiness per-key secret classification (`internal/api/readiness.go`) is
made to derive its `Optional` flag from the **same** `requiredSecretSet(m)` owner
the admission finding consults — a key in the set renders RED/blocking, an
unmarked secret stays the default yellow/advisory. There is exactly ONE predicate
for "is this vault key a required install gate", so readiness and admission can
never drift (architecture law: one owner per cross-cutting invariant). The
existing admission↔readiness parity guard
(`TestAdmissionCheckCorpusPreflightReadinessParity`) continues to hold.

The catalog projection carries the field into the persisted manifest
(`generateCommandDraft` → `projectVendoredAndAvailability`, stdio path only — a
required local-env secret is a local-stdio concern), routed through the same
`rejectUnsafeMarketplaceDraftStringSlice` owner every untrusted catalog string
slice uses. A catalog-authoring guard
(`validateCatalogVendoredAndAvailability`) asserts each named key actually
appears as a `secret:<key>` value in the entry's `Env`, so a typo key cannot
silently un-gate the row. The field is gated to catalog `schema_version 2`
(`newCatalogFieldKeys`) like the other additive D-2/D-3 metadata, so a frozen v1
catalog can never carry it.

## What stays unchanged (protected surfaces)

- **The resolver** (`secrets.ResolveMapBestEffort`) keeps its omit-on-missing
  behavior. Blocking happens at the **install gate**, never by making the
  spawn-time resolver fail-fast — the "secrets optional; server reports its own
  missing-key" contract that `paper-search`/`wolfram` rely on is intact.
- The default per-key readiness `Optional=true` posture is unchanged for
  **unmarked** secrets.
- `paper-search-mcp` and `wolfram` do NOT set `required_secrets`, so they stay
  advisory. The gate is **opt-in only**.
- The v1 catalog is frozen; the new field is v2-only.

## Future direction

Existing advisory-secret rows (`paper-search-mcp`, `wolfram`) MAY opt into
`required_secrets` later if their servers are found to hard-fail without the
credential — that is a per-row catalog edit, not a behavior change to the gate
or the resolver. The gate is additive and reversible: removing the field from a
row returns it to the default optional posture.

## Consequences

- A `suno`-style row with no token set fails LOUD at install with an actionable
  "set `<key>`" fix, instead of crash-looping a daemon.
- The blast radius is one new manifest field + one new admission finding + one
  readiness classification tweak + one catalog field/guard/projection. No
  resolver, spawn-path, or default-posture change.

---
status: candidate
severity: medium
context: adjacent-finding
defer: true
---

# Make a catalog row able to mark an stdio `env: secret:` ref as REQUIRED (blocking install)

## Summary

Today every stdio `env: secret:` reference is treated as **advisory** at the
readiness layer: an unset secret is reported with `Optional: true`, the server
still installs and spawns (the env var is simply omitted), and any hard
requirement is left to the daemon to enforce at its own startup. For a server
that genuinely cannot function without its credential (e.g. `suno`'s
`ACEDATACLOUD_API_TOKEN = secret:acedata_api_token`), the only enforcement is
the daemon's own `sys.exit(1)` on the missing token — so the install "succeeds"
and the operator discovers the gap only when the daemon crash-loops.

The enhancement: let a catalog row declare that a specific stdio `env: secret:`
ref is **REQUIRED**, so the install/readiness gate **hard-blocks** until the
secret is set, instead of advisory-prompt + daemon-crash-loop. This makes the
"I cannot run without this key" contract visible at the gate rather than only at
spawn time.

## This is a CLASS issue, not a per-row tweak

The advisory classification is uniform across every secret-using stdio row, so
this cannot be fixed by editing a single catalog row — there is no per-row knob
to flip. The same advisory pattern is already merged for at least:

- `paper-search-mcp` — `PAPER_SEARCH_MCP_UNPAYWALL_EMAIL: secret:unpaywall_email`
  (`marketplace/v2/catalog.json`, the paper-search row).
- `wolfram` — secret-keyed, same advisory shape.
- `suno` (this PR, #429) — `ACEDATACLOUD_API_TOKEN: secret:acedata_api_token`;
  the local `mcp-suno` daemon hard-exits (`sys.exit(1)`) without the token.

`suno` was kept on the advisory pattern deliberately so it stays **consistent
with the existing merged `paper-search-mcp` row**; making it a hard block on
just that one row would diverge the catalog from its own precedent and would not
actually change the readiness behavior (the classification is owner-side, not
row-side).

## The readiness owner (single point of change)

The `Optional=true` classification for stdio `env: secret:` refs lives in one
place:

- `internal/api/readiness.go:885-908` — the per-key loop that emits a
  `ReadinessRequirement{Name: "secret: " + key, Optional: true}` for every
  `secret:`-prefixed env value. The comment there records the deliberate
  decision ("Secrets are OPTIONAL: an unset key is advisory (Optional=true), NOT
  a blocker — the server still installs + spawns ... and reports its own
  missing-key if it actually needs it").

A REQUIRED-secret feature would add a row-declared signal (e.g. a
`required_secrets: ["acedata_api_token"]` array on the catalog entry, threaded
through the manifest the install builds) that this loop consults to set
`Optional: false` (blocking) for the named keys, while every undeclared
`secret:` ref keeps today's advisory default. Because the classification is
centralized here, this is the single owner to extend — no consumer-side
conditionals scattered across rows.

## Why it's deferred (out of scope for PR #429)

Changing the default — or adding the per-row REQUIRED signal — is a
**readiness-layer CLASS change**: it affects `paper-search`, `wolfram`, `suno`,
and every future secret-using stdio row, plus the catalog schema (a new
row-level field), the manifest that carries it, the GUI install/readiness UI
(how a blocking-vs-advisory secret renders and gates the Install button), and
the existing advisory-pattern tests. That is design + schema + UI + test scope,
not a single music-catalog row, so PR #429 keeps `suno` on the proven advisory
pattern and the daemon's `sys.exit(1)` remains the enforcement.

## Disposition

DEFER. Implement as a dedicated readiness/catalog-schema work-item: design the
row-level REQUIRED-secret signal, thread it from catalog → manifest →
`internal/api/readiness.go:885-908`, render the blocking state in the GUI
install gate, and update the advisory-pattern tests. When it lands, revisit the
`suno` / `paper-search` / `wolfram` rows to opt the genuinely-required keys into
the blocking classification.

---
status: open
severity: low
context: adjacent-finding
---

# Catalog validator should warn when a `kind:global` draft's args carry `${workspaceFolder}`

## Finding (adjacent finding — filed, NOT fixed in PR #426)

`${workspaceFolder}` is a GENERATE-time token: `mcphub marketplace generate <id>`
substitutes it with the operator's CWD at draft time and freezes the result into
the drafted manifest's `args`. That is correct for a workspace-scoped row (the
workspace IS the substitution context). But for a `kind:global` daemon — which
has no per-workspace identity — the frozen path is just whatever directory the
operator ran `generate` from, which is rarely the intended stable install
location. The spawn-time, per-workspace token is the DIFFERENT `${workspace.path}`
(workspace-scoped only); it does not apply to a global daemon. So a `kind:global`
row whose `args` carry `${workspaceFolder}` silently bakes a CWD-dependent path
that neither resolves at spawn nor stays stable across `generate` invocations.

This surfaced while disqualifying the mathcad Tier-1 row (its drafted
`args: ["${workspaceFolder}/mathcad-mcp/standalone.py"]` for a `kind:global`
daemon) — see `work-items/backlog/2026-06-24-mathcad-mcp-row-deferred.md`.

## Proposed fix (a future work-item, NOT this PR)

The catalog/manifest validator should emit a WARNING (advisory, not a hard reject
— `${workspaceFolder}` may be intentional in some odd global setups) when a
`kind:global` (or otherwise non-workspace-scoped) draft's `args` / `command`
contain the literal `${workspaceFolder}` token, naming the generate-time-freeze
footgun and pointing the operator at an absolute path or a vendored-clone target
dir instead. Likely owner: the `GenerateDraftManifest` warning channel (it already
warns on `${workspaceFolder}/..` parent-escape and sensitive env names) and/or
`config.ServerManifest.Validate`.

## Why NOT fixed here

PR #426 is a catalog-DATA PR plus the additive D-3 glob-probe enhancement. Adding
a new validator-warning rule is a separate behavioral change to a shared validator
(its own scope, its own tests, its own reviewer pass) and is out of the approved
change surface. Filed per the adjacent-findings protocol for the orchestrator to
prioritize.

## Closure (2026-06-30)

Fixed in fix/open-bug-batch: GenerateDraftManifest now warns when a kind:global draft's args/env carry ${workspaceFolder} (freeze-to-CWD footgun). build/vet/tests green.

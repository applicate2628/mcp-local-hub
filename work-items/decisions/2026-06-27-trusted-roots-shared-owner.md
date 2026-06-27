---
status: accepted
date: 2026-06-27
owners: architect (area-5 design) + architecture-reviewer (recommended on #437) + lead
epic: work-items/epics/2026-06-19-install-and-it-works-ux.md (area 5)
sibling decision: work-items/decisions/2026-06-27-area5-trusted-folders-design.md
---

# Trusted-roots store is a single SHARED owner (keep-not-rename); serena reuses the LSP store

The trusted-folders trust boundary is owned by ONE shared store
(`lsp-trusted-roots.json`) consumed by BOTH the LSP router and the serena router.
The store, its file shape, the canonical predicate, the containment helper, and
the bless/remove owners are all REUSED UNCHANGED — no store rename, no migration
shim. The serena trust gate reads through a thin server-NEUTRAL alias
(`WorkspaceRootTrusted`) over the existing LSP predicate so the call site reads
"is this workspace root trusted?" rather than naming the LSP store.

## Why keep-not-rename (rejected: rename to a server-neutral store)

A rename to a "neutral" basename (e.g. `trusted-roots.json`) would orphan every
operator's existing `lsp-trusted-roots.json` store on upgrade and require a
migration shim. The cosmetic benefit (a neutral filename) does NOT justify that
churn + the migration-correctness risk. The store has been the LSP trust store
since it shipped; it is simply being PROMOTED to the shared trust owner for both
routers. A single neutral PREDICATE alias delivers the only thing serena actually
needs (a server-neutral read) at zero migration cost.

## Why one shared store (rejected: a second serena-only store)

A parallel serena-only trust store would be a private copy of the same trust
stack — two owners for one cross-cutting invariant ("which workspace roots may
auto-register a daemon"). That is the parallel-silo anti-pattern: the two stores
would drift, and an operator blessing a root for one router would silently leave
it untrusted for the other. The trust boundary is ONE invariant → ONE owner.

## Decision

- **Store: KEEP `lsp-trusted-roots.json` + `LSPTrustedRootsFile` UNCHANGED.**
- **Neutral predicate alias** `WorkspaceRootTrusted(root)` == `LSPWorkspaceRootTrusted(root)`
  so serena's call site reads server-neutral and the store stays the single owner.
- **Serena gate placement:** inside `AutoRegisterSerenaWorkspace`, after the root
  is resolved + languages read and BEFORE any side effect, via
  `serenaTrustedRootCheckFn` → `WorkspaceRootTrusted`. FAIL-CLOSED (nil seam /
  error / false → refuse) with sentinel `ErrSerenaRootNotTrusted`.
- **Co-design (bless ships with the gate):** serena auto-bless on
  `mcphub workspace register --backend serena` via `serenaRegisterBlessTrustedRootFn`
  → `BlessDefaultTrustedRoot` (best-effort), so the out-of-box serena auto-introduce
  flow does not regress. Bless is reachable ONLY from explicit ops (register / trust /
  setup / GUI), NEVER the router auto-register path (a self-blessing router would
  re-open the original vulnerability).

## Evidence (shipped #437)

- Neutral alias: `WorkspaceRootTrusted(workspaceRoot)` in
  `internal/api/lsp_trusted_roots.go:312` (the `// KEEP-NOT-RENAME (area-5 decision
  2026-06-27)` doc comment at `lsp_trusted_roots.go:301`).
- Canonical predicate REUSED: `LSPWorkspaceRootTrusted` in
  `internal/api/lsp_trusted_roots.go:288`.
- Store UNCHANGED: `LSPTrustedRootsFileLeaf = "lsp-trusted-roots.json"` in
  `internal/api/lsp_trusted_roots.go:72`; shape `LSPTrustedRootsFile` at
  `internal/api/lsp_trusted_roots.go:85`.
- Serena gate: `serenaTrustedRootCheckFn` → `WorkspaceRootTrusted` in
  `internal/api/serena_auto_register.go:149`; sentinel `ErrSerenaRootNotTrusted`.

## Related

- Sibling design: [area-5 trusted-folders design](2026-06-27-area5-trusted-folders-design.md)
  (its `## Gap-c` block names this decision as the keep-not-rename sibling).

## Terms and Abbreviations

- LSP: Language Server Protocol.
- GUI: Graphical user interface.
- MCP: Model Context Protocol.
- JSON: JavaScript Object Notation.

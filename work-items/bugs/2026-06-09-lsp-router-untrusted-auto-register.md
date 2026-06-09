# GUI LSP router auto-registers a daemon for an attacker-chosen local path from untrusted MCP tool args

- **Status:** FIXED on branch `security/lsp-trusted-root-gate` (trusted-root
  containment gate). Supersedes PR #269 (branch
  `codex/fix-lsp-router-vulnerability`), which removed auto-register entirely
  and was rejected by the operator for killing the out-of-the-box convenience.
- **Date:** 2026-06-09
- **Severity:** P2 — resource exhaustion + arbitrary-local-path process spawn
  driven by untrusted MCP tool-call input. Localhost-scoped (the router only
  binds 127.0.0.1 and only spawns a supervised LSP proxy rooted at a local
  directory), but a real abuse surface: a malicious or compromised MCP client
  can make the hub stand up supervised daemons for directories the operator
  never named.
- **Found by:** security review of the PR #266 LSP auto-registration surface.
- **Related:** PR #266 (LSP auto-registration through the hub — the feature this
  hardens), PR #269 (the rejected blunt fix this supersedes), the serena
  auto-register DoS bound in `internal/api/serena_auto_register.go`
  (`ErrNotASerenaProject`) which this mirrors at the authorization layer.

## Summary

`internal/gui/lsp_router.go`'s `workspaceFromResolvedLSPPath` auto-registered a
brand-new supervised LSP workspace daemon for ANY local directory named by an
UNTRUSTED MCP `tools/call` `path` argument, as long as the resolved workspace
root carried a language project marker (`go.mod`, `package.json`,
`pyproject.toml`, `Cargo.toml`, …). The marker check was treated as the gate.
Project markers are extremely common in arbitrary directories, so the marker is
NOT an authorization boundary — an attacker who can reach the router (a
malicious MCP client, or a prompt-injected agent issuing a tool call) could pick
any local path under a marker-bearing tree and the hub would spawn a supervised
daemon rooted there.

## The fix — trusted-root containment gate

A first-touch auto-register now proceeds ONLY when the resolved workspace root
is equal to, or a true subdirectory of, a **trusted root**. Otherwise the router
refuses with the same error PR #269 introduced:

```
LSP workspace for <path> is not registered; run mcphub register for this
workspace before using the LSP router
```

A **trusted root** is the union of:

1. **Operator-configured allowed roots** — paths the operator hand-adds to the
   store file (pre-trust a broad tree).
2. **Auto-blessed roots** — the canonical `WorkspaceRoot` of every workspace
   registered through an EXPLICIT operator action (`mcphub register` CLI, GUI
   "Enable" / `/api/lsp/register`). "Bless on first explicit register."

Net effect: the FIRST workspace under any tree must be registered explicitly
(that seeds trust for the tree); afterward sibling/child workspaces under that
blessed root auto-register transparently through the router, preserving the
PR #266 convenience. An attacker-named arbitrary path with no trusted-root
ancestor is refused.

The project-marker check is retired as an authorization boundary (per PR #269's
reasoning, markers are only discovery hints). The trusted-root gate is the
authorization boundary now. The already-registered fast-path
(`resolved.Registered == true`) is unchanged — a workspace already in the
registry is not subject to the gate.

## Operator surface — `lsp-trusted-roots.json`

```text
<state-dir>/lsp-trusted-roots.json
{ "version": 1, "roots": ["<canonical-abs-path>", ...] }
```

- Owner-only state-dir file, written through the hardened state-file pipeline
  (`WriteStateFileBytesAtomic` → `SecureWriteClientConfig` chain, atomic
  temp+rename, per-file flock, owner-only DACL/mode). Reads tolerate an absent
  file (= no trusted roots) and apply the same parent-directory DACL read gate
  the supervisor-intent reader applies (a parent that grants write/delete to a
  non-allowlisted principal is a swap risk that could inject a trusted root, so
  it is refused on read unless `MCPHUB_ALLOW_UNHARDENED_STATE_READ=1`).
- **Operator-editable.** To pre-trust a broad tree (so every workspace under it
  auto-registers without an explicit first register), add its canonical absolute
  path to `roots` and save. Roots are canonicalized (abs + EvalSymlinks + clean,
  Windows drive-letter lowercased) and deduped on the next bless.
- A missing file means every first-touch auto-register is refused until the
  first explicit register (or a hand-added root). This is the secure default.

## Where it lives (file:line, branch `security/lsp-trusted-root-gate`)

- **Store + containment + bless:** `internal/api/lsp_trusted_roots.go`
  - `LoadLSPTrustedRoots` / `LoadDefaultLSPTrustedRoots` — tolerate-absent loader
    with the parent-DACL read gate.
  - `LSPWorkspaceRootTrusted(workspaceRoot)` (package func) — production read
    seam the router wires; loads the live store and reports containment. Fails
    CLOSED on a load error.
  - `(*LSPTrustedRootsFile).LSPWorkspaceRootTrusted` — the containment check.
  - `rootContains(trustedRoot, candidate)` — separator-aware ancestor test
    (`==` OR `candidate` has `trustedRoot + PathSeparator` prefix), case-fold on
    Windows, case-sensitive elsewhere. NEVER a bare string prefix, so `/dev`
    does not match `/developer` and `C:\proj` does not match `C:\project2`.
  - `BlessTrustedRoot` / `BlessDefaultTrustedRoot` — idempotent, canonicalizing,
    flock-guarded append. Called ONLY at explicit register sites.
- **The gate (router):** `internal/gui/lsp_router.go`
  `workspaceFromResolvedLSPPath` (first-touch branch) →
  `lspWorkspaceRootIsTrusted` (router-side adapter; fail-closed on nil gate /
  empty root / gate error). `TrustedRootCheckFn` deps seam wired to
  `api.LSPWorkspaceRootTrusted` in `SetLSPRouterProduction`. This is the READ
  path only — the router NEVER blesses.
- **Bless sites (EXPLICIT register only):**
  - `internal/api/register.go` `registerWithManifest` (end of success path) via
    the `registerBlessTrustedRootFn` seam → `BlessDefaultTrustedRoot`. Covers
    `mcphub register` (CLI) and `legacy_migrate`.
  - `internal/gui/lsp_register.go` `realLSPRegistrar.RegisterLSP` via the
    `blessLSPTrustedRootForGUI` seam → `BlessDefaultTrustedRoot`. Covers the GUI
    "Enable" handler / `/api/lsp/register`.
  - The router's `AutoRegisterFn` (production = `api.EnsureLSPRegistered`) does
    NOT bless. `EnsureLSPRegistered` itself was deliberately left unchanged so
    blessing on the router path is structurally impossible — re-opening the hole
    would require adding a bless call to a shared function, which a reviewer
    would catch.

## Tests

- `internal/api/lsp_trusted_roots_test.go` — absent-file-empty, bless→trust
  (exact + subdir), prefix-but-not-subdir refused, unrelated refused, idempotent
  bless, operator-hand-added root trusted, empty/nil fail-closed, Windows
  case-fold (case-sensitive elsewhere), stored-canonical.
- `internal/gui/lsp_router_test.go` — untrusted refused + AutoRegisterFn NOT
  called; trusted root auto-registers + forwards; gate-error fails closed; nil
  gate fails closed; **router auto-register path does NOT bless** (drives a
  trusted first-touch through the real handler under a redirected state dir and
  asserts `lsp-trusted-roots.json` is never created); git-only untrusted refused.
- `internal/gui/workspaces_test.go` — explicit GUI register blesses the
  canonical root exactly once; no bless when every language fails.

## Residual / out of scope

- A PRE-EXISTING, unrelated test failure exists on this branch AND on master:
  `internal/api/legacy_migrate_test.go:249`
  `TestMigrateLegacy_PreservesInPlaceReplacedEntry` ("freshly-migrated entry was
  deleted by post-register cleanup"). Verified independent of this change by
  reverting `internal/api/register.go` + `register_test.go` to HEAD and
  reproducing; the touched code path is
  `cleanupDirectLanguageServerEntriesAfterRegister`, which this work does not
  modify. Tracked separately, not fixed here (out of the trusted-root scope).
- The serena auto-register path (`internal/api/serena_auto_register.go`) is NOT
  changed: it already has its own DoS bound (`ErrNotASerenaProject` requires a
  `.serena/project.yml` marker the attacker would have to plant). Only the LSP
  router was vulnerable to the common-project-marker abuse.
- PR #269 should be closed AFTER this branch merges (the lead does that, not the
  implementer).

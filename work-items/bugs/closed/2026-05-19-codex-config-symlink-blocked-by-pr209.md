# Codex client column universally disabled when `~/.codex/config.toml` is a symlink

**Severity:** P2 (operator UX gap on dotfile-management setups; documented
by-design tradeoff in scan.go, but matrix surface is misleading and there
is no operator-facing escape hatch)

**Found:** 2026-05-19 by claude during PR #215 post-merge smoke test for
deferred user request: "снятие галочек на gemini в servers и установку
галочек в codex". Codex column rendered all checkboxes disabled with
tooltip "codex-cli's MCP config file could not be read (stat error).
Check file permissions and disk health, then refresh." — diagnostic
text suggests a transient I/O error, but root cause is a stable
symlink-refusal contract.

## Root cause

`internal/api/scan.go:105-114` (`probeClientConfigPresence`):

```go
if lst, lerr := os.Lstat(p.path); lerr == nil {
    isSymlink := lst.Mode()&os.ModeSymlink != 0
    if !lst.Mode().IsRegular() && !isSymlink {
        out[p.name] = "error"
        continue
    }
    if isSymlink {
        out[p.name] = "error"
        continue
    }
}
```

ANY symlink at a client config path is classified `routing = "config-error"`
regardless of strict / default-relax env mode. The reasoning is documented
inline at scan.go:92-104:

> post-PR #209: the secure-write pipeline now refuses pre-existing symlinks
> in ALL modes — `resolveSymlinkForSecureWrite` was removed from
> `secureWriteWithOperatorOpt`. Reporting "ok" while writes deterministically
> fail with symlink-refuse errors is the exact UX trap bot codex-r7 flagged:
> user sees a green matrix column, clicks Apply, every write fails.
> Dotfile-symlink setups that used to rely on default-mode resolve-to-target
> are now unsupported by design — the security boundary closure in PR #209
> traded that path for confused-deputy integrity protection.

The frontend tooltip at `internal/gui/frontend/src/screens/Servers.tsx:728-734`
renders this as a "stat error" message, which misleads the operator into
inspecting file permissions / disk health rather than understanding that
their dotfile setup is incompatible.

## Reproduction

1. `ln -s /some/real/path/.codex/config.toml ~/.codex/config.toml`
2. `mcphub gui` → Servers screen
3. Observe: codex-cli column entirely disabled with "stat error" tooltip

The error is **not** a stat error — `os.Stat` would follow the symlink
and succeed if the target file exists. The refusal is `os.Lstat`-based
+ explicit symlink-reject branch.

## Affected operators

- Users with dotfile-managed `~/.codex/`, `~/.gemini/`, etc. (common
  on developer workstations using `chezmoi`, `yadm`, `stow`, GNU stow,
  manual `ln -s` patterns).
- Users with corp-policy that redirects `%USERPROFILE%` subdirs via
  junctions (Windows reparse points are caught by the same Lstat branch).

## Suggested fixes (P3 follow-up; not blocking)

1. **Frontend message accuracy** (smallest, safest): change the tooltip
   in `Servers.tsx:734` to distinguish "symlink refused by security
   policy" from "stat I/O error". Example:

   > "Codex-cli's MCP config file is a symlink. The secure-write contract
   > refuses symlinked client configs to prevent confused-deputy attacks
   > (PR #209). Replace the symlink with a real file, or edit the target
   > path's config directly without going through the hub."

   Requires the backend to distinguish the two error categories in the
   scan response (split `"error"` into `"error-symlink"` and
   `"error-stat"`).

2. **Operator escape hatch** (deeper, requires security review): allow
   a single-user-host opt-in that resolves symlinks at scan + write
   time for client config paths only (NOT state files; state files
   stay strict). Gated by an explicit env var like
   `MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK=1` so the operator knows what
   tradeoff they're accepting. Would require re-introducing the
   `resolveSymlinkForSecureWrite` path PR #209 removed, with the env
   var as the gate.

3. **Documentation**: surface the dotfile-symlink limitation in
   `docs/INSTALL.md` or a new `docs/CLIENT-CONFIG-PATHS.md`. Currently
   the limitation lives only in scan.go and PR #209 review history.

## What is NOT a fix

- "Disable the symlink refusal" — PR #209 closed a confused-deputy
  attack surface and the refusal is load-bearing for that. Restoring
  unconditional symlink resolution is a security regression.
- "Tell users to delete their dotfile setup" — common dotfile patterns
  are widely used and the GUI surface should accommodate them with a
  clear opt-in.

## Workaround for today

Operators can either:
- Replace `~/.codex/config.toml` symlink with a real file (loses
  dotfile-repo source-of-truth).
- Edit `/path/to/real/config.toml` (the symlink target) directly. The
  hub's writes via Apply won't help, but manual edits at the target are
  unaffected.

## Status

**CLOSED — fixed in PR #217** (commit `9e89abe`, "fix(api): unblock
Servers matrix on dotfile-symlinked clients + write-broadened state-dir
parent" — "Fix 1", filed and fixed the same day) and **refined in
PR #258** (commit `44512e2`, "fix(gui): distinct 'error-symlink'
status — Servers matrix stops mislabeling symlink-refusal as 'stat
error'"). Both of this doc's Suggested fixes — #1 (frontend message
accuracy) and #2 (operator escape hatch) — are implemented.

What landed:

- **Operator escape hatch (Suggested fix #2):** the
  `MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK` opt-in const
  (`internal/api/client_write_init.go:180`) plus
  `OperatorAllowsClientConfigSymlink()`
  (`internal/api/client_write_init.go:415`) gate the re-introduced
  `resolveSymlinkForSecureWrite` path (`client_write_init.go:356`, used
  under the opt-in at `:280-281`). Strict mode
  (`MCPHUB_REQUIRE_SINGLE_USER_HOME=1`) overrides the opt-in and refuses
  symlinks unconditionally, so corp-managed hosts keep the PR #209
  confused-deputy hardening. The "What is NOT a fix" constraints above
  are respected — the refusal stays default-on; only an explicit
  operator opt-in resolves it.
- **Frontend message accuracy (Suggested fix #1):** PR #258 split the
  generic scan `"error"` into a distinct `"error-symlink"` category.
  `internal/api/scan.go:161` emits `"error-symlink"` for a refused
  symlink (and `:153-157` returns `"ok"` when the operator opt-in is
  set and the target resolves to a regular file), so the matrix renders
  a symlink-specific diagnostic/tooltip instead of the misleading "stat
  error" wording.

Verified 2026-06-03 (`9e89abe` and `44512e2` are both ancestors of
HEAD; `AllowClientConfigSymlinkEnv`, `OperatorAllowsClientConfigSymlink`,
the opt-in-gated `resolveSymlinkForSecureWrite`, and the
`"error-symlink"` scan category are all present at HEAD). The doc's
original "stat error" symptom no longer reproduces with the opt-in set.

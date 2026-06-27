---
status: accepted
date: 2026-06-21
accepted: 2026-06-28 — code merged + verified (#409/#410 named PRs + hardening #414/#415/#416); the AF-1 TOCTOU bug is resolved.
driver: A3 symlink-client-config — close the live AF-1 TOCTOU in the MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK opt-in lane (work-items/bugs/2026-06-21-symlink-optin-toctou-string-rewalk.md) and add a per-write scoped-consent seam so PR-2 can build GUI/CLI consent UX on a hardened foundation.
supersedes: nothing (extends the PR #258 MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK opt-in; does not remove it)
related: work-items/bugs/2026-06-21-symlink-optin-toctou-string-rewalk.md (AF-1, PR-1 is its fix); work-items/bugs/closed/2026-05-19-codex-config-symlink-blocked-by-pr209.md (the dotfile-symlink UX gap the env opt-in addressed); PR #209 (confused-deputy refusal); PR #258 (env opt-in + error-symlink scan category)
scope: PR-1 = security core (handle-pin + consent plumbing, no UX). PR-2 = GUI/CLI consent UX (out of scope here).
---

# Decision: symlink client-config writes — handle-pinned write + scoped consent

## Problem

The shipping `MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK=1` opt-in lane resolved a
symlink to a path STRING and let `SecureWriteClientConfig` re-walk it
(`filepath.Split` + re-open the parent BY PATH). That re-open is a TOCTOU: a
swap of the symlink (or an intermediate component) between resolve and re-open
redirects the privileged, token-bearing write to an attacker-chosen target
(AF-1). The lane needs to be safe AND to grow a per-write consent seam so PR-2
can ask the operator, per write, "this symlink resolves to <target>, follow
it?" — without re-opening the TOCTOU.

## Decision

### 1. Handle-pin requirement (the AF-1 closure)

When a symlink is followed, the write MUST resolve-to-HANDLE then
write-through-the-held-handle. The resolved target's PARENT directory is opened
by a component-by-component O_NOFOLLOW descent from the resolved target's VOLUME
ROOT (`openDirHandleNoReparse` on Windows / `unix.Open(O_DIRECTORY|O_NOFOLLOW)`
on POSIX for the volume-root anchor, then the shared `openExistingRealDirAt`
step per component), and the shared hardened owner
(`secureWriteClientConfigToResolvedParent`) runs against the final held
handle/fd. No `filepath.Split` of a resolved string and no path-based open of
the whole parent string after resolve.

> **F1 correction (PR-B, 2026-06-21).** The original wording here said the
> parent was opened "**exactly ONCE** … frozen at open … a post-open
> symlink/parent swap cannot redirect the write because there is no second
> path-walk to race." That was OVER-CLAIMING for an INTERMEDIATE-component swap:
> a single path-based open of the whole parent string (PR-A's shape) applies
> O_NOFOLLOW only to the FINAL component — the kernel / object manager re-walks
> every intermediate, so an intermediate dir swapped to a symlink between
> resolve and that open still redirected the write. PR-B (F1) replaces the
> single open with the component-by-component descent above (volume-root anchor;
> NO path-containment refusal — the resolved target is out-of-home by design;
> the only property is "no intermediate component is followed through a swap").
> Regression guard: `TestF1_IntermediateComponentSwap_Refused` (POSIX,
> deterministic via the `resolvedParentDescendStepHook` injection seam) +
> `TestF1_DecomposeResolvedParentWindows`.

The hardened post-parent-open sequence is a SINGLE OWNER
(`secureWriteClientConfigToResolvedParent`) reused by both the path-based
`secureWriteClientConfigImpl` and the resolve-to-handle write-through
(`secureWriteThroughResolvedParentHandle`) — so the two write entry points
cannot drift on the gate / symlink-refusal / DACL / rename / re-verify steps.

### 2. Scoped consent SUPPLEMENTS, does not replace, the env-var opt-in

`MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK=1` stays a valid, sufficient input to the
follow-symlink predicate (operators who set it keep working, unchanged). A new
`ResolvedSymlinkConsent` is a SECOND, per-write input to the SAME predicate.
Either alone is sufficient to follow a symlink. The two are inputs to ONE
predicate, not two parallel code paths — this is the no-parallel-silo rule:
the follow happens in one place (`secureWriteFollowingSymlink`) regardless of
which input authorized it.

A scoped consent carries `PinnedResolvedPath` — the resolved PARENT the
operator was shown and approved at confirm time. At write time the symlink is
RE-RESOLVED and the freshly-resolved parent MUST equal the pin, else the write
is refused. This is the swap-between-confirm-and-write guard: a co-resident who
repoints the symlink after the operator consented but before the write lands
cannot redirect the write, because the pin no longer matches.

### 3. Strict-override invariant (PROTECTED — unchanged)

Strict mode (`MCPHUB_REQUIRE_SINGLE_USER_HOME=1`, or the persisted
supervisor-intent strict_mode bit) overrides BOTH the env-var opt-in AND any
scoped consent and refuses symlink-follow unconditionally. Corp-managed /
multi-tenant hosts get the PR #209 confused-deputy hardening regardless of any
per-operator env var or per-write consent. The consent path explicitly
re-checks `operatorRequiresSingleUserHome()` so a scoped consent can never
bypass strict mode.

### 4. Audit

Each follow-symlink relax lane emits a DISTINCT warn audit event so the
security-boundary downgrade is visible to log monitoring:
- env-var lane: `client-write-symlink-resolved-via-optin` (pre-existing).
- scoped-consent lane: `client-write-symlink-resolved-via-scoped-consent` (new).

The broadened-parent relax retry still emits `client-write-unhardened-fallback`.

### 5. Client-config-only

A `ResolvedSymlinkConsent` is NEVER constructed for a state_dir /
supervisor-intent write. The state-file pipeline has its own strict-mode
resolution and does not follow symlinks. Symlink consent is client-config-only.

## Disclosed residual (non-strict namespace-rights limitation)

The handle-pin closes the resolve→re-open TOCTOU. It does NOT close a
co-resident principal who holds `FILE_DELETE_CHILD` (Windows) / write on the
resolved-target PARENT directory: such a principal can delete or replace the
directory entry itself, which sits outside the written file's own security
boundary. This is the SAME documented best-effort limitation that the existing
relax lanes carry (see CLAUDE.md "Hardened client-config writes"), and its
robust mitigation is unchanged: `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` (strict
mode), which refuses symlink-follow entirely and enforces the parent-dir gate.
The non-strict symlink-follow lane is scoped to solo-developer hosts who manage
their own dotfile symlinks and trust the parent directory's ACL.

## EXDEV non-issue

A legitimate concern for symlink-to-another-volume targets is that an atomic
rename across volumes (EXDEV) would fail. It does not arise here: the temp file
is created RELATIVE TO the resolved target's parent handle/fd, so the temp is a
SIBLING of the resolved destination and the rename is always intra-directory
(same volume). `TestA3_T4` exercises a symlink whose target lives in a separate
directory tree and confirms the write succeeds on the resolved location.

## Alternatives considered

- **Return the open handle/fd from `resolveSymlinkForSecureWrite` up into the
  build-neutral `client_write_init.go`.** Rejected: the handle/fd type is
  platform-specific (`windows.Handle` vs `int`) and cannot appear in a
  build-neutral signature. The chosen seam keeps the platform handle types
  confined to the `_windows.go` / `_posix.go` write-through functions and
  returns only the build-neutral resolved-parent PATH for the pin-match.
- **Keep resolve-to-string but re-stat the symlink right before the re-open.**
  Rejected: a re-stat narrows the window but does not close it — the re-open is
  still a fresh path-walk that can be raced after the re-stat. Only pinning the
  parent by handle removes the second path-walk.

## Status

**ACCEPTED (2026-06-28).** Both PRs merged and verified. PR-1 implemented
decisions 1-5 and the disclosed residual (the security core: handle-pin +
consent plumbing), verified on the POSIX leg (WSL, go1.26.4) with
`TestA3_T1..T6` + the scoped-consent pin-match control passing (7/7) and the
pre-existing F2 symlink tests still green; merged as #409 with hardening
follow-ups #414/#415/#416. PR-2 (the GUI/CLI consent UX surfacing
`PinnedResolvedPath` to the operator at confirm time) shipped as #410. The AF-1
TOCTOU bug this decision closed
(`bugs/2026-06-21-symlink-optin-toctou-string-rewalk.md`) is resolved. The
remaining accept-disclose relax is a documented namespace-rights posture (see
`## Disclosed residual` above), mitigated by
`MCPHUB_REQUIRE_SINGLE_USER_HOME=1` — not pending code.

---
title: MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK opt-in lane has a TOCTOU via resolve-to-string re-walk (AF-1)
severity: high
found-by: architect (A3 symlink-client-config research) — security-sensitive
found-in-phase: A3 PR-1 design (symlink-client-config scoped consent)
affected-surface: internal/api/client_write_init.go (secureWriteWithOperatorOpt symlink relax lane ~:335-347 + resolveSymlinkForSecureWrite ~:411-424); internal/api/secure_write_windows.go (filepath.Split + open-parent-by-path ~:127,141); internal/api/secure_write_posix.go (~:52,62)
context: design-finding
resolved: 2026-06-21
---

- **status:** fixed
- **fixed-by:** PR #409 (`9af679e8`) plus PR #415 (`0ccd37c6`) - handle-pinned write and intermediate-component walk.
- **HEAD reconciliation (2026-07-09):** Verified against master `63b6a008`; see `triage-2026-07-09.md` for code/test evidence.

# AF-1 — symlink opt-in lane TOCTOU: resolve returns a STRING, the write re-walks it

## Severity

**High.** This is a confused-deputy integrity defect in a SHIPPING code path
(the `MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK=1` opt-in lane, live since PR #258).
It re-opens the exact PR #209 confused-deputy surface the opt-in was meant to
re-introduce safely. A successful exploit redirects a privileged,
token-bearing client-config write to an attacker-chosen target.

## Root cause

The opt-in symlink relax lane resolves a symlink to its target and then hands
the **resolved path STRING** to `SecureWriteClientConfig`, which re-walks it:

1. `secureWriteWithOperatorOpt` (`client_write_init.go` ~:335-347): when
   `OperatorAllowsClientConfigSymlink()` is true and the destination is a
   symlink, it calls `resolveSymlinkForSecureWrite(path)` and reassigns
   `path = resolved` (a STRING), then calls `SecureWriteClientConfig(path, ...)`.
2. `resolveSymlinkForSecureWrite` (~:411-424) returns a resolved-target path
   STRING — it does NOT pin the target by handle.
3. `secureWriteClientConfigImpl` (`secure_write_windows.go` ~:127,141 /
   `secure_write_posix.go` ~:52,62) then `filepath.Split`s that resolved
   string and re-opens the parent **BY PATH** (`openDirHandleNoReparse` /
   `unix.Open`).

Between step 1's resolve and step 3's re-open there is a TOCTOU window. A
co-resident principal (or any actor who can write the operator's home /
dotfile tree) who SWAPS the symlink — or an intermediate path component —
during that window redirects the privileged write to a different, possibly
attacker-chosen, target. The resolve proved "target X is safe"; the write
landed on "target X' is whatever it is now".

The opt-in's own doc comment (`AllowClientConfigSymlinkEnv`,
`client_write_init.go` ~:200-235) already acknowledges a related plant-then-
enable risk but did NOT cover the resolve→re-open swap on EACH write.

## Why "I only resolved the symlink, the write is hardened" is not enough

The per-file restrictive DACL/mode IS installed correctly on the file the
pipeline creates — but it creates it under the parent it re-opened BY PATH at
write time, which may not be the parent the resolve verified. The hardening
protects the *contents* of whatever file gets written; it does not protect
*which* file gets written. The integrity boundary (which target receives the
operator's token-bearing config) is exactly what the re-walk breaks.

## Reproduction (threat model, deterministic via injection seam)

A natural race is timing-dependent; the regression test engineers the window
deterministically per the repo's race-window-assertion discipline:

1. Operator opts in (`MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK=1`), or (PR-1's new
   path) supplies a scoped consent pinned to parent P.
2. The write resolves the symlink → parent P.
3. An after-resolve hook repoints the symlink to a different parent P'
   (modeling the co-resident swap).
4. PRE-FIX: the write re-walks the resolved string and lands on P' — the swap
   target is written. POST-FIX: the pinned parent handle (and, for scoped
   consent, the pin-match) refuse the write.

See `TestA3_T3_ScopedConsentPinMismatchAfterSwap_Refused` and
`TestA3_T5_EnvVarLaneUsesHandlePinnedPath_NoStringReWalk` in
`internal/api/client_write_init_a3_toctou_test.go`.

## Fix (this PR — A3 PR-1)

Resolve-to-HANDLE, then write-through-the-held-handle — no second path-based
open after resolve:

- **SEAM-A (shared owner):** the hardened post-parent-open sequence (parent-
  dir gate, symlink refusal, born-with-restrictive-DACL temp create relative
  to the held handle, atomic handle-relative rename, post-rename re-verify) is
  extracted into `secureWriteClientConfigToResolvedParent(parentHandle/fd,
  base, contents, skipParentGate)` in both
  `secure_write_{windows,posix}.go`. The path-based
  `secureWriteClientConfigImpl` opens the parent and delegates to it — ONE
  owner of those steps.
- **Resolve-to-handle write-through:** `secureWriteThroughResolvedParentHandle`
  (`client_write_resolve_{windows,posix}.go`) descends to the resolved
  target's PARENT **component-by-component** from the resolved target's VOLUME
  ROOT (POSIX `/`; Windows drive root `C:\` or UNC share root
  `\\server\share`), opening one real component at a time
  O_NOFOLLOW-relative-to-the-previously-held-fd/handle via the shared
  `openExistingRealDirAt` step, then runs the shared owner against the final
  held parent handle/fd. No `filepath.Split` of a resolved string, no
  path-based open of the whole parent string.

  > **F1 correction (this PR-B, 2026-06-21).** The PR-A shape opened the
  > resolved target's parent BY PATH **exactly once** (`openDirHandleNoReparse`
  > / `unix.Open(parentDir, O_NOFOLLOW)`). O_NOFOLLOW on a single open of the
  > full parent string protects ONLY the FINAL component; the kernel / object
  > manager re-walks every INTERMEDIATE component at open time, so an
  > intermediate dir swapped to a symlink between resolve and that single open
  > still redirected the privileged write. The earlier wording in this doc and
  > in the resolver comments ("a co-resident who swaps … an intermediate
  > component … cannot redirect the privileged write") was OVER-CLAIMING:
  > PR-A's single path-based open did NOT close the intermediate-component
  > swap. PR-B (F1) closes it via the component-by-component descent above —
  > the trust anchor is the resolved target's VOLUME ROOT (NOT $HOME; the
  > resolved target is out-of-home by design), and the only property delivered
  > is "no intermediate component is followed through a swap" (no
  > path-containment refusal). Regression guard:
  > `TestF1_IntermediateComponentSwap_Refused` (POSIX) +
  > `TestF1_DecomposeResolvedParentWindows` (Windows decomposition).
- **F2 migration:** the `MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK` env-var lane now
  ALSO goes resolve-to-handle → SEAM-A. This is the high-value part — it
  closes the shipping AF-1 TOCTOU.
- **SEAM-B (scoped consent):** a `ResolvedSymlinkConsent` pins the resolved
  FULL TARGET the operator approved; at write time the re-resolved full target
  must match the pin or the write is refused (the swap-between-confirm-and-write
  guard).

## What is PRESERVED (not weakened by this fix)

- The DEFAULT symlink refusal (`refusePreexistingReparsePoint` /
  `refusePreexistingSymlink`) stays default-on.
- Strict mode (`MCPHUB_REQUIRE_SINGLE_USER_HOME=1`) overrides BOTH the env-var
  opt-in and any scoped consent and refuses symlink-follow unconditionally.
- The `SecureWriteClientConfig(path)` public signature and its "any error is a
  HARD FAIL" caller contract.
- The state-file write/read pipeline + its strict-mode resolution — symlink
  consent is CLIENT-CONFIG-ONLY and never references state_dir /
  supervisor-intent.
- The parent-dir DACL gate logic — reused, not weakened.

## Status

**FULLY RESOLVED across two PRs.**

- **PR-A (A3 PR-1)** closed the symlink-itself swap and the resolve→re-walk for
  the FINAL component: resolve-to-handle write-through + the SEAM-B full-target
  pin. Verified on the POSIX leg: `TestA3_T1..T7` + the scoped-consent pin-match
  positive control all PASS, with the pre-existing
  `TestSecureWriteWithOperatorOpt_Symlink*` F2 tests still passing.
- **PR-B (F1)** closed the remaining INTERMEDIATE-component swap: PR-A's single
  path-based open of the resolved parent string re-walked every intermediate
  component (O_NOFOLLOW guards only the final component), so an intermediate dir
  swapped to a symlink between resolve and that open still redirected the write.
  PR-B replaces the single open with a component-by-component O_NOFOLLOW descent
  from the resolved target's VOLUME ROOT. Verified on the POSIX leg (WSL,
  go1.26.4): `TestF1_IntermediateComponentSwap_Refused` +
  `TestF1_OutOfHomeSymlink_Succeeds` PASS; the per-component swap is
  falsification-proven load-bearing (the same swap against the pre-F1
  single-open shape writes the attacker file). Windows host passes
  `TestF1_DecomposeResolvedParentWindows` (drive-root + UNC decomposition) and
  the G17 `TestSecureCreateClientConfigParentDir_Windows*` suite stays green
  (the shared `openExistingRealDirAt` extraction did not regress parent-create).

Windows host skips the elevation-gated symlink tests and passes the
path-based, resolve-shape, and decomposition tests. The residual
namespace-rights limitation
(a co-resident with `FILE_DELETE_CHILD` on the resolved parent) is the
documented best-effort limitation covered by `MCPHUB_REQUIRE_SINGLE_USER_HOME=1`
— see `work-items/decisions/2026-06-21-symlink-client-config-scoped-consent.md`.

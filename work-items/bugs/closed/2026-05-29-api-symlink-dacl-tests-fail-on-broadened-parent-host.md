---
title: Two internal/api secure-write tests fail on a dev host whose %LOCALAPPDATA% parent DACL is broadened / lacks symlink-create privilege
severity: low
found-by: backend-engineer
found-in-phase: serena Phase 3 (client-reconcile to /serena/mcp)
affected-surface: internal/api/client_write_init_test.go, internal/api/state_file_helper_test.go
context: adjacent-finding
status: closed
related-pr: PR #264 (914d0cf)
---

## Status

CLOSED — fixed by PR #264 (`914d0cf`, merged 2026-06-03), the same merge whose
commit body explicitly names this bug's two failing tests. The host condition
(broadened `%LOCALAPPDATA%` parent DACL) let the operator's ambient
`AllowClientConfigSymlinkEnv` / `AllowUnhardenedStateWriteEnv` leak into the
tests, so the symlink-refusal and write-capable-parent-refusal paths went
unexercised and the assertions saw `nil`. PR #264 clears both envs per-test via
`t.Setenv`: `TestSecureWriteWithOperatorOpt_DefaultRefusesPreexistingSymlink`
gets `t.Setenv(AllowClientConfigSymlinkEnv, "")` in `client_write_init_test.go`,
and `TestWriteStateFileAtomic_StrictModeWithWriteCapableParent` gets
`t.Setenv(AllowUnhardenedStateWriteEnv, "")` in `state_file_helper_test.go`, so
both exercise the refusal paths regardless of the operator shell. Verified
2026-06-03 against the merged #264 diff. Surfaced for closure by codex bot
on #265 r1 (the same #264 "env" fix as the sibling
`2026-05-20-tests-leak-state-into-production-logs`).

## Reproduction

1. `go test -count=1 -timeout 5m ./internal/api/` from this dev tree (Windows 11,
   non-elevated shell, `%LOCALAPPDATA%\mcp-local-hub` parent DACL broadened to
   non-allowlisted SIDs — the same host condition as
   [2026-05-19-state-file-verify-rejects-write-broadened-parent-dacl.md](2026-05-19-state-file-verify-rejects-write-broadened-parent-dacl.md)).
2. Observe two FAILs:
   - `TestSecureWriteWithOperatorOpt_DefaultRefusesPreexistingSymlink`
     (`client_write_init_test.go:284`: "expected refusal for pre-existing
     symlink under default mode; got nil")
   - `TestWriteStateFileAtomic_StrictModeWithWriteCapableParent`
     (`state_file_helper_test.go:379`: "default-relax must still refuse
     write-capable parent (TOCTOU swap risk); got nil")

## Proof it is pre-existing (NOT caused by Phase 3)

- Added a clean detached worktree at HEAD `cc3a343` (`git worktree add -d`)
  with ZERO Phase-3 changes present and ran the same two `-run` filters →
  BOTH fail identically. So the failures exist on the base commit.
- Phase 3 touches only `internal/api/serena_client_reconcile.go` (new),
  `internal/api/serena_client_reconcile_test.go` (new),
  `internal/clients/clients.go` (additive `MCPEntry.RelayURL` field), and
  `internal/clients/antigravity.go` (relay `--url` form). None of these touch
  `SecureWriteClientConfig`, `WriteStateFileAtomic`, the symlink-refusal path,
  or the parent-DACL gate the two failing tests exercise.
- `git status --short` shows neither failing test file nor its production file
  is in the Phase-3 diff.

## Likely root cause (environment, not code)

> **SUPERSEDED HYPOTHESIS (2026-06-03).** This was the original guess at filing.
> The VERIFIED root cause (see `## Status`) is different: the operator's ambient
> `AllowClientConfigSymlinkEnv` / `AllowUnhardenedStateWriteEnv` (the relax /
> symlink opt-in a broadened-host operator sets) leaked into the tests and made
> the writer take the ALLOW path, so the refusal assertions never fired — NOT a
> precondition-synthesis problem. PR #264 fixed it by clearing those envs
> per-test. Kept for history; do NOT drive privilege/DACL work from this section.

Both tests assert a *refusal* that depends on host-specific Windows state:
- the symlink test needs symlink-create privilege to synthesize the
  pre-existing symlink it expects the writer to refuse — a non-elevated shell
  without `SeCreateSymbolicLinkPrivilege` cannot create it, so the "refusal"
  never has a symlink to refuse;
- the write-capable-parent test synthesizes a parent DACL it expects the
  default-relax lane to still refuse, but on a host whose real parent ACL
  posture already diverges (broadened `%LOCALAPPDATA%`), the synthesized
  precondition does not reproduce the refusal branch.

This is the test-side mirror of the operator-facing DACL gate concern already
tracked in closed/2026-05-19-state-file-verify-rejects-write-broadened-parent-dacl.md.

## Risk

Local dev only on hosts matching this DACL/privilege posture. The
secure-write production code is unaffected by Phase 3. CI runs on a clean
`windows-latest` runner where symlink privilege + a clean parent DACL are
present, so these tests pass there (they are existing merge-gate tests).

## Suggested fix (not blocking Phase 3)

- Gate the symlink-refusal test on symlink-create capability
  (`t.Skip` when a probe symlink-create fails), mirroring how other
  symlink tests in the tree guard the privilege.
- Make the write-capable-parent test synthesize its parent in a
  `hardenedTempDir`-style owner-only dir so the host's real `%LOCALAPPDATA%`
  ACL posture cannot leak into the assertion.

## Severity rationale

Low: pre-existing, host-environment-specific, no production impact, and
out of scope for Phase 3 (client-reconcile), which introduced zero new
failures (its own 6 tests pass; the full-package delta is zero).

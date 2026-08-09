# State-file verify rejects WRITE-broadened parent DACL even on solo-dev host

**Severity:** P2 (blocks demigrate Apply on common corp-policy /
third-party-installer parent ACLs; operator has to manually tighten
`%LOCALAPPDATA%\mcp-local-hub` DACL or lose hub-managed UX)

**Found:** 2026-05-19 by claude during PR #215 post-merge smoke test.
User authorized testing gemini-uncheck on the `memory` row in Servers.
Apply failed twice (with and without `MCPHUB_ALLOW_UNHARDENED_STATE_WRITE=1`
env var set in the launching shell).

## Error surfaced in GUI matrix

```text
memory/demigrate/gemini-cli: latest backup
  C:\Users\<you>\.gemini\settings.json.bak-mcp-local-hub-20260516-012732
  and -original sentinel both hold "memory" in hub-managed form,
  AND consulting managed-entries marker failed:
  hub-mcp state verify managed-entries.json:
  parent C:\Users\<you>\AppData\Local\mcp-local-hub
  grants write/delete/DAC-edit access to non-allowlisted SID
  (TOCTOU swap risk during fd-verify → path-read window):
  hub-mcp state file DACL grants read to a SID outside
  {current-user, LocalSystem, BuiltinAdministrators}:
  SID S-1-5-21-3565921023-4157258116-3636336328-1010 grants access
  (mask=0x00010156)
```

The demigrate path has three fallback sources for the pre-hub-managed
form: latest backup, `-original` sentinel, `managed-entries.json` marker.
Sources 1+2 are in hub-managed form (separate concern, see [2026-05-15
demigrate-fallback bug](../2026-05-15-demigrate-fallback-when-no-pre-hub-form.md)),
so the third source is load-bearing. It used to fail on the parent-DACL
gate; that gate now relaxes under default-relax (see Status — fixed in
PR #217).

## Affected parent DACL on user's machine

```text
Wave\CodexSandboxUsers                            Allow Modify, DeleteSubdirectoriesAndFiles, Synchronize
S-1-5-21-1236802581-478970963-2983620705-1857089675 Allow Modify, DeleteSubdirectoriesAndFiles, Synchronize  (orphan AD SID)
NT AUTHORITY\SYSTEM                               Allow FullControl
BUILTIN\Administrators                            Allow FullControl
Wave\Dmitry (current user)                        Allow FullControl
```

The error names a third SID `S-1-5-21-...-1010` which is also in the
ACL (likely a Win11 user-account SID surfaced via a third-party
installer; the inline comment at hub_mcp_state_dacl_windows.go:118-122
mentions exactly this scenario as the motivating case for the
default-relax PR #185).

## Root cause analysis

`internal/api/hub_mcp_state_dacl_windows.go:143-145`:

```go
if wrErr := verifyWindowsDACLFromHandleWriteOrAdmin(parentHandle); wrErr != nil {
    return fmt.Errorf("parent %s grants write/delete/DAC-edit access to non-allowlisted SID (TOCTOU swap risk during fd-verify → path-read window): %w", parentDir, wrErr)
}
```

The decision tree:

1. Strict mode (`MCPHUB_REQUIRE_SINGLE_USER_HOME=1`): any non-allowlisted
   ACE rejects. ✗ rejected.
2. Default-relax mode: re-check with narrower WRITE/DAC-edit mask.
   - If only READ access on parent: log warn + proceed (file's own
     DACL is the safety layer).
   - If WRITE/DAC-edit access on parent: ✗ rejected for TOCTOU safety.
3. `MCPHUB_ALLOW_UNHARDENED_STATE_WRITE=1`: bypasses the write-path
   TOCTOU check at `internal/api/state_file_helper.go:154` (the
   `operatorAllowsUnhardenedStateWrite()` gate around
   `checkStateDirParentWriteSafe`). It does NOT bypass the
   read-verify gate at `internal/api/hub_mcp_state_dacl_windows.go:143-145`
   because the read path has no symmetric env var — codex bot r4 P1
   on PR #192 reasoned that allowing the read relax-lane under
   write-broadening would create a TOCTOU swap during the
   fd-verify → path-read window, and the env var was intentionally
   scoped to the write side only. In this demigrate failure the
   blocking step is the `managed-entries.json` READ (line 143-145),
   so the env var has no effect on the symptom.

The user's DACL grants `Modify` (write + DAC-edit) to non-allowlisted
SIDs, so the WRITE/DAC-edit check at step 2 also fires, and the read
verify rejects.

## TOCTOU-swap concern motivating the rejection

The relax-lane comment at hub_mcp_state_dacl_windows.go:128-145 cites
codex bot r4 P1 on PR #192:

> write/delete/DAC-edit access on the PARENT lets a co-resident principal
> replace the target file's directory entry between the fd-bound verify
> above and the path-based os.ReadFile that readHubMcpStateFile performs
> next. That's a TOCTOU swap — the hub then reads attacker bytes that
> the verify never saw.

This is a real concern on multi-tenant systems. The rejection is
load-bearing for that threat model.

## Affected operators

- Workstations with `Wave\CodexSandboxUsers` ACE on
  `%LOCALAPPDATA%\mcp-local-hub` (third-party Codex sandboxing
  installer adds this principal).
- Domain-joined workstations with orphan AD SIDs in the inherited
  parent DACL from `%USERPROFILE%`.
- Any corp-managed Win11 host where group policy broadens
  `%LOCALAPPDATA%` to additional principals.

## Suggested fixes (P3 follow-up; not blocking)

1. **Inode-anchored re-read** (preferred, deeper): replace
   `readHubMcpStateFile`'s path-based `os.ReadFile` with an
   fd-bound `ReadFile(parentHandle, basename)` using the
   already-opened parent handle. That removes the TOCTOU swap
   window the current rejection is mitigating. Once the swap window
   is closed, the write-broadening rejection at line 143-145 can
   be downgraded to warn + proceed in default-relax mode, matching
   the READ-broadening relax behavior. This is the "fix the
   underlying invariant" path, not a workaround.

2. **New opt-in env var** (workaround): `MCPHUB_TRUST_PARENT_INODE_BIND=1`
   to skip the write/DAC-edit re-check on the operator's affirmation
   that the parent inode binding via `ntOpenRelative` is sufficient
   for their threat model. Tradeoff: operator-controlled security
   exception, not a security regression by default.

3. **Operator-facing error guidance**: the current error tells the
   operator the SID + mask but not what to do. A complementary
   `docs/troubleshooting/parent-dacl-broadened.md` with concrete
   PowerShell `icacls` recipes to tighten the parent would
   convert the rejection from "blocker" to "manual recovery
   path". The error message could link to it.

## Note on env-var visibility

A side-finding during diagnosis: `MCPHUB_ALLOW_UNHARDENED_STATE_WRITE=1`
set via `setx` at User scope does NOT propagate to already-running
Git Bash sessions started before the `setx` call. Operators who
verified the env var via `[Environment]::GetEnvironmentVariable(...,'User')`
in PowerShell may launch mcphub from a stale Git Bash where the env
var is absent. Recommend documenting "open a fresh shell after `setx`"
in install troubleshooting docs.

This env-var-visibility issue was NOT the root cause of the
demigrate failure here. Two distinct facts:

- The env var was missing from the running shell (visibility issue
  above), AND
- Even with the env var present, the demigrate would still fail at
  this exact step because the env var bypasses the WRITE TOCTOU
  check (`state_file_helper.go:154`) but does NOT bypass the READ
  verify gate (`hub_mcp_state_dacl_windows.go:143-145`); the
  managed-entries.json read is the blocking step.
The env-var-visibility note is recorded here for ops-troubleshooting
docs since both issues can co-occur on the same host. To fix THIS
demigrate failure specifically, see the suggested fixes section
above (inode-anchored re-read or new opt-in env var for the read
gate) — env-var visibility hygiene alone won't unblock it.

## Workaround for today

Operators can tighten `%LOCALAPPDATA%\mcp-local-hub` DACL to remove
the write-bearing ACEs (admin shell required):

```powershell
icacls "$env:LOCALAPPDATA\mcp-local-hub" /remove:g "CodexSandboxUsers"
# Remove orphan AD SID if present (use SID literal from `Get-Acl`).
icacls "$env:LOCALAPPDATA\mcp-local-hub" /remove:g *S-1-5-21-...-1857089675
# Verify after:
icacls "$env:LOCALAPPDATA\mcp-local-hub"
```

Apply demigrate / migrate operations should then succeed without
needing any env-var bypass.

## Status

**CLOSED — fixed in PR #217** (commit `9e89abe`, 2026-05-19 06:07:50 +0300,
"unblock Servers matrix on dotfile-symlinked clients + write-broadened
state-dir parent"). The preferred fix (Suggested fix #1, inode-anchored
re-read) shipped that same hour — **61 minutes after this doc was filed.**

What landed: `readHubMcpStateFile` now reads via `readStateFileInodeAnchored`
(`hub_mcp_state.go:205` → new `hub_mcp_state_read_inode_{windows,posix}.go`),
binding the read to the verified parent-handle inode (`ntOpenRelative` /
`openat` + handle-bound `ReadFile` / `unix.Read`) and closing the
fd-verify → path-read TOCTOU window at the kernel level. With the swap
window closed, both the read path and `writeHubMcpStateFile`
(`hub_mcp_state.go:90-163`) safely relax write-broadened parents under
default-relax (strict mode `MCPHUB_REQUIRE_SINGLE_USER_HOME=1` unchanged).
The legacy reject path at `hub_mcp_state_dacl_windows.go:143-145`
(`VerifyHubMcpStateDACL`) now has **zero production callers** — reachable
only from tests. Demigrate Apply on broadened-parent solo-dev hosts is
unblocked.

Verified 2026-06-02 (4 checks: `9e89abe` is an ancestor of HEAD; the new
read-inode files are present; `grep VerifyHubMcpStateDACL(` finds no
non-test caller; the in-code comment at `hub_mcp_state.go:183-195` names
this very doc as the fix target). The "OPEN" status was never reconciled
after the fix landed; `triage-2026-05-28.md` row 15 re-flagged it
"still-relevant-P2" from a pr-review pass that read this stale doc, not the
current source.

**NOT closed by this fix (#217)** — the sibling test bug
[2026-05-29-api-symlink-dacl-tests-fail-on-broadened-parent-host.md](2026-05-29-api-symlink-dacl-tests-fail-on-broadened-parent-host.md)
was a separate defect (since closed by #264, 2026-06-03): its failing tests exercise the WRITE-side refusal +
symlink-create privilege (host-environment-dependent harness defects),
which the read-side inode fix did not touch.

Related: [2026-05-15 demigrate-fallback bug](../2026-05-15-demigrate-fallback-when-no-pre-hub-form.md)
(separate concern that the latest backup + sentinel both held
hub-managed form — that's a different defect class about backup
generation, independent of the DACL gate).

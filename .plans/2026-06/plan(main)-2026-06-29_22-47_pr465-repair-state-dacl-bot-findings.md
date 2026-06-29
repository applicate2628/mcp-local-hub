# Plan: PR #465 repair-state-dacl bot findings

Date: 2026-06-29
Owner: main conversation

## Scope

Fix the three PR #465 bot findings on `feat/repair-state-dacl`: Windows repair
access minimization, POSIX operator guarantee scoping, and CLI relative `--path`
resolution. Keep the repair command operator-initiated and do not add read-path
auto-heal.

## Steps

1. Add failing regressions for a Windows WRITE_DAC-only stale DACL, relative
   `--path workspaces.yaml`, and POSIX limitation help text.
2. Reduce the Windows repair open to the rights required by DACL repair and
   verification.
3. Resolve relative CLI paths under the resolved state directory before
   containment checks.
4. Scope POSIX help, command output, runbook, and bug-record wording to say that
   operators must stop existing writers because `chmod` cannot revoke open file
   descriptors.
5. Run the requested targeted tests, build, vet, cross-OS builds, and
   publication-safety scan.
6. Commit and push `feat/repair-state-dacl`.

## Verification Notes

During the Windows sharing regression check, the runtime showed that a
metadata/security-only open with `ShareAccess=0` does not reject a new
`GENERIC_WRITE` opener. The committed wording therefore does not claim the old
writer-exclusion guarantee after access minimization.

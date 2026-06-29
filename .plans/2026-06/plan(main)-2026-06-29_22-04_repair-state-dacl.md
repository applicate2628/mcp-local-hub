# Plan: repair-state-dacl

Date: 2026-06-29
Owner: main conversation

## Scope

Implement `mcphub repair-state-dacl` as the operator-initiated remediation for
stale broad-DACL state files. Preserve the existing fail-closed read path and do
not add read-path auto-heal.

## Steps

1. Inspect existing state-file DACL/mode helpers, state-dir resolution, and CLI
   command patterns.
2. Add focused tests for current-user repair, sharing-violation refusal,
   foreign-owner refusal, state-dir path rejection, scan/repair command output,
   and POSIX mode repair.
3. Implement the CLI command as a thin layer under `internal/cli`.
4. Implement API repair helpers that reuse the existing handle-relative open,
   DACL setter, DACL verifier, and POSIX owner/mode verifier.
5. Update the bug record to fixed-by-operator-command and note the rejected
   auto-heal rationale.
6. Run targeted tests plus `go build`, `go vet`, cross-OS builds, and the
   publication-safety scan.
7. Commit and push `feat/repair-state-dacl`.

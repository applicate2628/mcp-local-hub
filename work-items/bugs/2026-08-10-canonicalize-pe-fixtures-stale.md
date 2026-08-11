# Bug: Canonicalize tests use binaries rejected by the Windows PE gate

- id: 2026-08-10-canonicalize-pe-fixtures-stale
- context: 2026-08-10-windows-console-opt-in
- status: open
- severity: high
- area: internal/cli/canonicalize_test.go
- found-by: qa-engineer

`go test -count=1 -timeout 60s ./internal/cli -run '^TestCanonicalizeBinaryToTarget$'`
fails all four rows on the candidate with `E_WINDOWS_PE_FORMAT`; immutable
`HEAD` passes. The accepted PE-admission contract is intentional, but the same
Phase C implementer left `FRESH-v2`, `SAME-BYTES`, and `RUNNING` byte strings as
the promotion fixtures. Replace them with admitted GUI PE fixtures without
weakening or bypassing pre-mutation admission.

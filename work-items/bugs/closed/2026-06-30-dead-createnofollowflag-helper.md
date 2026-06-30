---
status: closed
severity: low
context: adjacent-finding
opened: 2026-06-30
closed: 2026-06-30
---

# Dead `createNoFollowFlag()` helper — defined per-OS but never called (C6 stale residue)

## Finding (P4 — adjacent finding, surfaced during a portability review)

`createNoFollowFlag()` is defined under build tags in two files:

- `internal/clients/write_nofollow_posix.go:15` (returns `syscall.O_NOFOLLOW`)
- `internal/clients/write_nofollow_windows.go:20` (returns `0`)

A repo-wide grep finds NO call site for either definition — the symbol is
referenced only by its own definitions and doc comments. It is dead code.

The POSIX doc comment claims it is consumed by "EnsureClientConfigStub's
`O_CREAT|O_EXCL` open", but that is no longer true:

- `EnsureClientConfigStub` (`internal/clients/write.go:193`) delegates to
  `CreateConfigFileIfMissing`.
- The TEST default `fallbackCreateConfigFileIfMissing`
  (`internal/clients/write.go:213`) uses `os.CreateTemp` + `os.Link`, not a
  `createNoFollowFlag()`-flagged open.
- The PRODUCTION POSIX impl `secureCreateClientConfigIfMissingImpl`
  (`internal/api/secure_create_client_config_posix.go:82`) hardcodes
  `unix.O_CREAT | unix.O_EXCL | unix.O_WRONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC`
  directly — it does not call `createNoFollowFlag()`.

So the helper is a leftover from an earlier `EnsureClientConfigStub`
implementation that opened the destination directly with `O_NOFOLLOW`,
superseded by the temp+hardlink / `secure_create_client_config_posix.go`
designs. Per architecture law C6 ("a superseding change leaves only the
correct current state"), the dead helper + its now-inaccurate doc comment
are stale residue.

## Fix

Delete `internal/clients/write_nofollow_posix.go` and
`internal/clients/write_nofollow_windows.go`. Verify `go build ./... &&
go vet ./...` still passes (the symbol has no callers, so this is a pure
removal).

## Not a portability defect

The behavior the helper used to provide (kernel symlink refusal on POSIX) is
still present in `secure_create_client_config_posix.go`. This is a hygiene /
dead-code item, not a functional regression.

## Resolution

**Status:** closed
**Date:** 2026-06-30

Both files deleted whole-file (each contained only the one dead function
plus its build tag, package decl, and doc comment — nothing else to
orphan). Confirmed dead via `staticcheck` U1000 on both build
configurations before deletion:

```
internal\clients\write_nofollow_windows.go:20:6: func createNoFollowFlag is unused (U1000)   (GOOS=windows)
internal\clients\write_nofollow_posix.go:15:6: func createNoFollowFlag is unused (U1000)      (GOOS=linux)
```

Verified clean after deletion: `go build ./...`, `go vet
./internal/clients/`, `GOOS=linux GOARCH=amd64 go build ./...`, `GOOS=darwin
GOARCH=arm64 go build ./...` — all green (the per-OS build-tag split means
both platforms had to be checked independently; both still build with the
files removed). No orphaned imports (`syscall` in the POSIX file was used
solely by the deleted function, and the whole file was removed).

Done as part of the same PR as the broader P3/P4 dead-code cleanup
(11 internal/api refactor-leftover wrappers removed; a 12th candidate,
`rehydrateSystemEntryFromTrustedSource`, was KEPT because it carries a
`//nolint:unused // referenced by future Task 9` forward-marker), worktree
`fix/p3-dead-code-cleanup`.

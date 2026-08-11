# Bug: Native Linux release gate was red and timing coverage was not deterministic

- id: 2026-08-11-native-linux-release-gate-red
- context: 2026-08-11-windows-console-opt-in-r2
- status: fixed
- severity: high
- area: internal/process and internal/api native Linux tests
- found-by: qa-engineer

Original reproduction from equal-length immutable snapshots on Windows Subsystem for Linux 2 (WSL2) Ubuntu with Go 1.26.2 and separate cold build/module caches:

```text
go test -count=1 -timeout 12m ./cmd/mcphub ./internal/process ./internal/gui ./internal/api
go test -count=1 -timeout 5m -p=1 ./internal/process -run '<five platform-enumerated tests>' -v
go test -count=1 -timeout 5m -p=1 ./internal/api -run '<three platform-enumerated tests>' -v
```

Expected: all required native-Linux release tests pass deterministically.

Original result:

- the five `internal/process` tests that failed in the platform run passed in both candidate and immutable `HEAD`, but the original timing window had not yet been engineered;
- `TestWriteHubMcpStateFile_HonorsPersistedStrictModeWhenParentInsecure` failed identically on candidate and `HEAD` and still needs a native-POSIX contract diagnosis;
- `TestMigrateSetsRelayExePathForZed` failed identically because the test seeds `Zed/settings.json` while the POSIX producer resolves `zed/settings.json`;
- `TestDialSupervisorIPCReconcile_RoundtripWithFakeListener` failed identically because generated Unix-socket paths were 111–113 bytes, beyond the Linux `sockaddr_un.sun_path` payload.

## Resolution

The approved native-Linux correction changed exactly nine test files. Independent quality assurance (QA) reproduced every original owner-class failure on an immutable-`HEAD` control or targeted delay mutation, then verified the current bytes with Go 1.26.2 on WSL2 Ubuntu ext4:

- original API symptoms: 3/3 top-level tests plus both reconcile subtests reproduced red, then 3/3 plus 2/2 passed current normal and race checks;
- original process timing symptoms: 5/5 reproduced under an engineered pre-start delay, then 25/25 current exact repetitions and 25/25 race repetitions passed;
- real Linux helper joining remained exercised and passed alongside the virtual-clock fixture test;
- full affected normal, common non-command-line-interface packages, `go vet`, and `go build` passed;
- an unrelated unchanged Serena test's `calls=0` race failure was reproduced deterministically on both current and immutable `HEAD` by preserving its actual pre-readiness deadline-overrun condition; it is not caused by this correction.

Canonical verdict and exact commands, counts, times, and receipts: `work-items/active/2026-08-11-windows-console-opt-in-r2/qa-final-r2.md`. Raw local evidence: `.scratch/windows-console-contract/qa-linux-reverify-20260811-1638/`.

Resolved: 2026-08-11

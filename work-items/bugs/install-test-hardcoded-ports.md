# TestInstallAllInstallsEverything fails on dev workstations binding 9130/9131

**Status:** open
**Context:** adjacent-finding
**Found during:** DM-1/DM-2/DM-3 daemon-mgmt-hygiene PR (2026-04-27)
**Phase:** out-of-scope of current PR

## Summary

`internal/api/install_test.go::TestInstallAllInstallsEverything` constructs
two fake manifests with hardcoded ports 9130 and 9131 (via
`makeFakeManifest`). On any dev workstation that already runs an mcphub
daemon listening on those ports — for example a developer who has
installed an older mcphub.exe that registered tasks for those ports —
preflight rejects both manifests with `port 9131/9130 already in use`
and the test fails.

## Reproduction

```bash
# On a dev box with port 9130 or 9131 currently bound
go test ./internal/api/ -count=1 -run TestInstallAllInstallsEverything
# → FAIL with "port 9131 already in use"
```

Confirmed reproducing on the current `master` (2529c33d) — predates and
is independent of the DM-1/DM-2/DM-3 changes.

## Why this isn't fixed in the daemon-mgmt PR

DM-3b's port-release wait + DM-1/DM-2's status-truthfulness fixes are
narrowly scoped daemon-management hygiene. The hardcoded-port test
fixture is unrelated infrastructure — fixing it here would expand
scope and the test was already broken on master.

## Fix candidates (for future plan)

1. **Use port=0 + listener trick:** call `net.Listen("tcp", "127.0.0.1:0")`,
   capture the OS-assigned port, close the listener, and write that
   port into the fake manifest. Same pattern already used by
   `TestInstallAllFrom_PortConflictFailsThatServer` for its `occupied`
   port.
2. **Add `t.Skip` when the hardcoded ports are in use:** preserves
   coverage on clean CI but skips on dev boxes. Hides flakes; not
   recommended.

Option 1 is the right fix — drop the 9130/9131 literals from
`TestInstallAllInstallsEverything` and use OS-allocated ports just like
the sibling test already does.

## Related

- Test: `internal/api/install_test.go:356`
- Sibling using OS-assigned port: `internal/api/install_test.go:380`
- Helper: `internal/api/install_test.go:416` (`makeFakeManifest`)

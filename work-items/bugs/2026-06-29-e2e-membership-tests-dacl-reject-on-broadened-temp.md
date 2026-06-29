---
title: 3 settings e2e membership tests fail on a broadened-DACL temp drive (workspaces.yaml registry-read DACL rejection)
severity: low
found-by: frontend-engineer
found-in-phase: GUI pixel-perfect P1 fixes (fix/gui-pixel-perfect-p1) — e2e settings/logs regression run
affected-surface: >
  internal/gui/e2e/tests/settings.spec.ts:355 / :372 / :396 (the three
  WeeklyMembershipTable tests that write+read a seeded workspaces.yaml);
  the registry-read DACL gate on api.DefaultRegistryPath consumed by
  GET /api/daemons/weekly-refresh-membership.
context: adjacent-finding
status: open
---

## Symptom

On a host whose OS temp dir is a broadened-DACL volume (here `R:\Temp`, a
RAM disk whose parent grants `S-1-5-11` Authenticated Users), three settings
e2e tests fail:

- `Membership table renders mixed initial state (seed 2 rows ...)` (:355)
- `Toggle one row + Save persists to disk (reload round-trip)` (:372)
- `Select all flips every row to enabled; Clear all flips every row to disabled` (:396)

Failure mode: `expect(locator).toBeChecked()` — element not found, because the
membership table never leaves "Loading workspaces…". The backend stderr names
the real cause:

```
GET /api/daemons/weekly-refresh-membership registry load: read registry
<temp>\AppData\Local\mcp-local-hub\workspaces.yaml: ... not single-user safe:
hub-mcp state file DACL grants read to a SID outside {current-user, LocalSystem,
BuiltinAdministrators}: SID S-1-5-11 grants access ...; default-relax refuses
file WRITE/DAC/DELETE access granted to a non-allowlisted SID ...
```

## Root cause (verified)

The e2e hub fixture seeds `workspaces.yaml` into a temp HOME under the OS temp
dir. The registry-READ path enforces the owner-only DACL allowlist and refuses
a file whose parent granted `S-1-5-11` to the new file. The fixture sets
`MCPHUB_ALLOW_UNHARDENED_STATE_WRITE=1` (covers the WRITE relax lane) but the
membership registry READ still applies the strict allowlist, so the seeded
file is rejected and the API returns an error → the table never loads.

## Exoneration / scope

NOT introduced by the pixel-perfect CSS fix. Verified by running the same three
tests against the pristine base commit (0858a5b2) with a base-built binary: they
fail IDENTICALLY with the same `SID S-1-5-11` DACL rejection. This is a
pre-existing environment interaction (broadened temp-drive DACL + registry-read
allowlist), surfaced only because the dev temp dir is on a RAM disk with
Authenticated-Users on the namespace.

## Possible remediations (for the owner to prioritize)

1. Have the e2e fixture tighten the seeded state-dir DACL to owner-only after
   creating it (icacls /inheritance:r on Windows), mirroring the runbook in
   CLAUDE.md "secret daemons exit 1 on a sandbox-broadened %LOCALAPPDATA%".
2. OR extend the documented relax lane to the registry READ path under the
   `MCPHUB_ALLOW_UNHARDENED_STATE_WRITE` opt-in (review whether read-relax is
   safe — the gate exists to detect a tampered/swapped registry).
3. OR run e2e with a temp dir on a volume whose parent does not grant
   Authenticated Users (e.g. under the user profile rather than a RAM disk).

On CI (windows-latest, default temp under the runner profile) these tests
pass — this only bites a dev whose `%TEMP%` is a broadened-DACL volume.

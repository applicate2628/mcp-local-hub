---
status: fixed (PR #511 TerminatePIDWithIdentity + cmdline revalidate; see TRIAGE-2026-07-08.md)
severity: medium
filed: 2026-07-05
context: adjacent-finding (surfaced during the A2 npx-orphan security-design pass, architect-verified against HEAD)
---

# CleanupOrphans reaps with raw `taskkill /PID <pid> /F` — no identity re-verify (PID-recycle friendly-fire window)

## Finding

`API.CleanupOrphans` (`internal/api/cleanup.go:753`) already kills matched orphan
processes, but the kill is a raw `taskkill /PID <pid> /F` (`cleanup.go:864`) issued
against a PID captured at an EARLIER census/scan. Between the scan and the kill the
OS may recycle that PID onto an unrelated process → mcphub force-kills a bystander.
There is no re-verification of `{ExecutablePath, basename, kernel start-time}` on a
held handle at kill time.

This is shipped code (independent of the A2 npx-orphan work), and it is exactly the
PID-recycle friendly-fire class the supervisor's own reap path already defends
against: `process.TerminatePIDWithIdentity` (`internal/process/pid_identity_windows.go:51`)
re-verifies identity on a held handle and fails closed on ACCESS_DENIED / mismatch,
and the squatter reap (`internal/cli/supervise_squatter.go:390`) kills EXCLUSIVELY
through it. `CleanupOrphans` predates that primitive and never adopted it.

## Failure scenario

`mcphub cleanup --scan-clients --confirm` (or a future scheduled cleanup): the scan
records candidate orphan PID P; before the kill fires, P exits and the OS reuses P
for an unrelated process Q; `taskkill /PID P /F` kills Q. Rare but real on a busy
Windows host with high process churn (exactly the ~360-node.exe condition that
triggers a cleanup in the first place).

## Fix

Capture `{ExecutablePath, StartedAt(=CreationDateUnix)}` at census (already available
from `LookupProcessIdentity` / the snapshot), then kill via
`process.TerminatePIDWithIdentity(proof)` instead of raw `taskkill /F`. A PID recycled
between scan and kill fails the proof and is structurally unkillable — identical to the
squatter reap contract. This is folded into the A2 **P2** reaper-hardening
(`work-items/bugs/2026-07-04-npx-stdio-mcp-orphan-accumulation-bypasses-hub.md` §6.1),
but is filed separately so the shipped-code risk is not lost if A2-P2 slips: it can
land as a small standalone hardening PR ahead of the full P2 pipe-peer work.

## Note

Surfaced 2026-07-05 during the A2 orphan-reaper design pass; architect-verified against
HEAD. Registered per the Adjacent-findings protocol.

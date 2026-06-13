package gui

// SEC-F3 — Windows owner-SID arm of the `mcphub gui --force --kill` identity
// gate.
//
// processOwnerSIDMatchesCurrentFn is the package seam checkIdentityGateInternal
// consults as an ADDITIONAL fail-closed arm before authorizing a kill of the
// recorded flock holder. It mirrors the POSIX reaper's UID gate: a GUI process
// owned by a DIFFERENT user SID must not be terminated by this user's mcphub,
// even when its image basename, argv subcommand, and start time all match.
//
// Contract (FAIL CLOSED):
//   - (true,  nil) — target owner SID == current user → kill may proceed.
//   - (false, nil) — proven different-owner SID → refuse the kill.
//   - (false, err) — owner unverifiable (token open/query failed) → refuse.
//
// The production default is wired per-platform: Windows delegates to the single
// shared helper process.ProcessOwnerSIDMatchesCurrent (so the SID logic has one
// owner across api + cli + gui); non-Windows is a no-op returning (true, nil)
// so the Linux/macOS identity-gate behavior is UNCHANGED. Tests override the
// var to simulate same-SID / different-SID / unverifiable.
var processOwnerSIDMatchesCurrentFn = defaultProcessOwnerSIDMatchesCurrent

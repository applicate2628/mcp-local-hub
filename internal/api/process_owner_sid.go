package api

// SEC-F3 — Windows owner-SID arm of the `mcphub stop --force` kill gates.
//
// processOwnerSIDMatchesCurrentFn is the package seam the kill gates
// (requireMcphubPIDImage, requireMcphubPortOwnerPID in install.go) consult as
// an ADDITIONAL fail-closed arm before authorizing a taskkill against a PID.
// It mirrors the POSIX cold-start reaper's UID gate
// (supervise_reaper_posix.go): a process owned by a DIFFERENT user SID must
// not be force-killed by this user's mcphub, even when its image/argv/start
// time all match.
//
// Contract (FAIL CLOSED):
//   - (true,  nil) — target owner SID == current user → kill may proceed.
//   - (false, nil) — proven different-owner SID → refuse the kill.
//   - (false, err) — owner unverifiable (token open/query failed) → refuse.
//
// The production default is wired per-platform: Windows calls
// process.ProcessOwnerSIDMatchesCurrent; non-Windows is a no-op returning
// (true, nil) so the POSIX kill paths (which already carry their own UID gate
// where applicable, and have no Windows token model) are UNCHANGED. Tests
// override the var to simulate same-SID / different-SID / unverifiable.
var processOwnerSIDMatchesCurrentFn = defaultProcessOwnerSIDMatchesCurrent

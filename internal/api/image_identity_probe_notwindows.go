//go:build !windows

package api

// imageIdentityProbeSupported is false on every non-Windows target: their
// guiImageForPID is a stub with no process-image resolver behind it, so a miss
// there is STRUCTURAL, not a failure. See the Windows counterpart for the full
// rationale, and AssertMCPFrontPortSupervisorOwned's PLATFORM POSTURE block for
// what each tier is allowed to conclude.
//
// One declaration for ALL non-Windows targets rather than one per target: the
// property being declared is "no image resolver exists here", which is true by
// construction for anything that is not Windows, so a new POSIX target inherits
// the honest answer instead of needing someone to remember to add a `false`.
const imageIdentityProbeSupported = false

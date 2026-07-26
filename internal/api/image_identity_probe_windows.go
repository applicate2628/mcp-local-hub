//go:build windows

package api

// imageIdentityProbeSupported declares whether THIS build target can resolve a
// process's image basename at all (guiImageForPID).
//
// It exists so a consumer can tell a FAILED image lookup apart from a
// STRUCTURALLY ABSENT one. Both surface as guiImageForPID returning
// ("", false), but they demand opposite responses: on a platform that can
// answer, a miss is a real failure and an identity proof must fail closed on
// it; on a platform that has no resolver at all, refusing every call would
// simply make the feature unavailable there, and quietly proceeding would be
// an undocumented downgrade of the proof. Reading an explicit capability flag
// makes the platform tier a stated fact rather than something inferred from a
// miss. See AssertMCPFrontPortSupervisorOwned's PLATFORM POSTURE block.
//
// Windows resolves images via procNameAndParent, so an unresolvable image here
// is a genuine failure.
const imageIdentityProbeSupported = true

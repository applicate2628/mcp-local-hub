//go:build windows && amd64 && cgo

package caller

import "errors"

// ErrInvalidOutcome means no returned Outcome may be treated as a trustworthy
// observation. A separately transferred owner or cleared release slot remains
// a custody fact: preserve it even with this error; never retry the operation.
// This is protocol validation, not protection from hostile in-process code.
var ErrInvalidOutcome = errors.New("invalid native outcome")

// outcomeViolation validates the fixed ABI1 value contract after layout is
// checked. Unused fields are canonically zero, as initialized by the Rust ABI.
// All callers share this check; no alternate decoder or state registry exists.
func outcomeViolation(status uint32, v Outcome) string {
	switch {
	case status > notSettled:
		return "unknown call status"
	case v.State > cleanupFailed:
		return "unknown settlement state"
	case v.Flags & ^uint32(3) != 0:
		return "unknown observation flags"
	case v.CleanupMask & ^uint32(7) != 0:
		return "unknown cleanup slots"
	case status == nativeFailure && v.NativeError == 0:
		return "native failure without its code"
	case status != nativeFailure && v.NativeError != 0:
		return "native code without native-failure status"
	case v.Flags&1 == 0 && v.RootExit != 0:
		return "root code without a known root result"
	case v.Flags&2 == 0 && v.Active != 0:
		return "active count without a known observation"
	case v.State != 0 && v.PID == 0:
		return "settlement without a process identity"
	case (v.State == complete || v.State == cleanupFailed) && v.Flags&1 == 0:
		return "terminal settlement without a known root result"
	case (v.State == complete || v.State == cleanupFailed) && v.Active != 0:
		return "terminal settlement with active members"
	case v.State == cleanupFailed && v.CleanupMask == 0:
		return "cleanup failure without failed slots"
	case v.State != cleanupFailed && v.CleanupMask != 0:
		return "cleanup failures outside cleanup-failed state"
	}
	for i, code := range v.CleanupCodes {
		if (v.CleanupMask&(1<<i) != 0) != (code != 0) {
			return "cleanup slot and error code disagree"
		}
	}
	return ""
}

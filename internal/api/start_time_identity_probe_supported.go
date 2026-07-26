//go:build windows || linux

package api

// startTimeIdentityProbeSupported declares whether THIS build target can read
// a PID's kernel-recorded process creation time
// (internal/process.ProcessStartTime).
//
// Windows resolves it via GetProcessTimes on an OpenProcess handle; Linux via
// /proc/<pid>/stat field 22 against /proc/stat's btime. Both are real kernel
// facts, not self-reported values, which is what makes them usable as identity.
//
// It is a DECLARED capability rather than an inferred one, mirroring
// imageIdentityProbeSupported: a gate that needs this leg must be able to ask
// "can this platform answer at all?" and fail closed on a target that cannot,
// instead of silently treating an unavailable probe as a passing one.
const startTimeIdentityProbeSupported = true

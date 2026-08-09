//go:build !windows && !linux

package api

// startTimeIdentityProbeSupported is false on macOS and every other POSIX
// preview target: internal/process.ProcessStartTime is a stub there
// (process_start_time_other.go), so no start-time proof is available.
//
// Callers must fail closed rather than skip the leg. On those targets the
// mcp-front ownership gate already refuses for an independent reason — its
// loopback socket-owner resolver is a fail-closed stub too — so this changes
// no shipped behavior; it makes the reason explicit instead of accidental.
const startTimeIdentityProbeSupported = false

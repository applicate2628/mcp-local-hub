//go:build !windows

package cli

import "github.com/spf13/cobra"

// ephemeralRangePortContains — POSIX stub. The OS-ephemeral-range-overlaps-pool
// problem is a Windows/WSL2 phenomenon, so off Windows the L3 event's
// inside_ephemeral_range is always "unknown" (mirrors the Windows-only self-heal
// classification).
func ephemeralRangePortContains(int) (bool, bool) { return false, false }

// runSetupEphemeralRangeStep — POSIX no-op. `netsh` and the dynamic-port range
// are Windows-only, so the setup detect+warn step (and the --fix-ephemeral-range
// mutation) do nothing here. The flag is still accepted on the command for a
// uniform CLI surface; it is inert off Windows.
func runSetupEphemeralRangeStep(*cobra.Command, bool) {}

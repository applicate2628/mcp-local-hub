//go:build windows

package cli

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/process"
)

// TestNewInstallSupervisorCmdSuppressesConsoleAttach covers the
// highest-impact instance of the console-attach class: the supervisor that
// `mcphub install --upgrade` starts after replacing the binary.
//
// That command is normally TYPED AT A TERMINAL, so the CLI process holds a
// console. Without the marker the freshly started supervisor attaches to
// it; the operator then closes the window believing the upgrade finished,
// CTRL_CLOSE_EVENT reaches the new supervisor, and KILL_ON_JOB_CLOSE reaps
// the fleet it just started. Measured externally against a real
// -H windowsgui build with this site's flag set (0x00000208):
//
//	no marker -> child appears in the parent's GetConsoleProcessList
//	marker    -> never appears
//
// The strict-mode variant is asserted too because it is a separate argv
// built through the same function, and an implementation that marked only
// one shape would be a silent half-fix.
func TestNewInstallSupervisorCmdSuppressesConsoleAttach(t *testing.T) {
	marker := process.SuppressConsoleAttachEnv + "=1"
	hasMarker := func(env []string) bool {
		for _, e := range env {
			if strings.EqualFold(e, marker) {
				return true
			}
		}
		return false
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{"plain", []string{"supervise"}},
		{"strict-mode", []string{"supervise", "--strict-mode"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newInstallSupervisorCmd("mcphub", tc.args)
			if !hasMarker(cmd.Env) {
				t.Fatalf("install/upgrade supervisor spawn is not attach-suppressed (%s missing); "+
					"the new supervisor would die with the terminal the upgrade was typed into",
					process.SuppressConsoleAttachEnv)
			}
			if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags == 0 {
				t.Error("detach creation flags missing: the marker stops the deliberate re-attach, " +
					"but only the flags block console INHERITANCE at create time")
			}
		})
	}
}

// TestInstallSupervisorRetriesKeepTheSuppressionMarker is the one that
// matters on a locked-down host.
//
// startSupervisorDetachedBreakaway degrades on ERROR_ACCESS_DENIED by
// REBUILDING the command with creation flags stripped. Measured at
// CreationFlags=0, a child with no detach flags at all still does NOT
// become a console client when the marker is set — so the marker is the
// only protection that survives that degradation, and it survives only
// because every retry rebuilds through the same constructor. An
// implementation that applied the marker to the first attempt alone would
// leave the degraded path exposed, which is exactly the host where the
// flags were already refused.
// It exercises the BUILDER the spawn path actually hands to
// startSupervisorDetachedBreakaway, not the constructor directly. That
// distinction is the whole test: an earlier revision called
// newInstallSupervisorCmd three times in a row, which passed against a
// deliberately broken builder that marked only its first attempt. Calling
// the real builder repeatedly is what makes the retry property observable.
func TestInstallSupervisorRetriesKeepTheSuppressionMarker(t *testing.T) {
	marker := process.SuppressConsoleAttachEnv + "=1"

	for _, strictMode := range []bool{false, true} {
		build := installSupervisorCmdBuilder("mcphub", strictMode)
		// Three rebuilds: the initial attempt, the breakaway-cleared
		// retry, and the flags-stripped minimal retry.
		for attempt := 1; attempt <= 3; attempt++ {
			found := false
			for _, e := range build().Env {
				if strings.EqualFold(e, marker) {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("rebuild #%d (strictMode=%v) lost the suppression marker; the "+
					"breakaway/flagless retries would spawn an attach-capable supervisor on "+
					"exactly the hosts that refused the detach flags", attempt, strictMode)
			}
		}
	}
}

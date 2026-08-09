package gui

import (
	"os"
	"strings"
	"sync"
)

const (
	restartV3DefaultEnabled = true
	restartV3Env            = "MCPHUB_GUI_RESTART_V3"
)

var (
	restartV3Once     sync.Once
	restartV3Resolved bool
)

// RestartV3Enabled resolves the process-wide GUI restart rollout gate once.
// The environment override accepts 1/true and 0/false after trimming and
// case-folding; any other value keeps the compiled default.
func RestartV3Enabled() bool {
	restartV3Once.Do(func() {
		restartV3Resolved = resolveRestartV3Enabled(os.Getenv(restartV3Env))
	})
	return restartV3Resolved
}

func resolveRestartV3Enabled(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true":
		return true
	case "0", "false":
		return false
	default:
		return restartV3DefaultEnabled
	}
}

// ResetRestartV3ResolvedForTest clears the memoized RestartV3Enabled()
// resolution so a test can force a fresh env-var read. Production code never
// calls this — RestartV3Enabled() is deliberately resolved once per process.
// Exported (rather than a _test.go-local helper) so cross-package tests —
// e.g. internal/cli's ensure-alive tests proving the P1-2 review fix
// (recognition of another process's in-flight handoff marker must not
// depend on THIS process's own RestartV3Enabled() resolution) — can force a
// deterministic re-resolution without depending on test-execution order
// across the whole test binary.
func ResetRestartV3ResolvedForTest() {
	restartV3Once = sync.Once{}
	restartV3Resolved = false
}

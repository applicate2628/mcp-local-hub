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

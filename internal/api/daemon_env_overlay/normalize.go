// Package daemon_env_overlay provides helpers for the per-daemon env
// overlay file that augments SupervisorIntent at spawn time.
//
// Overlay keys must match SupervisorDaemon.TaskName canonical form
// (leading backslash, e.g. "\\mcp-local-hub-memory-default") — see
// internal/api/supervisor_intent.go:25. NormalizeOverlayKey is the
// single normalization point used by every overlay call site
// (spawn lookup, GUI write, mutator, orphan detection).
package daemon_env_overlay

import "strings"

// NormalizeOverlayKey returns the canonical overlay-map key for the
// given task name. The canonical form has a leading backslash to
// match SupervisorDaemon.TaskName (see supervisor_intent.go:25).
//
// Behavior:
//   - "mcp-local-hub-memory-default"  -> "\\mcp-local-hub-memory-default"
//   - "\\mcp-local-hub-memory-default" -> "\\mcp-local-hub-memory-default" (idempotent)
//   - ""                              -> "" (empty preserved)
func NormalizeOverlayKey(taskName string) string {
	if taskName == "" {
		return ""
	}
	if strings.HasPrefix(taskName, "\\") {
		return taskName
	}
	return "\\" + taskName
}

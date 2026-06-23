// internal/gui/separate_process_detect_windows.go
//go:build windows

// SeparateProcess=1 detection (bug
// work-items/bugs/2026-06-22-explorer-folder-window-orphan-flood.md). The
// Windows HKCU setting
// ...\Explorer\Advanced\SeparateProcess = 1 ("Launch folder windows in a
// separate process") makes every `explorer.exe /select` reveal spawn a
// SEPARATE persistent explorer.exe process instead of delegating to the
// running shell instance. That is the precondition for the reveal-window
// flood that `mcphub gui --force --reveal` can leave behind.
//
// detectSeparateProcessOnce reads the setting and, when it is 1, emits a
// single operator-facing warn event naming the behavior and the hub's
// mitigation (--force recovery is print-only by default; --reveal opts in
// to a single un-reapable persistent window). It is the single owner of
// this warning; no consumer-side duplication. Missing value / read error
// is treated as 0 (no warn) — fail-soft.

package gui

import (
	"mcp-local-hub/internal/api"

	"golang.org/x/sys/windows/registry"
)

const (
	// explorerAdvancedKeyPath is the HKCU subkey holding the shell's
	// Advanced folder options.
	explorerAdvancedKeyPath = `Software\Microsoft\Windows\CurrentVersion\Explorer\Advanced`
	// separateProcessValueName is the REG_DWORD toggled by the
	// "Launch folder windows in a separate process" folder option.
	separateProcessValueName = "SeparateProcess"
)

// readSeparateProcessFn is the injectable registry-read seam. Production
// binds it to the real HKCU read; tests swap it to drive value=1 / 0 /
// error deterministically without depending on the host's actual setting.
var readSeparateProcessFn = readExplorerSeparateProcess

// sepProcessWarnFn is the injectable warn-emit seam. Production binds it
// to api.LogHubMcpEvent; tests swap it to count emissions without
// touching the real hub-mcp.log.
var sepProcessWarnFn = func(level, event string, fields map[string]any) error {
	return api.LogHubMcpEvent(level, event, fields)
}

// detectSeparateProcessOnce emits the SeparateProcess=1 warning at most
// once per GUI process (guarded by s.revealSepProcessWarned). It is safe
// to call repeatedly; only the first call with the key == 1 emits. Called
// once during GUI startup wiring.
func detectSeparateProcessOnce(s *Server) {
	if !readSeparateProcessFn() {
		return
	}
	s.revealSepProcessWarned.Do(func() {
		_ = sepProcessWarnFn("warn", "explorer-separate-process-detected", map[string]any{
			"setting": `HKCU\` + explorerAdvancedKeyPath + `\` + separateProcessValueName,
			"value":   1,
			"impact": "Windows is set to 'Launch folder windows in a separate process', " +
				"so each file-manager reveal spawns a persistent explorer.exe process. " +
				"mcphub's --force recovery no longer auto-opens a folder window; pass " +
				"--force --reveal only when needed (it leaves one persistent explorer.exe " +
				"window this option cannot reap).",
		})
	})
}

// readExplorerSeparateProcess returns true iff
// HKCU\...\Explorer\Advanced\SeparateProcess is a DWORD equal to 1. A
// missing value, missing key, wrong type, or any read error returns false
// (fail-soft — the warning is advisory, never a startup blocker).
func readExplorerSeparateProcess() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, explorerAdvancedKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue(separateProcessValueName)
	if err != nil {
		return false
	}
	return v == 1
}

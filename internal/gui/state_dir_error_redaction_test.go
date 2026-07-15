package gui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// statePathDerivedErrorCodes are the GUI error codes whose error value derives
// from api.DaemonStateDir() or a supervisor-IPC setup/transport failure. On POSIX
// those errors embed the operator's absolute state-dir path (errStateParentInsecure,
// state_paths.go), the absolute supervisor.lock.owner.json path, or the absolute
// unix-socket path. Every one MUST be emitted through writeAPIErrorRedacted (logs the
// raw error server-side, returns {"error":"internal error","code":...}) — never bare
// writeAPIError, which would surface the path on the wire (phase-1 audit finding 4).
// These codes are ALWAYS redacted (their error can only embed a path, never a
// curated operator message). STATUS_FAILED is intentionally NOT here: it has a
// fail-closed allowlist branch that keeps the path-free ErrSupervisorDown message
// raw while redacting the path-bearing setup failure, so it is verified by
// handler-level tests (TestStatusEndpoint_* + TestStatusEndpoint_RedactsSetupFailurePath)
// rather than this per-line source scan.
var statePathDerivedErrorCodes = []string{
	`"STATE_DIR_FAILED"`,     // resolveOverlayPath / supervisor_restart -> DaemonStateDir
	`"STATE_READ_FAILED"`,    // loadCurrentSupervisorIntent -> DaemonStateDir + intent read
	`"RESPAWN_SETUP_FAILED"`, // DialSupervisorIPCRespawn setup (state dir / owner.json)
	`"IPC_FAILED"`,           // DialSupervisorIPCRespawn transport (unix-socket path)
}

// TestStatePathErrorsGoThroughRedactingHelper is the structural regression guard for
// phase-1 audit finding 4: every state-path/IPC-derived error response must stay
// redacted. The redaction contract itself is unit-tested by
// TestWriteAPIErrorRedacted_HidesRawPathKeepsCode; this guard proves the specific
// leak-prone sites stay wired to it (a revert to bare writeAPIError re-opens the
// leak). Source-level rather than handler-level because forcing a real DaemonStateDir
// failure needs an unexported api-package resolver stub the gui test package cannot
// reach.
//
// SCOPE: this closes the DaemonStateDir + supervisor-IPC-state leak class. The broader
// per-handler audit of every GUI writeAPIError that could echo a config/vault path
// (backups, secrets, workspaces, settings, install, ...) is tracked separately in
// backlog/2026-07-15-gui-error-redaction-broad-audit.md — some of those echoes are
// intentional operator-facing paths the phase-1 audit EXEMPTED, so a blanket redact
// would over-correct.
func TestStatePathErrorsGoThroughRedactingHelper(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	perCode := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			for _, code := range statePathDerivedErrorCodes {
				if !strings.Contains(line, code) {
					continue
				}
				// Only lines that actually emit an API error (write*APIError*) are
				// gated; a bare mention of the code elsewhere (a comment, a table)
				// is not a wire response.
				if !strings.Contains(line, "writeAPIError") {
					continue
				}
				perCode[code]++
				if !strings.Contains(line, "writeAPIErrorRedacted(") {
					t.Errorf("%s:%d emits %s via bare writeAPIError — leaks the raw "+
						"state/IPC path (finding-4): %s", name, i+1, code, strings.TrimSpace(line))
				}
			}
		}
	}
	for _, code := range statePathDerivedErrorCodes {
		if perCode[code] == 0 {
			t.Errorf("no writeAPIError emission of %s found in %s — anchor stale, verify the site still exists",
				code, filepath.Clean("."))
		}
	}
}

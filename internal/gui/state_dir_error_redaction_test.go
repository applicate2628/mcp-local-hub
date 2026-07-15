package gui

import (
	"os"
	"strings"
	"testing"
)

// TestStateDirFailedGoesThroughRedactingHelper is a structural regression guard
// for phase-1 audit finding 4's residual: the GUI state-dir-resolution error
// handlers must return a redacted body, never the raw error. On POSIX,
// api.DaemonStateDir()'s errStateParentInsecure embeds the operator's absolute
// state-dir path (internal/api/state_paths.go), so a bare writeAPIError(...,
// "STATE_DIR_FAILED") would surface that path on the wire. Every STATE_DIR_FAILED
// emission must therefore go through writeAPIErrorRedacted (which logs the raw
// error server-side and returns {"error":"internal error","code":...}).
//
// The redaction contract itself is unit-tested by
// TestWriteAPIErrorRedacted_HidesRawPathKeepsCode; this guard proves the specific
// state-dir sites stay wired to it (a revert to bare writeAPIError re-opens the
// leak). Source-level rather than handler-level because forcing a real
// DaemonStateDir failure needs an unexported api-package resolver stub the gui
// test package cannot reach.
func TestStateDirFailedGoesThroughRedactingHelper(t *testing.T) {
	files := []string{"daemon_env.go", "supervisor_restart.go"}
	const marker = `"STATE_DIR_FAILED"`
	found := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, marker) {
				continue
			}
			found++
			if !strings.Contains(line, "writeAPIErrorRedacted(") {
				t.Errorf("%s:%d emits %s without writeAPIErrorRedacted — leaks the raw "+
					"state-dir error (finding-4 residual): %s", f, i+1, marker, strings.TrimSpace(line))
			}
		}
	}
	if found == 0 {
		t.Fatalf("no %s emission found in %v — test anchor stale, verify the sites still exist", marker, files)
	}
}

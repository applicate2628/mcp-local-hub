//go:build windows && test_state_path_env

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
)

// testPipeDiscriminator (test_state_path_env build) returns a stable per-test
// pipe-name suffix derived from the cli test seam MCPHUB_STATE_DIR_OVERRIDE,
// or "" when the seam is unset. It is compiled ONLY into binaries built with
// the `test_state_path_env` tag — the SAME tag that enables
// MCPHUB_STATE_DIR_OVERRIDE state resolution — i.e. the test matrix, never a
// release binary (the release build links the no-op variant in
// supervisor_ipc_pipe_disc_release_windows.go instead).
//
// The cli supervise IPC tests set MCPHUB_STATE_DIR_OVERRIDE per test; both the
// in-process listener and client read this same process-global env and
// converge on a per-test pipe, eliminating the SID-pipe contention flake
// (bug 2026-05-29-cli-supervise-ipc-tests-flaky-in-full-suite.md). The
// 16-hex-char SHA-256 prefix is collision-safe across a test binary's temp
// dirs and is a valid Windows pipe-name leaf (no path separators, well under
// the 256-char limit).
func testPipeDiscriminator() string {
	override := strings.TrimSpace(os.Getenv("MCPHUB_STATE_DIR_OVERRIDE"))
	if override == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(override))
	return hex.EncodeToString(sum[:8])
}

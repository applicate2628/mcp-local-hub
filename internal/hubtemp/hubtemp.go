// Package hubtemp resolves the hub-owned writable scratch directory that MCP
// servers use for intermediate files which must land somewhere reliably
// writable — NOT the inherited process TEMP, which on the live host is a small
// RAM disk (r:\Temp) that fails large writes. Two independent symptoms of that
// RAM disk drove this owner into existence:
//
//   - oneapi-run: icx-cl dies with "error #10026: error generating temporary
//     file" when TEMP points at the RAM disk.
//   - drmemory: Dr. Memory logs "WARNING: Unable to write to the disk. Ensure
//     that you have enough space and permissions." when its -logdir is on the
//     RAM disk, risking a truncated/missing results.txt on a real target.
//
// Both needed the same decision — "where is the hub's reliably-writable scratch
// dir" — so it lives in ONE place here instead of being copied per server,
// where the two copies would drift.
package hubtemp

import (
	"os"
	"path/filepath"
	"runtime"
)

// Dir returns the hub-owned writable scratch directory for subdir. On Windows
// it is %LOCALAPPDATA%\mcp-local-hub\<subdir> (falling back to
// <home>\.mcphub\<subdir> when LOCALAPPDATA is empty). On non-Windows
// os.TempDir() is already reliably writable, so <os.TempDir()>/<subdir> is
// used. Returns ("", false) only when no base can be derived at all (no
// LOCALAPPDATA and no home dir on Windows). The directory is NOT created;
// callers MkdirAll (or MkdirTemp under it) as needed.
func Dir(subdir string) (string, bool) {
	if runtime.GOOS != "windows" {
		return filepath.Join(os.TempDir(), subdir), true
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		return filepath.Join(la, "mcp-local-hub", subdir), true
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".mcphub", subdir), true
	}
	return "", false
}

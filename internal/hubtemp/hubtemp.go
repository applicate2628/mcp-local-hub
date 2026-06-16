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
	"strings"
	"time"
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

const activeRunMarker = ".mcp-local-hub-active"

// MarkActive creates a marker inside a just-created per-run scratch directory so
// a concurrent SweepStale does not remove it while the owning run is still in
// flight. The returned cleanup function removes only that marker; callers may
// still remove the whole directory separately when the run finishes.
func MarkActive(dir string) (func(), error) {
	marker := filepath.Join(dir, activeRunMarker)
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, writeErr := f.WriteString(time.Now().UTC().Format(time.RFC3339Nano) + "\n")
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(marker)
		return nil, writeErr
	}
	if closeErr != nil {
		_ = os.Remove(marker)
		return nil, closeErr
	}
	return func() { _ = os.Remove(marker) }, nil
}

// SweepStale best-effort-removes immediate sub-entries of dir whose name starts
// with prefix and whose mtime is older than ttl. It bounds the accumulation of
// per-run scratch dirs (vtune result DBs, drmemory logdirs) that LEAK when a run
// is killed mid-flight — a timeout force-kill, a daemon restart / supervisor
// reconcile, or a host reboot during a run — so the normal per-run defer
// os.RemoveAll is no longer the only cleanup path.
//
// Active runs call MarkActive after creating their scratch dir. SweepStale skips
// any directory carrying that marker, so request-controlled runs whose timeout
// exceeds ttl are not removed solely because the top-level directory mtime is
// old. Errors (in-use entry, permission glitch) are ignored — a sweep that can't
// remove one entry must not fail the run it is making room for. Non-matching
// siblings (e.g. a shared "symcache" dir) are left untouched.
func SweepStale(dir, prefix string, ttl time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-ttl)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if _, err := os.Lstat(filepath.Join(path, activeRunMarker)); err == nil {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(path)
	}
}

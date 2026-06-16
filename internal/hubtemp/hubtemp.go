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
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Dir returns the hub-owned writable scratch directory for subdir. On Windows
// it is %LOCALAPPDATA%\mcp-local-hub\<subdir> (falling back to
// <home>\.mcphub\<subdir> when LOCALAPPDATA is empty). On non-Windows it
// uses the current user's cache directory, not a predictable shared /tmp child,
// so another local user cannot pre-create the hub scratch base and deny service.
// Returns ("", false) only when no base can be derived at all. The directory is
// NOT created; callers use EnsurePrivateDir (or MkdirTemp under a directory it
// prepared) as needed.
func Dir(subdir string) (string, bool) {
	if runtime.GOOS != "windows" {
		if cache, err := os.UserCacheDir(); err == nil && cache != "" {
			return filepath.Join(cache, "mcp-local-hub", subdir), true
		}
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, ".mcphub", subdir), true
		}
		return "", false
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		return filepath.Join(la, "mcp-local-hub", subdir), true
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".mcphub", subdir), true
	}
	return "", false
}

const (
	activeRunMarker = ".mcp-local-hub-active"
	// activeRunMarkerTTL is deliberately longer than the stale-run TTLs used by
	// callers, so legitimate long-running tools are not removed solely because
	// their top-level scratch directory is old. It still bounds disk growth when
	// a daemon, host, or tool process terminates before MarkActive's cleanup runs.
	activeRunMarkerTTL = 24 * time.Hour
)

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

// EnsurePrivateDir creates dir as an owner-only directory and verifies that the
// current process can create a file inside it. The writability probe matters for
// pre-existing directories: os.MkdirAll succeeds when a directory already
// exists, even if it is owned by another local user and unusable by this
// process.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".write-test-")
	if err != nil {
		return fmt.Errorf("verify writable %s: %w", dir, err)
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Remove(name); err != nil {
		return err
	}
	return nil
}

// SweepStale best-effort-removes immediate sub-entries of dir whose name starts
// with prefix and whose mtime is older than ttl. It bounds the accumulation of
// per-run scratch dirs (vtune result DBs, drmemory logdirs) that LEAK when a run
// is killed mid-flight — a timeout force-kill, a daemon restart / supervisor
// reconcile, or a host reboot during a run — so the normal per-run defer
// os.RemoveAll is no longer the only cleanup path.
//
// Active runs call MarkActive after creating their scratch dir. SweepStale skips
// marker-bearing directories only while the marker is fresh, so request-
// controlled runs whose timeout exceeds ttl are not removed solely because the
// top-level directory mtime is old. Abandoned markers left behind by crashes or
// hard kills eventually expire and the stale directory is reclaimed. Errors
// (in-use entry, permission glitch) are ignored — a sweep that can't remove one
// entry must not fail the run it is making room for. Non-matching siblings (e.g.
// a shared "symcache" dir) are left untouched.
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
		if markerInfo, err := os.Lstat(filepath.Join(path, activeRunMarker)); err == nil {
			if markerInfo.ModTime().After(time.Now().Add(-activeRunMarkerTTL)) {
				continue
			}
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(path)
	}
}

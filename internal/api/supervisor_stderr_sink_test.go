package api

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRotateSupervisorStderrSinkIfOversize_AtBoundary pins the rotation
// boundary itself: exactly-at-threshold rotates, one byte under does not.
// An off-by-one here would either rotate constantly (destroying the
// forensic window) or never (unbounded growth).
func TestRotateSupervisorStderrSinkIfOversize_AtBoundary(t *testing.T) {
	for _, tc := range []struct {
		name       string
		size       int64
		wantRotate bool
	}{
		{"one-byte-under-threshold", SupervisorStderrSinkRotateSizeBytes - 1, false},
		{"exactly-at-threshold", SupervisorStderrSinkRotateSizeBytes, true},
		{"over-threshold", SupervisorStderrSinkRotateSizeBytes + 1024, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, SupervisorStderrSinkFileLeaf)
			writeSizedFile(t, path, tc.size, 'A')

			rotated, err := RotateSupervisorStderrSinkIfOversize(path)
			if err != nil {
				t.Fatalf("RotateSupervisorStderrSinkIfOversize: %v", err)
			}
			if rotated != tc.wantRotate {
				t.Fatalf("rotated = %v, want %v (size %d, threshold %d)",
					rotated, tc.wantRotate, tc.size, SupervisorStderrSinkRotateSizeBytes)
			}

			backup := path + ".1"
			if tc.wantRotate {
				if _, err := os.Stat(backup); err != nil {
					t.Fatalf("expected rotated backup at %s: %v", backup, err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("active sink should be gone after rotation, stat err = %v", err)
				}
			} else {
				if _, err := os.Stat(backup); !os.IsNotExist(err) {
					t.Fatalf("no backup expected below threshold, stat err = %v", err)
				}
			}
		})
	}
}

// TestRotateSupervisorStderrSinkIfOversize_ReplacesExistingBackup proves
// only ONE backup generation is retained, matching the .1-only discipline
// every other file in the state-dir log family uses.
func TestRotateSupervisorStderrSinkIfOversize_ReplacesExistingBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, SupervisorStderrSinkFileLeaf)

	if err := os.WriteFile(path+".1", []byte("STALE-BACKUP"), 0o600); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	writeSizedFile(t, path, SupervisorStderrSinkRotateSizeBytes, 'N')

	if _, err := RotateSupervisorStderrSinkIfOversize(path); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	got, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(got[:12]) == "STALE-BACKUP" {
		t.Fatal("stale .1 backup survived rotation; only one generation may be retained")
	}
	if got[0] != 'N' {
		t.Fatalf("backup should hold the rotated active sink, first byte = %q", got[0])
	}
}

// TestRotateSupervisorStderrSinkIfOversize_MissingFileIsNoop guards the
// first-boot path: no sink yet must not be an error.
func TestRotateSupervisorStderrSinkIfOversize_MissingFileIsNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), SupervisorStderrSinkFileLeaf)
	rotated, err := RotateSupervisorStderrSinkIfOversize(path)
	if err != nil {
		t.Fatalf("missing sink must not error: %v", err)
	}
	if rotated {
		t.Fatal("missing sink must not report a rotation")
	}
}

// TestSupervisorStderrSinkOversize_MatchesRotationThreshold keeps the
// heartbeat's visibility probe and the rotation decision on ONE threshold.
// If these two ever drift, the supervisor would either warn about a file it
// will not rotate or rotate one it never warned about.
func TestSupervisorStderrSinkOversize_MatchesRotationThreshold(t *testing.T) {
	dir := t.TempDir()

	under := filepath.Join(dir, "under.log")
	writeSizedFile(t, under, SupervisorStderrSinkRotateSizeBytes-1, 'u')
	if SupervisorStderrSinkOversize(under) {
		t.Fatal("file below threshold reported oversize")
	}

	at := filepath.Join(dir, "at.log")
	writeSizedFile(t, at, SupervisorStderrSinkRotateSizeBytes, 'a')
	if !SupervisorStderrSinkOversize(at) {
		t.Fatal("file at threshold not reported oversize")
	}

	if SupervisorStderrSinkOversize(filepath.Join(dir, "absent.log")) {
		t.Fatal("absent file reported oversize")
	}
}

// TestSupervisorStderrSinkSharesLogFamilyCeiling pins the sink to the same
// 10 MB ceiling as the supervisor event log, per the state-dir log-family
// contract documented in CLAUDE.md.
func TestSupervisorStderrSinkSharesLogFamilyCeiling(t *testing.T) {
	if SupervisorStderrSinkRotateSizeBytes != supervisorEventLogRotateSize {
		t.Fatalf("stderr sink ceiling %d != supervisor event log ceiling %d; the state-dir log family must share one ceiling",
			SupervisorStderrSinkRotateSizeBytes, supervisorEventLogRotateSize)
	}
	if SupervisorStderrSinkRotateSizeBytes != GUIEventLogRotateSizeBytes {
		t.Fatalf("stderr sink ceiling %d != GUI event log ceiling %d",
			SupervisorStderrSinkRotateSizeBytes, GUIEventLogRotateSizeBytes)
	}
}

// writeSizedFile creates a file of exactly size bytes filled with fill.
func writeSizedFile(t *testing.T, path string, size int64, fill byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()

	const chunk = 1 << 20
	buf := make([]byte, chunk)
	for i := range buf {
		buf[i] = fill
	}
	remaining := size
	for remaining > 0 {
		n := int64(chunk)
		if remaining < n {
			n = remaining
		}
		if _, err := f.Write(buf[:n]); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		remaining -= n
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync %s: %v", path, err)
	}
}

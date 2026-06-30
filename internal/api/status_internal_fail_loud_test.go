package api

// Deep-review P3 fix: mcphub status (CLI, via statusInternal) used to fall
// through SILENTLY to the legacy scheduler scan when the supervisor IPC was
// unreachable, while GET /api/status (health.go's DaemonsSection, once
// wired) failed LOUD with ErrSupervisorDown for the exact same condition.
// These tests pin the corrected fail-loud contract on statusInternal /
// Status() directly, using the statusInternalDialFn test seam so no real
// supervisor.lock needs to exist on disk.

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// installTestStatusInternalDialFn patches the package-level IPC status dial
// seam used by statusInternal, restoring the production
// DialSupervisorIPCStatus value via t.Cleanup.
func installTestStatusInternalDialFn(t *testing.T, fn func(ctx context.Context) ([]DaemonStatus, error)) {
	t.Helper()
	orig := statusInternalDialFn
	statusInternalDialFn = fn
	t.Cleanup(func() { statusInternalDialFn = orig })
}

// TestStatusInternal_FailsLoudOnSupervisorIPCUnavailable is the core
// regression guard: an unreachable supervisor must surface
// errors.Is(err, ErrSupervisorDown), not a silent scheduler-scan fallback.
func TestStatusInternal_FailsLoudOnSupervisorIPCUnavailable(t *testing.T) {
	installTestStatusInternalDialFn(t, func(ctx context.Context) ([]DaemonStatus, error) {
		return nil, fmt.Errorf("supervisor IPC status: dial: %w", ErrSupervisorIPCUnavailable)
	})

	rows, err := NewAPI().Status()
	if err == nil {
		t.Fatalf("Status() returned nil err on ErrSupervisorIPCUnavailable; want fail-loud ErrSupervisorDown; rows=%+v", rows)
	}
	if !errors.Is(err, ErrSupervisorDown) {
		t.Fatalf("err = %v, want errors.Is(err, ErrSupervisorDown)", err)
	}
	if rows != nil {
		t.Fatalf("rows = %+v, want nil on the fail-loud path", rows)
	}
}

// TestStatusInternal_PropagatesNonUnavailableIPCErrorsVerbatim asserts that a
// real transport/decode failure (handshake mismatch, malformed frame —
// anything that is NOT the rollout-transition ErrSupervisorIPCUnavailable
// case) is neither swallowed nor remapped to ErrSupervisorDown; it must
// propagate unchanged so the caller sees the actual cause.
func TestStatusInternal_PropagatesNonUnavailableIPCErrorsVerbatim(t *testing.T) {
	wantErr := errors.New("supervisor IPC status: decode status result: unexpected EOF")
	installTestStatusInternalDialFn(t, func(ctx context.Context) ([]DaemonStatus, error) {
		return nil, wantErr
	})

	rows, err := NewAPI().Status()
	if err == nil {
		t.Fatalf("Status() returned nil err on a non-unavailable IPC failure; rows=%+v", rows)
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want errors.Is(err, wantErr) (verbatim propagation, no remap)", err)
	}
	if errors.Is(err, ErrSupervisorDown) {
		t.Fatalf("err = %v, want NOT ErrSupervisorDown (only ErrSupervisorIPCUnavailable remaps)", err)
	}
}

// TestStatusInternal_HappyPathReturnsIPCRows is the positive control: a
// successful IPC dial returns the rows unchanged, no error.
func TestStatusInternal_HappyPathReturnsIPCRows(t *testing.T) {
	want := []DaemonStatus{{TaskName: `\mcp-local-hub-memory-default`, State: "Running", Port: 9123}}
	installTestStatusInternalDialFn(t, func(ctx context.Context) ([]DaemonStatus, error) {
		return want, nil
	})

	rows, err := NewAPI().Status()
	if err != nil {
		t.Fatalf("Status(): %v", err)
	}
	if len(rows) != 1 || rows[0].TaskName != want[0].TaskName {
		t.Fatalf("rows = %+v, want %+v", rows, want)
	}
}

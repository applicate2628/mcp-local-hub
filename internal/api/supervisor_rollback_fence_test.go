package api

import (
	"context"
	"errors"
	"github.com/gofrs/flock"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWithEmptyStopSettlementFenceRejectsUnsafeReceiptShapes names the
// regression it catches: accepting a partial, tampered, or pending receipt map
// would let rollback kill a successor across an unproven lifecycle transition.
func TestWithEmptyStopSettlementFenceRejectsUnsafeReceiptShapes(t *testing.T) {
	validRows := map[string]StopSettlementReceiptV1{
		`\mcp-local-hub-time-default`: {Version: 1, TaskName: `\mcp-local-hub-time-default`, Epoch: 1},
	}
	validRowsDigest, err := StopSettlementMapDigest(1, 1, validRows)
	if err != nil {
		t.Fatalf("digest nonempty rows: %v", err)
	}

	cases := []struct {
		name  string
		state *SupervisorStateFile
	}{
		{
			name:  "partial epoch tuple",
			state: &SupervisorStateFile{Version: 1, StopSettlementEpoch: 1},
		},
		{
			name:  "uppercase digest",
			state: &SupervisorStateFile{Version: 1, StopSettlementEpoch: 1, StopSettlementMapGeneration: 1, StopSettlementDigest: strings.ToUpper(validRowsDigest)},
		},
		{
			name:  "mismatched digest",
			state: &SupervisorStateFile{Version: 1, StopSettlementEpoch: 1, StopSettlementMapGeneration: 1, StopSettlementDigest: strings.Repeat("0", 64)},
		},
		{
			name:  "valid but pending receipt",
			state: &SupervisorStateFile{Version: 1, StopSettlementEpoch: 1, StopSettlementMapGeneration: 1, StopSettlementDigest: validRowsDigest, StopSettlements: validRows},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(hardenedTempDir(t), "supervisor-state.json")
			if err := WriteSupervisorState(path, tc.state); err != nil {
				t.Fatalf("seed state: %v", err)
			}
			called := false
			err := WithEmptyStopSettlementFence(context.Background(), path, func() error {
				called = true
				return nil
			})
			if err == nil {
				t.Fatal("fence accepted unsafe receipt shape")
			}
			if called {
				t.Fatal("critical callback ran for unsafe receipt shape")
			}
		})
	}
}

// TestWithEmptyStopSettlementFenceRejectsUnsupportedStateVersions catches the
// fail-open version default: an absent, zero, or future state version must not
// authorize rollback's destructive callback merely because its tuple is zero.
func TestWithEmptyStopSettlementFenceRejectsUnsupportedStateVersions(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "absent version", raw: `{}`},
		{name: "zero version", raw: `{"version":0}`},
		{name: "future version", raw: `{"version":2}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(hardenedTempDir(t), "supervisor-state.json")
			if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
				t.Fatalf("seed raw state: %v", err)
			}
			callbackCalls := 0
			err := WithEmptyStopSettlementFence(context.Background(), path, func() error {
				callbackCalls++
				return nil
			})
			if err == nil {
				t.Fatal("fence accepted unsupported state version")
			}
			if callbackCalls != 0 {
				t.Fatalf("critical callback calls = %d, want 0", callbackCalls)
			}
		})
	}
}

// TestWithEmptyStopSettlementFenceAllowsOnlyEmptyAuthenticatedOrVirginState
// catches a future broadening that lets a nonempty receipt map through, or
// rejects a valid zero-generation state before its first stop transaction.
func TestWithEmptyStopSettlementFenceAllowsOnlyEmptyAuthenticatedOrVirginState(t *testing.T) {
	emptyRows := map[string]StopSettlementReceiptV1{}
	digest, err := StopSettlementMapDigest(9, 4, emptyRows)
	if err != nil {
		t.Fatalf("digest empty rows: %v", err)
	}
	cases := []struct {
		name  string
		state *SupervisorStateFile
	}{
		{
			name:  "authenticated empty state with nil rows",
			state: &SupervisorStateFile{Version: 1, StopSettlementEpoch: 9, StopSettlementMapGeneration: 4, StopSettlementDigest: digest},
		},
		{
			name:  "virgin state",
			state: &SupervisorStateFile{Version: 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(hardenedTempDir(t), "supervisor-state.json")
			if err := WriteSupervisorState(path, tc.state); err != nil {
				t.Fatalf("seed state: %v", err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read before: %v", err)
			}
			called := false
			if err := WithEmptyStopSettlementFence(context.Background(), path, func() error {
				called = true
				return nil
			}); err != nil {
				t.Fatalf("fence: %v", err)
			}
			if !called {
				t.Fatal("critical callback did not run")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read after: %v", err)
			}
			if string(after) != string(before) {
				t.Fatal("read-only fence rewrote supervisor state")
			}
		})
	}
}

// TestWithEmptyStopSettlementFenceRefusesMissingCorruptOrContendedState names
// the fail-closed inputs that must never reach the destructive callback.
func TestWithEmptyStopSettlementFenceRefusesMissingCorruptOrContendedState(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		path := filepath.Join(hardenedTempDir(t), "supervisor-state.json")
		called := false
		err := WithEmptyStopSettlementFence(context.Background(), path, func() error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("missing state err=%v called=%v, want refusal before callback", err, called)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		path := filepath.Join(hardenedTempDir(t), "supervisor-state.json")
		if err := os.WriteFile(path, []byte(`{"version":`), 0o600); err != nil {
			t.Fatal(err)
		}
		called := false
		err := WithEmptyStopSettlementFence(context.Background(), path, func() error {
			called = true
			return nil
		})
		if err == nil || called {
			t.Fatalf("corrupt state err=%v called=%v, want refusal before callback", err, called)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		path := filepath.Join(hardenedTempDir(t), "supervisor-state.json")
		if err := WriteSupervisorState(path, &SupervisorStateFile{Version: 1}); err != nil {
			t.Fatalf("seed state: %v", err)
		}
		lock := flock.New(path + ".lock")
		if err := lock.Lock(); err != nil {
			t.Fatalf("hold state lock: %v", err)
		}
		t.Cleanup(func() { _ = lock.Unlock() })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		called := false
		err := WithEmptyStopSettlementFence(ctx, path, func() error {
			called = true
			return nil
		})
		if !errors.Is(err, context.Canceled) || called {
			t.Fatalf("contended cancelled fence err=%v called=%v, want context cancellation before callback", err, called)
		}
	})

	t.Run("callback error releases lock", func(t *testing.T) {
		path := filepath.Join(hardenedTempDir(t), "supervisor-state.json")
		if err := WriteSupervisorState(path, &SupervisorStateFile{Version: 1}); err != nil {
			t.Fatalf("seed state: %v", err)
		}
		want := errors.New("force kill failed")
		if err := WithEmptyStopSettlementFence(context.Background(), path, func() error { return want }); !errors.Is(err, want) {
			t.Fatalf("callback error = %v, want %v", err, want)
		}
		if err := WithEmptyStopSettlementFence(context.Background(), path, func() error { return nil }); err != nil {
			t.Fatalf("second fence after callback failure: %v", err)
		}
	})
}

// TestWithEmptyStopSettlementFenceSerializesCriticalAdmission catches a
// regression in which the state is checked under flock but the destructive
// callback runs after unlock. Both ownership orders are driven by channels;
// there are no sleep-based race windows.
func TestWithEmptyStopSettlementFenceSerializesCriticalAdmission(t *testing.T) {
	for _, order := range []string{"first-a", "first-b"} {
		t.Run(order, func(t *testing.T) {
			path := filepath.Join(hardenedTempDir(t), "supervisor-state.json")
			if err := WriteSupervisorState(path, &SupervisorStateFile{Version: 1}); err != nil {
				t.Fatalf("seed state: %v", err)
			}
			firstEntered := make(chan struct{})
			releaseFirst := make(chan struct{})
			secondEntered := make(chan struct{})
			firstErr := make(chan error, 1)
			secondErr := make(chan error, 1)

			first := func() {
				firstErr <- WithEmptyStopSettlementFence(context.Background(), path, func() error {
					close(firstEntered)
					<-releaseFirst
					return nil
				})
			}
			second := func() {
				secondErr <- WithEmptyStopSettlementFence(context.Background(), path, func() error {
					close(secondEntered)
					return nil
				})
			}

			go first()
			<-firstEntered
			go second()
			select {
			case <-secondEntered:
				t.Fatal("second critical callback entered while first fence remained held")
			default:
			}
			close(releaseFirst)
			if err := <-firstErr; err != nil {
				t.Fatalf("first fence: %v", err)
			}
			if err := <-secondErr; err != nil {
				t.Fatalf("second fence: %v", err)
			}
			select {
			case <-secondEntered:
			default:
				t.Fatal("second critical callback never ran")
			}
		})
	}
}

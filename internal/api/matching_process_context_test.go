package api

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestListMatchingProcessesContextCancelsWithoutFallback(t *testing.T) {
	probeCause := errors.New("synthetic WMIC failure")
	for _, tc := range []struct {
		name   string
		runErr error
	}{
		{name: "failure", runErr: probeCause},
		{name: "late success"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), probeCommandTimeout)
			defer cancel()
			deadline, _ := ctx.Deadline()
			var commands []string
			var observedError error
			fixture := []byte("Node,CommandLine,ProcessId,WorkingSetSize\nHOST,mcphub.exe gui,4321,2048\n")
			rows, err := listMatchingProcessesContext(ctx, []string{"mcphub.exe"}, func(received context.Context, name string, _ ...string) ([]byte, error) {
				commands = append(commands, name)
				if name == "wmic" {
					gotDeadline, ok := received.Deadline()
					if !ok || !gotDeadline.Equal(deadline) {
						t.Error("WMIC did not receive the caller deadline")
					}
					cancel()
					observedError = received.Err()
					return fixture, tc.runErr
				}
				return fixture, nil
			})
			if observedError != context.Canceled {
				t.Errorf("WMIC context did not receive cancellation: %v", observedError)
			}
			if !reflect.DeepEqual(commands, []string{"wmic"}) {
				t.Errorf("unexpected command sequence: %v", commands)
			}
			if rows != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, ErrProbeTimeout) {
				t.Fatalf("expected no rows and both cancellation identities, got %v, %v", rows, err)
			}
			if tc.runErr != nil && !errors.Is(err, tc.runErr) {
				t.Fatalf("lost original runner error: %v", err)
			}
		})
	}
}

func TestListMatchingProcessesContextKeepsLiveFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), probeCommandTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	var commands []string
	rows, err := listMatchingProcessesContext(ctx, []string{"mcphub.exe"}, func(received context.Context, name string, _ ...string) ([]byte, error) {
		commands = append(commands, name)
		if got, ok := received.Deadline(); !ok || !got.Equal(deadline) || received.Err() != nil {
			t.Error("fallback chain did not keep the live caller deadline")
		}
		if name == "wmic" {
			return nil, errors.New("synthetic WMIC unavailable")
		}
		return []byte("HOST,\"MCPHUB.EXE gui\",4321,2048\nHOST,\"other.exe\",8765,1024\n"), nil
	})
	if !reflect.DeepEqual(commands, []string{"wmic", "powershell"}) {
		t.Fatalf("unexpected command sequence: %v", commands)
	}
	want := []ProcessInfo{{PID: 4321, RAMBytes: 2048, Cmdline: "MCPHUB.EXE gui"}}
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("fallback result %v, %v; want %v", rows, err, want)
	}
}

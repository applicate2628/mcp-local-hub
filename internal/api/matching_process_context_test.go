package api

import (
	"context"
	"errors"
	"reflect"
	"strings"
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
			rows, err := listMatchingProcessesContext(ctx, []string{"mcphub.exe"}, false, func(received context.Context, name string, _ ...string) ([]byte, error) {
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
	rows, err := listMatchingProcessesContext(ctx, []string{"mcphub.exe"}, false, func(received context.Context, name string, _ ...string) ([]byte, error) {
		commands = append(commands, name)
		if got, ok := received.Deadline(); !ok || !got.Equal(deadline) || received.Err() != nil {
			t.Error("fallback chain did not keep the live caller deadline")
		}
		if name == "wmic" {
			return nil, errors.New("synthetic WMIC unavailable")
		}
		return []byte("HOST,\"MCPHUB.EXE gui\",\"mcphub.exe\",4321,2048\nHOST,\"other.exe\",\"other.exe\",8765,1024\n"), nil
	})
	if !reflect.DeepEqual(commands, []string{"wmic", "powershell"}) {
		t.Fatalf("unexpected command sequence: %v", commands)
	}
	want := []ProcessInfo{{PID: 4321, RAMBytes: 2048, Cmdline: "MCPHUB.EXE gui"}}
	if err != nil || !reflect.DeepEqual(rows, want) {
		t.Fatalf("fallback result %v, %v; want %v", rows, err, want)
	}
}

func TestListProcessesByImageNameContextIgnoresCommandLineMentions(t *testing.T) {
	fixture := []byte("Node,CommandLine,Name,ProcessId,WorkingSetSize\n" +
		"HOST,\"python.exe check.py mcphub.exe\",python.exe,4321,2048\n" +
		"HOST,,MCPHUB.EXE,8765,1024\n" +
		"HOST,wrapper.exe,hub-issued-01.exe,9876,512\n" +
		"HOST,other.exe,old-mcphub.exe,9877,512\n")
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "wmic" || !reflect.DeepEqual(args, []string{"process", "get", "CommandLine,Name,ProcessId,WorkingSetSize", "/format:csv"}) {
			t.Fatalf("unexpected snapshot: %q %v", name, args)
		}
		return fixture, nil
	}
	names := []string{"mcphub.exe", "HUB-ISSUED-01.EXE"}
	exact, err := listMatchingProcessesContext(t.Context(), names, true, run)
	wantExact := []ProcessInfo{{PID: 8765, RAMBytes: 1024}, {PID: 9876, RAMBytes: 512, Cmdline: "wrapper.exe"}}
	if err != nil || !reflect.DeepEqual(exact, wantExact) {
		t.Fatalf("exact image matches = %v, %v; want %v", exact, err, wantExact)
	}
	broad, err := listMatchingProcessesContext(t.Context(), names, false, run)
	wantBroad := []ProcessInfo{{PID: 4321, RAMBytes: 2048, Cmdline: "python.exe check.py mcphub.exe"}}
	if err != nil || !reflect.DeepEqual(broad, wantBroad) {
		t.Fatalf("command-line matches = %v, %v; want %v", broad, err, wantBroad)
	}
}

func TestListProcessesByImageNameContextRequiresImageInventory(t *testing.T) {
	for _, tc := range []struct {
		name, fixture string
	}{
		{"missing Name header", "Node,CommandLine,ProcessId,WorkingSetSize\nHOST,mcphub.exe,4321,2048\n"},
		{"missing row Name", "Node,CommandLine,Name,ProcessId,WorkingSetSize\nHOST,mcphub.exe,,4321,2048\n"},
		{"malformed row", "Node,CommandLine,Name,ProcessId,WorkingSetSize\nHOST,mcphub.exe,mcphub.exe,4321\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := listMatchingProcessesContext(t.Context(), []string{"mcphub.exe"}, true, func(_ context.Context, name string, _ ...string) ([]byte, error) {
				if name != "wmic" {
					t.Fatalf("unqualified snapshot fell back to %s", name)
				}
				return []byte(tc.fixture), nil
			})
			if rows != nil || err == nil {
				t.Fatalf("incomplete image inventory returned %v, %v", rows, err)
			}
		})
	}
}

func TestListProcessesByImageNameContextValidatesCIMFallback(t *testing.T) {
	wmicErr := errors.New("synthetic WMIC unavailable")
	cimErr := errors.New("synthetic incomplete CIM enumeration")
	foreignRow := "HOST,\"python.exe check.py mcphub.exe\",\"python.exe\",4321,2048\n"
	for _, tc := range []struct {
		name    string
		output  string
		runErr  error
		wantErr bool
	}{
		{name: "header without process rows", wantErr: true},
		{name: "partial enumeration", output: foreignRow, runErr: cimErr, wantErr: true},
		{name: "complete inventory with no selected images", output: foreignRow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := listMatchingProcessesContext(t.Context(), []string{"mcphub.exe"}, true, func(_ context.Context, name string, args ...string) ([]byte, error) {
				switch name {
				case "wmic":
					return nil, wmicErr
				case "powershell":
					if len(args) == 0 || !strings.Contains(args[len(args)-1], "Get-CimInstance Win32_Process -ErrorAction Stop |") {
						t.Fatal("image inventory does not make incomplete CIM queries fatal")
					}
					return []byte(tc.output), tc.runErr
				default:
					t.Fatalf("unexpected process inventory command: %s", name)
					return nil, nil
				}
			})
			if len(rows) != 0 || (err != nil) != tc.wantErr {
				t.Fatalf("image inventory returned %v, %v; want zero rows and error=%v", rows, err, tc.wantErr)
			}
			if tc.runErr != nil && (!errors.Is(err, wmicErr) || !errors.Is(err, tc.runErr)) {
				t.Fatalf("fallback lost the original enumeration failures: %v", err)
			}
		})
	}
}

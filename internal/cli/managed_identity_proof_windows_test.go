//go:build windows

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

// Full command metadata in the image-only pass would violate this guard.
func TestManagedImageOnlyProof(t *testing.T) {
	start := time.Date(2026, 9, 1, 1, 2, 3, 123, time.UTC)
	for _, tc := range []struct {
		name       string
		require    bool
		pid        int
		bound      bool
		nativeErr  error
		image      string
		wantNative int
		fail       bool
	}{
		{"unbound", false, 0, false, nil, "synthetic.exe", 0, false},
		{"require unbound", true, 7, true, nil, "synthetic.exe", 0, true},
		{"invalid PID", false, 0, true, nil, "synthetic.exe", 0, true},
		{"native close failure", false, 7, true, os.ErrPermission, "synthetic.exe", 1, true},
		{"foreign image", false, 7, true, nil, "foreign.exe", 1, true},
		{"owned", false, 7, true, nil, "synthetic.exe", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			rows, anchors, err := observeWindowsProductIntentPortsWithInputs(context.Background(), "synthetic.exe", []windowsProductIntentPortV1{{TaskName: "synthetic", Port: 7}}, tc.require,
				func(context.Context, int) (int, bool, error) { return tc.pid, tc.bound, nil },
				func(pid int) (process.PIDHandleIdentity, error) {
					calls++
					return process.PIDHandleIdentity{PID: pid, ExecutablePath: tc.image, StartedAt: start}, tc.nativeErr
				})
			if (err != nil) != tc.fail || calls != tc.wantNative || (tc.fail && (rows != nil || anchors != nil)) {
				t.Fatalf("rows=%v anchors=%v error=%v native=%d", rows, anchors, err, calls)
			}
			if !tc.fail && tc.bound && !anchors[0].StartedAt.Equal(start) {
				t.Fatal("exact observation anchor lost")
			}
		})
	}
}

func TestManagedCaptureOrderedGenerationBinding(t *testing.T) {
	for _, defect := range []string{"none", "absent", "arguments", "metadata seconds", "exact generation", "parent", "account unreadable", "account mismatch", "zero anchors"} {
		t.Run(defect, func(t *testing.T) {
			stateDir := withTempStateDir(t)
			writeStateSidecarBytes(t, filepath.Join(stateDir, "supervisor-intent.json"), []byte(`{"version":1,"daemons":[{"task_name":"synthetic-a","command":"synthetic.exe","args":["daemon","--name","a"],"port":7},{"task_name":"synthetic-b","command":"synthetic.exe","args":["daemon","--name","a"],"port":8},{"task_name":"synthetic-unbound","command":"synthetic.exe","args":["daemon"],"port":9}]}`))
			start := time.Date(2026, 9, 1, 1, 2, 3, 123, time.UTC)
			setCalls, nativeCalls, parentCalls, userCalls := 0, 0, 0, 0
			in := upgradeGUIObservationInputs{
				CurrentIdentity: func(pid int) (process.PIDHandleIdentity, error) {
					nativeCalls++
					at := start
					if defect == "exact generation" && nativeCalls > 2 {
						at = at.Add(time.Nanosecond)
					}
					if defect == "zero anchors" {
						at = time.Time{}
					}
					return process.PIDHandleIdentity{PID: pid, ExecutablePath: "synthetic.exe", StartedAt: at}, nil
				},
				ParentPID: func(int) (int, error) {
					parentCalls++
					if defect == "parent" {
						return 99, nil
					}
					return 100, nil
				},
				SIDMatches: func(int) (bool, error) {
					userCalls++
					if defect == "account unreadable" {
						return false, os.ErrPermission
					}
					return defect != "account mismatch", nil
				},
			}
			portOwner := func(_ context.Context, port int) (int, bool, error) {
				if port == 9 {
					return 0, false, nil
				}
				return 7, true, nil
			}
			setQuery := func(_ context.Context, pids []int) (map[int]process.ProcessIdentity, error) {
				setCalls++
				if !reflect.DeepEqual(pids, []int{7}) {
					t.Fatalf("unique requested cohort=%v", pids)
				}
				if defect == "absent" {
					return map[int]process.ProcessIdentity{}, nil
				}
				cmd := "synthetic.exe daemon --name a"
				if defect == "arguments" {
					cmd = "synthetic.exe daemon --name wrong"
				}
				seconds := start.Unix()
				if defect == "metadata seconds" {
					seconds++
				}
				return map[int]process.ProcessIdentity{7: {PID: 7, CommandLine: cmd, ExecutablePath: "synthetic.exe", CreationDateUnix: seconds}}, nil
			}
			hash, frozen, owners, err := captureUpgradeGUIPortOwnersWithInputs(context.Background(), stateDir, "synthetic.exe", 100, in, portOwner, setQuery)
			if defect == "none" {
				if err != nil || hash == ([32]byte{}) || len(frozen) != 3 || len(owners) != 3 || owners[0].PID != 7 || owners[1].PID != 7 || owners[2].PID != 0 || owners[0].TaskName != "synthetic-a" || owners[1].TaskName != "synthetic-b" || !owners[0].StartedAt.Equal(start) || setCalls != 1 || nativeCalls != 4 || parentCalls != 2 || userCalls != 2 {
					t.Fatalf("valid capture hash=%x rows=%v owners=%v error=%v counts=%d/%d/%d/%d", hash, frozen, owners, err, setCalls, nativeCalls, parentCalls, userCalls)
				}
			} else if err == nil || hash != ([32]byte{}) || frozen != nil || owners != nil {
				t.Fatalf("defect=%s tuple=%x/%v/%v/%v", defect, hash, frozen, owners, err)
			}
			if defect == "absent" && !errors.Is(err, process.ErrProcessNotFound) {
				t.Fatalf("absence cause=%v", err)
			}
			if defect == "arguments" && (nativeCalls != 2 || parentCalls != 0 || userCalls != 0) {
				t.Fatal("argument-first proof order lost")
			}
			if (defect == "metadata seconds" || defect == "exact generation") && (parentCalls != 0 || userCalls != 0) {
				t.Fatal("generation-first proof order lost")
			}
			if defect == "account unreadable" && !errors.Is(err, os.ErrPermission) {
				t.Fatalf("account cause=%v", err)
			}
		})
	}
}

func TestManagedIdentityCallerCancellationPortReceiver(t *testing.T) {
	start := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
	hold := &upgradeGUIAuthorityJobHold{identity: process.PIDHandleIdentity{PID: 7, ExecutablePath: "synthetic.exe", StartedAt: start}}
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, nil} {
		in := upgradeGUIObservationInputs{
			OwnerPath:        func() (string, error) { t.Fatal("unexpected owner path read"); return "", nil },
			VerifyListener:   func(context.Context, string, int, int) error { t.Fatal("unexpected listener read"); return nil },
			ProbeLeaseUnheld: func(string) (bool, error) { t.Fatal("unexpected lease read"); return false, nil },
			FindGUIs:         func(string) ([]api.ProcessInfo, error) { t.Fatal("unexpected GUI read"); return nil, nil },
			ListProcesses:    func([]string) ([]api.ProcessInfo, error) { t.Fatal("unexpected process list"); return nil, nil },
			HoldTermination:  func(int) (process.HeldPIDGeneration, error) { t.Fatal("unexpected termination hold"); return nil, nil },
			HoldJob:          func(int) (process.HeldPIDJobObservation, error) { t.Fatal("unexpected Job hold"); return nil, nil },
			SIDMatches:       func(int) (bool, error) { t.Fatal("unexpected SID read"); return false, nil },
			ParentPID:        func(int) (int, error) { t.Fatal("unexpected parent read"); return 0, nil },
			SupervisorOwner: func(string) (api.SupervisorLockOwner, error) {
				return api.SupervisorLockOwner{PID: 7, StartedAt: start.Format(time.RFC3339Nano)}, nil
			},
			CommandIdentity: func(context.Context, int) (process.ProcessIdentity, error) {
				return process.ProcessIdentity{PID: 7, ExecutablePath: "synthetic.exe", CommandLine: "synthetic.exe supervise", CreationDateUnix: start.Unix()}, nil
			},
			CurrentIdentity: func(int) (process.PIDHandleIdentity, error) { return hold.identity, nil },
			TaskActions:     func(context.Context, string) ([]upgradeGUITaskAction, error) { return nil, nil },
			PortOwners: func(context.Context, string, string, int) ([32]byte, []windowsProductIntentPortV1, []upgradeGUIPortOwner, error) {
				return [32]byte{2}, nil, nil, cause
			},
		}
		held := &upgradeGUIHeldTopology{inputs: in, Supervisor: hold, SupervisorOwner: api.SupervisorLockOwner{PID: 7, StartedAt: start.Format(time.RFC3339Nano)}, IntentHash: [32]byte{1}, Identity: upgradeGUIIdentitySnapshot{Target: "synthetic.exe", Supervisor: upgradeGUIProcessIdentity{PID: 7, StartedAt: start}}}
		err := recheckUpgradeGUISupervisorAndInputs(context.Background(), held)
		if err == nil || !strings.Contains(err.Error(), "UPGRADE_GUI_IDENTITY_AMBIGUOUS") {
			t.Fatalf("receiver category=%v", err)
		}
		if cause != nil && !errors.Is(err, cause) {
			t.Fatalf("receiver lost cause: %v", err)
		}
		if cause == nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			t.Fatal("value drift recast as cancellation")
		}
		limit, cancel := context.WithCancel(context.Background())
		cancel()
		health := waitUpgradeGUIForwardHealth(limit, func() error { return err })
		if cause != nil && !errors.Is(health, cause) {
			t.Fatalf("enclosing health lost receiver cause: %v", health)
		}
	}
}

func TestManagedIdentityCallerCancellationCapture(t *testing.T) {
	for _, phase := range []string{"port", "image", "set", "generation", "parent", "account"} {
		t.Run(phase, func(t *testing.T) {
			stateDir := withTempStateDir(t)
			writeStateSidecarBytes(t, filepath.Join(stateDir, "supervisor-intent.json"), []byte(`{"version":1,"daemons":[{"task_name":"synthetic","command":"synthetic.exe","args":["daemon"],"port":7}]}`))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := time.Date(2026, 9, 1, 1, 2, 3, 123, time.UTC)
			native, setCalls, parentCalls, userCalls := 0, 0, 0, 0
			in := upgradeGUIObservationInputs{
				CurrentIdentity: func(pid int) (process.PIDHandleIdentity, error) {
					native++
					if (phase == "image" && native == 1) || (phase == "generation" && native == 2) {
						cancel()
					}
					return process.PIDHandleIdentity{PID: pid, ExecutablePath: "synthetic.exe", StartedAt: start}, nil
				},
				ParentPID: func(int) (int, error) {
					parentCalls++
					if phase == "parent" {
						cancel()
					}
					return 100, nil
				},
				SIDMatches: func(int) (bool, error) {
					userCalls++
					if phase == "account" {
						cancel()
					}
					return true, nil
				},
			}
			hash, rows, owners, err := captureUpgradeGUIPortOwnersWithInputs(ctx, stateDir, "synthetic.exe", 100, in,
				func(received context.Context, _ int) (int, bool, error) {
					if received != ctx {
						t.Fatal("caller port context replaced")
					}
					if phase == "port" {
						cancel()
					}
					return 7, true, nil
				},
				func(received context.Context, _ []int) (map[int]process.ProcessIdentity, error) {
					setCalls++
					if received != ctx {
						t.Fatal("caller set context replaced")
					}
					if phase == "set" {
						cancel()
					}
					return map[int]process.ProcessIdentity{7: {PID: 7, CommandLine: "synthetic.exe daemon", ExecutablePath: "synthetic.exe", CreationDateUnix: start.Unix()}}, nil
				})
			if !errors.Is(err, context.Canceled) || hash != ([32]byte{}) || rows != nil || owners != nil {
				t.Fatalf("phase=%s tuple=%x/%v/%v/%v", phase, hash, rows, owners, err)
			}
			if (phase == "port" || phase == "image") && setCalls != 0 {
				t.Fatal("metadata queried after image cancellation")
			}
			if (phase == "set" || phase == "generation") && (parentCalls != 0 || userCalls != 0) {
				t.Fatal("proof continued after cancellation")
			}
			if phase == "parent" && userCalls != 0 {
				t.Fatal("account read after canceled parent")
			}
		})
	}
}

func TestManagedIdentityCallerCancellationRoleHold(t *testing.T) {
	for _, lateSuccess := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		start := time.Date(2026, 9, 1, 1, 2, 3, 0, time.UTC)
		hold := &upgradeGUIAuthorityHold{identity: process.PIDHandleIdentity{PID: 7, ExecutablePath: "synthetic.exe", StartedAt: start}}
		in := upgradeGUIObservationInputs{
			HoldTermination: func(int) (process.HeldPIDGeneration, error) { return hold, nil },
			CurrentIdentity: func(int) (process.PIDHandleIdentity, error) { return hold.identity, nil },
			CommandIdentity: func(received context.Context, _ int) (process.ProcessIdentity, error) {
				if received != ctx {
					t.Fatal("role context replaced")
				}
				if lateSuccess {
					cancel()
					return process.ProcessIdentity{PID: 7, ExecutablePath: "synthetic.exe", CommandLine: "synthetic.exe gui", CreationDateUnix: start.Unix()}, nil
				}
				return process.ProcessIdentity{}, context.Canceled
			},
		}
		_, retained, err := holdUpgradeGUIRole(ctx, 7, "synthetic.exe", start, "gui", in)
		cancel()
		if !errors.Is(err, context.Canceled) || retained != nil || hold.closed != 1 {
			t.Fatalf("role cause=%v retained=%v close=%d", err, retained, hold.closed)
		}
	}
}

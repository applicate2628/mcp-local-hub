//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A backend returning success after cancellation must not publish an identity.
func TestManagedIdentityCallerCancellationScalarLateSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	swapLookupBackend(t, func(context.Context, int) (ProcessIdentity, error) {
		cancel()
		return ProcessIdentity{PID: 7}, nil
	})
	id, err := LookupProcessIdentityContext(ctx, 7)
	if !errors.Is(err, context.Canceled) || id != (ProcessIdentity{}) {
		t.Fatalf("late scalar success: identity=%+v error=%v", id, err)
	}
}

// Explicit opt-in only: the queried cohort consists solely of children started
// by this test, with retained Process handles and synthetic named arguments.
func TestManagedIdentitySetActualProducer(t *testing.T) {
	stage := os.Getenv("MCPHUB_IDENTITY_WIRE_STAGE")
	if stage == "" {
		t.Skip("actual wire probe requires its private creator-owned stage")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pids := make([]int, 0, 2)
	for _, name := range []string{"synthetic-identity-a", "synthetic-identity-b"} {
		cmd := exec.Command(exe, "-test.run=^TestManagedIdentitySyntheticChild$", "--", name)
		cmd.Env = append(os.Environ(), "MCPHUB_IDENTITY_SYNTHETIC_CHILD=1")
		NoConsole(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal("start synthetic cohort", err)
		}
		pids = append(pids, cmd.Process.Pid)
		t.Cleanup(func() {
			if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error("creator-handle cohort stop", err)
			}
			err := cmd.Wait()
			var exited *exec.ExitError
			if err != nil && !errors.As(err, &exited) {
				t.Error("cohort wait", err)
			}
			if cmd.ProcessState == nil {
				t.Error("cohort terminal state missing")
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	queries := 0
	set, err := lookupProcessIdentitiesWithQuery(ctx, pids, func(ctx context.Context, b identityQueryBackend, requested []int) ([]byte, error) {
		queries++
		out, err := queryProcessIdentitySet(ctx, b, requested)
		if err != nil {
			return nil, err
		}
		if b != identityQueryPowerShell {
			return nil, errors.New("actual sample selected WMIC; retain and qualify separately")
		}
		if err := os.WriteFile(filepath.Join(stage, "wire-actual.json"), out, 0600); err != nil {
			return nil, err
		}
		return out, nil
	})
	if err != nil || len(set) != 2 || queries != 1 {
		t.Fatalf("actual creator-cohort wire: count=%d attempts=%d error=%v", len(set), queries, err)
	}
	for _, pid := range pids {
		id, ok := set[pid]
		anchor, err := LookupPIDHandleIdentity(pid)
		if !ok || err != nil || id.CreationDateUnix != anchor.StartedAt.Unix() || id.ExecutablePath != anchor.ExecutablePath || !strings.Contains(id.CommandLine, "synthetic-identity-") {
			t.Fatal("actual wire/native generation/type/argument binding failed")
		}
	}
	t.Log("ACTUAL_WIRE PowerShell array=2 ProcessId=positive-integer Name/CommandLine/ExecutablePath=strings CreationDateUnix=signed-integer UTC-seconds native-second-equality=true attempts=1")
	// Real command cancellation must return a cause and settle its child/streams,
	// rather than turn a failed capability command into an available fallback.
	limit, stop := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stop()
	late, err := LookupProcessIdentitiesContext(limit, pids)
	if !errors.Is(err, context.DeadlineExceeded) || late != nil {
		t.Fatalf("actual canceled acquisition map=%v error=%v", late, err)
	}
	t.Log("ACTUAL_CANCEL DeadlineExceeded nil-map; creator cohort cleanup registered")
}

func TestManagedIdentitySyntheticChild(t *testing.T) {
	if os.Getenv("MCPHUB_IDENTITY_SYNTHETIC_CHILD") != "1" {
		t.Skip("creator-owned synthetic cohort child only")
	}
	time.Sleep(20 * time.Second)
}

func TestManagedIdentitySetWireAndDispatch(t *testing.T) {
	valid := `[{"ProcessId":7,"Name":"synthetic.exe","CommandLine":"synthetic --flag","ExecutablePath":"synthetic.exe","CreationDateUnix":1700000000}]`
	for _, tc := range []struct {
		name, wire string
		want       int
		fail       bool
	}{
		{"empty array", `[]`, 0, false},
		{"one requested and one absent", valid, 1, false},
		{"many", strings.TrimSuffix(valid, "]") + `,{"ProcessId":8,"Name":"synthetic.exe","CommandLine":"synthetic --other","ExecutablePath":"synthetic.exe","CreationDateUnix":0}]`, 2, false},
		{"object cardinality", strings.Trim(valid, "[]"), 0, true},
		{"null", `null`, 0, true},
		{"missing creation", strings.Replace(valid, `,"CreationDateUnix":1700000000`, "", 1), 0, true},
		{"null string", strings.Replace(valid, `"Name":"synthetic.exe"`, `"Name":null`, 1), 0, true},
		{"wrong PID type", strings.Replace(valid, `"ProcessId":7`, `"ProcessId":"7"`, 1), 0, true},
		{"unrequested", strings.Replace(valid, `"ProcessId":7`, `"ProcessId":9`, 1), 0, true},
		{"zero PID", strings.Replace(valid, `"ProcessId":7`, `"ProcessId":0`, 1), 0, true},
		{"duplicate PID", strings.TrimSuffix(valid, "]") + "," + strings.TrimPrefix(valid, "["), 0, true},
		{"malformed", strings.TrimSuffix(valid, "]"), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, err := parsePowerShellIdentitySet([]byte(tc.wire), map[int]struct{}{7: {}, 8: {}})
			if (err != nil) != tc.fail || (tc.fail && set != nil) || (!tc.fail && len(set) != tc.want) {
				t.Fatalf("set=%+v error=%v", set, err)
			}
			if !tc.fail && tc.want > 0 && set[7].CreationDateUnix != 1700000000 {
				t.Fatal("UTC seconds changed")
			}
		})
	}
	probes, queries, fallbackPaths := 0, 0, 0
	swapProbePowerShellCLM(t, func(context.Context) (bool, error) { probes++; return true, nil })
	swapWmicPathLookup(t, func() error { fallbackPaths++; return nil })
	query := func(_ context.Context, backend identityQueryBackend, pids []int) ([]byte, error) {
		queries++
		if backend != identityQueryPowerShell || len(pids) != 2 {
			t.Fatal("wrong selected backend/requested cohort")
		}
		if queries < 3 {
			return nil, errors.New("collector transient")
		}
		return []byte(valid), nil
	}
	set, err := lookupProcessIdentitiesWithQuery(context.Background(), []int{7, 8}, query)
	if err != nil || len(set) != 1 || probes != 1 || queries != 3 || fallbackPaths != 0 {
		t.Fatalf("set=%v err=%v probes=%d queries=%d fallback=%d", set, err, probes, queries, fallbackPaths)
	}
	second, err := lookupProcessIdentitiesWithQuery(context.Background(), []int{7, 8}, func(_ context.Context, _ identityQueryBackend, _ []int) ([]byte, error) {
		queries++
		return []byte(strings.Replace(valid, "synthetic --flag", "synthetic --fresh", 1)), nil
	})
	if err != nil || probes != 2 || queries != 4 || second[7].CommandLine != "synthetic --fresh" || set[7].CommandLine != "synthetic --flag" {
		t.Fatal("query selection/result reused across operations")
	}
	for _, pids := range [][]int{nil, {}, {0}, {-1}, {7, 7}} {
		before := queries + probes
		set, err := lookupProcessIdentitiesWithQuery(context.Background(), pids, query)
		if len(pids) == 0 {
			if err != nil || len(set) != 0 {
				t.Fatal("empty set")
			}
		} else if err == nil || set != nil {
			t.Fatal("invalid requested set accepted")
		}
		if queries+probes != before {
			t.Fatal("empty/invalid request executed command")
		}
	}
}

func TestManagedIdentitySetWireAndDispatchWmicDates(t *testing.T) {
	for _, tc := range []struct {
		raw         string
		scalar, set int64
		fail        bool
	}{
		{"20231115011320.000000+180", 1700010800, 1700000000, false},
		{"20231114171320.000000-300", 1699982000, 1700000000, false},
		{"20231114221320.123456+000", 1700000000, 1700000000, false},
		{"20231114221320.******+***", 1700000000, 0, true},
		{"20231114221320.123456+xxx", 1700000000, 0, true},
		{"20231314221320.000000+000", 0, 0, true},
		{"", 0, 0, true},
	} {
		scalar, err := normalizeWmicCreationDate(tc.raw, wmicCreationLegacyScalar)
		if err != nil || scalar != tc.scalar {
			t.Fatalf("legacy projection %q=%d/%v", tc.raw, scalar, err)
		}
		set, err := normalizeWmicCreationDate(tc.raw, wmicCreationUTCSet)
		if (err != nil) != tc.fail || set != tc.set {
			t.Fatalf("set projection %q=%d/%v", tc.raw, set, err)
		}
	}
	for _, tc := range []struct {
		wire string
		fail bool
	}{
		{"Node,ProcessId,CommandLine,CreationDate,ExecutablePath,Name\nsynthetic,7,synthetic --flag,20231115011320.000000+180,synthetic.exe,synthetic.exe\n", false},
		{"Node,Name,CreationDate,ExecutablePath,CommandLine,ProcessId\nsynthetic,synthetic.exe,20231115011320.000000+180,synthetic.exe,synthetic --flag,7\n", false},
		{"Node,ProcessId,CommandLine,CreationDate,ExecutablePath,Name\nsynthetic,7,synthetic --flag,20231115011320.000000+180,synthetic.exe,synthetic.exe\nsynthetic,7,synthetic --flag,20231115011320.000000+180,synthetic.exe,synthetic.exe\n", true},
		{"Node,ProcessId,CommandLine,CreationDate,ExecutablePath,Name\nsynthetic,7,synthetic --flag,20231115011320.******+***,synthetic.exe,synthetic.exe\n", true},
		{"Node,ProcessId,CommandLine,CreationDate,ExecutablePath,Name\n", false},
		{"", true},
		{"Node,Name\nsynthetic,synthetic.exe\n", true},
	} {
		set, err := parseWmicIdentitySet([]byte(tc.wire), map[int]struct{}{7: {}})
		if (err != nil) != tc.fail || (tc.fail && set != nil) {
			t.Fatalf("WMIC set=%v error=%v", set, err)
		}
		if !tc.fail && len(set) > 0 && set[7].CreationDateUnix != 1700000000 {
			t.Fatal("WMIC offset not normalized")
		}
	}
}

func TestManagedIdentityCallerCancellationSet(t *testing.T) {
	for _, phase := range []string{"capability", "actual", "backoff"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			queries := 0
			swapProbePowerShellCLM(t, func(context.Context) (bool, error) {
				if phase == "capability" {
					cancel()
					return false, nil
				}
				return true, nil
			})
			swapWmicPathLookup(t, func() error { t.Fatal("cancellation fallback"); return nil })
			if phase == "backoff" {
				time.AfterFunc(10*time.Millisecond, cancel)
			}
			set, err := lookupProcessIdentitiesWithQuery(ctx, []int{7}, func(context.Context, identityQueryBackend, []int) ([]byte, error) {
				queries++
				if phase == "backoff" {
					return nil, fmt.Errorf("collector transient")
				}
				cancel()
				return []byte(`[]`), nil
			})
			if !errors.Is(err, context.Canceled) || set != nil || queries > 1 {
				t.Fatalf("phase=%s set=%v error=%v queries=%d", phase, set, err, queries)
			}
		})
	}
}

// Capability cancellation is not the capability-false WMIC branch.
func TestManagedIdentityCallerCancellationCapability(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	swapProbePowerShellCLM(t, func(context.Context) (bool, error) {
		cancel()
		return false, nil
	})
	fallbacks := 0
	swapWmicPathLookup(t, func() error { fallbacks++; return nil })
	swapLookupViaWmic(t, func(context.Context, int) (ProcessIdentity, error) {
		fallbacks++
		return ProcessIdentity{PID: 7}, nil
	})
	id, err := realLookupBackend(ctx, 7)
	if !errors.Is(err, context.Canceled) || id != (ProcessIdentity{}) || fallbacks != 0 {
		t.Fatalf("canceled capability: identity=%+v error=%v fallback=%d", id, err, fallbacks)
	}
}

//go:build windows

package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
)

func withCOMRunner(t *testing.T, fn func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error)) {
	t.Helper()
	previous := schedulerCOMRun
	schedulerCOMRun = fn
	t.Cleanup(func() { schedulerCOMRun = previous })
}

func TestSchedulerCOMTypedOutcomesIgnoreHostileLocalizedText(t *testing.T) {
	for _, hostile := range []string{"ERROR: The system cannot find the file specified.", "ОШИБКА: Не удается найти указанный файл."} {
		withCOMRunner(t, func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error) {
			return schedulerCOMResponse{Kind: "task_absent", Phase: hostile}, nil
		})
		_, err := schedulerCOM(context.Background(), schedulerCOMRequest{Operation: "status", Name: `\mcp-local-hub-test`})
		if !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("hostile=%q err=%v", hostile, err)
		}
	}
}

func TestSchedulerCOMMapsTypedAccessCorruptAndUnavailable(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want error
	}{{"permission_denied", ErrPermissionDenied}, {"task_corrupt", ErrTaskCorrupt}, {"scheduler_unavailable", ErrUnavailable}} {
		t.Run(tc.kind, func(t *testing.T) {
			withCOMRunner(t, func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error) {
				return schedulerCOMResponse{Kind: tc.kind}, nil
			})
			_, err := schedulerCOM(context.Background(), schedulerCOMRequest{Operation: "status", Name: `\t`})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
		})
	}
}

func TestSchedulerCOMPreservesRunnerErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
		{"bridge", errors.New("bridge lost")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCOMRunner(t, func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error) {
				return schedulerCOMResponse{OK: true, Tasks: []schedulerCOMTask{{Name: "partial"}}}, fmt.Errorf("runner: %w", tc.cause)
			})
			response, err := schedulerCOM(context.Background(), schedulerCOMRequest{Operation: "list"})
			if !errors.Is(err, ErrUnavailable) || !errors.Is(err, tc.cause) {
				t.Fatalf("runner error identity lost: err=%v want ErrUnavailable and %v", err, tc.cause)
			}
			if response.OK || response.Task != nil || response.Tasks != nil {
				t.Fatalf("runner error returned partial response: %+v", response)
			}
		})
	}
}

func TestWindowsListContextPreservesStoppedContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"canceled", context.Canceled},
		{"deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ctx context.Context
			var cancel context.CancelFunc
			if tc.want == context.Canceled {
				ctx, cancel = context.WithCancel(context.Background())
				cancel()
			} else {
				ctx, cancel = context.WithDeadline(context.Background(), time.Unix(1, 0))
			}
			defer cancel()
			if ctx.Err() != tc.want {
				t.Fatalf("context fixture: got %v want %v", ctx.Err(), tc.want)
			}
			// Keep the production runner: the stopped context prevents process start.
			w := &windowsScheduler{}
			tasks, err := w.ListContext(ctx, "mcp-local-hub-canceled-fixture")
			if !errors.Is(err, ErrUnavailable) || !errors.Is(err, tc.want) {
				t.Fatalf("context error identity lost: err=%v want ErrUnavailable and %v", err, tc.want)
			}
			if tasks != nil {
				t.Fatalf("stopped context returned tasks: %+v", tasks)
			}
		})
	}
}

func TestTaskStatusFromCOMUsesNumericStateOnly(t *testing.T) {
	var lastResult int32
	for _, name := range []string{"English running", "Выполняется"} {
		status, err := taskStatusFromCOM(schedulerCOMTask{Name: name, State: 4, LastResult: &lastResult})
		if err != nil || status.RuntimeState != TaskRuntimeRunning {
			t.Fatalf("name=%q status=%#v err=%v", name, status, err)
		}
	}
	if _, err := taskStatusFromCOM(schedulerCOMTask{Name: "x", State: 99}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestSchedulerCOMScriptSerializesNegativeHRESULT(t *testing.T) {
	fixture := strings.Replace(schedulerCOMUTF8Preamble+schedulerCOMScript,
		`$s=New-Object -ComObject 'Schedule.Service';$s.Connect();`,
		`throw [System.Runtime.InteropServices.COMException]::new('fixture',-2147024773);`, 1)
	ctx, cancel := context.WithTimeout(context.Background(), schedulerCOMDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", fixture)
	process.NoConsole(cmd)
	cmd.Stdin = strings.NewReader(`{"operation":"status","name":"\\fixture"}`)
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("negative HRESULT fixture did not produce JSON: %v", err)
	}
	var response schedulerCOMResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("negative HRESULT fixture JSON=%q err=%v", raw, err)
	}
	if response.OK || response.Kind != "scheduler_unavailable" || response.Phase != "com" || response.HRESULT != 0x8007007B {
		t.Fatalf("negative HRESULT fixture response=%+v", response)
	}
	withCOMRunner(t, func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error) {
		return response, nil
	})
	if _, err := schedulerCOM(context.Background(), schedulerCOMRequest{Operation: "status", Name: `\fixture`}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("typed unavailable mapping error=%v", err)
	}
}

func TestSchedulerCOMScriptUTF8ConsoleRoundTripsUnicodeTaskJSON(t *testing.T) {
	prefix, _, found := strings.Cut(schedulerCOMUTF8Preamble+schedulerCOMScript, "try{")
	if !found {
		t.Fatal("scheduler COM script has no try boundary")
	}
	fixture := prefix + `try{[pscustomobject]@{ok=$true;task=[pscustomobject]@{name=[string]$r.name;state=4;xml='<Task><RegistrationInfo><Description>plan §2531 Привет</Description></RegistrationInfo></Task>';owner=''}}|ConvertTo-Json -Compress -Depth 4;exit}catch{throw}`
	request := schedulerCOMRequest{Operation: "status", Name: `\mcp-local-hub-§-Кириллица`}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), schedulerCOMDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", fixture)
	process.NoConsole(cmd)
	cmd.Stdin = strings.NewReader(string(raw))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("unicode fixture exit: %v", err)
	}
	var response schedulerCOMResponse
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatalf("unicode fixture JSON=%q err=%v", out, err)
	}
	if response.Task == nil || response.Task.Name != request.Name || !strings.Contains(response.Task.XML, "§2531 Привет") {
		t.Fatalf("unicode fixture response=%+v", response)
	}
}

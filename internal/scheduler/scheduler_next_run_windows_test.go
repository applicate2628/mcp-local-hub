//go:build windows

package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestSchedulerCOMNextRunProjection(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wire        string
		want        string
		state       TaskRuntimeState
		unavailable bool
	}{
		{"future", `{"state":3,"last_result":0,"next_run":"2030-12-31T03:04:05.0000000"}`, "2030-12-31T03:04:05.0000000", TaskRuntimeReady, false},
		{"observed-none", `{"state":3,"last_result":0,"next_run":""}`, "", TaskRuntimeReady, false},
		{"disabled-with-forecast", `{"state":1,"last_result":0,"next_run":"2030-12-31T03:04:05.0000000"}`, "2030-12-31T03:04:05.0000000", TaskRuntimeDisabled, false},
		{"missing", `{"state":3,"last_result":0}`, "", TaskRuntimeUnknown, true},
		{"null", `{"state":3,"last_result":0,"next_run":null}`, "", TaskRuntimeUnknown, true},
		{"malformed", `{"state":3,"last_result":0,"next_run":"31.12.2030 03:04"}`, "", TaskRuntimeUnknown, true},
		{"false-utc", `{"state":3,"last_result":0,"next_run":"2030-12-31T03:04:05Z"}`, "", TaskRuntimeUnknown, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var wire schedulerCOMTask
			if err := json.Unmarshal([]byte(tc.wire), &wire); err != nil {
				t.Fatal(err)
			}
			wire.Name = "fixture-next-run"
			status, err := taskStatusFromCOM(wire)
			if tc.unavailable {
				if !errors.Is(err, ErrUnavailable) || status != (TaskStatus{}) {
					t.Fatalf("unknown observation became a task snapshot: status=%+v err=%v", status, err)
				}
				return
			}
			if err != nil || status.NextRun != tc.want || status.RuntimeState != tc.state {
				t.Fatalf("status=%+v err=%v", status, err)
			}
		})
	}
}

// The complete production producer runs against local objects. Replacing its
// single COM connection prevents any observation or mutation of installed tasks.
func TestSchedulerCOMNextRunScript(t *testing.T) {
	const connect = `$s=New-Object -ComObject 'Schedule.Service';$s.Connect();$f=$s.GetFolder('\');`
	if strings.Count(schedulerCOMScript, connect) != 1 {
		t.Fatal("COM fixture boundary changed")
	}
	const objects = `
[Threading.Thread]::CurrentThread.CurrentCulture=[Globalization.CultureInfo]::GetCultureInfo('ru-RU')
$future=[pscustomobject]@{Name='fixture-future';State=3;LastTaskResult=0;NextRunTime=[DateTime]::new(2030,12,31,3,4,5,[DateTimeKind]::Unspecified);Xml='<Task/>';Definition=[pscustomobject]@{Principal=[pscustomobject]@{UserId='S-1-5-21-1'}}}
$none=[pscustomobject]@{Name='fixture-none';State=3;LastTaskResult=0;NextRunTime=[DateTime]::FromOADate(0);Xml='<Task/>';Definition=$future.Definition}
$disabled=[pscustomobject]@{Name='fixture-disabled';State=1;LastTaskResult=0;NextRunTime=$future.NextRunTime;Xml='<Task/>';Definition=$future.Definition}
$f=[pscustomobject]@{}
`
	for _, tc := range []struct {
		name        string
		request     schedulerCOMRequest
		population  string
		want        []string
		unavailable bool
	}{
		{"status", schedulerCOMRequest{Operation: "status", Name: `\fixture-future`}, `$script:tasks=@($future);`, []string{"2030-12-31T03:04:05.0000000"}, false},
		{"list", schedulerCOMRequest{Operation: "list", Prefix: "fixture-"}, `$script:tasks=@($future,$none,$disabled);`, []string{"2030-12-31T03:04:05.0000000", "", "2030-12-31T03:04:05.0000000"}, false},
		{"unknown-after-valid-row", schedulerCOMRequest{Operation: "list", Prefix: "fixture-"}, `$none.PSObject.Properties.Remove('NextRunTime');$script:tasks=@($future,$none);`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := objects + tc.population + `$f|Add-Member ScriptMethod GetTasks {param($flags);return @($script:tasks)};`
			script := schedulerCOMUTF8Preamble + strings.Replace(schedulerCOMScript, connect, fake, 1)
			input, err := json.Marshal(tc.request)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), schedulerCOMDeadline)
			defer cancel()
			result, err := runSchedulerCommand(ctx, exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script), input)
			if err != nil {
				t.Fatalf("isolated producer fixture: %v", err)
			}
			var response schedulerCOMResponse
			if err := json.Unmarshal(result.Stdout.Prefix, &response); err != nil {
				t.Fatal(err)
			}
			if tc.unavailable {
				if response.OK || response.Kind != "scheduler_unavailable" || response.Task != nil || len(response.Tasks) != 0 {
					t.Fatalf("unknown next run returned partial task data: %+v", response)
				}
				return
			}
			tasks := response.Tasks
			if tc.request.Operation == "status" && response.Task != nil {
				tasks = []schedulerCOMTask{*response.Task}
			}
			if !response.OK || len(tasks) != len(tc.want) {
				t.Fatalf("response=%+v", response)
			}
			for i, task := range tasks {
				status, err := taskStatusFromCOM(task)
				if err != nil || status.NextRun != tc.want[i] {
					t.Fatalf("row=%d status=%+v err=%v", i, status, err)
				}
				if task.Name == "fixture-disabled" && status.RuntimeState != TaskRuntimeDisabled {
					t.Fatal("forecast enabled a disabled task")
				}
			}
		})
	}
}

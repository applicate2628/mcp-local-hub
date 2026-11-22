//go:build windows

package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/process"
)

// Decode through the bridge contract rather than naming its private result
// field, so these regressions also compile against the pre-fix implementation.
func decodeLastResultCOMTask(t *testing.T, raw string) schedulerCOMTask {
	t.Helper()
	var task schedulerCOMTask
	if err := json.Unmarshal([]byte(raw), &task); err != nil {
		t.Fatalf("task JSON=%s: %v", raw, err)
	}
	return task
}

func TestSchedulerCOMLastResultMapping(t *testing.T) {
	for _, tc := range []struct {
		name    string
		state   int
		result  int32
		runtime TaskRuntimeState
	}{
		{"success", 3, 0, TaskRuntimeReady},
		{"application-error-while-running", 4, 87, TaskRuntimeRunning},
		{"scheduler-running-code", 3, 0x41301, TaskRuntimeReady},
		{"scheduler-disabled-code", 1, 0x41302, TaskRuntimeDisabled},
		{"scheduler-never-run-code", 3, 0x41303, TaskRuntimeReady},
		{"signed-hresult", 3, -2147024894, TaskRuntimeReady}, // 0x80070002
		{"signed-minimum", 2, -2147483648, TaskRuntimeQueued},
		{"signed-maximum", 3, 2147483647, TaskRuntimeReady},
		{"minus-one", 3, -1, TaskRuntimeReady},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := decodeLastResultCOMTask(t, fmt.Sprintf(
				`{"name":"fixture","state":%d,"owner":"DOMAIN\\alice","last_result":%d}`,
				tc.state, tc.result))
			status, err := taskStatusFromCOM(task)
			if err != nil {
				t.Fatal(err)
			}
			if status.LastResult != int(tc.result) || status.RuntimeState != tc.runtime || status.Name != `\fixture` || status.Owner != `DOMAIN\alice` {
				t.Fatalf("status=%+v, want result=%d runtime=%v", status, tc.result, tc.runtime)
			}
		})
	}
}

func TestSchedulerCOMLastResultRejectsMissingOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
	}{
		{"missing-result", `{"name":"fixture","state":3}`},
		{"null-result", `{"name":"fixture","state":3,"last_result":null}`},
		{"unknown-state-with-result", `{"name":"fixture","state":99,"last_result":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, err := taskStatusFromCOM(decodeLastResultCOMTask(t, tc.json)); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("status=%+v error=%v, want ErrUnavailable", status, err)
			}
		})
	}
}

func TestSchedulerCOMLastResultRejectsInvalidJSON(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"numeric-string", `"0"`},
		{"boolean", `true`},
		{"object", `{}`},
		{"fraction", `1.5`},
		{"above-signed-maximum", `2147483648`},
		{"below-signed-minimum", `-2147483649`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"ok":true,"task":{"name":"fixture","state":3,"last_result":` + tc.value + `}}`
			var response schedulerCOMResponse
			if err := json.Unmarshal([]byte(raw), &response); err == nil {
				t.Fatalf("invalid last_result accepted: %s", raw)
			}
		})
	}
}

func TestSchedulerCOMLastResultPublicQueries(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation string
		json      string
		want      []int
		wantError bool
	}{
		{
			name: "status-preserves-signed-result", operation: "status",
			json: `{"ok":true,"task":{"name":"fixture","state":3,"owner":"DOMAIN\\alice","last_result":-2147024894}}`,
			want: []int{-2147024894},
		},
		{
			name: "list-preserves-each-result", operation: "list",
			json: `{"ok":true,"tasks":[{"name":"fixture-zero","state":3,"owner":"S-1-5-21-1","last_result":0},{"name":"fixture-error","state":4,"owner":"S-1-5-21-1","last_result":87},{"name":"fixture-never-run","state":3,"owner":"S-1-5-21-1","last_result":267011}]}`,
			want: []int{0, 87, 267011},
		},
		{
			name: "status-rejects-missing-result", operation: "status",
			json:      `{"ok":true,"task":{"name":"fixture","state":3,"owner":"DOMAIN\\alice"}}`,
			wantError: true,
		},
		{
			name: "list-rejects-null-without-partial-results", operation: "list",
			json:      `{"ok":true,"tasks":[{"name":"fixture-good","state":3,"owner":"S-1-5-21-1","last_result":87},{"name":"fixture-null","state":3,"owner":"S-1-5-21-1","last_result":null}]}`,
			wantError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response schedulerCOMResponse
			if err := json.Unmarshal([]byte(tc.json), &response); err != nil {
				t.Fatal(err)
			}
			withCOMRunner(t, func(_ context.Context, request schedulerCOMRequest) (schedulerCOMResponse, error) {
				if request.Operation != tc.operation || (tc.operation == "status" && request.Name != `\fixture`) || (tc.operation == "list" && request.Prefix != "fixture-") {
					t.Fatalf("unexpected request=%+v", request)
				}
				return response, nil
			})
			scheduler := &windowsScheduler{username: "S-1-5-21-1"}
			var statuses []TaskStatus
			var err error
			if tc.operation == "status" {
				var status TaskStatus
				status, err = scheduler.Status(`\fixture`)
				if err == nil {
					statuses = []TaskStatus{status}
				}
			} else {
				statuses, err = scheduler.List("fixture-")
			}
			if tc.wantError {
				if !errors.Is(err, ErrUnavailable) || len(statuses) != 0 {
					t.Fatalf("statuses=%+v error=%v, want ErrUnavailable without results", statuses, err)
				}
				return
			}
			if err != nil || len(statuses) != len(tc.want) {
				t.Fatalf("statuses=%+v error=%v, want %d rows", statuses, err, len(tc.want))
			}
			for i, want := range tc.want {
				if statuses[i].LastResult != want {
					t.Fatalf("row %d result=%d, want %d", i, statuses[i].LastResult, want)
				}
			}
		})
	}
}

type lastResultFixtureTask struct {
	Name  string `json:"name"`
	Value int32  `json:"value"`
	Mode  string `json:"mode"`
}

const lastResultFixtureXML = `<Task><RegistrationInfo><Description>last-result-fixture</Description></RegistrationInfo></Task>`

// Exercise the complete production script against object fixtures. This checks
// PowerShell serialization and error propagation, not native scheduler behavior.
func TestSchedulerCOMLastResultScript(t *testing.T) {
	systemRoot := os.Getenv("SystemRoot")
	if !filepath.IsAbs(systemRoot) {
		t.Fatal("qualified absolute SystemRoot required")
	}
	powerShell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if info, err := os.Stat(powerShell); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("qualified PowerShell executable %q: %v", powerShell, err)
	}
	for _, tc := range []struct {
		name      string
		operation string
		tasks     []lastResultFixtureTask
		wantError bool
	}{
		{"status-zero", "status", []lastResultFixtureTask{{"fixture-zero", 0, ""}}, false},
		{"status-signed-result", "status", []lastResultFixtureTask{{"fixture-error", -2147024894, ""}}, false},
		{"list-results", "list", []lastResultFixtureTask{
			{"fixture-zero", 0, ""},
			{"fixture-exit", 87, ""},
			{"fixture-never-run", 0x41303, ""},
			{"fixture-hresult", -2147024894, ""},
			{"fixture-minimum", -2147483648, ""},
			{"fixture-maximum", 2147483647, ""},
		}, false},
		{"status-null", "status", []lastResultFixtureTask{{"fixture-null", 0, "null"}}, true},
		{"status-missing", "status", []lastResultFixtureTask{{"fixture-missing", 0, "missing"}}, true},
		{"status-getter-error", "status", []lastResultFixtureTask{{"fixture-throw", 0, "throw"}}, true},
		{"list-null-after-result", "list", []lastResultFixtureTask{{"fixture-good", 87, ""}, {"fixture-null", 0, "null"}}, true},
		{"list-missing-after-result", "list", []lastResultFixtureTask{{"fixture-good", 87, ""}, {"fixture-missing", 0, "missing"}}, true},
		{"list-getter-error-after-result", "list", []lastResultFixtureTask{{"fixture-good", 87, ""}, {"fixture-throw", 0, "throw"}}, true},
		{"export-does-not-read-result", "export", []lastResultFixtureTask{{"fixture-export", 0, "throw"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := schedulerCOMRequest{Operation: tc.operation, Name: `\` + tc.tasks[0].Name}
			if tc.operation == "list" {
				request.Name = ""
				request.Prefix = "fixture-"
			}
			response, reads := runLastResultCOMScriptFixture(t, powerShell, request, tc.tasks)
			if tc.operation != "export" {
				for _, task := range tc.tasks {
					if task.Mode != "missing" && !strings.Contains(reads, "fixture-last-result:"+task.Name) {
						t.Fatalf("LastTaskResult was not read for %q: stderr=%q", task.Name, reads)
					}
				}
			}
			if tc.wantError {
				if response.OK || response.Kind != "scheduler_unavailable" || response.Phase != "com" || response.Task != nil || len(response.Tasks) != 0 {
					t.Fatalf("response=%+v, want unavailable without partial task data", response)
				}
				return
			}
			if !response.OK {
				t.Fatalf("response=%+v, want success", response)
			}
			if tc.operation == "export" {
				if strings.Contains(reads, "fixture-last-result:") {
					t.Fatalf("export read LastTaskResult: %q", reads)
				}
				withCOMRunner(t, func(_ context.Context, wire schedulerCOMRequest) (schedulerCOMResponse, error) {
					if wire.Operation != "export" || wire.Name != request.Name {
						t.Fatalf("unexpected export request=%+v", wire)
					}
					return response, nil
				})
				xml, err := (&windowsScheduler{}).ExportXML(request.Name)
				if err != nil || string(xml) != lastResultFixtureXML {
					t.Fatalf("export XML=%q error=%v", xml, err)
				}
				return
			}
			tasks := response.Tasks
			if tc.operation == "status" {
				if response.Task == nil {
					t.Fatal("missing status task")
				}
				tasks = []schedulerCOMTask{*response.Task}
			}
			if len(tasks) != len(tc.tasks) {
				t.Fatalf("got %d task rows, want %d", len(tasks), len(tc.tasks))
			}
			for i, task := range tasks {
				status, err := taskStatusFromCOM(task)
				if err != nil || status.Name != `\`+tc.tasks[i].Name || status.LastResult != int(tc.tasks[i].Value) {
					t.Fatalf("row %d status=%+v error=%v, want result=%d", i, status, err, tc.tasks[i].Value)
				}
			}
		})
	}
}

func runLastResultCOMScriptFixture(t *testing.T, powerShell string, request schedulerCOMRequest, tasks []lastResultFixtureTask) (schedulerCOMResponse, string) {
	t.Helper()
	const serviceConnection = `$s=New-Object -ComObject 'Schedule.Service';$s.Connect();`
	if strings.Count(schedulerCOMScript, serviceConnection) != 1 {
		t.Fatal("scheduler COM script must have exactly one service connection to replace")
	}
	// No fixture object invokes a native service or task. The production
	// property getter, row construction, and catch block remain unchanged.
	const fixtureBody = `
$script:fixtureTasks=@(foreach($row in $script:lastResultFixture.tasks){
 $task=[pscustomobject]@{Name=[string]$row.name;State=3;Xml=[string]$script:lastResultFixture.xml;Definition=[pscustomobject]@{Principal=[pscustomobject]@{UserId='S-1-5-21-1'}};FixtureResult=$row.value;FixtureMode=[string]$row.mode}
 if($row.mode -cne 'missing'){
  $task|Add-Member ScriptProperty LastTaskResult {
   [Console]::Error.WriteLine('fixture-last-result:'+ $this.Name)
   if($this.FixtureMode -ceq 'throw'){throw 'fixture-last-result-failure'}
   if($this.FixtureMode -ceq 'null'){return $null}
   return [int]$this.FixtureResult
  }
 }
 $task
})
$script:fixtureFolder=[pscustomobject]@{}
$script:fixtureFolder|Add-Member ScriptMethod GetTasks {param($flags);return @($script:fixtureTasks)}
$s=[pscustomobject]@{}
$s|Add-Member ScriptMethod GetFolder {param($name);if($name -cne '\'){throw 'fixture-root-folder-required'};return $script:fixtureFolder};
`
	fixtureJSON, err := json.Marshal(map[string]any{"tasks": tasks, "xml": lastResultFixtureXML})
	if err != nil {
		t.Fatal(err)
	}
	fixture := `$script:lastResultFixture='` + strings.ReplaceAll(string(fixtureJSON), "'", "''") + `'|ConvertFrom-Json;` + fixtureBody
	code := strings.Replace(schedulerCOMUTF8Preamble+schedulerCOMScript, serviceConnection, fixture, 1)
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), schedulerCOMDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, powerShell, "-NoProfile", "-NonInteractive", "-Command", code)
	process.NoConsole(cmd)
	cmd.Stdin = bytes.NewReader(raw)
	var reads bytes.Buffer
	cmd.Stderr = &reads
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fixture PowerShell: %v, stderr=%q", err, reads.String())
	}
	var response schedulerCOMResponse
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatalf("fixture JSON=%q error=%v", out, err)
	}
	return response, reads.String()
}

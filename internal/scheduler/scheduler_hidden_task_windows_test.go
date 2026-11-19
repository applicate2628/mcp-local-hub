//go:build windows

package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/process"
)

// Run the production script with a flag-aware collection. Only the service
// connection is replaced; no fixture method invokes the OS scheduler.
func TestSchedulerCOMHiddenTaskLookup(t *testing.T) {
	systemRoot := os.Getenv("SystemRoot")
	if !filepath.IsAbs(systemRoot) {
		t.Fatal("qualified absolute SystemRoot required")
	}
	powerShell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if info, err := os.Stat(powerShell); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("qualified PowerShell executable %q: %v", powerShell, err)
	}
	const serviceConnection = `$s=New-Object -ComObject 'Schedule.Service';$s.Connect();`
	if strings.Count(schedulerCOMScript, serviceConnection) != 1 {
		t.Fatal("scheduler COM script must have exactly one service connection to replace")
	}
	const visibleName = `\fixture-visible`
	const hiddenName = `\fixture-hidden`
	const missingName = `\fixture-hidden-sibling`
	const visibleXML = `<Task><Settings><Hidden>false</Hidden></Settings></Task>`
	const hiddenXML = `<Task><Settings><Hidden>true</Hidden></Settings><RegistrationInfo><Description>Привет §</Description></RegistrationInfo></Task>`
	const fixtureBody = `
$visible=[pscustomobject]@{Name='fixture-visible';Hidden=$false;State=3;Xml=[string]$script:fixture.visible_xml;Definition=[pscustomobject]@{Principal=[pscustomobject]@{UserId='S-1-5-21-1'}}}
$hidden=[pscustomobject]@{Name='fixture-hidden';Hidden=$true;State=[int]$script:fixture.hidden_state;Xml=[string]$script:fixture.hidden_xml;Definition=[pscustomobject]@{Principal=[pscustomobject]@{UserId='S-1-5-21-1'}}}
foreach($task in @($visible,$hidden)){$task|Add-Member ScriptMethod Stop {param($flags);[Console]::Error.WriteLine('fixture-stop:'+ $this.Name)}}
$script:fixtureTasks=@($visible,$hidden)
$script:fixtureFolder=[pscustomobject]@{}
$script:fixtureFolder|Add-Member ScriptMethod GetTasks {param($flags);if($script:fixture.fail_enumeration){throw 'fixture-enumeration-error'};return @($script:fixtureTasks|Where-Object {!$_.Hidden -or ($flags -band 1) -ne 0})}
$script:fixtureFolder|Add-Member ScriptMethod DeleteTask {param($name,$flags);[Console]::Error.WriteLine('fixture-delete:'+ $name)}
$s=[pscustomobject]@{}
$s|Add-Member ScriptMethod GetFolder {param($name);if($name -cne '\'){throw 'fixture-root-folder-required'};return $script:fixtureFolder};
`
	for _, tc := range []struct {
		name, operation, taskName string
		hiddenState               int
		failEnumeration           bool
		wantKind, wantAction      string
	}{
		{"visible-status", "status", visibleName, 4, false, "", ""},
		{"hidden-status", "status", hiddenName, 4, false, "", ""},
		{"hidden-export", "export", hiddenName, 4, false, "", ""},
		{"hidden-stop-running", "stop", hiddenName, 4, false, "", "fixture-stop:fixture-hidden"},
		{"hidden-stop-ready", "stop", hiddenName, 3, false, "task_not_running", ""},
		{"hidden-delete", "delete", hiddenName, 4, false, "", "fixture-delete:fixture-hidden"},
		{"absent-status", "status", missingName, 4, false, "task_absent", ""},
		{"absent-export", "export", missingName, 4, false, "task_absent", ""},
		{"absent-stop", "stop", missingName, 4, false, "task_absent", ""},
		{"absent-delete", "delete", missingName, 4, false, "task_absent", ""},
		{"list-remains-visible-only", "list", "", 4, false, "", ""},
		{"enumeration-error", "export", hiddenName, 4, true, "scheduler_unavailable", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtureJSON, err := json.Marshal(map[string]any{
				"visible_xml":      visibleXML,
				"hidden_xml":       hiddenXML,
				"hidden_state":     tc.hiddenState,
				"fail_enumeration": tc.failEnumeration,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixture := `$script:fixture='` + strings.ReplaceAll(string(fixtureJSON), "'", "''") + `'|ConvertFrom-Json;` + fixtureBody
			code := strings.Replace(schedulerCOMUTF8Preamble+schedulerCOMScript, serviceConnection, fixture, 1)
			request := schedulerCOMRequest{Operation: tc.operation, Name: tc.taskName}
			if tc.operation == "list" {
				request.Prefix = "fixture-"
			}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), schedulerCOMDeadline)
			defer cancel()
			cmd := exec.CommandContext(ctx, powerShell, "-NoProfile", "-NonInteractive", "-Command", code)
			process.NoConsole(cmd)
			cmd.Stdin = strings.NewReader(string(raw))
			var actions bytes.Buffer
			cmd.Stderr = &actions
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("fixture PowerShell: %v, stderr=%q", err, actions.String())
			}
			var response schedulerCOMResponse
			if err := json.Unmarshal(out, &response); err != nil {
				t.Fatalf("fixture JSON=%q err=%v", out, err)
			}
			if response.OK != (tc.wantKind == "") || response.Kind != tc.wantKind {
				t.Fatalf("response=%+v, want kind=%q", response, tc.wantKind)
			}
			if got := strings.TrimSpace(actions.String()); got != tc.wantAction {
				t.Fatalf("fixture actions=%q, want %q", got, tc.wantAction)
			}
			if tc.failEnumeration && response.Phase != "com" {
				t.Fatalf("enumeration error phase=%q, want com", response.Phase)
			}
			if !response.OK {
				return
			}
			switch tc.operation {
			case "list":
				if len(response.Tasks) != 1 || response.Tasks[0].Name != "fixture-visible" || response.Tasks[0].XML != visibleXML {
					t.Fatalf("visible-only list=%+v", response.Tasks)
				}
			case "status", "export":
				wantState, wantXML := tc.hiddenState, hiddenXML
				if tc.taskName == visibleName {
					wantState, wantXML = 3, visibleXML
				}
				if response.Task == nil || response.Task.Name != strings.TrimPrefix(tc.taskName, `\`) || response.Task.State != wantState || response.Task.XML != wantXML || response.Task.Owner != "S-1-5-21-1" {
					t.Fatalf("named task=%+v, want name=%q state=%d xml=%q", response.Task, tc.taskName, wantState, wantXML)
				}
			}
		})
	}
}

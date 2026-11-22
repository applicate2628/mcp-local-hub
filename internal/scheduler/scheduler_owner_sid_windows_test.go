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
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
)

const schedulerFixtureOwnerSID = "S-1-5-21-100-200-300-1000"
const schedulerFixtureForeignSID = "S-1-5-21-100-200-300-2000"

// Reading this process's user identity and constructing XML never contacts the
// Task Scheduler. Do not put the observed account name or SID in test output.
func TestWindowsSchedulerCurrentOwnerUsesSID(t *testing.T) {
	current, err := user.Current()
	if err != nil || current == nil || current.Uid == "" {
		t.Fatal("current Windows user SID is unavailable")
	}
	owner, err := currentWindowsTaskOwner()
	if err != nil || owner != current.Uid {
		t.Fatal("migration owner must retain the current SID, not an account-name projection")
	}
	t.Setenv("SystemRoot", `D:\scheduler-fixture-system`)
	s, err := newPlatformScheduler()
	if err != nil {
		t.Fatalf("construct scheduler: %v", err)
	}
	w, ok := s.(*windowsScheduler)
	if !ok || w.username != current.Uid {
		t.Fatal("scheduler registration and filtering must retain the current SID")
	}
	doc := buildCreateXML(TaskSpec{Name: "fixture", Command: `D:\scheduler-fixture\helper.exe`, LogonTrigger: true}, w.username)
	if strings.Count(doc, "<UserId>"+xmlEscape(current.Uid)+"</UserId>") != 2 {
		t.Fatal("task principal and logon trigger must both carry the current SID")
	}
}

type schedulerOwnerFixtureTask struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	XML   string `json:"xml"`
}

func TestSchedulerCOMOwnerSIDProjection(t *testing.T) {
	own := schedulerOwnerFixtureTask{"fixture-own", `DOMAIN_A\alice`, "<Task/>"}
	foreign := schedulerOwnerFixtureTask{"fixture-foreign", `DOMAIN_B\alice`, "<Task/>"}
	for _, tc := range []struct {
		name, operation string
		tasks           []schedulerOwnerFixtureTask
		wantOwners      []string
		wantError       bool
	}{
		{"list-same-short-name-different-SID", "list", []schedulerOwnerFixtureTask{own, foreign}, []string{schedulerFixtureOwnerSID, schedulerFixtureForeignSID}, false},
		{"list-SID-literal", "list", []schedulerOwnerFixtureTask{{"fixture-own", schedulerFixtureOwnerSID, "<Task/>"}}, []string{schedulerFixtureOwnerSID}, false},
		{"list-unknown-after-good", "list", []schedulerOwnerFixtureTask{own, {"fixture-unknown", `UNRESOLVED\alice`, "<Task/>"}}, nil, true},
		{"list-empty-after-good", "list", []schedulerOwnerFixtureTask{own, {"fixture-empty", "", "<Task/>"}}, nil, true},
		{"status-qualified-owner", "status", []schedulerOwnerFixtureTask{own}, []string{schedulerFixtureOwnerSID}, false},
		{"status-SID-literal", "status", []schedulerOwnerFixtureTask{{"fixture-own", schedulerFixtureOwnerSID, "<Task/>"}}, []string{schedulerFixtureOwnerSID}, false},
		{"status-unknown-owner", "status", []schedulerOwnerFixtureTask{{"fixture-unknown", `UNRESOLVED\alice`, "<Task/>"}}, nil, true},
		{"status-empty-owner", "status", []schedulerOwnerFixtureTask{{"fixture-empty", "", "<Task/>"}}, nil, true},
		{"export-retains-raw-owner-without-lookup", "export", []schedulerOwnerFixtureTask{{"fixture-unknown", `UNRESOLVED\alice`, "<Task/>"}}, []string{`UNRESOLVED\alice`}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := schedulerCOMRequest{Operation: tc.operation, Name: `\` + tc.tasks[0].Name}
			if tc.operation == "list" {
				request.Name, request.Prefix = "", "fixture-"
			}
			response, actions := runSchedulerOwnerSIDFixture(t, request, tc.tasks)
			if actions != "" {
				t.Fatalf("query invoked a mutation: %q", actions)
			}
			withCOMRunner(t, func(_ context.Context, got schedulerCOMRequest) (schedulerCOMResponse, error) {
				if !reflect.DeepEqual(got, request) {
					t.Fatalf("unexpected request: operation=%q name=%q", got.Operation, got.Name)
				}
				return response, nil
			})
			if tc.wantError {
				if response.OK || response.Kind != "scheduler_unavailable" || response.Task != nil || len(response.Tasks) != 0 {
					t.Fatalf("unresolved owner returned success or partial data: %+v", response)
				}
				var err error
				if tc.operation == "list" {
					var listed []TaskStatus
					listed, err = (&windowsScheduler{username: schedulerFixtureOwnerSID}).ListContext(context.Background(), request.Prefix)
					if listed != nil {
						t.Fatal("unresolved owner returned a partial public list")
					}
				} else {
					var status TaskStatus
					status, err = (&windowsScheduler{}).Status(request.Name)
					if status.Name != "" {
						t.Fatal("unresolved owner returned a partial public status")
					}
				}
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("unresolved owner error=%v, want ErrUnavailable", err)
				}
				return
			}
			if !response.OK {
				t.Fatalf("owner projection failed: %+v", response)
			}
			tasks := response.Tasks
			if tc.operation != "list" {
				if response.Task == nil {
					t.Fatal("missing named task")
				}
				tasks = []schedulerCOMTask{*response.Task}
			}
			if len(tasks) != len(tc.wantOwners) {
				t.Fatalf("owner rows=%d want=%d", len(tasks), len(tc.wantOwners))
			}
			for i, owner := range tc.wantOwners {
				if tasks[i].Owner != owner {
					t.Fatalf("owner row %d was not resolved to its expected identity", i)
				}
			}
			if tc.operation == "list" {
				listed, err := (&windowsScheduler{username: schedulerFixtureOwnerSID}).ListContext(context.Background(), request.Prefix)
				if err != nil || len(listed) != 1 || listed[0].Name != `\fixture-own` || listed[0].Owner != schedulerFixtureOwnerSID {
					t.Fatalf("same-name foreign account entered the owned list or own account disappeared: rows=%d err=%v", len(listed), err)
				}
			}
		})
	}
}

// RunOwned already authorizes a SID. Its shared resolver must keep refusing an
// account in another domain even when the account's short name is identical.
func TestSchedulerCOMOwnedRunUsesSameSIDResolver(t *testing.T) {
	for _, tc := range []struct {
		name, owner string
		started     bool
	}{
		{"matching-account", `DOMAIN_A\alice`, true},
		{"same-short-name-foreign-domain", `DOMAIN_B\alice`, false},
		{"unresolved-owner", `UNRESOLVED\alice`, false},
		{"empty-owner", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const name = "fixture-owned"
			const command = `D:\scheduler-fixture\helper.exe`
			xml := fmt.Sprintf(`<Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><RegistrationInfo><URI>\%s</URI></RegistrationInfo><Principals><Principal id="Owner"><UserId>%s</UserId></Principal></Principals><Settings><Enabled>true</Enabled></Settings><Actions Context="Owner"><Exec><Command>%s</Command><Arguments>gui --no-browser</Arguments></Exec></Actions></Task>`, name, xmlEscape(tc.owner), command)
			request := schedulerCOMRequest{Operation: "run_owned", Name: `\` + name, ExpectedCommand: command, ExpectedRole: "gui", ExpectedOwner: schedulerFixtureOwnerSID}
			response, actions := runSchedulerOwnerSIDFixture(t, request, []schedulerOwnerFixtureTask{{name, tc.owner, xml}})
			wantRuns := 0
			if tc.started {
				wantRuns = 1
			}
			if strings.Count(actions, "fixture-run") != wantRuns {
				t.Fatalf("Run effect count differs from SID authorization: %q", actions)
			}
			if tc.started {
				if !response.OK || !response.Started {
					t.Fatalf("matching SID refused: %+v", response)
				}
			} else if response.OK || response.Started || response.Kind != "task_definition_mismatch" {
				t.Fatalf("unproven SID admitted: %+v", response)
			}
		})
	}
}

// Only two external boundaries are replaced: the service connection and account
// lookup. Literal SID parsing, production validation, JSON, and error propagation
// remain real. These fixtures cannot query/register/run any native task or resolve
// a domain account. The PowerShell child has no child-launching fixture method.
func runSchedulerOwnerSIDFixture(t *testing.T, request schedulerCOMRequest, tasks []schedulerOwnerFixtureTask) (schedulerCOMResponse, string) {
	t.Helper()
	const serviceConnection = `$s=New-Object -ComObject 'Schedule.Service';$s.Connect();`
	const accountLookup = `return ([System.Security.Principal.NTAccount]::new($owner)).Translate([System.Security.Principal.SecurityIdentifier]).Value`
	if strings.Count(schedulerCOMScript, serviceConnection) != 1 || strings.Count(schedulerCOMScript, accountLookup) != 1 {
		t.Fatal("exact service and account-lookup fixture boundaries required")
	}
	fixtureJSON, err := json.Marshal(tasks)
	if err != nil {
		t.Fatal(err)
	}
	fixture := `$script:ownerFixtures='` + strings.ReplaceAll(string(fixtureJSON), "'", "''") + `'|ConvertFrom-Json;
$script:ownerTasks=@($script:ownerFixtures|ForEach-Object{
 $task=[pscustomobject]@{Name=[string]$_.name;State=3;Enabled=$true;LastTaskResult=0;Xml=[string]$_.xml;Definition=[pscustomobject]@{Principal=[pscustomobject]@{UserId=[string]$_.owner}}}
 $task|Add-Member ScriptMethod Run {param($args);[Console]::Error.WriteLine('fixture-run');return [pscustomobject]@{InstanceGuid='fixture'}}
 $task|Add-Member ScriptMethod Stop {param($flags);throw 'unexpected fixture Stop'}
 $task
});
$script:ownerFolder=[pscustomobject]@{};
$script:ownerFolder|Add-Member ScriptMethod GetTasks {param($flags);return $script:ownerTasks};
$script:ownerFolder|Add-Member ScriptMethod GetTask {param($name);return @($script:ownerTasks|Where-Object {$_.Name -ceq $name})[0]};
$script:ownerFolder|Add-Member ScriptMethod DeleteTask {param($name,$flags);throw 'unexpected fixture Delete'};
$s=[pscustomobject]@{};$s|Add-Member ScriptMethod GetFolder {param($name);if($name -cne '\'){throw 'fixture root required'};return $script:ownerFolder};`
	code := strings.Replace(schedulerCOMUTF8Preamble+schedulerCOMScript, serviceConnection, fixture, 1)
	code = strings.Replace(code, accountLookup, `switch -CaseSensitive ($owner){'DOMAIN_A\alice' {return '`+schedulerFixtureOwnerSID+`'} 'DOMAIN_B\alice' {return '`+schedulerFixtureForeignSID+`'} default {throw 'fixture owner unresolved'}}`, 1)
	if strings.Contains(code, "New-Object -ComObject") || strings.Contains(code, "NTAccount]::new") {
		t.Fatal("native task connection or account lookup remained in fixture")
	}
	systemRoot := os.Getenv("SystemRoot")
	if !filepath.IsAbs(systemRoot) {
		t.Fatal("qualified absolute SystemRoot required")
	}
	powerShell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if info, err := os.Stat(powerShell); err != nil || !info.Mode().IsRegular() {
		t.Fatal("qualified PowerShell executable required")
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), schedulerCOMDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, powerShell, "-NoProfile", "-NonInteractive", "-Command", code)
	process.NoConsole(cmd)
	cmd.WaitDelay = time.Second
	cmd.Stdin = bytes.NewReader(raw)
	var actions bytes.Buffer
	cmd.Stderr = &actions
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("owner fixture child: %v; stderr=%q", err, actions.String())
	}
	var response schedulerCOMResponse
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatalf("owner fixture JSON: %v; stdout=%q", err, out)
	}
	return response, strings.TrimSpace(actions.String())
}

//go:build windows

package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/process"
)

func TestSchedulerRunOwnedAdmissionAndPositiveReply(t *testing.T) {
	request := OwnedRunRequest{Name: `\fixture`, ExpectedCommand: `C:\private\mcphub.exe`, ExpectedRole: "gui", ExpectedOwner: "S-1-5-21-1"}
	s := &windowsScheduler{}
	calls := 0
	withCOMRunner(t, func(_ context.Context, wire schedulerCOMRequest) (schedulerCOMResponse, error) {
		calls++
		if wire.Operation != "run_owned" || wire.Name != request.Name || wire.ExpectedCommand != request.ExpectedCommand || wire.ExpectedRole != request.ExpectedRole || wire.ExpectedOwner != request.ExpectedOwner {
			t.Fatalf("wire=%+v", wire)
		}
		return schedulerCOMResponse{OK: true}, nil // a query/noop success must not authorize startup
	})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{nil, canceled} {
		if err := s.RunOwned(ctx, request); err == nil {
			t.Fatal("invalid context admitted")
		}
	}
	bad := request
	bad.ExpectedOwner = ""
	if err := s.RunOwned(context.Background(), bad); err == nil {
		t.Fatal("missing owner admitted")
	}
	if calls != 0 {
		t.Fatal("invalid input reached bridge")
	}
	if err := s.RunOwned(context.Background(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("noop reply error=%v", err)
	}
	withCOMRunner(t, func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error) {
		return schedulerCOMResponse{OK: true, Started: true}, nil
	})
	if err := s.RunOwned(context.Background(), request); err != nil {
		t.Fatal(err)
	}
}

// Execute the production JSON consumer and validator against in-process native
// object fixtures. The fixture never creates, queries, or runs an OS task.
func TestSchedulerCOMGuardedRunDefinition(t *testing.T) {
	systemRoot := os.Getenv("SystemRoot")
	if !filepath.IsAbs(systemRoot) {
		t.Fatal("qualified absolute SystemRoot required")
	}
	powerShell := filepath.Join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if info, err := os.Stat(powerShell); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("qualified PowerShell executable %q: %v", powerShell, err)
	}
	const valid = `<Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><RegistrationInfo><URI>\fixture</URI></RegistrationInfo><Principals><Principal id="Owner"><UserId>S-1-5-21-1</UserId></Principal></Principals><Settings><Enabled>true</Enabled></Settings><Actions Context="Owner"><Exec><Command>C:\private\mcphub.exe</Command><Arguments>gui --no-browser</Arguments></Exec></Actions></Task>`
	creatorXML := buildCreateXML(TaskSpec{Name: `\fixture`, Command: `C:\private\mcphub.exe`, Args: []string{"gui", "--no-browser"}, LogonTrigger: true}, "S-1-5-21-1")
	// Task Scheduler supplies the registered URI when exporting creator XML.
	creatorXML = strings.Replace(creatorXML, "<RegistrationInfo>", `<RegistrationInfo><URI>\fixture</URI>`, 1)
	for _, tc := range []struct {
		name, xml, owner string
		state            int
		drift, started   bool
	}{
		{"matching", valid, "S-1-5-21-1", 3, false, true},
		{"omitted-enabled-defaults-true", strings.Replace(valid, "<Enabled>true</Enabled>", "", 1), "S-1-5-21-1", 3, false, true},
		{"numeric-true", strings.Replace(valid, "<Enabled>true</Enabled>", "<Enabled>1</Enabled>", 1), "S-1-5-21-1", 3, false, true},
		{"xml-disabled", strings.Replace(valid, "<Enabled>true</Enabled>", "<Enabled>false</Enabled>", 1), "S-1-5-21-1", 3, false, false},
		{"numeric-false", strings.Replace(valid, "<Enabled>true</Enabled>", "<Enabled>0</Enabled>", 1), "S-1-5-21-1", 3, false, false},
		{"empty-enabled", strings.Replace(valid, "<Enabled>true</Enabled>", "<Enabled/>", 1), "S-1-5-21-1", 3, false, false},
		{"malformed-enabled", strings.Replace(valid, "<Enabled>true</Enabled>", "<Enabled>enabled</Enabled>", 1), "S-1-5-21-1", 3, false, false},
		{"duplicate-enabled", strings.Replace(valid, "<Enabled>true</Enabled>", "<Enabled>true</Enabled><Enabled>true</Enabled>", 1), "S-1-5-21-1", 3, false, false},
		{"nested-enabled", strings.Replace(valid, "<Enabled>true</Enabled>", "<Enabled><Value>true</Value></Enabled>", 1), "S-1-5-21-1", 3, false, false},
		{"foreign-enabled-namespace", strings.Replace(valid, "<Enabled>true</Enabled>", `<Enabled xmlns="urn:foreign">true</Enabled>`, 1), "S-1-5-21-1", 3, false, false},
		{"native-disabled", strings.Replace(valid, "<Enabled>true</Enabled>", "", 1), "S-1-5-21-1", 3, false, false},
		{"creator-shape", creatorXML, "S-1-5-21-1", 3, false, true},
		{"strict-gui", strings.Replace(valid, "gui --no-browser", "gui --no-browser --strict-mode", 1), "S-1-5-21-1", 3, false, true},
		{"foreign-target", strings.Replace(valid, `C:\private`, `C:\installed`, 1), "S-1-5-21-1", 3, false, false},
		{"wrong-native-owner", valid, "S-1-5-21-2", 3, false, false},
		{"wrong-xml-owner", strings.Replace(valid, "S-1-5-21-1", "S-1-5-21-2", 1), "S-1-5-21-1", 3, false, false},
		{"wrong-uri", strings.Replace(valid, `\fixture</URI>`, `\foreign</URI>`, 1), "S-1-5-21-1", 3, false, false},
		{"wrong-role", strings.Replace(valid, "gui --no-browser", "supervise", 1), "S-1-5-21-1", 3, false, false},
		{"extra-argument", strings.Replace(valid, "gui --no-browser", "gui --no-browser --other", 1), "S-1-5-21-1", 3, false, false},
		{"multi-action", strings.Replace(valid, "</Actions>", "<Exec><Command>C:\\foreign.exe</Command></Exec></Actions>", 1), "S-1-5-21-1", 3, false, false},
		{"malformed", "<Task>", "S-1-5-21-1", 3, false, false},
		{"missing-expectation", valid, "S-1-5-21-1", 3, false, false},
		{"wrong-expected-role", valid, "S-1-5-21-1", 3, false, false},
		{"empty-expected-target", valid, "S-1-5-21-1", 3, false, false},
		{"disabled", valid, "S-1-5-21-1", 1, false, false},
		{"definition-drift", valid, "S-1-5-21-1", 3, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixtureJSON, _ := json.Marshal(map[string]any{"xml": tc.xml, "owner": tc.owner, "state": tc.state, "drift": tc.drift, "enabled": tc.name != "native-disabled"})
			// Definition/Xml reads and Run share the same retained object. A
			// second Xml read deterministically exposes publication drift.
			fixture := `$fixture='` + strings.ReplaceAll(string(fixtureJSON), "'", "''") + `'|ConvertFrom-Json;$script:reads=0;$script:runs=0;$task=[pscustomobject]@{Name='fixture';State=[int]$fixture.state;Enabled=[bool]$fixture.enabled;Definition=[pscustomobject]@{Principal=[pscustomobject]@{UserId=$fixture.owner}}};$task|Add-Member ScriptProperty Xml {$script:reads++;if($fixture.drift -and $script:reads -gt 1){return '<changed/>'};return [string]$fixture.xml};$task|Add-Member ScriptMethod Run {param($args);$script:runs++;[Console]::Error.WriteLine('fixture-run');return [pscustomobject]@{InstanceGuid='fixture'}};$folder=[pscustomobject]@{};$folder|Add-Member ScriptMethod GetTasks {param($flags);return @($task)};$folder|Add-Member ScriptMethod GetTask {param($name);return $task};$s=[pscustomobject]@{};$s|Add-Member ScriptMethod GetFolder {param($name);return $folder};`
			code := strings.Replace(schedulerCOMUTF8Preamble+schedulerCOMScript, `$s=New-Object -ComObject 'Schedule.Service';$s.Connect();`, fixture, 1)
			request := schedulerCOMRequest{Operation: "run_owned", Name: `\fixture`, ExpectedCommand: `C:\private\mcphub.exe`, ExpectedRole: "gui", ExpectedOwner: "S-1-5-21-1"}
			switch tc.name {
			case "missing-expectation":
				request.ExpectedOwner = ""
			case "wrong-expected-role":
				request.ExpectedRole = "supervise"
			case "empty-expected-target":
				request.ExpectedCommand = ""
			}
			raw, _ := json.Marshal(request)
			ctx, cancel := context.WithTimeout(context.Background(), schedulerCOMDeadline)
			defer cancel()
			cmd := exec.CommandContext(ctx, powerShell, "-NoProfile", "-NonInteractive", "-Command", code)
			process.NoConsole(cmd)
			cmd.Stdin = strings.NewReader(string(raw))
			var runEvidence bytes.Buffer
			cmd.Stderr = &runEvidence
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("fixture failed: %v", err)
			}
			var result schedulerCOMResponse
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatalf("JSON=%q err=%v", out, err)
			}
			if got := strings.Count(runEvidence.String(), "fixture-run"); got != map[bool]int{false: 0, true: 1}[tc.started] {
				t.Fatalf("native Run count=%d stderr=%q", got, runEvidence.String())
			}
			if tc.started {
				if !result.OK || !result.Started {
					t.Fatalf("matching result=%+v", result)
				}
			} else if result.OK || result.Started || result.Kind != "task_definition_mismatch" {
				t.Fatalf("unsafe result=%+v", result)
			}
		})
	}
}

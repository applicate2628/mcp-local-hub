//go:build windows

package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"mcp-local-hub/internal/process"
)

const schedulerCOMDeadline = 15 * time.Second

type schedulerCOMRequest struct {
	Operation string `json:"operation"`
	Name      string `json:"name,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
}

type schedulerCOMTask struct {
	Name  string `json:"name"`
	State int    `json:"state"`
	XML   string `json:"xml,omitempty"`
	Owner string `json:"owner,omitempty"`
}

type schedulerCOMResponse struct {
	OK      bool               `json:"ok"`
	Kind    string             `json:"kind,omitempty"`
	Phase   string             `json:"phase,omitempty"`
	HRESULT uint32             `json:"hresult,omitempty"`
	Task    *schedulerCOMTask  `json:"task,omitempty"`
	Tasks   []schedulerCOMTask `json:"tasks,omitempty"`
}

// The script is fixed code. Request values arrive solely through JSON stdin;
// task names are never interpolated into PowerShell source.
const schedulerCOMScript = `$r=[Console]::In.ReadToEnd()|ConvertFrom-Json;try{$s=New-Object -ComObject 'Schedule.Service';$s.Connect();$f=$s.GetFolder('\');$all=@($f.GetTasks(0));$n=([string]$r.name).TrimStart('\');$t=@($all|Where-Object {$_.Name -ceq $n})[0];if($r.operation -eq 'list'){$rows=@($all|Where-Object {$_.Name.StartsWith([string]$r.prefix)}|ForEach-Object{[pscustomobject]@{name=$_.Name;state=[int]$_.State;xml=[string]$_.Xml;owner=[string]$_.Definition.Principal.UserId}});[pscustomobject]@{ok=$true;tasks=$rows}|ConvertTo-Json -Compress -Depth 4;exit};if($null -eq $t){[pscustomobject]@{ok=$false;kind='task_absent'}|ConvertTo-Json -Compress;exit};if($r.operation -eq 'delete'){$f.DeleteTask($t.Name,0);[pscustomobject]@{ok=$true}|ConvertTo-Json -Compress;exit};if($r.operation -eq 'stop'){if([int]$t.State -ne 4){[pscustomobject]@{ok=$false;kind='task_not_running'}|ConvertTo-Json -Compress;exit};$t.Stop(0);[pscustomobject]@{ok=$true}|ConvertTo-Json -Compress;exit};[pscustomobject]@{ok=$true;task=[pscustomobject]@{name=$t.Name;state=[int]$t.State;xml=[string]$t.Xml;owner=[string]$t.Definition.Principal.UserId}}|ConvertTo-Json -Compress -Depth 4}catch{[pscustomobject]@{ok=$false;kind='scheduler_unavailable';phase='com';hresult=[uint32]$_.Exception.HResult}|ConvertTo-Json -Compress}`

var schedulerCOMRun = func(ctx context.Context, request schedulerCOMRequest) (schedulerCOMResponse, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return schedulerCOMResponse{}, err
	}
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", schedulerCOMScript)
	process.NoConsole(cmd)
	cmd.Stdin = strings.NewReader(string(raw))
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return schedulerCOMResponse{}, fmt.Errorf("%w: timeout", ErrUnavailable)
	}
	if err != nil {
		return schedulerCOMResponse{}, fmt.Errorf("%w: bridge", ErrUnavailable)
	}
	var response schedulerCOMResponse
	if err := json.Unmarshal(out, &response); err != nil {
		return schedulerCOMResponse{}, fmt.Errorf("%w: protocol", ErrUnavailable)
	}
	return response, nil
}

func schedulerCOM(ctx context.Context, request schedulerCOMRequest) (schedulerCOMResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, schedulerCOMDeadline)
	defer cancel()
	response, err := schedulerCOMRun(ctx, request)
	if err != nil {
		return schedulerCOMResponse{}, fmt.Errorf("%w: bridge execution: %v", ErrUnavailable, err)
	}
	if response.OK {
		return response, nil
	}
	switch response.Kind {
	case "task_absent":
		return response, ErrTaskNotFound
	case "task_not_running":
		return response, ErrTaskNotRunning
	case "permission_denied":
		return response, ErrPermissionDenied
	case "task_corrupt":
		return response, ErrTaskCorrupt
	default:
		return response, fmt.Errorf("%w: %s", ErrUnavailable, response.Phase)
	}
}

func taskStatusFromCOM(task schedulerCOMTask) (TaskStatus, error) {
	state := TaskRuntimeUnknown
	switch task.State {
	case 1:
		state = TaskRuntimeDisabled
	case 2:
		state = TaskRuntimeQueued
	case 3:
		state = TaskRuntimeReady
	case 4:
		state = TaskRuntimeRunning
	default:
		return TaskStatus{}, fmt.Errorf("%w: unknown task state %d", ErrUnavailable, task.State)
	}
	return TaskStatus{Name: `\` + strings.TrimPrefix(task.Name, `\`), RuntimeState: state, Owner: task.Owner, State: map[TaskRuntimeState]string{TaskRuntimeDisabled: "Disabled", TaskRuntimeQueued: "Queued", TaskRuntimeReady: "Ready", TaskRuntimeRunning: "Running"}[state]}, nil
}

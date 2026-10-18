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

const schedulerCOMUTF8Preamble = `$utf8=[System.Text.UTF8Encoding]::new($false);[Console]::InputEncoding=$utf8;[Console]::OutputEncoding=$utf8;`

type schedulerCOMRequest struct {
	Operation       string `json:"operation"`
	Name            string `json:"name,omitempty"`
	Prefix          string `json:"prefix,omitempty"`
	ExpectedCommand string `json:"expected_command,omitempty"`
	ExpectedRole    string `json:"expected_role,omitempty"`
	ExpectedOwner   string `json:"expected_owner,omitempty"`
}

type schedulerCOMTask struct {
	Name  string `json:"name"`
	State int    `json:"state"`
	XML   string `json:"xml,omitempty"`
	Owner string `json:"owner,omitempty"`
}

type schedulerCOMResponse struct {
	OK      bool               `json:"ok"`
	Started bool               `json:"started,omitempty"`
	Kind    string             `json:"kind,omitempty"`
	Phase   string             `json:"phase,omitempty"`
	HRESULT uint32             `json:"hresult,omitempty"`
	Task    *schedulerCOMTask  `json:"task,omitempty"`
	Tasks   []schedulerCOMTask `json:"tasks,omitempty"`
}

// The script is fixed code. Request values arrive solely through JSON stdin;
// task names are never interpolated into PowerShell source.
const schedulerCOMScript = `$r=[Console]::In.ReadToEnd()|ConvertFrom-Json;try{
function Get-OwnerSID([string]$owner){
 if([string]::IsNullOrWhiteSpace($owner)){return ''}
 if($owner.StartsWith('S-1-')){return ([System.Security.Principal.SecurityIdentifier]::new($owner)).Value}
 return ([System.Security.Principal.NTAccount]::new($owner)).Translate([System.Security.Principal.SecurityIdentifier]).Value
}
function Test-OwnedDefinition($task,[string]$snapshot,$request){
 try{
  if($request.expected_role -cne 'gui' -or [string]::IsNullOrWhiteSpace([string]$request.expected_owner)){return $false}
  $command=[string]$request.expected_command
  if($command -notmatch '^(?:[A-Za-z]:\\|\\\\[^\\]+\\[^\\]+\\)' -or [IO.Path]::GetFullPath($command) -ine $command){return $false}
  $settings=[System.Xml.XmlReaderSettings]::new();$settings.DtdProcessing=[System.Xml.DtdProcessing]::Prohibit;$settings.XmlResolver=$null
  $reader=[System.Xml.XmlReader]::Create([System.IO.StringReader]::new($snapshot),$settings)
  try{$doc=[System.Xml.XmlDocument]::new();$doc.XmlResolver=$null;$doc.Load($reader)}finally{$reader.Dispose()}
  $ns=[System.Xml.XmlNamespaceManager]::new($doc.NameTable);$ns.AddNamespace('t','http://schemas.microsoft.com/windows/2004/02/mit/task')
  if($doc.DocumentElement.LocalName -cne 'Task' -or $doc.DocumentElement.NamespaceURI -cne $ns.LookupNamespace('t')){return $false}
  foreach($path in @('/t:Task/t:RegistrationInfo','/t:Task/t:RegistrationInfo/t:URI','/t:Task/t:Principals','/t:Task/t:Principals/t:Principal','/t:Task/t:Principals/t:Principal/t:UserId','/t:Task/t:Settings','/t:Task/t:Actions','/t:Task/t:Actions/*','/t:Task/t:Actions/t:Exec','/t:Task/t:Actions/t:Exec/t:Command','/t:Task/t:Actions/t:Exec/t:Arguments')){
   if($doc.SelectNodes($path,$ns).Count -ne 1){return $false}
  }
  $uri=$doc.SelectSingleNode('/t:Task/t:RegistrationInfo/t:URI',$ns)
  $principal=$doc.SelectSingleNode('/t:Task/t:Principals/t:Principal',$ns)
  $owner=$doc.SelectSingleNode('/t:Task/t:Principals/t:Principal/t:UserId',$ns)
  $actions=$doc.SelectSingleNode('/t:Task/t:Actions',$ns)
  $exec=$doc.SelectSingleNode('/t:Task/t:Actions/t:Exec',$ns)
  $cmd=$exec.SelectSingleNode('t:Command',$ns);$args=$exec.SelectSingleNode('t:Arguments',$ns)
  $enabledNodes=$doc.SelectNodes('/t:Task/t:Settings/t:Enabled',$ns)
  if($enabledNodes.Count -gt 1 -or $doc.SelectNodes('/t:Task/t:Settings/*[local-name()="Enabled"]',$ns).Count -ne $enabledNodes.Count){return $false}
  # Task Scheduler settingsType defaults an omitted Enabled element to true.
  $xmlEnabled=$true
  if($enabledNodes.Count -eq 1){
   $enabled=$enabledNodes[0]
   if($enabled.SelectNodes('*').Count -ne 0){return $false}
   $xmlEnabled=[System.Xml.XmlConvert]::ToBoolean($enabled.InnerText.Trim())
  }
  foreach($node in @($uri,$owner,$cmd,$args)){if($node.SelectNodes('*').Count -ne 0){return $false}}
  if($exec.SelectNodes('*').Count -gt 3 -or $exec.SelectNodes('*[not(self::t:Command or self::t:Arguments or self::t:WorkingDirectory)]',$ns).Count -ne 0 -or $exec.SelectNodes('t:WorkingDirectory',$ns).Count -gt 1){return $false}
  if($uri.InnerText -cne [string]$request.name -or ('\'+[string]$task.Name) -cne [string]$request.name -or $cmd.InnerText -ine $command){return $false}
  if([string]::IsNullOrWhiteSpace($principal.GetAttribute('id')) -or $actions.GetAttribute('Context') -cne $principal.GetAttribute('id')){return $false}
  if((Get-OwnerSID $owner.InnerText) -cne [string]$request.expected_owner -or (Get-OwnerSID ([string]$task.Definition.Principal.UserId)) -cne [string]$request.expected_owner){return $false}
  if($args.InnerText -cnotmatch '^gui\s+--no-browser(?:\s+--strict-mode)?$' -or !$xmlEnabled -or !$task.Enabled -or [int]$task.State -notin @(2,3,4)){return $false}
  return $true
 }catch{return $false}
}
$s=New-Object -ComObject 'Schedule.Service';$s.Connect();$f=$s.GetFolder('\');$all=@($f.GetTasks(0));$n=([string]$r.name).TrimStart('\');$matches=@($all|Where-Object {$_.Name -ceq $n});$t=$matches[0];
if($r.operation -eq 'list'){$rows=@($all|Where-Object {$_.Name.StartsWith([string]$r.prefix)}|ForEach-Object{[pscustomobject]@{name=$_.Name;state=[int]$_.State;xml=[string]$_.Xml;owner=[string]$_.Definition.Principal.UserId}});[pscustomobject]@{ok=$true;tasks=$rows}|ConvertTo-Json -Compress -Depth 4;exit};
if($null -eq $t){[pscustomobject]@{ok=$false;kind='task_absent'}|ConvertTo-Json -Compress;exit};
if($r.operation -eq 'run_owned'){
 $snapshot=[string]$t.Xml
 if($matches.Count -ne 1 -or !(Test-OwnedDefinition $t $snapshot $r)){[pscustomobject]@{ok=$false;kind='task_definition_mismatch'}|ConvertTo-Json -Compress;exit}
 # The recovery caller owns the cooperative lease. These reads detect observed
 # drift; Task Scheduler exposes no atomic compare-and-run for external writers.
 $current=$f.GetTask($n)
 if([string]$t.Xml -cne $snapshot -or [string]$current.Xml -cne $snapshot -or !(Test-OwnedDefinition $current $snapshot $r)){
  [pscustomobject]@{ok=$false;kind='task_definition_mismatch'}|ConvertTo-Json -Compress;exit
 }
 $running=$t.Run($null)
 if($null -eq $running){[pscustomobject]@{ok=$false;kind='scheduler_unavailable';phase='run_unconfirmed'}|ConvertTo-Json -Compress;exit}
 [pscustomobject]@{ok=$true;started=$true}|ConvertTo-Json -Compress;exit
};
if($r.operation -eq 'delete'){$f.DeleteTask($t.Name,0);[pscustomobject]@{ok=$true}|ConvertTo-Json -Compress;exit};
if($r.operation -eq 'stop'){if([int]$t.State -ne 4){[pscustomobject]@{ok=$false;kind='task_not_running'}|ConvertTo-Json -Compress;exit};$t.Stop(0);[pscustomobject]@{ok=$true}|ConvertTo-Json -Compress;exit};
if($r.operation -notin @('status','export')){[pscustomobject]@{ok=$false;kind='scheduler_unavailable';phase='operation'}|ConvertTo-Json -Compress;exit};
[pscustomobject]@{ok=$true;task=[pscustomobject]@{name=$t.Name;state=[int]$t.State;xml=[string]$t.Xml;owner=[string]$t.Definition.Principal.UserId}}|ConvertTo-Json -Compress -Depth 4
}catch{[pscustomobject]@{ok=$false;kind='scheduler_unavailable';phase='com';hresult=[uint32]([int64]$_.Exception.HResult -band 0xffffffffL)}|ConvertTo-Json -Compress}`

var schedulerCOMRun = func(ctx context.Context, request schedulerCOMRequest) (schedulerCOMResponse, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return schedulerCOMResponse{}, err
	}
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", schedulerCOMUTF8Preamble+schedulerCOMScript)
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
	case "task_definition_mismatch":
		return response, fmt.Errorf("%w: guarded task definition mismatch", ErrTaskCorrupt)
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

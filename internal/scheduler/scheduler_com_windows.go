//go:build windows

package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
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
	XMLUTF16LE      []byte `json:"xml_utf16le,omitempty"`
}

type schedulerCOMTask struct {
	Name       string  `json:"name"`
	State      int     `json:"state"`
	XML        string  `json:"xml,omitempty"`
	Owner      string  `json:"owner,omitempty"`
	LastResult *int32  `json:"last_result"`
	NextRun    *string `json:"next_run"` // nil is unavailable; empty is observed DATE zero
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
function Get-TaskLastResult($task){
 $result=$task.LastTaskResult
 if($null -eq $result){throw 'task result unavailable'}
 return [int]$result
}
function Get-TaskNextRun($task){
 $next=$task.NextRunTime
 if($null -eq $next -or $next -isnot [DateTime]){throw 'task next run unavailable'}
 # Native DATE zero is observed no-calendar-time, not a failed property read.
 if($next.ToOADate() -eq 0){return ''}
 # Task Scheduler supplies local wall-clock time (Kind Unspecified). Do not
 # attach a UTC marker or infer a time-zone offset from this observation.
 return $next.ToString('yyyy-MM-ddTHH:mm:ss.fffffff',[Globalization.CultureInfo]::InvariantCulture)
}
function Get-NativeHRESULT($exception){
 $native=$null;$last=$exception
 while($null -ne $exception){
  $last=$exception
  if($exception -is [System.Runtime.InteropServices.COMException]){$native=$exception.HResult}
  $exception=$exception.InnerException
 }
 if($null -eq $native){$native=$last.HResult}
 return [uint32]([int64]$native -band 0xffffffffL)
}
function Get-OwnerSID([string]$owner){
 if([string]::IsNullOrWhiteSpace($owner)){throw 'task owner unavailable'}
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
$s=New-Object -ComObject 'Schedule.Service';$s.Connect();$f=$s.GetFolder('\');
if($r.operation -eq 'create_xml_if_absent'){
 $phase='xml_decode'
 if([string]::IsNullOrWhiteSpace([string]$r.name)){throw [System.FormatException]::new('empty task name')}
 $raw=[Convert]::FromBase64String([string]$r.xml_utf16le)
 if($raw.Length -le 2 -or $raw[0] -ne 255 -or $raw[1] -ne 254){throw [System.FormatException]::new('expected UTF-16LE XML with BOM')}
 $decoder=[System.Text.UnicodeEncoding]::new($false,$false,$true);$xml=$decoder.GetString($raw,2,$raw.Length-2)
 if([string]::IsNullOrWhiteSpace($xml)){throw [System.FormatException]::new('empty task XML')}
 $phase='xml_register'
 # TASK_CREATE | TASK_IGNORE_REGISTRATION_TRIGGERS. Never update an occupant.
 # TASK_LOGON_NONE and empty credential overrides preserve the XML principal.
 # https://learn.microsoft.com/en-us/windows/win32/api/taskschd/ne-taskschd-task_creation
 $created=$f.RegisterTask([string]$r.name,$xml,34,$null,$null,0,$null)
 if($null -eq $created){[pscustomobject]@{ok=$false;kind='scheduler_unavailable';phase='xml_register_unconfirmed'}|ConvertTo-Json -Compress;exit}
 [pscustomobject]@{ok=$true}|ConvertTo-Json -Compress;exit
};
# Include hidden tasks in named lookups so hidden does not mean absent; preserve List's visible-only scope.
$flags=1;if($r.operation -eq 'list'){$flags=0};$all=@($f.GetTasks($flags));$n=([string]$r.name).TrimStart('\');$matches=@($all|Where-Object {$_.Name -ceq $n});$t=$matches[0];
# Owner filtering and status consumers receive a resolved SID, never a short account name.
if($r.operation -eq 'list'){$rows=@($all|Where-Object {$_.Name.StartsWith([string]$r.prefix)}|ForEach-Object{[pscustomobject]@{name=$_.Name;state=[int]$_.State;xml=[string]$_.Xml;owner=(Get-OwnerSID ([string]$_.Definition.Principal.UserId));last_result=(Get-TaskLastResult $_);next_run=(Get-TaskNextRun $_)}});[pscustomobject]@{ok=$true;tasks=$rows}|ConvertTo-Json -Compress -Depth 4;exit};
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
$row=[ordered]@{name=$t.Name;state=[int]$t.State;xml=[string]$t.Xml;owner=[string]$t.Definition.Principal.UserId}
if($r.operation -eq 'status'){$row['owner']=Get-OwnerSID ([string]$row['owner']);$row['last_result']=Get-TaskLastResult $t;$row['next_run']=Get-TaskNextRun $t}
[pscustomobject]@{ok=$true;task=[pscustomobject]$row}|ConvertTo-Json -Compress -Depth 4
}catch{
 if($r.operation -eq 'create_xml_if_absent'){
  $h=Get-NativeHRESULT $_.Exception;$code='{0:X8}' -f $h;$kind='scheduler_unavailable'
  if($phase -eq 'xml_decode'){$kind='task_corrupt'}
  elseif($code -eq '80070005'){$kind='permission_denied'}
  elseif($phase -eq 'xml_register'){
   switch($code){
    '800700B7' {$kind='task_already_exists'}
    {$_ -in @('80070057','80041316','80041317','80041318','80041319','8004131A','8004131D')} {$kind='task_corrupt'}
   }
  }
  if([string]::IsNullOrEmpty($phase)){$phase='com'}
  [pscustomobject]@{ok=$false;kind=$kind;phase=$phase;hresult=$h}|ConvertTo-Json -Compress
 }else{
  [pscustomobject]@{ok=$false;kind='scheduler_unavailable';phase='com';hresult=[uint32]([int64]$_.Exception.HResult -band 0xffffffffL)}|ConvertTo-Json -Compress
 }
}`

var schedulerCOMRun = func(ctx context.Context, request schedulerCOMRequest) (schedulerCOMResponse, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return schedulerCOMResponse{}, err
	}
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", schedulerCOMUTF8Preamble+schedulerCOMScript)
	result, err := runSchedulerCommand(ctx, cmd, raw)
	if err != nil {
		// Preserve the command boundary's outer dispatch evidence. The public
		// COM boundary adds operation context after classifying that evidence.
		return schedulerCOMResponse{}, err
	}
	var response schedulerCOMResponse
	if err := json.Unmarshal(result.Stdout.Prefix, &response); err != nil {
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
	if err := ctx.Err(); err != nil {
		return schedulerCOMResponse{}, fmt.Errorf("%w: %w", ErrUnavailable, errors.Join(err, context.Cause(ctx)))
	}
	response, err := schedulerCOMRun(ctx, request)
	if err != nil {
		if schedulerCOMMutates(request.Operation) && !schedulerCommandWasNotDispatched(err) {
			err = errors.Join(ErrMutationOutcomeUnknown, err)
		}
		return schedulerCOMResponse{}, fmt.Errorf("%w: bridge execution: %w", ErrUnavailable, err)
	}
	if response.OK {
		return response, nil
	}
	switch response.Kind {
	case "task_already_exists":
		return response, ErrTaskAlreadyExists
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
		err := fmt.Errorf("%w: %s", ErrUnavailable, response.Phase)
		if schedulerCOMMutates(request.Operation) {
			err = errors.Join(ErrMutationOutcomeUnknown, err)
		}
		return response, err
	}
}

func schedulerCOMMutates(operation string) bool {
	return operation == "create_xml_if_absent" || operation == "run_owned" || operation == "delete" || operation == "stop"
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
	if task.LastResult == nil {
		return TaskStatus{}, fmt.Errorf("%w: missing task result", ErrUnavailable)
	}
	if task.NextRun == nil {
		return TaskStatus{}, fmt.Errorf("%w: missing task next run", ErrUnavailable)
	}
	if *task.NextRun != "" {
		if _, err := time.Parse("2006-01-02T15:04:05.9999999", *task.NextRun); err != nil {
			return TaskStatus{}, fmt.Errorf("%w: malformed task next run: %w", ErrUnavailable, err)
		}
	}
	return TaskStatus{
		Name:         `\` + strings.TrimPrefix(task.Name, `\`),
		RuntimeState: state,
		LastResult:   int(*task.LastResult),
		NextRun:      *task.NextRun,
		Owner:        task.Owner,
		State:        map[TaskRuntimeState]string{TaskRuntimeDisabled: "Disabled", TaskRuntimeQueued: "Queued", TaskRuntimeReady: "Ready", TaskRuntimeRunning: "Running"}[state],
	}, nil
}

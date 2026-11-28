# Proposal only. No scheduler access without -Apply. Windows PowerShell 5.1.
[CmdletBinding()]
param(
    [switch]$Apply,
    [string]$SourceFile
)
$ErrorActionPreference = 'Stop'
$head = '1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3'
$sourceHash = 'e2594cbe0a0cca61038a09ea7d37a701132be378bec6b935236e8b4233ed43fa'
$workRoot = 'D:\dev\ChatGPT'
if (!$Apply) {
    [ordered]@{
        mode = 'PLAN_ONLY'; head = $head; source_sha256 = $sourceHash
        root = $workRoot; tasks = @('\mcp-r17-<guid>-visible', '\mcp-r17-<guid>-hidden')
        registration = 'TASK_CREATE | TASK_DISABLE = 10; no triggers; current interactive user; least privilege'
        reads = @('status and export of both tasks', 'visible-only list', 'status and export of absent sibling')
        cleanup = 'Only successful creations; recheck definition, principal and disabled state; retain on uncertainty'
        task_execution = 'No Run, RunOwned or Stop requests; nonexistent action; demand start disabled'
    } | ConvertTo-Json -Depth 4
    return
}
if ([Environment]::OSVersion.Platform -ne 'Win32NT') { throw 'Apply requires Windows.' }
foreach ($path in @('D:\', 'D:\dev', $workRoot)) {
    $item = Get-Item -LiteralPath $path
    if (!$item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Work root must be a real directory without junctions.' }
}
if ([string]::IsNullOrWhiteSpace($SourceFile)) { throw 'Apply requires the exact reviewed source file via -SourceFile.' }
$SourceFile = [IO.Path]::GetFullPath($SourceFile)
if (!$SourceFile.StartsWith($workRoot + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Source must be inside the authorized work root.' }
$sourceBytes = [IO.File]::ReadAllBytes($SourceFile)
$sha256 = [Security.Cryptography.SHA256]::Create()
try { $actualHash = [BitConverter]::ToString($sha256.ComputeHash($sourceBytes)).Replace('-', '') } finally { $sha256.Dispose() }
if ($actualHash -ine $sourceHash) { throw 'Source does not match the reviewed #660 snapshot.' }
$source = [Text.Encoding]::UTF8.GetString($sourceBytes)
$preamble = [regex]::Matches($source, '(?s)const schedulerCOMUTF8Preamble = `([^`]*)`')
$body = [regex]::Matches($source, '(?s)const schedulerCOMScript = `([^`]*)`')
if ($preamble.Count -ne 1 -or $body.Count -ne 1) { throw 'Expected exactly one production preamble and script.' }
$encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($preamble[0].Groups[1].Value + $body[0].Groups[1].Value))
$powerShell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
if (!(Test-Path -LiteralPath $powerShell -PathType Leaf)) { throw 'Windows PowerShell is unavailable; no installation attempted.' }
$utf8 = [Text.UTF8Encoding]::new($false)
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if ($identity.IsSystem -or ![Environment]::UserInteractive -or [Diagnostics.Process]::GetCurrentProcess().SessionId -eq 0) { throw 'Use the current interactive user session, not a service or session 0.' }
$ownerSid = $identity.User.Value
$runId = [Guid]::NewGuid().ToString('N')
$prefix = 'mcp-r17-' + $runId + '-'
$parentDir = Join-Path $workRoot 'mcp-r17-native-qualification-20261119'
if (Test-Path -LiteralPath $parentDir) {
    $item = Get-Item -LiteralPath $parentDir
    if (!$item.PSIsContainer -or ($item.Attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Evidence parent must be a real directory.' }
} else { $null = New-Item -Path $parentDir -ItemType Directory }
$runDir = Join-Path $parentDir $runId
$null = New-Item -Path $runDir -ItemType Directory
$actionPath = Join-Path $runDir 'never-created-fixture-action.exe'
$marker = 'R17 #660 ' + $runId + ' ' + [char]0x0416 + [char]0x00A7
$records = @('visible', 'hidden') | ForEach-Object {
    [pscustomobject]@{ name = $prefix + $_; hidden = ($_ -eq 'hidden'); created = $false; confirmed = $false; attempted = $false; xml = $null; cleanup = 'not_created'; error = $null }
}
[IO.File]::WriteAllText((Join-Path $runDir 'intent.json'), ([ordered]@{
    head = $head; source_sha256 = $sourceHash; run_id = $runId; principal_sid = $ownerSid
    marker = $marker; action = $actionPath; names = @($records.name); policy = 'CREATE-only, disabled, no triggers; no task execution'
} | ConvertTo-Json -Depth 4), $utf8)
$checks = @(); $children = @(); $childUnsettled = $false; $readsPassed = $false; $failure = $null; $folder = $null

function Assert-OwnedDisabledTask($task, $record) {
    $definition = $task.Definition
    $sid = [string]$definition.Principal.UserId
    if (!$sid.StartsWith('S-1-')) { $sid = ([Security.Principal.NTAccount]::new($sid)).Translate([Security.Principal.SecurityIdentifier]).Value }
    if ([string]$task.Path -cne ('\' + $record.name) -or $sid -cne $ownerSid) { throw 'Task path or execution principal changed.' }
    if ($task.Enabled -or [int]$task.State -ne 1 -or $definition.Settings.Enabled -or $definition.Settings.AllowDemandStart) { throw 'Task is not disabled and inert.' }
    if ($definition.Settings.Hidden -ne $record.hidden -or $definition.Triggers.Count -ne 0) { throw 'Visibility or triggers changed.' }
    if ([int]$definition.Principal.LogonType -ne 3 -or [int]$definition.Principal.RunLevel -ne 0) { throw 'Principal is not current interactive user at least privilege.' }
    if ([string]$definition.RegistrationInfo.Description -cne $marker -or [string]$definition.RegistrationInfo.URI -cne ('\' + $record.name)) { throw 'Run ownership marker changed.' }
    if ($definition.Actions.Count -ne 1) { throw 'Action count changed.' }
    $action = $definition.Actions.Item(1)
    if ([int]$action.Type -ne 0 -or [string]$action.Path -cne $actionPath -or [string]$action.Arguments -cne '' -or (Test-Path -LiteralPath $actionPath)) { throw 'Inert action changed or its executable now exists.' }
    if ($null -ne $record.xml -and [string]$task.Xml -cne $record.xml) { throw 'Registered definition drifted from the successful creation snapshot.' }
}

function Invoke-ProductionRead($request, [string]$label) {
    if ($request.operation -notin @('status', 'export', 'list')) { throw 'Only production read operations are allowed.' }
    $info = [Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $powerShell; $info.Arguments = '-NoProfile -NonInteractive -EncodedCommand ' + $encoded; $info.WorkingDirectory = $runDir
    $info.UseShellExecute = $false; $info.CreateNoWindow = $true
    $info.RedirectStandardInput = $true; $info.RedirectStandardOutput = $true; $info.RedirectStandardError = $true
    $info.StandardOutputEncoding = $utf8; $info.StandardErrorEncoding = $utf8
    $child = [Diagnostics.Process]::new(); $child.StartInfo = $info
    $entry = [pscustomobject]@{ check = $label; pid = $null; started_utc = $null; settled = $false; exit_code = $null }
    $script:children += $entry
    try {
        if (!$child.Start()) { throw 'Production-script child was not started.' }
        $entry.pid = $child.Id; $entry.started_utc = $child.StartTime.ToUniversalTime().ToString('o')
        $stdout = $child.StandardOutput.ReadToEndAsync(); $stderr = $child.StandardError.ReadToEndAsync()
        # Request names and prefix are ASCII; the unmodified preamble sets UTF-8 output.
        $child.StandardInput.Write(($request | ConvertTo-Json -Compress)); $child.StandardInput.Close()
        if (!$child.WaitForExit(15000)) { throw 'Production read exceeded the 15-second deadline.' }
        $entry.settled = $true; $entry.exit_code = $child.ExitCode
        $raw = $stdout.Result; $err = $stderr.Result
        [IO.File]::WriteAllText((Join-Path $runDir ($label + '.json')), $raw, $utf8)
        if (![string]::IsNullOrWhiteSpace($err)) { [IO.File]::WriteAllText((Join-Path $runDir ($label + '.stderr.txt')), $err, $utf8) }
        if ($child.ExitCode -ne 0) { throw 'Production read failed; stdout retained locally.' }
        return ($raw | ConvertFrom-Json)
    } finally {
        if ($null -ne $entry.pid -and !$entry.settled) {
            try { if (!$child.HasExited) { $child.Kill() }; $entry.settled = $child.WaitForExit(5000) } catch { $entry.settled = $false }
            if (!$entry.settled) { $script:childUnsettled = $true }
        }
        $child.Dispose()
    }
}

try {
    $service = New-Object -ComObject 'Schedule.Service'; $service.Connect(); $folder = $service.GetFolder('\')
    foreach ($record in $records) {
        if (Test-Path -LiteralPath $actionPath) { throw 'Action path must not exist.' }
        $definition = $service.NewTask(0)
        $definition.RegistrationInfo.Description = $marker; $definition.RegistrationInfo.URI = '\' + $record.name
        $definition.Principal.Id = 'R17Fixture'; $definition.Principal.UserId = $ownerSid
        $definition.Principal.LogonType = 3; $definition.Principal.RunLevel = 0
        $definition.Settings.Enabled = $false; $definition.Settings.Hidden = $record.hidden
        $definition.Settings.AllowDemandStart = $false
        $definition.Actions.Context = 'R17Fixture'; $definition.Actions.Create(0).Path = $actionPath
        if ($definition.Triggers.Count -ne 0) { throw 'Unexpected trigger in new definition.' }
        $record.attempted = $true; $record.cleanup = 'registration_unknown'
        # 2 = TASK_CREATE, 8 = TASK_DISABLE. Never UPDATE, CREATE_OR_UPDATE, or /F.
        $task = $folder.RegisterTaskDefinition($record.name, $definition, 10, $ownerSid, $null, 3, $null)
        $record.created = $true; $record.cleanup = 'pending'
        $record.xml = [string]$task.Xml
        if ([string]::IsNullOrWhiteSpace($record.xml)) { throw 'Missing registration XML snapshot; preserve task.' }
        Assert-OwnedDisabledTask $task $record
        [IO.File]::WriteAllText((Join-Path $runDir ($record.name + '.xml')), $record.xml, $utf8)
        $record.confirmed = $true
    }
    foreach ($record in $records) {
        try {
            foreach ($operation in @('status', 'export')) {
                $label = $operation + '-' + $(if ($record.hidden) { 'hidden' } else { 'visible' })
                $response = Invoke-ProductionRead @{ operation = $operation; name = '\' + $record.name } $label
                if (!$response.ok -or $response.task.name -cne $record.name -or [int]$response.task.state -ne 1 -or $response.task.xml -cne $record.xml) { throw "Unexpected production $label response." }
                $sid = [string]$response.task.owner
                if (!$sid.StartsWith('S-1-')) { $sid = ([Security.Principal.NTAccount]::new($sid)).Translate([Security.Principal.SecurityIdentifier]).Value }
                if ($sid -cne $ownerSid) { throw "Unexpected production $label principal." }
                $checks += $label
            }
        } catch { $record.confirmed = $false; throw }
    }
    try {
        $response = Invoke-ProductionRead @{ operation = 'list'; prefix = $prefix } 'list-visible-only'
        $rows = @($response.tasks)
        if (!$response.ok -or $rows.Count -ne 1 -or $rows[0].name -cne $records[0].name -or [int]$rows[0].state -ne 1 -or $rows[0].xml -cne $records[0].xml) { throw 'List did not return exactly the visible fixture.' }
        $sid = [string]$rows[0].owner
        if (!$sid.StartsWith('S-1-')) { $sid = ([Security.Principal.NTAccount]::new($sid)).Translate([Security.Principal.SecurityIdentifier]).Value }
        if ($sid -cne $ownerSid) { throw 'Unexpected production list principal.' }
    } catch { foreach ($record in $records) { $record.confirmed = $false }; throw }
    $checks += 'list-visible-only'
    foreach ($operation in @('status', 'export')) {
        $label = $operation + '-absent'
        $response = Invoke-ProductionRead @{ operation = $operation; name = '\' + $prefix + 'absent' } $label
        if ($response.ok -or $response.kind -cne 'task_absent') { throw "Unexpected production $label response." }
        $checks += $label
    }
    $readsPassed = $true
} catch { $failure = $_.Exception.Message }
finally {
    foreach ($record in $records) {
        if (!$record.created) { continue }
        $record.cleanup = 'retained'
        try {
            if ($childUnsettled -or !$record.confirmed -or $null -eq $record.xml) { throw 'Incomplete process settlement, ownership or unchanged snapshot evidence; preserve task.' }
            $current = $folder.GetTask($record.name)
            # Last read immediately precedes deletion; Windows provides no atomic compare-and-delete.
            Assert-OwnedDisabledTask $current $record
            $record.cleanup = 'delete_result_unknown'
            $folder.DeleteTask($record.name, 0)
            $record.cleanup = 'deletion_unconfirmed'
            if (@($folder.GetTasks(1) | Where-Object { $_.Name -ceq $record.name }).Count -ne 0) { throw 'Task still present after deletion; do not retry.' }
            $record.cleanup = 'removed'
        } catch { $record.error = $_.Exception.Message }
    }
    $clean = (@($records | Where-Object { $_.attempted -and $_.cleanup -ne 'removed' }).Count -eq 0)
    $receipt = [ordered]@{
        head = $head; source_sha256 = $sourceHash; reads_passed = $readsPassed; checks = $checks
        cleanup_complete = $clean; error = $failure; children = $children; tasks = $records
        claim = 'Unmodified #660 PowerShell script against native Scheduler; not the public Go backend'
    }
    [IO.File]::WriteAllText((Join-Path $runDir 'result.json'), ($receipt | ConvertTo-Json -Depth 8), $utf8)
}
[pscustomobject]@{ reads_passed = $readsPassed; cleanup_complete = $clean; evidence = $runDir; failure_recorded = ($null -ne $failure) } | ConvertTo-Json
if (!$readsPassed -or !$clean) { exit 1 }

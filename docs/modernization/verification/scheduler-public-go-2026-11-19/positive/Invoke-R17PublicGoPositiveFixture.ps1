# Proposal only: one separately approved positive Go run. Default has no scheduler access.
[CmdletBinding()]
param(
    [switch]$Apply,
    [string]$SourceFile,
    [string]$ProbePath,
    [string]$ProbeSHA256,
    [string]$RunId
)
$ErrorActionPreference = 'Stop'
$head = '1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3'
$sourceHash = 'e2594cbe0a0cca61038a09ea7d37a701132be378bec6b935236e8b4233ed43fa'
$workRoot = 'D:\dev\ChatGPT'
if (!$Apply) {
    [ordered]@{
        mode = 'PLAN_ONLY'; head = $head; source_sha256 = $sourceHash
        root = $workRoot; tasks = @('\mcp-r17-<RunId>-visible', '\mcp-r17-<RunId>-hidden')
        registration = 'TASK_CREATE | TASK_DISABLE = 10; no triggers; current interactive user; least privilege'
        reads = @('public Go Status and ExportXML of both tasks', 'public Go List: exactly visible')
        cleanup = 'Only successful creations; recheck definition, principal and disabled state; retain on uncertainty'
        task_execution = 'No Run, RunOwned or Stop requests; nonexistent action; demand start disabled'
        admission = 'Explicit new RunId and verified ProbeSHA256; existing run directory forbids reuse'
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
if ($RunId -cnotmatch '^[a-f0-9]{32}$' -or $ProbeSHA256 -notmatch '^[a-fA-F0-9]{64}$') { throw 'Apply requires an explicit new RunId and ProbeSHA256.' }
if ([string]::IsNullOrWhiteSpace($ProbePath)) { throw 'Apply requires the reviewed positive probe executable.' }
$ProbePath = [IO.Path]::GetFullPath($ProbePath)
if (!$ProbePath.StartsWith($workRoot + '\', [StringComparison]::OrdinalIgnoreCase)) { throw 'Probe must be inside the authorized work root.' }
if ([string]::Equals(([string]$env:MCPHUB_E2E_SCHEDULER).Trim(), 'none', [StringComparison]::OrdinalIgnoreCase)) { throw 'The scheduler noop override is forbidden.' }
$utf8 = [Text.UTF8Encoding]::new($false)
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if ($identity.IsSystem -or ![Environment]::UserInteractive -or [Diagnostics.Process]::GetCurrentProcess().SessionId -eq 0) { throw 'Use the current interactive user session, not a service or session 0.' }
$ownerSid = $identity.User.Value
$runId = $RunId
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
    [pscustomobject]@{ name = $prefix + $_; hidden = ($_ -eq 'hidden'); created = $false; confirmed = $false; attempted = $false; xml = $null; owner = $null; cleanup = 'not_created'; error = $null }
}
function Write-NewJSON([string]$path, $value) {
    $bytes = $utf8.GetBytes(($value | ConvertTo-Json -Depth 8))
    $stream = [IO.File]::Open($path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
    try { $stream.Write($bytes, 0, $bytes.Length); $stream.Flush($true) } finally { $stream.Dispose() }
}
Write-NewJSON (Join-Path $runDir 'intent.json') ([ordered]@{
    head = $head; source_sha256 = $sourceHash; run_id = $runId; principal_sid = $ownerSid
    probe_sha256 = $ProbeSHA256.ToLowerInvariant(); marker = $marker; action = $actionPath; names = @($records.name)
    policy = 'One new positive public Go run; CREATE-only, disabled, no triggers; no task execution'
})
$checks = @(); $children = @(); $childUnsettled = $false; $readsPassed = $false; $failure = $null; $folder = $null; $probeGuard = $null

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

function Invoke-PositiveGoRead([string]$manifestJSON) {
    $info = [Diagnostics.ProcessStartInfo]::new()
    $info.FileName = $ProbePath; $info.WorkingDirectory = $runDir
    $info.UseShellExecute = $false; $info.CreateNoWindow = $true
    $info.RedirectStandardInput = $true; $info.RedirectStandardOutput = $true; $info.RedirectStandardError = $true
    $info.StandardOutputEncoding = $utf8; $info.StandardErrorEncoding = $utf8
    $info.EnvironmentVariables['TEMP'] = $tempDir; $info.EnvironmentVariables['TMP'] = $tempDir
    $info.EnvironmentVariables['USERPROFILE'] = $profileDir
    $info.EnvironmentVariables['APPDATA'] = $appDataDir; $info.EnvironmentVariables['LOCALAPPDATA'] = $localAppDataDir
    $info.EnvironmentVariables['PSModuleAnalysisCachePath'] = Join-Path $moduleCacheDir 'ModuleAnalysisCache'
    $info.EnvironmentVariables['PATH'] = $powerShellDir + ';' + $systemDir
    $child = [Diagnostics.Process]::new(); $child.StartInfo = $info
    $entry = [pscustomobject]@{ check = 'public-go-positive'; start_attempted = $false; pid = $null; started_utc = $null; probe_sha256 = $ProbeSHA256.ToLowerInvariant(); settled = $false; capture_complete = $false; stdout_written = $false; stderr_written = $false; persistence_errors = @(); exit_code = $null }
    $stdout = $null; $stderr = $null; $raw = $null; $err = $null; $primaryError = $null
    $stdoutCaptured = $false; $stderrCaptured = $false; $events = @()
    $script:children += $entry
    try {
        $entry.start_attempted = $true
        if (!$child.Start()) { throw 'Positive Go probe was not started.' }
        $entry.pid = $child.Id; $entry.started_utc = $child.StartTime.ToUniversalTime().ToString('o')
        Write-NewJSON (Join-Path $runDir 'go-child-start.json') $entry
        $stdout = $child.StandardOutput.ReadToEndAsync(); $stderr = $child.StandardError.ReadToEndAsync()
        # The manifest contains private XML and Unicode; bypass the default stdin text encoding.
        $inputBytes = $utf8.GetBytes($manifestJSON); $inputStream = $child.StandardInput.BaseStream
        $inputStream.Write($inputBytes, 0, $inputBytes.Length); $inputStream.Close()
        # Normal settlement only: killing Go would not establish settlement of its PowerShell children.
        $child.WaitForExit(); $entry.exit_code = $child.ExitCode
        $raw = $stdout.Result; $stdoutCaptured = $true
        $err = $stderr.Result; $stderrCaptured = $true
        $entry.capture_complete = $true; $entry.settled = $true
        if ($child.ExitCode -ne 0) { throw 'Positive Go probe returned nonzero.' }
        $events = @($raw -split '\r?\n' | Where-Object { ![string]::IsNullOrWhiteSpace($_) } | ForEach-Object { $_ | ConvertFrom-Json })
    } catch { $primaryError = $_ }
    finally {
        if ($entry.start_attempted -and (!$entry.settled -or !$entry.capture_complete)) {
            try { $child.StandardInput.Close() } catch {}
            try {
                $child.WaitForExit(); $entry.exit_code = $child.ExitCode
                if ($null -ne $stdout -and $null -ne $stderr) {
                    $raw = $stdout.Result; $stdoutCaptured = $true
                    $err = $stderr.Result; $stderrCaptured = $true
                    $entry.capture_complete = $true; $entry.settled = $true
                }
            } catch {
                $entry.settled = $false; $entry.capture_complete = $false
                if ($null -eq $primaryError) { $primaryError = $_ }
            }
            if (!$entry.settled -or !$entry.capture_complete) { $script:childUnsettled = $true }
        }
        try { $child.Dispose() } catch { if ($null -eq $primaryError) { $primaryError = $_ } }
    }
    # Persist every acquired stream after settlement, including stdin/parse failure paths.
    if ($stdoutCaptured) {
        try { [IO.File]::WriteAllText((Join-Path $runDir 'positive-go.jsonl'), $raw, $utf8); $entry.stdout_written = $true }
        catch { $entry.persistence_errors += [pscustomobject]@{ stream = 'stdout'; error = $_.Exception.Message } }
    }
    if ($stderrCaptured) {
        try { [IO.File]::WriteAllText((Join-Path $runDir 'positive-go.stderr.txt'), $err, $utf8); $entry.stderr_written = $true }
        catch { $entry.persistence_errors += [pscustomobject]@{ stream = 'stderr'; error = $_.Exception.Message } }
    }
    if ($null -ne $primaryError) { throw $primaryError }
    if ($entry.persistence_errors.Count -ne 0) { throw 'Positive Go capture persistence failed; see child evidence.' }
    return $events
}

try {
    # Hold the admitted image against write/replacement until this owner exits its resource block.
    $probeGuard = [IO.File]::Open($ProbePath, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
    $sha256 = [Security.Cryptography.SHA256]::Create()
    try { $actualProbeHash = [BitConverter]::ToString($sha256.ComputeHash($probeGuard)).Replace('-', '') } finally { $sha256.Dispose() }
    if ($actualProbeHash -ine $ProbeSHA256) { throw 'Positive probe image hash mismatch; no tasks created.' }
    $profileDir = Join-Path $runDir 'profile'; $tempDir = Join-Path $runDir 'tmp'
    $appDataDir = Join-Path $profileDir 'AppData\Roaming'; $localAppDataDir = Join-Path $profileDir 'AppData\Local'
    $moduleCacheDir = Join-Path $localAppDataDir 'Microsoft\Windows\PowerShell'
    foreach ($directory in @($profileDir, $tempDir, $appDataDir, $localAppDataDir, $moduleCacheDir)) { $null = [IO.Directory]::CreateDirectory($directory) }
    $systemDir = [Environment]::SystemDirectory; $powerShellDir = Join-Path $systemDir 'WindowsPowerShell\v1.0'
    if (!(Test-Path -LiteralPath (Join-Path $powerShellDir 'powershell.exe') -PathType Leaf)) { throw 'Trusted system PowerShell is unavailable.' }
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
        $record.owner = [string]$task.Definition.Principal.UserId
        [IO.File]::WriteAllText((Join-Path $runDir ($record.name + '.xml')), $record.xml, $utf8)
        $record.confirmed = $true
    }
    $manifest = [ordered]@{ head = $head; prefix = $prefix; tasks = @($records | ForEach-Object { [ordered]@{ name = '\' + $_.name; hidden = $_.hidden; xml = $_.xml; owner = $_.owner } }) }
    Write-NewJSON (Join-Path $runDir 'go-manifest.json') $manifest
    $events = @(Invoke-PositiveGoRead ($manifest | ConvertTo-Json -Compress -Depth 5))
    $expected = @('status-visible', 'export-visible', 'status-hidden', 'export-hidden', 'list-visible-only')
    $results = @($events | Where-Object { $_.event -ceq 'result' })
    $overall = @($events | Where-Object { $_.event -ceq 'overall' })
    if ($results.Count -ne 5 -or $overall.Count -ne 1 -or $overall[0].pass -isnot [bool] -or $overall[0].pass -ne $true -or $overall[0].status -cne 'PASS' -or $events[-1].event -cne 'overall') { throw 'Incomplete positive Go protocol; fresh ownership guard decides cleanup.' }
    for ($index = 0; $index -lt 5; $index++) {
        if ($results[$index].check -cne $expected[$index] -or $results[$index].pass -isnot [bool] -or $results[$index].pass -ne $true) { throw 'Unexpected positive Go check order or result; fresh ownership guard decides cleanup.' }
    }
    $checks = $expected
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
    if ($null -ne $probeGuard) { $probeGuard.Dispose() }
    $receipt = [ordered]@{
        head = $head; source_sha256 = $sourceHash; probe_sha256 = $ProbeSHA256.ToLowerInvariant(); reads_passed = $readsPassed; checks = $checks
        cleanup_complete = $clean; error = $failure; children = $children; tasks = $records
        claim = 'Five positive public Go reads on disabled owned tasks; LastResult is observation only; no task execution'
    }
    Write-NewJSON (Join-Path $runDir 'result.json') $receipt
}
[pscustomobject]@{ reads_passed = $readsPassed; cleanup_complete = $clean; evidence = $runDir; failure_recorded = ($null -ne $failure) } | ConvertTo-Json
if (!$readsPassed -or !$clean) { exit 1 }

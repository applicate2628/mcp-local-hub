# PROPOSAL ONLY. -Apply requires a new, separate cleanup approval.
[CmdletBinding()]
param([switch]$Apply)
$ErrorActionPreference = 'Stop'
$runId = 'd55a6763eb064445a46ef592d52a9e6f'
$runDir = 'D:\dev\ChatGPT\mcp-r17-native-qualification-20261119\' + $runId
$name = 'mcp-r17-' + $runId + '-visible'; $taskPath = '\' + $name
if (!$Apply) {
    [ordered]@{ state = 'PLAN_ONLY'; task_path = $taskPath; evidence = $runDir; requires_new_approval = $true; creates_tasks = $false } | ConvertTo-Json
    return
}
$head = '1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3'
$sourceHash = 'e2594cbe0a0cca61038a09ea7d37a701132be378bec6b935236e8b4233ed43fa'
$fixtureHash = '6a7d123334ae13aaf5890a491813091a2a2a4831650d25940d1814ca40a2ee4c'
$hashes = [ordered]@{
    'intent.json' = 'eab630c66d1c377d7201f204c6b29f2fd6933a462e856936af4de5e2e5eb2d8d'
    'result.json' = 'c151564a1c5d873aa4f3197b91124289235b8d73e4d1d472f6f778afe0971833'
    ($name + '.xml') = '28affda1fe9d55a47e7bc5cbd9add47a041bc7e28c0e7d1da2b10f988b853c97'
    'status-visible.json' = '7f1acf1e4659982ec7992fb4868fc476707d160bb9e9d25998a30a4bdefcc13b'
    'status-visible.stderr.txt' = '2370f40cd9e212d09714414f67c671a9f624d59210af1cadae4922f5df21bba7'
}
$utf8 = [Text.UTF8Encoding]::new($false, $true)
$intentPath = $runDir + '\recovery-visible-intent.json'; $receiptPath = $runDir + '\recovery-visible-result.json'
$intentWritten = $false; $receiptWritten = $false; $deleteAttempted = $false; $absent = $false; $state = 'BLOCKED_BEFORE_DELETE'; $privateError = $null
function Get-PrincipalSID([string]$value) {
    if ($value.StartsWith('S-1-')) { return ([Security.Principal.SecurityIdentifier]::new($value)).Value }
    return ([Security.Principal.NTAccount]::new($value)).Translate([Security.Principal.SecurityIdentifier]).Value
}
function Write-ExclusiveJSON([string]$path, $value) {
    $bytes = $utf8.GetBytes(($value | ConvertTo-Json -Depth 6))
    $stream = [IO.File]::Open($path, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
    try { $stream.Write($bytes, 0, $bytes.Length); $stream.Flush($true) } finally { $stream.Dispose() }
}
try {
    if ([Environment]::OSVersion.Platform -ne 'Win32NT') { throw 'Recovery requires Windows.' }
    foreach ($path in @('D:\', 'D:\dev', 'D:\dev\ChatGPT', 'D:\dev\ChatGPT\mcp-r17-native-qualification-20261119', $runDir)) {
        $attributes = [IO.File]::GetAttributes($path)
        if (!($attributes -band [IO.FileAttributes]::Directory) -or ($attributes -band [IO.FileAttributes]::ReparsePoint)) { throw 'Recovery evidence directory is missing or redirected.' }
    }
    # CREATE_NEW is the one-shot boundary. Never remove/reuse this marker to retry.
    if (Test-Path -LiteralPath $receiptPath) { throw 'A recovery result already exists; deletion must not be attempted again.' }
    Write-ExclusiveJSON $intentPath ([ordered]@{
        task_path = $taskPath; run_id = $runId; head = $head; source_sha256 = $sourceHash; original_fixture_sha256 = $fixtureHash
        expected_files = $hashes; started_utc = [DateTime]::UtcNow.ToString('o'); verification = 'pending'; authority = 'Separate approved cleanup; original confirmed=false remains unchanged'
    })
    $intentWritten = $true
    $saved = @{}; $sha256 = [Security.Cryptography.SHA256]::Create()
    try {
        foreach ($file in $hashes.Keys) {
            $bytes = [IO.File]::ReadAllBytes($runDir + '\' + $file)
            $actual = [BitConverter]::ToString($sha256.ComputeHash($bytes)).Replace('-', '')
            if ($actual -ine $hashes[$file]) { throw ('Historical file hash mismatch: ' + $file) }
            $saved[$file] = $utf8.GetString($bytes)
        }
    } finally { $sha256.Dispose() }
    $intent = $saved['intent.json'] | ConvertFrom-Json; $result = $saved['result.json'] | ConvertFrom-Json
    $response = $saved['status-visible.json'] | ConvertFrom-Json; $xml = $saved[$name + '.xml']
    $marker = 'R17 #660 ' + $runId + ' ' + [char]0x0416 + [char]0x00A7; $actionPath = $runDir + '\never-created-fixture-action.exe'
    if ($intent.head -cne $head -or $result.head -cne $head -or $intent.source_sha256 -cne $sourceHash -or $result.source_sha256 -cne $sourceHash) { throw 'Historical source identity mismatch.' }
    if ($intent.run_id -cne $runId -or $intent.marker -cne $marker -or $intent.action -cne $actionPath -or @($intent.names).Count -ne 2 -or $intent.names -cnotcontains $name -or $intent.names -cnotcontains ('mcp-r17-' + $runId + '-hidden')) { throw 'Historical fixture identity mismatch.' }
    $records = @($result.tasks | Where-Object { $_.name -ceq $name })
    if ($records.Count -ne 1 -or $records[0].created -ne $true -or $records[0].attempted -ne $true -or $records[0].confirmed -ne $false -or $records[0].hidden -ne $false -or $records[0].cleanup -cne 'retained' -or $records[0].xml -cne $xml -or [string]::IsNullOrWhiteSpace($xml)) { throw 'Successful creation and retained snapshot are not established.' }
    $children = @($result.children)
    if ($children.Count -ne 1 -or $children[0].check -cne 'status-visible' -or $children[0].settled -ne $true -or $children[0].exit_code -ne 0) { throw 'Original read child is not confirmed settled with exit zero.' }
    $ownerSid = Get-PrincipalSID ([string]$intent.principal_sid)
    if ([Security.Principal.WindowsIdentity]::GetCurrent().User.Value -cne $ownerSid) { throw 'Current user differs from the original creator.' }
    if ($response.ok -ne $true -or $response.task.name -cne $name -or [int]$response.task.state -ne 1 -or $response.task.xml -cne $xml -or (Get-PrincipalSID ([string]$response.task.owner)) -cne $ownerSid) { throw 'Saved production stdout does not establish the expected disabled task.' }
    # The reviewed stderr is accepted only by its exact pinned hash, not a general stderr exception.
    $service = New-Object -ComObject 'Schedule.Service'; $service.Connect(); $folder = $service.GetFolder('\')
    $task = $folder.GetTask($name); $definition = $task.Definition
    if ([string]$task.Name -cne $name -or [string]$task.Path -cne $taskPath -or (Get-PrincipalSID ([string]$definition.Principal.UserId)) -cne $ownerSid) { throw 'Current task path or execution principal changed.' }
    if ($task.Enabled -ne $false -or [int]$task.State -ne 1 -or $definition.Settings.Enabled -ne $false -or $definition.Settings.AllowDemandStart -ne $false) { throw 'Current task is not disabled and inert.' }
    if ($definition.Settings.Hidden -ne $false -or $definition.Triggers.Count -ne 0) { throw 'Current visibility or triggers changed.' }
    if ([int]$definition.Principal.LogonType -ne 3 -or [int]$definition.Principal.RunLevel -ne 0) { throw 'Current principal logon or run level changed.' }
    if ([string]$definition.RegistrationInfo.Description -cne $marker -or [string]$definition.RegistrationInfo.URI -cne $taskPath -or $definition.Actions.Count -ne 1) { throw 'Current marker, URI or action count changed.' }
    $action = $definition.Actions.Item(1)
    if ([int]$action.Type -ne 0 -or [string]$action.Path -cne $actionPath -or [string]$action.Arguments -cne '' -or (Test-Path -LiteralPath $actionPath)) { throw 'Current action changed or its executable exists.' }
    if ([string]$task.Xml -cne $xml) { throw 'Current complete XML differs from the successful creation snapshot.' }
    # Last native comparison precedes the sole Delete; Scheduler has no atomic compare-and-delete.
    $deleteAttempted = $true; $state = 'DELETE_RESULT_UNKNOWN'
    $folder.DeleteTask($name, 0)
    $state = 'DELETION_UNCONFIRMED'
    if (@($folder.GetTasks(1) | Where-Object { $_.Name -ceq $name }).Count -ne 0) { throw 'Exact target remains present; no retry is permitted.' }
    $absent = $true; $state = 'ABSENCE_OBSERVED'
} catch { $privateError = $_.Exception.ToString() }
finally {
    if ($intentWritten) {
        try {
            Write-ExclusiveJSON $receiptPath ([ordered]@{
                task_path = $taskPath; head = $head; source_sha256 = $sourceHash; original_fixture_sha256 = $fixtureHash
                delete_attempted = $deleteAttempted; absence_observed = $absent; state = $state; error = $privateError; completed_utc = [DateTime]::UtcNow.ToString('o')
            })
            $receiptWritten = $true
        } catch { $receiptWritten = $false }
    }
}
[ordered]@{ state = $state; delete_attempted = $deleteAttempted; absence_observed = $absent; receipt_written = $receiptWritten; evidence = $runDir } | ConvertTo-Json
if (!$absent -or !$receiptWritten) { exit 1 }

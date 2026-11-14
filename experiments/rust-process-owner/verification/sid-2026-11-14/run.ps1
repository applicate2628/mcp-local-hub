param(
    [Parameter(Mandatory=$true)][string]$TaskRoot,
    [Parameter(Mandatory=$true)][string]$CargoHome,
    [string]$Run = 'final'
)
$ErrorActionPreference = 'Stop'
if ($Run -notmatch '^[a-zA-Z0-9_-]+$') { throw 'Run must be a single directory name' }
$crate = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$task = [IO.Path]::GetFullPath($TaskRoot)
$cargoCache = [IO.Path]::GetFullPath($CargoHome)
if (-not (Test-Path -LiteralPath $cargoCache -PathType Container)) { throw 'Existing offline Cargo cache required' }
$results = Join-Path $task ('evidence\' + $Run)
if (Test-Path -LiteralPath $results) { throw 'Do not overwrite a previous run' }
$copies = Join-Path $task ('copies-' + $Run)
if (Test-Path -LiteralPath $copies) { throw 'Copy destination must be new' }
New-Item -ItemType Directory -Path $results,$copies -Force | Out-Null
$env:TEMP = Join-Path $task 'tmp'
$env:TMP = $env:TEMP
$env:CARGO_HOME = $cargoCache
$env:CARGO_NET_OFFLINE = 'true'
$env:RUSTUP_OFFLINE = '1'
$env:CARGO_TARGET_DIR = Join-Path $task 'target'
$env:MCPHUB_RX02_PARENT_SENTINEL = 'owned-test-parent-only-20261113'
New-Item -ItemType Directory -Path $env:TEMP -Force | Out-Null
$utf8 = New-Object Text.UTF8Encoding($false)
$steps = New-Object 'Collections.Generic.List[object]'

function Write-Text([string]$Path, [string]$Text) {
    $normalized = (($Text -replace "`r`n", "`n") -replace '[ \t]+(?=\n|$)', '').TrimEnd() + "`n"
    [IO.File]::WriteAllText($Path, $normalized, $utf8)
}
function Hash-File([string]$Path) {
    $stream = [IO.File]::OpenRead($Path)
    $hash = [Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($hash.ComputeHash($stream))).Replace('-', '').ToLowerInvariant() }
    finally { $stream.Dispose(); $hash.Dispose() }
}
function Source-Hashes([string]$Root) {
    $files = @(Get-ChildItem -LiteralPath (Join-Path $Root 'src'),(Join-Path $Root 'tests') -Recurse -File)
    $files += Get-Item -LiteralPath (Join-Path $Root 'Cargo.toml'),(Join-Path $Root 'Cargo.lock')
    return @($files | Sort-Object FullName | ForEach-Object {
        [pscustomobject]@{ path=$_.FullName.Substring($Root.Length + 1).Replace('\','/'); sha256=(Hash-File $_.FullName) }
    })
}
function Scrub([string]$Text) {
    return $Text.Replace($task, '<task-root>').Replace($cargoCache, '<cargo-cache>')
}
function Invoke-Cargo([string]$Name, [string]$Root, [string[]]$Arguments, [int]$Expected=0, [string]$Oracle='', [string]$FailurePattern='') {
    $env:MCPHUB_RX02_FIXTURES = Join-Path $task ('fixtures-' + $Run + '-' + $Name)
    New-Item -ItemType Directory -Path $env:MCPHUB_RX02_FIXTURES | Out-Null
    Push-Location $Root
    $started = [DateTime]::UtcNow.ToString('o')
    try {
        # Windows PowerShell represents native stderr as ErrorRecord. Compiler
        # progress on stderr is not a failed command; its exit code decides.
        $ErrorActionPreference = 'Continue'
        $output = & cargo @Arguments 2>&1
        $exitCode = $LASTEXITCODE
        $ErrorActionPreference = 'Stop'
        $text = ($output | ForEach-Object { $_.ToString() }) -join "`n"
        $semantic = (-not $Oracle) -or (($text -match 'test result: FAILED') -and ($text -match $Oracle) -and ($text -match $FailurePattern))
        $step = [pscustomobject]@{name=$Name; command=@('cargo')+$Arguments; expected=$Expected; actual=$exitCode; semanticOracle=$Oracle; failurePattern=$FailurePattern; oracleMatched=$semantic; startedUtc=$started; finishedUtc=[DateTime]::UtcNow.ToString('o')}
        $steps.Add($step)
        [IO.File]::AppendAllText((Join-Path $results 'run.log'), (Scrub ("## $Name`n" + $text)).TrimEnd()+"`n", $utf8)
        Write-Text (Join-Path $results 'steps.json') (ConvertTo-Json -InputObject @($steps.ToArray()) -Depth 5)
        Write-Output "$Name exit=$exitCode expected=$Expected semantic=$semantic"
        if ($exitCode -ne $Expected -or -not $semantic) { throw "Unexpected result: $Name" }
    } finally { $ErrorActionPreference = 'Stop'; Pop-Location }
}
function Copy-Crate([string]$Name) {
    $destination = Join-Path $copies $Name
    New-Item -ItemType Directory -Path $destination | Out-Null
    foreach ($part in @('src','tests','Cargo.toml','Cargo.lock')) {
        Copy-Item -LiteralPath (Join-Path $crate $part) -Destination $destination -Recurse
    }
    return $destination
}
function Replace-First([string]$Path, [string]$Before, [string]$After) {
    $text = [IO.File]::ReadAllText($Path)
    $at = $text.IndexOf($Before, [StringComparison]::Ordinal)
    if ($at -lt 0) { throw 'Mutation preimage unavailable' }
    Write-Text $Path ($text.Substring(0,$at) + $After + $text.Substring($at+$Before.Length))
}

$before = Source-Hashes $crate
Write-Text (Join-Path $results 'source-hashes.json') (ConvertTo-Json -InputObject $before -Depth 4)
Write-Text (Join-Path $results 'runner.sha256') (Hash-File $PSCommandPath)
$metadata = [ordered]@{
    sourceGitHead = ((& git -C $crate rev-parse HEAD) -join '')
    rustc = ((& rustc --version) -join '')
    cargo = ((& cargo --version) -join '')
    system = [Environment]::OSVersion.VersionString
    architecture = $env:PROCESSOR_ARCHITECTURE
    sourceIdentity = 'git head plus exact source hashes; working overlay is permitted and explicit'
}
Write-Text (Join-Path $results 'environment.json') (ConvertTo-Json $metadata)
Invoke-Cargo 'fmt' $crate @('fmt','--all','--','--check')
Invoke-Cargo 'debug' $crate @('test','--offline','--locked','--','--test-threads=1')
Invoke-Cargo 'release' $crate @('test','--release','--offline','--locked','--','--test-threads=1')
Invoke-Cargo 'clippy' $crate @('clippy','--offline','--locked','--all-targets','--','-D','warnings')

$mutations = @(
    @{ name='ambient-process'; before='process.as_raw_handle()'; after='windows_sys::Win32::System::Threading::GetCurrentProcess()'; test='token_user_open_failure_uses_original_process_and_keeps_owner'; failure='token lookup ignored retained process handle' },
    @{ name='ignore-close'; before='CloseHandle(raw_token) } == 0'; after='CloseHandle(raw_token) } == 0 && false'; test='token_user_protected_close_preserves_both_outcomes'; failure='protected token close became success or lost its receipt' },
    @{ name='erase-query-error'; before='observation: result.err(),'; after='observation: None,'; test='token_user_protected_close_preserves_both_outcomes'; failure='token cleanup erased the query error' },
    @{ name='wrong-close-code'; before='let code = unsafe { GetLastError() } as i32;'; after='let code = unsafe { GetLastError() } as i32 + 1;'; test='token_user_protected_close_preserves_both_outcomes'; failure='token close changed native error' },
    @{ name='ignore-returned-length'; before='let header_length = size_of::<TOKEN_USER>();'; after='let returned = size_of_val(buffer); let header_length = size_of::<TOKEN_USER>();'; test='rejects_short_headers_and_lengths_outside_returned_storage'; failure='called .*unwrap_err.* on an .*Ok.* value' },
    @{ name='accept-invalid-sid'; before='if valid == 0 {'; after='if false {'; test='rejects_invalid_revision_excessive_count_and_truncated_subauthorities'; failure='called .*unwrap_err.* on an .*Ok.* value' }
)
$env:CARGO_TARGET_DIR = Join-Path $task 'target-controls-sid'
foreach ($mutation in $mutations) {
    $copy = Copy-Crate $mutation.name
    Replace-First (Join-Path $copy 'src\token_user.rs') $mutation.before $mutation.after
    Write-Text (Join-Path $results ($mutation.name+'-hashes.json')) (ConvertTo-Json -InputObject (Source-Hashes $copy) -Depth 4)
    Invoke-Cargo $mutation.name $copy @('test','--offline','--locked','--lib',$mutation.test,'--','--test-threads=1') 101 ('test .*'+[regex]::Escape($mutation.test)+' .*FAILED') $mutation.failure
}
Write-Text (Join-Path $results 'mutations.json') (ConvertTo-Json -InputObject $mutations -Depth 4)

$after = Source-Hashes $crate
if ((ConvertTo-Json -InputObject $before -Compress) -ne (ConvertTo-Json -InputObject $after -Compress)) { throw 'Normal source changed during copy-only controls' }
Write-Text (Join-Path $results 'COMPLETED') 'All expected exits and semantic negative controls matched; ordinary source unchanged.'
Write-Output ('Completed: ' + $results)
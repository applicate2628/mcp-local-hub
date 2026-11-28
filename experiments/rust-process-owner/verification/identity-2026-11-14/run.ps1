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
    @{ name='recapture'; before='let original = (*self.original_creation).map_err(io::Error::from_raw_os_error)?;'; after='let original = creation_time_100ns(self.handle).map_err(io::Error::from_raw_os_error)?;'; test='identity_initial_capture_error_is_not_repaired_by_later_success'; failure='called .*unwrap_err.* on an .*Ok.* value' },
    @{ name='any-job'; before='self.job.as_raw_handle(),'; after='null_mut(),'; test='identity_membership_checks_exact_job_instead_of_any_job'; failure='membership in another Job was accepted' },
    @{ name='failed-wait'; before='WAIT_FAILED => Err(io::Error::last_os_error()),'; after='WAIT_FAILED => Ok(RootWait::NotSignaled),'; test='identity_wait_failure_is_not_a_liveness_result'; failure='failed observation became a liveness value: NotSignaled' },
    @{ name='signaled-alive'; before='WAIT_OBJECT_0 => Ok(RootWait::Signaled),'; after='WAIT_OBJECT_0 => Ok(RootWait::NotSignaled),'; test='identity_signaled_process_with_exit_259_is_not_alive_or_resumed'; failure='(?s)left: NotSignaled\s+right: Signaled' }
)
$env:CARGO_TARGET_DIR = Join-Path $task 'target-controls'
foreach ($mutation in $mutations) {
    $copy = Copy-Crate $mutation.name
    Replace-First (Join-Path $copy 'src\windows.rs') $mutation.before $mutation.after
    Write-Text (Join-Path $results ($mutation.name+'-hashes.json')) (ConvertTo-Json -InputObject (Source-Hashes $copy) -Depth 4)
    Invoke-Cargo $mutation.name $copy @('test','--offline','--locked','--lib',$mutation.test,'--','--test-threads=1') 101 ('test .*'+[regex]::Escape($mutation.test)+' .*FAILED') $mutation.failure
}
Write-Text (Join-Path $results 'mutations.json') (ConvertTo-Json -InputObject $mutations -Depth 4)

# Force a real failing GetProcessTimes call exactly at initial capture in an
# isolated copy: the original primary-thread handle is not a process handle.
# No normal runtime hook, dependency or public export is added.
$capture = Copy-Crate 'native-capture'
$captureSource = Join-Path $capture 'src\windows.rs'
Replace-First $captureSource 'let original_creation = creation_time_100ns(process.as_handle());' 'let original_creation = creation_time_100ns(thread.as_handle());'
$captureTest = @'

#[test]
fn identity_real_capture_failure_keeps_owner() {
    let mut owner = suspended();
    let initial = owner.original_creation.expect_err("forced native capture unexpectedly succeeded");
    assert_ne!(initial, 0);
    eprintln!("controlled initial GetProcessTimes native error: {initial}");
    assert!(owner.root_pid() > 0);
    assert_eq!(owner.wait_root(Duration::ZERO).unwrap(), None);
    assert_eq!(owner.active_processes().unwrap(), 1);
    let error = owner.admit_and_resume(|view| view.creation_time_100ns().map(|_| ())).unwrap_err();
    assert_eq!(error.raw_os_error(), Some(initial));
    stop_and_finish(&mut owner);
}
'@
$captureTestsPath = Join-Path $capture 'src\native_failure_tests.rs'
Write-Text $captureTestsPath ([IO.File]::ReadAllText($captureTestsPath) + $captureTest)
Write-Text (Join-Path $results 'native-capture-hashes.json') (ConvertTo-Json -InputObject (Source-Hashes $capture) -Depth 4)
Invoke-Cargo 'native-capture' $capture @('test','--offline','--locked','--lib','identity_real_capture_failure_keeps_owner','--','--test-threads=1','--nocapture')
# A second copy detects the exact regression this positive fixture qualifies:
# returning Err after successful CreateProcess instead of retaining the owner.
$ownerless = Join-Path $copies 'ownerless-capture'
Copy-Item -LiteralPath $capture -Destination $ownerless -Recurse
Replace-First (Join-Path $ownerless 'src\windows.rs') 'let original_creation = creation_time_100ns(thread.as_handle());' 'eprintln!("negative control reached initial capture after successful CreateProcessW"); let original_creation = Ok(creation_time_100ns(thread.as_handle()).map_err(io::Error::from_raw_os_error)?);'
Write-Text (Join-Path $results 'ownerless-capture-hashes.json') (ConvertTo-Json -InputObject (Source-Hashes $ownerless) -Depth 4)
Invoke-Cargo 'ownerless-capture' $ownerless @('test','--offline','--locked','--lib','identity_real_capture_failure_keeps_owner','--','--test-threads=1') 101 'test .*identity_real_capture_failure_keeps_owner .*FAILED' '(?s)negative control reached initial capture after successful CreateProcessW.*called .*unwrap.* on an .*Err.* value: Os \{ code: 6,'

$after = Source-Hashes $crate
if ((ConvertTo-Json -InputObject $before -Compress) -ne (ConvertTo-Json -InputObject $after -Compress)) { throw 'Normal source changed during copy-only controls' }
Write-Text (Join-Path $results 'COMPLETED') 'All expected exits and semantic negative controls matched; ordinary source unchanged.'
Write-Output ('Completed: ' + $results)

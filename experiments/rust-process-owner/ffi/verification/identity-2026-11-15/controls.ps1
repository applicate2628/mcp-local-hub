[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$SourceRoot,
    [Parameter(Mandatory=$true)][string]$NewDll,
    [Parameter(Mandatory=$true)][string]$Child,
    [Parameter(Mandatory=$true)][string]$OldDll,
    [string]$TaskRoot = 'D:\dev\ChatGPT\mcphub-ffi-identity-20261115',
    [string]$CargoHome = 'D:\dev\ChatGPT\mcphub-rx02-stdio-20261113\cargo-home'
)
# Representative controls, not exhaustive fuzzing or real Windows failure injection.
# Original sources are read only; each mutation uses a new private source copy.
# Run after normal qualification passes and compiler/test inputs are frozen.
# No installation, download, git/CI action, deletion or trash operation is performed.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$Utf8 = New-Object System.Text.UTF8Encoding($false)
$Lf = [string][char]10

function Allowed-Path([string]$Path) {
    $full = [IO.Path]::GetFullPath($Path)
    if (!$full.StartsWith('D:\dev\ChatGPT\', [StringComparison]::OrdinalIgnoreCase)) {
        throw "Path is outside the authorized work area: $full"
    }
    return $full.TrimEnd('\')
}
function Digest([string]$Path) {
    $hash = [Security.Cryptography.SHA256]::Create()
    try {
        return ([BitConverter]::ToString($hash.ComputeHash([IO.File]::ReadAllBytes($Path)))).Replace('-','').ToLowerInvariant()
    } finally { $hash.Dispose() }
}
function Write-Lf([string]$Path, [string]$Text) {
    [IO.File]::WriteAllText($Path, $Text.Replace([string][char]13,''), $Utf8)
}
function Write-Json([string]$Path, $Value) {
    Write-Lf $Path ((ConvertTo-Json -InputObject $Value -Depth 12) + $Lf)
}

$SourceRoot = Allowed-Path $SourceRoot
$TaskRoot = Allowed-Path $TaskRoot
$CargoHome = Allowed-Path $CargoHome
$NewDll = Allowed-Path $NewDll
$Child = Allowed-Path $Child
$OldDll = Allowed-Path $OldDll
foreach ($path in @($SourceRoot, $CargoHome, $NewDll, $Child, $OldDll)) {
    if (!(Test-Path -LiteralPath $path)) { throw "Required existing input not found: $path" }
}
$NativeSource = Join-Path $SourceRoot 'experiments\rust-process-owner'
$RunId = [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssfffffffZ')
$RunRoot = Join-Path $TaskRoot ("evidence\controls\runs\" + $RunId)
if (Test-Path -LiteralPath $RunRoot) { throw "Fresh run directory already exists: $RunRoot" }
[void][IO.Directory]::CreateDirectory($RunRoot)
$TempRoot = Join-Path $TaskRoot ("tmp\identity-controls\" + $RunId)
[void][IO.Directory]::CreateDirectory($TempRoot)
$CacheRoot = Join-Path $TaskRoot 'cache\identity-controls'
[void][IO.Directory]::CreateDirectory($CacheRoot)

# Copy only manifests and compiler/test input trees, never verification, targets,
# private markers, caches, repository metadata or another worktree.
$SourceFiles = New-Object 'System.Collections.Generic.List[object]'
foreach ($relative in @('Cargo.toml','Cargo.lock','ffi\Cargo.toml','ffi\Cargo.lock')) {
    $path = Join-Path $NativeSource $relative
    if (!(Test-Path -LiteralPath $path -PathType Leaf)) { throw "Missing manifest: $relative" }
    [void]$SourceFiles.Add([pscustomobject]@{ Path=$relative; Bytes=(Get-Item -LiteralPath $path).Length; SHA256=(Digest $path) })
}
foreach ($relative in @('src','ffi\src','ffi\include','ffi\go')) {
    $directory = Join-Path $NativeSource $relative
    foreach ($item in @(Get-ChildItem -LiteralPath $directory -Recurse -Force)) {
        if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "Refusing a source reparse point: $($item.FullName)"
        }
        if (!$item.PSIsContainer) {
            $name = $item.FullName.Substring($NativeSource.Length + 1)
            [void]$SourceFiles.Add([pscustomobject]@{ Path=$name; Bytes=$item.Length; SHA256=(Digest $item.FullName) })
        }
    }
}
$SourceManifest = Join-Path $RunRoot 'source-inputs.json'
Write-Json $SourceManifest @($SourceFiles.ToArray())
$SourceManifestHash = Digest $SourceManifest
$Cases = New-Object 'System.Collections.Generic.List[object]'
$ArtifactHashes = [ordered]@{
    QualifiedNewDll = @{ Path=$NewDll; SHA256=(Digest $NewDll) }
    LegacyDll = @{ Path=$OldDll; SHA256=(Digest $OldDll) }
    Child = @{ Path=$Child; SHA256=(Digest $Child) }
}
$Metadata = [ordered]@{
    Schema=1; StartedUtc=[DateTime]::UtcNow.ToString('o'); RunId=$RunId
    SourceRoot=$SourceRoot; NativeSource=$NativeSource; SourceManifestSHA256=$SourceManifestHash
    Inputs=$ArtifactHashes
    Scope='Eight representative malformed producers; one legacy baseline; four weakened consumers. Synthetic errors do not establish spontaneous Windows failure behavior.'
    QualifiedNewDllUse='Provenance input only; normal qualification belongs to the main runner.'
    SourceUntouched=$false; Completed=$false
    CargoHome=$CargoHome; WorkingRoot=$RunRoot; PrivateFixtures=$TempRoot
    Cases=@()
}
function Save-Cases {
    $Metadata.Cases = @($Cases.ToArray())
    Write-Json (Join-Path $RunRoot 'cases.json') $Metadata
}
function Assert-Source-Unchanged {
    foreach ($entry in $SourceFiles) {
        if ((Digest (Join-Path $NativeSource $entry.Path)) -ne $entry.SHA256) {
            throw "Original compiler/test input changed during controls: $($entry.Path)"
        }
    }
}
function Copy-Source([string]$Name) {
    $destination = Join-Path $RunRoot ("copies\" + $Name)
    if (Test-Path -LiteralPath $destination) { throw "Copy already exists: $Name" }
    [void][IO.Directory]::CreateDirectory($destination)
    foreach ($entry in $SourceFiles) {
        $target = Join-Path $destination $entry.Path
        [void][IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName($target))
        [IO.File]::Copy((Join-Path $NativeSource $entry.Path), $target, $false)
        if ((Digest $target) -ne $entry.SHA256) { throw "Copy changed input bytes: $($entry.Path)" }
    }
    return $destination
}
function Mutate-Once([string]$Copy, [string]$Relative, [string]$Pattern, [string]$Replacement, [string]$Purpose) {
    $path = Join-Path $Copy $Relative
    $before = Digest $path
    $original = [IO.File]::ReadAllText($path).Replace([string][char]13,'')
    $expression = New-Object Text.RegularExpressions.Regex($Pattern)
    $found = $expression.Matches($original)
    if ($found.Count -ne 1) { throw "Mutation needs exactly one anchor ($Relative): found $($found.Count); $Purpose" }
    $match = $found[0]
    $updated = $original.Substring(0,$match.Index) + $Replacement + $original.Substring($match.Index+$match.Length)
    if ($updated -eq $original) { throw "Mutation is a no-op: $Purpose" }
    Write-Lf $path $updated
    return [pscustomobject]@{ Path=$Relative; Purpose=$Purpose; BeforeSHA256=$before; AfterSHA256=(Digest $path); Pattern=$Pattern; Replacement=$Replacement }
}
function Invoke-Logged([string]$Executable, [string[]]$Arguments, [string]$Directory, [string]$Log) {
    $start = [DateTime]::UtcNow
    $previous = $ErrorActionPreference
    Push-Location -LiteralPath $Directory
    try {
        $ErrorActionPreference = 'Continue'
        $lines = @(& $Executable @Arguments 2>&1)
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previous
        Pop-Location
    }
    $text = ($lines | ForEach-Object { $_.ToString() }) -join $Lf
    Write-Lf $Log ($text + $Lf)
    return [pscustomobject]@{
        Executable=$Executable; Arguments=$Arguments; WorkingDirectory=$Directory
        ExitCode=$code; StartedUtc=$start.ToString('o'); FinishedUtc=[DateTime]::UtcNow.ToString('o')
        Log=[IO.Path]::GetFileName($Log); LogSHA256=(Digest $Log)
    }
}
function Build-Dll([string]$Copy, [string]$Name) {
    $env:CARGO_TARGET_DIR = Join-Path $RunRoot ("targets\" + $Name)
    $log = Join-Path $RunRoot ($Name + '.cargo.log')
    $build = Invoke-Logged 'cargo.exe' @('build','--offline','--locked','--manifest-path',(Join-Path $Copy 'ffi\Cargo.toml')) $Copy $log
    if ($build.ExitCode -ne 0) { throw "Control DLL did not compile ($Name); this is not a successful control. See $log" }
    $dll = Join-Path $env:CARGO_TARGET_DIR 'debug\mcphub_owner_ffi_spike.dll'
    if (!(Test-Path -LiteralPath $dll -PathType Leaf)) { throw "Expected built DLL missing: $dll" }
    return [pscustomobject]@{ Path=$dll; SHA256=(Digest $dll); Build=$build }
}
function Run-Go([string]$Copy, [string]$Name, [string]$Dll, [string]$Mode, [string]$Tag, [string]$TestName, [bool]$ExpectFailure, [string]$Diagnostic) {
    $env:MCPHUB_RX02_FFI_DLL = $Dll
    $env:MCPHUB_RX02_FFI_CHILD = $Child
    $env:MCPHUB_IDENTITY_FAULT = $Mode
    $env:MCPHUB_RX02_IDENTITY_FIXTURES = Join-Path $TempRoot $Name
    [void][IO.Directory]::CreateDirectory($env:MCPHUB_RX02_IDENTITY_FIXTURES)
    $log = Join-Path $RunRoot ($Name + '.go.jsonl')
    $result = Invoke-Logged 'go.exe' @('test','-json','-race','-count=1','-timeout=90s',('-tags='+$Tag),'-run',('^'+$TestName+'$'),'.') (Join-Path $Copy 'ffi\go') $log
    $events = @(
        foreach ($line in [IO.File]::ReadLines($log)) {
            if ($line.TrimStart().StartsWith('{')) {
                try { ConvertFrom-Json -InputObject $line -ErrorAction Stop } catch { }
            }
        }
    )
    $testEvents = @($events | Where-Object { $_.PSObject.Properties.Name -contains 'Test' -and $_.Test -eq $TestName })
    $runs = @($testEvents | Where-Object { $_.Action -eq 'run' }).Count
    $passes = @($testEvents | Where-Object { $_.Action -eq 'pass' }).Count
    $failures = @($testEvents | Where-Object { $_.Action -eq 'fail' }).Count
    $skips = @($events | Where-Object { $_.Action -eq 'skip' }).Count
    $output = (@($testEvents | Where-Object { $_.PSObject.Properties.Name -contains 'Output' } | ForEach-Object { $_.Output })) -join ''
    $hasDiagnostic = $Diagnostic -eq '' -or $output.Contains($Diagnostic)
    $accepted = if ($ExpectFailure) {
        $result.ExitCode -eq 1 -and $runs -eq 1 -and $failures -eq 1 -and $passes -eq 0 -and $skips -eq 0 -and $hasDiagnostic
    } else {
        $result.ExitCode -eq 0 -and $runs -eq 1 -and $passes -eq 1 -and $failures -eq 0 -and $skips -eq 0
    }
    return [pscustomobject]@{
        Command=$result; Test=$TestName; Mode=$Mode; ExpectedFailure=$ExpectFailure
        Runs=$runs; Passes=$passes; Failures=$failures; Skips=$skips
        IntendedDiagnostic=$Diagnostic; DiagnosticObserved=$hasDiagnostic; Accepted=$accepted
    }
}

$envNames = @('TEMP','TMP','GOTMPDIR','GOCACHE','GOMODCACHE','GOPATH','GOTOOLCHAIN','GOPROXY','GOSUMDB','GOTELEMETRY','CGO_ENABLED','CC','CARGO_HOME','CARGO_TARGET_DIR','CARGO_NET_OFFLINE','RUSTUP_OFFLINE','GOEXPERIMENT','MCPHUB_RX02_FFI_DLL','MCPHUB_RX02_FFI_CHILD','MCPHUB_IDENTITY_FAULT','MCPHUB_RX02_IDENTITY_FIXTURES')
$SavedEnvironment = @{}
foreach ($name in $envNames) { $SavedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
try {
    $env:TEMP=$TempRoot; $env:TMP=$TempRoot; $env:GOTMPDIR=$TempRoot
    $env:GOCACHE=Join-Path $CacheRoot 'go-build'
    $env:GOMODCACHE=Join-Path $CacheRoot 'go-mod'
    $env:GOPATH=Join-Path $CacheRoot 'go-path'
    foreach ($directory in @($env:GOCACHE,$env:GOMODCACHE,$env:GOPATH)) { [void][IO.Directory]::CreateDirectory($directory) }
    $env:GOTOOLCHAIN='local'; $env:GOPROXY='off'; $env:GOSUMDB='off'; $env:GOTELEMETRY='off'
    $env:CGO_ENABLED='1'; $env:CC='c:\msys64\ucrt64\bin\gcc.exe'
    $env:CARGO_HOME=$CargoHome; $env:CARGO_NET_OFFLINE='true'; $env:RUSTUP_OFFLINE='1'
    $env:GOEXPERIMENT='cgocheck2'
    Save-Cases

    $afterSid = 'value\.sid\[\.\.sid\.len\(\)\]\.copy_from_slice\(&sid\);'
    $sidCopy = 'value.sid[..sid.len()].copy_from_slice(&sid);'
    $tokenQuery = 'let sid = view\s*\.token_user_sid\(\)\s*\.map_err\(\|error\| record_token_error\(report, error\)\)\?;'
    $tokenFailure = @'
let sid = view
        .token_user_sid()
        .and_then(|_| Err::<Vec<u8>, TokenUserError>(TokenUserError::Cleanup {
            observation: OBSERVATION,
            failure: mcphub_process_owner_spike::NativeFailure {
                operation: "CloseToken",
                code: 6,
            },
        }))
        .map_err(|error| record_token_error(report, error))?;
'@
    $producerCases = @(
        @{ Name='identity-version'; Mode='invalid-observation'; Pattern=$afterSid; Replacement=$sidCopy+$Lf+'    value.version = VERSION + 1;'; Purpose='Unsupported observation schema version' },
        @{ Name='creation-zero'; Mode='invalid-observation'; Pattern=$afterSid; Replacement=$sidCopy+$Lf+'    value.creation_time_100ns = 0;'; Purpose='Missing process creation binding' },
        @{ Name='job-unknown'; Mode='invalid-observation'; Pattern=$afterSid; Replacement=$sidCopy+$Lf+'    value.job_membership = 0;'; Purpose='Unknown Job membership enum' },
        @{ Name='sid-revision'; Mode='invalid-observation'; Pattern=$afterSid; Replacement=$sidCopy+$Lf+'    value.sid[0] = 0;'; Purpose='Invalid binary SID revision' },
        @{ Name='sid-tail'; Mode='invalid-observation'; Pattern=$afterSid; Replacement=$sidCopy+$Lf+'    value.sid_len = 8;'+$Lf+'    value.sid[1] = 0;'+$Lf+'    value.sid[SID_CAPACITY - 1] = 1;'; Purpose='Well-shaped short SID prefix with forbidden nonzero unused storage' },
        @{ Name='late-report-version'; Mode='late-report'; Pattern='if report\.stage != STAGE_NONE && report\.stage != STAGE_COMPLETE \{'; Replacement='report.version = VERSION + 1;'+$Lf+'            if report.stage != STAGE_NONE && report.stage != STAGE_COMPLETE {'; Purpose='Incompatible report after actual policy approval and native resume' },
        @{ Name='query-close'; Mode='query-close'; Pattern=$tokenQuery; Replacement=$tokenFailure.Replace('OBSERVATION','Some(io::Error::from_raw_os_error(5))'); Purpose='Synthetic simultaneous query error 5 and token close error 6, after real token query/close succeeds' },
        @{ Name='close-only'; Mode='close-only'; Pattern=$tokenQuery; Replacement=$tokenFailure.Replace('OBSERVATION','None'); Purpose='Synthetic token close error 6 without a query error, after real token query/close succeeds' }
    )
    $ProducerArtifacts = @{}
    foreach ($case in $producerCases) {
        $copy = Copy-Source $case.Name
        $mutation = Mutate-Once $copy 'ffi\src\identity.rs' $case.Pattern $case.Replacement $case.Purpose
        $artifact = Build-Dll $copy $case.Name
        $test = Run-Go $copy $case.Name $artifact.Path $case.Mode 'identity_faults' 'TestIdentityForeignFault' $false ''
        [void]$Cases.Add([pscustomobject]@{ Name=$case.Name; Category='malformed-producer'; SourceManifestSHA256=$SourceManifestHash; Mutations=@($mutation); Artifact=$artifact; Test=$test })
        Save-Cases
        if (!$test.Accepted) { throw "Malformed producer control failed its semantic oracle: $($case.Name)" }
        $ProducerArtifacts[$case.Name] = $artifact
        Write-Output ("PASS producer " + $case.Name)
    }

    # Calibrate missing-export behavior using the unchanged optional loader.
    $legacyCopy = Copy-Source 'legacy-optional-baseline'
    $legacyTest = Run-Go $legacyCopy 'legacy-optional-baseline' $OldDll '' 'identity_legacy' 'TestIdentityUnsupportedPreservesLegacyAdmission' $false ''
    [void]$Cases.Add([pscustomobject]@{ Name='legacy-optional-baseline'; Category='legacy-baseline'; SourceManifestSHA256=$SourceManifestHash; Mutations=@(); Artifact=$ArtifactHashes.LegacyDll; Test=$legacyTest })
    Save-Cases
    if (!$legacyTest.Accepted) { throw 'Old-DLL baseline failed; the optional-export negative control is invalid.' }
    Write-Output 'PASS legacy optional baseline'

    $negativeCases = @(
        @{ Name='unchecked-identity-version'; Producer='identity-version'; Mode='invalid-observation'; Relative='ffi\go\identity_windows.go'; Pattern='if raw\.size != C\.sizeof_mh_identity \|\| raw\.version != C\.MH_ID_VERSION \|\| raw\.reserved != 0 \{'; Replacement='if false {'; Purpose='Remove observation schema validation'; Diagnostic='malformed identity observation reached policy' },
        @{ Name='unchecked-report-version'; Producer='late-report-version'; Mode='late-report'; Relative='ffi\go\identity_windows.go'; Pattern='if raw\.size != C\.sizeof_mh_identity_report \|\| raw\.version != C\.MH_ID_VERSION \{'; Replacement='if false {'; Purpose='Remove report schema validation'; Diagnostic='malformed identity report was accepted or callback was lost' },
        @{ Name='lost-token-close'; Producer='query-close'; Mode='query-close'; Relative='ffi\go\identity_windows.go'; Pattern='TokenCloseError: int32\(raw\.token_close_error\)'; Replacement='TokenCloseError: 0'; Purpose='Discard independent token close error'; Diagnostic='token close receipt was lost or changed' }
    )
    foreach ($case in $negativeCases) {
        $copy = Copy-Source $case.Name
        $mutation = Mutate-Once $copy $case.Relative $case.Pattern $case.Replacement $case.Purpose
        $artifact = $ProducerArtifacts[$case.Producer]
        $test = Run-Go $copy $case.Name $artifact.Path $case.Mode 'identity_faults' 'TestIdentityForeignFault' $true $case.Diagnostic
        [void]$Cases.Add([pscustomobject]@{ Name=$case.Name; Category='weakened-consumer'; SourceManifestSHA256=$SourceManifestHash; Mutations=@($mutation); Artifact=@{ ProducerCase=$case.Producer; Path=$artifact.Path; SHA256=$artifact.SHA256 }; Test=$test })
        Save-Cases
        if (!$test.Accepted) { throw "Consumer mutation was not detected by its intended assertion: $($case.Name)" }
        Write-Output ("PASS negative " + $case.Name)
    }

    $requiredCopy = Copy-Source 'required-identity-export'
    $required = Mutate-Once $requiredCopy 'ffi\go\bridge_windows.go' 'p=GetProcAddress\(b->module,"mh_admit_identity1"\);' ('p=GetProcAddress(b->module,"mh_admit_identity1");'+$Lf+'    if (!p) { *error=ERROR_PROC_NOT_FOUND; goto fail; }') 'Make the additive identity export mandatory at DLL load'
    # Keep the real error and its numeric value: Windows messages can be localized.
    $annotation = Mutate-Once $requiredCopy 'ffi\go\bridge_windows.go' 'return nil, syscall\.Errno\(code\)' 'return nil, fmt.Errorf("control: required identity export error=%d: %w", uint32(code), syscall.Errno(code))' 'Diagnostic annotation only; preserve the loader error cause and numeric code'
    $requiredTest = Run-Go $requiredCopy 'required-identity-export' $OldDll '' 'identity_legacy' 'TestIdentityUnsupportedPreservesLegacyAdmission' $true 'control: required identity export error=127:'
    [void]$Cases.Add([pscustomobject]@{ Name='required-identity-export'; Category='weakened-consumer'; SourceManifestSHA256=$SourceManifestHash; Mutations=@($required,$annotation); Artifact=$ArtifactHashes.LegacyDll; Test=$requiredTest })
    Save-Cases
    if (!$requiredTest.Accepted) { throw 'Required optional-export mutation missed the intended old-DLL load error.' }
    Write-Output 'PASS negative required identity export'

    Assert-Source-Unchanged
    $Metadata.SourceUntouched=$true
    $Metadata.Completed=$true
    $Metadata.FinishedUtc=[DateTime]::UtcNow.ToString('o')
    Save-Cases
    Write-Output ("PASS controls: 8 malformed producers, 1 legacy baseline, 4 semantic negatives; evidence " + $RunRoot)
} catch {
    $Metadata.Failure=$_.Exception.Message
    $Metadata.FinishedUtc=[DateTime]::UtcNow.ToString('o')
    try {
        Assert-Source-Unchanged
        $Metadata.SourceUntouched=$true
    } catch {
        $Metadata.SourceCheckFailure=$_.Exception.Message
    }
    Save-Cases
    throw
} finally {
    foreach ($name in $envNames) {
        [Environment]::SetEnvironmentVariable($name, $SavedEnvironment[$name], 'Process')
    }
}

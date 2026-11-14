param([string]$TaskRoot='D:\dev\ChatGPT\mcphub-ffi-identity-20261115')
$ErrorActionPreference='Stop'
$sourceRoot=Join-Path $TaskRoot 'source'
$native=Join-Path $sourceRoot 'experiments\rust-process-owner'
$ffi=Join-Path $native 'ffi'
$logRoot=Join-Path $TaskRoot 'evidence\qualification'
@('tmp','target','go-cache','go-mod-cache','go-path','tmp\identity-qualified','evidence\qualification','compatibility') | ForEach-Object { [IO.Directory]::CreateDirectory((Join-Path $TaskRoot $_)) | Out-Null }
$env:TEMP=Join-Path $TaskRoot 'tmp'; $env:TMP=$env:TEMP; $env:GOTMPDIR=$env:TEMP
$env:CARGO_HOME='D:\dev\ChatGPT\mcphub-rx02-stdio-20261113\cargo-home'
$env:CARGO_TARGET_DIR=Join-Path $TaskRoot 'target'; $env:CARGO_NET_OFFLINE='true'; $env:RUSTUP_OFFLINE='1'
$env:GOCACHE=Join-Path $TaskRoot 'go-cache'; $env:GOMODCACHE=Join-Path $TaskRoot 'go-mod-cache'; $env:GOPATH=Join-Path $TaskRoot 'go-path'
$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'; $env:GOSUMDB='off'; $env:GOTELEMETRY='off'; $env:CGO_ENABLED='1'; $env:CC='c:\msys64\ucrt64\bin\gcc.exe'
$env:MCPHUB_RX02_FFI_CHILD=Join-Path $env:CARGO_TARGET_DIR 'debug\owner_child.exe'
$env:MCPHUB_RX02_IDENTITY_FIXTURES=Join-Path $TaskRoot 'tmp\identity-qualified'
$steps=New-Object Collections.Generic.List[object]
function Write-Text([string]$Path,[string]$Content){
 [IO.File]::WriteAllText($Path,($Content -replace '\r\n',[string][char]10).TrimEnd()+[char]10,(New-Object Text.UTF8Encoding($false)))
}
function Digest([string]$Path){
 $h=[Security.Cryptography.SHA256]::Create()
 try { ([BitConverter]::ToString($h.ComputeHash([IO.File]::ReadAllBytes($Path)))).Replace('-','').ToLowerInvariant() } finally {$h.Dispose()}
}
function Save-Steps { Write-Text (Join-Path $logRoot 'steps.json') (ConvertTo-Json -InputObject @($steps.ToArray()) -Depth 6) }
function Step([string]$Name,[string]$Program,[string[]]$Arguments){
 $started=[DateTime]::UtcNow.ToString('o')
 $old=$ErrorActionPreference; $ErrorActionPreference='Continue'
 & $Program @Arguments 2>&1 | Out-File -Encoding utf8 (Join-Path $logRoot ($Name+'.log'))
 $code=$LASTEXITCODE; $ErrorActionPreference=$old
 $steps.Add([ordered]@{name=$Name;program=$Program;arguments=$Arguments;cwd=(Get-Location).Path;startedUtc=$started;finishedUtc=[DateTime]::UtcNow.ToString('o');exitCode=$code;log=$Name+'.log'})
 Save-Steps
 Write-Output "$Name exit=$code"
 if($code -ne 0){Get-Content (Join-Path $logRoot ($Name+'.log')) -Tail 70; throw "$Name failed: $code"}
}
function Check-Go([string]$Name,[int]$ExpectedTop,[int]$ExpectedNested){
 $events=@(Get-Content (Join-Path $logRoot ($Name+'.log')) | Where-Object {$_ -match '^\{'} | ForEach-Object {$_ | ConvertFrom-Json})
 $top=@($events | Where-Object {$_.Action -eq 'pass' -and $_.Test -and $_.Test -notmatch '/'}).Count
 $nested=@($events | Where-Object {$_.Action -eq 'pass' -and $_.Test -match '/'}).Count
 $skip=@($events | Where-Object {$_.Action -eq 'skip'}).Count
 $fail=@($events | Where-Object {$_.Action -eq 'fail'}).Count
 $last=$steps[$steps.Count-1]
 $last['topLevelPass']=$top; $last['nestedPass']=$nested; $last['skips']=$skip; $last['failures']=$fail
 Save-Steps
 Write-Output "$Name top=$top nested=$nested skips=$skip failures=$fail"
 if($top -ne $ExpectedTop -or $nested -ne $ExpectedNested -or $skip -ne 0 -or $fail -ne 0){throw "$Name unexpected test census"}
}
Set-Location $sourceRoot
$parent='84119b51e089cce9ca42da2fce35a012eb86cb7c'
$oldForeign='eb4ca1d40244ba1915638b76c3e30b2a684d9b07'
$nativePaths=@('Cargo.toml','Cargo.lock','src','tests')
foreach($p in $nativePaths){
 $rel='experiments/rust-process-owner/'+$p
 $want=(& git rev-parse ($parent+':'+$rel)).Trim()
 $have=(& git rev-parse ('HEAD:'+$rel)).Trim()
 if($LASTEXITCODE -ne 0 -or $want -ne $have){throw "Native parent identity differs: $rel"}
}
$sourceFiles=@(
 Get-Item (Join-Path $native 'Cargo.toml'),(Join-Path $native 'Cargo.lock'),(Join-Path $ffi 'Cargo.toml'),(Join-Path $ffi 'Cargo.lock')
 Get-ChildItem (Join-Path $native 'src') -File -Recurse
 Get-ChildItem (Join-Path $ffi 'src'),(Join-Path $ffi 'include'),(Join-Path $ffi 'go') -File -Recurse
)
$sourceHashes=@($sourceFiles | Sort-Object FullName -Unique | ForEach-Object {[ordered]@{path=$_.FullName.Substring($sourceRoot.Length+1).Replace('\','/');bytes=$_.Length;sha256=(Digest $_.FullName)}})
Write-Text (Join-Path $logRoot 'source-hashes.json') (ConvertTo-Json -InputObject $sourceHashes -Depth 4)
Write-Text (Join-Path $logRoot 'source-identity.json') (ConvertTo-Json -InputObject ([ordered]@{sourceHead=(& git rev-parse HEAD).Trim();qualifiedNativeParent=$parent;foreignBaseline=$oldForeign;dirtyRuntimeOverlay=$true;runtimeFiles=$sourceHashes.Count;recordedUtc=[DateTime]::UtcNow.ToString('o')}) -Depth 4)
Set-Location $ffi
Step 'rust-fmt' 'cargo' @('fmt','--check')
Step 'rust-clippy' 'cargo' @('clippy','--offline','--locked','--all-targets','--','-D','warnings')
Step 'rust-tests-debug' 'cargo' @('test','--offline','--locked','--','--test-threads=1')
Step 'rust-tests-release' 'cargo' @('test','--offline','--locked','--release','--','--test-threads=1')
Step 'dll-debug' 'cargo' @('build','--offline','--locked')
Step 'dll-release' 'cargo' @('build','--offline','--locked','--release')
Set-Location (Join-Path $ffi 'go')
Step 'go-format' 'gofmt' @('-l','bridge_windows.go','identity_windows.go','identity_windows_test.go','identity_legacy_windows_test.go','identity_faults_windows_test.go')
if((Get-Content (Join-Path $logRoot 'go-format.log') | Out-String).Trim()){throw 'Go formatting differs'}
foreach($profile in @('debug','release')){
 $env:MCPHUB_RX02_FFI_DLL=Join-Path $TaskRoot ("target\"+$profile+'\mcphub_owner_ffi_spike.dll')
 Step ("go-race-"+$profile) 'go' @('test','-json','-race','-count=1','-timeout=120s','.')
 Check-Go ("go-race-"+$profile) 54 68
 $env:GOEXPERIMENT='cgocheck2'
 Step ("go-cgocheck2-"+$profile) 'go' @('test','-json','-count=1','-timeout=120s','.')
 Check-Go ("go-cgocheck2-"+$profile) 54 68
 $env:GOEXPERIMENT=''
}
Step 'go-vet' 'go' @('vet','.')
# Assemble only pinned source files, never the earlier 624 evidence files.
$compat=Join-Path $TaskRoot 'compatibility'
$oldSource=Join-Path $compat 'source'
if(Test-Path $oldSource){throw 'Compatibility source already exists; preserve and choose a new task root'}
Set-Location $sourceRoot
Step 'archive-native-parent' 'git' @('archive','--format=zip',('--output='+(Join-Path $compat 'native.zip')),$parent,'--','experiments/rust-process-owner/Cargo.toml','experiments/rust-process-owner/Cargo.lock','experiments/rust-process-owner/src')
Step 'archive-old-foreign' 'git' @('archive','--format=zip',('--output='+(Join-Path $compat 'foreign.zip')),$oldForeign,'--','experiments/rust-process-owner/ffi/Cargo.toml','experiments/rust-process-owner/ffi/Cargo.lock','experiments/rust-process-owner/ffi/src','experiments/rust-process-owner/ffi/include','experiments/rust-process-owner/ffi/go')
Add-Type -AssemblyName System.IO.Compression.FileSystem
[IO.Compression.ZipFile]::ExtractToDirectory((Join-Path $compat 'native.zip'),$oldSource)
[IO.Compression.ZipFile]::ExtractToDirectory((Join-Path $compat 'foreign.zip'),$oldSource)
$oldFfi=Join-Path $oldSource 'experiments\rust-process-owner\ffi'
$env:CARGO_TARGET_DIR=Join-Path $compat 'target'
Set-Location $oldFfi
Step 'legacy-dll-build' 'cargo' @('build','--offline','--locked')
$oldDLL=Join-Path $env:CARGO_TARGET_DIR 'debug\mcphub_owner_ffi_spike.dll'
# Frozen old Go caller against the new debug DLL.
$env:MCPHUB_RX02_FFI_DLL=Join-Path $TaskRoot 'target\debug\mcphub_owner_ffi_spike.dll'
Set-Location (Join-Path $oldFfi 'go')
Step 'old-go-new-dll' 'go' @('test','-json','-race','-count=1','-timeout=120s','.')
Check-Go 'old-go-new-dll' 49 63
# New Go loader against the old DLL: explicit unsupported leaves admission available.
Set-Location (Join-Path $ffi 'go')
$env:MCPHUB_RX02_FFI_DLL=$oldDLL
Step 'new-go-old-dll' 'go' @('test','-json','-race','-tags=identity_legacy','-run','^TestIdentityUnsupportedPreservesLegacyAdmission$','-count=1','-timeout=60s','.')
Check-Go 'new-go-old-dll' 1 0
$artifacts=@(
 (Join-Path $TaskRoot 'target\debug\mcphub_owner_ffi_spike.dll'),
 (Join-Path $TaskRoot 'target\release\mcphub_owner_ffi_spike.dll'),
 (Join-Path $TaskRoot 'target\debug\owner_child.exe'),$oldDLL,
 (Join-Path $compat 'native.zip'),(Join-Path $compat 'foreign.zip')
) | ForEach-Object {[ordered]@{path=$_;bytes=(Get-Item $_).Length;sha256=(Digest $_)}}
Write-Text (Join-Path $logRoot 'artifacts.json') (ConvertTo-Json -InputObject @($artifacts) -Depth 4)
foreach($record in $sourceHashes){
 $p=Join-Path $sourceRoot $record.path
 if((Digest $p) -ne $record.sha256){throw "Qualified source changed during run: $($record.path)"}
}
Write-Output 'QUALIFICATION_COMPLETE'

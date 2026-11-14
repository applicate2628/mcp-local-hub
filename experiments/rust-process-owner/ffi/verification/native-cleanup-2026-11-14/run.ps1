# Build an instrumented OWNED source copy. Ordinary source and DLLs never change.
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$LabRoot,
    [Parameter(Mandatory=$true)][string]$CCompiler,
    [Parameter(Mandatory=$true)][ValidatePattern('^[a-zA-Z0-9][a-zA-Z0-9._-]*$')][string]$RunName,
    [ValidateSet('none','erase-failure','erase-mask')][string]$Mutation='none'
)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$utf8=[Text.UTF8Encoding]::new($false)
function Approved([string]$path){$p=[IO.Path]::GetFullPath($path);if(-not $p.StartsWith('D:\dev\ChatGPT\',[StringComparison]::OrdinalIgnoreCase)){throw 'All writable paths must be under approved D:\dev\ChatGPT'};return $p}
$LabRoot=Approved $LabRoot
$ffi=Approved ((Get-Item $PSScriptRoot).Parent.Parent.FullName)
$native=Split-Path $ffi -Parent
$repo=(Get-Item $native).Parent.Parent.FullName
$report=Join-Path (Join-Path (Split-Path $PSScriptRoot -Parent) 'runs') ('native-cleanup-'+$RunName)
$copy=Join-Path $LabRoot ('qualification-native-cleanup-'+$RunName)
if((Test-Path $report)-or(Test-Path $copy)){throw 'Choose a new name; previous evidence/copies are not overwritten'}
if(-not(Test-Path $CCompiler -PathType Leaf)-or-not(Test-Path (Join-Path $LabRoot 'cargo-home'))){throw 'Existing compiler/cache required; no download/install'}
New-Item -ItemType Directory -Path $report,$copy|Out-Null
if(-not(Test-Path (Join-Path $LabRoot 'tmp'))){New-Item -ItemType Directory -Path (Join-Path $LabRoot 'tmp')|Out-Null}
$env:TEMP=Join-Path $LabRoot 'tmp';$env:TMP=$env:TEMP;$env:GOTMPDIR=$env:TEMP
$env:CARGO_HOME=Join-Path $LabRoot 'cargo-home';$env:CARGO_TARGET_DIR=Join-Path $LabRoot 'target-native-cleanup'
$env:CARGO_NET_OFFLINE='true';$env:RUSTUP_OFFLINE='1';$env:GOTOOLCHAIN='local'
$env:GOPROXY='off';$env:GOSUMDB='off';$env:GOTELEMETRY='off';$env:CGO_ENABLED='1';$env:CC=$CCompiler
$env:GOCACHE='D:\dev\ChatGPT\go-build-cache';$env:GOMAXPROCS='4';$env:GOEXPERIMENT='';$env:MCPHUB_FFI_FAULT=''
$env:GIT_OPTIONAL_LOCKS='0';$env:GIT_TERMINAL_PROMPT='0'
function Hash([string]$path){$sha=[Security.Cryptography.SHA256]::Create();try{[BitConverter]::ToString($sha.ComputeHash([IO.File]::ReadAllBytes($path))).Replace('-','').ToLowerInvariant()}finally{$sha.Dispose()}}
function Write-Text([string]$path,[string]$text){$text=$text.Replace($LabRoot,'<LAB>').Replace($repo,'<REPOSITORY>').Replace("`r`n","`n");$text=[regex]::Replace($text,'(?i)C:\\Users\\[^\\\s"''<>]+','<USERPROFILE>');$text=[regex]::Replace($text,'(?m)[ \t]+$','').TrimEnd()+"`n";[IO.File]::WriteAllText($path,$text,$utf8)}
$steps=[Collections.Generic.List[object]]::new()
function Check([string]$name,[string]$tool,[string[]]$arguments,[int]$expected=0){
    $ErrorActionPreference='Continue';$lines=& $tool @arguments 2>&1;$code=$LASTEXITCODE;$ErrorActionPreference='Stop'
    $text=(@(("Command: $tool "+($arguments-join ' ')),"Expected exit: $expected","Actual exit: $code",'')+@($lines|ForEach-Object{$_.ToString()}))-join "`n"
    Write-Text (Join-Path $report ($name+'.log')) $text
    $steps.Add([ordered]@{name=$name;command=($tool+' '+($arguments-join ' '));expected=$expected;actual=$code;matched=($code-eq $expected)})
    Write-Text (Join-Path $report 'steps.json') ($steps.ToArray()|ConvertTo-Json -Depth 4)
    Write-Output "$name exit=$code expected=$expected"
    if($code-ne $expected){$lines|Select-Object -Last 24;throw ('Unexpected result: '+$name)}
}
$inputs=@(Get-ChildItem (Join-Path $native 'src'),(Join-Path $ffi 'src'),(Join-Path $ffi 'go'),$PSScriptRoot -File -Recurse|Where-Object{$_.Extension-in @('.rs','.go','.ps1')}|ForEach-Object{[ordered]@{path=$_.FullName.Substring($repo.Length+1).Replace('\','/');sha256=(Hash $_.FullName)}})
foreach($p in @((Join-Path $native 'Cargo.toml'),(Join-Path $native 'Cargo.lock'),(Join-Path $ffi 'Cargo.toml'),(Join-Path $ffi 'Cargo.lock'))){$inputs+=@([ordered]@{path=$p.Substring($repo.Length+1).Replace('\','/');sha256=(Hash $p)})}
Write-Text (Join-Path $report 'source-hashes.json') ($inputs|ConvertTo-Json -Depth 4)
$normalDebug=Join-Path $LabRoot 'target\debug\mcphub_owner_ffi_spike.dll'
$normalRelease=Join-Path $LabRoot 'target\release\mcphub_owner_ffi_spike.dll'
$normalHashes=@($normalDebug,$normalRelease|ForEach-Object{[ordered]@{path=$_;sha256=(Hash $_)}})
$copiedNative=Join-Path $copy 'native';$copiedFfi=Join-Path $copiedNative 'ffi'
try{
    Check '00-exit-control' $env:ComSpec @('/d','/c','exit 7') 7
    Check '01-source-head' 'git' @('-C',$repo,'rev-parse','HEAD')
    Check '02-source-status' 'git' @('-C',$repo,'status','--short')
    Check '03-go-version' 'go' @('version')
    Check '04-rust-version' 'rustc' @('--version','--verbose')
    Write-Text (Join-Path $report 'environment.txt') ("OS="+[Environment]::OSVersion.VersionString+"`nArch="+$env:PROCESSOR_ARCHITECTURE+"`nOffline=true`nMutation=$Mutation`n")
    New-Item -ItemType Directory -Path $copiedNative,$copiedFfi|Out-Null
    Copy-Item (Join-Path $native 'src') $copiedNative -Recurse
    Copy-Item (Join-Path $native 'Cargo.toml'),(Join-Path $native 'Cargo.lock') $copiedNative
    Copy-Item (Join-Path $ffi 'src') $copiedFfi -Recurse
    Copy-Item (Join-Path $ffi 'Cargo.toml'),(Join-Path $ffi 'Cargo.lock') $copiedFfi
    [IO.File]::AppendAllText((Join-Path $copiedNative 'src\windows.rs'),"`n"+[IO.File]::ReadAllText((Join-Path $PSScriptRoot 'owner_slots.rs')),$utf8)
    $dllSource=Join-Path $copiedFfi 'src\lib.rs'
    [IO.File]::AppendAllText($dllSource,"`n"+[IO.File]::ReadAllText((Join-Path $PSScriptRoot 'ffi_slots.rs')),$utf8)
    $code=[IO.File]::ReadAllText($dllSource)
    if($Mutation-ne 'none'){
        $anchor=if($Mutation-eq 'erase-failure'){'out.state = CLEANUP_FAILED;'}else{'out.cleanup_mask |= 1 << index;'}
        $replacement=if($Mutation-eq 'erase-failure'){'out.state = COMPLETE; // deliberate invalid report'}else{'out.cleanup_mask |= 0 << index; // deliberate lost failures'}
        if(($code.Split([string[]]@($anchor),[StringSplitOptions]::None).Length-1)-ne 1){throw 'Mutation anchor changed; no guessed patch'}
        [IO.File]::WriteAllText($dllSource,$code.Replace($anchor,$replacement),$utf8)
    }
    $copyManifest=@(Get-ChildItem $copiedNative -File -Recurse|ForEach-Object{[ordered]@{path=$_.FullName.Substring($copy.Length+1).Replace('\','/');sha256=(Hash $_.FullName)}})
    Write-Text (Join-Path $report 'instrumented-source-hashes.json') ($copyManifest|ConvertTo-Json -Depth 4)
    Push-Location (Join-Path $ffi 'go')
    try{
        $env:MCPHUB_FFI_NATIVE_FAULT='normal'
        foreach($normal in @($normalDebug,$normalRelease)){
            $env:MCPHUB_RX02_FFI_DLL=$normal
            $name=if($normal-eq $normalDebug){'05-normal-debug-absent'}else{'05-normal-release-absent'}
            Check $name 'go' @('test','-tags','ffi_native_faults','.','-run','^TestNativeCleanupQualificationExportAbsentFromOrdinaryDLL$','-count=1','-timeout=30s')
        }
    }finally{Pop-Location}
    Push-Location $copiedFfi
    try{
        Check '06-build-child' 'cargo' @('build','--manifest-path','../Cargo.toml','--offline','--locked','--bin','owner_child')
        Check '07-debug-fixture-dll' 'cargo' @('build','--offline','--locked')
        if($Mutation-eq 'none'){
            Check '08-fixture-clippy' 'cargo' @('clippy','--offline','--locked','--all-targets','--','-D','warnings')
            Check '09-release-fixture-dll' 'cargo' @('build','--release','--offline','--locked')
        }
    }finally{Pop-Location}
    Push-Location (Join-Path $ffi 'go')
    try{
        $env:MCPHUB_FFI_NATIVE_FAULT='protected-close'
        $env:MCPHUB_RX02_FFI_CHILD=Join-Path $env:CARGO_TARGET_DIR 'debug\owner_child.exe'
        foreach($mode in @('debug','release')){
            if($Mutation-ne 'none'-and $mode-eq 'release'){continue}
            $env:MCPHUB_RX02_FFI_DLL=Join-Path $env:CARGO_TARGET_DIR ("$mode\mcphub_owner_ffi_spike.dll")
            $expected=if($Mutation-eq 'none'){0}else{1}
            Check ("10-$mode-native-close") 'go' @('test','-race','-tags','ffi_native_faults','.','-run','^TestNativeCleanup(RealFailures|FailureRemains)','-count=3','-timeout=90s','-v') $expected
        }
        if($Mutation-eq 'none'){
            $env:GOEXPERIMENT='cgocheck2'
            Check '11-native-cgocheck2' 'go' @('test','-tags','ffi_native_faults','.','-run','^TestNativeCleanup(RealFailures|FailureRemains)','-count=1','-timeout=60s')
            $env:GOEXPERIMENT=''
            Check '12-native-go-vet' 'go' @('vet','-tags','ffi_native_faults','.')
        }
    }finally{Pop-Location}
    foreach($entry in $inputs){if((Hash (Join-Path $repo $entry.path))-ne $entry.sha256){throw 'Original source changed during isolated run'}}
    foreach($entry in $normalHashes){if((Hash $entry.path)-ne $entry.sha256){throw 'Ordinary artifact changed during isolated run'}}
    Write-Text (Join-Path $report 'normal-artifact-hashes.json') ($normalHashes|ConvertTo-Json -Depth 4)
    Write-Text (Join-Path $report 'COMPLETED.txt') "All recorded expected exits matched. Mutation=$Mutation. Original source and ordinary DLLs unchanged. Qualifies selected owned Windows cleanup failures, not a new recovery API or arbitrary OS failure schedules."
}finally{
    $env:GOEXPERIMENT='';$env:MCPHUB_FFI_NATIVE_FAULT=''
    $sums=@(Get-ChildItem $report -File|Where-Object{$_.Name-ne 'SHA256SUMS.txt'}|Sort-Object Name|ForEach-Object{(Hash $_.FullName)+'  '+$_.Name})
    Write-Text (Join-Path $report 'SHA256SUMS.txt') ($sums-join "`n")
}

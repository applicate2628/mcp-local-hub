# Reproducible, cache-only Windows qualification. No installs, pushes or cleanup.
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$LabRoot,
    [Parameter(Mandatory=$true)][string]$CCompiler,
    [string]$GoCache='D:\dev\ChatGPT\go-build-cache',
    [Parameter(Mandatory=$true)][ValidatePattern('^[a-zA-Z0-9][a-zA-Z0-9._-]*$')][string]$RunName
)
$ErrorActionPreference='Stop'
Set-StrictMode -Version Latest
$utf8=[Text.UTF8Encoding]::new($false)
function Approved([string]$Path) {
    $full=[IO.Path]::GetFullPath($Path)
    if (-not $full.StartsWith('D:\dev\ChatGPT\',[StringComparison]::OrdinalIgnoreCase)) {
        throw 'All writable paths must be inside the user-approved D:\dev\ChatGPT area'
    }
    return $full
}
$LabRoot=Approved $LabRoot
$GoCache=Approved $GoCache
$ffi=Approved (Split-Path $PSScriptRoot -Parent)
$native=Split-Path $ffi -Parent
$repo=(Get-Item $native).Parent.Parent.FullName
$report=Join-Path (Join-Path $PSScriptRoot 'runs') $RunName
if (Test-Path -LiteralPath $report) { throw 'Choose a new run name; existing evidence must not be overwritten' }
if (-not (Test-Path -LiteralPath $CCompiler -PathType Leaf)) { throw 'Existing C compiler required; no installation is attempted' }
foreach($tool in @('git','go','cargo','rustc','gofmt')) { if (-not(Get-Command $tool -ErrorAction SilentlyContinue)) { throw ('Missing existing tool: '+$tool) } }
if (-not(Test-Path (Join-Path $LabRoot 'cargo-home'))) { throw 'Existing Cargo cache required; no dependency fetch is attempted' }
New-Item -ItemType Directory -Path $report,(Join-Path $LabRoot 'tmp'),$GoCache -Force|Out-Null
$env:TEMP=Join-Path $LabRoot 'tmp';$env:TMP=$env:TEMP;$env:GOTMPDIR=$env:TEMP
$env:CARGO_HOME=Join-Path $LabRoot 'cargo-home';$env:CARGO_TARGET_DIR=Join-Path $LabRoot 'target'
$env:CARGO_NET_OFFLINE='true';$env:RUSTUP_OFFLINE='1';$env:GOTOOLCHAIN='local'
$env:GOPROXY='off';$env:GOSUMDB='off';$env:GOTELEMETRY='off';$env:GOCACHE=$GoCache
$env:CGO_ENABLED='1';$env:CC=$CCompiler;$env:GOMAXPROCS='4';$env:GOEXPERIMENT=''
$env:GIT_OPTIONAL_LOCKS='0';$env:GIT_TERMINAL_PROMPT='0';$env:MCPHUB_FFI_FAULT=''
$steps=[Collections.Generic.List[object]]::new()
function Clean([string]$Text) {
    $Text=$Text.Replace($LabRoot,'<LAB>').Replace($repo,'<REPOSITORY>').Replace("`r`n","`n")
    $Text=[regex]::Replace($Text,'(?i)C:\\Users\\[^\\\s"''<>]+','<USERPROFILE>')
    return [regex]::Replace($Text,'(?m)[ \t]+$','')
}
function Write-Text([string]$Path,[string]$Text) { [IO.File]::WriteAllText($Path,(Clean $Text).TrimEnd([char[]]@("`r","`n"))+"`n",$utf8) }
function Check([string]$Name,[string]$Tool,[string[]]$Arguments,[int]$Expected=0) {
    $ErrorActionPreference='Continue'
    $begin=[DateTime]::UtcNow.ToString('o')
    $lines=& $Tool @Arguments 2>&1
    $actual=$LASTEXITCODE
    $ErrorActionPreference='Stop'
    $text=(@('Command: '+$Tool+' '+($Arguments -join ' '),'UTC: '+$begin,'Expected exit: '+$Expected,'Actual exit: '+$actual,'')+@($lines|ForEach-Object{$_.ToString()})) -join "`n"
    Write-Text (Join-Path $report ($Name+'.log')) ($text+"`n")
    $steps.Add([pscustomobject]@{name=$Name;command=($Tool+' '+($Arguments -join ' '));expected=$Expected;actual=$actual;utc=$begin;matched=($actual-eq $Expected)})
    Write-Text (Join-Path $report 'steps.json') (($steps.ToArray()|ConvertTo-Json -Depth 4)+"`n")
    Write-Output ("{0}: exit={1}, expected={2}" -f $Name,$actual,$Expected)
    if ($actual-ne $Expected) { throw ('Verification failed: '+$Name+'; raw evidence was preserved') }
}
function Digest([byte[]]$Bytes) {
    $sha=[Security.Cryptography.SHA256]::Create()
    try { [BitConverter]::ToString($sha.ComputeHash($Bytes)).Replace('-','').ToLowerInvariant() }
    finally { $sha.Dispose() }
}
function Source-Manifest {
    $files=@(Get-ChildItem (Join-Path $ffi 'src'),(Join-Path $ffi 'go'),(Join-Path $ffi 'include') -File -Recurse)
    $files+=@(Get-Item (Join-Path $ffi 'Cargo.toml'),(Join-Path $ffi 'Cargo.lock'),$PSCommandPath)
    $records=@($files|Sort-Object FullName -Unique|ForEach-Object {
        [pscustomobject]@{path=$_.FullName.Substring($repo.Length+1).Replace('\','/');sha256=(Digest ([IO.File]::ReadAllBytes($_.FullName)))}
    })
    Write-Text (Join-Path $report 'source-hashes.json') (($records|ConvertTo-Json -Depth 4)+"`n")
}
Push-Location $ffi
try {
    Check '00-exit-propagation-control' $env:ComSpec @('/d','/c','exit 7') 7
    Check '01-source-head' 'git' @('-C',$repo,'rev-parse','HEAD')
    Check '02-source-status' 'git' @('-C',$repo,'status','--short')
    Check '03-go-version' 'go' @('version')
    Check '04-rust-version' 'rustc' @('--version','--verbose')
    Write-Text (Join-Path $report 'environment.txt') ("OS="+[Environment]::OSVersion.VersionString+"`nArchitecture="+$env:PROCESSOR_ARCHITECTURE+"`nGOMAXPROCS=4`nOffline=true`nSource overlay, if any, identified by hashes and status log`n")
    Source-Manifest
    Check '05-build-child' 'cargo' @('build','--manifest-path','../Cargo.toml','--offline','--locked','--bin','owner_child')
    Check '06-abi-debug' 'cargo' @('test','--offline','--locked')
    Check '07-abi-release' 'cargo' @('test','--release','--offline','--locked')
    Check '08-abi-clippy' 'cargo' @('clippy','--offline','--locked','--all-targets','--','-D','warnings')
    Check '09-abi-format' 'cargo' @('fmt','--all','--','--check')
    Check '10-debug-dll' 'cargo' @('build','--offline','--locked')
    Check '11-release-dll' 'cargo' @('build','--release','--offline','--locked')
    $env:MCPHUB_RX02_FFI_CHILD=Join-Path $env:CARGO_TARGET_DIR 'debug\owner_child.exe'
    Push-Location (Join-Path $ffi 'go')
    try {
        $format=@(& gofmt -l .)
        if ($LASTEXITCODE-ne 0 -or $format.Count-ne 0) { throw 'Go files are not formatted' }
        $env:MCPHUB_RX02_FFI_DLL=Join-Path $env:CARGO_TARGET_DIR 'debug\mcphub_owner_ffi_spike.dll'
        Check '12-go-debug-race' 'go' @('test','-race','.','-count=3','-timeout=90s')
        Check '13-go-vet' 'go' @('vet','.')
        $env:GOEXPERIMENT='cgocheck2'
        Check '14-cgocheck2' 'go' @('test','.','-count=1','-timeout=90s')
        $env:GOEXPERIMENT=''
        $env:MCPHUB_RX02_FFI_DLL=Join-Path $env:CARGO_TARGET_DIR 'release\mcphub_owner_ffi_spike.dll'
        Check '15-go-release-race' 'go' @('test','-race','.','-count=3','-timeout=90s')
        Check '16-go-counts' 'go' @('test','.','-count=1','-json','-timeout=90s')
    } finally { Pop-Location }
    $events=@(Get-Content (Join-Path $report '16-go-counts.log')|Where-Object{$_.StartsWith('{')}|ForEach-Object{ConvertFrom-Json $_})
    $counts=[ordered]@{top_level_pass=@($events|Where-Object{$_.Action-eq 'pass' -and $_.PSObject.Properties['Test'] -and $_.Test -notlike '*/*'}).Count;nested_pass=@($events|Where-Object{$_.Action-eq 'pass' -and $_.PSObject.Properties['Test'] -and $_.Test -like '*/*'}).Count;skipped=@($events|Where-Object{$_.Action-eq 'skip'}).Count}
    if ($counts.top_level_pass-eq 0 -or $counts.skipped-ne 0) { throw 'No normal tests or unexpected skips; do not claim acceptance' }
    Write-Text (Join-Path $report 'go-counts.json') (($counts|ConvertTo-Json)+"`n")
    Push-Location $native
    try {
        $env:MCPHUB_RX02_PARENT_SENTINEL='owned-test-parent-only-20261113'
        foreach($mode in @('debug','release')) {
            $env:MCPHUB_RX02_FIXTURES=Join-Path $LabRoot ('fixtures-'+$RunName+'-'+$mode+'-'+[Guid]::NewGuid().ToString('N').Substring(0,8))
            New-Item -ItemType Directory -Path $env:MCPHUB_RX02_FIXTURES|Out-Null
            $a=@('test','--offline','--locked');if($mode-eq 'release'){$a+= '--release'};$a+=@('--','--test-threads=1')
            Check ('17-parent-'+$mode) 'cargo' $a
        }
        Check '18-parent-clippy' 'cargo' @('clippy','--offline','--locked','--all-targets','--','-D','warnings')
        Check '19-parent-format' 'cargo' @('fmt','--all','--','--check')
    } finally { Pop-Location }
    Check '20-diff-check' 'git' @('-C',$repo,'diff','--check')
    Write-Text (Join-Path $report 'COMPLETED.txt') "All commands matched expected exits. This qualifies only the experiment on this Windows host, not the product, native Unix, independent review or installed acceptance. Step00 is an intentional exit7 control, not a product failure.`n"
} finally {
    $env:GOEXPERIMENT=''
    $hashes=@(Get-ChildItem $report -File|Where-Object{$_.Name-ne 'SHA256SUMS.txt'}|ForEach-Object{(Digest ([IO.File]::ReadAllBytes($_.FullName)))+'  '+$_.Name})
    Write-Text (Join-Path $report 'SHA256SUMS.txt') (($hashes -join "`n")+"`n")
    Pop-Location
}

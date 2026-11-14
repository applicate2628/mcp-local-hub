# Focused verification for this increment; no install, build, push or cleanup.
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$LabRoot,
    [Parameter(Mandatory=$true)][string]$CCompiler,
    [Parameter(Mandatory=$true)][ValidatePattern('^[a-zA-Z0-9._-]+$')][string]$Name,
    [string]$Pattern='.',
    [ValidateSet(0,1)][int]$ExpectedExit=0,
    [switch]$Race,
    [ValidateSet('debug','release')][string]$Mode='debug',
    [string]$Tags='',
    [string]$Fault=''
)
$ErrorActionPreference='Stop'
$utf8=[Text.UTF8Encoding]::new($false)
$LabRoot=[IO.Path]::GetFullPath($LabRoot)
if(-not $LabRoot.StartsWith('D:\dev\ChatGPT\',[StringComparison]::OrdinalIgnoreCase)){throw 'Use an approved private lab'}
$ffi=(Get-Item $PSScriptRoot).Parent.Parent.FullName
$repo=(Get-Item $ffi).Parent.Parent.Parent.FullName
$log=Join-Path $PSScriptRoot ($Name+'.log')
if(Test-Path $log){throw 'Existing evidence may not be overwritten'}
if(-not(Test-Path $CCompiler -PathType Leaf)){throw 'Existing C compiler required'}
$env:TEMP=Join-Path $LabRoot 'tmp';$env:TMP=$env:TEMP;$env:GOTMPDIR=$env:TEMP
$env:GOCACHE='D:\dev\ChatGPT\go-build-cache';$env:GOTOOLCHAIN='local'
$env:GOPROXY='off';$env:GOSUMDB='off';$env:GOTELEMETRY='off';$env:GOMAXPROCS='4'
$env:CGO_ENABLED='1';$env:CC=$CCompiler;$env:GOEXPERIMENT='';$env:MCPHUB_FFI_FAULT=$Fault
$env:MCPHUB_RX02_FFI_DLL=Join-Path $LabRoot ("target\$Mode\mcphub_owner_ffi_spike.dll")
$env:MCPHUB_RX02_FFI_CHILD=Join-Path $LabRoot 'target\debug\owner_child.exe'
foreach($p in @($env:MCPHUB_RX02_FFI_DLL,$env:MCPHUB_RX02_FFI_CHILD)){if(-not(Test-Path $p)){throw 'Build the explicit owned artifacts first; no download attempted'}}
function Digest([string]$Path){$sha=[Security.Cryptography.SHA256]::Create();try{[BitConverter]::ToString($sha.ComputeHash([IO.File]::ReadAllBytes($Path))).Replace('-','').ToLowerInvariant()}finally{$sha.Dispose()}}
function Clean([string]$Text){$Text=$Text.Replace($LabRoot,'<LAB>').Replace($repo,'<REPOSITORY>').Replace("`r`n","`n");$Text=[regex]::Replace($Text,'(?i)C:\\Users\\[^\\\s"''<>]+','<USERPROFILE>');return [regex]::Replace($Text,'(?m)[ \t]+$','').TrimEnd()+"`n"}
$inputs=@(Get-ChildItem (Join-Path $ffi 'go'),(Join-Path $ffi 'src'),(Join-Path $ffi 'include') -File -Recurse|ForEach-Object{[ordered]@{path=$_.FullName.Substring($repo.Length+1).Replace('\','/');sha256=(Digest $_.FullName)}})
$inputs+=@([ordered]@{path='probe.ps1';sha256=(Digest $PSCommandPath)},[ordered]@{path="<TARGET>/$Mode/mcphub_owner_ffi_spike.dll";sha256=(Digest $env:MCPHUB_RX02_FFI_DLL)},[ordered]@{path='<TARGET>/debug/owner_child.exe';sha256=(Digest $env:MCPHUB_RX02_FFI_CHILD)})
[IO.File]::WriteAllText((Join-Path $PSScriptRoot ($Name+'.inputs.json')),(Clean ($inputs|ConvertTo-Json -Depth 4)),$utf8)
$testArgs=@('test','.','-run',$Pattern,'-count=1','-timeout=45s','-v')
if($Race){$testArgs+='-race'}
if($Tags){$testArgs+=@('-tags',$Tags)}
Push-Location (Join-Path $ffi 'go')
try{
    $head=(& git -C $repo rev-parse HEAD).Trim();if($LASTEXITCODE-ne 0){throw 'source commit unavailable'}
    $ErrorActionPreference='Continue'
    $lines=& go @testArgs 2>&1;$code=$LASTEXITCODE
    $ErrorActionPreference='Stop'
    $text=(@('Command: go '+($testArgs -join ' '),'Source base: '+$head,'UTC: '+[DateTime]::UtcNow.ToString('o'),"Expected exit: $ExpectedExit","Actual exit: $code","Mode=$Mode; GOMAXPROCS=4; cache-only; fault=$Fault",'')+@($lines|ForEach-Object{$_.ToString()}))-join "`n"
    [IO.File]::WriteAllText($log,(Clean $text),$utf8)
    $lines|Select-Object -Last 30
    Write-Output "Recorded exit=$code expected=$ExpectedExit"
    if($code-ne $ExpectedExit){throw 'Observed result differs from expected; evidence retained'}
}finally{Pop-Location;$env:MCPHUB_FFI_FAULT=''}

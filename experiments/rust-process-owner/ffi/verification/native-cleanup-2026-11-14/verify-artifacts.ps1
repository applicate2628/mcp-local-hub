# Verify existing post-build ordinary DLLs; do not rebuild or install anything.
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$LabRoot,
    [Parameter(Mandatory=$true)][string]$CCompiler,
    [Parameter(Mandatory=$true)][ValidatePattern('^[a-zA-Z0-9][a-zA-Z0-9._-]*$')][string]$RunName
)
$ErrorActionPreference='Stop'
$utf8=[Text.UTF8Encoding]::new($false)
$LabRoot=[IO.Path]::GetFullPath($LabRoot)
$ffi=(Get-Item $PSScriptRoot).Parent.Parent.FullName
$repo=(Get-Item $ffi).Parent.Parent.Parent.FullName
if(-not $LabRoot.StartsWith('D:\dev\ChatGPT\',[StringComparison]::OrdinalIgnoreCase)){throw 'Approved lab required'}
$report=Join-Path (Join-Path (Split-Path $PSScriptRoot -Parent) 'runs') ('native-cleanup-'+$RunName)
if(Test-Path $report){throw 'Do not overwrite earlier evidence'}
if(-not(Test-Path $CCompiler -PathType Leaf)){throw 'Existing compiler required'}
New-Item -ItemType Directory -Path $report|Out-Null
$env:TEMP=Join-Path $LabRoot 'tmp';$env:TMP=$env:TEMP;$env:GOTMPDIR=$env:TEMP
$env:GOCACHE='D:\dev\ChatGPT\go-build-cache';$env:GOTOOLCHAIN='local';$env:GOPROXY='off';$env:GOSUMDB='off'
$env:GOTELEMETRY='off';$env:GOMAXPROCS='4';$env:CGO_ENABLED='1';$env:CC=$CCompiler;$env:GOEXPERIMENT=''
$env:MCPHUB_FFI_FAULT='';$env:MCPHUB_FFI_NATIVE_FAULT='normal';$env:GIT_OPTIONAL_LOCKS='0'
$env:MCPHUB_RX02_FFI_CHILD=Join-Path $LabRoot 'target\debug\owner_child.exe'
function Hash([string]$p){$h=[Security.Cryptography.SHA256]::Create();try{[BitConverter]::ToString($h.ComputeHash([IO.File]::ReadAllBytes($p))).Replace('-','').ToLowerInvariant()}finally{$h.Dispose()}}
function Text([string]$p,[string]$t){$t=$t.Replace($LabRoot,'<LAB>').Replace($repo,'<REPOSITORY>').Replace("`r`n","`n");$t=[regex]::Replace($t,'(?i)C:\\Users\\[^\\\s"''<>]+','<USERPROFILE>');[IO.File]::WriteAllText($p,([regex]::Replace($t,'(?m)[ \t]+$','').TrimEnd()+"`n"),$utf8)}
$files=@(Get-ChildItem (Join-Path $ffi 'go'),(Join-Path $ffi 'src'),(Join-Path $ffi 'include') -File -Recurse)+@(Get-Item $PSCommandPath)
$source=@($files|ForEach-Object{[ordered]@{path=$_.FullName.Substring($repo.Length+1).Replace('\','/');sha256=(Hash $_.FullName)}})
Text (Join-Path $report 'source-hashes.json') ($source|ConvertTo-Json -Depth 4)
$steps=[Collections.Generic.List[object]]::new()
$artifacts=[Collections.Generic.List[object]]::new()
Push-Location (Join-Path $ffi 'go')
try{
    foreach($mode in @('debug','release')){
        $dll=Join-Path $LabRoot ("target\$mode\mcphub_owner_ffi_spike.dll")
        $env:MCPHUB_RX02_FFI_DLL=$dll
        $before=Hash $dll
        $artifact=[ordered]@{path=("<LAB>/target/$mode/mcphub_owner_ffi_spike.dll");sha256=$before;last_write_utc=(Get-Item $dll).LastWriteTimeUtc.ToString('o')}
        $artifacts.Add($artifact)
        foreach($kind in @('absence','normal-suite')){
            $args=@('test','.','-count=1','-timeout=90s','-race')
            if($kind-eq 'absence'){$args+=@('-tags','ffi_native_faults','-run','^TestNativeCleanupQualificationExportAbsentFromOrdinaryDLL$')}
            $begin=[DateTime]::UtcNow.ToString('o');$ErrorActionPreference='Continue'
            $output=& go @args 2>&1;$exit=$LASTEXITCODE;$ErrorActionPreference='Stop'
            Text (Join-Path $report ("$mode-$kind.log")) ((@(('Command: go '+($args-join ' ')),"UTC: $begin","Actual exit: $exit","Artifact SHA256: $before",'')+@($output|ForEach-Object{$_.ToString()}))-join "`n")
            $steps.Add([ordered]@{name="$mode-$kind";actual=$exit;expected=0;matched=($exit-eq 0);artifact_sha256=$before})
            Text (Join-Path $report 'steps.json') ($steps.ToArray()|ConvertTo-Json -Depth 4)
            Write-Output "$mode-$kind exit=$exit"
            if($exit-ne 0){throw 'Ordinary artifact verification failed'}
        }
        if((Hash $dll)-ne $before){throw 'DLL changed during its verification'}
    }
    foreach($entry in $source){if((Hash (Join-Path $repo $entry.path))-ne $entry.sha256){throw 'Source changed during verification'}}
    Text (Join-Path $report 'artifact-hashes.json') ($artifacts.ToArray()|ConvertTo-Json -Depth 4)
    Text (Join-Path $report 'COMPLETED.txt') 'Both existing ordinary DLLs passed absence controls and full normal Go tests under race; artifact hashes stable through each run. No native or DLL build occurred in this verification.'
}finally{
    $env:MCPHUB_FFI_NATIVE_FAULT=''
    $sums=@(Get-ChildItem $report -File|Where-Object{$_.Name-ne 'SHA256SUMS.txt'}|Sort-Object Name|ForEach-Object{(Hash $_.FullName)+'  '+$_.Name})
    Text (Join-Path $report 'SHA256SUMS.txt') ($sums-join "`n")
    Pop-Location
}

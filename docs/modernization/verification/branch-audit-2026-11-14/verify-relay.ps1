# Qualification of the private combined relay tree; no install, fetch or push.
[CmdletBinding()]
param([Parameter(Mandatory=$true)][string]$Repo,[Parameter(Mandatory=$true)][string]$RunName)
$ErrorActionPreference='Stop'
if(-not [IO.Path]::GetFullPath($Repo).StartsWith('D:\dev\ChatGPT\',[StringComparison]::OrdinalIgnoreCase)){throw 'Private approved worktree required'}
if($RunName-notmatch '^[a-z0-9-]+$'){throw 'safe run name required'}
$report=Join-Path $PSScriptRoot $RunName;if(Test-Path $report){throw 'Do not overwrite evidence'}
$work=Join-Path $Repo 'private-build-artifacts'
New-Item -ItemType Directory -Path $report,$work -Force|Out-Null
$env:TEMP=Join-Path $work 'tmp';New-Item -ItemType Directory $env:TEMP -Force|Out-Null
$env:TMP=$env:TEMP;$env:GOTMPDIR=$env:TEMP;$env:GOCACHE='D:\dev\ChatGPT\go-build-cache'
$env:GOPROXY='off';$env:GOSUMDB='off';$env:GOTOOLCHAIN='local';$env:GOTELEMETRY='off'
$env:GOMAXPROCS='4';$env:CGO_ENABLED='1';$env:CC='C:\msys64\ucrt64\bin\gcc.exe'
$env:MCPHUB_RX03_NATIVE_PIPE='1';$env:MCPHUB_RX03_MAIN_NATIVE='1';$env:MCPHUB_GUI_SPAWN_TESTS=''
$utf=[Text.UTF8Encoding]::new($false);$steps=[Collections.Generic.List[object]]::new()
function Save([string]$p,[string]$v){$v=$v.Replace($Repo,'<RELAY_WORKTREE>').Replace("`r`n","`n");$v=[regex]::Replace($v,'(?i)C:\\Users\\[^\\\s"''<>]+','<USERPROFILE>');[IO.File]::WriteAllText($p,([regex]::Replace($v,'(?m)[ \t]+$','').TrimEnd()+"`n"),$utf)}
function Check([string]$name,[string]$tool,[string[]]$arguments){
 $ErrorActionPreference='Continue';$lines=& $tool @arguments 2>&1;$exit=$LASTEXITCODE;$ErrorActionPreference='Stop'
 Save (Join-Path $report ($name+'.log')) ((@('Command: '+$tool+' '+($arguments-join ' '),'Exit: '+$exit)+@($lines|ForEach-Object{$_.ToString()}))-join "`n")
 $steps.Add([ordered]@{name=$name;command=($tool+' '+($arguments-join ' '));exit=$exit})
 Save (Join-Path $report 'steps.json') ($steps.ToArray()|ConvertTo-Json -Depth 4)
 Write-Output "$name exit=$exit"
 if($exit-ne 0){$lines|Select-Object -Last 18;throw ('Failed check '+$name)}
}
Push-Location $Repo
try{
 Check 'go-version' 'go' @('version')
 Check 'indexed-tree' 'git' @('write-tree')
 Check 'daemon-race' 'go' @('test','-mod=readonly','-race','./internal/daemon','-run','^Test(Relay|ReadJSONRPC|Qualif)','-count=3','-timeout=120s')
 Check 'cli-race' 'go' @('test','-mod=readonly','-race','./internal/cli','-run','^Test(BindProcessRelayInput|ProcessRelayInputWindowsQualification|ResolveRelayURL_(DirectURL|MutuallyExclusive|ResolvesFromEmbeddedManifest|MissingFlags))','-count=3','-timeout=90s')
 Check 'main-race' 'go' @('test','-mod=readonly','-race','./cmd/mcphub','-count=3','-timeout=90s')
 Check 'selected-json' 'go' @('test','-mod=readonly','./internal/daemon','./internal/cli','./cmd/mcphub','-run','^Test(Relay|ReadJSONRPC|Qualif|BindProcessRelayInput|ProcessRelayInput|ResolveRelayURL|ClassifyWindowsEntrypoint)','-count=1','-json','-timeout=90s')
 Check 'product-build' 'go' @('build','-mod=readonly','-o',(Join-Path $work 'mcphub-compile-only.exe'),'./cmd/mcphub')
 Check 'affected-vet' 'go' @('vet','-mod=readonly','./internal/daemon','./internal/cli','./cmd/mcphub')
 $events=@(Get-Content (Join-Path $report 'selected-json.log') -Encoding UTF8|ForEach-Object{try{$_|ConvertFrom-Json}catch{}})
 Save (Join-Path $report 'counts.json') ([ordered]@{top_level_pass=@($events|Where-Object{$_.Action-eq 'pass'-and $_.Test-and $_.Test-notlike '*/*'}).Count;nested_pass=@($events|Where-Object{$_.Action-eq 'pass'-and $_.Test-like '*/*'}).Count;skipped=@($events|Where-Object{$_.Action-eq 'skip'}).Count}|ConvertTo-Json)
 Save (Join-Path $report 'COMPLETED.txt') 'Listed scoped relay/CLI/bootstrap checks passed. Not whole-repository, native Unix, GUI, installed, independent-review or release acceptance.'
}finally{Pop-Location;$env:MCPHUB_RX03_NATIVE_PIPE='';$env:MCPHUB_RX03_MAIN_NATIVE=''}

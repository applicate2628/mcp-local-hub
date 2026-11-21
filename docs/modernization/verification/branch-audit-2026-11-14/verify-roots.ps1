# Verify synchronized owned roots in one disposable detached worktree.
# No remote writes, installation, downloads or changes to another worktree.
[CmdletBinding()]
param([Parameter(Mandatory=$true)][string]$Repository,[Parameter(Mandatory=$true)][string]$Worktree,[Parameter(Mandatory=$true)][string]$RustLab)
$ErrorActionPreference='Stop'
foreach($p in @($Repository,$Worktree,$RustLab)){if(-not [IO.Path]::GetFullPath($p).StartsWith('D:\dev\ChatGPT\',[StringComparison]::OrdinalIgnoreCase)){throw 'Approved private paths required'}}
if(Test-Path $Worktree){throw 'New private worktree required'}
$report=Join-Path $PSScriptRoot 'roots-verification';if(Test-Path $report){throw 'Evidence exists'}
New-Item -ItemType Directory $report|Out-Null
$env:GIT_OPTIONAL_LOCKS='0';$env:GIT_TERMINAL_PROMPT='0';$env:GOPROXY='off';$env:GOSUMDB='off';$env:GOTOOLCHAIN='local';$env:GOTELEMETRY='off';$env:GOMAXPROCS='4'
$env:TEMP=Join-Path $RustLab 'tmp';$env:TMP=$env:TEMP;$env:GOTMPDIR=$env:TEMP;$env:GOCACHE='D:\dev\ChatGPT\go-build-cache'
$env:CGO_ENABLED='1';$env:CC='C:\msys64\ucrt64\bin\gcc.exe';$env:CARGO_HOME=Join-Path $RustLab 'cargo-home';$env:CARGO_TARGET_DIR=Join-Path $RustLab 'target-anchored-read';$env:CARGO_NET_OFFLINE='true';$env:RUSTUP_OFFLINE='1'
$utf=[Text.UTF8Encoding]::new($false);$steps=[Collections.Generic.List[object]]::new()
function Text([string]$p,[string]$s){$s=$s.Replace($Worktree,'<WORKTREE>').Replace($Repository,'<SOURCE>').Replace($RustLab,'<LAB>').Replace("`r`n","`n");$s=[regex]::Replace($s,'(?i)C:\\Users\\[^\\\s"''<>]+','<USERPROFILE>');[IO.File]::WriteAllText($p,([regex]::Replace($s,'(?m)[ \t]+$','').TrimEnd()+"`n"),$utf)}
function Check([string]$name,[string]$tool,[string[]]$arguments){
 $ErrorActionPreference='Continue';$lines=& $tool @arguments 2>&1;$exit=$LASTEXITCODE;$ErrorActionPreference='Stop'
 $body=(@("Step: $name",('Command: '+$tool+' '+($arguments-join ' ')),"Exit: $exit")+@($lines|ForEach-Object{$_.ToString()}))-join "`n"
 [IO.File]::AppendAllText((Join-Path $report 'run.log'),($body.Replace("`r`n","`n")+"`n"),$utf)
 $steps.Add([ordered]@{step=$name;exit=$exit;command=($tool+' '+($arguments-join ' '))});Text (Join-Path $report 'steps.json') ($steps.ToArray()|ConvertTo-Json -Depth 4)
 Write-Output "$name exit=$exit";if($exit-ne 0){$lines|Select-Object -Last 20;throw ('check failed '+$name)}
}
$plans=Get-Content (Join-Path $PSScriptRoot 'prepared-sync.json') -Raw -Encoding UTF8|ConvertFrom-Json
Check 'worktree' 'git' @('-C',$Repository,'worktree','add','--detach',$Worktree,($plans|Where-Object{$_.pr-eq 624}).new)
Push-Location $Worktree
try{
 foreach($id in @(624,626,628,627)){
  $row=$plans|Where-Object{$_.pr-eq $id};Check "checkout-$id" 'git' @('switch','--detach',$row.new)
  switch($id){
   624 {Check 'cmake-tests-race' 'go' @('test','-mod=readonly','-race','./internal/vcpkgmcp/cmaketrace','./internal/vcpkgmcp/publicresult','-count=1','-timeout=120s');Check 'cmake-vet' 'go' @('vet','-mod=readonly','./internal/vcpkgmcp/cmaketrace','./internal/vcpkgmcp/publicresult')}
   626 {Check 'relay-baseline-contracts' 'go' @('test','-mod=readonly','-race','./internal/daemon','-run','^Test(RelayLoad|Relay|ReadJSONRPC)','-count=3','-timeout=120s')}
   628 {Check 'native-observation' 'go' @('test','-mod=readonly','-race','./internal/process','-run','^TestNativeObservation','-count=3','-timeout=60s');Check 'process-vet' 'go' @('vet','-mod=readonly','./internal/process')}
   627 {
    $env:MCPHUB_RX04_FIXTURES=Join-Path $RustLab ('read-master-sync-fixtures-'+[guid]::NewGuid().ToString('N'));New-Item -ItemType Directory $env:MCPHUB_RX04_FIXTURES|Out-Null
    Push-Location 'experiments/rust-anchored-read';try{Check 'reader-tests' 'cargo' @('test','--offline','--locked','--','--test-threads=1');Check 'reader-clippy' 'cargo' @('clippy','--offline','--locked','--all-targets','--','-D','warnings');Check 'reader-format' 'cargo' @('fmt','--all','--','--check')}finally{Pop-Location}
   }
  }
  if(@(git status --porcelain).Count-ne 0){throw 'verification worktree changed'}
 }
 Text (Join-Path $report 'COMPLETED.txt') 'Listed synchronized-root checks passed. Native-owner/FFI full suite is recorded separately. No whole-Hub or new performance/Unix claim.'
}finally{Pop-Location;$log=Join-Path $report 'run.log';if(Test-Path $log){Text $log ([IO.File]::ReadAllText($log))}}

# Deliberately inconsistent RETURN VALUES from an owned Rust DLL copy.
# Ordinary source/DLLs never modified. No install, merge, push or automatic cleanup.
[CmdletBinding()]
param(
 [Parameter(Mandatory=$true)][string]$LabRoot,
 [Parameter(Mandatory=$true)][string]$CCompiler,
 [Parameter(Mandatory=$true)][ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._-]*$')][string]$RunName,
 [ValidateSet('red','green')][string]$Phase='green',
 [string[]]$Cases=@()
)
$ErrorActionPreference='Stop';$utf8=[Text.UTF8Encoding]::new($false)
$LabRoot=[IO.Path]::GetFullPath($LabRoot)
if(-not $LabRoot.StartsWith('D:\dev\ChatGPT\',[StringComparison]::OrdinalIgnoreCase)){throw 'Approved owned lab required'}
$ffi=(Get-Item $PSScriptRoot).Parent.Parent.FullName;$native=Split-Path $ffi -Parent
$repo=(Get-Item $native).Parent.Parent.FullName
$report=Join-Path (Split-Path $PSScriptRoot -Parent) ('runs\outcome-'+$RunName)
$work=Join-Path $LabRoot ('qualification-outcome-'+$RunName)
if((Test-Path $report)-or(Test-Path $work)){throw 'Use a new run name; previous evidence must survive'}
if(-not(Test-Path $CCompiler -PathType Leaf)){throw 'Existing C compiler required'}
$child=Join-Path $LabRoot 'target\debug\owner_child.exe'
if(-not(Test-Path $child)){throw 'Build ordinary owner_child first with existing offline runner'}
New-Item -ItemType Directory -Path $report,$work|Out-Null
$env:TEMP=Join-Path $LabRoot 'tmp';$env:TMP=$env:TEMP;$env:GOTMPDIR=$env:TEMP
$env:GOCACHE='D:\dev\ChatGPT\go-build-cache';$env:GOTOOLCHAIN='local';$env:GOPROXY='off';$env:GOSUMDB='off';$env:GOTELEMETRY='off'
$env:GOMAXPROCS='4';$env:CGO_ENABLED='1';$env:CC=$CCompiler;$env:GOEXPERIMENT='';$env:GIT_OPTIONAL_LOCKS='0'
$env:CARGO_HOME=Join-Path $LabRoot 'cargo-home';$env:CARGO_TARGET_DIR=Join-Path $work 'target';$env:CARGO_NET_OFFLINE='true';$env:RUSTUP_OFFLINE='1'
function Hash([string]$p){$h=[Security.Cryptography.SHA256]::Create();try{[BitConverter]::ToString($h.ComputeHash([IO.File]::ReadAllBytes($p))).Replace('-','').ToLowerInvariant()}finally{$h.Dispose()}}
function Clean([string]$s){$s=$s.Replace($LabRoot,'<LAB>').Replace($repo,'<REPOSITORY>').Replace("`r`n","`n");$s=[regex]::Replace($s,'(?i)C:\\Users\\[^\\\s"''<>]+','<USERPROFILE>');return [regex]::Replace($s,'(?m)[ \t]+$','').TrimEnd()+"`n"}
function Text([string]$p,[string]$s){[IO.File]::WriteAllText($p,(Clean $s),$utf8)}
$steps=[Collections.Generic.List[object]]::new()
function Check([string]$name,[string]$tool,[string[]]$a,[int]$expected=0,[string]$marker=''){
 $ErrorActionPreference='Continue';$lines=& $tool @a 2>&1;$exit=$LASTEXITCODE;$ErrorActionPreference='Stop'
 $body=@($lines|ForEach-Object{$_.ToString()})-join "`n";$matched=($exit-eq $expected)-and(($marker-eq '')-or $body.Contains($marker))
 $header=@("Step: $name",('Command: '+$tool+' '+($a-join ' ')),('UTC: '+[DateTime]::UtcNow.ToString('o')),"Expected exit: $expected","Actual exit: $exit","Matched: $matched",'')-join "`n"
 [IO.File]::AppendAllText((Join-Path $report 'run.log'),(Clean ($header+$body)),$utf8)
 $steps.Add([ordered]@{name=$name;command=($tool+' '+($a-join ' '));expected=$expected;actual=$exit;required_marker=$marker;matched=$matched})
 Text (Join-Path $report 'steps.json') ($steps.ToArray()|ConvertTo-Json -Depth 4)
 Write-Output "$name exit=$exit expected=$expected matched=$matched"
 if(-not $matched){$lines|Select-Object -Last 24;throw ('Unexpected result '+$name)}
}
$variants=[ordered]@{
 'unknown-state'='out.state = 99;'; 'unknown-flags'='out.flags |= 4;';
 'missing-root'='out.flags &= !ROOT_KNOWN; out.root_exit = 0;';
 'unflagged-root'='out.state = PENDING; out.flags = 0;';
 'unflagged-active'='out.active = 1;'; 'live-terminal'='out.flags |= ACTIVE_KNOWN; out.active = 1;';
 'cleanup-on-complete'='out.cleanup_mask = 1; out.cleanup_codes[0] = 5;';
 'cleanup-empty'='out.state = CLEANUP_FAILED;';
 'cleanup-no-code'='out.state = CLEANUP_FAILED; out.cleanup_mask = 1;';
 'cleanup-stray-code'='out.state = CLEANUP_FAILED; out.cleanup_mask = 1; out.cleanup_codes = [5,0,6];';
 'cleanup-unknown-bit'='out.state = CLEANUP_FAILED; out.cleanup_mask = 8;';
 'native-on-success'='out.native_error = 87;'; 'finish-unknown'='out.state = 0;';
 'unknown-status'='return 99;'; 'native-no-code'='return NATIVE;';
 'native-invalid-flags'='out.flags |= 4; out.native_error = 87; return NATIVE;';
 'bad-layout-native'='out.version = 2; out.native_error = 87; return NATIVE;'
 'create-metadata'='out.size = 47;'; 'release-metadata'='out.size = 47;'
}
foreach($mask in 1..7){$codes=@(0..2|ForEach-Object{if($mask-band (1-shl $_)){5+$_}else{0}});$variants["valid-cleanup-$mask"]='out.state = CLEANUP_FAILED; out.cleanup_mask = '+$mask+'; out.cleanup_codes = ['+($codes-join ',')+'];'}
if($Cases.Count-eq 0){$Cases=@($variants.Keys)}
foreach($case in $Cases){if(-not $variants.Contains($case)){throw ('Unknown case '+$case)}}
$originalFiles=@(Get-ChildItem (Join-Path $ffi 'go'),(Join-Path $ffi 'src'),(Join-Path $ffi 'include'),(Join-Path $native 'src') -File -Recurse)
$originalFiles+=@(Get-Item (Join-Path $ffi 'Cargo.toml'),(Join-Path $ffi 'Cargo.lock'),(Join-Path $native 'Cargo.toml'),(Join-Path $native 'Cargo.lock'),$PSCommandPath)
$source=@($originalFiles|Sort-Object FullName -Unique|ForEach-Object{[ordered]@{path=$_.FullName.Substring($repo.Length+1).Replace('\','/');sha256=(Hash $_.FullName)}})
Text (Join-Path $report 'source-hashes.json') ($source|ConvertTo-Json -Depth 4)
$ordinary=@('debug','release'|ForEach-Object{$p=Join-Path $LabRoot "target\$_\mcphub_owner_ffi_spike.dll";[ordered]@{path=$p;sha256=(Hash $p)}})
$copy=Join-Path $work 'native';$copyFfi=Join-Path $copy 'ffi'
New-Item -ItemType Directory -Path $copy,$copyFfi|Out-Null
Copy-Item (Join-Path $native 'src') $copy -Recurse
Copy-Item (Join-Path $native 'Cargo.toml'),(Join-Path $native 'Cargo.lock') $copy
Copy-Item (Join-Path $ffi 'src') $copyFfi -Recurse
Copy-Item (Join-Path $ffi 'Cargo.toml'),(Join-Path $ffi 'Cargo.lock') $copyFfi
$lib=Join-Path $copyFfi 'src\lib.rs';$normal=[IO.File]::ReadAllText($lib).Replace("`r`n","`n")
$anchor="    snapshot(owner, out);`n    status`n}"
if(($normal.Split([string[]]@($anchor),[StringSplitOptions]::None).Length-1)-ne 1){throw 'Outcome return anchor changed; do not guess'}
$goRoot=Join-Path $ffi 'go'
if($Phase-eq 'red'){
 # Reproduce the old decoder from its immutable commit in a disposable caller
 # copy. Ordinary working source is never reverted for a negative experiment.
 $regression=Join-Path $work 'caller-regression';New-Item -ItemType Directory $regression|Out-Null
 Copy-Item (Join-Path $ffi 'go'),(Join-Path $ffi 'include') $regression -Recurse
 $goRoot=Join-Path $regression 'go'
 $old=@(& git -C $repo show '085a47fe165bc4be5fcc6ea6d35e2e8dc5c0b449:experiments/rust-process-owner/ffi/go/bridge_windows.go')
 if($LASTEXITCODE-ne 0){throw 'Exact old decoder source unavailable; do not approximate'}
 [IO.File]::WriteAllText((Join-Path $goRoot 'bridge_windows.go'),($old-join "`n")+"`n",$utf8)
 Text (Join-Path $report 'decoder-regression.json') ([ordered]@{commit='085a47fe165bc4be5fcc6ea6d35e2e8dc5c0b449';path='experiments/rust-process-owner/ffi/go/bridge_windows.go';sha256=(Hash (Join-Path $goRoot 'bridge_windows.go'))}|ConvertTo-Json)
}
$mutations=[Collections.Generic.List[object]]::new()
try{
 Check 'exit-control' $env:ComSpec @('/d','/c','exit 7') 7
 Check 'source-head' 'git' @('-C',$repo,'rev-parse','HEAD')
 Check 'go-version' 'go' @('version');Check 'rust-version' 'rustc' @('--version')
 foreach($case in $Cases){
  $before=$anchor;$body=$variants[$case];$tags='ffi_outcome_faults';$env:MCPHUB_FFI_FAULT=''
  $ret=if($body.Contains('return ')){''}else{'return status;'}
  $after="    snapshot(owner, out);`n    if out.state == COMPLETE || out.state == CLEANUP_FAILED { $body $ret }`n    status`n}"
  $test=if($case.StartsWith('valid-cleanup-')){'^TestOutcomePreservesValidCleanupReport$'}else{'^TestOutcomeRejectsContradictoryMetadata$'}
  if($case-eq 'create-metadata'){$before='snapshot(&owner, out);';$after=$before+' out.size = 47;';$test='^TestCorruptCreateRetainsTransferredOwner$';$tags='ffi_faults';$env:MCPHUB_FFI_FAULT=$case}
  if($case-eq 'release-metadata'){$before='drop(Box::from_raw(raw));';$after='out.size = 47; '+$before;$test='^TestCorruptReleasePreservesDeallocationFact$';$tags='ffi_faults';$env:MCPHUB_FFI_FAULT=$case}
  if(($normal.Split([string[]]@($before),[StringSplitOptions]::None).Length-1)-ne 1){throw 'Selected mutation anchor changed'}
  [IO.File]::WriteAllText($lib,$normal.Replace($before,$after),$utf8)
  $mutations.Add([ordered]@{case=$case;before=$before;after=$after;mutated_sha256=(Hash $lib)})
  Text (Join-Path $report 'mutations.json') ($mutations.ToArray()|ConvertTo-Json -Depth 4)
  Check ('build-'+$case) 'cargo' @('build','--manifest-path',(Join-Path $copyFfi 'Cargo.toml'),'--offline','--locked')
  $env:MCPHUB_RX02_FFI_DLL=Join-Path $env:CARGO_TARGET_DIR 'debug\mcphub_owner_ffi_spike.dll';$env:MCPHUB_RX02_FFI_CHILD=$child
  $env:MCPHUB_FFI_OUTCOME_CASE=$case
  $expected=if($Phase-eq 'red'){1}else{0};$marker=if($Phase-eq 'red'){'contradictory outcome accepted without protocol rejection'}else{'PASS'}
  Push-Location $goRoot
  try{Check ('test-'+$case) 'go' @('test','-tags',$tags,'.','-run',$test,'-race','-count=1','-timeout=30s','-v') $expected $marker}finally{Pop-Location}
 }
 foreach($item in $source){if((Hash (Join-Path $repo $item.path))-ne $item.sha256){throw 'Ordinary source changed during controlled tests'}}
 foreach($item in $ordinary){if((Hash $item.path)-ne $item.sha256){throw 'Ordinary DLL changed'}}
 Text (Join-Path $report 'ordinary-artifacts.json') ($ordinary|ConvertTo-Json -Depth 3)
 Text (Join-Path $report 'COMPLETED.txt') "Phase=$Phase; all $($Cases.Count) selected variants matched semantic assertions. Synthetic serialized outcomes after real native settlement; NOT actual kernel cleanup faults, product readiness or hostile-DLL safety. Ordinary source/DLLs unchanged."
}finally{
 $env:MCPHUB_FFI_OUTCOME_CASE='';$env:MCPHUB_FFI_FAULT='';$env:GOEXPERIMENT=''
 $sums=@(Get-ChildItem $report -File|Where-Object{$_.Name-ne 'SHA256SUMS.txt'}|Sort-Object Name|ForEach-Object{(Hash $_.FullName)+'  '+$_.Name})
 Text (Join-Path $report 'SHA256SUMS.txt') ($sums-join "`n")
}

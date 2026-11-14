# Explicit retirement of this completed qualification's owned reproducible copies.
# Never deletes or empties trash. Refuses any identity mismatch before movement.
[CmdletBinding()]
param([Parameter(Mandatory=$true)][string]$LabRoot,
      [Parameter(Mandatory=$true)][string]$TrashName)
$ErrorActionPreference='Stop'
$LabRoot=[IO.Path]::GetFullPath($LabRoot)
if(-not $LabRoot.StartsWith('D:\dev\ChatGPT\',[StringComparison]::OrdinalIgnoreCase)){throw 'Approved lab required'}
if($TrashName -notmatch '^[a-zA-Z0-9._-]+$'){throw 'Simple trash directory name required'}
$verify=(Get-Item $PSScriptRoot).Parent.FullName
function Hash([string]$p){$h=[Security.Cryptography.SHA256]::Create();try{[BitConverter]::ToString($h.ComputeHash([IO.File]::ReadAllBytes($p))).Replace('-','').ToLowerInvariant()}finally{$h.Dispose()}}
function Text([string]$p,[string]$t){$t=[regex]::Replace($t.Replace("`r`n","`n"),'(?m)[ \t]+$','');[IO.File]::WriteAllText($p,$t.TrimEnd()+"`n",[Text.UTF8Encoding]::new($false))}
function Json([string]$p){ConvertFrom-Json -InputObject ([IO.File]::ReadAllText($p))}
$plan=@()
foreach($name in @('red','green-initial','red-reproducible','final')){
 $report=Join-Path $verify "runs\outcome-$name"
 $work=Join-Path $LabRoot "qualification-outcome-$name"
 if(-not(Test-Path "$report\COMPLETED.txt")-or-not(Test-Path $work)){throw "Incomplete or absent owned copy: $name"}
 $rows=Json "$report\mutations.json";$last=$rows[-1]
 if((Hash "$work\native\ffi\src\lib.rs")-ne $last.mutated_sha256){throw "Changed mutated source: $name"}
 $source=Json "$report\source-hashes.json"
 foreach($entry in $source){
  $p=$entry.path;$relative=$null
  if($p.StartsWith('experiments/rust-process-owner/ffi/src/')-and-not $p.EndsWith('/lib.rs')){$relative='native\ffi\'+$p.Substring('experiments/rust-process-owner/ffi/'.Length)}
  elseif($p.StartsWith('experiments/rust-process-owner/src/')){$relative='native\'+$p.Substring('experiments/rust-process-owner/'.Length)}
  elseif($p-in @('experiments/rust-process-owner/ffi/Cargo.toml','experiments/rust-process-owner/ffi/Cargo.lock')){$relative='native\ffi\'+[IO.Path]::GetFileName($p)}
  elseif($p-in @('experiments/rust-process-owner/Cargo.toml','experiments/rust-process-owner/Cargo.lock')){$relative='native\'+[IO.Path]::GetFileName($p)}
  if($relative-and (Hash (Join-Path $work $relative))-ne $entry.sha256){throw "Changed copied source: $p"}
 }
 if(Test-Path "$work\caller-regression"){
  $reg=Json "$report\decoder-regression.json"
  if((Hash "$work\caller-regression\go\bridge_windows.go")-ne $reg.sha256){throw 'Old caller copy changed'}
 }
 $plan+=[ordered]@{directory="qualification-outcome-$name";report="runs/outcome-$name";final_variant=$last.case;verified_source_sha256=$last.mutated_sha256}
}
$report=Join-Path $verify 'runs\native-cleanup-outcome-regression'
$work=Join-Path $LabRoot 'qualification-native-cleanup-outcome-regression'
if(-not(Test-Path "$report\COMPLETED.txt")){throw 'Native-close run incomplete'}
foreach($entry in (Json "$report\instrumented-source-hashes.json")){
 if((Hash (Join-Path $work $entry.path))-ne $entry.sha256){throw 'Instrumented native-close source changed'}
}
$plan+=[ordered]@{directory='qualification-native-cleanup-outcome-regression';report='runs/native-cleanup-outcome-regression';verified='full instrumented-source manifest'}
Text (Join-Path $PSScriptRoot 'cleanup-preflight.json') ($plan|ConvertTo-Json -Depth 4)
$trash=Join-Path 'D:\dev\ChatGPT\trash' $TrashName
if(Test-Path $trash){throw 'Trash destination exists; do not merge or overwrite'}
New-Item -ItemType Directory -Path $trash|Out-Null
$moved=@()
foreach($entry in $plan){
 $from=Join-Path $LabRoot $entry.directory;$to=Join-Path $trash $entry.directory
 Move-Item -LiteralPath $from -Destination $to
 if((Test-Path $from)-or-not(Test-Path $to)){throw 'Move not confirmed'}
 $moved+=$entry.directory
 Text (Join-Path $PSScriptRoot 'cleanup-receipt.json') ([ordered]@{verified_moved=$moved;destination=('<APPROVED_TRASH>/'+$TrashName);deleted=$false;trash_emptied=$false;active_source_cache_ordinary_targets_retained=$true}|ConvertTo-Json -Depth 4)
}
Write-Output "Verified direct trash moves: $($moved.Count); no deletion or emptying"

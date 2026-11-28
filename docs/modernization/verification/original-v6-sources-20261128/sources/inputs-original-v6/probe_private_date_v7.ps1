#Requires -Version 7.5
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$BindingsPath,
    [Parameter(Mandatory=$true)][ValidatePattern('^[0-9a-f]{64}$')][string]$BindingsSHA256,
    [Parameter(Mandatory=$true)][string]$OutputPath
)
$ErrorActionPreference='Stop'
$stream=[IO.File]::OpenRead($BindingsPath); $hash=[Security.Cryptography.SHA256]::Create()
try { $actual=[Convert]::ToHexString($hash.ComputeHash($stream)).ToLowerInvariant() }
finally { $stream.Dispose(); $hash.Dispose() }
if ($actual -cne $BindingsSHA256) { throw 'Reviewed input DATA changed' }
$raw=[IO.File]::ReadAllText($BindingsPath)
$automatic=$raw | ConvertFrom-Json -AsHashtable
$literal=$raw | ConvertFrom-Json -AsHashtable -DateKind String
$expected='2026-11-28T11:11:34Z'
$pattern='^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$'
$oldAccepted=($automatic.release_build_date -cmatch $pattern)
$newAccepted=($literal.release_build_date -cmatch $pattern)
if ($automatic.release_build_date -isnot [datetime] -or $oldAccepted -or
    $literal.release_build_date -isnot [string] -or $literal.release_build_date -cne $expected -or
    -not $newAccepted -or $literal.release_version -cne '0.4.60') {
    throw 'Observed failure hypothesis was not reproduced by DATA-only parsing'
}
$result=[ordered]@{
    schema='original-private-date-parser-probe-v1'; status='PASS'
    powershell_version=$PSVersionTable.PSVersion.ToString(); bindings_sha256=$actual
    default_type=$automatic.release_build_date.GetType().FullName
    default_display=[string]$automatic.release_build_date; old_regex_accepted=$oldAccepted
    string_type=$literal.release_build_date.GetType().FullName
    exact_literal=$literal.release_build_date; fixed_regex_accepted=$newAccepted
    child_processes_started=0; source_or_cache_read=$false; original_v6_gate_reclassified=$false
}
$bytes=[Text.UTF8Encoding]::new($false).GetBytes(($result | ConvertTo-Json -Depth 5)+[char]10)
$out=[IO.File]::Open($OutputPath,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
try { $out.Write($bytes,0,$bytes.Length); $out.Flush($true) } finally { $out.Dispose() }
$result | ConvertTo-Json -Depth 5

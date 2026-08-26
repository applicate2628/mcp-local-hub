#Requires -Version 5.1
[CmdletBinding()]
param([switch]$Unsigned)
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$image = Join-Path $root 'mcphub-cst-runtime.exe'
$manifest = Join-Path $root 'cst-native-runtime-manifest-v1.json'
$py = (Get-Command py.exe -ErrorAction Stop).Source
& $py -3 (Join-Path $root 'verify_cst_native_pe.py') $image
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
$record = Get-Content -LiteralPath $manifest -Raw | ConvertFrom-Json
$hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $image).Hash.ToLowerInvariant()
if ($record.runtime_image_sha256 -ne $hash -or $record.unsigned_builds_byte_identical -ne $true) { throw 'manifest/image binding invalid' }
Write-Host "W2 manifest/image verification PASS $hash"

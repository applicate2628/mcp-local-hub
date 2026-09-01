#Requires -Version 5.1
# Build script for mcp-local-hub on Windows.
#
# Produces the Windows product payload declared in
# windows-product-artifacts.json:
#   * mcphub.exe: canonical terminal CLI (CUI subsystem 3).
#   * mcphub-windowless.exe: supported GUI/Explorer adapter (GUI subsystem 2).
#   * mcphub-pe-admit.exe: bounded role-aware PE admission helper (CUI 3).
# The two product artifacts receive identical version/commit/build-date ldflags.
# In addition, mcphub.exe carries the existing Windows version resource.
#
# The build provides:
#   * Windows version resource (FileDescription, CompanyName, etc.) embedded
#     via cmd/mcphub/resource.syso (regenerated from versioninfo.json).
#   * Build metadata (version, commit, build date) embedded via ldflags -X;
#     visible via `./mcphub.exe version`.
#   * Reproducible output via -trimpath.
#
# Prerequisites: Go 1.26+, git. Goversioninfo is pinned to v1.5.0 via
# `go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.5.0`.
#
# Note on antivirus: unsigned Go binaries can trip Windows Defender's ML
# heuristic (Wacatac.B!ml and friends). If mcphub.exe disappears after build,
# add <repo-root> to Defender exclusions. See INSTALL.md.

param(
    [switch]$PrintPlanJson
)

$ErrorActionPreference = "Stop"

$version = "0.4.35"
try { $commit = (git rev-parse --short HEAD 2>$null) } catch { $commit = "unknown" }
if ([string]::IsNullOrWhiteSpace($commit)) { $commit = "unknown" }
$buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

$outDir = "bin"
$contractPath = Join-Path $PSScriptRoot "windows-product-artifacts.json"
$contract = Get-Content -Raw -LiteralPath $contractPath | ConvertFrom-Json
if ($contract.schemaVersion -ne 1 -or @($contract.artifacts).Count -ne 3) {
    throw "windows-product-artifacts.json must be schemaVersion 1 with exactly three artifacts"
}

$artifacts = @(
    foreach ($artifact in $contract.artifacts) {
        if ($artifact.subsystem -notin @(2, 3)) {
            throw "unsupported PE subsystem $($artifact.subsystem) for role $($artifact.role)"
        }
        $flags = @()
        if ($artifact.productMetadata) {
            $flags += "-X main.version=$version"
            $flags += "-X main.commit=$commit"
            $flags += "-X main.buildDate=$buildDate"
        }
        if ($artifact.subsystem -eq 2) {
            $flags += "-H windowsgui"
        }
        [ordered]@{
            role = [string]$artifact.role
            filename = [string]$artifact.filename
            source = [string]$artifact.source
            subsystem = [int]$artifact.subsystem
            productMetadata = [bool]$artifact.productMetadata
            npmPayload = [bool]$artifact.npmPayload
            npmBin = [bool]$artifact.npmBin
            ldflags = ($flags -join " ")
            output = "$outDir/$($artifact.filename)"
        }
    }
)

$helper = @($artifacts | Where-Object role -eq "admission-helper")
$productArtifacts = @($artifacts | Where-Object productMetadata)
if ($helper.Count -ne 1 -or $productArtifacts.Count -ne 2) {
    throw "Windows artifact contract must declare one admission-helper and two productMetadata artifacts"
}
$admissionCommands = @(
    foreach ($artifact in $productArtifacts) {
        [ordered]@{
            executable = $helper[0].output
            argv = @($artifact.role, $artifact.output)
        }
    }
)
$plan = [ordered]@{
    schemaVersion = 1
    version = $version
    commit = $commit
    buildDate = $buildDate
    artifacts = $artifacts
    admissionCommands = $admissionCommands
}

if ($PrintPlanJson) {
    $plan | ConvertTo-Json -Depth 6
    exit 0
}

if (-not (Test-Path $outDir)) {
    New-Item -ItemType Directory -Path $outDir | Out-Null
}

Write-Host "==> Generating Windows version resource (cmd/mcphub/resource.syso)"
go generate ./cmd/mcphub
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

foreach ($artifact in $artifacts) {
    $outFile = Join-Path $outDir $artifact.filename
    $buildArgs = @("build", "-trimpath")
    if (-not [string]::IsNullOrWhiteSpace($artifact.ldflags)) {
        $buildArgs += @("-ldflags", $artifact.ldflags)
    }
    $buildArgs += @("-o", $outFile, $artifact.source)
    Write-Host "==> Building role=$($artifact.role) subsystem=$($artifact.subsystem) metadata=$($artifact.productMetadata) -> $outFile"
    & go @buildArgs
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

$admitFile = Join-Path $outDir $helper[0].filename
foreach ($command in $admissionCommands) {
    $candidate = $command.argv[1] -replace '/', [IO.Path]::DirectorySeparatorChar
    & $admitFile $command.argv[0] $candidate
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
}

foreach ($artifact in $artifacts) {
    $outFile = Join-Path $outDir $artifact.filename
    if (-not (Test-Path -LiteralPath $outFile -PathType Leaf)) {
        Write-Error "$outFile missing after build — check Defender exclusions (see INSTALL.md)."
        exit 1
    }
}

Write-Host "==> Canonical CLI Windows version resource:"
(Get-Item (Join-Path $outDir "mcphub.exe")).VersionInfo | Format-List FileVersion,ProductName,FileDescription,CompanyName,LegalCopyright,Comments
Write-Host "==> Done. Role/subsystem admission passed; product metadata version=$version commit=$commit buildDate=$buildDate."

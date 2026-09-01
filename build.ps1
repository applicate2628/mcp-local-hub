#Requires -Version 5.1
# Build script for mcp-local-hub on Windows.
#
# Produces the Windows product payload declared in
# windows-product-artifacts.json:
#   * mcphub.exe: canonical terminal CLI (CUI subsystem 3).
#   * mcphub-windowless.exe: supported GUI/Explorer adapter (GUI subsystem 2).
#   * mcphub-pe-admit.exe: bounded role-aware PE admission helper (CUI 3).
# The two product artifacts receive role-specific VERSIONINFO resources from
# one captured version/commit/build-date tuple. The canonical CLI receives the
# same tuple through its existing ldflags surface.
#
# The build provides:
#   * Windows version resources embedded in both product executables.
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

$version = "0.4.36"
try { $commit = (git rev-parse HEAD 2>$null) } catch { $commit = "unknown" }
if ([string]::IsNullOrWhiteSpace($commit)) { $commit = "unknown" }
$buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$versionCore = $version.Split('-', 2)[0].Split('+', 2)[0].Split('.')
if ($versionCore.Count -ne 3 -or @($versionCore | Where-Object { $_ -notmatch '^(0|[1-9][0-9]*)$' }).Count -ne 0) {
    throw "product version must have a three-component SemVer core: $version"
}

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
        if ($artifact.role -eq "cli") {
            $flags += "-X main.version=$version"
            $flags += "-X main.commit=$commit"
            $flags += "-X main.buildDate=$buildDate"
        }
        if ($artifact.subsystem -eq 2) {
            $flags += "-H windowsgui"
        }
        $versionInfo = $null
        if ($artifact.productMetadata) {
            $internalName = if ($artifact.role -eq "cli") { "mcphub-cli" } else { "mcphub-windowless" }
            $versionInfo = [ordered]@{
                ProductName = "mcp-local-hub"
                ProductVersion = $version
                PrivateBuild = "mcphub-build-v1;commit=$commit;build_date=$buildDate"
                SpecialBuild = "mcphub-role-v1;role=$($artifact.role)"
                InternalName = $internalName
                OriginalFilename = [string]$artifact.filename
            }
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
            versionInfo = $versionInfo
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

$resourceScratch = Join-Path (Join-Path $PSScriptRoot ".scratch") "windows-versioninfo-$PID-$([Guid]::NewGuid().ToString('N'))"
$generatedResources = @()
try {
    New-Item -ItemType Directory -Force -Path $resourceScratch | Out-Null
    $templatePath = Join-Path $PSScriptRoot "cmd/mcphub/versioninfo.json"
    foreach ($artifact in $productArtifacts) {
        $config = Get-Content -Raw -LiteralPath $templatePath | ConvertFrom-Json
        $config.IconPath = (Join-Path $PSScriptRoot "cmd/mcphub/mcphub.ico")
        $config.FixedFileInfo.FileVersion.Major = [int]$versionCore[0]
        $config.FixedFileInfo.FileVersion.Minor = [int]$versionCore[1]
        $config.FixedFileInfo.FileVersion.Patch = [int]$versionCore[2]
        $config.FixedFileInfo.FileVersion.Build = 0
        $config.FixedFileInfo.ProductVersion.Major = [int]$versionCore[0]
        $config.FixedFileInfo.ProductVersion.Minor = [int]$versionCore[1]
        $config.FixedFileInfo.ProductVersion.Patch = [int]$versionCore[2]
        $config.FixedFileInfo.ProductVersion.Build = 0
        $config.StringFileInfo.FileVersion = "$($versionCore[0]).$($versionCore[1]).$($versionCore[2]).0"
        foreach ($entry in $artifact.versionInfo.GetEnumerator()) {
            if ($config.StringFileInfo.PSObject.Properties.Name -contains $entry.Key) {
                $config.StringFileInfo.($entry.Key) = $entry.Value
            } else {
                $config.StringFileInfo | Add-Member -NotePropertyName $entry.Key -NotePropertyValue $entry.Value
            }
        }
        $resourceJson = Join-Path $resourceScratch "$($artifact.role).json"
        $config | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $resourceJson -Encoding UTF8
        $commandDir = Join-Path $PSScriptRoot ($artifact.source -replace '^\./', '')
        $resourcePath = Join-Path $commandDir "zz_product_versioninfo.syso"
        if (Test-Path -LiteralPath $resourcePath) {
            Remove-Item -LiteralPath $resourcePath -Force
        }
        Write-Host "==> Generating VERSIONINFO role=$($artifact.role)"
        & go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.5.0 -64 -o $resourcePath $resourceJson
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
        $generatedResources += $resourcePath
    }

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
} finally {
    foreach ($resourcePath in $generatedResources) {
        Remove-Item -LiteralPath $resourcePath -Force -ErrorAction SilentlyContinue
    }
    Remove-Item -LiteralPath $resourceScratch -Recurse -Force -ErrorAction SilentlyContinue
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

Write-Host "==> Product VERSIONINFO resources:"
foreach ($artifact in $productArtifacts) {
    (Get-Item (Join-Path $outDir $artifact.filename)).VersionInfo | Format-List ProductVersion,PrivateBuild,SpecialBuild,InternalName,OriginalFilename
}
Write-Host "==> Done. Role/subsystem/artifact-identity admission passed; product metadata version=$version commit=$commit buildDate=$buildDate."

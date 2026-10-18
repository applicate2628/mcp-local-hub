#Requires -Version 5.1
# Build script for mcp-local-hub on Windows.
#
# Produces the Windows product payload declared in
# windows-product-artifacts.json:
#   * mcphub.exe: the sole native product executable (GUI subsystem 2).
#   * windows-product-identity.json: generated non-executable admitted identity.
# The product receives VERSIONINFO and linker metadata from one captured
# version/commit/build-date tuple.
#
# The build provides:
#   * Windows version resources embedded in the product executable.
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
    [switch]$PrintPlanJson,
    [switch]$TestStateIsolatedProduct,
    [ValidateSet("amd64", "arm64")]
    [string]$WindowsTargetArch,
    [string]$OutputDirectory = "bin",
    [string]$ReleaseVersion,
    [string]$ReleaseCommit,
    [string]$ReleaseBuildDate,
    [string]$SourceSnapshotPath
)

$ErrorActionPreference = "Stop"

$version = "0.4.41"
try { $commit = (git -C $PSScriptRoot rev-parse HEAD 2>$null) } catch { $commit = "unknown" }
if ([string]::IsNullOrWhiteSpace($commit)) { $commit = "unknown" }
$buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$releaseTupleSupplied = -not [string]::IsNullOrWhiteSpace($ReleaseVersion) -or
    -not [string]::IsNullOrWhiteSpace($ReleaseCommit) -or
    -not [string]::IsNullOrWhiteSpace($ReleaseBuildDate)
if ($TestStateIsolatedProduct -and $releaseTupleSupplied) {
    throw "TestStateIsolatedProduct does not accept release metadata"
}
if ($TestStateIsolatedProduct) {
    if (-not $PSBoundParameters.ContainsKey("OutputDirectory")) {
        throw "TestStateIsolatedProduct requires an explicit absolute OutputDirectory beneath .scratch"
    }
    if ($OutputDirectory -notmatch '^(?:[A-Za-z]:[\\/]|\\\\[^\\/]+[\\/][^\\/]+(?:[\\/]|$))') {
        throw "TestStateIsolatedProduct OutputDirectory must be absolute"
    }
    $testScratchRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot ".scratch")).TrimEnd(
        [IO.Path]::DirectorySeparatorChar,
        [IO.Path]::AltDirectorySeparatorChar)
    $testOutputDirectory = [IO.Path]::GetFullPath($OutputDirectory).TrimEnd(
        [IO.Path]::DirectorySeparatorChar,
        [IO.Path]::AltDirectorySeparatorChar)
    $testScratchPrefix = $testScratchRoot + [IO.Path]::DirectorySeparatorChar
    if (-not $testOutputDirectory.StartsWith(
        $testScratchPrefix,
        [StringComparison]::OrdinalIgnoreCase)) {
        throw "TestStateIsolatedProduct OutputDirectory must be strictly beneath $testScratchRoot"
    }
    $OutputDirectory = $testOutputDirectory
}
if ([string]::IsNullOrWhiteSpace($WindowsTargetArch) -and $releaseTupleSupplied) {
    throw "ReleaseVersion, ReleaseCommit, and ReleaseBuildDate require WindowsTargetArch"
}
if (-not [string]::IsNullOrWhiteSpace($WindowsTargetArch)) {
    if ([string]::IsNullOrWhiteSpace($ReleaseVersion) -or
        [string]::IsNullOrWhiteSpace($ReleaseCommit) -or
        [string]::IsNullOrWhiteSpace($ReleaseBuildDate)) {
        throw "WindowsTargetArch requires ReleaseVersion, ReleaseCommit, and ReleaseBuildDate"
    }
    if ($ReleaseCommit -notmatch '^[0-9a-f]{40}$') {
        throw "ReleaseCommit must be the full lowercase 40-hex Git commit"
    }
    if ($ReleaseBuildDate -notmatch '^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$') {
        throw "ReleaseBuildDate must use strict UTC-seconds format"
    }
    try {
        $parsedBuildDate = [datetime]::ParseExact(
            $ReleaseBuildDate,
            "yyyy-MM-ddTHH:mm:ssZ",
            [Globalization.CultureInfo]::InvariantCulture,
            [Globalization.DateTimeStyles]::AssumeUniversal)
    } catch {
        throw "ReleaseBuildDate must be a real strict UTC-seconds timestamp"
    }
    if ($parsedBuildDate.ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ") -ne $ReleaseBuildDate) {
        throw "ReleaseBuildDate must be canonical UTC"
    }
    $version = $ReleaseVersion
    $commit = $ReleaseCommit
    $buildDate = $ReleaseBuildDate
}
$versionCore = $version.Split('-', 2)[0].Split('+', 2)[0].Split('.')
if ($versionCore.Count -ne 3 -or @($versionCore | Where-Object { $_ -notmatch '^(0|[1-9][0-9]*)$' }).Count -ne 0) {
    throw "product version must have a three-component SemVer core: $version"
}

$outDir = $OutputDirectory
$publisherSource = $null
function Get-VerifiedPublisherSourceInput {
    # Root's full Source/table/helper preflight precedes this helper compilation.
    # This script selects no source files and never serializes the epoch body.
    Push-Location -LiteralPath $PSScriptRoot
    try {
        $rows = @(& go run ./tools/windows-product-validate --source-snapshot $SourceSnapshotPath --source-root $PSScriptRoot)
        if ($LASTEXITCODE -ne 0 -or $rows.Count -ne 1) { throw "PUBLISHER_SOURCE_TABLE_INVALID: verifier failed" }
        $row = $rows[0] | ConvertFrom-Json
        if ($row.schema -cne "windows-publisher-source-input-v1" -or
            [string]$row.source_epoch -cnotmatch '^[0-9a-f]{64}$' -or
            [string]$row.source_snapshot_sha256 -cnotmatch '^[0-9a-f]{64}$' -or
            [int]$row.file_count -le 0) { throw "PUBLISHER_SOURCE_TABLE_INVALID: verifier output" }
        return $row
    } finally { Pop-Location }
}
if (-not [string]::IsNullOrEmpty($SourceSnapshotPath)) {
    if ($TestStateIsolatedProduct) { throw "PUBLISHER_SOURCE_TABLE_INVALID: test product cannot issue prepared source" }
    $SourceSnapshotPath = [IO.Path]::GetFullPath($SourceSnapshotPath)
    $sourcePrefix = [IO.Path]::GetFullPath($PSScriptRoot).TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
    $outputAbsolute = [IO.Path]::GetFullPath($outDir)
    if ($outputAbsolute.StartsWith($sourcePrefix, [StringComparison]::OrdinalIgnoreCase) -or $outputAbsolute.TrimEnd([IO.Path]::DirectorySeparatorChar) -ceq $sourcePrefix.TrimEnd([IO.Path]::DirectorySeparatorChar)) {
        throw "PUBLISHER_SOURCE_TABLE_INVALID: output must be outside Source"
    }
    $publisherSource = Get-VerifiedPublisherSourceInput
}
$contractPath = Join-Path $PSScriptRoot "windows-product-artifacts.json"
$contract = Get-Content -Raw -LiteralPath $contractPath | ConvertFrom-Json
if ($contract.schemaVersion -ne 2 -or @($contract.artifacts).Count -ne 1 -or
    $contract.identityManifest.schema -ne "windows-product-identity-v1" -or
    $contract.identityManifest.filename -ne "windows-product-identity.json") {
    throw "windows-product-artifacts.json must be schemaVersion 2 with one product and the V1 identity manifest"
}

$artifacts = @(
    foreach ($artifact in $contract.artifacts) {
        if ($artifact.subsystem -notin @(2, 3)) {
            throw "unsupported PE subsystem $($artifact.subsystem) for role $($artifact.role)"
        }
        $flags = @()
        if ($artifact.role -eq "product") {
            $flags += "-X main.version=$version"
            $flags += "-X main.commit=$commit"
            $flags += "-X main.buildDate=$buildDate"
            if ($null -ne $publisherSource) {
                $flags += "-X mcp-local-hub/internal/buildinfo.publisherSourceEpoch=$($publisherSource.source_epoch)"
            }
        }
        if ($artifact.subsystem -eq 2) {
            $flags += "-H windowsgui"
        }
        $versionInfo = $null
        if ($artifact.productMetadata) {
            $versionInfo = [ordered]@{
                ProductName = "mcp-local-hub"
                ProductVersion = $version
                PrivateBuild = "mcphub-build-v1;commit=$commit;build_date=$buildDate"
                SpecialBuild = "mcphub-role-v1;role=$($artifact.role)"
                InternalName = "mcphub"
                OriginalFilename = [string]$artifact.filename
            }
            if ($null -ne $publisherSource) {
                $originalComments = [string](Get-Content -Raw -Encoding UTF8 -LiteralPath (Join-Path $PSScriptRoot "cmd/mcphub/versioninfo.json") | ConvertFrom-Json).StringFileInfo.Comments
                if ($originalComments.Contains("mcphub-source-v1;sha256=")) { throw "PUBLISHER_SOURCE_TABLE_INVALID: template already stamped" }
                $versionInfo.Comments = $originalComments + [char]10 + "mcphub-source-v1;sha256=$($publisherSource.source_epoch)"
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

$productArtifacts = @($artifacts | Where-Object productMetadata)
if ($productArtifacts.Count -ne 1 -or $productArtifacts[0].role -ne "product" -or
    $productArtifacts[0].subsystem -ne 2 -or $productArtifacts[0].npmBin) {
    throw "Windows artifact contract must declare one GUI product with no platform-package bin owner"
}
$effectiveWindowsTargetArch = $WindowsTargetArch
if ([string]::IsNullOrWhiteSpace($effectiveWindowsTargetArch)) {
    $effectiveWindowsTargetArch = (& go env GOARCH).Trim()
}
if ($effectiveWindowsTargetArch -notin @("amd64", "arm64")) {
    throw "Windows build target must resolve to amd64 or arm64, got $effectiveWindowsTargetArch"
}
$expectedMachine = if ($effectiveWindowsTargetArch -eq "arm64") { 0xaa64 } else { 0x8664 }
$resourceGeneratorArgs = @("-64")
if ($effectiveWindowsTargetArch -eq "arm64") {
    $resourceGeneratorArgs += "-arm"
}
$identityManifest = [ordered]@{
    schema = [string]$contract.identityManifest.schema
    filename = [string]$contract.identityManifest.filename
    output = "$outDir/$($contract.identityManifest.filename)"
}
$admissionCommand = [ordered]@{
    executable = "go"
    argv = @(
        "run",
        "./tools/windows-product-validate",
        "--candidate",
        $productArtifacts[0].output,
        "--expected-version",
        $version,
        "--expected-commit",
        $commit,
        "--expected-build-date",
        $buildDate
    )
}
$retiredOutputFiles = @("mcphub-windowless.exe", "mcphub-pe-admit.exe")
if ($null -ne $publisherSource) {
    $admissionCommand.argv += @("--expected-source-epoch", [string]$publisherSource.source_epoch)
}
$buildTags = @()
if ($TestStateIsolatedProduct) {
    $buildTags = @("test_state_path_env")
}
$plan = [ordered]@{
    schemaVersion = 2
    testOnly = [bool]$TestStateIsolatedProduct
    buildTags = @($buildTags)
    version = $version
    commit = $commit
    buildDate = $buildDate
    windowsTargetArch = $WindowsTargetArch
    expectedMachine = $expectedMachine
    resourceGeneratorArgs = $resourceGeneratorArgs
    artifacts = $artifacts
    identityManifest = $identityManifest
    admissionCommand = $admissionCommand
    retiredOutputFiles = $retiredOutputFiles
}
if ($null -ne $publisherSource) {
    $plan.publisherSource = [ordered]@{
        source_epoch = [string]$publisherSource.source_epoch
        source_snapshot_sha256 = [string]$publisherSource.source_snapshot_sha256
        file_count = [int]$publisherSource.file_count
    }
}

if ($PrintPlanJson) {
    $plan | ConvertTo-Json -Depth 6
    exit 0
}

if (-not (Test-Path $outDir)) {
    New-Item -ItemType Directory -Path $outDir | Out-Null
}
foreach ($retiredFilename in $retiredOutputFiles) {
    $retiredPath = Join-Path $outDir $retiredFilename
    if (Test-Path -LiteralPath $retiredPath) {
        Remove-Item -LiteralPath $retiredPath -Force
    }
}
$identityManifestPath = Join-Path $outDir $identityManifest.filename
if (Test-Path -LiteralPath $identityManifestPath) {
    Remove-Item -LiteralPath $identityManifestPath -Force
}

$resourceScratch = Join-Path (Join-Path $PSScriptRoot ".scratch") "windows-versioninfo-$PID-$([Guid]::NewGuid().ToString('N'))"
$generatedResources = @()
$hadGOOS = Test-Path Env:GOOS
$hadGOARCH = Test-Path Env:GOARCH
$priorGOOS = $env:GOOS
$priorGOARCH = $env:GOARCH
$hadGOFLAGS = Test-Path Env:GOFLAGS
$priorGOFLAGS = $env:GOFLAGS
if ($TestStateIsolatedProduct) {
    $env:GOFLAGS = ""
}
$builderPrimaryError = $null
$builderPrimaryExitCode = $null
$builderPrimaryExitStage = $null
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
        [IO.File]::WriteAllText($resourceJson, ($config | ConvertTo-Json -Depth 8), (New-Object Text.UTF8Encoding($false)))
        $commandDir = Join-Path $PSScriptRoot ($artifact.source -replace '^\./', '')
        $resourcePath = Join-Path $commandDir "zz_product_versioninfo.syso"
        if (Test-Path -LiteralPath $resourcePath) {
            Remove-Item -LiteralPath $resourcePath -Force
        }
        $generatedResources += $resourcePath
        Write-Host "==> Generating VERSIONINFO role=$($artifact.role)"
        & go run github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.5.0 @resourceGeneratorArgs -o $resourcePath $resourceJson
        if ($LASTEXITCODE -ne 0) {
            $builderPrimaryExitCode = [int]$LASTEXITCODE
            $builderPrimaryExitStage = "version-resource"
            exit $LASTEXITCODE
        }
    }

    if (-not [string]::IsNullOrWhiteSpace($WindowsTargetArch)) {
        $env:GOOS = "windows"
        $env:GOARCH = $WindowsTargetArch
    }
    foreach ($artifact in $artifacts) {
        $outFile = Join-Path $outDir $artifact.filename
        $buildArgs = @("build", "-trimpath")
        if ($TestStateIsolatedProduct -and $artifact.role -eq "product") {
            $buildArgs += @("-tags", $buildTags[0])
        }
        if (-not [string]::IsNullOrWhiteSpace($artifact.ldflags)) {
            $buildArgs += @("-ldflags", $artifact.ldflags)
        }
        $buildArgs += @("-o", $outFile, $artifact.source)
        Write-Host "==> Building role=$($artifact.role) subsystem=$($artifact.subsystem) metadata=$($artifact.productMetadata) -> $outFile"
        & go @buildArgs
        if ($LASTEXITCODE -ne 0) {
            $builderPrimaryExitCode = [int]$LASTEXITCODE
            $builderPrimaryExitStage = "product-build"
            exit $LASTEXITCODE
        }
    }
} catch {
    $builderPrimaryError = $_
    throw
} finally {
    $builderCleanupErrors = [Collections.Generic.List[Exception]]::new()
    try {
        if ($hadGOOS) { $env:GOOS = $priorGOOS } elseif (Test-Path Env:GOOS) { Remove-Item Env:GOOS -ErrorAction Stop }
    } catch { $builderCleanupErrors.Add($_.Exception) }
    try {
        if ($hadGOARCH) { $env:GOARCH = $priorGOARCH } elseif (Test-Path Env:GOARCH) { Remove-Item Env:GOARCH -ErrorAction Stop }
    } catch { $builderCleanupErrors.Add($_.Exception) }
    try {
        if ($TestStateIsolatedProduct) {
            if ($hadGOFLAGS) { $env:GOFLAGS = $priorGOFLAGS } elseif (Test-Path Env:GOFLAGS) { Remove-Item Env:GOFLAGS -ErrorAction Stop }
        }
    } catch { $builderCleanupErrors.Add($_.Exception) }

    # This original builder owns only its declared syso files and the private
    # resource JSON directory. An error in one target never skips another.
    $cleanupTargets = @(
        foreach ($ownedResource in $generatedResources) { @{ Path = $ownedResource; Directory = $false } }
        @{ Path = $resourceScratch; Directory = $true }
    )
    foreach ($cleanupTarget in $cleanupTargets) {
        $cleanupAbsolute = $null
        try {
            $sourceAbsolute = [IO.Path]::GetFullPath($PSScriptRoot).TrimEnd([IO.Path]::DirectorySeparatorChar)
            $cleanupAbsolute = [IO.Path]::GetFullPath([string]$cleanupTarget.Path)
            $sourcePrefix = $sourceAbsolute + [IO.Path]::DirectorySeparatorChar
            if (-not $cleanupAbsolute.StartsWith($sourcePrefix, [StringComparison]::OrdinalIgnoreCase)) { throw "PUBLISHER_SOURCE_TABLE_INVALID: cleanup target outside owned Source" }
            if ($cleanupTarget.Directory) {
                $scratchParent = Join-Path $sourceAbsolute ".scratch"
                if (-not [string]::Equals([IO.Path]::GetDirectoryName($cleanupAbsolute), $scratchParent, [StringComparison]::OrdinalIgnoreCase) -or
                    [IO.Path]::GetFileName($cleanupAbsolute) -cnotmatch ('^windows-versioninfo-' + $PID + '-[0-9a-f]{32}$')) { throw "PUBLISHER_SOURCE_TABLE_INVALID: cleanup directory is not original resource scratch" }
            } else {
                $knownResource = $false
                foreach ($artifact in $productArtifacts) {
                    $expectedResource = [IO.Path]::GetFullPath((Join-Path (Join-Path $sourceAbsolute ($artifact.source -replace '^\./', '')) "zz_product_versioninfo.syso"))
                    if ([string]::Equals($cleanupAbsolute, $expectedResource, [StringComparison]::OrdinalIgnoreCase)) { $knownResource = $true }
                }
                if (-not $knownResource) { throw "PUBLISHER_SOURCE_TABLE_INVALID: unregistered resource cleanup target" }
            }
            # Inspect every existing absolute path component before any removal;
            # a missing component is absence, an inspection error is refusal.
            $componentPath = [IO.Path]::GetPathRoot($cleanupAbsolute)
            $componentTail = $cleanupAbsolute.Substring($componentPath.Length)
            $components = @($componentTail.Split([IO.Path]::DirectorySeparatorChar, [StringSplitOptions]::RemoveEmptyEntries))
            $inspectPaths = @($componentPath)
            foreach ($component in $components) { $componentPath = Join-Path $componentPath $component; $inspectPaths += $componentPath }
            foreach ($inspectPath in $inspectPaths) {
                if (-not (Test-Path -LiteralPath $inspectPath -ErrorAction Stop)) { break }
                $entry = Get-Item -LiteralPath $inspectPath -Force -ErrorAction Stop
                if ($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "PUBLISHER_SOURCE_TABLE_INVALID: linked cleanup path" }
            }
            if (Test-Path -LiteralPath $cleanupAbsolute -ErrorAction Stop) {
                $entry = Get-Item -LiteralPath $cleanupAbsolute -Force -ErrorAction Stop
                if ([bool]$entry.PSIsContainer -ne [bool]$cleanupTarget.Directory) { throw "PUBLISHER_SOURCE_TABLE_INVALID: cleanup target type changed" }
                if ($cleanupTarget.Directory) {
                    # The existing generator writes no nested directories here.
                    # Refuse unknown leaves rather than deleting foreign data.
                    foreach ($child in @(Get-ChildItem -LiteralPath $cleanupAbsolute -Force -ErrorAction Stop)) {
                        if ($child.Attributes -band [IO.FileAttributes]::ReparsePoint -or $child.PSIsContainer) { throw "PUBLISHER_SOURCE_TABLE_INVALID: unknown or linked scratch descendant" }
                        $knownJSON = $false
                        foreach ($artifact in $productArtifacts) {
                            if ([string]::Equals($child.FullName, (Join-Path $cleanupAbsolute "$($artifact.role).json"), [StringComparison]::OrdinalIgnoreCase)) { $knownJSON = $true }
                        }
                        if (-not $knownJSON) { throw "PUBLISHER_SOURCE_TABLE_INVALID: unowned scratch leaf" }
                    }
                    Remove-Item -LiteralPath $cleanupAbsolute -Recurse -Force -ErrorAction Stop
                } else { Remove-Item -LiteralPath $cleanupAbsolute -Force -ErrorAction Stop }
            }
        } catch { $builderCleanupErrors.Add($_.Exception) }
        # Receive residue independently even when the one removal attempt fails.
        if ($null -ne $cleanupAbsolute) {
            try {
                if (Test-Path -LiteralPath $cleanupAbsolute -ErrorAction Stop) { throw "PUBLISHER_SOURCE_TABLE_INVALID: owned generated output remains" }
            } catch { $builderCleanupErrors.Add($_.Exception) }
        }
    }
    if ($builderCleanupErrors.Count -gt 0) {
        $builderAllErrors = [Collections.Generic.List[Exception]]::new()
        if ($null -ne $builderPrimaryError) { $builderAllErrors.Add($builderPrimaryError.Exception) }
        if ($null -ne $builderPrimaryExitCode) { $builderAllErrors.Add([Exception]::new("Marked builder native exit $builderPrimaryExitStage=$builderPrimaryExitCode")) }
        foreach ($cleanupError in $builderCleanupErrors) { $builderAllErrors.Add($cleanupError) }
        throw [AggregateException]::new("PUBLISHER_SOURCE_TABLE_INVALID: generated cleanup incomplete", [Exception[]]$builderAllErrors)
    }
}

if ($null -ne $publisherSource) {
    $afterSource = Get-VerifiedPublisherSourceInput
    if ($afterSource.source_epoch -cne $publisherSource.source_epoch -or
        $afterSource.source_snapshot_sha256 -cne $publisherSource.source_snapshot_sha256 -or
        [int]$afterSource.file_count -ne [int]$publisherSource.file_count) { throw "PUBLISHER_SOURCE_EPOCH_MISMATCH: postcompile Source" }
    Write-Host ("publisherSource=" + ($plan.publisherSource | ConvertTo-Json -Compress))
}

$candidate = $productArtifacts[0].output -replace '/', [IO.Path]::DirectorySeparatorChar
$validatorArgs = @($admissionCommand.argv)
$validatorOutput = @(& go @validatorArgs)
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
if ($validatorOutput.Count -ne 1 -or [string]::IsNullOrWhiteSpace([string]$validatorOutput[0])) {
    Write-Error "windows-product-validate must emit exactly one nonempty JSON line"
    exit 1
}
$identityLine = [string]$validatorOutput[0]
try {
    $identity = $identityLine | ConvertFrom-Json
} catch {
    Write-Error "windows-product-validate emitted invalid JSON: $($_.Exception.Message)"
    exit 1
}
$identityBuildDate = if ($identity.build_date -is [datetime]) {
    $identity.build_date.ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
} else {
    [string]$identity.build_date
}
$identityChecks = @(
    @("schema", [string]$identity.schema, $identityManifest.schema),
    @("role", [string]$identity.role, "product"),
    @("filename", [string]$identity.filename, $productArtifacts[0].filename),
    @("subsystem", [int]$identity.subsystem, 2),
    @("machine", [int]$identity.machine, $expectedMachine),
    @("version", [string]$identity.version, $version),
    @("commit", [string]$identity.commit, $commit),
    @("build_date", $identityBuildDate, $buildDate)
)
foreach ($check in $identityChecks) {
    if ($check[1] -ne $check[2]) {
        Write-Error "windows product identity $($check[0]) mismatch: actual=$($check[1]) expected=$($check[2])"
        exit 1
    }
}
if ([string]$identity.sha256 -cnotmatch '^[0-9a-f]{64}$') {
    Write-Error "windows product identity sha256 must be lowercase 64-hex"
    exit 1
}
$candidateHash = (Get-FileHash -LiteralPath $candidate -Algorithm SHA256).Hash.ToLowerInvariant()
if ([string]$identity.sha256 -cne $candidateHash) {
    Write-Error "windows product identity SHA-256 does not match the built product"
    exit 1
}
[IO.File]::WriteAllText(
    $identityManifestPath,
    $identityLine + [char]10,
    (New-Object Text.UTF8Encoding($false)))

foreach ($artifact in $artifacts) {
    $outFile = Join-Path $outDir $artifact.filename
    if (-not (Test-Path -LiteralPath $outFile -PathType Leaf)) {
        Write-Error "$outFile missing after build — check Defender exclusions (see INSTALL.md)."
        exit 1
    }
}
if (-not (Test-Path -LiteralPath $identityManifestPath -PathType Leaf)) {
    Write-Error "$identityManifestPath missing after product admission."
    exit 1
}
$mcpHubExecutables = @(Get-ChildItem -LiteralPath $outDir -File -Filter "mcphub*.exe")
if ($mcpHubExecutables.Count -ne 1 -or
    $mcpHubExecutables[0].Name -cne $productArtifacts[0].filename) {
    Write-Error "Windows build output must contain exactly one MCPHub executable: $($productArtifacts[0].filename)"
    exit 1
}

if ([System.Environment]::OSVersion.Platform -eq [System.PlatformID]::Win32NT) {
    Write-Host "==> Product VERSIONINFO resources:"
    foreach ($artifact in $productArtifacts) {
        (Get-Item (Join-Path $outDir $artifact.filename)).VersionInfo | Format-List ProductVersion,PrivateBuild,SpecialBuild,InternalName,OriginalFilename
    }
}
Write-Host "==> Done. Single-product admission passed; identity manifest=$identityManifestPath version=$version commit=$commit buildDate=$buildDate."

#Requires -Version 7.5
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$ConfigPath,
    [Parameter(Mandatory=$true)][ValidatePattern('^[0-9a-f]{64}$')][string]$ConfigSHA256,
    [Parameter(Mandatory=$true)][ValidateSet('marked','native')][string]$Role
)
$ErrorActionPreference = 'Stop'

function Get-SuccessorHash([string]$Path) {
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}
function Require-SuccessorOrdinary([string]$Path,[bool]$Directory) {
    $item=Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if ([bool]$item.PSIsContainer -ne $Directory) { throw 'Input path kind differs' }
    while ($null -ne $item) {
        if (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw 'Linked input/output ancestor refused' }
        if ($item -is [IO.FileInfo]) { $item=$item.Directory } else { $item=$item.Parent }
    }
}
function Require-SuccessorPin([object]$Pair) {
    if ($Pair.path -isnot [string] -or $Pair.sha256 -cnotmatch '^[0-9a-f]{64}$') { throw 'Explicit input pin required' }
    Require-SuccessorOrdinary $Pair.path $false
    if ((Get-SuccessorHash $Pair.path) -cne $Pair.sha256) { throw 'Explicit input bytes differ' }
    return [string]$Pair.path
}
function Get-SuccessorFullPath([string]$Path) {
    if (-not [IO.Path]::IsPathFullyQualified($Path)) { throw 'Absolute role path required' }
    return [IO.Path]::GetFullPath($Path).TrimEnd([IO.Path]::DirectorySeparatorChar,[IO.Path]::AltDirectorySeparatorChar)
}
function Require-SuccessorBelow([string]$Path,[string]$Root) {
    $full=Get-SuccessorFullPath $Path
    $prefix=(Get-SuccessorFullPath $Root)+[IO.Path]::DirectorySeparatorChar
    if (-not $full.StartsWith($prefix,[StringComparison]::OrdinalIgnoreCase)) { throw 'Output outside declared role root' }
}
function Write-SuccessorJson([string]$Path,[object]$Value) {
    $bytes=[Text.UTF8Encoding]::new($false).GetBytes(($Value | ConvertTo-Json -Depth 40)+[char]10)
    $stream=[IO.File]::Open($Path,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
    try { $stream.Write($bytes,0,$bytes.Length); $stream.Flush($true) } finally { $stream.Dispose() }
}
function Get-SuccessorArtifact([string]$Path) {
    Require-SuccessorOrdinary $Path $false
    return [ordered]@{path=$Path;sha256=(Get-SuccessorHash $Path)}
}
function Require-SuccessorFiles([string]$Root,[object]$Map) {
    Require-SuccessorOrdinary $Root $true
    foreach ($relative in $Map.files.Keys) {
        if ($relative -isnot [string] -or $relative.Contains('\') -or
            [IO.Path]::IsPathRooted($relative) -or
            @($relative.Split('/')) -contains '..' -or @($relative.Split('/')) -contains '.' -or
            $relative.Contains('//') -or $Map.files[$relative] -cnotmatch '^[0-9a-f]{64}$') {
            throw 'Unclean tracked source path/hash'
        }
        $path=Join-Path $Root $relative
        Require-SuccessorOrdinary $path $false
        if ((Get-SuccessorHash $path) -cne $Map.files[$relative]) { throw "Tracked source bytes differ: $relative" }
    }
}
function Require-SuccessorGit([string]$Root,[string]$Git,[string]$Commit,[string]$Tree) {
    $head=@(& $Git -C $Root rev-parse HEAD)
    if ($LASTEXITCODE -ne 0 -or $head.Count -ne 1 -or $head[0] -cne $Commit) { throw 'Actual Git source HEAD differs' }
    $actualTree=@(& $Git -C $Root rev-parse 'HEAD^{tree}')
    if ($LASTEXITCODE -ne 0 -or $actualTree.Count -ne 1 -or $actualTree[0] -cne $Tree) { throw 'Actual Git source tree differs' }
    $status=@(& $Git -C $Root status --porcelain=v1 --untracked-files=normal)
    if ($LASTEXITCODE -ne 0 -or $status.Count -ne 0) { throw 'Actual Git source is not clean' }
}

$null=Require-SuccessorPin ([ordered]@{path=$ConfigPath;sha256=$ConfigSHA256})
$successorCfg=[IO.File]::ReadAllText($ConfigPath) | ConvertFrom-Json -AsHashtable -DateKind String
if ($successorCfg.schema -cne 'successor-product-build-config-v1' -or
    $successorCfg.source_commit -cnotmatch '^[0-9a-f]{40}$' -or
    $successorCfg.source_tree -cnotmatch '^[0-9a-f]{40}$' -or
    $successorCfg.source_file_count -ne 3369 -or
    $successorCfg.release_version -cne '0.4.65' -or
    $successorCfg.release_build_date -isnot [string] -or
    $successorCfg.release_build_date -cnotmatch '^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$' -or
    $successorCfg.windows_target_arch -cne 'amd64') { throw 'Reviewed concrete successor build binding required' }
$successorMapPath=Require-SuccessorPin $successorCfg.source_map
$successorSnapshotPath=Require-SuccessorPin $successorCfg.source_snapshot
$successorMap=[IO.File]::ReadAllText($successorMapPath) | ConvertFrom-Json -AsHashtable -DateKind String
$successorSnapshot=[IO.File]::ReadAllText($successorSnapshotPath) | ConvertFrom-Json -AsHashtable -DateKind String
if ($successorMap.commit -cne $successorCfg.source_commit -or $successorMap.tree -cne $successorCfg.source_tree -or
    $successorMap.source_root -cne $successorCfg.source_root -or
    $successorMap.tracked_files -ne 3369 -or $successorMap.files.Count -ne 3369 -or
    $successorSnapshot.schema -cne 'reused-graphify-source-snapshot-v1' -or $successorSnapshot.files.Count -ne 3369) {
    throw 'Exact current tracked source map and publisher snapshot required'
}
foreach ($successorRelative in $successorMap.files.Keys) {
    if (-not $successorSnapshot.files.Contains($successorRelative) -or
        $successorSnapshot.files[$successorRelative].sha256 -cne $successorMap.files[$successorRelative]) {
        throw 'Publisher snapshot differs from exact current source map'
    }
}
$successorGo=Require-SuccessorPin $successorCfg.tools.go
$successorGit=Require-SuccessorPin $successorCfg.tools.git
foreach ($successorTool in @(@('go',$successorGo),@('git',$successorGit))) {
    # A typed application listing can contain multiple PATH hits; resolve the actual command.
    $resolvedCommands=@(Get-Command -Name $successorTool[0] -ErrorAction Stop)
    if ($resolvedCommands.Count -ne 1 -or
        $resolvedCommands[0].CommandType -ne [Management.Automation.CommandTypes]::Application) {
        throw 'Builder command must resolve to one application'
    }
    $resolved=$resolvedCommands[0].Source
    if (-not [string]::Equals((Get-SuccessorFullPath $resolved),(Get-SuccessorFullPath $successorTool[1]),[StringComparison]::OrdinalIgnoreCase)) {
        throw 'Builder PATH does not resolve the pinned tool'
    }
}
$successorSource=Get-SuccessorFullPath $successorCfg.source_root
$successorExport=Get-SuccessorFullPath $successorCfg.export_root
$successorOutput=Get-SuccessorFullPath $successorCfg.output_root
Require-SuccessorOrdinary $successorOutput $true
Require-SuccessorBelow $successorExport $successorOutput
Require-SuccessorBelow $successorCfg.marked_output $successorOutput
Require-SuccessorBelow $successorCfg.marked_receipt_root $successorOutput
Require-SuccessorBelow $successorCfg.native_receipt_root $successorOutput
$successorNativeParent=Join-Path (Join-Path $successorSource '.scratch') 'qualified-successor-native-product'
if (-not [string]::Equals((Get-SuccessorFullPath $successorCfg.native_output),
    (Get-SuccessorFullPath (Join-Path $successorNativeParent 'bundle')),[StringComparison]::OrdinalIgnoreCase)) {
    throw 'Native Product output must use the declared fresh source scratch bundle'
}
$successorBuildRoot=if ($Role -ceq 'marked') { $successorExport } else { $successorSource }
$successorBundle=Get-SuccessorFullPath $successorCfg[$Role+'_output']
$successorReceipts=Get-SuccessorFullPath $successorCfg[$Role+'_receipt_root']
foreach ($successorNewPath in @($successorBundle,$successorReceipts)) {
    if (Test-Path -LiteralPath $successorNewPath) { throw 'Role output already exists' }
    Require-SuccessorOrdinary ([IO.Path]::GetDirectoryName($successorNewPath)) $true
}
if ($Role -ceq 'marked') {
    if (Test-Path -LiteralPath (Join-Path $successorExport '.git')) { throw 'Marked publisher requires the independent plain tracked export' }
    $successorExportPrefix=$successorExport+[IO.Path]::DirectorySeparatorChar
    if ($successorBundle.StartsWith($successorExportPrefix,[StringComparison]::OrdinalIgnoreCase) -or
        [string]::Equals($successorBundle,$successorExport,[StringComparison]::OrdinalIgnoreCase)) { throw 'Marked output must be outside Source export' }
}
$successorGenerated=Join-Path $successorBuildRoot 'cmd/mcphub/zz_product_versioninfo.syso'
if (Test-Path -LiteralPath $successorGenerated) { throw 'Pre-existing generated Product resource refused' }
Require-SuccessorFiles $successorBuildRoot $successorMap
if ($Role -ceq 'native') { Require-SuccessorGit $successorSource $successorGit $successorCfg.source_commit $successorCfg.source_tree }
$successorBuildScript=Join-Path $successorBuildRoot 'build.ps1'
if ($successorCfg.build_script_sha256 -cne $successorMap.files['build.ps1'] -or
    (Get-SuccessorHash $successorBuildScript) -cne $successorCfg.build_script_sha256) { throw 'Reviewed source builder differs' }
New-Item -ItemType Directory -Path $successorReceipts -ErrorAction Stop | Out-Null
$successorBuilderArguments=@{OutputDirectory=$successorBundle}
if ($Role -ceq 'marked') {
    $successorBuilderArguments.WindowsTargetArch=$successorCfg.windows_target_arch
    $successorBuilderArguments.ReleaseVersion=$successorCfg.release_version
    $successorBuilderArguments.ReleaseCommit=$successorCfg.source_commit
    $successorBuilderArguments.ReleaseBuildDate=$successorCfg.release_build_date
    $successorBuilderArguments.SourceSnapshotPath=$successorSnapshotPath
} else {
    $successorBuilderArguments.TestStateIsolatedProduct=$true
}
# Dot-sourcing retains the actual post-build plan and resource configuration.
# No PrintPlanJson preview, replacement builder, private test image, or epoch override is used.
Push-Location -LiteralPath $successorBuildRoot
try {
    . $successorBuildScript @successorBuilderArguments | Out-Host
    $successorActualPlan=$plan
    $successorActualConfig=$config
    $successorEntryObservation=$entrySetResult
    if ($null -eq $successorActualPlan -or $null -eq $successorActualConfig -or
        $successorActualPlan.schemaVersion -ne 3 -or $successorActualPlan.commit -cne $successorCfg.source_commit -or
        $successorActualPlan.version -cne $successorCfg.release_version -or
        $successorActualPlan.expectedMachine -ne 0x8664 -or $successorActualPlan.canonicalEntry.received -cne $true -or
        $successorActualPlan.canonicalEntry.expectedCanonicalEntrySHA256 -cnotmatch '^[0-9a-f]{64}$' -or
        $successorEntryObservation.schema -cne 'windows-canonical-entry-build-observation-v1') { throw 'Actual completed builder result is unavailable or differs' }
    if ($Role -ceq 'marked') {
        if ($successorActualPlan.testOnly -cne $false -or @($successorActualPlan.buildTags).Count -ne 0 -or
            $successorActualPlan.buildDate -cne $successorCfg.release_build_date -or
            $successorActualPlan.publisherSource.file_count -ne 3369 -or
            $successorActualPlan.publisherSource.source_snapshot_sha256 -cne $successorCfg.source_snapshot.sha256 -or
            $successorActualPlan.publisherSource.source_epoch -cnotmatch '^[0-9a-f]{64}$') { throw 'Marked publisher plan/source identity differs' }
    } elseif ($successorActualPlan.testOnly -cne $true -or
        (@($successorActualPlan.buildTags) -join ',') -cne 'test_state_path_env' -or
        $successorActualPlan.Contains('publisherSource')) { throw 'Native test Product must have no publisher Source epoch' }
    $successorPlanPath=Join-Path $successorReceipts 'actual-build-plan.json'
    $successorConfigPath=Join-Path $successorReceipts 'actual-versioninfo.json'
    Write-SuccessorJson $successorPlanPath $successorActualPlan
    Write-SuccessorJson $successorConfigPath $successorActualConfig
    Write-SuccessorJson (Join-Path $successorReceipts 'actual-entry-observation.json') $successorEntryObservation
    $successorResource=$null
    if ($Role -ceq 'marked') {
        # Re-create the incoming CLI resource from the actual captured Product
        # configuration, including that build's canonical launcher SHA and comments.
        $successorResourcePath=Join-Path $successorReceipts 'captured-product-versioninfo.syso'
        $successorResourceArgs=@('run','github.com/josephspurrier/goversioninfo/cmd/goversioninfo@v1.5.0')+
            @($successorActualPlan.resourceGeneratorArgs)+@('-o',$successorResourcePath,$successorConfigPath)
        & $successorGo @successorResourceArgs
        if ($LASTEXITCODE -ne 0) { throw 'Actual Product resource regeneration failed' }
        $successorResource=Get-SuccessorArtifact $successorResourcePath
    }
} finally { Pop-Location }

if (Test-Path -LiteralPath $successorGenerated) { throw 'Builder generated resource remains in tracked source tree' }
Require-SuccessorFiles $successorBuildRoot $successorMap
if ($Role -ceq 'native') { Require-SuccessorGit $successorSource $successorGit $successorCfg.source_commit $successorCfg.source_tree }
$null=Require-SuccessorPin ([ordered]@{path=$ConfigPath;sha256=$ConfigSHA256})
$null=Require-SuccessorPin $successorCfg.source_map
$null=Require-SuccessorPin $successorCfg.source_snapshot
$successorExpectedFiles=@('mcphub-product/mcphub.exe','mcphub.exe','windows-product-identity.json')
$successorActualFiles=@(Get-ChildItem -LiteralPath $successorBundle -File -Recurse | ForEach-Object {
    [IO.Path]::GetRelativePath($successorBundle,$_.FullName).Replace('\','/')
})
[Array]::Sort($successorActualFiles,[StringComparer]::Ordinal)
if (($successorActualFiles -join '|') -cne ($successorExpectedFiles -join '|')) { throw 'Actual Product bundle does not contain exactly the three V1 files' }
$successorFiles=[ordered]@{}
foreach ($successorRelative in $successorExpectedFiles) {
    $successorFile=Join-Path $successorBundle $successorRelative
    Require-SuccessorOrdinary $successorFile $false
    $successorFiles[$successorRelative]=[ordered]@{bytes=(Get-Item -LiteralPath $successorFile).Length;sha256=(Get-SuccessorHash $successorFile)}
}
if ($successorFiles['mcphub.exe'].sha256 -cne $successorActualPlan.canonicalEntry.expectedCanonicalEntrySHA256 -or
    $successorFiles['mcphub-product/mcphub.exe'].sha256 -cne $successorEntryObservation.product_sha256 -or
    $successorFiles['windows-product-identity.json'].sha256 -cne $successorEntryObservation.identity_sha256) { throw 'Actual admitted Product bundle bytes changed' }
$successorEpoch=if ($Role -ceq 'marked') { [string]$successorActualPlan.publisherSource.source_epoch } else { '' }
if ([string]$successorEntryObservation.source_epoch -cne $successorEpoch -or
    $successorEntryObservation.build_id -cne $successorActualPlan.entrySetBuildID) { throw 'Actual entry observation epoch/build identity differs' }
$successorFixture=[ordered]@{
    product_root=$successorBundle;source_epoch=$successorEpoch;product_version=$successorActualPlan.version
    build_id=$successorActualPlan.entrySetBuildID;reverse_projection='../mcphub.exe'
    canonical_entry_sha256=$successorFiles['mcphub.exe'].sha256
    product_sha256=$successorFiles['mcphub-product/mcphub.exe'].sha256
    identity_sha256=$successorFiles['windows-product-identity.json'].sha256
}
$successorFixturePath=Join-Path $successorReceipts 'product-fixture.json'
Write-SuccessorJson $successorFixturePath $successorFixture
$successorInputs=[ordered]@{
    schema='current-source-product-receiver-inputs-v1';source_commit=$successorCfg.source_commit
    build_plan=$successorActualPlan;build_plan_file=(Get-SuccessorArtifact $successorPlanPath)
    actual_product=[ordered]@{path=$successorBundle;files=$successorFiles;fixture=$successorFixturePath}
}
if ($Role -ceq 'marked') {
    # These are additional flags for the future incoming CLI receiver, not a
    # statement that the original Product compiler used these flags.
    $successorInputs.receiver_linker_flags=@('-s','-w')
    $successorInputs.publisher_source=$successorActualPlan.publisherSource
    $successorInputs.resource=$successorResource
}
Write-SuccessorJson (Join-Path $successorReceipts 'product-inputs.json') $successorInputs
Write-SuccessorJson (Join-Path $successorReceipts 'preparation-scope.json') ([ordered]@{
    schema='successor-product-preparation-scope-v1';role=$Role;source_commit=$successorCfg.source_commit
    source_tree=$successorCfg.source_tree;source_files_checked=3369;tracked_source_unchanged=$true
    compiler_inputs_only=$true;tests_executed=$false;runtime_acceptance_granted=$false
    config=(Get-SuccessorArtifact $ConfigPath);source_map=(Get-SuccessorArtifact $successorMapPath)
    source_snapshot=(Get-SuccessorArtifact $successorSnapshotPath)
    fixture=(Get-SuccessorArtifact $successorFixturePath)
})
Write-Host "Completed current-source $Role Product input preparation; no tests were executed."

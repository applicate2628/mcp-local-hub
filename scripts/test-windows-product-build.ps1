#Requires -Version 5.1

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$buildScript = Join-Path $repoRoot "build.ps1"
$scratchRoot = Join-Path $repoRoot ".scratch"
$fixtureRoot = Join-Path $scratchRoot "windows-build-plan-$PID-$([Guid]::NewGuid().ToString('N'))"
$fakeTools = Join-Path $fixtureRoot "fake-tools"
$pwsh = (Get-Command pwsh -ErrorAction Stop).Source

New-Item -ItemType Directory -Force -Path $fakeTools | Out-Null
Copy-Item -LiteralPath $buildScript -Destination (Join-Path $fixtureRoot "build.ps1")
$contract = Join-Path $repoRoot "windows-product-artifacts.json"
if (Test-Path -LiteralPath $contract) {
    Copy-Item -LiteralPath $contract -Destination (Join-Path $fixtureRoot "windows-product-artifacts.json")
}
[IO.File]::WriteAllText((Join-Path $fakeTools "git.cmd"), "@echo deadbeef`r`n@exit /b 0`r`n")
[IO.File]::WriteAllText((Join-Path $fakeTools "go.cmd"), "@echo unexpected go invocation 1^>^&2`r`n@exit /b 93`r`n")

$priorPath = $env:PATH
try {
    $env:PATH = $fakeTools
    Push-Location $fixtureRoot
    try {
        $json = & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") -PrintPlanJson
        if ($LASTEXITCODE -ne 0) {
            throw "build.ps1 -PrintPlanJson failed with exit $LASTEXITCODE"
        }
    } finally {
        Pop-Location
    }
} finally {
    $env:PATH = $priorPath
    Remove-Item -LiteralPath $fixtureRoot -Recurse -Force -ErrorAction SilentlyContinue
}

$plan = $json | ConvertFrom-Json

if ($plan.schemaVersion -ne 1) { throw "schemaVersion=$($plan.schemaVersion), want 1" }
if ($plan.artifacts.Count -ne 3) { throw "artifact count=$($plan.artifacts.Count), want 3" }

$expected = @(
    [pscustomobject]@{ role = "cli"; filename = "mcphub.exe"; source = "./cmd/mcphub"; subsystem = 3; productMetadata = $true; npmPayload = $true; npmBin = $true },
    [pscustomobject]@{ role = "windowless"; filename = "mcphub-windowless.exe"; source = "./cmd/mcphub-windowless"; subsystem = 2; productMetadata = $true; npmPayload = $true; npmBin = $false },
    [pscustomobject]@{ role = "admission-helper"; filename = "mcphub-pe-admit.exe"; source = "./cmd/mcphub-pe-admit"; subsystem = 3; productMetadata = $false; npmPayload = $true; npmBin = $false }
)

for ($i = 0; $i -lt $expected.Count; $i++) {
    $actual = $plan.artifacts[$i]
    $want = $expected[$i]
    foreach ($field in @("role", "filename", "source", "subsystem", "productMetadata", "npmPayload", "npmBin")) {
        if ($actual.$field -ne $want.$field) {
            throw "artifact[$i].$field=$($actual.$field), want $($want.$field)"
        }
    }
}

$cli = $plan.artifacts[0]
$windowless = $plan.artifacts[1]
$helper = $plan.artifacts[2]
if ($cli.ldflags -match 'windowsgui') { throw "cli linker flags must not select WINDOWS_GUI: $($cli.ldflags)" }
if ($windowless.ldflags -notmatch '(?:^| )-H windowsgui(?: |$)') { throw "windowless linker flags missing -H windowsgui: $($windowless.ldflags)" }
if ($helper.ldflags -match 'windowsgui') { throw "admission helper must remain CUI: $($helper.ldflags)" }

foreach ($metadata in @("version", "commit", "buildDate")) {
    if ($cli.ldflags -notmatch "-X main\.$metadata=") {
        throw "CLI linker flags do not embed ${metadata}: $($cli.ldflags)"
    }
}
if ($windowless.ldflags -match '-X main\.(version|commit|buildDate)=') {
    throw "windowless adapter must derive identity from VERSIONINFO, not nonexistent runtime symbols: $($windowless.ldflags)"
}
if ($helper.ldflags -match '-X main\.(version|commit|buildDate)=') {
    throw "admission helper must not claim product metadata: $($helper.ldflags)"
}

$wantVersionInfo = @{
    cli = @{ InternalName = "mcphub-cli"; OriginalFilename = "mcphub.exe"; SpecialBuild = "mcphub-role-v1;role=cli" }
    windowless = @{ InternalName = "mcphub-windowless"; OriginalFilename = "mcphub-windowless.exe"; SpecialBuild = "mcphub-role-v1;role=windowless" }
}
$planBuildDate = if ($plan.buildDate -is [datetime]) { $plan.buildDate.ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ") } else { [string]$plan.buildDate }
foreach ($artifact in @($cli, $windowless)) {
    $identity = $artifact.versionInfo
    if ($identity.ProductName -ne "mcp-local-hub" -or $identity.ProductVersion -ne $plan.version) {
        throw "$($artifact.role) VERSIONINFO product identity drift"
    }
    if ($identity.PrivateBuild -ne "mcphub-build-v1;commit=$($plan.commit);build_date=$planBuildDate") {
        throw "$($artifact.role) VERSIONINFO PrivateBuild drift: $($identity.PrivateBuild)"
    }
    foreach ($field in @("InternalName", "OriginalFilename", "SpecialBuild")) {
        if ($identity.$field -ne $wantVersionInfo[$artifact.role][$field]) {
            throw "$($artifact.role) VERSIONINFO ${field}=$($identity.$field), want $($wantVersionInfo[$artifact.role][$field])"
        }
    }
}
if ($null -ne $helper.versionInfo) { throw "admission helper must not carry product VERSIONINFO identity" }

if ($plan.admissionCommands.Count -ne 2) { throw "admission command count=$($plan.admissionCommands.Count), want 2" }
if (($plan.admissionCommands[0].argv -join "|") -ne "cli|bin/mcphub.exe") {
    throw "cli admission argv drift: $($plan.admissionCommands[0].argv -join '|')"
}
if (($plan.admissionCommands[1].argv -join "|") -ne "windowless|bin/mcphub-windowless.exe") {
    throw "windowless admission argv drift: $($plan.admissionCommands[1].argv -join '|')"
}

function Get-PESubsystem([string]$Path) {
    $bytes = [IO.File]::ReadAllBytes($Path)
    $peOffset = [BitConverter]::ToInt32($bytes, 0x3c)
    return [BitConverter]::ToUInt16($bytes, $peOffset + 24 + 68)
}

$compileRoot = Join-Path $scratchRoot "windows-linker-probe-$PID-$([Guid]::NewGuid().ToString('N'))"
try {
    $source = "package main`nfunc main() {}`n"
    foreach ($name in @("a", "b")) {
        $dir = Join-Path $compileRoot $name
        New-Item -ItemType Directory -Force -Path $dir | Out-Null
        [IO.File]::WriteAllText((Join-Path $dir "main.go"), $source)
        & go build -trimpath -buildvcs=false -o (Join-Path $dir "cui.exe") (Join-Path $dir "main.go")
        if ($LASTEXITCODE -ne 0) { throw "CUI fixture build $name failed with exit $LASTEXITCODE" }
        & go build -trimpath -buildvcs=false -ldflags "-H windowsgui" -o (Join-Path $dir "gui.exe") (Join-Path $dir "main.go")
        if ($LASTEXITCODE -ne 0) { throw "GUI fixture build $name failed with exit $LASTEXITCODE" }
    }

    $cuiA = Join-Path $compileRoot "a\cui.exe"
    $cuiB = Join-Path $compileRoot "b\cui.exe"
    $guiA = Join-Path $compileRoot "a\gui.exe"
    $guiB = Join-Path $compileRoot "b\gui.exe"
    if ((Get-PESubsystem $cuiA) -ne 3) { throw "default Go linker output is not PE subsystem 3" }
    if ((Get-PESubsystem $guiA) -ne 2) { throw "-H windowsgui output is not PE subsystem 2" }
    $cuiHashA = (Get-FileHash -Algorithm SHA256 -LiteralPath $cuiA).Hash
    $cuiHashB = (Get-FileHash -Algorithm SHA256 -LiteralPath $cuiB).Hash
    $guiHashA = (Get-FileHash -Algorithm SHA256 -LiteralPath $guiA).Hash
    $guiHashB = (Get-FileHash -Algorithm SHA256 -LiteralPath $guiB).Hash
    if ($cuiHashA -ne $cuiHashB) { throw "CUI fixture hashes differ across workspace paths" }
    if ($guiHashA -ne $guiHashB) { throw "GUI fixture hashes differ across workspace paths" }
    Write-Output "PASS: linker flag delta observed (default subsystem=3; -H windowsgui subsystem=2); two-path hashes CUI=$cuiHashA GUI=$guiHashA."
} finally {
    Remove-Item -LiteralPath $compileRoot -Recurse -Force -ErrorAction SilentlyContinue
}

Write-Output "PASS: Windows build plan has canonical CUI CLI, GUI windowless adapter, CUI admission helper, shared product metadata, and explicit role admission."

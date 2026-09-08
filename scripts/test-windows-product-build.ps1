#Requires -Version 5.1

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$buildScript = Join-Path $repoRoot "build.ps1"
$scratchRoot = Join-Path $repoRoot ".scratch"
$fixtureRoot = Join-Path $scratchRoot "windows-build-plan-$PID-$([Guid]::NewGuid().ToString('N'))"
$fakeTools = Join-Path $fixtureRoot "fake-tools"
$pwsh = (Get-Command pwsh -ErrorAction Stop).Source
$windowsPowerShell = (Get-Command powershell -ErrorAction Stop).Source
$realGo = (Get-Command go -ErrorAction Stop).Source
$releaseVersion = "1.2.3-beta.1"
$releaseCommit = "0123456789abcdef0123456789abcdef01234567"
$releaseBuildDate = "2026-09-07T06:07:08Z"

New-Item -ItemType Directory -Force -Path $fakeTools | Out-Null
Copy-Item -LiteralPath $buildScript -Destination (Join-Path $fixtureRoot "build.ps1")
$contract = Join-Path $repoRoot "windows-product-artifacts.json"
if (Test-Path -LiteralPath $contract) {
    Copy-Item -LiteralPath $contract -Destination (Join-Path $fixtureRoot "windows-product-artifacts.json")
}
[void](New-Item -ItemType Directory -Force -Path (Join-Path $fixtureRoot "cmd/mcphub"))
[void](New-Item -ItemType Directory -Force -Path (Join-Path $fixtureRoot "cmd/mcphub-windowless"))
Copy-Item -LiteralPath (Join-Path $repoRoot "cmd/mcphub/versioninfo.json") -Destination (Join-Path $fixtureRoot "cmd/mcphub/versioninfo.json")
Copy-Item -LiteralPath (Join-Path $repoRoot "cmd/mcphub/mcphub.ico") -Destination (Join-Path $fixtureRoot "cmd/mcphub/mcphub.ico")
[IO.File]::WriteAllText((Join-Path $fakeTools "git.cmd"), "@echo deadbeef`r`n@exit /b 0`r`n")
[IO.File]::WriteAllText((Join-Path $fakeTools "go.cmd"), '@if "%1"=="env" (@echo %MCPHUB_FAKE_GOARCH% & @exit /b 0) else (@echo partial-resource^>cmd\mcphub\zz_product_versioninfo.syso & @exit /b 93)' + "`r`n")

$priorPath = $env:PATH
try {
    $env:PATH = $fakeTools
    $priorFakeArch = $env:MCPHUB_FAKE_GOARCH
    $env:MCPHUB_FAKE_GOARCH = "amd64"
    Push-Location $fixtureRoot
    try {
        $json = & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") -PrintPlanJson
        if ($LASTEXITCODE -ne 0) {
            throw "build.ps1 -PrintPlanJson failed with exit $LASTEXITCODE"
        }
        $nativeArmJson = & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") -PrintPlanJson
        if ($LASTEXITCODE -ne 0) { throw "native amd64 plan failed" }
        $releasePlans = @{}
        foreach ($arch in @("amd64", "arm64")) {
            $releaseJson = & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") `
                -PrintPlanJson `
                -WindowsTargetArch $arch `
                -OutputDirectory "npm/packages/win32-$arch/bin" `
                -ReleaseVersion $releaseVersion `
                -ReleaseCommit $releaseCommit `
                -ReleaseBuildDate $releaseBuildDate
            if ($LASTEXITCODE -ne 0) {
                throw "build.ps1 release plan for $arch failed with exit $LASTEXITCODE"
            }
            $releasePlans[$arch] = $releaseJson | ConvertFrom-Json
        }
        $env:MCPHUB_FAKE_GOARCH = "arm64"
        $nativeArmJson = & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") -PrintPlanJson
        if ($LASTEXITCODE -ne 0) { throw "native arm64 plan failed" }
        $nativeArmPlan = $nativeArmJson | ConvertFrom-Json
        if ($nativeArmPlan.windowsTargetArch -ne "" -or ($nativeArmPlan.resourceGeneratorArgs -join "|") -ne "-64|-arm") {
            throw "omitted-target arm64 plan=$($nativeArmPlan.resourceGeneratorArgs -join '|')"
        }
        $env:MCPHUB_FAKE_GOARCH = "amd64"
        & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") -PrintPlanJson -WindowsTargetArch amd64 *> $null
        if ($LASTEXITCODE -eq 0) { throw "release plan accepted a missing release tuple" }
        & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") `
            -PrintPlanJson `
            -WindowsTargetArch amd64 `
            -ReleaseVersion $releaseVersion `
            -ReleaseCommit deadbeef `
            -ReleaseBuildDate $releaseBuildDate *> $null
        if ($LASTEXITCODE -eq 0) { throw "release plan accepted a short commit" }
        & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") `
            -PrintPlanJson `
            -ReleaseVersion $releaseVersion `
            -ReleaseCommit $releaseCommit `
            -ReleaseBuildDate $releaseBuildDate *> $null
        if ($LASTEXITCODE -eq 0) { throw "release plan accepted a tuple without WindowsTargetArch" }
        & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") `
            -WindowsTargetArch amd64 `
            -OutputDirectory "out" `
            -ReleaseVersion $releaseVersion `
            -ReleaseCommit $releaseCommit `
            -ReleaseBuildDate $releaseBuildDate *> $null
        if ($LASTEXITCODE -ne 93) { throw "failing resource generator exited $LASTEXITCODE, want 93" }
        if (Test-Path -LiteralPath (Join-Path $fixtureRoot "cmd/mcphub/zz_product_versioninfo.syso")) {
            throw "failing resource generation left a partial syso"
        }
        if (@(Get-ChildItem -LiteralPath (Join-Path $fixtureRoot ".scratch") -Directory -Filter "windows-versioninfo-*" -ErrorAction SilentlyContinue).Count -ne 0) {
            throw "failing resource generation left its scratch directory"
        }
    } finally {
        Pop-Location
    }
} finally {
    $env:PATH = $priorPath
    $env:MCPHUB_FAKE_GOARCH = $priorFakeArch
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

foreach ($arch in @("amd64", "arm64")) {
    $releasePlan = $releasePlans[$arch]
    if ($releasePlan.windowsTargetArch -ne $arch) {
        throw "release target arch=$($releasePlan.windowsTargetArch), want $arch"
    }
    $wantGeneratorArgs = if ($arch -eq "arm64") { "-64|-arm" } else { "-64" }
    if (($releasePlan.resourceGeneratorArgs -join "|") -ne $wantGeneratorArgs) {
        throw "$arch resource generator args=$($releasePlan.resourceGeneratorArgs -join '|'), want $wantGeneratorArgs"
    }
    foreach ($artifact in @($releasePlan.artifacts | Where-Object productMetadata)) {
        if ($artifact.versionInfo.ProductVersion -ne $releaseVersion) {
            throw "$arch/$($artifact.role) release ProductVersion drift"
        }
        if ($artifact.versionInfo.PrivateBuild -ne "mcphub-build-v1;commit=$releaseCommit;build_date=$releaseBuildDate") {
            throw "$arch/$($artifact.role) release PrivateBuild drift: $($artifact.versionInfo.PrivateBuild)"
        }
    }
}

$publishWorkflow = Get-Content -Raw -LiteralPath (Join-Path $repoRoot ".github/workflows/npm-publish.yml")
if ($publishWorkflow -match [regex]::Escape("go generate ./cmd/mcphub")) {
    throw "npm publish workflow must not call the retired go:generate resource path"
}
if ($publishWorkflow -notmatch 'COMMIT="\$\(git rev-parse HEAD\)"') {
    throw "npm publish workflow must capture the full Windows release commit"
}
if ($publishWorkflow -notmatch 'SHORT_COMMIT="\$\(git rev-parse --short HEAD\)"' -or
    $publishWorkflow -notmatch 'main\.commit=\$SHORT_COMMIT') {
    throw "npm publish workflow must preserve short commit metadata for non-Windows payloads"
}
if ($publishWorkflow -notmatch '(?s)build\.ps1.+-WindowsTargetArch.+-ReleaseVersion.+-ReleaseCommit.+-ReleaseBuildDate') {
    throw "npm publish workflow must delegate Windows payload generation to build.ps1 with the captured tuple"
}
if ($publishWorkflow -notmatch '-ReleaseBuildDate "\$DATE" \|\| return \$\?') {
    throw "npm publish workflow must propagate build.ps1 failure from the build function"
}

$encodingProbe = Join-Path $scratchRoot "windows-build-json-encoding-$PID-$([Guid]::NewGuid().ToString('N'))"
try {
    New-Item -ItemType Directory -Force -Path $encodingProbe | Out-Null
    $decoder = Join-Path $encodingProbe "decode.go"
    [IO.File]::WriteAllText($decoder, 'package main; import ("encoding/json"; "os"); func main(){ var v interface{}; b,_:=os.ReadFile(os.Args[1]); if json.Unmarshal(b,&v)!=nil { os.Exit(1) } }', (New-Object Text.UTF8Encoding($false)))
    $noBOM = Join-Path $encodingProbe "resource.json"
    & $windowsPowerShell -NoProfile -Command "[IO.File]::WriteAllText('$noBOM', '{""probe"":true}', (New-Object Text.UTF8Encoding(`$false)))"
    & $realGo run $decoder $noBOM
    if ($LASTEXITCODE -ne 0) { throw "Windows PowerShell no-BOM JSON failed Go decoder" }
    $bom = Join-Path $encodingProbe "old-bom.json"
    [IO.File]::WriteAllText($bom, '{"probe":true}', (New-Object Text.UTF8Encoding($true)))
    & $realGo run $decoder $bom *> $null
    if ($LASTEXITCODE -eq 0) { throw "Go JSON decoder accepted old UTF8 BOM fixture" }
} finally {
    Remove-Item -LiteralPath $encodingProbe -Recurse -Force -ErrorAction SilentlyContinue
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

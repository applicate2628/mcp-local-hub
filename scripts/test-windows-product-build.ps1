#Requires -Version 5.1

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$buildScript = Join-Path $repoRoot "build.ps1"
$scratchRoot = Join-Path $repoRoot ".scratch"
$fixtureRoot = Join-Path $scratchRoot "windows-build-plan-$PID-$([Guid]::NewGuid().ToString('N'))"
$fakeTools = Join-Path $fixtureRoot "fake-tools"
$fakeBuildTools = Join-Path $fixtureRoot "fake-build-tools"
$fakeBuildLog = Join-Path $fixtureRoot "fake-build-go.jsonl"
$pwsh = (Get-Command pwsh -ErrorAction Stop).Source
$windowsPowerShell = (Get-Command powershell -ErrorAction Stop).Source
$realGo = (Get-Command go -ErrorAction Stop).Source
$system32 = Join-Path $env:SystemRoot "System32"
$releaseVersion = "1.2.3-beta.1"
$releaseCommit = "0123456789abcdef0123456789abcdef01234567"
$releaseBuildDate = "2026-09-07T06:07:08Z"
$fixtureCommit = "abcdef0123456789abcdef0123456789abcdef01"

New-Item -ItemType Directory -Force -Path $fakeTools,$fakeBuildTools | Out-Null
Copy-Item -LiteralPath $buildScript -Destination (Join-Path $fixtureRoot "build.ps1")
$contract = Join-Path $repoRoot "windows-product-artifacts.json"
if (Test-Path -LiteralPath $contract) {
    Copy-Item -LiteralPath $contract -Destination (Join-Path $fixtureRoot "windows-product-artifacts.json")
}
[void](New-Item -ItemType Directory -Force -Path (Join-Path $fixtureRoot "cmd/mcphub"))
Copy-Item -LiteralPath (Join-Path $repoRoot "cmd/mcphub/versioninfo.json") -Destination (Join-Path $fixtureRoot "cmd/mcphub/versioninfo.json")
Copy-Item -LiteralPath (Join-Path $repoRoot "cmd/mcphub/mcphub.ico") -Destination (Join-Path $fixtureRoot "cmd/mcphub/mcphub.ico")
$fakeGit = @"
@echo off
@if not "%~1"=="-C" exit /b 91
@if /I not "%~2"=="$fixtureRoot" exit /b 92
@if not "%~3"=="rev-parse" exit /b 93
@if not "%~4"=="HEAD" exit /b 94
@echo $fixtureCommit
@exit /b 0
"@
[IO.File]::WriteAllText((Join-Path $fakeTools "git.cmd"), $fakeGit)
[IO.File]::WriteAllText((Join-Path $fakeTools "go.cmd"), '@if "%1"=="env" (@echo %MCPHUB_FAKE_GOARCH% & @exit /b 0) else (@echo partial-resource^>cmd\mcphub\zz_product_versioninfo.syso & @exit /b 93)' + "`r`n")
Copy-Item -LiteralPath (Join-Path $fakeTools "git.cmd") -Destination (Join-Path $fakeBuildTools "git.cmd")
$fakeGoScript = @'
$CommandArgs = [string[]]$args
$record = [ordered]@{
    argv = @($CommandArgs)
    goflags = if (Test-Path Env:GOFLAGS) { [string]$env:GOFLAGS } else { $null }
}
[IO.File]::AppendAllText(
    $env:MCPHUB_FAKE_GO_LOG,
    (($record | ConvertTo-Json -Compress) + [char]10),
    (New-Object Text.UTF8Encoding($false)))
if ($CommandArgs.Count -ge 2 -and $CommandArgs[0] -eq "env" -and $CommandArgs[1] -eq "GOARCH") {
    Write-Output $env:MCPHUB_FAKE_GOARCH
    exit 0
}
if ($CommandArgs.Count -ge 2 -and $CommandArgs[0] -eq "run" -and
    $CommandArgs[1] -like "github.com/josephspurrier/goversioninfo/*") {
    $outputIndex = [Array]::IndexOf($CommandArgs, "-o")
    if ($outputIndex -lt 0 -or $outputIndex + 1 -ge $CommandArgs.Count) { exit 81 }
    [IO.File]::WriteAllText($CommandArgs[$outputIndex + 1], "fake-resource")
    exit 0
}
if ($CommandArgs.Count -ge 1 -and $CommandArgs[0] -eq "build") {
    $outputIndex = [Array]::IndexOf($CommandArgs, "-o")
    if ($outputIndex -lt 0 -or $outputIndex + 1 -ge $CommandArgs.Count) { exit 82 }
    [IO.File]::WriteAllText($CommandArgs[$outputIndex + 1], "fake-product")
    exit 0
}
if ($CommandArgs.Count -ge 2 -and $CommandArgs[0] -eq "run" -and
    $CommandArgs[1] -eq "./tools/windows-product-validate") {
    function Value-After([string]$Name) {
        $index = [Array]::IndexOf($CommandArgs, $Name)
        if ($index -lt 0 -or $index + 1 -ge $CommandArgs.Count) { exit 83 }
        return $CommandArgs[$index + 1]
    }
    $candidate = Value-After "--candidate"
    $identity = [ordered]@{
        schema = "windows-product-identity-v1"
        role = "product"
        filename = "mcphub.exe"
        subsystem = 2
        machine = 34404
        version = Value-After "--expected-version"
        commit = Value-After "--expected-commit"
        build_date = Value-After "--expected-build-date"
        sha256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $candidate).Hash.ToLowerInvariant()
    }
    Write-Output ($identity | ConvertTo-Json -Compress)
    exit 0
}
exit 84
'@
$fakeGoScriptPath = Join-Path $fakeBuildTools "fake-go.ps1"
[IO.File]::WriteAllText($fakeGoScriptPath, $fakeGoScript, (New-Object Text.UTF8Encoding($false)))
$fakeGoCommand = @"
@echo off
"$pwsh" -NoProfile -ExecutionPolicy Bypass -File "$fakeGoScriptPath" %*
@exit /b %ERRORLEVEL%
"@
[IO.File]::WriteAllText((Join-Path $fakeBuildTools "go.cmd"), $fakeGoCommand)

$priorPath = $env:PATH
try {
    $env:PATH = $fakeTools + [IO.Path]::PathSeparator + $system32
    $priorFakeArch = $env:MCPHUB_FAKE_GOARCH
    $env:MCPHUB_FAKE_GOARCH = "amd64"
    $outsideCwd = Join-Path $fixtureRoot "caller-cwd"
    New-Item -ItemType Directory -Force -Path $outsideCwd | Out-Null
    Push-Location $outsideCwd
    try {
        $outsideJson = & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") -PrintPlanJson
        if ($LASTEXITCODE -ne 0) { throw "outside-CWD build plan failed with exit $LASTEXITCODE" }
        $outsidePlan = $outsideJson | ConvertFrom-Json
        if ($outsidePlan.commit -ne $fixtureCommit) {
            throw "outside-CWD build plan commit=$($outsidePlan.commit), want script-root commit $fixtureCommit"
        }
    } finally {
        Pop-Location
    }
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
        $testOutput = Join-Path $fixtureRoot ".scratch/o7-product-contract"
        $testJson = & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") `
            -PrintPlanJson `
            -TestStateIsolatedProduct `
            -OutputDirectory $testOutput
        if ($LASTEXITCODE -ne 0) {
            throw "test-state-isolated plan failed with exit $LASTEXITCODE"
        }
        $testPlan = $testJson | ConvertFrom-Json
        if (-not $testPlan.testOnly -or ($testPlan.buildTags -join "|") -ne "test_state_path_env") {
            throw "test-state-isolated plan mode drift: testOnly=$($testPlan.testOnly) tags=$($testPlan.buildTags -join '|')"
        }
        $ordinaryPlan = $json | ConvertFrom-Json
        if ($ordinaryPlan.testOnly -or @($ordinaryPlan.buildTags).Count -ne 0) {
            throw "ordinary plan acquired test-only state"
        }
        foreach ($arch in @("amd64", "arm64")) {
            if ($releasePlans[$arch].testOnly -or @($releasePlans[$arch].buildTags).Count -ne 0) {
                throw "$arch release plan acquired test-only state"
            }
        }
        $invalidTestModeArguments = @(
            @("-PrintPlanJson", "-TestStateIsolatedProduct"),
            @("-PrintPlanJson", "-TestStateIsolatedProduct", "-OutputDirectory", "relative-o7"),
            @("-PrintPlanJson", "-TestStateIsolatedProduct", "-OutputDirectory", (Join-Path $fixtureRoot "outside-scratch")),
            @("-PrintPlanJson", "-TestStateIsolatedProduct", "-OutputDirectory", (Join-Path $fixtureRoot ".scratch")),
            @("-PrintPlanJson", "-TestStateIsolatedProduct", "-OutputDirectory", $testOutput,
                "-ReleaseVersion", $releaseVersion, "-ReleaseCommit", $releaseCommit, "-ReleaseBuildDate", $releaseBuildDate)
        )
        foreach ($invalidArguments in $invalidTestModeArguments) {
            & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") @invalidArguments *> $null
            if ($LASTEXITCODE -eq 0) {
                throw "test-state-isolated plan accepted invalid arguments: $($invalidArguments -join ' ')"
            }
        }
        $priorModePath = $env:PATH
        $priorFakeLog = $env:MCPHUB_FAKE_GO_LOG
        try {
            $env:PATH = $fakeBuildTools + [IO.Path]::PathSeparator + $system32
            $env:MCPHUB_FAKE_GO_LOG = $fakeBuildLog
            $escapedBuildScript = (Join-Path $fixtureRoot "build.ps1").Replace("'", "''")
            $escapedTestOutput = $testOutput.Replace("'", "''")
            $probeCommand = @"
`$env:GOFLAGS = '-tags=ambient_should_not_apply'
. '$escapedBuildScript' -TestStateIsolatedProduct -OutputDirectory '$escapedTestOutput'
Write-Output ('__GOFLAGS_AFTER__=' + `$env:GOFLAGS)
"@
            $probeOutput = @(& $pwsh -NoProfile -Command $probeCommand)
            if ($LASTEXITCODE -ne 0) {
                throw "test-state-isolated build probe failed with exit $LASTEXITCODE"
            }
        } finally {
            $env:PATH = $priorModePath
            $env:MCPHUB_FAKE_GO_LOG = $priorFakeLog
        }
        if ($probeOutput[-1] -ne "__GOFLAGS_AFTER__=-tags=ambient_should_not_apply") {
            throw "test-state-isolated build did not restore caller GOFLAGS: $($probeOutput[-1])"
        }
        $fakeGoCalls = @(Get-Content -LiteralPath $fakeBuildLog | ForEach-Object { $_ | ConvertFrom-Json })
        $buildCalls = @($fakeGoCalls | Where-Object { $_.argv[0] -eq "build" })
        if ($buildCalls.Count -ne 1) { throw "test-state-isolated build call count=$($buildCalls.Count), want 1" }
        $buildArgv = @($buildCalls[0].argv)
        $tagIndexes = @(0..($buildArgv.Count - 1) | Where-Object { $buildArgv[$_] -eq "-tags" })
        if ($tagIndexes.Count -ne 1 -or $tagIndexes[0] + 1 -ge $buildArgv.Count -or
            $buildArgv[$tagIndexes[0] + 1] -ne "test_state_path_env") {
            throw "test-state-isolated build tag argv drift: $($buildArgv -join '|')"
        }
        if ([string]$buildCalls[0].goflags -ne "") {
            throw "test-state-isolated build inherited GOFLAGS=$($buildCalls[0].goflags)"
        }
        $compilePhaseCalls = @($fakeGoCalls | Where-Object {
            $_.argv[0] -eq "build" -or
            ($_.argv[0] -eq "run" -and $_.argv[1] -like "github.com/josephspurrier/goversioninfo/*")
        })
        if (@($compilePhaseCalls | Where-Object { [string]$_.goflags -ne "" }).Count -ne 0) {
            throw "test-state-isolated compile phase inherited ambient GOFLAGS"
        }
        $validatorCalls = @($fakeGoCalls | Where-Object {
            $_.argv[0] -eq "run" -and $_.argv[1] -eq "./tools/windows-product-validate"
        })
        if ($validatorCalls.Count -ne 1 -or
            [string]$validatorCalls[0].goflags -ne "-tags=ambient_should_not_apply") {
            throw "test-state-isolated build did not restore GOFLAGS before repository validation"
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

if ($plan.schemaVersion -ne 2) { throw "schemaVersion=$($plan.schemaVersion), want 2" }
if ($plan.artifacts.Count -ne 1) { throw "artifact count=$($plan.artifacts.Count), want 1" }

$expected = @(
    [pscustomobject]@{ role = "product"; filename = "mcphub.exe"; source = "./cmd/mcphub"; subsystem = 2; productMetadata = $true; npmPayload = $true; npmBin = $false }
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

$product = $plan.artifacts[0]
if ($product.ldflags -notmatch '(?:^| )-H windowsgui(?: |$)') { throw "product linker flags missing -H windowsgui: $($product.ldflags)" }

foreach ($metadata in @("version", "commit", "buildDate")) {
    if ($product.ldflags -notmatch "-X main\.$metadata=") {
        throw "product linker flags do not embed $($metadata): $($product.ldflags)"
    }
}

$wantVersionInfo = @{
    product = @{ InternalName = "mcphub"; OriginalFilename = "mcphub.exe"; SpecialBuild = "mcphub-role-v1;role=product" }
}
$planBuildDate = if ($plan.buildDate -is [datetime]) { $plan.buildDate.ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ") } else { [string]$plan.buildDate }
foreach ($artifact in @($product)) {
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

if ($plan.identityManifest.filename -ne "windows-product-identity.json" -or
    $plan.identityManifest.schema -ne "windows-product-identity-v1" -or
    $plan.identityManifest.output -ne "bin/windows-product-identity.json") {
    throw "identity manifest plan drift"
}
if (($plan.retiredOutputFiles -join "|") -ne "mcphub-windowless.exe|mcphub-pe-admit.exe") {
    throw "retired output cleanup drift: $($plan.retiredOutputFiles -join '|')"
}
$admissionArgs = @($plan.admissionCommand.argv | ForEach-Object {
    if ($_ -is [datetime]) { $_.ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ") } else { [string]$_ }
})
if ($plan.admissionCommand.executable -ne "go" -or
    ($admissionArgs -join "|") -ne
        "run|./tools/windows-product-validate|--candidate|bin/mcphub.exe|--expected-version|$($plan.version)|--expected-commit|$($plan.commit)|--expected-build-date|$planBuildDate") {
    throw "product admission command drift: $($admissionArgs -join '|')"
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
    $wantMachine = if ($arch -eq "arm64") { 0xaa64 } else { 0x8664 }
    if ($releasePlan.expectedMachine -ne $wantMachine) {
        throw "$arch expected machine=$($releasePlan.expectedMachine), want $wantMachine"
    }
}

$publishWorkflow = Get-Content -Raw -LiteralPath (Join-Path $repoRoot ".github/workflows/npm-publish.yml")
if ($publishWorkflow -notmatch "(?m)^\s*node-version:\s*'24\.21\.0'\s*$" -or
    $publishWorkflow -notmatch "(?m)^\s*package-manager-cache:\s*false\s*$") {
    throw "npm publish workflow must pin the Trusted Publishing Node 24.21.0 distribution with package-manager caching disabled"
}
if ($publishWorkflow -notmatch 'actions/setup-node@a0853c24544627f65ddf259abe73b1d18a591444' -or
    $publishWorkflow -notmatch "(?m)^\s*runs-on:\s*ubuntu-latest\s*$" -or
    $publishWorkflow -notmatch "(?m)^\s*id-token:\s*write(?:\s+#.*)?$") {
    throw "npm publish workflow must retain its pinned setup-node action, GitHub-hosted runner, and OIDC permission"
}
if ($publishWorkflow -match 'NPM_TOKEN|NODE_AUTH_TOKEN|_authToken|continue-on-error') {
    throw "npm publish workflow must have no long-lived publishing token or fail-open fallback"
}
if ($publishWorkflow -match "(?m)^\s*environment:\s*" -or
    $publishWorkflow -match 'npm\s+(?:install|i)\s+(?:--global|-g)\s+npm') {
    throw "npm publish workflow must not invent an environment binding or mutate the bundled npm toolchain"
}
$toolchainStepMarker = "      - name: Verify Trusted Publishing toolchain"
$publishStepMarker = "      - name: Publish platform packages, then the meta"
$toolchainStepStart = $publishWorkflow.IndexOf($toolchainStepMarker)
$publishStepStart = $publishWorkflow.IndexOf($publishStepMarker)
$nextStepAfterToolchain = if ($toolchainStepStart -ge 0) {
    $publishWorkflow.IndexOf("`n      - name:", $toolchainStepStart + $toolchainStepMarker.Length)
} else {
    -1
}
if ($toolchainStepStart -lt 0 -or $nextStepAfterToolchain -lt 0 -or
    $publishStepStart -lt 0 -or $toolchainStepStart -ge $publishStepStart) {
    throw "npm publish workflow must run the named Trusted Publishing toolchain gate before the publish step"
}
$toolchainStep = $publishWorkflow.Substring($toolchainStepStart, $nextStepAfterToolchain - $toolchainStepStart)
if ($toolchainStep -notmatch '\$\(node --version\)" != "v24\.21\.0"' -or
    $toolchainStep -notmatch '\$\(npm --version\)" != "11\.19\.0"') {
    throw "npm publish workflow must fail closed unless Node and npm match the exact Trusted Publishing toolchain"
}
if ($publishWorkflow -notmatch "(?s)push:\s*\n\s*tags:\s*\n\s*- 'v\*\.\*\.\*'" -or
    $publishWorkflow -notmatch "(?s)workflow_dispatch:.+dry_run:.+default:\s*true" -or
    $publishWorkflow -notmatch "\^\[0-9\]\+\\\.\[0-9\]\+\\\.\[0-9\]\+" -or
    $publishWorkflow -notmatch 'case "\$V" in \*-\*\) TAG="--tag beta"' -or
    @([regex]::Matches($publishWorkflow, 'npm publish --provenance --access public')).Count -ne 2) {
    throw "npm publish workflow must preserve trigger, dry-run, semver, beta-tag, and provenance behavior"
}
$publishStep = $publishWorkflow.Substring($publishStepStart)
$platformLoop = $publishStep.IndexOf('for d in win32-x64 win32-arm64 darwin-x64 darwin-arm64 linux-x64 linux-arm64')
$platformPublishPattern = '(?m)^ {12}\( cd "npm/packages/\$d" && npm publish --provenance --access public \$TAG \$DRY \)\s*$'
$metaPublishPattern = '(?m)^ {10}\( cd npm && npm publish --provenance --access public \$TAG \$DRY \)\s*$'
$platformPublishes = @([regex]::Matches($publishStep, $platformPublishPattern))
$metaPublishes = @([regex]::Matches($publishStep, $metaPublishPattern))
if ($publishStep -match '\|\|\s*true' -or $platformPublishes.Count -ne 1 -or $metaPublishes.Count -ne 1) {
    throw "npm publish workflow must use exactly two direct unmasked fail-propagating publish commands"
}
if ($platformLoop -lt 0 -or $platformPublishes[0].Index -le $platformLoop -or
    $metaPublishes[0].Index -le $platformPublishes[0].Index) {
    throw "npm publish workflow must publish the real six-package loop before the meta package"
}
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
if ($publishWorkflow -notmatch 'tools/windows-product-validate' -or
    $publishWorkflow -match 'mcphub-pe-admit') {
    throw "npm publish workflow must use only the repository product validator"
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

Write-Output "PASS: Windows build plan has one GUI-subsystem product, repository-only admission, and a non-executable identity manifest."

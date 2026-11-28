#Requires -Version 5.1

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$buildScript = Join-Path $repoRoot "build.ps1"
$scratchRoot = Join-Path $repoRoot ".scratch"
$fixtureRoot = Join-Path $scratchRoot "windows-build-plan-$PID-$([Guid]::NewGuid().ToString('N'))"
$fakeTools = Join-Path $fixtureRoot "fake-tools"
$fakeBuildTools = Join-Path $fixtureRoot "fake-build-tools"
$fakeBuildLog = Join-Path $fixtureRoot "fake-build-go.jsonl"
$commandTestBuildLog = Join-Path $fixtureRoot "fake-command-test-go.jsonl"
$privateOutputsRoot = Join-Path $scratchRoot ((Split-Path -Leaf $fixtureRoot) + "-private-inputs")
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
# The DATA fixture also replaces destructive filesystem cleanup: the real
# builder still selects and checks its original targets, but its test-local
# Remove-Item receives those files by move. Keep this evidence outside trash
# until every assertion in this script has passed. Release cleanup is unchanged.
$cleanupBoundary = @'
function Remove-Item {
    [CmdletBinding()]
    param([Parameter(Position=0)][string[]]$Path, [string[]]$LiteralPath, [switch]$Force, [switch]$Recurse)
    $targets = if ($null -ne $LiteralPath) { $LiteralPath } else { $Path }
    foreach ($target in $targets) {
        if ($target -match '^Env:') {
            Microsoft.PowerShell.Management\Remove-Item -LiteralPath $target -Force:$Force -ErrorAction Stop
            continue
        }
        $absolute = [IO.Path]::GetFullPath($target)
        $fixturePrefix = [IO.Path]::GetFullPath($PSScriptRoot).TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
        if (-not $absolute.StartsWith($fixturePrefix, [StringComparison]::OrdinalIgnoreCase)) {
            throw "DATA cleanup refused an operand outside its own fixture"
        }
        $preserved = Join-Path $PSScriptRoot "preserved-legacy-cleanup"
        if (-not (Test-Path -LiteralPath $preserved)) { New-Item -ItemType Directory -Path $preserved -ErrorAction Stop | Out-Null }
        $destination = Join-Path $preserved ([Guid]::NewGuid().ToString("N") + "-" + [IO.Path]::GetFileName($absolute))
        if (Test-Path -LiteralPath $destination) { throw "DATA cleanup destination already exists" }
        Move-Item -LiteralPath $absolute -Destination $destination -ErrorAction Stop
    }
}
'@
$fixtureBuildPath = Join-Path $fixtureRoot "build.ps1"
$fixtureBuildText = [IO.File]::ReadAllText($fixtureBuildPath)
$cleanupInsertion = '$ErrorActionPreference = "Stop"'
if ([regex]::Matches($fixtureBuildText, [regex]::Escape($cleanupInsertion)).Count -ne 1) {
    throw "DATA cleanup boundary requires the exact builder initialization point"
}
[IO.File]::WriteAllText($fixtureBuildPath, $fixtureBuildText.Replace($cleanupInsertion, $cleanupInsertion + [char]10 + $cleanupBoundary),
    (New-Object Text.UTF8Encoding($false)))
$contract = Join-Path $repoRoot "windows-product-artifacts.json"
if (Test-Path -LiteralPath $contract) {
    Copy-Item -LiteralPath $contract -Destination (Join-Path $fixtureRoot "windows-product-artifacts.json")
}
[void](New-Item -ItemType Directory -Force -Path (Join-Path $fixtureRoot "cmd/mcphub"))
[void](New-Item -ItemType Directory -Force -Path (Join-Path $fixtureRoot "internal/cli"))
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
# PowerShell -File rebinds a leading-dash argument containing a colon (the
# single -ldflags value contains an ISO build date). Receive the original
# process argv before that script binder; the real go.exe receives this tuple.
$processArgs = [Environment]::GetCommandLineArgs()
$scriptAt = [Array]::IndexOf($processArgs, $PSCommandPath)
if ($scriptAt -ge 0) {
    if ($scriptAt -lt 1 -or $processArgs[$scriptAt - 1] -cne "-File" -or
        @($processArgs | Where-Object { $_ -ceq $PSCommandPath }).Count -ne 1) { exit 80 }
    $CommandArgs = [string[]]@($processArgs | Select-Object -Skip ($scriptAt + 1))
} else {
    # The three existing dot-source probes call the explicit go alias inside
    # their PowerShell process. '& go' reports InvocationName='&'; receive
    # its original builder ScriptName and actual alias definition instead.
    $expectedBuilder = Join-Path (Split-Path -Parent $PSScriptRoot) "build.ps1"
    $goAlias = Get-Alias -Name go -ErrorAction Stop
    if ($MyInvocation.InvocationName -cne "&" -or $MyInvocation.ScriptName -cne $expectedBuilder -or
        $MyInvocation.MyCommand.Path -cne $PSCommandPath -or $goAlias.Definition -cne $PSCommandPath) { exit 80 }
    $CommandArgs = [string[]]$args
}
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
    if ($env:MCPHUB_FAKE_RESOURCE_EXIT -eq "93") { exit 93 }
    exit 0
}
if ($CommandArgs.Count -ge 1 -and $CommandArgs[0] -eq "build") {
    $outputIndex = [Array]::IndexOf($CommandArgs, "-o")
    if ($outputIndex -lt 0 -or $outputIndex + 1 -ge $CommandArgs.Count) { exit 82 }
    [IO.File]::WriteAllText($CommandArgs[$outputIndex + 1], $(if ($CommandArgs[-1] -eq "./cmd/mcphub-launcher") { "fake-launcher" } else { "fake-product" }))
    exit 0
}
if ($CommandArgs.Count -ge 2 -and $CommandArgs[0] -eq "test" -and $CommandArgs -contains "-c") {
    $outputIndex = [Array]::IndexOf($CommandArgs, "-o")
    if ($outputIndex -lt 0 -or $outputIndex + 1 -ge $CommandArgs.Count) { exit 85 }
    [IO.File]::WriteAllText($CommandArgs[$outputIndex + 1], "fake-command-test-product")
    if ($env:MCPHUB_FAKE_EXTRA_EXE -eq "1") {
        $productDir = Split-Path -Parent $CommandArgs[$outputIndex + 1]
        $outputRoot = Split-Path -Parent $productDir
        [IO.File]::WriteAllText((Join-Path $outputRoot "mcphub-extra.exe"), "unexpected-extra-executable")
    }
    exit 0
}
if ($CommandArgs.Count -ge 2 -and $CommandArgs[0] -eq "run" -and
    $CommandArgs[1] -eq "./tools/windows-product-validate") {
    function Value-After([string]$Name) {
        $index = [Array]::IndexOf($CommandArgs, $Name)
        if ($index -lt 0 -or $index + 1 -ge $CommandArgs.Count) { exit 83 }
        return $CommandArgs[$index + 1]
    }
    if ($CommandArgs -contains "--source-snapshot" -and
        $CommandArgs -notcontains "--candidate" -and
        $CommandArgs -notcontains "--validate-entry-set-build" -and
        $CommandArgs -notcontains "--stage-runtime") {
        $sourceReads = @(Get-Content -LiteralPath $env:MCPHUB_FAKE_GO_LOG | ForEach-Object { $_ | ConvertFrom-Json } |
            Where-Object { $_.argv -contains "--source-snapshot" -and $_.argv -notcontains "--candidate" -and
                $_.argv -notcontains "--validate-entry-set-build" -and $_.argv -notcontains "--stage-runtime" })
        $epoch = if ($env:MCPHUB_FAKE_SOURCE_CHANGE -eq "1" -and $sourceReads.Count -gt 1) { "c" * 64 } else { "a" * 64 }
        Write-Output ([ordered]@{ schema = "windows-publisher-source-input-v1"; source_epoch = $epoch;
            source_snapshot_sha256 = (Get-FileHash -LiteralPath (Value-After "--source-snapshot") -Algorithm SHA256).Hash.ToLowerInvariant();
            file_count = 1 } | ConvertTo-Json -Compress)
        exit 0
    }
    if ($CommandArgs -contains "--stage-runtime") {
        $runtimeOutput = Value-After "--runtime-output"
        New-Item -ItemType Directory -Path $runtimeOutput -ErrorAction Stop | Out-Null
        $runtimeIndex = Join-Path $runtimeOutput "runtime-payload.json"
        [IO.File]::WriteAllText($runtimeIndex, '{"fixture":"control-data-only"}')
        Write-Output ([ordered]@{ schema = "hub-runtime-staging-v1";
            runtime_payload_sha256 = (Get-FileHash -LiteralPath $runtimeIndex -Algorithm SHA256).Hash.ToLowerInvariant();
            file_count = 1 } | ConvertTo-Json -Compress)
        exit 0
    }
    if ($CommandArgs -contains "--encode-entry-comments") {
        $request = Get-Content -Raw -LiteralPath (Value-After "--encode-entry-comments") | ConvertFrom-Json
        [IO.File]::WriteAllText(($env:MCPHUB_FAKE_GO_LOG + ".comments-request.json"), ($request | ConvertTo-Json -Compress))
        Write-Output '{"comments":"synthetic-codec-result"}'
        exit 0
    }
    if ($CommandArgs -contains "--validate-entry-set-build") {
        $entry = Value-After "--validate-entry-set-build"
        $identityRelative = if ($CommandArgs -contains "--runtime-payload") { "mcphub-product/windows-product-identity.json" } else { "windows-product-identity.json" }
        $identityPath = Join-Path (Split-Path $entry) $identityRelative
        $identity = Get-Content -Raw -LiteralPath $identityPath | ConvertFrom-Json
        $observation = [ordered]@{
            schema = "windows-canonical-entry-build-observation-v1"
            build_id = Value-After "--expected-entry-set-build-id"
            registry_projection_sha256 = ("b" * 64)
            canonical_entry_sha256 = (Get-FileHash $entry -Algorithm SHA256).Hash.ToLowerInvariant()
            product_sha256 = $identity.sha256
            identity_sha256 = (Get-FileHash $identityPath -Algorithm SHA256).Hash.ToLowerInvariant()
        }
        if ($CommandArgs -contains "--expected-source-epoch") { $observation.source_epoch = Value-After "--expected-source-epoch" }
        Write-Output ($observation | ConvertTo-Json -Compress)
        exit 0
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
    if ($CommandArgs -contains "--runtime-payload") {
        $identity.schema = "windows-product-identity-v2"
        $identity.source_epoch = Value-After "--expected-source-epoch"
        $identity.launcher_sha256 = (Get-FileHash -LiteralPath (Value-After "--helper") -Algorithm SHA256).Hash.ToLowerInvariant()
        $identity.runtime_payload_path = Value-After "--runtime-payload"
        $identity.runtime_payload_sha256 = (Get-FileHash -LiteralPath (Join-Path (Split-Path -Parent $candidate) $identity.runtime_payload_path) -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($env:MCPHUB_FAKE_RUNTIME_MISMATCH -eq "1") { $identity.runtime_payload_sha256 = "f" * 64 }
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
"$pwsh" -NoProfile -File "$fakeGoScriptPath" %*
@exit /b %ERRORLEVEL%
"@
[IO.File]::WriteAllText((Join-Path $fakeBuildTools "go.cmd"), $fakeGoCommand)
[IO.File]::WriteAllText((Join-Path $fakeTools "go.cmd"), ($fakeGoCommand.Replace("@echo off", "@echo off" + [char]13 + [char]10 + "set MCPHUB_FAKE_RESOURCE_EXIT=93" + [char]13 + [char]10 + "set MCPHUB_FAKE_GO_LOG=" + $fakeBuildLog)))

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
        $commandTestOutput = Join-Path $fixtureRoot ".scratch/o7-command-test-product"
        $commandTestJson = & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") `
            -PrintPlanJson `
            -TestStateIsolatedProduct `
            -CanonicalEntryCommandTestProduct `
            -OutputDirectory $commandTestOutput
        if ($LASTEXITCODE -ne 0) {
            throw "canonical command test Product plan failed with exit $LASTEXITCODE"
        }
        $commandTestPlan = $commandTestJson | ConvertFrom-Json
        if (-not $commandTestPlan.testOnly -or -not $commandTestPlan.canonicalEntryCommandTestProduct -or
            $commandTestPlan.testProductPackage -cne "./internal/cli" -or
            $commandTestPlan.artifacts[0].source -cne "./internal/cli" -or
            $commandTestPlan.canonicalCommandFixture -cne (Join-Path $commandTestOutput "windows-canonical-command-test-fixture.json")) {
            throw "canonical command test Product plan did not expose its exact test package and fixture output"
        }
        $ordinaryPlan = $json | ConvertFrom-Json
        if ($ordinaryPlan.testOnly -or @($ordinaryPlan.buildTags).Count -ne 0) {
            throw "ordinary plan acquired test-only state"
        }
        foreach ($otherPlan in @($ordinaryPlan, $testPlan, $releasePlans["amd64"], $releasePlans["arm64"])) {
            foreach ($field in @("canonicalEntryCommandTestProduct", "testProductPackage", "canonicalCommandFixture")) {
                if (@($otherPlan.PSObject.Properties.Name) -contains $field) {
                    throw "non-command-test plan acquired $field"
                }
            }
        }
        foreach ($arch in @("amd64", "arm64")) {
            if ($releasePlans[$arch].testOnly -or @($releasePlans[$arch].buildTags).Count -ne 0) {
                throw "$arch release plan acquired test-only state"
            }
        }
        $invalidTestModeArguments = @(
            @("-PrintPlanJson", "-CanonicalEntryCommandTestProduct"),
            @("-PrintPlanJson", "-CanonicalEntryCommandTestProduct", "-OutputDirectory", $commandTestOutput),
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
            $escapedFakeGoScript = $fakeGoScriptPath.Replace("'", "''")
            $probeCommand = @"
Set-Alias -Name go -Value '$escapedFakeGoScript' -Scope Local
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
        if ($buildCalls.Count -ne 2 -or $buildCalls[0].argv[-1] -ne "./cmd/mcphub-launcher" -or $buildCalls[1].argv[-1] -ne "./cmd/mcphub") { throw "CUI must compile once before the GUI Product" }
        $buildArgv = @($buildCalls[1].argv)
        $tagIndexes = @(0..($buildArgv.Count - 1) | Where-Object { $buildArgv[$_] -eq "-tags" })
        if ($tagIndexes.Count -ne 1 -or $tagIndexes[0] + 1 -ge $buildArgv.Count -or
            $buildArgv[$tagIndexes[0] + 1] -ne "test_state_path_env") {
            throw "test-state-isolated build tag argv drift: $($buildArgv -join '|')"
        }
        if (@($buildCalls | Where-Object { [string]$_.goflags -ne "" }).Count -ne 0) {
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
        $postBuildValidatorCalls = @($validatorCalls | Where-Object { $_.argv -notcontains "--encode-entry-comments" })
        if ($postBuildValidatorCalls.Count -ne 2 -or
            @($postBuildValidatorCalls | Where-Object { [string]$_.goflags -ne "-tags=ambient_should_not_apply" }).Count -ne 0) {
            throw "test-state-isolated build did not restore GOFLAGS before repository validation"
        }
        $cuiFlags = [string]$buildCalls[0].argv[[Array]::IndexOf([string[]]$buildCalls[0].argv, "-ldflags") + 1]
        $guiFlags = [string]$buildCalls[1].argv[[Array]::IndexOf([string[]]$buildCalls[1].argv, "-ldflags") + 1]
        $request = Get-Content -Raw -LiteralPath ($fakeBuildLog + ".comments-request.json") | ConvertFrom-Json
        $fakeEntryHash = (Get-FileHash -LiteralPath (Join-Path $testOutput "mcphub.exe") -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($request.build_id -notmatch '^[0-9a-f]{32}$' -or $request.launcher_sha256 -cne $fakeEntryHash -or $request.reverse_projection -cne "../mcphub.exe" -or
            $cuiFlags -notlike ("*-X main.publicEntrySetBuildID=" + $request.build_id + "*") -or
            $guiFlags -notlike ("*-X main.entrySetBuildID=" + $request.build_id + "*") -or
            $guiFlags -notlike ("*-X main.expectedCanonicalEntrySHA256=" + $fakeEntryHash + "*") -or
            $guiFlags.Contains("<actual-launcher-sha256>")) {
            throw ("Original CUI hash/BuildID did not reach the GUI and encoder; witness=" + ([ordered]@{
                predicates = [ordered]@{
                    buildIDValid = ($request.build_id -match '^[0-9a-f]{32}$')
                    launcherHashMatches = ($request.launcher_sha256 -ceq $fakeEntryHash)
                    reverseProjectionMatches = ($request.reverse_projection -ceq "../mcphub.exe")
                    cuiBuildIDMatches = ($cuiFlags -like ("*-X main.publicEntrySetBuildID=" + $request.build_id + "*"))
                    guiBuildIDMatches = ($guiFlags -like ("*-X main.entrySetBuildID=" + $request.build_id + "*"))
                    guiLauncherHashMatches = ($guiFlags -like ("*-X main.expectedCanonicalEntrySHA256=" + $fakeEntryHash + "*"))
                    guiPlaceholderAbsent = (-not $guiFlags.Contains("<actual-launcher-sha256>"))
                }
                request = [ordered]@{ build_id = $request.build_id; launcher_sha256 = $request.launcher_sha256; reverse_projection = $request.reverse_projection }
                fakeEntryHash = $fakeEntryHash
                cuiFlags = $cuiFlags
                guiFlags = $guiFlags
            } | ConvertTo-Json -Depth 4 -Compress))
        }
        $encoderCalls = @($validatorCalls | Where-Object { $_.argv -contains "--encode-entry-comments" })
        if ($encoderCalls.Count -ne 1 -or $cuiFlags.Contains("expectedCanonicalEntrySHA256") -or $buildCalls[0].argv -contains "-tags") {
            throw "Build topology contains a launcher hash cycle, extra launcher build, or tagged CUI"
        }

        $priorModePath = $env:PATH
        $priorFakeLog = $env:MCPHUB_FAKE_GO_LOG
        try {
            $env:PATH = $fakeBuildTools + [IO.Path]::PathSeparator + $system32
            $env:MCPHUB_FAKE_GO_LOG = $commandTestBuildLog
            $escapedBuildScript = (Join-Path $fixtureRoot "build.ps1").Replace("'", "''")
            $escapedCommandTestOutput = $commandTestOutput.Replace("'", "''")
            $escapedFakeGoScript = $fakeGoScriptPath.Replace("'", "''")
            $commandProbe = @"
Set-Alias -Name go -Value '$escapedFakeGoScript' -Scope Local
`$env:GOFLAGS = '-tags=ambient_should_not_apply'
. '$escapedBuildScript' -TestStateIsolatedProduct -CanonicalEntryCommandTestProduct -OutputDirectory '$escapedCommandTestOutput'
Write-Output ('__GOFLAGS_AFTER__=' + `$env:GOFLAGS)
"@
            $commandProbeOutput = @(& $pwsh -NoProfile -Command $commandProbe)
            if ($LASTEXITCODE -ne 0) {
                throw "canonical command test Product build probe failed with exit $LASTEXITCODE"
            }
        } finally {
            $env:PATH = $priorModePath
            $env:MCPHUB_FAKE_GO_LOG = $priorFakeLog
        }
        if ($commandProbeOutput[-1] -ne "__GOFLAGS_AFTER__=-tags=ambient_should_not_apply") {
            throw "canonical command test Product build did not restore caller GOFLAGS"
        }
        $commandCalls = @(Get-Content -LiteralPath $commandTestBuildLog | ForEach-Object { $_ | ConvertFrom-Json })
        $commandBuildCalls = @($commandCalls | Where-Object { $_.argv[0] -eq "build" })
        $commandTestCalls = @($commandCalls | Where-Object { $_.argv[0] -eq "test" -and $_.argv -contains "-c" })
        if ($commandBuildCalls.Count -ne 1 -or $commandBuildCalls[0].argv[-1] -ne "./cmd/mcphub-launcher" -or
            $commandTestCalls.Count -ne 1 -or $commandTestCalls[0].argv[-1] -ne "./internal/cli") {
            throw "canonical command fixture must build one launcher and compile internal/cli once with go test -c"
        }
        $commandTestArgv = @($commandTestCalls[0].argv)
        $commandTagIndex = [Array]::IndexOf([string[]]$commandTestArgv, "-tags")
        if ($commandTagIndex -lt 0 -or $commandTestArgv[$commandTagIndex + 1] -ne "test_state_path_env" -or
            [string]$commandTestCalls[0].goflags -ne "") {
            throw "canonical command test Product did not isolate its test_state_path_env tag"
        }
        $fixtureInputPath = Join-Path $commandTestOutput "windows-canonical-command-test-fixture.json"
        if (-not (Test-Path -LiteralPath $fixtureInputPath -PathType Leaf)) {
            throw "canonical command test Product did not issue fixture input"
        }
        $fixtureInput = Get-Content -Raw -LiteralPath $fixtureInputPath | ConvertFrom-Json
        $commandCommentsRequest = Get-Content -Raw -LiteralPath ($commandTestBuildLog + ".comments-request.json") | ConvertFrom-Json
        $commandEntry = Join-Path $commandTestOutput "mcphub.exe"
        $commandProduct = Join-Path $commandTestOutput "mcphub-product/mcphub.exe"
        $commandIdentity = Join-Path $commandTestOutput "windows-product-identity.json"
        if ($fixtureInput.product_root -cne [IO.Path]::GetFullPath($commandTestOutput) -or
            $fixtureInput.source_epoch -cne "" -or $fixtureInput.product_version -cne $commandTestPlan.version -or
            $fixtureInput.build_id -cne $commandCommentsRequest.build_id -or
            $fixtureInput.reverse_projection -cne "../mcphub.exe" -or
            $fixtureInput.canonical_entry_sha256 -cne (Get-FileHash $commandEntry -Algorithm SHA256).Hash.ToLowerInvariant() -or
            $fixtureInput.product_sha256 -cne (Get-FileHash $commandProduct -Algorithm SHA256).Hash.ToLowerInvariant() -or
            $fixtureInput.identity_sha256 -cne (Get-FileHash $commandIdentity -Algorithm SHA256).Hash.ToLowerInvariant()) {
            throw "canonical command fixture input disagrees with the emitted original files"
        }

        $refusalOutput = Join-Path $fixtureRoot ".scratch/o7-command-test-extra-exe"
        $priorModePath = $env:PATH
        $priorFakeLog = $env:MCPHUB_FAKE_GO_LOG
        $priorExtraExe = $env:MCPHUB_FAKE_EXTRA_EXE
        try {
            $env:PATH = $fakeBuildTools + [IO.Path]::PathSeparator + $system32
            $env:MCPHUB_FAKE_GO_LOG = Join-Path $fixtureRoot "fake-command-test-refusal-go.jsonl"
            $env:MCPHUB_FAKE_EXTRA_EXE = "1"
            $escapedRefusalOutput = $refusalOutput.Replace("'", "''")
            $refusalProbe = @"
Set-Alias -Name go -Value '$escapedFakeGoScript' -Scope Local
. '$escapedBuildScript' -TestStateIsolatedProduct -CanonicalEntryCommandTestProduct -OutputDirectory '$escapedRefusalOutput'
"@
            & $pwsh -NoProfile -Command $refusalProbe *> $null
            if ($LASTEXITCODE -eq 0) {
                throw "canonical command test Product admitted an undeclared executable"
            }
        } finally {
            $env:PATH = $priorModePath
            $env:MCPHUB_FAKE_GO_LOG = $priorFakeLog
            $env:MCPHUB_FAKE_EXTRA_EXE = $priorExtraExe
        }
        if (Test-Path -LiteralPath (Join-Path $refusalOutput "windows-canonical-command-test-fixture.json")) {
            throw "failed canonical command build issued a usable fixture"
        }
        if (Test-Path -LiteralPath (Join-Path $fixtureRoot "internal/cli/zz_product_versioninfo.syso")) {
            throw "failed canonical command build retained its generated VERSIONINFO resource"
        }
        if ((Test-Path -LiteralPath (Join-Path $refusalOutput "trash")) -or
            -not (Test-Path -LiteralPath (Join-Path $refusalOutput "retained-build-inputs/zz_product_versioninfo.syso"))) {
            throw "failed private command admission did not preserve its original inputs outside trash"
        }
        & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") -PrintPlanJson `
            -TestStateIsolatedProduct -OutputDirectory $testOutput *> $null
        if ($LASTEXITCODE -eq 0 -or (Get-FileHash -LiteralPath (Join-Path $testOutput "mcphub.exe") -Algorithm SHA256).Hash.ToLowerInvariant() -cne $fakeEntryHash) {
            throw "test-only builder reused or changed an existing Product output"
        }

        # These compiler/validator stubs test the producer's dataflow only.
        # Synthetic bytes below are never executed or accepted as qualification.
        New-Item -ItemType Directory -Path $privateOutputsRoot -ErrorAction Stop | Out-Null
        $sourceSnapshot = Join-Path $privateOutputsRoot "source-control.json"
        [IO.File]::WriteAllText($sourceSnapshot, '{"fixture":"not-a-qualified-source-snapshot"}')
        $runtimeArchives = Join-Path $privateOutputsRoot "runtime-archives"
        New-Item -ItemType Directory -Path $runtimeArchives -ErrorAction Stop | Out-Null
        $privateCommon = @("-WindowsTargetArch", "amd64", "-ReleaseVersion", $releaseVersion,
            "-ReleaseCommit", $releaseCommit, "-ReleaseBuildDate", $releaseBuildDate, "-SourceSnapshotPath", $sourceSnapshot)
        $registryBase64 = [Convert]::ToBase64String([IO.File]::ReadAllBytes((Join-Path $fixtureRoot "windows-product-artifacts.json")))
        $privateEnvironment = @{}
        foreach ($name in @("PATH", "GOFLAGS", "MCPHUB_FAKE_GO_LOG", "MCPHUB_FAKE_RESOURCE_EXIT", "MCPHUB_FAKE_SOURCE_CHANGE", "MCPHUB_FAKE_RUNTIME_MISMATCH")) {
            $privateEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
        }
        try {
            $env:PATH = $fakeBuildTools + [IO.Path]::PathSeparator + $system32
            $env:GOFLAGS = "-mod=readonly"
            $env:MCPHUB_FAKE_RESOURCE_EXIT = ""
            $env:MCPHUB_FAKE_SOURCE_CHANGE = ""
            $env:MCPHUB_FAKE_RUNTIME_MISMATCH = ""
            foreach ($lane in @("default", "test_state_path_env")) {
                $laneOutput = Join-Path $privateOutputsRoot $lane
                $env:MCPHUB_FAKE_GO_LOG = Join-Path $privateOutputsRoot "$lane-plan.jsonl"
                $privatePlanRows = & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") @privateCommon `
                    -PrepareCLIPrivatePreparationInputs -PrivateCLIPreparationLane $lane -OutputDirectory $laneOutput -PrintPlanJson
                if ($LASTEXITCODE -ne 0) { throw "$lane private compiler input plan failed" }
                $privatePlan = $privatePlanRows | ConvertFrom-Json
                $expectedTags = if ($lane -ceq "default") { "" } else { "test_state_path_env" }
                if ($privatePlan.schema -cne "windows-cli-publisher-test-inputs-v1" -or $privatePlan.prepared -or
                    $privatePlan.purpose -cne "private-serena-migration-integration" -or $privatePlan.role -cne "publisher-test" -or
                    $privatePlan.goEntrypoint -cne "mcp-local-hub/internal/cli.test" -or $privatePlan.package -cne "./internal/cli" -or
                    $privatePlan.subsystem -ne 2 -or $privatePlan.machine -ne 34404 -or ($privatePlan.buildTags -join ",") -cne $expectedTags -or
                    $privatePlan.publisherSource.source_epoch -cne ("a" * 64) -or $null -ne $privatePlan.resourceSHA256 -or
                    -not $privatePlan.ldflags.Contains("-X mcp-local-hub/internal/binaryadmission.compiledWindowsProductRegistryBase64=$registryBase64") -or
                    $privatePlan.versionInfo.SpecialBuild -cne "mcphub-role-v1;role=publisher-test" -or (Test-Path -LiteralPath $laneOutput)) {
                    throw "$lane private compiler plan lost its role, Source, registry or exact lane"
                }
                $env:MCPHUB_FAKE_GO_LOG = Join-Path $privateOutputsRoot "$lane-build.jsonl"
                & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") @privateCommon `
                    -PrepareCLIPrivatePreparationInputs -PrivateCLIPreparationLane $lane -OutputDirectory $laneOutput | Out-Null
                if ($LASTEXITCODE -ne 0) { throw "$lane private compiler input generation failed" }
                $issued = Get-Content -Raw -LiteralPath (Join-Path $laneOutput "cli-publisher-test-inputs.json") | ConvertFrom-Json
                $resourceConfiguration = Get-Content -Raw -LiteralPath $issued.resourceConfigurationPath | ConvertFrom-Json
                $calls = @(Get-Content -LiteralPath $env:MCPHUB_FAKE_GO_LOG | ForEach-Object { $_ | ConvertFrom-Json })
                if (-not $issued.prepared -or $issued.ldflags -cne $privatePlan.ldflags -or
                    ($issued.buildTags -join ",") -cne $expectedTags -or
                    $issued.resourceSHA256 -cne (Get-FileHash -LiteralPath $issued.resourcePath -Algorithm SHA256).Hash.ToLowerInvariant() -or
                    $resourceConfiguration.StringFileInfo.SpecialBuild -cne "mcphub-role-v1;role=publisher-test" -or
                    -not $resourceConfiguration.StringFileInfo.Comments.Contains("mcphub-source-v1;sha256=" + ("a" * 64)) -or
                    @($calls | Where-Object { $_.argv[0] -in @("build", "test") }).Count -ne 0 -or
                    @($calls | Where-Object { $_.argv -contains "--source-snapshot" }).Count -ne 2) {
                    throw "$lane compiler input issuance did not recheck Source or preserved a wrong resource"
                }
            }
            foreach ($failure in @("source-change", "resource-failure")) {
                $failedOutput = Join-Path $privateOutputsRoot $failure
                $env:MCPHUB_FAKE_GO_LOG = Join-Path $privateOutputsRoot "$failure.jsonl"
                $env:MCPHUB_FAKE_SOURCE_CHANGE = if ($failure -ceq "source-change") { "1" } else { "" }
                $env:MCPHUB_FAKE_RESOURCE_EXIT = if ($failure -ceq "resource-failure") { "93" } else { "" }
                & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") @privateCommon `
                    -PrepareCLIPrivatePreparationInputs -OutputDirectory $failedOutput *> $null
                if ($LASTEXITCODE -eq 0 -or (Test-Path -LiteralPath (Join-Path $failedOutput "cli-publisher-test-inputs.json"))) {
                    throw "$failure issued usable private compiler inputs"
                }
                if (-not (Test-Path -LiteralPath (Join-Path $failedOutput "cli-publisher-test.syso"))) {
                    throw "$failure removed original compiler failure residue before receiving"
                }
            }
            $env:MCPHUB_FAKE_RESOURCE_EXIT = ""
            $env:MCPHUB_FAKE_SOURCE_CHANGE = ""
            $currentOutput = Join-Path $privateOutputsRoot "current-runtime-actor"
            $env:MCPHUB_FAKE_GO_LOG = Join-Path $privateOutputsRoot "current-plan.jsonl"
            $currentPlanRows = & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") @privateCommon `
                -CurrentWindowsPackage -CurrentStateIsolatedTestProduct -RuntimeArchiveDirectory $runtimeArchives -OutputDirectory $currentOutput -PrintPlanJson
            if ($LASTEXITCODE -ne 0) { throw "current state-isolated actor plan failed" }
            $currentPlan = $currentPlanRows | ConvertFrom-Json
            if (-not $currentPlan.testOnly -or -not $currentPlan.currentStateIsolatedTestProduct -or
                ($currentPlan.buildTags -join ",") -cne "test_state_path_env" -or
                $currentPlan.identityManifest.schema -cne "windows-product-identity-v2" -or
                $currentPlan.canonicalEntry.identityRelativePath -cne "mcphub-product/windows-product-identity.json" -or
                -not $currentPlan.runtimePayload.precompileStagingRequired -or
                $currentPlan.canonicalRuntimeActorFixture -cne (Join-Path $currentOutput "private-serena-runtime-product.json") -or
                (Test-Path -LiteralPath $currentOutput)) { throw "current actor plan did not preserve its V4 test-only contract" }
            $env:MCPHUB_FAKE_GO_LOG = Join-Path $privateOutputsRoot "current-build.jsonl"
            & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") @privateCommon `
                -CurrentWindowsPackage -CurrentStateIsolatedTestProduct -RuntimeArchiveDirectory $runtimeArchives -OutputDirectory $currentOutput | Out-Null
            if ($LASTEXITCODE -ne 0) { throw "controlled current actor build failed" }
            $actor = Get-Content -Raw -LiteralPath $currentPlan.canonicalRuntimeActorFixture | ConvertFrom-Json
            $currentCalls = @(Get-Content -LiteralPath $env:MCPHUB_FAKE_GO_LOG | ForEach-Object { $_ | ConvertFrom-Json })
            $currentBuilds = @($currentCalls | Where-Object { $_.argv[0] -eq "build" })
            if ($currentBuilds.Count -ne 2 -or $currentBuilds[0].argv[-1] -cne "./cmd/mcphub-launcher" -or
                $currentBuilds[1].argv[-1] -cne "./cmd/mcphub") { throw "current actor changed executable roles" }
            foreach ($build in $currentBuilds) {
                $tagAt = [Array]::IndexOf([string[]]$build.argv, "-tags")
                $flagsAt = [Array]::IndexOf([string[]]$build.argv, "-ldflags")
                if ($build.argv.Count -ne 9 -or $tagAt -lt 0 -or $build.argv[$tagAt + 1] -cne "test_state_path_env" -or $build.goflags -cne "-mod=readonly" -or
                    $flagsAt -lt 0 -or -not ([string]$build.argv[$flagsAt + 1]).Contains("compiledWindowsProductRegistryBase64=$registryBase64")) {
                    throw "both current actor images require the exact test tag and original V4 registry"
                }
            }
            $currentProductFlags = [string]$currentBuilds[1].argv[[Array]::IndexOf([string[]]$currentBuilds[1].argv, "-ldflags") + 1]
            if (-not $currentProductFlags.Contains("main.buildDate=$releaseBuildDate")) {
                throw "current actor compiler argv split the original ISO build date"
            }
            $actorFiles = @{ canonical_entry_sha256 = "mcphub.exe"; product_sha256 = "mcphub-product/mcphub.exe";
                identity_sha256 = "mcphub-product/windows-product-identity.json" }
            foreach ($field in $actorFiles.Keys) {
                if ($actor.entry_set.$field -cne (Get-FileHash -LiteralPath (Join-Path $currentOutput $actorFiles[$field]) -Algorithm SHA256).Hash.ToLowerInvariant()) {
                    throw "current actor fixture does not bind the actual $field"
                }
            }
            if ($actor.entry_set.source_epoch -cne ("a" * 64) -or $actor.entry_set.product_root -cne $currentOutput -or
                $actor.runtime_payload_sha256 -cne (Get-FileHash -LiteralPath (Join-Path $currentOutput "mcphub-product/runtime-payload.json") -Algorithm SHA256).Hash.ToLowerInvariant() -or
                -not (Test-Path -LiteralPath (Join-Path $currentOutput "trash/zz_product_versioninfo.syso")) -or
                (Test-Path -LiteralPath (Join-Path $fixtureRoot "cmd/mcphub/zz_product_versioninfo.syso"))) {
                throw "current actor lost its actual Source/runtime binding or original resource disposition"
            }
            $env:MCPHUB_FAKE_RUNTIME_MISMATCH = "1"
            $badRuntimeOutput = Join-Path $privateOutputsRoot "foreign-runtime"
            $env:MCPHUB_FAKE_GO_LOG = Join-Path $privateOutputsRoot "foreign-runtime.jsonl"
            & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") @privateCommon `
                -CurrentWindowsPackage -CurrentStateIsolatedTestProduct -RuntimeArchiveDirectory $runtimeArchives -OutputDirectory $badRuntimeOutput *> $null
            if ($LASTEXITCODE -eq 0 -or (Test-Path -LiteralPath (Join-Path $badRuntimeOutput "private-serena-runtime-product.json"))) {
                throw "foreign runtime identity issued a current actor fixture"
            }
            if ((Test-Path -LiteralPath (Join-Path $badRuntimeOutput "trash")) -or
                -not (Test-Path -LiteralPath (Join-Path $badRuntimeOutput "retained-build-inputs/zz_product_versioninfo.syso"))) {
                throw "failed current runtime admission moved its original inputs to trash"
            }
            $env:MCPHUB_FAKE_RUNTIME_MISMATCH = ""
            foreach ($invalid in @(
                @("-PrivateCLIPreparationLane", "default"),
                @("-CurrentStateIsolatedTestProduct"),
                @("-PrepareCLIPrivatePreparationInputs", "-CurrentWindowsPackage"),
                @("-PrepareCLIPrivatePreparationInputs", "-TestStateIsolatedProduct"),
                @("-PrepareCLIPrivatePreparationInputs", "-PrivateCLIPreparationLane", "DEFAULT"),
                @("-PrepareCLIPrivatePreparationInputs", "-OutputDirectory", "relative-private-output"),
                @("-PrepareCLIPrivatePreparationInputs", "-OutputDirectory", $privateOutputsRoot)
            )) {
                & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") @privateCommon -PrintPlanJson @invalid *> $null
                if ($LASTEXITCODE -eq 0) { throw "private/current mode accepted invalid operands: $($invalid -join ' ')" }
            }
            $env:GOFLAGS = "-tags=ambient_should_not_apply"
            & $pwsh -NoProfile -File (Join-Path $fixtureRoot "build.ps1") @privateCommon -PrintPlanJson `
                -PrepareCLIPrivatePreparationInputs -OutputDirectory (Join-Path $privateOutputsRoot "ambient-flags") *> $null
            if ($LASTEXITCODE -eq 0) { throw "private publisher producer accepted ambient compiler overrides" }
        } finally {
            foreach ($name in $privateEnvironment.Keys) { [Environment]::SetEnvironmentVariable($name, $privateEnvironment[$name], "Process") }
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
        $legacyPreserved = Join-Path $fixtureRoot "preserved-legacy-cleanup"
        $partialResourcesBefore = if (Test-Path -LiteralPath $legacyPreserved) {
            @(Get-ChildItem -LiteralPath $legacyPreserved -File -Filter "*-zz_product_versioninfo.syso").Count
        } else { 0 }
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
        if (@(Get-ChildItem -LiteralPath $legacyPreserved -File -Filter "*-zz_product_versioninfo.syso").Count -ne $partialResourcesBefore + 1) {
            throw "DATA cleanup did not preserve the failed legacy generator's original resource"
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
}

$plan = $json | ConvertFrom-Json

if ($plan.schemaVersion -ne 3) { throw "schemaVersion=$($plan.schemaVersion), want 3" }
if ($plan.artifacts.Count -ne 2) { throw "artifact count=$($plan.artifacts.Count), want 2" }

$expected = @(
    [pscustomobject]@{ role = "product"; filename = "mcphub-product/mcphub.exe"; source = "./cmd/mcphub"; subsystem = 2; productMetadata = $true; npmPayload = $true; npmBin = $false }
    [pscustomobject]@{ role = "launcher"; filename = "mcphub.exe"; source = "./cmd/mcphub-launcher"; subsystem = 3; productMetadata = $false; npmPayload = $true; npmBin = $false }
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

if ($plan.entrySetBuildID -notmatch '^[0-9a-f]{32}$' -or $plan.canonicalEntry.received -or
    $null -ne $plan.canonicalEntry.expectedCanonicalEntrySHA256 -or
    ($plan.canonicalEntry.buildOrder -join "|") -cne "launcher|product" -or
    $plan.canonicalEntry.reverseProjection -cne "../mcphub.exe") {
    throw "PrintPlan must expose one build ID and unreceived CUI digest"
}
if ($plan.artifacts[1].productMetadata -or $null -ne $plan.artifacts[1].versionInfo) {
    throw "CUI launcher must not receive Product VERSIONINFO"
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
        "run|./tools/windows-product-validate|--candidate|bin/mcphub-product/mcphub.exe|--expected-version|$($plan.version)|--expected-commit|$($plan.commit)|--expected-build-date|$planBuildDate") {
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
$nodeMinimumMatch = [regex]::Match($publishWorkflow, '(?m)^\s*PUBLISH_NODE_MIN_VERSION:\s*''(?<version>\d+\.\d+\.\d+)''\s*$')
if (-not $nodeMinimumMatch.Success -or
    $publishWorkflow -notmatch '(?m)^\s*node-version:\s*''>=\$\{\{\s*env\.PUBLISH_NODE_MIN_VERSION\s*\}\}''\s*$' -or
    $publishWorkflow -notmatch "(?m)^\s*package-manager-cache:\s*false\s*$") {
    throw "npm publish workflow must select Node using the declared minimum with package-manager caching disabled"
}
$goModule = Get-Content -Raw -LiteralPath (Join-Path $repoRoot 'go.mod')
$goMinimumMatch = [regex]::Match($goModule, '(?m)^go[ \t]+(?<version>\d+\.\d+\.\d+)[ \t]*\r?$')
if (-not $goMinimumMatch.Success -or
    $publishWorkflow -notmatch "(?m)^\s*go-version:\s*'>=$([regex]::Escape($goMinimumMatch.Groups['version'].Value))'\s*$") {
    throw "npm publish workflow must select Go using the module's declared minimum"
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
$toolchainProgramMatch = [regex]::Match($toolchainStep, '(?ms)^ {10}node - "\$NODE_VERSION" "\$NPM_VERSION" <<''NODE''\r?\n(?<program>.*?)^ {10}NODE[ \t]*\r?$')
if (-not $toolchainProgramMatch.Success -or
    $toolchainStep -notmatch '(?m)^ {10}NODE_VERSION="\$\(node --version\)" \|\| \{ echo "::error::could not read Node version"; exit 1; \}' -or
    $toolchainStep -notmatch '(?m)^ {10}NPM_VERSION="\$\(npm --version\)" \|\| \{ echo "::error::could not read npm version"; exit 1; \}') {
    throw "npm publish workflow must read actual Node/npm versions and fail closed through its minimum-version gate"
}
$toolchainProgram = [regex]::Replace($toolchainProgramMatch.Groups['program'].Value, '(?m)^ {10}', '')
$toolchainNode = Get-Command node -CommandType Application -ErrorAction Stop | Select-Object -First 1
function Test-PublishingToolchainGate {
    param(
        [string[]]$Versions,
        [string]$ExpectedError = '',
        [string]$NodeMinimum = $nodeMinimumMatch.Groups['version'].Value
    )
    # Expected rejection diagnostics are captured; their nonzero exit remains required.
    $ErrorActionPreference = 'Continue'
    # JSON preserves empty arguments even under Windows PowerShell 5.1.
    $versionArguments = ConvertTo-Json -InputObject @($Versions) -Compress
    $minimumArgument = ConvertTo-Json -InputObject $NodeMinimum -Compress
    $testProgram = "process.env.PUBLISH_NODE_MIN_VERSION = $minimumArgument;`nprocess.argv = [process.argv[0], '-', ...$versionArguments];`n$toolchainProgram"
    $diagnostic = ($testProgram | & $toolchainNode.Source - 2>&1 | Out-String)
    $exitCode = $LASTEXITCODE
    if (($ExpectedError -eq '' -and $exitCode -ne 0) -or
        ($ExpectedError -ne '' -and ($exitCode -eq 0 -or $diagnostic -notmatch [regex]::Escape('::error::' + $ExpectedError)))) {
        throw "Trusted Publishing minimum-version gate returned an unexpected exit or diagnostic: $diagnostic"
    }
}
Test-PublishingToolchainGate -Versions @('v22.14.0', '11.5.1')
Test-PublishingToolchainGate -Versions @('v24.21.0', '11.19.0')
Test-PublishingToolchainGate -Versions @('v26.12.1', '11.11.0')
Test-PublishingToolchainGate -Versions @('v100.0.0', '100.0.0')
Test-PublishingToolchainGate -Versions @('v22.14.0+build.1', '11.5.2-beta.1')
foreach ($versions in @(@('v22.13.99', '11.5.1'), @('v22.9.0', '11.5.1'), @('v22.14.0-rc.1', '11.5.1'), @('v0.0.0', '11.5.1'))) {
    Test-PublishingToolchainGate -Versions $versions -ExpectedError 'Node 22.14.0 or newer is required'
}
foreach ($versions in @(@('v22.14.0', '11.5.0'), @('v22.14.0', '11.5.1-rc.1'), @('v22.14.0', '0.0.0'))) {
    Test-PublishingToolchainGate -Versions $versions -ExpectedError 'npm 11.5.1 or newer is required'
}
foreach ($value in @('', '22', '22.14', '022.14.0', '22.14.0-01', '22.14.0 trailing')) {
    Test-PublishingToolchainGate -Versions @($value, '11.5.1') -ExpectedError 'Node version is missing or malformed'
}
Test-PublishingToolchainGate -Versions @('v22.14.0', '') -ExpectedError 'npm version is missing or malformed'
Test-PublishingToolchainGate -Versions @() -ExpectedError 'Node version is missing or malformed'
Test-PublishingToolchainGate -Versions @('v22.14.0') -ExpectedError 'npm version is missing or malformed'
Test-PublishingToolchainGate -Versions @('v24.21.0', '11.19.0') -NodeMinimum '' -ExpectedError 'Node minimum is missing or malformed'
Test-PublishingToolchainGate -Versions @('v24.21.0', '11.19.0') -NodeMinimum '22.14' -ExpectedError 'Node minimum is missing or malformed'
if ($publishWorkflow -notmatch "(?s)push:\s*\n\s*tags:\s*\n\s*- 'v\*\.\*\.\*'" -or
    $publishWorkflow -notmatch "(?s)workflow_dispatch:.+dry_run:.+default:\s*true" -or
    $publishWorkflow -notmatch "\^\[0-9\]\+\\\.\[0-9\]\+\\\.\[0-9\]\+" -or
    $publishWorkflow -notmatch 'case "\$V" in \*-\*\) TAG="--tag beta"' -or
    @([regex]::Matches($publishWorkflow, 'npm publish "\$(?:PLATFORM|META)_ARCHIVE" --provenance --access public')).Count -ne 2) {
    throw "npm publish workflow must preserve trigger, dry-run, semver, beta-tag, and provenance behavior"
}
$publishStep = $publishWorkflow.Substring($publishStepStart)
$platformPublish = [regex]::Match($publishStep, '(?m)^ {10}npm publish "\$PLATFORM_ARCHIVE" --provenance --access public \$TAG \$DRY[ \t]*\r?$')
$metaPublish = [regex]::Match($publishStep, '(?m)^ {10}npm publish "\$META_ARCHIVE" --provenance --access public \$TAG \$DRY[ \t]*\r?$')
$archiveChecks = [regex]::Match($publishStep, '(?m)^ {10}verify_archive "\$PLATFORM_ARCHIVE" "applicate2628-mcp-local-hub-win32-x64-\$V\.tgz" "\$PLATFORM_ARCHIVE_SHA256"\r?\n {10}verify_archive "\$META_ARCHIVE" "mcp-local-hub-\$V\.tgz" "\$META_ARCHIVE_SHA256"[ \t]*\r?$')
$metaChecks = [regex]::Matches($publishStep, '(?m)^ {10}verify_archive "\$META_ARCHIVE" "mcp-local-hub-\$V\.tgz" "\$META_ARCHIVE_SHA256"[ \t]*\r?$')
if ($publishStep -notmatch '(?m)^ {8}shell: bash[ \t]*\r?$' -or
    $publishStep -match '\|\|\s*true' -or -not $archiveChecks.Success -or
    -not $platformPublish.Success -or -not $metaPublish.Success -or
    $archiveChecks.Index -ge $platformPublish.Index -or $metaChecks.Count -ne 2 -or
    $metaChecks[1].Index -le $platformPublish.Index -or $metaChecks[1].Index -ge $metaPublish.Index) {
    throw "npm publish workflow must preflight both archives and recheck the pending meta after the platform publish"
}
if ($publishWorkflow -match [regex]::Escape("go generate ./cmd/mcphub")) {
    throw "npm publish workflow must not call the retired go:generate resource path"
}
if ($publishWorkflow -notmatch 'COMMIT="\$\(git rev-parse HEAD\)"') {
    throw "npm publish workflow must capture the full Windows release commit"
}
$packageSetStep = [regex]::Match($publishWorkflow, '(?ms)^ {6}- name: Set package versions \+ regenerate platform packages\r?\n.*?(?=^ {6}- name:|\z)').Value
$targetAdmissionPattern = '(?m)^ {10}TARGETS="\$\(node -e "process\.stdout\.write\(Object\.keys\(require\(''\./npm/lib/platform-binary''\)\.PACKAGE_BY_PLATFORM\)\.join\('' ''\)\)"\)"\r?\n {10}if \[ "\$TARGETS" != "win32-x64" \]; then\r?\n {12}echo "::error::[^"\r\n]*"; exit 1\r?\n {10}fi[ \t]*\r?$'
if ($packageSetStep -notmatch $targetAdmissionPattern) {
    throw "npm publish workflow must publish only the admitted Windows x64 catalog target"
}
$nativeBuildStep = [regex]::Match($publishWorkflow, '(?ms)^ {6}- name:[^\r\n]*\r?\n {8}id: native[ \t]*\r?\n.*?(?=^ {6}- name:|\z)').Value
$fetchPlan = [regex]::Match($nativeBuildStep, '(?m)^ {12}go run \./tools/windows-product-validate --runtime-fetch-plan \\\r?\n {14}--source-root "\$SOURCE_EXPORT" \\\r?\n {14}--source-snapshot "\$SOURCE_SNAPSHOT" \\\r?\n {14}--expected-source-epoch "\$SOURCE_EPOCH"\r?\n {10}\) > "\$RUNTIME_PLAN"[ \t]*\r?$')
$fetchProgram = $nativeBuildStep.IndexOf('RUNTIME_PLAN="$RUNTIME_PLAN" RUNTIME_ARCHIVES=')
$planEncoding = $nativeBuildStep.IndexOf("!raw.equals(Buffer.from(JSON.stringify(plan) + '\n'))")
$downloadLoop = $nativeBuildStep.IndexOf('for (const archive of archives) await download(archive);')
if (-not $fetchPlan.Success -or $fetchProgram -le $fetchPlan.Index -or
    $planEncoding -le $fetchProgram -or $downloadLoop -le $planEncoding) {
    throw "npm publish workflow must obtain and verify the compact runtime plan from the original prepared source before acquisition"
}
$directWindowsBuildPattern = '(?m)^ {10}\(\r?\n {12}cd "\$SOURCE_EXPORT"\r?\n {12}pwsh -NoProfile -File \./build\.ps1 \\\r?\n {14}-CurrentWindowsPackage \\\r?\n {14}-WindowsTargetArch amd64 \\\r?\n {14}-OutputDirectory "\$PLATFORM_STAGE/bin" \\\r?\n {14}-ReleaseVersion "\$V" \\\r?\n {14}-ReleaseCommit "\$COMMIT" \\\r?\n {14}-ReleaseBuildDate "\$DATE" \\\r?\n {14}-SourceSnapshotPath "\$SOURCE_SNAPSHOT" \\\r?\n {14}-RuntimeArchiveDirectory "\$RUNTIME_ARCHIVES"\r?\n {10}\)[ \t]*\r?$'
if ($nativeBuildStep -notmatch '(?m)^ {8}shell: bash[ \t]*\r?$' -or
    $nativeBuildStep -notmatch $directWindowsBuildPattern) {
    throw "npm publish workflow must run the snapshot-bound Windows V4 build as an unmasked Bash subshell"
}
$platformPackStep = [regex]::Match($publishWorkflow, '(?ms)^ {6}- name:[^\r\n]*\r?\n {8}id: platform_pack[ \t]*\r?\n.*?(?=^ {6}- name:|\z)').Value
$packValidationPattern = '(?m)^ {12}go run \./tools/windows-product-validate \\\r?\n {14}--candidate "\$PLATFORM_STAGE/bin/mcphub-product/mcphub\.exe" \\\r?\n {14}--helper "\$PLATFORM_STAGE/bin/mcphub\.exe" \\\r?\n {14}--expected-version "\$VERSION" \\\r?\n {14}--expected-commit "\$COMMIT" \\\r?\n {14}--expected-build-date "\$BUILD_DATE" \\\r?\n {14}--expected-source-epoch "\$SOURCE_EPOCH" \\\r?\n {14}--runtime-payload runtime-payload\.json \\\r?\n {14}--pack "\$ARCHIVE" \\\r?\n {14}--pack-source-root "\$SOURCE_EXPORT" \\\r?\n {14}--pack-source-snapshot "\$SOURCE_SNAPSHOT"[ \t]*\r?$'
if ($platformPackStep -notmatch $packValidationPattern -or $publishWorkflow -match 'mcphub-pe-admit') {
    throw "npm publish workflow must use the repository product validator on the actual Windows V4 archive and original prepared source"
}
$metaPackStep = [regex]::Match($publishWorkflow, '(?ms)^ {6}- name:[^\r\n]*\r?\n {8}id: meta_pack[ \t]*\r?\n.*?(?=^ {6}- name:|\z)').Value
$metaValidationPattern = '(?m)^ {12}go run \./tools/windows-product-validate --meta-pack "\$ARCHIVE" \\\r?\n {14}--pack-source-root "\$SOURCE_EXPORT" \\\r?\n {14}--pack-source-snapshot "\$SOURCE_SNAPSHOT" \\\r?\n {14}--expected-source-epoch "\$SOURCE_EPOCH" \\\r?\n {14}--expected-version "\$VERSION"\r?\n {10}\) > "\$PACK_OWNER/verification\.json"[ \t]*\r?$'
$metaEncoding = $metaPackStep.IndexOf("!raw.equals(Buffer.from(JSON.stringify(receipt) + '\n'))")
$metaDigest = $metaPackStep.IndexOf('process.stdout.write(receipt.archive_sha256)')
if ($metaPackStep -notmatch $metaValidationPattern -or $metaEncoding -lt 0 -or $metaDigest -le $metaEncoding) {
    throw "npm publish workflow must check the compact meta verifier receipt before forwarding its digest"
}


$encodingProbe = Join-Path $scratchRoot "windows-build-json-encoding-$PID-$([Guid]::NewGuid().ToString('N'))"
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


function Get-PESubsystem([string]$Path) {
    $bytes = [IO.File]::ReadAllBytes($Path)
    $peOffset = [BitConverter]::ToInt32($bytes, 0x3c)
    return [BitConverter]::ToUInt16($bytes, $peOffset + 24 + 68)
}

$compileRoot = Join-Path $scratchRoot "windows-linker-probe-$PID-$([Guid]::NewGuid().ToString('N'))"
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


# Success-only disposition is deliberately after the final assertion. Every
# unexpected failure preserves all four original fixture roots for receiving.
$fixtureTrash = Join-Path $scratchRoot "trash"
if (-not (Test-Path -LiteralPath $fixtureTrash)) { New-Item -ItemType Directory -Path $fixtureTrash -ErrorAction Stop | Out-Null }
foreach ($ownedFixture in @($fixtureRoot, $privateOutputsRoot, $encodingProbe, $compileRoot)) {
    if (Test-Path -LiteralPath $ownedFixture) {
        $destination = Join-Path $fixtureTrash (Split-Path -Leaf $ownedFixture)
        if (Test-Path -LiteralPath $destination) { throw "fixture trash destination already exists" }
        Move-Item -LiteralPath $ownedFixture -Destination $destination -ErrorAction Stop
    }
}

Write-Output "PASS: Windows build plan has one GUI-subsystem product, repository-only admission, and a non-executable identity manifest."

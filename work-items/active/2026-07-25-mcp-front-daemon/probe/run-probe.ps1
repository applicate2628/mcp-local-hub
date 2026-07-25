<#
.SYNOPSIS
  Increment-1 north-star survival probe: mcphub route keeps forwarding
  /serena/mcp tool-calls after the GUI process dies.

.DESCRIPTION
  See README.md in this directory for the full narrative + hard safety
  rules. This script is a consolidated, reusable version of the manual
  sequence run during the Increment-1 F1/F2/F3 review-response session
  (2026-07-25); see transcript-2026-07-25.md for that session's captured
  output.

  HARD SAFETY (do not weaken):
    - Every mcphub-shaped process is launched via Start-Process
      -WindowStyle Hidden with redirected stdout/stderr, never inline.
    - Every kill first verifies the target PID's on-disk exe path equals
      the exact probe binary this script itself launched, and refuses
      (does not kill) on any mismatch.
    - USERPROFILE / LOCALAPPDATA / MCPHUB_STATE_DIR_OVERRIDE are always
      redirected to $ScratchDir before anything is launched.

.PARAMETER RepoRoot
  Path to the mcp-local-hub worktree to build the probe binary from.

.PARAMETER ScratchDir
  A fresh (will be created) directory for all probe state, logs, and
  binaries. Must NOT be the operator's real profile/state dir.
#>
param(
    [Parameter(Mandatory = $true)][string]$RepoRoot,
    [Parameter(Mandatory = $true)][string]$ScratchDir
)

$ErrorActionPreference = 'Stop'

function Assert-IdentityAndKill($ProcId, $ExpectedPath, $Label) {
    $proc = Get-Process -Id $ProcId -ErrorAction SilentlyContinue
    if ($null -eq $proc) {
        Write-Host "[$Label] pid $ProcId already gone"
        return
    }
    if ($proc.Path -ne $ExpectedPath) {
        Write-Host "[$Label] IDENTITY MISMATCH: pid $ProcId path is $($proc.Path), expected $ExpectedPath. REFUSING to kill."
        return
    }
    Write-Host "[$Label] identity verified (pid $ProcId == $ExpectedPath); killing."
    Stop-Process -Id $ProcId -Force
}

# --- 1. Scratch layout -------------------------------------------------
$bin        = Join-Path $ScratchDir 'bin'
$fakeDir    = Join-Path $ScratchDir 'fake-daemon'
$home       = Join-Path $ScratchDir 'home'
$appdata    = Join-Path $ScratchDir 'appdata'
$state      = Join-Path $ScratchDir 'state'
$logs       = Join-Path $ScratchDir 'logs'
$workspace  = Join-Path $ScratchDir 'workspace\MyProject'
New-Item -ItemType Directory -Force -Path $bin, $fakeDir, $home, $appdata, $state, $logs, "$workspace\src", "$workspace\.serena" | Out-Null
Set-Content -Path "$workspace\src\main.go" -Value "package main"
Set-Content -Path "$workspace\.serena\project.yml" -Value "project_name: MyProject"

# --- 2. Build the real probe binary + fixtures -------------------------
Push-Location $RepoRoot
try {
    & go build -trimpath -ldflags "-X main.version=probe -X main.commit=probe -X main.buildDate=probe -H windowsgui" `
        -tags test_state_path_env -o "$bin\mcphub-probe.exe" ./cmd/mcphub
    if ($LASTEXITCODE -ne 0) { throw "build mcphub-probe.exe failed" }
} finally {
    Pop-Location
}

Push-Location (Join-Path $RepoRoot 'work-items\active\2026-07-25-mcp-front-daemon\probe\_fixtures\fake_daemon')
try {
    & go build -o "$fakeDir\fake-daemon.exe" .
    if ($LASTEXITCODE -ne 0) { throw "build fake-daemon.exe failed" }
} finally {
    Pop-Location
}

# --- 3. Register the probe workspace -----------------------------------
$regPath = Join-Path $appdata 'mcp-local-hub\workspaces.yaml'
New-Item -ItemType Directory -Force -Path (Split-Path $regPath) | Out-Null
Push-Location $RepoRoot
try {
    & go run ./work-items/active/2026-07-25-mcp-front-daemon/probe/_fixtures/register_workspace `
        -registry $regPath -workspace-path $workspace -port 19301
    if ($LASTEXITCODE -ne 0) { throw "register_workspace failed" }
} finally {
    Pop-Location
}

# --- 4. Launch fake-daemon + GUI + route, all detached + redirected ----
$env:USERPROFILE               = $home
$env:LOCALAPPDATA              = $appdata
$env:MCPHUB_STATE_DIR_OVERRIDE = $state
$env:MCPHUB_E2E_SUPERVISOR     = 'none'
$env:MCPHUB_E2E_SCHEDULER      = 'none'
$env:MCPHUB_NO_CONSOLE_ATTACH  = '1'

$fakeProc  = Start-Process -FilePath "$fakeDir\fake-daemon.exe" -ArgumentList '-port', '19301' `
    -WindowStyle Hidden -PassThru -RedirectStandardOutput "$logs\fake.out.log" -RedirectStandardError "$logs\fake.err.log"
$guiProc   = Start-Process -FilePath "$bin\mcphub-probe.exe" -ArgumentList 'gui', '--no-browser', '--no-tray', '--port', '19125' `
    -WindowStyle Hidden -PassThru -RedirectStandardOutput "$logs\gui.out.log" -RedirectStandardError "$logs\gui.err.log"
$routeProc = Start-Process -FilePath "$bin\mcphub-probe.exe" -ArgumentList 'route', '--port', '19137' `
    -WindowStyle Hidden -PassThru -RedirectStandardOutput "$logs\route.out.log" -RedirectStandardError "$logs\route.err.log"

Start-Sleep -Seconds 2
$probeExe = (Resolve-Path "$bin\mcphub-probe.exe").Path
$fakeExe  = (Resolve-Path "$fakeDir\fake-daemon.exe").Path

# --- 5. Forwarded tool-call, both ports, GUI alive ---------------------
$toolFile = Join-Path $workspace 'src\main.go'
$argsObj  = @{ relative_path = $toolFile } | ConvertTo-Json -Compress
$body1    = '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"find_symbol","arguments":' + $argsObj + '}}'
$body2    = '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"find_symbol","arguments":' + $argsObj + '}}'

function Invoke-ToolCall($Port, $Body) {
    try {
        $r = Invoke-WebRequest -Uri ("http://127.0.0.1:{0}/serena/mcp" -f $Port) -Method Post -Body $Body -ContentType 'application/json' -UseBasicParsing -TimeoutSec 10
        return [PSCustomObject]@{ Ok = $true; Status = [int]$r.StatusCode; Body = $r.Content }
    } catch {
        return [PSCustomObject]@{ Ok = $false; Status = $null; Body = $_.Exception.Message }
    }
}

$before19125 = Invoke-ToolCall 19125 $body1
$before19137 = Invoke-ToolCall 19137 $body1
Write-Host "BEFORE kill — GUI(19125): $($before19125.Status) $($before19125.Body)"
Write-Host "BEFORE kill — route(19137): $($before19137.Status) $($before19137.Body)"

# --- 6. Kill ONLY the GUI PID, identity-gated --------------------------
Assert-IdentityAndKill $guiProc.Id $probeExe 'gui'
Start-Sleep -Milliseconds 800

# --- 7. Repeat on the route port; confirm GUI port refused -------------
$after19137 = Invoke-ToolCall 19137 $body2
$after19125 = Invoke-ToolCall 19125 $body2
Write-Host "AFTER kill — route(19137): $($after19137.Status) $($after19137.Body)"
Write-Host "AFTER kill — GUI(19125): ok=$($after19125.Ok) detail=$($after19125.Body)"

# --- 8. Verdict ---------------------------------------------------------
$pass = $before19125.Status -eq 200 -and $before19137.Status -eq 200 -and $after19137.Status -eq 200 -and (-not $after19125.Ok)
Write-Host ""
if ($pass) {
    Write-Host "PROBE RESULT: PASS — route daemon forwarded the SAME real tool-call after the GUI died; GUI port refused."
} else {
    Write-Host "PROBE RESULT: FAIL — see the BEFORE/AFTER lines above."
}

# --- 9. Cleanup (identity-gated) ----------------------------------------
Assert-IdentityAndKill $routeProc.Id $probeExe 'route'
Assert-IdentityAndKill $fakeProc.Id $fakeExe 'fake-daemon'

if (-not $pass) { exit 1 }

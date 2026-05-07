<#
.SYNOPSIS
    Find and (optionally) terminate orphaned log-watcher processes left
    behind by exited agent shells (Claude Code, codex CLI, etc.) on Windows.

.DESCRIPTION
    Background brief: d:\dev\orphaned-log-watchers-report.md.

    Agent CLIs (Claude Code's shell-snapshot launcher and similar) start
    pipelines like `tail -F <log> | grep --line-buffered <pattern>` to
    monitor long-running optimization runs. When the parent agent session
    exits or loses the process handle, the inner `bash.exe -> tail.exe ->
    grep.exe` tree gets reparented and runs forever, accumulating into
    100+ stale processes that cause workstation lag.

    This utility is a STOPGAP. The proper fix is in each agent CLI's
    process launcher (Job Object on Windows, recursive process-tree
    cleanup on session close). Until those land, this script lets an
    operator audit and reclaim the watcher trees.

    Defaults are conservative. Without -Apply, only a dry-run report is
    printed; nothing is killed. The detection rules require BOTH a
    process-name match AND a command-line shape match, plus an explicit
    deny-list excludes user-active processes (em.exe, python.exe,
    Antigravity, VS Code, Cursor, generic Codex sessions, mcphub itself).

.PARAMETER Apply
    Actually kill the matched processes. Without this flag, the script
    runs in dry-run mode and prints what it WOULD kill.

.PARAMETER Yes
    Skip the interactive confirmation prompt under -Apply. Required for
    non-interactive (scripted) use; otherwise the prompt blocks waiting
    for user input.

.PARAMETER IncludeLive
    By default the kill phase only terminates processes whose parent is
    NO LONGER in the snapshot — i.e. true orphans. Without this flag,
    path-matched processes whose parent is still alive are listed in
    the dry-run report (so the operator can see them) but skipped in
    Apply mode. They almost always represent CURRENT active agent
    sessions, not zombies.

    Set -IncludeLive to also kill path-matched processes with live
    parents. Use only when the operator confirms those parents are
    themselves stale agents that should die.

.PARAMETER ScratchPathPattern
    Regex on CommandLine for path-matched watchers. Defaults to a broad
    `.scratch.*\.(log|txt)` pattern that catches typical agent watcher
    pipelines across projects (layered-filter, mcp-local-hub, etc.).
    Override to narrow or broaden.

.PARAMETER OrphanGrepRegex
    Regex of recognizable watcher tokens in `grep.exe` command lines, to
    detect orphan greps whose CommandLine no longer carries the watched
    log path (the `tail | grep` pattern strips it on the grep side).
    Defaults pulled from the report's Observed Evidence section.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File scripts\cleanup-orphan-watchers.ps1
    Dry-run; prints what would be killed.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File scripts\cleanup-orphan-watchers.ps1 -Apply
    Interactive: prints, asks for confirmation, then kills.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File scripts\cleanup-orphan-watchers.ps1 -Apply -Yes
    Non-interactive: kills without prompting. For scheduled / scripted use.

.NOTES
    Background brief: d:\dev\orphaned-log-watchers-report.md.
    Acceptance criteria, non-goals, and reproduction steps live there.
#>

[CmdletBinding()]
param(
    [switch]$Apply,
    [switch]$Yes,
    [switch]$IncludeLive,
    [string]$ScratchPathPattern = '\.scratch[\\/].*\.(log|txt)',
    [string]$OrphanGrepRegex = 'BOBYQA|Traceback|Done|early_kill|S11|Sonnet|AXIEM|ERROR|Error|stage_|tick'
)

$ErrorActionPreference = 'Stop'

# Process names whose command-line we'll inspect for the watcher shape.
# Limited to shell + tail + grep so we never scan unrelated processes.
$WatcherNames = @('bash.exe', 'sh.exe', 'tail.exe', 'grep.exe')

# Explicit deny list. Even if a process matches the watcher heuristics
# (which it shouldn't given $WatcherNames above), refuse to kill these.
# Belt-and-suspenders against future broadening of the match rule.
$NeverKill = @(
    'em.exe',           # Sonnet EM solver (active simulation runs)
    'python.exe',       # active optimization runners
    'pythonw.exe',
    'mcphub.exe',       # this project's daemon
    'codex.exe',        # active codex sessions
    'claude.exe',       # claude CLI
    'cursor.exe',       # Cursor editor
    'code.exe',         # VS Code
    'antigravity.exe',
    'powershell.exe',
    'pwsh.exe',
    'cmd.exe'
)

Write-Host '== Orphan-watcher cleanup ==' -ForegroundColor Cyan
Write-Host ('Mode:        ' + $(if ($Apply) { 'APPLY (will kill)' } else { 'dry-run' }))
Write-Host ('Path regex:  ' + $ScratchPathPattern)
Write-Host ('Grep regex:  ' + $OrphanGrepRegex)
Write-Host ''

# Snapshot all processes once. We compute orphan-status by checking
# whether each ProcessId's parent is still in the same snapshot — using
# a live Get-Process per row would race with process exits.
$procs = Get-CimInstance -ClassName Win32_Process |
    Select-Object Name, ProcessId, ParentProcessId, CreationDate, ExecutablePath, CommandLine

$idTable = @{}
foreach ($p in $procs) { $idTable[[int]$p.ProcessId] = $true }

# Pass 1: process-name + path-shape match. Catches the typical
# `tail.exe -F <log>` or `bash.exe -c '... tail ... grep ...'` pattern.
$pathMatched = $procs | Where-Object {
    $WatcherNames -contains $_.Name -and
    $_.CommandLine -match $ScratchPathPattern
}

# Pass 2: orphan grep.exe. grep's command-line typically contains only
# the regex pattern (no path), so a name-only match would over-fire on
# legitimate grep usage. Guard with: parent-PID-not-in-snapshot AND
# command-line carries one of the watcher regex tokens AND the parent
# isn't 0 (PID 0 is the system idle process; not informative).
$orphanGreps = $procs | Where-Object {
    $_.Name -eq 'grep.exe' -and
    $_.ParentProcessId -ne 0 -and
    -not $idTable.ContainsKey([int]$_.ParentProcessId) -and
    $_.CommandLine -match $OrphanGrepRegex
}

# Union and de-dup by PID.
$candidates = @{}
foreach ($p in @($pathMatched) + @($orphanGreps)) {
    if ($null -ne $p) { $candidates[[int]$p.ProcessId] = $p }
}

# Apply NeverKill deny list.
$filtered = $candidates.Values | Where-Object { $NeverKill -notcontains $_.Name }

if (-not $filtered -or $filtered.Count -eq 0) {
    Write-Host 'No orphan watchers found.' -ForegroundColor Green
    exit 0
}

# Compute parent-alive status and age for the report. IsKillTarget is
# the actual kill gate: true orphans (ParentAlive=no) always; path-matched
# live-parent processes only when -IncludeLive is set.
$now = Get-Date
$rows = foreach ($p in $filtered) {
    $parentAlive = $idTable.ContainsKey([int]$p.ParentProcessId)
    $age = if ($p.CreationDate) { ($now - $p.CreationDate).ToString('d\.hh\:mm\:ss') } else { '?' }
    $killTarget = (-not $parentAlive) -or $IncludeLive
    [pscustomobject]@{
        PID          = $p.ProcessId
        ParentPID    = $p.ParentProcessId
        ParentAlive  = if ($parentAlive) { 'yes' } else { 'NO (orphan)' }
        IsKillTarget = $killTarget
        Name         = $p.Name
        Age          = $age
        CmdExcerpt   = if ($p.CommandLine) { $p.CommandLine.Substring(0, [Math]::Min(120, $p.CommandLine.Length)) } else { '' }
    }
}

# Sort orphans first, then by age descending (oldest worst).
$rows = $rows | Sort-Object @{Expression = { if ($_.ParentAlive -like 'NO*') { 0 } else { 1 } } }, Age -Descending

$kills = $rows | Where-Object { $_.IsKillTarget }
$skips = $rows | Where-Object { -not $_.IsKillTarget }

Write-Host ('Candidates: ' + $rows.Count + ' total  |  ' + @($kills).Count + ' kill targets  |  ' + @($skips).Count + ' skipped (live parent)') -ForegroundColor Yellow
$rows | Format-Table -AutoSize -Wrap PID, ParentPID, ParentAlive, IsKillTarget, Name, Age, CmdExcerpt

if (-not $Apply) {
    Write-Host ''
    Write-Host 'Dry-run only. Re-run with -Apply to terminate orphans (parent dead).' -ForegroundColor Yellow
    Write-Host '  -Apply              : kill orphans only (interactive confirm)'
    Write-Host '  -Apply -Yes         : kill orphans only (no prompt; for scripted use)'
    Write-Host '  -Apply -IncludeLive : also kill path-matched processes with LIVE parents (current sessions)'
    exit 0
}

if (@($kills).Count -eq 0) {
    Write-Host ''
    Write-Host 'No kill targets after filter. Use -IncludeLive to include live-parent rows.' -ForegroundColor Yellow
    exit 0
}

# Confirm gate. -Yes skips it; otherwise prompt explicitly.
if (-not $Yes) {
    $reply = Read-Host -Prompt ('Kill ' + @($kills).Count + ' processes (parent dead, or -IncludeLive)? (yes/no)')
    if ($reply -ne 'yes') {
        Write-Host 'Aborted.' -ForegroundColor Yellow
        exit 0
    }
}

# Kill phase. Use Stop-Process -Force; do not use taskkill /T because we
# already have the full descendant set in $kills (path-matched +
# orphan-grep union). Killing -T from a partial root could miss reparented
# greps whose path-match wasn't recorded.
$killed = 0
$failed = 0
foreach ($row in $kills) {
    try {
        Stop-Process -Id $row.PID -Force -ErrorAction Stop
        $killed++
        Write-Host ('killed pid=' + $row.PID + ' ' + $row.Name) -ForegroundColor DarkGreen
    } catch {
        $failed++
        Write-Host ('FAILED pid=' + $row.PID + ' ' + $row.Name + ': ' + $_.Exception.Message) -ForegroundColor Red
    }
}

Write-Host ''
Write-Host ('Killed: ' + $killed + '  Failed: ' + $failed) -ForegroundColor Cyan
if ($failed -gt 0) { exit 4 } else { exit 0 }

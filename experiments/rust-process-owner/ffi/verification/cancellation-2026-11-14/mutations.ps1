# Negative controls on an OWNED experimental checkout. Restores source bytes
# after each probe. Never run against a shared/live installation or branch copy.
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$LabRoot,
    [Parameter(Mandatory=$true)][string]$CCompiler,
    [Parameter(Mandatory=$true)][ValidatePattern('^[a-zA-Z0-9._-]+$')][string]$Prefix
)
$ErrorActionPreference='Stop'
$ffi=(Get-Item $PSScriptRoot).Parent.Parent.FullName
$file=Join-Path $ffi 'go\finish_context_windows.go'
$bytes=[IO.File]::ReadAllBytes($file)
$original=[Text.Encoding]::UTF8.GetString($bytes)
$utf8=[Text.UTF8Encoding]::new($false)
$lock='if o.mu.TryLock() {'
$cancel="case <-ctx.Done():`n`t`t`treturn last, finishCancellation(ctx)"
if(($original.Split([string[]]@($lock),[StringSplitOptions]::None).Length-1)-ne 1){throw 'lock anchor changed; review rather than guess'}
if(($original.Split([string[]]@($cancel),[StringSplitOptions]::None).Length-1)-ne 1){throw 'cancellation anchor changed; review rather than guess'}
$cases=@(
    @{name='blocking-lock';source=$original.Replace($lock,'if func() bool { o.mu.Lock(); return true }() {');pattern='^TestFinishContextDoesNotWaitOnBusyOwnerAfterCancel$'},
    @{name='implicit-stop';source=$original.Replace('return last, finishCancellation(ctx)','_, _ = o.Stop(93); return last, finishCancellation(ctx)');pattern='^Test(FinishContextDeadlineRetainsSuspendedOwner|IOCanceledObserverCanResumeNormalDrainWithoutStop)$'},
    @{name='erased-observation';source=$original.Replace('return last, finishCancellation(ctx)','return Outcome{}, finishCancellation(ctx)');pattern='^TestFinishContextRootExitDoesNotImplyDescendantSettlement$'}
)
foreach($case in $cases){
    try{
        [IO.File]::WriteAllText($file,$case.source,$utf8)
        & (Join-Path $PSScriptRoot 'probe.ps1') -LabRoot $LabRoot -CCompiler $CCompiler -Name ($Prefix+'-'+$case.name) -Pattern $case.pattern -ExpectedExit 1
    }finally{[IO.File]::WriteAllBytes($file,$bytes)}
}
if(-not [Linq.Enumerable]::SequenceEqual([byte[]][IO.File]::ReadAllBytes($file),[byte[]]$bytes)){throw 'source restore failed'}
& (Join-Path $PSScriptRoot 'probe.ps1') -LabRoot $LabRoot -CCompiler $CCompiler -Name ($Prefix+'-restored') -Pattern '^Test(FinishContext|IO)' -Race

# R17 / #660: two-task native fixture proposal

**Status: statically reviewed; syntax parse passed; ordinary script launch blocked; never applied.** This folder contains one small native fixture proposal. It does not authorize Windows Task Scheduler or execution-policy changes. The proposed run needs explicit approval for the precise creation, conditional cleanup, and one-session execution policy below.

## Contents

1. [Scope and authority](#scope-and-authority)
2. [Exact input and invocation](#exact-input-and-invocation)
3. [Completed preparation and host block](#completed-preparation-and-host-block)
4. [Checks and the meaning of success](#checks-and-the-meaning-of-success)
5. [Cleanup and recovery](#cleanup-and-recovery)
6. [Evidence and limitations](#evidence-and-limitations)
7. [Primary contracts](#primary-contracts)

## Scope and authority

The approval unit is **one invocation** of `Invoke-R17HiddenFixture.ps1 -Apply` on the approved Windows machine, in the current interactive user's session. The script generates a unique 32-digit GUID (globally unique identifier), then attempts exactly two root-folder tasks: `\mcp-r17-<guid>-visible` and `\mcp-r17-<guid>-hidden`. A partial failure can leave fewer than two confirmed creations. There are no retries or replacement attempts.

Both definitions are disabled before registration, registered with `TASK_CREATE | TASK_DISABLE = 2 | 8 = 10`, and have zero triggers. The task principal is the current user's SID (security identifier), with interactive-token logon and least privilege. Demand start is disabled. The sole action names a deliberately nonexistent executable beneath `D:\dev\ChatGPT\mcp-r17-native-qualification-20261119\<guid>\`; the script never creates that executable. The two tasks differ only in name, URI (uniform resource identifier), and visibility. A unique marker containing two non-ASCII characters tests preservation of Unicode text in the returned definition.

The script requests only `status`, `export`, and `list` from the production script. **It never requests task Run, RunOwned, or Stop.** Windows PowerShell child processes execute the read requests; no fixture action is invoked. An unresolved child is recorded and prevents task cleanup. On a read timeout, the script terminates only its retained child process object, never a process selected by name.

The proposed approval covers conditional removal of these confirmed creations after an immediate ownership and definition recheck. Because ordinary script launch has been blocked, it separately requests `RemoteSigned` for this new PowerShell session and its children only, using the exact command below. No approval has yet been granted. It does not cover any existing task, installed Hub, service, scheduled run, access-control policy, persistent user/machine execution-policy change, Group Policy change, installation, network operation, or additional branch. A name collision fails CREATE; there is no UPDATE, CREATE_OR_UPDATE, `/F`, or fallback mechanism. If the approved invocation is still blocked, stop and report that boundary.

## Exact input and invocation

The source input must be the unmodified `internal/scheduler/scheduler_com_windows.go` snapshot from #660 commit `1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3`. Its exact file SHA-256 (Secure Hash Algorithm, 256-bit digest) is:

```text
e2594cbe0a0cca61038a09ea7d37a701132be378bec6b935236e8b4233ed43fa
```

The reviewed [fixture script](Invoke-R17HiddenFixture.ps1) has SHA-256 `6a7d123334ae13aaf5890a491813091a2a2a4831650d25940d1814ca40a2ee4c`. Keep it and the exact source snapshot in the authorized work area without converting line endings. The source argument must also resolve lexically inside that area. A hash mismatch stops before task creation. The script extracts the two existing Go raw-string constants, concatenates the preamble and body unchanged, and passes that code through Windows PowerShell's `-EncodedCommand` transport. Request values arrive through standard input as JSON (JavaScript Object Notation); they are never interpolated into the script.

```powershell
$fixture = 'D:\dev\ChatGPT\mcp-r17-resume-20261119\Invoke-R17HiddenFixture.ps1'
$source  = 'D:\dev\ChatGPT\mcp-r17-resume-20261119\scheduler_com_windows-1b870e04.go'

# Default path: plan only if permitted by the host. The actual check was BLOCKED.
powershell.exe -NoProfile -NonInteractive -File $fixture

# PROPOSAL ONLY: not executed; requires approval of the exact task and policy scope.
powershell.exe -ExecutionPolicy RemoteSigned -NoProfile -NonInteractive -File $fixture -SourceFile $source -Apply
```

The script has no automatic elevation. It explicitly refuses `WindowsIdentity.IsSystem`, session 0, or a noninteractive Windows station; it does not classify every possible service account. Registration uses the current identity without credentials. It requires the authorized root and parent directories to be real directories rather than junctions. It creates a new per-run evidence directory; it never overwrites a previous run directory.

The `-ExecutionPolicy` argument applies to the new session and its children without changing registry policy. Group Policy takes precedence [7, 8]. `RemoteSigned` allows locally written unsigned scripts but can still reject files marked as downloaded; no `Unblock-File`, `Bypass`, persistent policy change, or alternate execution route is proposed. This is a request for a specific exception, not an assumption that the host will permit it.

## Completed preparation and host block

On 19 November 2026, preparation checks ran on Comet. The [preparation receipt](fixture-validation.json) distinguishes the final script from the earlier version used for the blocked default invocation:

- The native PowerShell parser reported zero syntax errors, 2459 tokens, and the `Apply` parameter on the final script after the source-digest correction. `Parser.ParseFile` did not evaluate the script or access Task Scheduler.
- An ordinary `powershell.exe -NoProfile -NonInteractive -File …` invocation without `-Apply`, on the preceding script version with SHA-256 `342661c6f69bbbadaecd258fd193b033fb3378f3aa251bb92c707b58ea166e69`, was blocked before script code ran, with `FullyQualifiedErrorId=UnauthorizedAccess` and the host's scripts-disabled error. No plan JSON was produced. This is a host execution-policy denial, not an automatic approval-review rejection.
- The earlier read-only `Get-ExecutionPolicy -List` query failed while autoloading `Microsoft.PowerShell.Security`, so the effective policy value and its governing scope remain unknown. No policy change or module reinstall was performed.

Final source verification against Git blob `bbc2d73a2fa3d59c3edb206e5be48c5cbd6f0eb7` found that the first prepared source copy had one extra final LF byte. The exact Git source is 10040 bytes. The isolated copy and the fixture's source-digest literal were corrected; the production script constants were unchanged. The final syntax check and both remote file digests were repeated. The unchanged host-policy block was not retried on this new script version, and the earlier blocked run is not presented as its execution.

The fixture computes its source digest through .NET rather than that module's `Get-FileHash`. A syntax parse does not establish that the default plan, modules, registration, reads, or cleanup can execute. No `-Apply` run or native fixture observation has occurred. A further policy or module failure remains a stop condition; it does not authorize installation, repair, or an alternate executor.

## Checks and the meaning of success

**Table 1. Proposed native observations.** None has yet been executed.

| Request | Required observation |
|---|---|
| `status` of visible fixture | Exact task name; disabled native state `1`; exact successful-registration XML; same execution principal |
| `export` of visible fixture | Same exact definition and principal, including the Unicode marker |
| `status` of hidden fixture | Hidden task is found, with disabled state `1`, exact definition and same principal |
| `export` of hidden fixture | Hidden task is found; exact XML (Extensible Markup Language) and principal are preserved |
| `list` with the unique run prefix | Exactly the visible task, with its exact definition, principal and disabled state; hidden task is omitted |
| `status` of absent sibling | `ok=false`, `kind=task_absent` |
| `export` of absent sibling | `ok=false`, `kind=task_absent` |

The concrete distinction is the #660 production script's use of `GetTasks(1)` for named lookup and `GetTasks(0)` for List. COM (Component Object Model) is the native Windows interface used by that code. This test does not substitute a mock service or edit the production constants.

Exit code zero requires every observation in Table 1, settled read children, and confirmed removal of both created tasks. The result records read success independently from cleanup success. A passing read suite with retained resources is **not** a fully successful run. The wrapper's exit code alone is insufficient: inspect `result.json` and each task's cleanup status.

## Cleanup and recovery

Positive fixture ownership requires a successful CREATE result in this invocation, the unique run marker, the exact task path, and the expected execution principal. Immediately after registration, the script checks the inert definition and captures its complete XML. Before deletion it obtains the task again and verifies:

- The exact captured XML and expected principal still match.
- Both the registered task and its settings remain disabled, and native state is `1`.
- Visibility, zero triggers, interactive logon, least privilege, demand-start prohibition, marker, URI, and the sole nonexistent action remain as created.

Only then does it call native `DeleteTask` for that one exact name. It verifies absence with hidden-inclusive enumeration afterwards. The native cleanup is a fixture responsibility; it is not a test of the product's Delete implementation.

If a registration call throws, the result is unknown even if a task may have been created. No deletion is attempted for that name. If a snapshot is missing, an ownership check fails, a read child cannot be confirmed settled, or current state cannot be read, the task is preserved. A failed named read revokes cleanup authority for that fixture; a failed List check revokes it for both. Observed drift cannot be erased by a later successful reread. A deletion error or failed post-deletion observation is reported as unknown, without an automatic second delete.

**Table 2. Recovery states.**

| Saved state | Meaning and required next step |
|---|---|
| `removed` | Delete returned and hidden-inclusive enumeration found no matching task |
| `not_created` | Registration was never attempted for this name |
| `registration_unknown` | Registration was attempted but success was not confirmed; preserve and inspect the exact name |
| `retained` | Cleanup stopped before Delete; preserve the task and its evidence |
| `delete_result_unknown` / `deletion_unconfirmed` | Delete result or subsequent observation is uncertain; inspect, do not repeat automatically |
| Missing final receipt | The invocation may have been interrupted; use `intent.json` to identify only this run's names and preserve everything pending review |

After interruption or retention, **do not rerun `-Apply`**: it would propose a new pair and would not recover the old pair. Read the saved intent, result, and captured XML first. The following optional recovery inspection is read-only and prints no unrelated definitions:

```powershell
# Set this to the per-run evidence directory printed by the interrupted run.
$runDir = 'D:\dev\ChatGPT\mcp-r17-native-qualification-20261119\<guid>'
$intent = Get-Content -LiteralPath (Join-Path $runDir 'intent.json') -Raw | ConvertFrom-Json
$service = New-Object -ComObject 'Schedule.Service'
$service.Connect()
$folder = $service.GetFolder('\')
foreach ($name in $intent.names) {
    try {
        $task = $folder.GetTask($name)
        [pscustomobject]@{ name = $name; enabled = $task.Enabled; state = [int]$task.State; observed = $true }
    } catch {
        [pscustomobject]@{ name = $name; observed = $false; result = 'Unknown: preserve; inspect the error locally' }
    }
}
```

This read-only inspection does not establish deletion authority and deliberately does not map every exception to absence. Further cleanup requires review of the saved successful-creation evidence, exact current definition, principal, and disabled state. Missing or drifting evidence is a reason to keep the task, not to force deletion.

Windows Task Scheduler does not provide an atomic compare-and-delete operation. A writer can change a task after the last check. The proposal detects observed drift; it cannot prevent an uncooperative concurrent writer. If another writer is known or suspected to be changing these fixture names, suspend cleanup and retain them for review. An interruption during a native COM call can leave a disabled task and only the intent record; no retry is implied.

## Evidence and limitations

All output stays under `D:\dev\ChatGPT\mcp-r17-native-qualification-20261119\<guid>\`. `intent.json` is written before any registration. Successful creations save their exact registered XML. The seven read responses are saved as separate JSON files, and nonempty standard-error output is retained beside them. `result.json` records the source hash, completed checks, read-child settlement, both task states, cleanup outcomes, and any failure. These runtime files may contain local principal identifiers and must remain local until a separately reviewed redacted report is prepared. The proposal itself contains none. Standard output contains only pass/cleanup booleans, a failure-present flag, and the authorized evidence-directory path; detailed errors and definitions are not printed as shareable results.

Evidence and recovery files remain outside trash. There is no automatic file deletion, trash purge, or removal of uncertain state. The missing executable is intentionally never created.

A future successful run would qualify the exact production PowerShell script against the native scheduler on that machine, for the cases in Table 1. It would **not** qualify the Go public backend, its subprocess launch arguments or timeout mapping, Go error conversion, public status conversion, successful RunOwned, running Stop, production Delete, task action settlement, access-denied behavior, policy behavior, or release/installable-product readiness. The extra `-EncodedCommand` transport is part of this proposal, while production Go uses `-Command`. Both carry the same source script, but their launch paths are not asserted equivalent by this test.

The 15-second wait in `Invoke-ProductionRead` bounds only child-process exit. Reading the two asynchronous stream results has no separate deadline, and in-process COM calls have no enforced overall deadline. The source review found no descendant process launch in the pinned read script that would keep an inherited stream writer open, but this is not proof of a globally bounded run. An interrupted run requires the recovery procedure above.

Preparation included local review and the remote parser/blocked default invocation described above. It included no fixture registration, deletion, start, stop, installation, or policy change. Publication of this proposal and source reading are not native qualification.

## Primary contracts

1. [Microsoft: TASK_CREATION enumeration](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/ne-taskschd-task_creation): `TASK_CREATE=2`, `TASK_DISABLE=8`; update flags are not selected.
2. [Microsoft: ITaskFolder::RegisterTaskDefinition](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-itaskfolder-registertaskdefinition): registration flags, identity, logon type, and optional security descriptor parameters.
3. [Microsoft: TASK_LOGON_TYPE enumeration](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/ne-taskschd-task_logon_type): interactive-token logon is `3`.
4. [Microsoft: TASK_RUNLEVEL_TYPE enumeration](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/ne-taskschd-task_runlevel_type): least-privilege run level is `0`.
5. [Microsoft: ITaskFolder::GetTasks](https://learn.microsoft.com/en-us/windows/win32/api/taskschd/nf-taskschd-itaskfolder-gettasks): `TASK_ENUM_HIDDEN=1` includes hidden tasks.
6. [Pinned #660 production source](https://github.com/applicate2628/mcp-local-hub/blob/1b870e04f58bbf60fde2d2339fcd4ae9b50b24c3/internal/scheduler/scheduler_com_windows.go): exact script and its production Go caller; local snapshot hash above.
7. [Microsoft: about_PowerShell_exe, ExecutionPolicy](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_powershell_exe?view=powershell-5.1#-executionpolicy-executionpolicy): session setting without a registry policy change.
8. [Microsoft: about_Execution_Policies](https://learn.microsoft.com/en-us/powershell/module/microsoft.powershell.core/about/about_execution_policies?view=powershell-5.1): session/child scope, Group Policy precedence, and `RemoteSigned` constraints.

In the production response, `owner` is the execution principal. It is not the owner field of a Windows security descriptor. Fixture ownership additionally depends on successful creation and the run-specific definition evidence described above.

# Recovery namespace fixture on Windows

This harness supplies the external fixture required by these existing API tests:

- `TestSecretsInit_RecoveryNamespaceBoundary`
- `TestSecretsInit_RecoveryNamespaceRetainedDirectory`
- `TestSecretsInit_RecoveryNamespaceFailurePaths`

## Contents

- [Run](#run)
- [Authority and process lifetime](#authority-and-process-lifetime)
- [Observations and negative fixtures](#observations-and-negative-fixtures)
- [Evidence and limits](#evidence-and-limits)

## Run

Prerequisites: Windows, an already installed PowerShell 7.2 or later, the Go
version required by `go.mod`, and the repository's dependencies already present
in the module cache. This harness does not install tools, download modules,
enable privileges, change execution policy, or change machine configuration.
The repository and temporary parent must use ordinary local drive paths;
reparse points in their ancestry are refused.

From the repository root in PowerShell 7:

```powershell
$fixtureGo = (Get-Command go.exe -CommandType Application -ErrorAction Stop).Source
$fixturePowerShell = (Get-Command pwsh.exe -CommandType Application -ErrorAction Stop).Source
$fixtureSavedEnvironment = @{}
foreach ($name in @('GOTOOLCHAIN', 'GOPROXY', 'GOSUMDB')) {
    $fixtureSavedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
try {
    $env:GOTOOLCHAIN = 'local'
    $env:GOPROXY = 'off'
    $env:GOSUMDB = 'off'
    & $fixtureGo run -mod=readonly ./tools/test-recovery-namespace `
        -repo (Get-Location).ProviderPath `
        -go $fixtureGo -pwsh $fixturePowerShell
    if ($LASTEXITCODE -ne 0) { throw "Recovery namespace harness failed: $LASTEXITCODE" }
} finally {
    foreach ($name in $fixtureSavedEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($name, $fixtureSavedEnvironment[$name], 'Process')
    }
}
```

Explicit absolute executable paths can replace `Get-Command`. The optional
`-parent 'E:\private-test-parent'` selects an existing ordinary parent for the
new fixture directory. Defaults are five minutes for API test compilation and
three minutes for the tests; `-build-timeout` and `-test-timeout` accept positive
Go duration strings up to ten minutes. Each process has a separate ten-second
cleanup bound. The Go bootstrap above is ordinary compilation of the launcher;
an invoking automation should also bound that outer command.

The launcher prints the fresh evidence directory. It retains that directory
on success and failure so the result can be inspected. It never reuses or
automatically removes a previous evidence directory.

## Authority and process lifetime

The Go launcher creates a fresh random directory with a protected discretionary
access control list (DACL). Only the current user, LocalSystem, and built-in
Administrators receive full control. It retains ordinary ancestor handles
without write or delete sharing, then creates and retains `temp`, `proof`, and `bin`
inside that namespace. `TEMP`, `TMP`, and `TMPDIR` point to its private `temp`.
No existing directory's permissions are modified.

Child environments discard inherited `MCPHUB_*` values before installing the
four namespace bindings. In particular, an old API child-process sentinel
cannot bypass `TestMain` sandbox setup and select an external audit directory.
The invoking process's environment is unchanged by the Go launcher.

The launcher copies the entire helper, including its native API declarations,
to `helper.ps1` in the new namespace. It supplies the four existing contract
variables, including the SHA-256 digest checked by the Go tests before every
helper invocation. Selected source files and the copied helper are recorded
by digest; source handles exclude write and delete sharing during the run.

The launcher uses `process.RunContainedStreamObserved` for both `go test -c`
and the resulting `api.test.exe`. This existing owner creates containment
before child execution, supplies hidden Windows startup, and owns descendant
termination, the single root wait, standard streams, and handle settlement.
The launcher requires positive native and input/output settlement and a zero
root exit code. There is no task-list sweep or termination by a reopened PID
(process identifier). The helper's PowerShell processes remain in that same
contained test process tree.

The helper requires a protected creator root owned by its current user. Every
target must be a strict descendant of `root\temp`; every path component is
opened without following a reparse point, and ordinary ancestor handles stay
open without write or delete sharing during the operation. Only `Snapshot` and `RemoveJunction` accept a
reparse point at the final component. The latter deletes only the held link
object after verifying its exact recorded junction target.

## Observations and negative fixtures

| Action | Actual operation and evidence |
|---|---|
| `Snapshot` | Captures existence, native volume/file identity, attributes, link count, creation/write times, size, owner/group/DACL, sorted entries, and SHA-256 of ordinary file contents. It records raw reparse data and does not traverse the target. |
| `Broaden` | Applies a protected DACL to the selected ordinary fixture directory, adding an actual Authenticated Users read grant. The surrounding creator root remains protected. |
| `Junction` | Exclusively creates a private directory, sets a mount-point reparse buffer using `FSCTL_SET_REPARSE_POINT`, and checks native readback. The target must also be an ordinary private directory. |
| `RemoveJunction` | Reads the original link's reparse buffer through its retained handle, checks the exact expected target, and marks that held link object for deletion. It does not remove the target. |
| `WrongOwner` | Prepares the exact `temp\wrong-owner-fixture` directory and makes one owner-assignment call for Builtin Users (`S-1-5-32-545`), which is outside the product's owner allowlist. It reads back the actual owner and records success or the Windows error. |
| `Handles` | Opens the live actor for observation, verifies its executable is the exact private `bin\api.test.exe`, and calls `GetProcessHandleCount`. It records the real count, PID, creation `FILETIME`, and image path. |

No privilege adjustment or elevation is attempted for `WrongOwner`. A normal
token may be unable to assign the requested owner. In that case the existing
test logs **UNVERIFIED actual wrong-owner receiving**, and the launcher repeats
that limitation. A refused assignment is not evidence that the production
reader rejected an actual foreign owner.

The two handle tests require equality of their actual before/after counts.
The launcher additionally requires all four observations to match the original
PID and creation time returned by the contained process owner. An expected
count is never substituted for an observed count.

## Evidence and limits

`proof` retains build/test output, process settlement observations, selected
source and binary hashes, snapshots, all handle measurements, and the one
owner-assignment result. These files include local paths and account security
identifiers; inspect them privately rather than publishing them wholesale.

The selected-source digest list is explicit in `inputs.json`; it is not a
claim that every compiler input was independently attested. Snapshot equality
proves the selected object's observed state across the tested operation; it
does not establish absence of every possible side effect elsewhere. Snapshots
intentionally omit last-access time because reading can update it. Snapshot
traversal is bounded to 16 levels, 2,048 entries, and one MiB per regular file;
limits cause failure instead of a partial successful snapshot.

Native API references: [file security by handle](https://learn.microsoft.com/en-us/windows/win32/api/aclapi/nf-aclapi-getsecurityinfo),
[file identity](https://learn.microsoft.com/en-us/windows/win32/api/winbase/ns-winbase-file_id_info),
[reparse operations](https://learn.microsoft.com/en-us/windows/win32/fileio/reparse-point-operations),
and [process handle count](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-getprocesshandlecount).

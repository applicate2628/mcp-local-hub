# Cancellation qualification: distinguishing tests and negative controls

## Scope

Source base: `0bcb5f0a1044f81dc3d28019ebfc353cc0ef10c7` plus the precisely hashed changes in the associated logs. All fixtures and processes are owned by the experimental tests. No installed Hub, user configuration, network/vendor operation or production launch is involved.

The [probe runner](probe.ps1) records the actual command, exit status, source and artifact hashes before each selected run. It requires an already built owned DLL/child and never installs dependencies. It refuses to overwrite prior evidence. The [mutation runner](mutations.ps1) modifies only the isolated Go observer source, restores the exact original bytes in a `finally` block after each case, and reruns the known-good cases at the end. A nonzero expected exit is a successful negative control, not a successful product test.

## Compiling RED scaffold

The initial implementation was deliberately just:

```go
func (o *Owner) FinishContext(ctx context.Context) (Outcome, error) {
    return o.Finish(0)
}
```

It compiled and called the real DLL but failed all seven initial observation tests. This exposed missing cancellation/cause handling, native work despite a pre-canceled context, blocking on the owner mutex, lost known root/descendant state, and a second observer returning before explicit settlement. It was replaced by the context-aware loop; a separate test subsequently covered a real checked-admission callback holding the owner lock.

## Three implementation mutations

| Mutation | Violated invariant | Distinguishing result |
|---|---|---|
| Replace `TryLock` with an ordinary blocking `Lock` | An observer must be able to stop waiting even when another caller owns the mutex | Busy-owner cancellation test fails; its safety release then joins the waiting test goroutine |
| Add `Stop(93)` when the observer context closes | Canceling observation must not request termination or consume the operation's stop decision | The original stop code can no longer be chosen, and the normal resumed I/O scenario gets exit93 instead of its natural result |
| Replace the last observation with `Outcome{}` on cancellation | Known root exit7 and remaining-descendant evidence must survive an expired observer deadline | The descendant case fails because its returned observation has been erased |

The captured negative logs are `negative-1-blocking-lock.log`, `negative-1-implicit-stop.log`, and `negative-1-erased-observation.log`. The restored source passes in `negative-1-restored.log`. The script asserts the replacement anchor is unique instead of guessing an edit in a future version.

Reproduce only in a private checkout of the matching source and with the existing local toolchain/artifacts:

```powershell
.\mutations.ps1 -LabRoot 'D:\dev\ChatGPT\your-owned-lab' `
    -CCompiler 'C:\path\to\existing\gcc.exe' -Prefix 'new-negative-run'
```

The paths above are placeholders. All writable paths are required to remain in `D:\dev\ChatGPT`; the compiler itself may be read/executed from an already installed location. Restore source and rebuild ordinary artifacts whenever a different experiment also mutates the DLL. This script does not change the DLL.

## Native I/O test assumptions that failed

`native-io-initial.log` preserves two failed **test assumptions**, not two alleged defects in the library:

1. A pending pipe Write may have its whole queued buffer visible through `PeekNamedPipe`. Available byte count is not proof that Write returned. The corrected test uses the native count only to establish actual request arrival, and separately observes the operation-completion channel. It preserves both byte count and error after interruption, including a count equal to the requested size.
2. This qualified Windows `os.Pipe` rejected `SetReadDeadline` with "file type does not support deadline". The corrected safety path uses only the actual endpoint owner's `Close`, then joins the test operation. No generic File/Reader deadline or cancellation guarantee is inferred.

`native-io-corrected.log` and `context-and-io-green.log` distinguish the corrected positive observations. Native kernel behavior is not replaced with an in-memory fake merely to get a green result.

## Limits of the controls

Successful context cancellation is not successful process termination, stream drain, native resource cleanup or rollback of remote effects. No test makes a claim about arbitrary policy code, synchronous calls already entered, hostile raw pointers, all handle profiles, fairness, or every concurrent schedule. The isolated test's cleanup is not the evidence for the property being asserted. Expected outcomes are sampled before cleanup; late errors and observed data remain separate.

### Final reproducibility refinement

The first implicit-stop control (`negative-1`) inserted Stop only in the
`ctx.Done` select branch and detected the violation on that schedule. Final
`negative-final` injects it at every cancellation return, all of which have
released (or never acquired) the owner mutex. This avoids relying on which of a
simultaneously ready timer/context branch the runtime selects. It does not
change the real observer implementation. Both results and mutated-source hashes
are retained. The final executable script implements this stronger version.
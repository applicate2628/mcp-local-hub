# Qualified Windows Job fixture entry

This test controller runs the two process tests that require an actual retained
outer Job owner:

- `TestJob_BreakawayEnabledDescendantEscapesClose`
- `TestJobInformationTransportPreparationBoundary`, including
  `real-native-typed-gc-and-creator`

It uses `process.NewJobObserved` and `StartWithJobFilesOriginal`. The existing
process implementation supplies at-create Job assignment, original process and
thread handles, native generation and member observations, the one original
waiter, and terminal settlement. The controller's only direct Windows API call
reads the same owned Job's extended limits and returned buffer length.

## Build and run

Use an existing Go toolchain and cached dependencies on Windows, with
`GOTOOLCHAIN=local`, `GOPROXY=off`, and `GOSUMDB=off`. Build both executables from the same
absolute checkout without changing its sources between the build and run. Do
not use `-trimpath`: the receiver compares the configured source directory with
its actual compiled source location. The controller makes the same check for
its checkout. Use an existing private parent directory for the outputs; the
controller requires a fresh `run-root` and never replaces existing output.

```powershell
$repo = (Get-Location).Path
$artifacts = 'C:\private\job-entry-artifacts'
go build -mod=readonly -trimpath=false -o "$artifacts\test-job-entry.exe" ./tools/test-job-entry
if ($LASTEXITCODE -ne 0) { throw 'controller build failed' }
go test -mod=readonly -c -trimpath=false -ldflags='-H windowsgui' -o "$artifacts\process.test.exe" ./internal/process
if ($LASTEXITCODE -ne 0) { throw 'process test build failed' }
& "$artifacts\test-job-entry.exe" `
  -test-binary "$artifacts\process.test.exe" `
  -source-dir "$repo\internal\process" `
  -run-root "$artifacts\run-001" `
  -timeout 90s
if ($LASTEXITCODE -ne 0) { throw 'qualified Job fixtures failed; inspect run-001' }
```

The caller should launch the controller with its usual hidden Windows process
runner. The controller uses `process.NoConsole` for the test process. No shell
policy switch, elevation, installation, process-name kill, post-start Job
assignment, or test skip is used. The inherited `MCPHUB_*` environment is removed
before supplying the new entry configuration, so another test's helper mode or
stale fixture does not select a different execution path.

The process test binary must use the Windows GUI executable subsystem. A
console-subsystem binary can create a `conhost.exe` member despite
`CREATE_NO_WINDOW`; the controller deliberately requires exactly the original
test process before GRANT. It checks the portable executable (PE) header before
launch and retains the same native membership check. Standard input/output
still use the explicit inherited handles. Neither selected test needs a console.

For the controller's protocol unit tests:

```powershell
go test ./tools/test-job-entry
```

Those unit tests cover malformed and uncorrelated wire messages, bounded stream
capture, and helper-environment isolation. They do not substitute for the two
native tests above.

## Evidence and limits

`packet.json` records the actual test and controller executable hashes, both
controller source hashes, the four required companion source hashes, and exact
test arguments. `entry-config.json` binds the exact packet bytes by SHA-256
(Secure Hash Algorithm, 256 bits). Source hashes describe the frozen checkout used by
the documented build; they are not compiler provenance attestations.

The child emits READY with its native process creation time and random
challenge. Before replying, the controller checks the original at-create
process, revalidates the original member source, requires exactly one live
member using the borrowed original process handle, and re-queries the original
Job's limits. It closes the member observation and requires its same-owner
close receipt before sending GRANT. GRANT is followed by stdin EOF. The child's
ACK must name this exact run, challenge, owner generation and GRANT payload
hash. A JSON flag supplied by the caller cannot produce these native facts.

Success requires both selected tests and the native subtest to report PASS,
the original test process to exit with code zero, both captured streams to reach
EOF, and the retained owner's native settlement to confirm all current Job
members exited and its handles closed without requiring a terminal action.
A protocol, test, stream, deadline or
cleanup failure produces a nonzero controller exit. Failure cleanup is never
accepted as a successful test result.

The work deadline is at most five minutes and cleanup has at most ten additional
seconds. Each stdout/stderr log is capped at 2 MiB; control frames are capped at
8192 bytes. The receiver also enforces its own ten-second handshake deadline.
`result.json`, `stdout.log` and `stderr.log` retain the observed outcome and
errors. Files are kept for review; the controller does not remove them.

The output parent must already be private to the caller. Creating a directory
with a Go permission mode does not establish a new Windows access-control
policy. Run this harness only with the repository's freshly built test binary.

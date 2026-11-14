# Source-issued Serena qualification publisher

This Windows-only development tool compiles exactly `./internal/cli` with
`test_state_path_env,serena_qualification`. Its output has the explicit
`serena-qualification` PE (Portable Executable) role and cannot pass ordinary
Product admission. It does not run tests, install Serena, launch the Hub,
change the installed product, or alter the release builder.

## Build

Use the existing native Windows/amd64 Go toolchain and an already populated
module cache, including the pinned `goversioninfo@v1.5.0` resource generator.
Go reads module metadata and archives only from that cache through a local
`file://` proxy, with no network or direct fallback. The cache must also contain
the metadata used by Go's deprecation lookup; it does not change the pinned
generator version. Supply the compiler's actual `GOROOT`, which may differ
from the directory containing `go.exe`.
Before launching, direct the builder process's `TEMP`, `TMP` and `GOTMPDIR`
to an existing owned task directory: the shared source-preparation owner
launches local Git plumbing before the isolated Go child environment exists.

```text
go build -o <owned-tools>/test-serena-publisher.exe ./tools/test-serena-publisher
test-serena-publisher.exe --checkout <absolute-checkout> --expected-commit <40-hex-commit> --go <absolute-go.exe> --go-root <absolute-GOROOT> --module-cache <absolute-existing-cache> --work-root <fresh-absolute-directory>
```

The checkout's selected files must equal the expected commit. The shared
`PreparePublisherSource` owner exports the real Git-selected closure and
issues the source snapshot. This tool verifies that snapshot, generates a
VERSIONINFO resource with the issued epoch and current build tuple, and
compiles the fixed test target in the export. It moves the transient resource
to the work root's `trash` before verifying the entire source closure again.
The build uses a finite deadline and existing contained-process ownership;
each child must have successful root exit, native cleanup and settled output.

The work root is newly created with a protected discretionary access control
list (DACL) for the current user, LocalSystem and Administrators. Existing
inputs and directory ancestors are opened without following reparse points
and retained during the build. The output is parsed from its actual bytes
and matched to role, source epoch, build tuple, architecture and subsystem.
Input handles close before the final publisher receipt is written.

All generated source, binary, temporary files and build cache remain in the
work root. The explicitly supplied module cache can be used by Go. Failed
build evidence is retained for inspection; the tool never empties `trash`.

## Outputs and evidence

| Relative path | Meaning |
| --- | --- |
| `source/` | Exact exported selected source closure |
| `source-snapshot.json` | Existing source owner's persisted file table |
| `bin/serena-qualification.test.exe` | Qualified CLI test process, GUI subsystem |
| `proof/source-preparation.json` | Original Git/source preparation receipt |
| `proof/*.stdout.log`, `proof/*.stderr.log` | Complete stage output within explicit limits |
| `proof/*.settlement.json` | Native process and stream completion; `process_and_io_complete` concerns those resources only |
| `proof/publisher.json` | Final image hash and source/build binding, emitted only after all stages and input-handle closes succeed |

The source association is cooperative build provenance. It is not a code
signature, memory attestation or proof that tests have passed.

The separately reviewed namespace and CLI fixtures consume this image. The
builder receipt alone does not authorize ordinary product use or claim
successful package preparation or test execution.

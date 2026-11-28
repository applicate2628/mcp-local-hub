# go-winio source provenance

The local replacement retains the original module path and uses Microsoft go-winio v0.6.2 as its base. The retained Go files cover the root package and the transitive root-package test imports. The checked-in generated Windows syscall files came from the release archive. The upstream archive owns the standalone packages and syscall generator.

- Go module checksum: `h1:F2VQgta7ecxGYO8k3ZZz3RS8fVIXVxONVUPlNERoyfY=`
- Module metadata checksum: `h1:yd8OoFMLzJbo9gZq8j5qaps8bJ9aShtEA8Ipt1oGCvU=`
- Module archive SHA-256: `cd007bdb877a578efde2780bf5dd2f57dd557b5b08c3614597b820d0369ebb0f`
- License: upstream [MIT license](LICENSE).

The local patch captures the original native close outcome after I/O drain and makes the receipt available through `CloseObserved`. The ordinary `Close` method keeps its existing contract. The local focused Windows tests are in `file_close_observed_windows_test.go`.

The dial initialization path now creates that same original `win32File` owner immediately after `CreateFile` succeeds. Metadata or I/O initialization refusal closes it once and returns `PipeDialError`, retaining its original owner and `CloseReceipt` alongside the primary and close errors. `CloseResult` only receives the original observation; a nil connection is not a close receipt. Refusals before successful `CreateFile` have no acquired handle, and a confirmed successful partial close remains an ordinary failure rather than an unresolved owner. The existing `DialPipe*` paths and deadlines are unchanged. The shared `makeWin32File` constructor still leaves error cleanup to its existing callers.

Failed native close retains the uncertain handle privately in `CloseReceipt`; the active file handle is consumed, `Fd` refuses after closing begins, and neither the receipt nor the typed error grants retry authority. Focused dial failure/receiving tests are in `pipe_dial_close_observed_windows_test.go`. Their injected failure points do not replace the existing native named-pipe acceptance tests.

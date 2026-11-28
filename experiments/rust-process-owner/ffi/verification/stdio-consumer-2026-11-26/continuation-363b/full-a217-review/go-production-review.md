# a217: full static review of Go implementation and module

## Contents

1. [Scope and provenance](#scope-and-provenance)
2. [A217-GO-01: unload failure is lost on loader rejection](#a217-go-01-unload-failure-is-lost-on-loader-rejection)
3. [Full-file coverage](#full-file-coverage)
4. [Evidence limits](#evidence-limits)
5. [Sources](#sources)

## Scope and provenance

The lead reviewer (`root`) fully reviewed nine Go implementation files and `go.mod` at commit `a217c7799a0214967bf86211542e508162c680b5`, tree `ca118ff1ec513cc830bb8b96b4d6782d23915a26`: **10 files, 74,894 bytes, 1,960 lines**. This document renders the original [go-production-review.json](go-production-review.json); consolidation does not claim a second full-file review. Exact pins and the original review digest are retained in the JSON and [provenance.json](provenance.json).

## A217-GO-01: unload failure is lost on loader rejection

**Classification:** conditional static error-preservation defect; priority P2 in the original lead review.

**Location:** `experiments/rust-process-owner/ffi/go/bridge_windows.go:89–90`, C `bridge_open` failure label.

**Condition:** `LoadLibraryExW` has succeeded, required symbol resolution or ABI version validation rejects the DLL, and the subsequent `FreeLibrary` call returns false.

**Source behavior:** the failure label ignores the unload return value, frees the bridge allocation and returns only the original loading/ABI error. No `Library` or retained module capability is returned through this error path.

**Consequence:** the secondary unload failure and the unresolved module reference are not accounted for by a returned cleanup receipt or retained owner. This is a conditional source finding. No actual `FreeLibrary` failure, observed module leak or product cleanup failure is claimed.

**Test coverage boundary:** the loader tests exercise rejection followed by successful unload/reference checks, not failure of the unload call itself. Go test finding GT2 documents this limit; it supports A217-GO-01 and is not a duplicate production defect. Protected `CloseHandle` fixtures concern different operations and cannot supply that missing evidence.

**Source applicability:** this complete Go file is byte-identical in native build source `efa73c0e23f7af16b8aa2ccb047b745a58ba4f71`, so the same conditional branch finding applies. The changed `windows.rs` startup window flags do not alter the loader path.

**Bounded next decision:** preserve both the primary rejection and any unload failure, with an explicit disposition for remaining module custody. Any implementation or qualification remains separate from this static review. Do not turn a modeled or substituted failure into a claim of native reproduction.

## Full-file coverage

**Table 1. Files reviewed by the lead Go reviewer.** Exact Git blob IDs and SHA-256 digests are recorded in [go-production-review.json](go-production-review.json); the full 64-file allocation is in [coverage.json](coverage.json).

| File relative to `experiments/rust-process-owner/` | Lines | Bytes | Review notes |
| --- | ---: | ---: | --- |
| `ffi/go/admission_windows.go` | 77 | 2439 | Read complete callback: call-scoped cgo.Handle, single call, native UTF-16 extent/alignment/NUL validation, Go-owned copy before policy, panic containment; policy Goexit/fatal abort expressly unsupported. |
| `ffi/go/bridge_windows.go` | 835 | 33852 | Read all 835 lines including C shim: loader symbol/version negotiation, library/owner mutex and live count, bounded C-owned request buffers, transferred-owner preservation, split wait/close/release, admission/query metadata validation and member generation/cached-close rules. Conditional unload-error loss at C fail label recorded separately. |
| `ffi/go/environment_windows.go` | 14 | 778 | Read full explicit sorted/unique environment contract; no inherited environment overlay or logging claimed. |
| `ffi/go/finish_context_windows.go` | 78 | 2581 | Read full synchronous Finish(0) polling with TryLock, cancellation cause preservation and reusable timer; no cancellation/termination/reader ownership claimed. |
| `ffi/go/go.mod` | 3 | 42 | Read complete isolated module declaration and Go 1.27.3 language/toolchain requirement; not a Hub backend import. |
| `ffi/go/identity_windows.go` | 245 | 8623 | Read full fixed report and identity-copy validation, native token-close error preservation, Go-owned UTF-16/SID copies, admission and query callbacks, repeated callback and panic rejection. |
| `ffi/go/members_windows.go` | 497 | 18885 | Read all 497 lines: bounded/aligned array validation, census ordering, retained resource prefix, original generation, partial acquisition and close-state validation, cached resource binding, Go-owned callback copies. |
| `ffi/go/outcome_windows.go` | 51 | 2115 | Read complete shared ABI status/state/flags/native-error/cleanup-mask validation. Validation is a trusted DLL protocol boundary, not arbitrary pointer safety. |
| `ffi/go/stdio_windows.go` | 89 | 3148 | Read complete caller-owned File duplication via RawConn.Control, one-shot temporary close, no original inheritance mutation, aliasing contract and owner-preserving late close error. |
| `ffi/go/wait_windows.go` | 71 | 2431 | Read complete exit-only observation decoder, split-export availability requirement, pending/exited coherence; no native cleanup authority conferred. |

## Evidence limits

No tests, native processes or builds were run by this review slice. Its per-file notes describe source behavior and contract checks, not runtime PASS results. An invalid ABI result does not automatically revoke separately transferred ownership; the Go release path preserves the cleared-owner-slot fact independently of outcome validation. The cross-language zero-code findings are documented in the [Rust/ABI review](RUST-ABI-REVIEW.md).

The experiment's trusted DLL protocol is not an arbitrary in-process memory-safety boundary. This review does not authorize production adoption, new fallback behavior, mutation of historical test outcomes or a merge.

## Sources

1. [Immutable a217 Go source](https://github.com/applicate2628/mcp-local-hub/tree/a217c7799a0214967bf86211542e508162c680b5/experiments/rust-process-owner/ffi/go).
2. Original lead-review contents, preserved in [go-production-review.json](go-production-review.json), with original digest in [provenance.json](provenance.json).
3. [Go test review](go-tests-review.md), GT2, and [native source mapping](native-dll-source-map.json).

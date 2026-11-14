# Standard streams across Go / C / Rust

Implementation: `4fbbea19e9db7dacc0daef9448cd4c05f2e0c2fa`.
Previous head: `37f0f8bb2b0f4ce068aaeb13000f0ccb6ac7b6f5`.
Native dependency unchanged: `7ce04f7ef2f96680ed94db1edaf77cf0358421c4`, PR #625.
Owner PR: #631; canonical plan: #623. No installed Hub/runtime switch.

## Contents

1. [Implemented boundary](#implemented-boundary)
2. [Verification and source identity](#verification-and-source-identity)
3. [Failures and negative controls](#failures-and-negative-controls)
4. [Review and remaining limits](#review-and-remaining-limits)
5. [Recovery and reproduction](#recovery-and-reproduction)

## Implemented boundary

`NewWithStdio` supplies three explicit caller-owned Go files, plus a complete
explicit environment, to additive `mh_create_io1`. It reuses the existing
native `BorrowedStdio`, at-create handle allowlist, Job and admission path.
No second process engine, supervisor or handle registry was introduced.

The Go adapter accepts non-inheritable source handles by making three bounded,
short-lived inheritable copies. Each DuplicateHandle call occurs inside
RawConn.Control while the source handle is valid. Original handles are never
closed, assigned inheritance flags or transferred to Rust. No naked Fd plus
KeepAlive substitution is used. Copies are closed once after create, including
partial acquisition failure. Any already transferred process owner survives
late copy-cleanup or metadata errors.

Sources may be closed after create returns, before admission. Child references
remain valid. Shared stdout/stderr share the same kernel object/file position,
not separate copied data. Exact binary payloads, unchanged source flags, absence
of effects before admission and source usability after finish are tested.

The raw C contract still requires real live inheritable handles. Null and
pseudo-handle checks are not general pointer/handle authorization. Native
category and inheritance checks are not proof of access rights or pipe subtype.

## Verification and source identity

The [complete final run](../runs/stdio-2026-11-14-final/COMPLETED.txt) uses the
committed [runner](../run.ps1). Its [source manifest](../runs/stdio-2026-11-14-final/source-hashes.json)
identifies the exact runtime/test/runner bytes. The run was executed with
implementation HEAD above plus an additional actual-Go-pipe test and pending
qualification/documentation. It is not described as a clean run of a later
commit. The final published source must match that manifest.

The [earlier implementation run](../runs/stdio-2026-11-14-implementation/COMPLETED.txt)
passed before adding the ordinary Go os.Pipe case. Both distinct runs are
retained; the earlier count is not substituted for the final count.

**Table 1. Final verification.**

| Layer | Observed result |
|---|---|
| Full experimental Go module | 34 top-level functions, 31 nested cases, zero skips |
| New standard-stream functions | 11: exact bytes/ownership, caller close, shared output, native pipe EOF, deny/missing image, six invalid source cases, allowlist exclusion, environment/admission composition, partial duplication, seven close-failure masks, actual Go pipes |
| Debug and release DLL | Each full Go suite passed race-enabled count=3 |
| Go pointer/static checks | cgocheck2, go vet, formatting passed |
| Rust ABI | 14 unit tests passed debug/release; Clippy -Dwarnings and formatting passed |
| Unchanged native parent | All 53 checks repeated debug/release, Clippy and formatting passed |
| Runner | All 22 step exits matched; step00 deliberately expects exit7 and is not a product test |

[Per-command status](../runs/stdio-2026-11-14-final/steps.json) and
[Go counts](../runs/stdio-2026-11-14-final/go-counts.json) are versioned with logs.
The native synchronous anonymous-pipe test and the ordinary Go os.Pipe test
both observe EOF after child exit and parent-side source closure. The payload
is bounded below pipe capacity; this is not a concurrent streaming benchmark.
Go race/cgo checks do not prove every Rust/C schedule or arbitrary native input.

## Failures and negative controls

- [Initial RED](01-red.log): a compiling, rejecting Go stub failed seven real
  behavior tests. Refusing invalid sources alone did not count as implementation.
- [C compilation failure](03-integration.log): a text insertion split an existing
  multiline typedef. The declaration was moved after the complete prototype;
  [integration then passed](04-integration.log). This was an editing defect,
  not an OS/ownership failure, and its log is retained.
- [Omitted stdio](10-ignore-stdio-negative.log): replacing only the native stdio
  argument with None failed ordinary observed-child tests. Their owned-process
  cleanup ran; a pending child was not reported as success.
- [Hidden close errors](12-hidden-close-negative.log): suppressing the temporary
  close result failed all seven protected-close combinations.
- [Late metadata failure](14-metadata-owner-preserved.log): corrupting only create
  output size retained the actual owner and valid source files; no DLL unload,
  fallback or second launch occurred.
- [Missing stdio export](16-missing-export-refused.log): an owned DLL with only
  that symbol renamed was refused with code127. This is a controlled missing
  symbol, not qualification of an independently released old binary.
- All temporary edits were restored before the [normal rebuild](17-normal-dll-restored.log),
  [focused race repeat](18-restored-stdio-race.log) and the full final run.

[Exact mutation instructions](MUTATIONS.md) reproduce these checks without
uncommitted machine-specific scripts. An initial source-hash command used an
unavailable Get-FileHash cmdlet; the existing .NET SHA256 API verified the same
files before commit, without installation. That command issue is recorded in
the [versioned progress ledger](../PROGRESS.md), not classified as a code defect.

## Review and remaining limits

Temporary-copy failures and process-owner cleanup are distinct. Seven genuine
protected-close masks are induced in the Go copy-owner helper, and their
operation/slot/native codes remain observable. The fixture can unprotect its
saved exact handles; the production adapter does not claim that authority for
an unknown failed close. A failed close may leave a copy live and is NOT reported
as completed cleanup. No unsafe stale-handle retry is performed.

The count check covers repeated third-source acquisition failures after a
warm-up, not all process-wide first-use initialization. No full foreign-call
copy-close fault was injected after real create; helper failure evidence and
metadata-transfer evidence must not be combined into that stronger claim.

The child allowlist excludes a separate inheritable test event. It protects
this child, not unrelated concurrent native launchers that inherit all handles
while temporary copies exist. Product-wide coordination is still a gate.
Files and bounded local synchronous/Go pipes were exercised; sockets, consoles,
device handles, all overlapped profiles, access-right correctness and full-duplex
backpressure/cancellation were not qualified. Caller must supply correct ends,
retain appropriate rights and coordinate shared file-position use.

No dependencies, parent library/API/lock, root product or installation changed.
No native Linux/macOS runtime/cross-build, performance claim, independent review
or product-wide acceptance is implied. Actual foreign kernel process-cleanup,
allocator/panic/loader faults, complete product admission fields, blocking I/O
cancel/drain/join and installed acceptance remain open.

15 relevant skills were applied: Using Superpowers, Executing Plans, TDD,
API/Interface Design, Architecture Review, Go Concurrency Lifecycle, Go Security
Hardening/process boundary, Go Testing/Verification, Using Git Worktrees
(existing isolated clone), Rust Unsafe Checker, Rust Resource Lifecycle,
Ponytail Review, Systematic Debugging, Git Workflow/Versioning, Verification
Before Completion. Rust skills were read via omgskills at
`actionbook/rust-skills@5c40d3ad785193231b7d0dbfb8e1eb447e5edd94`.
This is author self-review, not 15 reviewers or independent certification.

## Recovery and reproduction

Clone the PR and follow [the ABI README](../../README.md) and [runner](../run.ps1)
with already available approved Windows Rust/Go/C tools and offline caches.
Use a fresh run directory. Required source, contracts, commands, failures,
conclusions, step exits and hashes are committed in this PR. Compiler targets,
DLLs and temporary fixtures are rebuildable and are not required from this PC.
Nothing here installs, merges, launches vendor/models or activates the live Hub.

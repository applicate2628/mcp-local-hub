# Execution ledger — plan: docs/modernization/plans/rust-independent-execution-plan-2026-11-12.md

## 2026-11-14 — PR-durable evidence and explicit environment calling boundary

Base: `04ca025e49de430de7d8f1a7dcea0ffd2cab33d2`, existing PR #631.
Native parent #625 remains `7ce04f7ef2f96680ed94db1edaf77cf0358421c4`.

Ruling: user requires all necessary results in PR; local-only ledgers are not
recovery. Historical evidence for the native dependency and FFI is imported with
original/published hashes, without rerunning or upgrading its claims.

Ruling: continue in the already isolated clone and approved branch. No active
primary checkout, shared installation, other code branch, dependency version or
root build is changed. Cache-only toolchains; no install, merge, model launch or
explicit CI/bot dispatch. Repository-wide/installed gates remain open.

Next bounded task: transmit an explicit complete environment through the existing
Go/C/Rust creation boundary. Preserve `mh_create1` and its empty-environment
contract. An additive export must reuse the same native creation owner, validate
count/size and preserve pending/denied/late-error ownership. Empty means empty,
not inherit; input buffers live only through the synchronous call; no parent
mutation. Do not introduce an environment overlay or hidden required-key copy.

Work steps: archive/push existing evidence; write observed-child tests and watch
an ignoring stub fail; implement additive ABI and caller marshalling; exercise
invalid names/order/duplicates/lengths, GC, deny, parallel children and empty
compatibility; run native/foreign suites plus negative controls; self-review;
commit implementation and final results separately; verify remote contents.

All active/uncertain source, required evidence and recoverable data stay outside
trash. Only confirmed redundant files may move directly into approved trash;
never empty it automatically.
Archive verification: the initial diff check rejected trailing whitespace and
CRLF in generated JSON. Published text now removes only end-of-line whitespace
and normalizes LF; original-byte hashes remain intact and published hashes were
recomputed. No test result or failure message was changed or upgraded.

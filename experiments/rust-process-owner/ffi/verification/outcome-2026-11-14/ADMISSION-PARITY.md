# Admission delta after PR #616 merged

## Snapshot and scope

On this continuation the GitHub source of truth reports [PR #616](https://github.com/applicate2628/mcp-local-hub/pull/616) **merged**, not an outstanding parallel branch. The immutable merge commit is [`17bc78fc602efa204a74596409c939a3bf1ddc42`](https://github.com/applicate2628/mcp-local-hub/commit/17bc78fc602efa204a74596409c939a3bf1ddc42). Its own receipt still distinguishes source delivery from incomplete full-suite/native/installed acceptance. This note does not clear those gates.

The Rust experiment remains based on the original owner PR #625; this iteration does not merge/rebase that family or switch the working Hub. Readback of [creator_identity.go at the merge commit](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/creator_identity.go) confirms that a two-field PID/image observation is **not complete parity** with the accepted Go identity contract. Do not adapt by supplying plausible defaults for missing facts.

## Required evidence versus the existing spike

**Table 1. Source-grounded field comparison.**

| Accepted Go contract | Current native Rust/Go spike | Remaining work |
|---|---|---|
| Original PID and executable path from retained creator | `AdmitChecked` observes original ProcessView and copies PID/path into Go | Preserve current original-handle lifetime; do not replace it with PID reopening |
| `OriginalStartedAt` and the original creator binding | Not part of the exported ProcessObservation | Add an original-handle birth observation and compare it to its original binding; a caller timestamp is not a substitute |
| `OwnerSID` plus a validated SID-format observation | No token/SID evidence in exported callback | Qualify token-query ownership, representation, failure and lifetime before crossing the boundary |
| Original at-create Job binding and known current membership | At-create containment is implemented internally; current membership is not an exported identity fact | Supply the requested evidence using retained objects and retain native failures; do not synthesize true |
| Root wait known/result and consistent liveness | Internal native admission checks liveness; the full structured observation is not exported | Preserve wait-state validity and correspondence with membership without treating a numeric exit sentinel as identity |
| Version, canonical snapshot and evidence digest tied to the original owner | Copied callback is a bounded observation, not the Go `CreatorIdentity` capability | Integrate the real capability/validation contract rather than equating a copied struct/hash with authority |
| Context/custody gates and identity-in-flight exclusion | Call-scoped callback holds one Go owner lock; `FinishContext` only cancels an observer | Reconcile actual product concurrency/retirement rules; no automatic replay or transfer to another supervisor |

The digest is not itself an authorization proof. PID/path are useful diagnostic observations but not a file signature, immutable executable-file identity, access-token proof or permission to reopen/kill a process. Missing fields must remain unsupported rather than silently accepted.

## Ordering of the next implementation

1. Review the exact intended product consumer and its required fields against the merged source, including the remaining source/native acceptance limits reported by #616.
2. Extend the existing native owner capability only where necessary, with original-handle acquisition, exact error and lifetime tests. Keep this a separate owner responsibility, not a new FFI-side process observer that reopens by PID.
3. Add a versioned foreign observation layout or additive export without corrupting existing ABI1 layouts; validate its metadata, copies, unavailable facts and one-shot admission behavior.
4. Only then map to the real Go product policy. Reconcile the experimental branch family with master under ordinary review. Do not infer installed or cross-platform readiness from the Windows experiment.

The present implementation instead fixes the already shared result decoder first: a boundary that accepts inconsistent completion/cleanup evidence should not be expanded with additional supposedly authoritative fields. That correction is limited to the experiment and does not implement the identity rows above.

## Sources and limits

[Creator identity contract at the merged commit](https://github.com/applicate2628/mcp-local-hub/blob/17bc78fc602efa204a74596409c939a3bf1ddc42/internal/process/creator_identity.go) is the primary readback used here. The corresponding Windows implementation was inspected in the experiment's existing product-source snapshot for call-path context; it is not represented as an independent runtime qualification on merged master. No full repository/installed checks were run by this admission comparison. The source delivery and its remaining gates are reported in [#616](https://github.com/applicate2628/mcp-local-hub/pull/616); migration progress remains in [#623](https://github.com/applicate2628/mcp-local-hub/pull/623).

Terms: PID — process identifier; SID — Windows security identifier; ABI — application binary interface; FFI — foreign function interface. A Windows Job is the kernel grouping/containment object, not an abstract promise of identical behavior on Linux or macOS.

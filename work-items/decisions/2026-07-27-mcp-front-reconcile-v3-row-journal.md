---
status: proposed
date: 2026-07-27
owner: architect
decision: mcp-front-reconcile-v3-row-journal
revision: R3
---

# MCP-front reconcile version-3 row journal: R3 contract

## Verdict

**PASS — accepted-ready for repeat reliability review; planner-blocked until
that review returns `PASS`.**

This R3 decision supersedes the R2 contents previously stored at this path. It
keeps the accepted row-journal direction and closes the six architecture gaps
proved by the two R2 reviews. Promotion from `proposed` to `accepted` remains an
independent architecture-review gate; implementation must not weaken the
contract merely because the version-3 format is not yet shipped.

The decision is one transaction design, not six local patches:

1. the `clients.lockingClient` wrapper authorizes a target mutation and every
   same-config dependency under one config-file lock;
2. one CLI-owned classifier maps durable attempt state to ownership certainty,
   while forward and rollback callers apply different policies to that result;
3. one CLI-owned secure pin loader opens, validates, hashes, and reads every
   Serena pin once, then passes the exact verified bytes to the inverse;
4. every Serena precondition conflict is a durable no-write row, including the
   first generation;
5. rollback skips only an uncertain row or LSP dependency group and continues
   every independent safe inverse before returning an aggregate pending result.

## Current-source evidence and R2 review disposition

The source below was independently re-read at
`31b9ca9412ebf42eaa8b141009e94f34bf0acedc`.

| R2 review class | Verified current source | R3 disposition |
| --- | --- | --- |
| Cross-entry LSP dependency race | Forward keeps `canonicalReady` in process memory, but the mutation wrapper checks only `op.EntryName` (`internal/api/lsp_client_router.go:277`, `internal/api/lsp_client_router.go:310-327`). Rollback proves legacy readiness before a later canonical-only conditional mutation (`internal/api/lsp_client_router_snapshot.go:314-338`, `internal/api/lsp_client_router_snapshot.go:455-475`). | Add one wrapper-owned multi-entry authorization primitive. |
| Persisted post-write conflict can be superseded | `effectiveMCPFrontAppliedReceipt` marks both `prepared` and `conflict` uncertain (`internal/cli/install_reconcile_mcp_front.go:282-299`), but settlement handles only `prepared` (`internal/cli/install_reconcile_mcp_front.go:302-334`) and the constructor then increments generation and replaces `ActivePlan` (`internal/cli/install_reconcile_mcp_front.go:1237-1280`). | Centralize classification and block forward admission for both uncertain states. |
| Pin time-of-check/time-of-use and link escape | Pin verification performs lexical containment and hashes one path read (`internal/cli/install_reconcile_mcp_front.go:1082-1124`), while Serena rollback later reopens `BackupPath` (`internal/api/serena_client_reconcile.go:729-740`). | Open securely once; inverse consumes verified bytes, never a path. |
| First-generation Serena conflict disappears | `finishAttempt` returns success without persisting when the row is absent and no mutation was invoked (`internal/cli/install_reconcile_mcp_front.go:1386-1393`). | Persist a pinless no-write Serena row with a terminal precondition-conflict attempt. |
| Rollback loses independent progress | Rollback calls settlement and returns before either surface when a prepared row survives (`internal/cli/install_reconcile_mcp_front.go:700-708`). | Settlement returns classifications; rollback continues independent rows/groups and aggregates pending state. |
| Missing class falsifiers | The current code has tests for individual conditional mutations, but the cross-entry, conflict-replacement, full pin, legacy command-owner, full equality, and independent-progress matrices are not all executable guards. | The acceptance matrix below is mandatory and exact. |

Review inputs:

- `.scratch/external-reviews/r2-claim.out`, SHA-256
  `1F7DBAF2CC49F367491814D8ADDC4FB759FDA1676CB2BE7E88C79AF98927EDE0`;
- `.scratch/external-reviews/r2-adversarial.out`, SHA-256
  `763FD04D7C3D59C914DB0C8E06B7F4260191C0B6F7A6C8C6CFE155280ABAFA9F`.

## Finding classes and preserved decisions

The ten original classes remain the scope boundary.

| Class | State | R3 contract |
| --- | --- | --- |
| C1 legacy LSP rollback identity | REAL | Exact canonical and every removable legacy entry remain separate immutable rows. |
| C2 latest applied port | REAL | Ownership is per-row `Applied.PostState`; no report-wide port authorizes an inverse. |
| C3 `--check` dispatch | ALREADY FIXED, protected | Read-only mode remains rejected before reconcile dispatch. |
| C4 Serena rollback ownership | REAL | Serena inverse remains compare-and-swap against the exact applied fingerprint and exact verified baseline bytes. |
| C5 operation serialization | ALREADY FIXED, protected | One reconcile operation lock spans report read, all client mutations, dispositions, and retirement. |
| C6 LSP readiness | ALREADY FIXED, protected | Total Serena and LSP route preflight remains before the first client write. |
| C7 route-owned cleanup | ALREADY FIXED, protected | Route-process cleanup remains owned by the route daemon. |
| C8 Serena success promotion | REAL | Only a same-call post-invocation observation may create an applied receipt. |
| C9 absent/unreachable rollback | REAL | A row requiring an inverse stays pending while its client is unreachable. |
| C10 snapshot/apply population | REAL | Planning freezes the client population and exact rows; apply never re-enumerates. |

Protected C3/C5/C6/C7 surfaces are not implementation latitude. Any edit to
`internal/cli/install.go`, `internal/cli/route.go`, reconcile operation-lock
scope, or total route preflight requires a new admitted finding and re-review.

## Invariants

The R3 implementation preserves these non-negotiable invariants:

- **I1 — immutable first baseline.** A row's first exact baseline never changes
  across retries or ports.
- **I2 — row-only authority.** Only the version-3 `Rows` map authorizes
  mutation or rollback. No top-level compatibility projection, report port, or
  regenerated plan substitutes for row evidence.
- **I3 — frozen population.** A forward generation mutates only the exact client
  and row set captured in its durable plan.
- **I4 — causation, not equality.** State equality after process re-entry never
  proves that the interrupted invocation wrote it.
- **I5 — durable prepare before invocation.** A potentially mutating adapter call
  starts only after its exact row attempt and required Serena pin are durable.
- **I6 — exact receipt.** An applied receipt records the exact post-state
  observed in the same wrapper call and the generation that caused it.
- **I7 — compare-and-swap inverse.** Rollback mutates only when live state still
  equals the row's effective applied receipt.
- **I8 — route preservation.** A legacy LSP entry is removed only while the exact
  canonical route dependency is live under the same config lock. A canonical
  rollback inverse runs only while every required legacy route is live under
  that same lock.
- **I9 — exact verified Serena bytes.** The bytes hashed during pin validation
  are the only bytes an inverse may consume.
- **I10 — monotonic recovery.** A terminal row never becomes non-terminal; only
  rollback may advance a non-terminal disposition to a terminal one. Once any
  rollback disposition exists, forward admission refuses both same-plan replay
  and changed-plan replacement until explicit rollback retires the report. An
  uncertain row cannot be hidden by a new generation; retirement requires a
  durable re-read proving all rows terminal.
- **I11 — independent rollback progress.** Uncertainty in one Serena row or one
  LSP `(client, language)` group cannot suppress a safe inverse in another
  independent row/group.
- **I12 — legacy refusal.** Version-1 and version-2 artifacts are read-only
  refusals in both forward and rollback modes: exact artifact bytes and client
  mutation counts remain unchanged.

## Decision A: one multi-entry authorization owner

`internal/clients/config_lock.go` owns a new generic wrapper-only capability,
provisionally named `ConditionalEntryGroupMutation`. Concrete adapters must not
implement it and callers must not emulate it with multiple reads.

One request contains:

- one target entry, exact target predicate, add/remove operation, optional
  backup, and durable `BeforeMutation` callback;
- an ordered set of dependency entry names with exact predicates;
- no path supplied by the caller: all target and dependency entries belong to
  the wrapped client's one `ConfigPath`.

`lockingClient` executes this sequence inside one `withConfigLock` call:

1. read the target and every dependency;
2. reject a target or dependency mismatch with `Invoked=false`;
3. capture the optional backup;
4. durably prepare the target row;
5. invoke exactly one target mutation through the wrapped concrete adapter;
6. read back the target and dependencies before releasing the lock;
7. return the complete observation.

The primitive mutates one target. “Multi-entry” describes its authorization
set, not a multi-write transaction. This keeps one durable row per write while
making a dependency a real mutation precondition.

Forward LSP policy:

- canonical add/replace uses target-only authorization;
- each legacy remove adds a dependency predicate requiring the canonical entry
  to equal the exact intended front-route state;
- a failed dependency predicate persists a no-write conflict for the legacy row
  and performs no legacy removal.

Rollback LSP policy:

- restore legacy rows first within each `(client, language)` group;
- the canonical inverse adds dependency predicates for every legacy baseline
  row that must preserve routing;
- each dependency must equal its exact routable baseline immediately before
  the canonical inverse;
- a failed dependency predicate leaves the canonical row pending or
  `skipped-dependency-conflict` and performs no canonical mutation.

This replaces the in-memory `canonicalReady` authorization role. It may remain
only as reporting/ordering state; it cannot authorize a write.

## Decision B: one uncertainty classifier, two caller policies

`internal/cli/install_reconcile_mcp_front.go` owns one pure classifier for every
row attempt. It is the sole definition of ownership certainty.

| Durable attempt state | Classification | Effective receipt |
| --- | --- | --- |
| no attempt | settled | existing `Applied`, if any |
| `prepared` | uncertain-post-invocation | none |
| `post-write-conflict` (the explicit R3 name for current `conflict`) | uncertain-post-invocation | none |
| `precondition-conflict` with `Invoked=false` | settled-no-write-conflict | existing prior `Applied`, if any; never a new receipt |
| `confirmed-no-write` | settled-no-write | previous `Applied`, if any |
| `applied` with receipt | settled-applied | exact receipt |
| unknown state or `applied` without receipt | invalid/uncertain | none |

The classifier is policy-free. Two callers consume it:

- **Forward admission policy:** first refuse when any row has any rollback
  disposition, whether pending or terminal and whether the requested plan is
  byte-identical or changed. Until explicit rollback/retirement, generation,
  `ActivePlan`, row bytes, pins, and client configs remain unchanged; the
  discriminator is `forward-recovery-disposition-active`. Otherwise any
  uncertain or invalid row blocks generation increment, active-plan
  replacement, pin replacement, and every client mutation. The prior report may
  be updated only to make uncertainty explicit; its discriminator is
  `forward-previous-attempt-uncertain`.
- **Rollback policy:** uncertain/invalid Serena rows become
  `pending-ownership-unknown`; an uncertain/invalid LSP row blocks only its
  `(client, language)` dependency group. Rollback continues all independent
  groups and returns one aggregate pending error after durable dispositions and
  verification.

`settleMCPFrontReconcileAttempts` therefore becomes a classification and
durable-marking pass returning structured row/group results. It must not own the
forward-versus-rollback decision and must not return early merely because a row
is uncertain.

The attempt-settled/committed event is the durable row write containing either
an `Applied` receipt or a same-call no-invocation result. Row terminality is a
separate disposition lifecycle: a no-invocation conflict with prior ownership
is attempt-settled but remains rollback-pending. Adapter success, callback
return, or in-memory state is not a commit event.

There are no automatic adapter retries and no backoff loop. One admitted plan
operation invokes its adapter at most once. Operator command re-entry is the
only retry mechanism, and it must pass the durable classifier and forward
admission policy before any new invocation.

## Decision C: open, validate, and consume Serena pins once

The CLI reconcile owner loads all rollback pins before the first inverse. The
loader returns an immutable map from row key to `VerifiedSerenaPin{Bytes}`; it
does not return an authority-bearing path.

For each restorable or ownership-uncertain Serena row, the CLI validates:

1. validate row/client/pin agreement, uniqueness, declared pin-set completeness,
   and the absence of pins on LSP rows;
2. enforce lexical containment as an early diagnostic only;

It then passes only `(context, pin root, relative components)` to the API
state-read owner. The API primitive, provisionally
`ReadStateFileBeneathRootNoFollow`, must:

3. open and validate the pin root without following a link/reparse point;
4. open every later component relative to the already-validated parent handle
   and reject a link/reparse object before using it as the next parent;
5. require directory objects for intermediates and one regular,
   non-directory final object;
6. read the final handle once through
   `io.LimitReader(handle, maxStateFileBytes+1)`, reject a result above the
   existing 1 MiB cap, hash those exact bytes, compare the recorded SHA-256, and
   retain those bytes in memory;
7. close every root, intermediate, and final handle on success, error,
   cancellation, and size/hash refusal before any client mutation.

Platform owners:

- **Windows:** `internal/api/state_read_beneath_root_windows.go` is a
  root-anchored handle-relative component walker. It opens the root with
  `windows.NtCreateFile`, `FILE_OPEN_REPARSE_POINT`, directory-only options, and
  `OBJECT_ATTRIBUTES.Attributes` including `OBJ_DONT_REPARSE`; then opens each
  child with `OBJECT_ATTRIBUTES.RootDirectory` set to the validated parent
  handle. Every handle is inspected for
  `FILE_ATTRIBUTE_REPARSE_POINT`; intermediates must be directories and the
  final object must be regular/non-directory. Final-handle
  `GetFileInformationByHandle` and `GetFinalPathNameByHandle` volume/root checks
  are defense-in-depth after handle-relative authorization, not substitutes for
  it.
- **POSIX:** `internal/api/state_read_beneath_root_posix.go` opens the root
  directory with no-follow semantics, walks each component with
  `openat(parentFD, component, O_NOFOLLOW|O_CLOEXEC)` using `O_DIRECTORY` for
  intermediates, and opens the final leaf read-only with `O_NOFOLLOW`. `fstat`
  must prove a regular final file; the root-relative descriptor chain is the
  containment authority.

Installed-surface evidence pins this implementation choice:

- `go.mod:16` selects `golang.org/x/sys v0.46.0`;
- `$GOMODCACHE/golang.org/x/sys@v0.46.0/windows/types_windows.go:2942-2963`
  exposes `OBJECT_ATTRIBUTES.RootDirectory` and `OBJ_DONT_REPARSE`;
- `$GOMODCACHE/golang.org/x/sys@v0.46.0/windows/types_windows.go:2992-3013`
  exposes `FILE_OPEN_REPARSE_POINT` and directory/non-directory options;
- `$GOMODCACHE/golang.org/x/sys@v0.46.0/windows/zsyscall_windows.go:3730`
  exposes `NtCreateFile`;
- `$GOMODCACHE/golang.org/x/sys@v0.46.0/windows/syscall_windows_test.go:1127-1164`
  demonstrates a child `NtCreateFile` with `RootDirectory` bound to an open
  directory handle.

The size-limit owner is not an assumption: the API state-read package already
defines `maxStateFileBytes = 1 << 20` for ordinary control files
(`internal/api/state_read_caps.go:9-11`). The new API primitive resides in that
package and uses this exact constant; the CLI must not duplicate or override
it.

`Lstat`, `EvalSymlinks`, lexical/absolute pathname checks, a final-pathname
containment check, and an absolute final open are forbidden as authorization.
They may exist only as diagnostics or defense-in-depth after handle-relative
authorization. In particular, no “check pathname, then open absolute path”
sequence satisfies this contract.

Serena inverse requests take `BaselineBytes []byte`; the version-3 rollback path
must not pass or reopen `BackupPath`. Diagnostic `Origin` and `Path` remain in
the journal but cannot authorize a read. Replacing a pin pathname after loading
cannot change the bytes consumed by the inverse.

The declared pin set is exact: every row requiring a possible inverse has one
unique pin; LSP rows and first-generation authority-free Serena
precondition-conflict rows have none. A precondition-conflict row retaining a
prior `Applied` receipt also retains its required pin. Undeclared pin objects
other than the writer's defined lock sidecars are an error before any client
mutation.

## Decision D: durable first-generation Serena no-write conflict

A first-generation Serena row may be pinless only in this exact shape:

- `Surface=serena`, exact client and entry identity, `BaselineSet=true`;
- no `Pin` and no `Applied` receipt;
- `Attempt.State=precondition-conflict`, `Invoked=false` by contract;
- exact planned pre-state and intended state retained in the attempt;
- terminal disposition `skipped-conflict` with reason
  `forward-plan-precondition-conflict`.

`finishAttempt` must create and persist this row even when no prior row exists.
The validator accepts the pinless shape only when every condition above holds.
It authorizes no rollback inverse and can never be promoted from later equality.

When a row already has an `Applied` receipt, a same-call no-invocation
`precondition-conflict` means only “this attempt wrote nothing.” `finishAttempt`
must retain the prior receipt and its row-owned pin byte-for-byte. It writes the
new attempt plus non-terminal disposition `pending` with reason
`forward-precondition-conflict-prior-owned`. Rollback may authorize an inverse
only from that retained prior receipt: matching live state permits exactly one
compare-and-swap inverse; diverged live state permits zero inverse writes and
records conflict. It must never synthesize a receipt from the conflicting
attempt.

Thus a forward-time no-write conflict always writes a disposition:

- without a prior receipt it writes terminal `skipped-conflict` and carries no
  inverse authority;
- with a prior receipt it writes non-terminal `pending`, retains that receipt
  and pin, and requires explicit rollback to settle the older ownership.

Forward admission cannot clear either shape. Explicit rollback is the only
transition owner: it retires an all-terminal first-generation conflict report,
or processes the retained prior receipt and advances the pending row to a
terminal restored/conflict disposition before retirement.

Any Serena row that might have invoked a mutation (`prepared`,
`post-write-conflict`, or `applied`) requires its row-owned pin. A missing pin in
those states is invalid and blocks all writes.

## Decision E: rollback completes independent safe work

Rollback phases are:

1. decode and validate the strict version-3 report;
2. classify and durably mark uncertain rows without returning early;
3. securely load all pins required by rows that are eligible for a Serena
   inverse;
4. process Serena rows independently in stable key order;
5. process LSP dependency groups independently in stable group order;
6. persist every disposition and verify every invoked inverse;
7. durable re-read; retire only if every row is terminal;
8. otherwise return one aggregate pending/failed error listing all remaining
   rows after all independent safe work has run.

A pin error is a global pre-write failure because pin validity is the rollback
input contract. Runtime uncertainty, client unreachability, compare-and-swap
conflict, or dependency conflict is row/group-local and must not suppress other
independent work.

## Locking and state order

The mandatory lock order is:

1. reconcile operation lock;
2. one client config-file lock;
3. journal/report state-file lock used by the durable callback;
4. release journal lock;
5. perform the target client mutation while the config lock remains held;
6. release config lock;
7. eventually retire under the still-held operation lock.

No code may acquire the operation lock or a second config lock while holding a
config lock. Secure pin loading occurs under the operation lock before any
config lock and retains bytes, not open handles. Journal callbacks may write the
report while holding one config lock because no report path calls back into a
client. These constraints preserve the existing C5 serialization boundary and
avoid lock inversion.

## Single owners and change-surface contract

| Concern | Single owner |
| --- | --- |
| Config-file critical section and multi-entry authorization | `internal/clients/config_lock.go` |
| LSP forward dependency predicates and frozen plan | `internal/api/lsp_client_router.go` |
| LSP rollback groups, dependency predicates, and exact inverses | `internal/api/lsp_client_router_snapshot.go` |
| Serena exact-byte compare-and-swap inverse | `internal/api/serena_client_reconcile.go` |
| Version-3 schema, uncertainty classification, forward/rollback policy, secure pin orchestration, retirement | `internal/cli/install_reconcile_mcp_front.go` |
| Windows root-handle-relative pin read and ordinary-file size cap | `internal/api/state_read_beneath_root_windows.go`, using `internal/api/state_read_caps.go:9-11` without changing that owner |
| POSIX root-FD-relative pin read and ordinary-file size cap | `internal/api/state_read_beneath_root_posix.go`, using `internal/api/state_read_caps.go:9-11` without changing that owner |

Allowed production files are exactly the seven changed/new files in that table;
`state_read_caps.go` is cited as an unchanged owner, not an allowed edit. New
production files are limited to the two API platform pin readers. Allowed tests:

- `internal/clients/config_lock_wrapped_test.go`;
- `internal/api/lsp_client_router_plan_test.go`;
- `internal/api/lsp_client_router_snapshot_review_test.go`;
- `internal/api/serena_client_reconcile_test.go`;
- `internal/cli/install_reconcile_mcp_front_v3_test.go`;
- `internal/cli/install_reconcile_mcp_front_pr588_r2_test.go`;
- new narrowly named API platform beneath-root reader tests.

No edit is allowed in `internal/cli/install.go`, `internal/cli/route.go`,
scheduler/GUI/supervisor code, state-path resolution, or `.codegraph*`.
Widening requires architect re-review before implementation.

## Falsifiable claims

| Claim | Guarantee | Single owner | Enforcement probe |
| --- | --- | --- | --- |
| R3-1 | A legacy remove cannot race past a changed canonical route. | `lockingClient` group mutation | Inject a canonical edit at the dependency boundary; legacy mutation count is zero and a route remains. |
| R3-2 | A canonical rollback inverse cannot race past a changed required legacy route. | `lockingClient` group mutation | Delete/replace legacy at the dependency boundary; canonical mutation count is zero and canonical remains. |
| R3-3 | Every post-invocation uncertain state blocks forward plan replacement. | CLI uncertainty classifier + forward policy | Persist `prepared` and `post-write-conflict` variants; changed-plan retry preserves generation and active-plan bytes with zero adapter calls. |
| R3-4 | Rollback uncertainty is local, not a global early return. | CLI rollback policy | One uncertain row/group plus one independent applied row/group leaves the former pending and restores the latter. |
| R3-5 | A Serena inverse consumes the bounded bytes read and hashed from one final validated handle. | API beneath-root reader + CLI pin loader | Replace the path after load but before inverse; inverse receives original verified bytes and never reopens the path. |
| R3-6 | Root, intermediate, and final links/reparse points cannot escape pin-root authority. | API platform beneath-root readers | Inject each reparse location and a check/open component swap; refusal precedes every client write and all handles close. |
| R3-7 | First-generation Serena precondition conflict is durable and non-owning. | CLI journal | With no prior row, inject an intervening edit; row-only conflict is durable, pinless, performs zero writes, and rollback invokes no inverse. |
| R3-8 | Equality cannot manufacture causation. | CLI same-call finish transition | For each add/remove surface, re-entry equality variants retain uncertainty or prior receipt and cause zero rollback mutation. |
| R3-9 | Legacy artifacts remain immutable refusals. | CLI decoder/command owner | Forward and rollback command tests for v1/v2 preserve exact bytes and make zero adapter calls. |
| R3-10 | Retirement cannot erase incomplete recovery. | CLI durable retirement gate | Any pending/failed/uncertain row keeps the active report; all-terminal durable re-read is the sole retirement proof. |
| R3-11 | A no-invocation conflict creates no new ownership but cannot erase older ownership or be cleared by forward retry. | CLI finish transition + admission policy | Prior receipt survives conflict and is the sole rollback CAS authority; first generation remains authority-free; same/changed forward replay preserves report bytes with zero invocations. |

## Deterministic acceptance matrix

All tests below are mandatory. Hooks/seams must create race windows
deterministically; timing-only tests do not satisfy the contract.

### F2 causation/equality table

For each surface `Serena add`, `LSP add`, and `LSP remove`, test:

| Re-entry state | Durable attempt | Expected effective ownership | Rollback mutation count |
| --- | --- | --- | --- |
| live equals intended; prior receipt absent | `prepared` | uncertain, no receipt | 0 |
| live equals pre-state; prior receipt absent | `prepared` | uncertain, no receipt | 0 |
| live equals intended; prior receipt exists | `prepared` | uncertain, prior receipt not promoted | 0 |
| live equals pre-state; prior receipt exists | `confirmed-no-write` produced in same call only | prior receipt retained | inverse authorized only against that prior receipt |
| same-call post-state equals intended | `applied` plus exact receipt | current receipt | 1 only when CAS still matches |
| same-call post-state differs from pre and intended | `post-write-conflict` | uncertain, no new receipt | 0 |
| wrapper rejects precondition; prior receipt absent | `precondition-conflict` | settled no-write conflict, no receipt | 0 |
| wrapper rejects precondition; prior receipt exists | `precondition-conflict` | prior receipt retained; no new receipt | inverse only against prior receipt |

### Conflict and caller-policy matrix

- `prepared` blocks changed-plan forward retry: generation, active-plan bytes,
  pins, and adapter counts unchanged.
- `post-write-conflict` blocks the same retry with those same assertions.
- Unknown attempt state and `applied` without receipt fail closed.
- Any pending or terminal disposition blocks both byte-identical and changed-plan
  forward re-entry: exact generation, `ActivePlan`, row/pin bytes, and adapter
  counts remain unchanged.
- Prior `Applied` plus a later no-invocation `precondition-conflict`, with live
  state equal to the prior receipt, authorizes exactly one inverse against only
  that receipt; diverged live state authorizes zero inverse writes.
- First-generation `precondition-conflict` remains pinless and inverse-free.
- Repeated command entry has zero hidden retries/backoff, at most one admitted
  adapter invocation per row, and one durable settled/committed attempt event.
- One uncertain Serena row plus an independent applied Serena row: uncertain
  stays pending; independent row restores and verifies.
- One uncertain LSP group plus an independent applied LSP group: uncertain group
  stays pending; independent group restores and verifies.

### Multi-entry dependency races

- Forward: change/remove canonical immediately after target read but before
  legacy authorization. The wrapper observes the dependency conflict under the
  one lock, invokes no legacy remove, and at least one route remains.
- Rollback: delete, disable, or replace a required legacy entry immediately
  before canonical authorization. The wrapper invokes no canonical inverse and
  canonical remains.
- A test adapter that cannot provide the wrapper-owned group capability fails
  closed with zero target mutations.

### Serena pin matrix

Each case fails before the first client mutation unless the expected outcome
explicitly says otherwise:

- missing required pin;
- extra undeclared pin object;
- pin attached to an LSP row;
- duplicate path shared by two rows;
- lexical path escape;
- pin root itself is a symlink/reparse point;
- an intermediate component is a symlink/reparse point;
- the final component is a symlink/reparse point;
- a component is swapped after a pathname diagnostic but before the child open;
- unreadable or non-regular final object;
- final object above `maxStateFileBytes` (1 MiB);
- checksum mismatch;
- row/client/path metadata disagreement;
- pin-set disagreement with the exact restorable Serena row set;
- path swapped after secure load but before inverse: inverse consumes the
  retained verified bytes and does not read the replacement;
- every refusal closes root, intermediate, and final handles and occurs before
  the first client mutation;
- pinless first-generation `precondition-conflict`: accepted only in the exact
  no-write shape and never passed to an inverse.

### Version and schema matrix

- Command-owner forward and rollback tests for version 1 and version 2 assert
  exact report-byte equality and zero client writes.
- The same command-owner tests assert the refusal is ACTIONABLE, not merely
  correct: all six elements listed under "The upgrade-in-place question,
  answered" must be present in the message, and the artifact must survive
  byte-identical with no retired sibling. A test asserting only that an error
  occurred does not satisfy this row.
- A version-2 body carrying `version: 3` (the interim pre-release build) is
  refused through the SAME message, not through the raw unknown-field error.
- Strict version-3 decode rejects unknown top-level compatibility projections,
  trailing values, incomplete rows, malformed attempt/receipt combinations,
  and pins outside their permitted row shapes.
- Rollback-side fixtures are built from the production journal structs, never
  hand-written literals: a hand-written body silently moves the failure to the
  decoder and stops exercising the semantic refusal the test names.
- A completed rollback with any pending durable row cannot retire.

### Preserved class guards

Existing tests for C3, C5, C6, and C7 remain green. C1/C2/C4/C8/C9/C10 tests
remain class-level: every removable legacy candidate is captured/restored,
applied ports are row-specific, Serena restore is CAS-owned, receipt promotion
is post-success, unreachable required rows stay pending, and a newly appearing
client cannot bypass the frozen plan.

## Failure modes and observable discriminators

| Failure mode | Required discriminator | Effect |
| --- | --- | --- |
| Target entry changed | `entry-precondition-conflict` | No mutation; durable no-write conflict. |
| LSP dependency changed | `dependency-precondition-conflict` | No target mutation; affected group pending/conflict. |
| Mutation invoked but ownership unresolved | `forward-ownership-unknown` | Durable `post-write-conflict`; blocks forward admission. |
| Re-entered prepared attempt | `forward-previous-attempt-uncertain` | Blocks forward; rollback localizes pending state. |
| Any rollback disposition exists on forward entry | `forward-recovery-disposition-active` | Refuse same/changed plan with report bytes and client state unchanged until explicit rollback/retirement. |
| First-gen Serena conflict could not persist | `serena-precondition-conflict-not-durable` | Fail command; no mutation; no success report. |
| Pin set malformed or incomplete | `serena-pin-set-invalid` | Global rollback pre-write refusal. |
| Pin path contains link/reparse escape | `serena-pin-open-unsafe` | Global rollback pre-write refusal. |
| Pin exceeds ordinary state-file cap | `serena-pin-too-large` | Global rollback pre-write refusal at 1 MiB; all handles closed. |
| Pin bytes mismatch | `serena-pin-checksum-mismatch` | Global rollback pre-write refusal. |
| Row/group unavailable or uncertain | `rollback-row-pending` | Continue independent work; retain report. |
| Durable disposition write fails | `rollback-disposition-not-durable` | Stop before that row's mutation; retain report. |
| Retirement re-read is not all-terminal | `rollback-recovery-active` | No retirement; aggregate pending result. |
| Version 1/2 encountered | `legacy-ownership-unproven` | Exact bytes preserved; zero client writes. |

Errors must retain the row key or `(client, language)` group and causal error.
No catch-and-swallow or synthesized ownership fallback is allowed.

## Compatibility and migration

Version 3 is unshipped on the PR remote head, so R3 revises version 3 in place;
there is no version-3-to-version-3 migration. Any artifact produced by an
interim local R2 implementation must pass the R3 strict validator or be refused
without writes. It must not be silently repaired, projected, or upgraded.

Version 1 and version 2 remain inspectable only for the
`legacy-ownership-unproven` diagnostic. Forward and rollback do not modify,
replace, retire, or append to them. The user must explicitly move a legacy
artifact aside or use a separately reviewed migration tool.

### The upgrade-in-place question, answered (added 2026-07-27)

Earlier revisions stated the refusal without stating what an operator who
already has a legacy journal on disk is supposed to do about it. That omission
is what let a strict unknown-field decoder ship whose entire field-facing
answer to a real upgrade was `json: unknown field "lsp"`. The gap is closed
here, not left to the implementation.

**Decision: version 3 REFUSES a version-1/2 journal. It never upgrades one in
place, and there is no compatibility shim.** The reason is not schema
tidiness. A version-1/2 journal records which client entries were *captured*;
it carries no per-row attempt, no same-call applied receipt, and no generation,
so it cannot say which client write actually *landed*. Synthesising version-3
rows from it would manufacture rollback authority that was never proven, and
the first thing that authority does is overwrite a live client entry. Refusing
costs the operator one manual step; upgrading can silently destroy an entry
this hub never wrote. This is the same reason `I4 — causation, not equality`
forbids promoting live equality to a receipt.

**The refusal is part of the contract, not an implementation detail.** A
refusal that the operator cannot act on is a field defect of the same class as
a wrong upgrade, because the practical outcome is identical: their clients stay
on the front port with no supported way back. Every legacy refusal MUST carry,
in one message:

1. the discriminator `legacy-ownership-unproven`;
2. the artifact's absolute path;
3. why the upgrade is refused rather than attempted — that the old format never
   recorded which write landed;
4. that nothing was read from the file and no client config was touched;
5. both concrete remedies: (a) run `--rollback` with the OLDER mcphub binary
   that wrote the file, which understands the format; or (b) move the file
   aside and restore the recorded entries by hand, naming where in the file
   those entries are;
6. that a fresh forward run then starts a clean version-3 journal.

**Four inputs, one owner, two opposite remedies.** A decoder that keys only on
the `version` field misses the third row below; a refusal that assumes "foreign
version means older" gets the fourth actively wrong.

| Input | How it is recognised | Outcome |
| --- | --- | --- |
| genuine version-1 journal | `version` field | refusal, detail `it declares version 1`, OLDER-binary remedy |
| genuine version-2 journal | `version` field | refusal, detail `it declares version 2`, OLDER-binary remedy |
| interim pre-release build: `version: 3` with a version-2 body | strict decode fails AND a version-1/2 top-level body key (`serena`, `lsp`, `pins`, `port`) is present | refusal, detail `carries the pre-version-3 body shape`, OLDER-binary remedy |
| version ABOVE 3 | `version` field | refusal naming the artifact version, NEWER-binary remedy |

The last row is not symmetry for its own sake. A version above 3 means the
operator downgraded, or is running a different install than the one that wrote
the file; the binary that can read it is the NEWER one. Sending them to "the
older mcphub that wrote this file" points at the one binary guaranteed not to
understand it, so the remedy is selected from the declared version and never
assumed.

Bytes that are none of the four — a corrupt or truncated version-3 journal —
keep the exact strict-decoder error, which is the honest diagnostic for a file
that is not a foreign-version journal at all.

**Single owner.** One function owns this refusal text, and both the decoder's
version gate and the validator's defence-in-depth version check route through
it, so the operator never meets two refusals that disagree about what to do
next.

The external command shape and the already-fixed `--check` contract do not
change. There is no new dependency, service, schema outside the recovery
artifact, or cross-platform behavioral fork.

## Alternatives rejected

- **Keep point-in-time dependency checks.** Rejected because the dependency can
  change before the later target mutation.
- **Batch all LSP writes into one opaque transaction.** Rejected because the
  journal needs one durable row/receipt per write and adapters expose one config
  owner, not a portable multi-file database transaction.
- **Treat live equality as an interrupted-write receipt.** Rejected because it
  proves state, not causation.
- **Block rollback globally on any uncertain row.** Rejected because it destroys
  independent safe recovery progress.
- **Reopen a verified pin path in the API.** Rejected because pathname identity
  is mutable and lexical containment is not object containment.
- **Give every no-write conflict a dummy pin.** Rejected because it fabricates
  rollback authority for a mutation that never ran.
- **Auto-upgrade version 1/2 or interim malformed version 3.** Rejected because
  missing row ownership cannot be reconstructed safely.

## Source-based hypotheses and residuals

No unverified premise authorizes an implementation change in this decision.
The following source-based hypotheses are explicit and have named falsifiers:

- **H1:** canonical and legacy entries for one LSP `(client, language)` share the
  same wrapped client's config path. Evidence: the frozen plan binds one
  `clientMap` adapter per client (`internal/api/lsp_client_router.go:127-137`,
  `internal/api/lsp_client_router.go:286-310`). Falsifier: a test adapter whose
  dependency resolves to a different config owner must be refused before write.
- **H2:** the current per-file wrapper is the only cross-process client-config
  lock owner. Evidence: `ConditionalEntryMutator` is wrapper-only
  (`internal/clients/config_lock.go:228-248`). Falsifier: mechanism inventory
  finds a second production entry-mutation path bypassing `lockingClient`; that
  finding blocks implementation until routed through the owner.
- **H3:** version 3 has not shipped on the PR remote. Evidence: the current local
  R2 commit is `31b9ca94`, while both external reviews are against that local
  R2 tree and classify R3 as not yet implemented. Release/publication state
  must be re-probed before any later migration decision.

Residual risks after implementation:

- an operator can still edit a client config between independent groups; CAS
  and dependency authorization turn this into visible pending/conflict state;
- in-memory pin bytes are bounded per row by the API state-read owner's existing
  `maxStateFileBytes` 1 MiB cap (`internal/api/state_read_caps.go:9-11`);
- a process crash after a client mutation but before durable receipt remains an
  uncertain row by design; manual resolution is safer than manufactured
  ownership;
- target-platform secure-open behavior remains implementation evidence, not an
  architecture assumption; Windows and POSIX root/intermediate/final
  link/reparse, component-swap, size-bound, and handle-leak tests must pass
  before the implementation architecture gate can close.

## Planner-ready gate

**Planner status: BLOCKED pending repeat reliability review `PASS`.**

After that gate, planning may start only from this R3 file and must preserve the allowed
production/test surfaces, lock order, state table, and complete acceptance
matrix. Implementation acceptance requires:

1. reliability review `PASS`;
2. planner artifact mapping every invariant and falsifier to one implementation
   step and one executable test;
3. implementation within the declared change surface;
4. mutation evidence that each mandatory falsifier fails against the reverted
   or deliberately mutated implementation and passes after restoration;
5. scoped tagged tests, build, vet, and independent architecture review;
6. no push without human publication review.

## Terms and Abbreviations

- **ADR** — architecture decision record.
- **CAS** — compare-and-swap: mutate only if current state equals expected state.
- **CLI** — command-line interface.
- **LSP** — Language Server Protocol.
- **POSIX** — portable operating-system interface used by Unix-like targets.
- **Receipt** — durable evidence of the exact post-state observed in the same
  mutation call.
- **Reparse point** — Windows filesystem object that can redirect path
  resolution.
- **Row** — one exact `(surface, client, language, entry)` recovery authority.
- **Serena** — the Serena Model Context Protocol client entry surface.
- **TOCTOU** — time-of-check/time-of-use race.

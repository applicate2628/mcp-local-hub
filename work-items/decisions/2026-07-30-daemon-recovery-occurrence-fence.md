- id: 2026-07-30-daemon-recovery-occurrence-fence
- status: proposed
- date: 2026-07-30
- decided-by: architect through external-worker; architecture-reviewer promotion pending
- context: PR589 durable daemon-recovery occurrence fence
- supersedes: none
- superseded-by: none

# Daemon recovery occurrence state has one adapter owner and a fail-closed versioned schema

## Decision

`internal/gui.auditLockAdapter` is the sole writer-owner of daemon-recovery
occurrence state. Its authority covers the version-1
`daemon-recovery-occurrences.json` store, the exact per-task admission fence,
and a private current-generation uncertainty overlay used only when a terminal
store transaction cannot be proven. No route, Dashboard component, or backend
recovery function may write or independently reinterpret occurrence state.

The canonical effective state for one record is resolved in this order:

1. A durable terminal, uncertain, or consumed record is authoritative.
2. A matching current-generation uncertainty overlay dominates only a durable
   `in_flight` record and projects `uncertain`.
3. Otherwise the durable record is projected unchanged.

The overlay is keyed by store generation plus the complete correlation tuple
and is mutated under the same adapter serialization boundary as the store. A
terminalization failure installs it before the adapter releases its
in-process store lock. Exact replay, lookup, snapshot, acknowledgement, and
same-task admission all consume the effective record resolver rather than
calling `auditLockReceiptFromRecord` directly. A successful durable
terminalization or acknowledgement removes the matching overlay only after
the durable write succeeds. Adapter close discards memory; the next startup
claim converts every prior-generation durable `in_flight` record to durable
`uncertain` before serving requests, preserving the same fail-closed truth
after restart.

The one downstream-observable state event remains the existing
`audit-lock-state` Server-Sent Events event. The adapter emits that event after
an effective occurrence transition; it is a lossy notification only.
Authoritative `GET /api/daemon/recover/audit-lock-state` and exact replay both
resolve through the same adapter owner.

Dashboard admission remains task-keyed. An unresolved record for task A fences
task A but cannot veto task B. The Dashboard retains its task-keyed correlation
map and same-task reconciliation branch; the global
`pendingRecoveriesRef.current.size > 0` admission veto is removed. The durable
adapter remains the final admission authority, so concurrent or stale browser
actions cannot bypass the at-most-once fence.

Version-1 persisted enum validation is exact:

- occurrence status is owned by the adapter's six status constants;
- lock authorization is owned by `auditLockAuthorization` and its exact
  cross-field rules;
- persisted recovery error identifiers are owned by one typed Go
  `daemonRecoverErrorCode` set beside `daemonRecoverHTTPFailure`;
- `port_owner_check`, `port_wait_outcome`, and `audit_handoff` are accepted
  only through `daemonrecovery.PortOwnerCheck.Valid`,
  `daemonrecovery.PortWaitOutcome.Valid`, and
  `daemonrecovery.AuditHandoff.Valid`.

`validateAuditLockStore` remains the single schema validator called after
decode and before every write. It validates membership and the cross-field
relationship among status, HTTP status, error code, success evidence,
termination commitment, audit handoff, and lock authorization. Shape-only
validation is not an enum-membership decision.

Evidence: the current store owner and schema are
`internal/gui/audit_lock_state.go:25-146`; all read and write routes converge
on `readStoreLockHeld` and `writeStoreLockHeld` at
`internal/gui/audit_lock_state.go:669-699`; current terminal failure returns an
in-memory uncertain receipt at `internal/gui/audit_lock_state.go:844-894`;
restart claims prior-generation `in_flight` as `uncertain` at
`internal/gui/audit_lock_state.go:702-740`; exact task admission is already
enforced at `internal/gui/audit_lock_state.go:799-811`; the contradictory
Dashboard global veto is `internal/gui/frontend/src/screens/Dashboard.tsx:1138-1160`.

## Rationale

The versioned receipt file is a cross-process, restart-surviving safety
contract rather than a local cache. The route reserves before destructive
recovery and the Dashboard never repeats the destructive POST
(`docs/supervisor-architecture.md:67-92`). A terminal store failure currently
returns `uncertain`, but lookup and snapshot rebuild the unchanged durable
`in_flight` bytes (`internal/gui/audit_lock_state.go:897-921` and
`internal/gui/audit_lock_state.go:1002-1033`). Exact replay then reports
`recovery_in_flight` (`internal/gui/daemon_recover.go:302-323`). One adapter
owner with one effective-record resolver removes that split without moving
recovery execution or storage into the frontend.

The task key is already the durable fence domain
(`internal/gui/audit_lock_state.go:805-807`). A process-wide browser veto is a
second, broader policy owner and blocks unrelated work. Removing only that
veto preserves the existing task fence and keeps admission at the durable
transaction boundary.

The persisted success fields are emitted from backend-owned enum values
(`internal/gui/daemon_recover.go:207-218`), whose exact valid sets are defined
at `internal/daemonrecovery/recovery.go:17-126`. Accepting any bounded
identifier-shaped string lets corrupt disk bytes become a new wire producer.
Calling the existing owners' `Valid` methods preserves forward evolution:
adding a value at its actual owner automatically makes it acceptable to the
store without copying a list into the schema layer.

## Consequences

- Immediate response, same-process GET, exact replay, acknowledgement, and
  restarted GET share one effective receipt resolver and cannot regress an
  observed terminalization failure to `in_flight`.
- The durable bytes remain untouched when a failing write cannot be proven.
  The overlay is not an outbox, retry queue, instruction, or second source of
  policy; it is a monotonic fail-closed projection owned inside the existing
  adapter.
- Task A remains fenced by both the Dashboard's task-keyed correlation and the
  adapter's durable per-task check. Task B is independently admissible.
- Unknown enum-shaped disk values fail initialization or the active operation
  without rewriting the original file.
- Same-origin authorization, correlation validation, cancellation, backend
  mutation ownership, daemon/CLI/API wire shapes, and the existing GET/SSE
  cadence are unchanged.
- The existing `audit-lock-state` event is reused; no second receipt event,
  second EventSource, or second polling loop is introduced.

## Migration and compatibility

There is no pre-version-1 data migration. An absent file expands to a
version-1 empty store through the existing startup claim. A valid version-1
file is validated, prior-generation `in_flight` records become durable
`uncertain`, the generation advances, and the active server instance rotates.
An unknown version, unknown field, invalid enum, or invalid cross-field
combination fails closed and leaves the inode and bytes untouched.

There is no mixed-version write window: version 1 remains the only readable
and writable format in this change. Any future version requires a superseding
decision that defines an expand-read/expand-write/contract sequence and the
old-reader window before a new writer emits the new version.

Rollback to a binary that does not enforce this file is safe only after the
current binary proves there are no unresolved records and the GUI process is
closed. The rollback does not delete or rewrite the version-1 file. If a
rollback has already occurred while a record is unresolved, destructive GUI
recovery is prohibited: restore the current binary, reconcile and acknowledge
the record, then repeat rollback. Automatic deletion, downgrade, or
best-effort parsing is forbidden because it erases the at-most-once fence.

## Failure discrimination

| Failure class | Observable discriminator | Required behavior |
| --- | --- | --- |
| Terminal store transaction not proven | HTTP `409 RECOVER_OUTCOME_UNCERTAIN`; effective receipt status `uncertain` | Install the current-generation overlay atomically; never advise or perform a retry. |
| Exact replay of the same uncertain tuple | HTTP `409 RECOVER_OUTCOME_UNCERTAIN` with the same correlation | Backend call count remains unchanged. |
| Strict load or schema validation failure | HTTP `500 AUDIT_LOCK_ADAPTER_INIT_FAILED` or adapter initialization failure | Preserve original bytes; recovery remains unavailable. |
| Same-task unresolved receipt | HTTP `409 RECOVER_ATTEMPT_CONFLICT` | Fence that task before backend mutation. |
| Different-task unresolved receipt | No receipt-policy error for the different task | Admit through the normal adapter transaction, subject to capacity and baseline checks. |
| Stale store generation | HTTP `409 RECOVER_BASELINE_STALE` | No relabel, no mutation, no fallback interpretation. |
| Acknowledgement before effective terminal state | HTTP `409 RECOVER_RECEIPT_IN_FLIGHT` | Do not consume the record. |

## Promotion gate

`status: proposed` may become `accepted` only after the
`architecture-reviewer` verifies:

1. the deterministic terminal-write-failure guard across immediate response,
   repeated same-process GET and replay, acknowledgement, and restarted GET;
2. the two-task Dashboard guard showing A fenced and B admitted;
3. exact owner-derived validation for every persisted enum field on decode and
   write paths;
4. one adapter writer-owner, one `audit-lock-state` event family, no second
   policy or state store, and no external wire change;
5. migration and rollback documentation references this decision id.

## Alternatives rejected

### Keep the current immediate-response-only uncertainty

Rejected because later GET and replay reconstruct `in_flight`, contradicting
the response and temporarily hiding explicit acknowledgement.

### Persist a second sidecar or outbox

Rejected because it creates another durable schema, another recovery and
rollback path, and an ordering problem between two files. The existing
generation claim already supplies restart conversion.

### Retry terminal persistence until it succeeds

Rejected because a persistent state-store or lock failure would hang the
request or hide failure behind an unbounded retry. A bounded retry cannot
prove the final write and still needs the same uncertainty state.

### Fence all Dashboard recovery while any receipt exists

Rejected because the accepted durable domain is the exact canonical task, not
the process. A global fence narrows unrelated task behavior and duplicates
policy in the browser.

### Keep shape-only enum validation for forward compatibility

Rejected because version 1 has no pass-through contract for unknown values.
Forward compatibility requires a new schema version and a defined reader
window, not acceptance of corrupt producer values.

### Delete the store during rollback

Rejected because deletion erases unresolved at-most-once evidence. Rollback is
gated on a proven empty unresolved set and preserves the file for forward
restoration.

## Terms and Abbreviations

- API: Application Programming Interface.
- GET: read-only Hypertext Transfer Protocol request.
- POST: mutating Hypertext Transfer Protocol request.
- SSE: Server-Sent Events.
- UUID: Universally Unique Identifier.

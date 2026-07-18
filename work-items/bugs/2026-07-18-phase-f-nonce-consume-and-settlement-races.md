# Bug: Phase-F nonce consumption and Commit settlement violate adversarial contracts

- id: 2026-07-18-phase-f-nonce-consume-and-settlement-races
- context: 2026-07-16-productization-gui-solidify
- status: open
- severity: medium
- area: internal/api, internal/gui, internal/cli
- found-by: security-reviewer

## Reproduction and findings

### 1. POSIX consume has a directory-entry swap window

Expected: strict consume either removes the exact opened nonce inode on every
post-open outcome or fails without leaving that authorization credential linked.

Actual: `internal/api/hub_mcp_state_read_inode_posix.go:161-170` performs
`Fstatat`, compares device/inode, and then calls `Unlinkat` in a separate syscall.
Two falsifying interleavings remain:

1. Rename/exchange before `Fstatat`: revalidation fails at lines 165-167, but the
   already-open credential can remain linked under the attacker's new name.
2. Rename/exchange after the equality check and before `Unlinkat`: `Unlinkat`
   can remove the replacement entry while the verified/opened credential remains
   linked elsewhere.

The strict directory and file mode gates exclude a different UID, but they do
not prevent malicious rename operations by another process running under the
same UID. The direct-human contract explicitly requires exact opened-entry
cleanup even for entry swaps. This finding does not claim that POSIX exposes an
unlink-by-handle primitive. The smallest required authorization invariant is:
nonce bytes are never accepted unless the opened inode is proved unlinked after
the consume operation. The stronger requested lifetime invariant additionally
requires an owned storage/transfer design that can make the opened credential
unlinked despite a same-UID rename, or an explicit direct-human threat-contract
change. An `nlink == 0` check can falsify unsafe acceptance but is not, by
itself, a cleanup mechanism.

### 2. Commit liveness check and durable transition are not serialized

Expected: `committed` has one linearization point at which the activated runtime
and context are still live. If runtime/context death wins, no committed marker
is persisted.

Actual: `internal/gui/gui_restart_protocol.go:313-320` checks `ctx.Err()` and
`RuntimeDone`, then calls a separate uncancellable `MarkerStore.Commit`. A
falsifying interleaving is:

1. The two liveness checks observe live state.
2. The runtime exits (or the context is canceled) and releases its lease.
3. `Commit` persists `committed` and the protocol publishes success.

The same interleaving can occur while `Commit` is blocked in durable I/O.
Post-write liveness recheck plus compensating terminalization does not satisfy
the current durable-state contract: `committed` would already have been
observable, and `HandoffMarkerStore.Interrupt` rejects committed as terminal.
The smallest required invariant is stronger serialization under one settlement
owner: runtime/context terminal signaling and the reserved-to-committed write
must be ordered so whichever wins prevents the other transition, with runtime
live at Commit's linearization point.

The separate exhaustion path is correct: lines 346-360 request durable
`interrupted` with reason `gui-restart-commit-write-failed`, and the Phase-I
classifier only admits `in-progress` and `reserved` at
`internal/cli/supervise_ensure_alive.go:537-545`.

### 3. Structured handoff authorizes an arbitrary absolute consume path

Expected: the restart child may consume only the exact `gui-restart-nonce` entry
owned by the canonical GUI state directory for the current startup.

Actual: `internal/gui/gui_restart_protocol.go:91-93` accepts any absolute
`NoncePath`; `internal/cli/gui.go:106-113` validates the target port but does not
bind the path to the trusted pidport/state directory. Both platform consumers
schedule deletion before the 32-byte nonce-length check at
`internal/gui/gui_restart_protocol.go:122-124`. A crafted gate-on structured
environment can therefore point at another strict owner-only regular file and
delete it before rejection.

Severity is medium: the gate defaults off and the launch environment is under
the same operating-system user, so this is not a cross-user authorization
bypass; it is nevertheless an irreversible arbitrary-file deletion capability
inside an authorization-credential parser. The smallest required owner-bound
validation is to derive the nonce path from the trusted canonical state
directory (preferred), or compare the supplied path with that derived exact
`gui-restart-nonce` path using operating-system-correct canonical path identity,
before calling the consuming API. An absolute-path check alone is insufficient.

## Verification evidence

The commissioned focused API, GUI, and CLI test groups passed with
`-tags=test_state_path_env`; focused GUI and CLI race-detector reruns also
passed. Those tests cover already-dead/ canceled states and ordinary deletion,
but they do not engineer either check-to-write race above or an arbitrary-path
substitution. Phase-I protected hashes remained exactly as commissioned.

## Terms and Abbreviations

- CLI: Command-Line Interface.
- POSIX: Portable Operating System Interface family used by Unix-like systems.
- UID: operating-system user identifier.


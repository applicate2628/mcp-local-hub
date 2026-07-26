// internal/api/serena_removal_fence.go
//
// The per-workspace serena-unregister LIVENESS FENCE.
//
// Why a fence at all. A serena unregister marks its registry row
// PendingSerenaRemoval so RepairSerenaIntentFromRegistry can tell "deliberately
// being torn down" apart from "orphaned by a crash" and skip it instead of
// re-appending the descriptor the teardown just removed. That mark alone is not
// enough: an unregister that is KILLED between the mark and its DeleteSerenaRow
// leaves the mark set with nobody left to clear it, so the repair also needs a
// way to reclaim the row. The first answer to that was a wall-clock lease
// (serenaPendingRemovalLeaseTTL) — but elapsed time is not liveness. A teardown
// that is merely SLOW (its RemoveSerenaIntent blocks on supervisor-intent.json's
// BLOCKING flock, or its DeleteSerenaRow blocks on the registry flock — neither
// acquire has a timeout) is indistinguishable from a dead one once the clock
// runs out, and the repair then reclaims a row whose owner is still alive:
//
//	unregister marks the row and removes the descriptor
//	  -> blocks past the lease on the intent lock
//	  -> an apply-reconcile's repair sees the lease expired, reclassifies the
//	     row as an ordinary crash orphan and CLEARS the mark
//	  -> the unregister finally proceeds; its own reconcile re-appends the
//	     now-unmarked "orphan", then it deletes only the registry row
//	net: a resurrected descriptor with NO registry row behind it.
//
// The fence answers the question the clock cannot: is the process that set this
// mark still alive? It is a gofrs/flock leaf held by the unregistering process
// across the whole marked window. The kernel releases a flock when its holder
// dies, so:
//
//   - HELD   => a live owner exists right now      => the repair must not reclaim
//   - FREE   => whoever took it is provably gone   => the mark is reclaimable debris
//
// This is the same "the flock is the authoritative liveness signal, a recorded
// PID is only a diagnostic hint" discipline SupervisorRunningUnderStateDir
// documents (supervisor_lock.go): a PID probe is racy against PID reuse and can
// pin a row forever behind a recycled PID, which is exactly the permanent
// stranding the lease exists to end.
//
// WHY THIS IS NOT THE REGISTRY FLOCK. The defect this branch exists to fix was
// `mcphub workspace register` holding the REGISTRY flock across its whole
// command, which starved RepairSerenaIntentFromRegistry's ~250ms
// tryLockRegistryBrief budget into a silent no-op. The fence must therefore be a
// SEPARATE per-workspace primitive: the repair still takes the registry lock
// normally, and only then TryLocks this fence to decide about ONE workspace. A
// fence holder blocks nothing the repair needs — during its own blocking waits
// the unregister holds neither the registry nor the intent lock, which is
// precisely the window the repair runs in.
//
// LOCK ORDER + DEADLOCK-FREEDOM. The only BLOCKING acquirer is the unregister,
// and it takes the fence FIRST, while holding no other lock: fence -> registry
// -> intent. The repair takes registry -> fence, i.e. the inverse order, but its
// fence acquisition is a non-blocking TryLock that skips on contention, so it
// can never participate in a wait cycle. This mirrors the reasoning already
// recorded on RepairSerenaIntentFromRegistry: deadlock-freedom comes from
// TryLock-and-skip, not from a globally consistent acquire order. Do NOT
// "optimize" the probe below into a blocking Lock.
//
// FILE RETENTION. The lock leaf is never unlinked, matching supervisor.lock and
// the registry lock (both keep their .lock leaf; only supervisor.lock's separate
// .owner.json sidecar is removed on release). Unlinking a flock leaf while
// another process is blocked opening/locking it is the classic
// unlink-under-holder race: the waiter would end up holding a lock on an
// unlinked inode while the next acquirer creates a fresh one at the same path,
// and both would believe they own the fence. The residual is one empty 0-byte
// file per workspace ever unregistered under <registryDir>/serena-removal-fences/.

package api

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

// serenaRemovalFenceDirLeaf is the subdirectory (alongside workspaces.yaml)
// holding one fence leaf per workspace. A dedicated directory keeps these
// short-lived-semantics files out of the state-dir root, where every leaf is a
// durable, individually-meaningful state file.
const serenaRemovalFenceDirLeaf = "serena-removal-fences"

// serenaRemovalFencePath is the SINGLE owner of the fence leaf's location. Both
// sides (the unregister's acquire, the repair's probe) must derive the path from
// the SAME registryDir — the directory of the registry path each side already
// resolved — so a state-path override seam can never point them at different
// roots. This is the same co-location rule the default-workspace marker follows
// (default_workspace_marker.go): the caller passes filepath.Dir(regPath); the
// path is never re-resolved internally.
func serenaRemovalFencePath(registryDir, workspaceKey string) string {
	return filepath.Join(registryDir, serenaRemovalFenceDirLeaf, workspaceKey+".lock")
}

// validSerenaRemovalFenceKey reports whether workspaceKey is the exact shape
// WorkspaceKey produces (workspace_path.go: the first 8 lowercase hex characters
// of a sha256), and is therefore safe to use verbatim as a path leaf.
//
// This primitive validates its own input rather than trusting its callers: the
// repair probes with the key it read out of workspaces.yaml, and that file is
// operator-editable, so an arbitrary string (`..\..\something`) must never reach
// filepath.Join. The check is the producer's shape, not a blocklist — a key that
// does not match is refused outright, which the callers treat as
// "liveness undeterminable" and handle fail-closed. A hand-edited divergent key
// is separately caught by the repair's divergence guard.
func validSerenaRemovalFenceKey(workspaceKey string) bool {
	if workspaceKey == "" || len(workspaceKey) > 64 {
		return false
	}
	for i := 0; i < len(workspaceKey); i++ {
		c := workspaceKey[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// AcquireSerenaRemovalFence takes the per-workspace unregister fence and returns
// its release closure. The caller MUST hold it across the ENTIRE marked window
// — from before SetSerenaPendingRemoval(true) until after the registry-row
// delete (or after the failure path has cleared the mark again) — so a repair
// running at any instant inside that window observes a live owner. Acquiring it
// after the mark write would leave a gap in which the mark is set and the fence
// is free, i.e. exactly the state the repair reads as reclaimable debris.
//
// The acquire is BLOCKING, which makes the fence a per-workspace mutex between
// concurrent teardowns as well (a second `mcphub workspace unregister` / GUI
// auto-prune sweep for the same workspace waits rather than interleaving its
// mark/clear with the first one's). That preserves the pre-fence observable
// behavior — both teardowns still run to completion, the loser simply finding
// nothing left to remove — and introduces no new hang class: every acquire on
// this path already blocks on the registry and supervisor-intent flocks, which
// have no timeout either. Because the fence is taken while holding no other
// lock, a blocking wait here can only ever wait on another teardown of the SAME
// workspace.
//
// registryDir is filepath.Dir of the caller's already-resolved registry path.
func AcquireSerenaRemovalFence(registryDir, workspaceKey string) (release func(), err error) {
	if registryDir == "" {
		return nil, fmt.Errorf("serena removal fence: empty registry dir (caller must thread the resolved registry directory)")
	}
	if !validSerenaRemovalFenceKey(workspaceKey) {
		return nil, fmt.Errorf("serena removal fence: refusing workspace key %q (not the canonical WorkspaceKey hex shape)", workspaceKey)
	}
	path := serenaRemovalFencePath(registryDir, workspaceKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("serena removal fence: mkdir %s: %w", filepath.Dir(path), err)
	}
	fl := flock.New(path)
	if err := fl.Lock(); err != nil {
		return nil, fmt.Errorf("serena removal fence: lock %s: %w", path, err)
	}
	return func() { _ = fl.Unlock() }, nil
}

// serenaRemovalFenceHeld reports whether a LIVE unregister currently owns the
// fence for workspaceKey. It is TRI-STATE, deliberately mirroring
// SupervisorRunningUnderStateDir's contract:
//
//   - (false, nil) — provably FREE: no live holder (or none ever existed).
//   - (true,  nil) — provably HELD: a live teardown owns this workspace.
//   - (false, err) — UNDETERMINABLE: the probe itself failed. Callers gating a
//     destructive decision MUST treat this as "assume held" and fail closed;
//     answering "free" on a probe error would silently disable the fence on
//     exactly the locked-down / AV-instrumented hosts where a flock syscall can
//     error instead of cleanly reporting contention.
//
// A MISSING leaf is FREE, not an error, and is answered by a stat WITHOUT
// touching the lock: flock.New opens with O_CREATE, so probing a never-used
// workspace would otherwise create the file (and fail outright when the fence
// directory does not exist yet — the state of every host where no unregister has
// ever run). The stat cannot report a false "missing" for a marked row: the
// acquire happens BEFORE the mark write, and the repair reads the mark under the
// registry lock, so a mark visible to the repair implies the leaf already exists.
//
// Non-blocking by contract — see the deadlock-freedom note in the file header.
func serenaRemovalFenceHeld(registryDir, workspaceKey string) (held bool, err error) {
	if registryDir == "" {
		return false, fmt.Errorf("serena removal fence probe: empty registry dir")
	}
	if !validSerenaRemovalFenceKey(workspaceKey) {
		return false, fmt.Errorf("serena removal fence probe: refusing workspace key %q (not the canonical WorkspaceKey hex shape)", workspaceKey)
	}
	path := serenaRemovalFencePath(registryDir, workspaceKey)
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			return false, nil // never acquired → definitively no live holder
		}
		return false, fmt.Errorf("serena removal fence probe: stat %s: %w", path, statErr)
	}
	fl := flock.New(path)
	locked, lerr := fl.TryLock()
	if lerr != nil {
		return false, fmt.Errorf("serena removal fence probe: try-lock %s: %w", path, lerr)
	}
	if locked {
		_ = fl.Unlock() // we only asked a question; never keep the fence
		return false, nil
	}
	return true, nil
}

// serenaRemovalFenceHeldFn is the test seam over the probe, matching the
// package's existing seam idiom. Tests override it to drive the UNDETERMINABLE
// branch, which cannot be produced by a real flock on a healthy filesystem. The
// HELD and FREE branches are exercised against the real primitive.
var serenaRemovalFenceHeldFn = serenaRemovalFenceHeld

// internal/api/serena_removal_fence.go
//
// The per-workspace serena-unregister LIVENESS FENCE.
//
// Why a fence at all. A serena unregister marks its registry row
// PendingSerenaRemoval so RepairSerenaIntentFromRegistry can tell "deliberately
// being torn down" apart from "orphaned by a crash" and skip it instead of
// re-appending the descriptor the teardown just removed. That mark alone is not
// enough: an unregister interrupted after BeginSerenaPendingRemoval but before
// row deletion or rollback completes leaves the mark set with nobody left to
// clear it, so the repair also needs a
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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/gofrs/flock"
)

// ErrSerenaRemovalFenceReleaseFailed classifies a fence release that could not
// be confirmed. The caller may still hold the fence, even if its teardown
// mutations committed.
var ErrSerenaRemovalFenceReleaseFailed = errors.New("serena removal fence: release could not be confirmed; this process may still hold the fence")

// serenaRemovalFenceUnlockFn is the injectable release seam. Production uses
// (*flock.Flock).Unlock; tests use it to exercise an unlock failure that a
// healthy filesystem cannot deterministically produce.
var serenaRemovalFenceUnlockFn = func(fl *flock.Flock) error { return fl.Unlock() }

// serenaRemovalFenceDirLeaf is the subdirectory (alongside workspaces.yaml)
// holding one fence leaf per workspace. A dedicated directory keeps these
// short-lived-semantics files out of the state-dir root, where every leaf is a
// durable, individually-meaningful state file.
const serenaRemovalFenceDirLeaf = "serena-removal-fences"

const serenaRemovalFenceGenerationSuffix = ".generation"

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

// serenaRemovalFenceGenerationPath is owned by this API so callers cannot
// accidentally couple generation metadata to a different state root or the
// flock inode itself.
func serenaRemovalFenceGenerationPath(registryDir, workspaceKey string) string {
	return filepath.Join(registryDir, serenaRemovalFenceDirLeaf, workspaceKey+serenaRemovalFenceGenerationSuffix)
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
// its release closure. The fence is acquired before generation publication and
// BeginSerenaPendingRemoval, remains held across intent teardown, and is
// released only after row deletion or the returned exact rollback. Acquiring it
// after Begin or releasing it before the delete/rollback verdict exposes a false
// reclaimable window.
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
// The returned release reports whether the lock was actually dropped. A failed
// release leaves this process appearing live to repair and blocks a later
// teardown for this workspace, so callers must not report a clean teardown.
//
// registryDir is filepath.Dir of the caller's already-resolved registry path.
func AcquireSerenaRemovalFence(registryDir, workspaceKey string) (release func() error, err error) {
	return AcquireSerenaRemovalFences(registryDir, workspaceKey)
}

// AcquireSerenaRemovalFences acquires one teardown transaction across every
// registry key that may name the workspace. Keys are validated, deduplicated,
// and sorted before blocking so canonical/legacy callers cannot deadlock by
// presenting the same pair in a different order. A partial acquire is rolled
// back before returning; the returned release is one-shot and releases every
// acquired leaf in reverse order while preserving all release failures.
func AcquireSerenaRemovalFences(registryDir string, workspaceKeys ...string) (release func() error, err error) {
	if registryDir == "" {
		return nil, fmt.Errorf("serena removal fence: empty registry dir (caller must thread the resolved registry directory)")
	}
	keys, err := normalizedSerenaRemovalFenceKeys(workspaceKeys)
	if err != nil {
		return nil, err
	}
	releases := make([]func() error, 0, len(keys))
	for _, key := range keys {
		keyRelease, acquireErr := acquireSerenaRemovalFence(registryDir, key)
		if acquireErr != nil {
			var releaseErrs []error
			for i := len(releases) - 1; i >= 0; i-- {
				if releaseErr := releases[i](); releaseErr != nil {
					releaseErrs = append(releaseErrs, releaseErr)
				}
			}
			return nil, errors.Join(acquireErr, errors.Join(releaseErrs...))
		}
		releases = append(releases, keyRelease)
	}
	var once sync.Once
	var releaseErr error
	return func() error {
		once.Do(func() {
			var errs []error
			for i := len(releases) - 1; i >= 0; i-- {
				if err := releases[i](); err != nil {
					errs = append(errs, err)
				}
			}
			releaseErr = errors.Join(errs...)
		})
		return releaseErr
	}, nil
}

func acquireSerenaRemovalFence(registryDir, workspaceKey string) (release func() error, err error) {
	path := serenaRemovalFencePath(registryDir, workspaceKey)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("serena removal fence: mkdir %s: %w", filepath.Dir(path), err)
	}
	// Capture the seam once so a held release closure cannot observe a test
	// cleanup restoring the package variable while it is in flight.
	unlockFn := serenaRemovalFenceUnlockFn
	ledgeredRelease, err := lockLeafLedgeredWithUnlock(path, unlockFn)
	if err != nil {
		if errors.Is(err, ErrLockReleaseUnconfirmed) {
			return nil, fmt.Errorf("%w: %s: %w", ErrSerenaRemovalFenceReleaseFailed, path, err)
		}
		return nil, fmt.Errorf("serena removal fence: lock %s: %w", path, err)
	}
	return func() error {
		if unlockErr := ledgeredRelease(); unlockErr != nil {
			return fmt.Errorf("%w: %s: %w", ErrSerenaRemovalFenceReleaseFailed, path, unlockErr)
		}
		return nil
	}, nil
}

// PublishSerenaRemovalFenceGeneration atomically replaces the sidecar metadata
// for a fence the caller already holds. It never writes, replaces, or unlinks
// the flock leaf: its returned opaque value is at least 128 random bits and is
// safe to persist in the registry as an identity, not as a credential.
func PublishSerenaRemovalFenceGeneration(registryDir, workspaceKey string) (string, error) {
	return PublishSerenaRemovalFenceGenerationForKeys(registryDir, workspaceKey)
}

// PublishSerenaRemovalFenceGenerationForKeys publishes one shared generation
// under every fence held for a canonical/legacy workspace-key transaction. The
// pending-removal tuple therefore matches the sidecar repair will probe on each
// actual marked row, including a legacy-only row.
func PublishSerenaRemovalFenceGenerationForKeys(registryDir string, workspaceKeys ...string) (string, error) {
	if registryDir == "" {
		return "", fmt.Errorf("serena removal fence generation: empty registry dir")
	}
	keys, err := normalizedSerenaRemovalFenceKeys(workspaceKeys)
	if err != nil {
		return "", fmt.Errorf("serena removal fence generation: %w", err)
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("serena removal fence generation: random token: %w", err)
	}
	generation := hex.EncodeToString(raw[:])
	type priorSidecar struct {
		path   string
		raw    []byte
		exists bool
	}
	prior := make([]priorSidecar, len(keys))
	for i, key := range keys {
		prior[i].path = serenaRemovalFenceGenerationPath(registryDir, key)
		raw, readErr := ReadStateFileInodeAnchored(prior[i].path)
		if readErr == nil {
			prior[i].raw = raw
			prior[i].exists = true
			continue
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return "", fmt.Errorf("serena removal fence generation: snapshot %s: %w", prior[i].path, readErr)
		}
	}
	for i := range prior {
		if writeErr := writeSerenaRemovalFenceGenerationFn(prior[i].path, []byte(generation+"\n")); writeErr != nil {
			primary := fmt.Errorf("serena removal fence generation: publish %s: %w", prior[i].path, writeErr)
			var rollbackErrs []error
			for j := i; j >= 0; j-- {
				var rollbackErr error
				if prior[j].exists {
					rollbackErr = WriteStateFileBytesAtomic(prior[j].path, prior[j].raw)
				} else {
					rollbackErr = os.Remove(prior[j].path)
					if errors.Is(rollbackErr, os.ErrNotExist) {
						rollbackErr = nil
					}
				}
				if rollbackErr != nil {
					rollbackErrs = append(rollbackErrs, fmt.Errorf("restore %s: %w", prior[j].path, rollbackErr))
				}
			}
			return "", errors.Join(primary, errors.Join(rollbackErrs...))
		}
	}
	return generation, nil
}

func normalizedSerenaRemovalFenceKeys(workspaceKeys []string) ([]string, error) {
	seen := make(map[string]struct{}, len(workspaceKeys))
	keys := make([]string, 0, len(workspaceKeys))
	for _, key := range workspaceKeys {
		if key == "" {
			continue
		}
		if !validSerenaRemovalFenceKey(key) {
			return nil, fmt.Errorf("serena removal fence: refusing workspace key %q (not the canonical WorkspaceKey hex shape)", key)
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("serena removal fence: no workspace keys")
	}
	sort.Strings(keys)
	return keys, nil
}

// writeSerenaRemovalFenceGenerationFn is the narrow test seam over the
// repository's canonical cross-platform state writer. Production always uses
// its handle-relative Windows/POSIX publication and cleanup contract.
var writeSerenaRemovalFenceGenerationFn = WriteStateFileBytesAtomic

// serenaRemovalFenceObservation distinguishes an existing current-generation
// fence leaf from a missing leaf. Missing is not an error, but its provenance is
// ambiguous for a fresh pending-removal mark: an older writer may be live while
// knowing nothing about fences.
type serenaRemovalFenceObservation struct {
	exists     bool
	held       bool
	generation string
}

func validSerenaRemovalFenceGeneration(generation string) bool {
	if len(generation) != 32 {
		return false
	}
	_, err := hex.DecodeString(generation)
	return err == nil
}

func readSerenaRemovalFenceGeneration(registryDir, workspaceKey string) (string, error) {
	path := serenaRemovalFenceGenerationPath(registryDir, workspaceKey)
	bytes, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("serena removal fence generation: read %s: %w", path, err)
	}
	generation := string(bytes)
	if len(generation) == 33 && generation[32] == '\n' {
		generation = generation[:32]
	}
	if !validSerenaRemovalFenceGeneration(generation) {
		return "", fmt.Errorf("serena removal fence generation: malformed metadata %s", path)
	}
	return generation, nil
}

// readSerenaRemovalFenceGenerationFn is the observation seam for the rare case
// where generation read and self-release both fail. Production always uses the
// canonical sidecar reader.
var readSerenaRemovalFenceGenerationFn = readSerenaRemovalFenceGeneration

// observeSerenaRemovalFence reports the current per-workspace fence state. It
// deliberately preserves both existence and lock state:
//
//   - ({exists:false}, nil) — no current-generation fence leaf exists.
//   - ({exists:true, held:false}, nil) — an existing fence is provably FREE.
//   - ({exists:true, held:true}, nil) — a live teardown owns this workspace.
//   - ({}, err) — UNDETERMINABLE: the probe itself failed. Callers gating a
//     destructive decision MUST treat this as "assume held" and fail closed;
//     answering "free" on a probe error would silently disable the fence on
//     exactly the locked-down / AV-instrumented hosts where a flock syscall can
//     error instead of cleanly reporting contention. A failed self-release is
//     also undeterminable: the probe now holds the fence it was measuring.
//
// A MISSING leaf is observed without error and answered by a stat WITHOUT
// touching the lock: flock.New opens with O_CREATE, so probing a never-used
// workspace would otherwise create the file (and fail outright when the fence
// directory does not exist yet — the state of every host where no unregister has
// ever run). A current writer acquires before marking, so its visible mark
// implies exists=true. A mixed-version writer may mark without creating a leaf;
// the repair combines exists=false with its lease instead of treating absence as
// proof that the writer died.
//
// Non-blocking by contract — see the deadlock-freedom note in the file header.
func observeSerenaRemovalFence(registryDir, workspaceKey string) (serenaRemovalFenceObservation, error) {
	if registryDir == "" {
		return serenaRemovalFenceObservation{}, fmt.Errorf("serena removal fence probe: empty registry dir")
	}
	if !validSerenaRemovalFenceKey(workspaceKey) {
		return serenaRemovalFenceObservation{}, fmt.Errorf("serena removal fence probe: refusing workspace key %q (not the canonical WorkspaceKey hex shape)", workspaceKey)
	}
	path := serenaRemovalFencePath(registryDir, workspaceKey)
	if _, statErr := os.Stat(path); statErr != nil {
		if os.IsNotExist(statErr) {
			return serenaRemovalFenceObservation{}, nil
		}
		return serenaRemovalFenceObservation{}, fmt.Errorf("serena removal fence probe: stat %s: %w", path, statErr)
	}
	// Bind the probe's release behavior to this observation. If it fails, the
	// acquired handle remains held and the result must be undeterminable.
	unlockFn := serenaRemovalFenceUnlockFn
	release, locked, lerr := tryLockLeafLedgeredWithUnlock(path, unlockFn)
	if lerr != nil {
		return serenaRemovalFenceObservation{}, fmt.Errorf("serena removal fence probe: try-lock %s: %w", path, lerr)
	}
	if locked {
		generation, generationErr := readSerenaRemovalFenceGenerationFn(registryDir, workspaceKey)
		unlockErr := release()
		if generationErr != nil || unlockErr != nil {
			var causes []error
			if generationErr != nil {
				causes = append(causes, generationErr)
			}
			if unlockErr != nil {
				causes = append(causes, fmt.Errorf("%w: %s: %w", ErrSerenaRemovalFenceReleaseFailed, path, unlockErr))
			}
			return serenaRemovalFenceObservation{}, fmt.Errorf("serena removal fence probe: %w", errors.Join(causes...))
		}
		return serenaRemovalFenceObservation{exists: true, generation: generation}, nil
	}
	return serenaRemovalFenceObservation{exists: true, held: true}, nil
}

// serenaRemovalFenceHeld retains the existing package-local boolean query for
// callers and primitive tests that only need lock state. Repair uses the richer
// observation owner below because missing provenance changes fresh-lease policy.
func serenaRemovalFenceHeld(registryDir, workspaceKey string) (bool, error) {
	observation, err := observeSerenaRemovalFence(registryDir, workspaceKey)
	return observation.held, err
}

// observeSerenaRemovalFenceFn is the test seam over the observation, matching the
// package's existing seam idiom. Tests override it to drive the UNDETERMINABLE
// branch, which cannot be produced by a real flock on a healthy filesystem. The
// MISSING, HELD, and existing-FREE branches are exercised against the real
// primitive.
var observeSerenaRemovalFenceFn = observeSerenaRemovalFence

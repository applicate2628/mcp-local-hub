// Package api — tests for the per-workspace serena-unregister LIVENESS FENCE
// and for the repair behavior it gates.
//
// The load-bearing case is the SLOW (not crashed) unregister: a teardown that
// blocks past the pending-removal lease while remaining perfectly alive. Before
// the fence, the repair reclassified that row as a crash orphan and cleared its
// mark, after which the teardown's own reconcile re-appended the now-unmarked
// descriptor and its DeleteSerenaRow removed only the registry row — a
// resurrected descriptor with nothing behind it.
//
// Every case here is constructed DIRECTLY (the mark stamped at a chosen age, the
// fence taken by the test itself) rather than by racing a real teardown, so the
// discriminator under test is the fence state and nothing else. In particular
// the two arms of TestRepairSerenaIntentFromRegistry_ExpiredLease_FenceDecides
// hold the mark's age FIXED and vary ONLY the fence, which is exactly the claim:
// liveness, not elapsed time, decides the reclaim.
package api

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// ---------------------------------------------------------------------------
// The fence primitive itself.
// ---------------------------------------------------------------------------

func TestSerenaRemovalFence_AcquireMakesHolderObservable(t *testing.T) {
	dir := t.TempDir()
	const key = "abcd1234"

	// Never acquired → definitively FREE, and answered without creating
	// anything (the probe must work on a host where no unregister ever ran).
	held, err := serenaRemovalFenceHeld(dir, key)
	if err != nil {
		t.Fatalf("probe on a never-acquired fence: unexpected error: %v", err)
	}
	if held {
		t.Fatal("probe reported HELD for a fence nobody ever acquired")
	}
	if _, statErr := os.Stat(filepath.Join(dir, serenaRemovalFenceDirLeaf)); !os.IsNotExist(statErr) {
		t.Errorf("probe created the fence directory; a read-only liveness question must not mutate the state dir (stat err = %v)", statErr)
	}

	release, err := AcquireSerenaRemovalFence(dir, key)
	if err != nil {
		t.Fatalf("AcquireSerenaRemovalFence: %v", err)
	}

	// Held by a live owner — this is the signal the repair skips on.
	held, err = serenaRemovalFenceHeld(dir, key)
	if err != nil {
		t.Fatalf("probe while held: unexpected error: %v", err)
	}
	if !held {
		t.Fatal("probe reported FREE while the fence was held; the repair would reclaim a live teardown's row")
	}

	// A DIFFERENT workspace is unaffected — the fence is per-workspace, so one
	// slow unregister must not freeze recovery for every other workspace.
	otherHeld, err := serenaRemovalFenceHeld(dir, "00ff9911")
	if err != nil {
		t.Fatalf("probe on a different key: unexpected error: %v", err)
	}
	if otherHeld {
		t.Error("probe reported HELD for an unrelated workspace key; the fence must be per-workspace")
	}

	release()

	// Released → free again. (A killed holder reaches this same state via the
	// kernel dropping its flock, which is why the fence answers liveness.)
	held, err = serenaRemovalFenceHeld(dir, key)
	if err != nil {
		t.Fatalf("probe after release: unexpected error: %v", err)
	}
	if held {
		t.Fatal("probe still reported HELD after release; a finished teardown must free the row for recovery")
	}
}

// A stale leaf left behind by a KILLED holder must read FREE. The file survives
// (the fence leaf is deliberately never unlinked), so "does a marker exist" and
// "is the holder alive" are genuinely different questions — this pins that the
// probe answers the second one.
func TestSerenaRemovalFence_StaleLeafWithoutHolderIsFree(t *testing.T) {
	dir := t.TempDir()
	const key = "beef0001"

	path := serenaRemovalFencePath(dir, key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir fence dir: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write stale fence leaf: %v", err)
	}

	held, err := serenaRemovalFenceHeld(dir, key)
	if err != nil {
		t.Fatalf("probe on a stale leaf: unexpected error: %v", err)
	}
	if held {
		t.Fatal("a fence leaf with no live holder reported HELD; a killed unregister would strand its row forever")
	}
}

// The primitive validates its own path input: workspaces.yaml is
// operator-editable, so a hand-written WorkspaceKey must never reach
// filepath.Join. Both entry points refuse, and the probe's refusal is an ERROR
// (undeterminable), which callers handle fail-closed.
func TestSerenaRemovalFence_RefusesNonCanonicalKey(t *testing.T) {
	dir := t.TempDir()
	bad := []string{"", "..", filepath.Join("..", "escape"), "ABCD1234", "abcd-1234", "abcd123z"}
	for _, key := range bad {
		if _, err := AcquireSerenaRemovalFence(dir, key); err == nil {
			t.Errorf("AcquireSerenaRemovalFence(%q) succeeded; a non-canonical key must be refused before it reaches a path join", key)
		}
		if _, err := serenaRemovalFenceHeld(dir, key); err == nil {
			t.Errorf("serenaRemovalFenceHeld(%q) returned a clean verdict; a non-canonical key must be UNDETERMINABLE so callers fail closed", key)
		}
	}
	if _, err := AcquireSerenaRemovalFence("", "abcd1234"); err == nil {
		t.Error("AcquireSerenaRemovalFence with an empty registry dir succeeded; the caller must thread a resolved directory")
	}
}

// The fence is a real cross-holder mutex: a second acquirer cannot take it while
// the first holds it. (TryLock stands in for the production blocking Lock so the
// test cannot hang if the invariant regresses.)
func TestSerenaRemovalFence_ExcludesASecondHolder(t *testing.T) {
	dir := t.TempDir()
	const key = "0123abcd"

	release, err := AcquireSerenaRemovalFence(dir, key)
	if err != nil {
		t.Fatalf("AcquireSerenaRemovalFence: %v", err)
	}
	defer release()

	second := flock.New(serenaRemovalFencePath(dir, key))
	locked, err := second.TryLock()
	if err != nil {
		t.Fatalf("second acquirer TryLock: %v", err)
	}
	if locked {
		_ = second.Unlock()
		t.Fatal("a second holder acquired the fence while the first held it; concurrent teardowns could interleave their mark/clear")
	}
}

// ---------------------------------------------------------------------------
// The repair decision the fence gates.
// ---------------------------------------------------------------------------

// seedSlowUnregisterState builds the exact on-disk shape a teardown leaves once
// it has marked its row and removed its descriptor: a healthy row with its
// daemon, plus a marked row whose descriptor is already gone. The mark is
// stamped past the lease, i.e. the teardown has been blocked for longer than any
// wall clock would tolerate. Returns (regPath, intentPath, slowKey).
func seedSlowUnregisterState(t *testing.T) (string, string, string) {
	t.Helper()
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	slowPath, slowPort := liveWorkspace(t), 9151
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	slowKey := seedSerenaRegistryRow(t, regPath, slowPath, slowPort)

	seedPendingRemovalMark(t, regPath, slowKey,
		time.Now().UTC().Add(-serenaPendingRemovalLeaseTTL-time.Minute))

	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})
	return regPath, intentPath, slowKey
}

// TestRepairSerenaIntentFromRegistry_ExpiredLease_FenceDecides is the Finding-1
// regression. Both arms present an IDENTICAL registry (same row, same
// past-the-lease mark age) and differ ONLY in whether the unregister that set the
// mark is still alive. A wall-clock lease cannot tell them apart and reclaims
// both; the fence must reclaim exactly one.
func TestRepairSerenaIntentFromRegistry_ExpiredLease_FenceDecides(t *testing.T) {
	t.Run("live but slow teardown: fence HELD → skip, mark preserved", func(t *testing.T) {
		regPath, intentPath, slowKey := seedSlowUnregisterState(t)

		// The teardown is alive — blocked on supervisor-intent.json.lock or
		// workspaces.yaml.lock, both of which it acquires WITHOUT holding, which
		// is precisely why this repair pass gets to run at all.
		release, err := AcquireSerenaRemovalFence(filepath.Dir(regPath), slowKey)
		if err != nil {
			t.Fatalf("acquire fence for the slow teardown: %v", err)
		}
		defer release()

		before, err := os.ReadFile(intentPath)
		if err != nil {
			t.Fatalf("read intent before: %v", err)
		}

		repaired, deferred, err := NewAPI().RepairSerenaIntentFromRegistry(mustStateDir(t))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if repaired != 0 {
			t.Errorf("repaired = %d, want 0 — the teardown holding this workspace's fence is ALIVE, so its descriptor must not be re-appended (it would survive with no registry row once that teardown's DeleteSerenaRow lands)", repaired)
		}
		if len(deferred) != 0 {
			t.Errorf("deferred = %v, want none (a live fence is a skip, not a deferral)", deferred)
		}
		after, err := os.ReadFile(intentPath)
		if err != nil {
			t.Fatalf("read intent after: %v", err)
		}
		if string(before) != string(after) {
			t.Errorf("supervisor-intent.json was written while a live teardown held the fence:\nbefore=%s\nafter=%s", before, after)
		}
		if got := readIntent(t, intentPath); got.HasSerenaDaemonForWorkspaceKey(slowKey) {
			t.Errorf("slow-teardown key %q was appended; the resurrected descriptor would outlive its registry row", slowKey)
		}

		// The mark itself must SURVIVE. Clearing it is what let the teardown's own
		// reconcile re-append the row as an unmarked orphan.
		row := readSerenaRowFresh(t, regPath, slowKey)
		if !row.PendingSerenaRemoval {
			t.Error("PendingSerenaRemoval was cleared under a live fence; the teardown's own reconcile would then see an unmarked orphan and re-append it")
		}
		if row.PendingSerenaRemovalAt.IsZero() {
			t.Error("PendingSerenaRemovalAt was cleared under a live fence; the mark must be left exactly as its owner wrote it")
		}
	})

	t.Run("crashed teardown: fence FREE → reclaimed", func(t *testing.T) {
		regPath, intentPath, slowKey := seedSlowUnregisterState(t)

		// Same registry, same mark age — but the teardown is gone, so the kernel
		// has released its fence. Take and immediately drop it to prove a leaf
		// EXISTS while no holder does (a killed process leaves exactly this).
		release, err := AcquireSerenaRemovalFence(filepath.Dir(regPath), slowKey)
		if err != nil {
			t.Fatalf("acquire fence: %v", err)
		}
		release()

		repaired, _, err := NewAPI().RepairSerenaIntentFromRegistry(mustStateDir(t))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if repaired != 1 {
			t.Fatalf("repaired = %d, want 1 — with no live holder the mark is crash debris and the row must be recovered, or it is stranded forever", repaired)
		}
		if got := readIntent(t, intentPath); !got.HasSpecBearingSerenaDaemonForWorkspaceKey(slowKey) {
			t.Errorf("key %q was not materialized after its owner died", slowKey)
		}
		if row := readSerenaRowFresh(t, regPath, slowKey); row.PendingSerenaRemoval {
			t.Error("the stale mark survived recovery; the registry keeps asserting a teardown that is not happening")
		}
	})
}

// An UNDETERMINABLE fence (the probe itself failed — a locked-down or
// AV-instrumented host where a flock syscall errors instead of cleanly reporting
// contention) must fail CLOSED. Answering "free" on a probe error would silently
// disable the fence on exactly the hosts where it cannot be observed.
func TestRepairSerenaIntentFromRegistry_ExpiredLease_FenceProbeError_FailsClosed(t *testing.T) {
	regPath, intentPath, slowKey := seedSlowUnregisterState(t)

	probeErr := errors.New("simulated LockFileEx failure")
	prev := serenaRemovalFenceHeldFn
	serenaRemovalFenceHeldFn = func(string, string) (bool, error) { return false, probeErr }
	t.Cleanup(func() { serenaRemovalFenceHeldFn = prev })

	repaired, _, err := NewAPI().RepairSerenaIntentFromRegistry(mustStateDir(t))
	if err != nil {
		t.Fatalf("a fence probe failure must not fail the whole repair: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 — an unobservable fence cannot PROVE the teardown is gone, so the reclaim must fail closed", repaired)
	}
	if got := readIntent(t, intentPath); got.HasSerenaDaemonForWorkspaceKey(slowKey) {
		t.Errorf("key %q was appended on an unobservable fence", slowKey)
	}
	if row := readSerenaRowFresh(t, regPath, slowKey); !row.PendingSerenaRemoval {
		t.Error("the mark was cleared on an unobservable fence; fail-closed means leaving it for a pass that can actually observe liveness")
	}
}

// A FRESH mark is honored without probing at all — the fence is an ADDITIONAL
// gate past the lease, never a replacement that could shorten the window a
// genuinely in-flight teardown depends on. This also pins the behavior for an
// older binary that sets the mark but takes no fence: it keeps the full lease.
func TestRepairSerenaIntentFromRegistry_FreshMark_SkipsWithoutProbingFence(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	pendingPath, pendingPort := liveWorkspace(t), 9151
	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	pendingKey := seedSerenaRegistryRow(t, regPath, pendingPath, pendingPort)
	seedPendingRemovalMark(t, regPath, pendingKey, time.Now().UTC())

	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})

	probed := 0
	prev := serenaRemovalFenceHeldFn
	serenaRemovalFenceHeldFn = func(dir, key string) (bool, error) {
		probed++
		return prev(dir, key)
	}
	t.Cleanup(func() { serenaRemovalFenceHeldFn = prev })

	repaired, _, err := NewAPI().RepairSerenaIntentFromRegistry(mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 (a fresh mark is still honored)", repaired)
	}
	if probed != 0 {
		t.Errorf("fence probed %d times for a FRESH mark, want 0 — inside the lease the mark is honored outright, with no per-row filesystem probe on the supervisor's startup path", probed)
	}
	if got := readIntent(t, intentPath); got.HasSerenaDaemonForWorkspaceKey(pendingKey) {
		t.Errorf("fresh pending-removal key %q was appended", pendingKey)
	}
}

// The preview must reach the SAME verdict as apply under a live fence — the two
// modes may never disagree about which rows are orphaned — while still writing
// nothing.
func TestPreviewSerenaIntentRepairFromRegistry_LiveFence_MatchesApplyAndWritesNothing(t *testing.T) {
	regPath, intentPath, slowKey := seedSlowUnregisterState(t)

	release, err := AcquireSerenaRemovalFence(filepath.Dir(regPath), slowKey)
	if err != nil {
		t.Fatalf("acquire fence: %v", err)
	}
	defer release()

	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent before: %v", err)
	}

	wouldRepair, deferred, err := NewAPI().PreviewSerenaIntentRepairFromRegistry(mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wouldRepair != 0 {
		t.Errorf("wouldRepair = %d, want 0 — preview must classify a live fence exactly as apply does", wouldRepair)
	}
	if len(deferred) != 0 {
		t.Errorf("deferred = %v, want none", deferred)
	}
	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent after: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("preview WROTE supervisor-intent.json:\nbefore=%s\nafter=%s", before, after)
	}
	if row := readSerenaRowFresh(t, regPath, slowKey); !row.PendingSerenaRemoval {
		t.Error("preview cleared the pending-removal mark; a dry run must never mutate the registry")
	}
}

// The repair must still be able to take the REGISTRY lock while a teardown holds
// a fence — the fence exists precisely because the branch's original P1 was a
// command starving the repair's ~250ms tryLockRegistryBrief budget by holding
// the registry lock across its whole run. A fence that blocked the registry lock
// would reintroduce that defect in a new place.
func TestRepairSerenaIntentFromRegistry_HeldFenceDoesNotStarveTheRegistryLock(t *testing.T) {
	regPath := autoRegisterTestEnv(t)

	healthyPath, healthyPort := liveWorkspace(t), 9150
	orphanPath, orphanPort := liveWorkspace(t), 9151
	slowPath, slowPort := liveWorkspace(t), 9152

	seedSerenaRegistryRow(t, regPath, healthyPath, healthyPort)
	orphanKey := seedSerenaRegistryRow(t, regPath, orphanPath, orphanPort)
	slowKey := seedSerenaRegistryRow(t, regPath, slowPath, slowPort)
	seedPendingRemovalMark(t, regPath, slowKey,
		time.Now().UTC().Add(-serenaPendingRemovalLeaseTTL-time.Minute))

	intentPath := seedIntent(t, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{healthySerenaDaemon(t, healthyPath, healthyPort)},
	})

	release, err := AcquireSerenaRemovalFence(filepath.Dir(regPath), slowKey)
	if err != nil {
		t.Fatalf("acquire fence: %v", err)
	}
	defer release()

	// The repair acquires the registry lock, reads BOTH rows, and acts on the
	// unfenced one — a fence that serialized against the registry lock would
	// make this a zero-repair no-op instead.
	repaired, _, err := NewAPI().RepairSerenaIntentFromRegistry(mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired = %d, want 1 — a fence held for ONE workspace must not stop the repair from acquiring the registry lock and healing every other row", repaired)
	}
	got := readIntent(t, intentPath)
	if !got.HasSpecBearingSerenaDaemonForWorkspaceKey(orphanKey) {
		t.Errorf("the unfenced orphan %q was not repaired", orphanKey)
	}
	if got.HasSerenaDaemonForWorkspaceKey(slowKey) {
		t.Errorf("the fenced key %q was repaired", slowKey)
	}
}

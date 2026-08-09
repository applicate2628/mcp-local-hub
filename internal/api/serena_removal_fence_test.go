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
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestAcquireSerenaRemovalFences_LegacyKeySharesGenerationAndLiveness(t *testing.T) {
	dir := t.TempDir()
	const (
		canonicalKey = "abcdef12"
		legacyKey    = "1234abcd"
	)

	release, err := AcquireSerenaRemovalFences(dir, canonicalKey, legacyKey, canonicalKey)
	if err != nil {
		t.Fatalf("AcquireSerenaRemovalFences: %v", err)
	}
	generation, err := PublishSerenaRemovalFenceGenerationForKeys(dir, canonicalKey, legacyKey)
	if err != nil {
		t.Fatalf("PublishSerenaRemovalFenceGenerationForKeys: %v", err)
	}
	for _, key := range []string{canonicalKey, legacyKey} {
		observation, observeErr := observeSerenaRemovalFence(dir, key)
		if observeErr != nil || !observation.exists || !observation.held {
			t.Fatalf("held observation for %s = %+v, err=%v", key, observation, observeErr)
		}
		gotGeneration, readErr := readSerenaRemovalFenceGeneration(dir, key)
		if readErr != nil || gotGeneration != generation {
			t.Fatalf("generation for %s = %q, err=%v; want shared %q", key, gotGeneration, readErr, generation)
		}
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("second one-shot release: %v", err)
	}
	for _, key := range []string{canonicalKey, legacyKey} {
		observation, observeErr := observeSerenaRemovalFence(dir, key)
		if observeErr != nil || !observation.exists || observation.held || observation.generation != generation {
			t.Fatalf("released observation for %s = %+v, err=%v; want free shared generation", key, observation, observeErr)
		}
	}
}

// failingFenceUnlock retains every handle whose release it rejects so cleanup
// can explicitly release it after restoring the production seam.
func failingFenceUnlock(t *testing.T, cause error) {
	t.Helper()
	previous := serenaRemovalFenceUnlockFn
	var mu sync.Mutex
	var retained []*flock.Flock
	serenaRemovalFenceUnlockFn = func(fl *flock.Flock) error {
		mu.Lock()
		retained = append(retained, fl)
		mu.Unlock()
		return cause
	}
	t.Cleanup(func() {
		serenaRemovalFenceUnlockFn = previous
		mu.Lock()
		handles := append([]*flock.Flock(nil), retained...)
		mu.Unlock()
		for _, fl := range handles {
			if err := fl.Unlock(); err != nil {
				t.Errorf("cleanup retained Serena removal fence: %v", err)
			}
		}
	})
}

func releaseFenceOrFail(t *testing.T, release func() error) {
	t.Helper()
	if err := release(); err != nil {
		t.Fatalf("release Serena removal fence: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The fence primitive itself.
// ---------------------------------------------------------------------------

func TestSerenaRemovalFence_AcquireMakesHolderObservable(t *testing.T) {
	dir := t.TempDir()
	const key = "abcd1234"

	// Never acquired → not held, and answered without creating anything. The
	// richer observation owner retains missing provenance for repair policy.
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

	releaseFenceOrFail(t, release)

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

func TestSerenaRemovalFence_GenerationDoesNotReplaceLeaf(t *testing.T) {
	dir := t.TempDir()
	const key = "beef0001"
	release, err := AcquireSerenaRemovalFence(dir, key)
	if err != nil {
		t.Fatalf("AcquireSerenaRemovalFence first generation: %v", err)
	}
	before, err := os.Stat(serenaRemovalFencePath(dir, key))
	if err != nil {
		releaseFenceOrFail(t, release)
		t.Fatalf("stat leaf before publish: %v", err)
	}
	first, err := PublishSerenaRemovalFenceGeneration(dir, key)
	if err != nil {
		releaseFenceOrFail(t, release)
		t.Fatalf("PublishSerenaRemovalFenceGeneration first: %v", err)
	}
	releaseFenceOrFail(t, release)

	release, err = AcquireSerenaRemovalFence(dir, key)
	if err != nil {
		t.Fatalf("AcquireSerenaRemovalFence second generation: %v", err)
	}
	second, err := PublishSerenaRemovalFenceGeneration(dir, key)
	if err != nil {
		releaseFenceOrFail(t, release)
		t.Fatalf("PublishSerenaRemovalFenceGeneration second: %v", err)
	}
	after, err := os.Stat(serenaRemovalFencePath(dir, key))
	releaseFenceOrFail(t, release)
	if err != nil {
		t.Fatalf("stat leaf after publish: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("generation publication replaced the stable flock leaf")
	}
	if first == second || !validSerenaRemovalFenceGeneration(first) || !validSerenaRemovalFenceGeneration(second) {
		t.Fatalf("generation tokens = %q, %q; want distinct 128-bit opaque values", first, second)
	}
}

func TestSerenaRemovalFence_GenerationPublishFailurePreservesCompleteOldValueAndCleansTemps(t *testing.T) {
	dir := t.TempDir()
	const key = "beef0001"
	release, err := AcquireSerenaRemovalFence(dir, key)
	if err != nil {
		t.Fatalf("AcquireSerenaRemovalFence: %v", err)
	}
	oldGeneration, err := PublishSerenaRemovalFenceGeneration(dir, key)
	if err != nil {
		releaseFenceOrFail(t, release)
		t.Fatalf("initial PublishSerenaRemovalFenceGeneration: %v", err)
	}
	publicationErr := errors.New("injected canonical state writer failure")
	previousWriter := writeSerenaRemovalFenceGenerationFn
	writeSerenaRemovalFenceGenerationFn = func(string, []byte) error { return publicationErr }
	t.Cleanup(func() { writeSerenaRemovalFenceGenerationFn = previousWriter })
	if _, err := PublishSerenaRemovalFenceGeneration(dir, key); !errors.Is(err, publicationErr) {
		releaseFenceOrFail(t, release)
		t.Fatalf("publish error = %v, want %v", err, publicationErr)
	}
	releaseFenceOrFail(t, release)
	got, err := os.ReadFile(serenaRemovalFenceGenerationPath(dir, key))
	if err != nil {
		t.Fatalf("read retained generation: %v", err)
	}
	if string(got) != oldGeneration+"\n" {
		t.Fatalf("canonical generation = %q, want complete old value %q", got, oldGeneration+"\n")
	}
	entries, err := os.ReadDir(filepath.Join(dir, serenaRemovalFenceDirLeaf))
	if err != nil {
		t.Fatalf("ReadDir fence dir: %v", err)
	}
	allowed := map[string]bool{
		key + ".lock":                                      true,
		key + serenaRemovalFenceGenerationSuffix:           true,
		key + serenaRemovalFenceGenerationSuffix + ".lock": true,
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] || strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("publication failure leaked unexpected/temp artifact %q", entry.Name())
		}
	}
}

func TestSerenaRemovalFence_MultiKeyPublishFailureRestoresEarlierSidecars(t *testing.T) {
	dir := t.TempDir()
	const firstKey = "aaaa0001"
	const secondKey = "bbbb0002"
	firstPath := serenaRemovalFenceGenerationPath(dir, firstKey)
	secondPath := serenaRemovalFenceGenerationPath(dir, secondKey)
	oldFirst := []byte("0123456789abcdef0123456789abcdef\n")
	oldSecond := []byte("fedcba9876543210fedcba9876543210\n")
	if err := WriteStateFileBytesAtomic(firstPath, oldFirst); err != nil {
		t.Fatal(err)
	}
	if err := WriteStateFileBytesAtomic(secondPath, oldSecond); err != nil {
		t.Fatal(err)
	}

	publicationErr := errors.New("injected second sidecar failure")
	previousWriter := writeSerenaRemovalFenceGenerationFn
	writeSerenaRemovalFenceGenerationFn = func(path string, raw []byte) error {
		if path == secondPath {
			return publicationErr
		}
		return WriteStateFileBytesAtomic(path, raw)
	}
	t.Cleanup(func() { writeSerenaRemovalFenceGenerationFn = previousWriter })

	if _, err := PublishSerenaRemovalFenceGenerationForKeys(dir, firstKey, secondKey); !errors.Is(err, publicationErr) {
		t.Fatalf("publish error = %v, want %v", err, publicationErr)
	}
	if got, err := os.ReadFile(firstPath); err != nil || !bytes.Equal(got, oldFirst) {
		t.Fatalf("first sidecar after rollback = %q, err=%v; want %q", got, err, oldFirst)
	}
	if got, err := os.ReadFile(secondPath); err != nil || !bytes.Equal(got, oldSecond) {
		t.Fatalf("second sidecar after rollback = %q, err=%v; want %q", got, err, oldSecond)
	}
}

func TestSerenaRemovalFence_MultiKeyPublishFailureRemovesNewEarlierSidecar(t *testing.T) {
	dir := t.TempDir()
	const firstKey = "aaaa0001"
	const secondKey = "bbbb0002"
	firstPath := serenaRemovalFenceGenerationPath(dir, firstKey)
	secondPath := serenaRemovalFenceGenerationPath(dir, secondKey)
	oldSecond := []byte("fedcba9876543210fedcba9876543210\n")
	if err := WriteStateFileBytesAtomic(secondPath, oldSecond); err != nil {
		t.Fatal(err)
	}

	publicationErr := errors.New("injected second sidecar failure")
	previousWriter := writeSerenaRemovalFenceGenerationFn
	writeSerenaRemovalFenceGenerationFn = func(path string, raw []byte) error {
		if path == secondPath {
			return publicationErr
		}
		return WriteStateFileBytesAtomic(path, raw)
	}
	t.Cleanup(func() { writeSerenaRemovalFenceGenerationFn = previousWriter })

	if _, err := PublishSerenaRemovalFenceGenerationForKeys(dir, firstKey, secondKey); !errors.Is(err, publicationErr) {
		t.Fatalf("publish error = %v, want %v", err, publicationErr)
	}
	if _, err := os.Stat(firstPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new first sidecar survived rollback: %v", err)
	}
	if got, err := os.ReadFile(secondPath); err != nil || !bytes.Equal(got, oldSecond) {
		t.Fatalf("second sidecar after rollback = %q, err=%v; want %q", got, err, oldSecond)
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
	defer func() { releaseFenceOrFail(t, release) }()

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

func TestSerenaRemovalFence_FailedReleaseMakesSameProcessReacquireFailLoud(t *testing.T) {
	dir := t.TempDir()
	const key = "0123abce"
	cause := errors.New("synthetic retained fence handle")
	previousUnlock := serenaRemovalFenceUnlockFn
	var retained *flock.Flock
	calls := 0
	serenaRemovalFenceUnlockFn = func(fl *flock.Flock) error {
		calls++
		retained = fl
		return cause
	}
	t.Cleanup(func() {
		serenaRemovalFenceUnlockFn = previousUnlock
		if retained != nil {
			if err := retained.Unlock(); err != nil {
				t.Errorf("cleanup retained Serena removal fence: %v", err)
			}
		}
	})

	release, err := AcquireSerenaRemovalFence(dir, key)
	if err != nil {
		t.Fatalf("AcquireSerenaRemovalFence first: %v", err)
	}
	if err := release(); !errors.Is(err, ErrSerenaRemovalFenceReleaseFailed) || !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, cause) {
		t.Fatalf("first fence release = %v, want fence and ledger sentinels with cause", err)
	}
	if err := release(); !errors.Is(err, ErrSerenaRemovalFenceReleaseFailed) || !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, cause) {
		t.Fatalf("second fence release = %v, want memoized fence and ledger sentinels with cause", err)
	}
	if calls != 1 {
		t.Fatalf("underlying fence unlock calls = %d, want 1", calls)
	}

	done := make(chan error, 1)
	go func() {
		_, err := AcquireSerenaRemovalFence(dir, key)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrSerenaRemovalFenceReleaseFailed) || !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, cause) {
			t.Fatalf("second fence acquire = %v, want fence and ledger sentinels with cause", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second fence acquire waited on this process's retained handle")
	}
}

func TestSerenaRemovalFenceHeld_UnconfirmedProbeReleaseIsUndeterminable(t *testing.T) {
	dir := t.TempDir()
	const key = "c0ffee00"

	// Create a stale leaf first, while normal unlock behavior is installed, so
	// the probe takes the free-lock branch rather than the missing-leaf branch.
	release, err := AcquireSerenaRemovalFence(dir, key)
	if err != nil {
		t.Fatalf("AcquireSerenaRemovalFence: %v", err)
	}
	releaseFenceOrFail(t, release)

	unlockErr := errors.New("simulated UnlockFileEx failure")
	failingFenceUnlock(t, unlockErr)
	held, err := serenaRemovalFenceHeld(dir, key)
	if err == nil {
		t.Fatal("probe reported a clean free verdict after it could not release the lock it acquired")
	}
	if held {
		t.Fatalf("held = true with an unconfirmed probe release; want undeterminable false/error")
	}
	if !errors.Is(err, ErrSerenaRemovalFenceReleaseFailed) || !errors.Is(err, unlockErr) {
		t.Fatalf("err = %v, want release sentinel and unlock cause", err)
	}
}

func TestObserveSerenaRemovalFence_JoinsGenerationReadAndReleaseErrors(t *testing.T) {
	dir := t.TempDir()
	const key = "c0ffee01"
	release, err := AcquireSerenaRemovalFence(dir, key)
	if err != nil {
		t.Fatalf("AcquireSerenaRemovalFence: %v", err)
	}
	releaseFenceOrFail(t, release) // retain a leaf so observe takes the lock path.

	readErr := errors.New("simulated generation read failure")
	previousRead := readSerenaRemovalFenceGenerationFn
	readSerenaRemovalFenceGenerationFn = func(string, string) (string, error) { return "", readErr }
	t.Cleanup(func() { readSerenaRemovalFenceGenerationFn = previousRead })
	unlockErr := errors.New("simulated UnlockFileEx failure")
	failingFenceUnlock(t, unlockErr)

	_, err = observeSerenaRemovalFence(dir, key)
	if !errors.Is(err, readErr) || !errors.Is(err, unlockErr) || !errors.Is(err, ErrSerenaRemovalFenceReleaseFailed) {
		t.Fatalf("err = %v, want generation-read, unlock, and release-sentinel causes", err)
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
		defer func() { releaseFenceOrFail(t, release) }()

		before, err := os.ReadFile(intentPath)
		if err != nil {
			t.Fatalf("read intent before: %v", err)
		}

		repaired, deferred, err := repairSerenaIntentForTest(t, mustStateDir(t))
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
		releaseFenceOrFail(t, release)

		repaired, _, err := repairSerenaIntentForTest(t, mustStateDir(t))
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
	prev := observeSerenaRemovalFenceFn
	observeSerenaRemovalFenceFn = func(string, string) (serenaRemovalFenceObservation, error) {
		return serenaRemovalFenceObservation{}, probeErr
	}
	t.Cleanup(func() { observeSerenaRemovalFenceFn = prev })

	repaired, _, err := repairSerenaIntentForTest(t, mustStateDir(t))
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

func TestRepairSerenaIntentFromRegistry_ExpiredLease_ProbeReleaseFailure_FailsClosed(t *testing.T) {
	regPath, intentPath, slowKey := seedSlowUnregisterState(t)
	release, err := AcquireSerenaRemovalFence(filepath.Dir(regPath), slowKey)
	if err != nil {
		t.Fatalf("AcquireSerenaRemovalFence: %v", err)
	}
	releaseFenceOrFail(t, release)

	failingFenceUnlock(t, errors.New("simulated UnlockFileEx failure"))
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent before repair: %v", err)
	}
	repaired, _, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("repair after unconfirmed probe release: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired = %d, want 0 after an unconfirmed probe release", repaired)
	}
	after, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent after repair: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("repair rewrote supervisor intent after an unconfirmed probe release")
	}
	if row := readSerenaRowFresh(t, regPath, slowKey); !row.PendingSerenaRemoval {
		t.Fatal("repair cleared a pending-removal mark after an unconfirmed probe release")
	}
}

// A FRESH mark with no fence leaf has ambiguous legacy provenance. The repair
// still probes to distinguish it from an existing free current fence, but must
// preserve the lease because an older unregister may still be live while
// knowing nothing about the newer fence mechanism.
func TestRepairSerenaIntentFromRegistry_FreshLegacyMarkWithoutFence_PreservesLease(t *testing.T) {
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
	prev := observeSerenaRemovalFenceFn
	observeSerenaRemovalFenceFn = func(dir, key string) (serenaRemovalFenceObservation, error) {
		probed++
		return prev(dir, key)
	}
	t.Cleanup(func() { observeSerenaRemovalFenceFn = prev })

	repaired, _, err := repairSerenaIntentForTest(t, mustStateDir(t))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repaired != 0 {
		t.Errorf("repaired = %d, want 0 (a fresh mark is still honored)", repaired)
	}
	if probed != 1 {
		t.Errorf("fence probed %d times for a fresh legacy mark, want 1", probed)
	}
	if got := readIntent(t, intentPath); got.HasSerenaDaemonForWorkspaceKey(pendingKey) {
		t.Errorf("fresh pending-removal key %q was appended", pendingKey)
	}
	if row := readSerenaRowFresh(t, regPath, pendingKey); !row.PendingSerenaRemoval {
		t.Error("fresh legacy mark was cleared even though missing fence provenance cannot prove the writer is dead")
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
	defer func() { releaseFenceOrFail(t, release) }()

	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatalf("read intent before: %v", err)
	}

	wouldRepair, deferred, err := previewSerenaIntentForTest(t, mustStateDir(t))
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
	defer func() { releaseFenceOrFail(t, release) }()

	// The repair acquires the registry lock, reads BOTH rows, and acts on the
	// unfenced one — a fence that serialized against the registry lock would
	// make this a zero-repair no-op instead.
	repaired, _, err := repairSerenaIntentForTest(t, mustStateDir(t))
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

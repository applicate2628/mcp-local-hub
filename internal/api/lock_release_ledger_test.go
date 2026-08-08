package api

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"

	"github.com/gofrs/flock"
)

func assertRegistryReleased(t testing.TB, release func() error) {
	t.Helper()
	if release == nil {
		t.Fatal("nil Registry release callback")
	}
	if err := release(); err != nil {
		t.Fatalf("release Registry lock: %v", err)
	}
}

func TestLockRelease_ConcurrentOneShotMemoizesFailure(t *testing.T) {
	path := filepath.Join(apitest.HardenedTempDir(t), "one-shot.lock")
	cause := errors.New("synthetic unlock failure")
	var calls atomic.Int32
	release := newLedgeredFlockRelease(path, func() error {
		calls.Add(1)
		return cause
	}, nil)

	const callers = 32
	results := make([]error, callers)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = release()
		}(i)
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("underlying unlock calls = %d, want 1", got)
	}
	for i, err := range results {
		if !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, cause) {
			t.Fatalf("result[%d] = %v, want sentinel and original cause", i, err)
		}
		if err != results[0] {
			t.Fatalf("result[%d] is not the memoized first result", i)
		}
	}
}

func TestGhostRegistryLock_FailsLoudWithoutBlocking(t *testing.T) {
	reg := NewRegistry(filepath.Join(apitest.HardenedTempDir(t), "workspaces.yaml"))
	cause := errors.New("synthetic retained handle")
	recorded := recordUnconfirmedLockRelease(reg.LockPath(), cause)

	if release, locked, err := reg.TryLock(); release != nil || locked || !errors.Is(err, recorded) {
		t.Fatalf("TryLock ghost = (release_non_nil=%t, %v, %v), want (false, false, recorded error)", release != nil, locked, err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := reg.Lock()
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, cause) {
			t.Fatalf("Lock ghost error = %v, want sentinel and cause", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Registry.Lock blocked on a process-local ghost")
	}
}

func TestRegistryLock_GenuineContentionRemainsOrdinary(t *testing.T) {
	reg := NewRegistry(filepath.Join(apitest.HardenedTempDir(t), "workspaces.yaml"))
	held := flock.New(reg.LockPath())
	if err := held.Lock(); err != nil {
		t.Fatalf("hold foreign Registry leaf: %v", err)
	}
	defer func() {
		if err := held.Unlock(); err != nil {
			t.Fatalf("release foreign Registry leaf: %v", err)
		}
	}()

	release, locked, err := reg.TryLock()
	if release != nil || locked || err != nil {
		t.Fatalf("TryLock contention = (release_non_nil=%t, %v, %v), want (false, false, nil)", release != nil, locked, err)
	}
}

func TestLockRelease_FirstFailureWins(t *testing.T) {
	path := filepath.Join(apitest.HardenedTempDir(t), "first-wins.lock")
	first := errors.New("first")
	second := errors.New("second")
	gotFirst := recordUnconfirmedLockRelease(path, first)
	gotSecond := recordUnconfirmedLockRelease(path, second)
	if gotSecond != gotFirst || !errors.Is(gotSecond, first) || errors.Is(gotSecond, second) {
		t.Fatalf("second record = %v, want identical first failure %v", gotSecond, gotFirst)
	}
}

func TestRegistryLock_ProcessExitRestoresLeafAvailability(t *testing.T) {
	if path := os.Getenv("MCPHUB_TEST_EXIT_WITH_REGISTRY_LOCK"); path != "" {
		reg := NewRegistry(path)
		if _, err := reg.Lock(); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}

	path := filepath.Join(apitest.HardenedTempDir(t), "process-exit.yaml")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRegistryLock_ProcessExitRestoresLeafAvailability$")
	cmd.Env = append(os.Environ(), "MCPHUB_TEST_EXIT_WITH_REGISTRY_LOCK="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("locked child exit: %v\n%s", err, output)
	}
	release, err := NewRegistry(path).Lock()
	if err != nil {
		t.Fatalf("parent reacquire after child exit: %v", err)
	}
	assertRegistryReleased(t, release)
}

func TestReleaseAndJoin_PreservesPrimaryAndReleaseCauses(t *testing.T) {
	primaryCause := errors.New("primary")
	releaseCause := errors.New("release")
	err := primaryCause
	ReleaseAndJoin(&err, func() error { return releaseCause })
	if !errors.Is(err, primaryCause) || !errors.Is(err, releaseCause) {
		t.Fatalf("joined error = %v, want both causes", err)
	}
}

func TestLockRelease_PanicUnwindRecordsGhost(t *testing.T) {
	path := filepath.Join(apitest.HardenedTempDir(t), "panic.lock")
	cause := errors.New("panic-unwind release")
	func() {
		defer func() { _ = recover() }()
		release := newLedgeredFlockRelease(path, func() error { return cause }, nil)
		defer func() {
			if err := release(); !errors.Is(err, cause) {
				t.Errorf("panic-unwind release = %v, want cause", err)
			}
		}()
		panic("synthetic")
	}()
	if err := unconfirmedLockRelease(path); !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, cause) {
		t.Fatalf("panic-unwind ledger = %v, want sentinel and cause", err)
	}
}

func TestLockLeafLedgered_ForeignHolderStillBlocksUntilReleased(t *testing.T) {
	path := filepath.Join(apitest.HardenedTempDir(t), "foreign-holder.lock")
	foreign := flock.New(path)
	if err := foreign.Lock(); err != nil {
		t.Fatalf("hold foreign lock leaf: %v", err)
	}
	foreignHeld := true
	t.Cleanup(func() {
		if foreignHeld {
			if err := foreign.Unlock(); err != nil {
				t.Errorf("cleanup foreign lock leaf: %v", err)
			}
		}
	})

	type acquireResult struct {
		release func() error
		err     error
	}
	done := make(chan acquireResult, 1)
	go func() {
		release, err := lockLeafLedgered(path)
		done <- acquireResult{release: release, err: err}
	}()

	entry := lockReleaseLedgerEntryFor(path)
	deadline := time.Now().Add(100 * time.Millisecond)
	for {
		entry.mu.Lock()
		acquiring := entry.acquiring
		entry.mu.Unlock()
		if acquiring {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ledgered acquire did not reserve the foreign-held leaf")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case result := <-done:
		if result.release != nil {
			_ = result.release()
		}
		t.Fatalf("ledgered acquire returned before foreign release: %v", result.err)
	case <-time.After(20 * time.Millisecond):
	}

	if err := foreign.Unlock(); err != nil {
		t.Fatalf("release foreign lock leaf: %v", err)
	}
	foreignHeld = false
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("ledgered acquire after foreign release: %v", result.err)
		}
		if result.release == nil {
			t.Fatal("ledgered acquire after foreign release returned nil release")
		}
		if err := result.release(); err != nil {
			t.Fatalf("release ledgered lock leaf: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("ledgered acquire did not complete after foreign release")
	}
}

func TestLockLeafLedgered_WaiterObservesFailedReleaseBeforeOSAcquire(t *testing.T) {
	path := filepath.Join(apitest.HardenedTempDir(t), "interleaving.lock")
	cause := errors.New("synthetic retained handle")
	var calls atomic.Int32
	var retained *flock.Flock
	firstRelease, err := lockLeafLedgeredWithUnlock(path, func(fl *flock.Flock) error {
		calls.Add(1)
		retained = fl
		return cause
	})
	if err != nil {
		t.Fatalf("acquire first ledgered lock leaf: %v", err)
	}
	t.Cleanup(func() {
		if retained != nil {
			if err := retained.Unlock(); err != nil {
				t.Errorf("cleanup retained first lock leaf: %v", err)
			}
		}
	})

	waiterReady := make(chan struct{})
	type acquireResult struct {
		release func() error
		err     error
	}
	done := make(chan acquireResult, 1)
	go func() {
		release, err := lockLeafLedgeredWithUnlockAndWaitObserver(
			path,
			func(fl *flock.Flock) error { return fl.Unlock() },
			func() { close(waiterReady) },
		)
		done <- acquireResult{release: release, err: err}
	}()
	select {
	case <-waiterReady:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second ledgered acquire did not enter the local-held wait")
	}

	firstErr := firstRelease()
	if !errors.Is(firstErr, ErrLockReleaseUnconfirmed) || !errors.Is(firstErr, cause) {
		t.Fatalf("first ledgered release = %v, want release sentinel and cause", firstErr)
	}
	select {
	case result := <-done:
		if result.release != nil {
			_ = result.release()
			t.Fatal("second ledgered acquire returned a release callback after failed first release")
		}
		if !errors.Is(result.err, ErrLockReleaseUnconfirmed) || !errors.Is(result.err, cause) {
			t.Fatalf("second ledgered acquire = %v, want release sentinel and cause", result.err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("second ledgered acquire waited on this process's retained lock leaf")
	}

	secondErr := firstRelease()
	if secondErr != firstErr {
		t.Fatalf("second release result = %v, want memoized first result %v", secondErr, firstErr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("underlying unlock calls = %d, want 1", got)
	}
}

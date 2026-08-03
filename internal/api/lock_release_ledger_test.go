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
	})

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
		release := newLedgeredFlockRelease(path, func() error { return cause })
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

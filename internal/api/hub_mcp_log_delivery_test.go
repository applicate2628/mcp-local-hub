package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// Only append-file outcomes are synthetic. The real serialization, shared
// append body, production lock and production rotation decision still execute.
type quit041DeliveryFile struct {
	calls                       *[]string
	writeErr, syncErr, closeErr error
	short                       bool
	data                        []byte
}

func (f *quit041DeliveryFile) Write(p []byte) (int, error) {
	*f.calls = append(*f.calls, "write")
	f.data = append([]byte(nil), p...)
	if f.short {
		return len(p) - 1, nil
	}
	return len(p), f.writeErr
}
func (f *quit041DeliveryFile) Sync() error {
	*f.calls = append(*f.calls, "sync")
	return f.syncErr
}
func (f *quit041DeliveryFile) Close() error {
	*f.calls = append(*f.calls, "file-close")
	return f.closeErr
}

// The wrapper records REAL legacy flock closure. unlockErr is a synthetic
// adapter-return failure, not evidence of a native failed unlock or close.
type quit041DeliveryLock struct {
	*flock.Flock
	calls     *[]string
	unlockErr error
	closeErr  error
}

func (l *quit041DeliveryLock) Lock() error {
	*l.calls = append(*l.calls, "lock")
	return l.Flock.Lock()
}
func (l *quit041DeliveryLock) Unlock() error {
	*l.calls = append(*l.calls, "unlock")
	return errors.Join(l.Flock.Unlock(), l.unlockErr)
}

func (l *quit041DeliveryLock) TryLockContextObserved(ctx context.Context, poll time.Duration) (flock.AcquisitionObservation, error) {
	*l.calls = append(*l.calls, "observed-lock")
	return l.Flock.TryLockContextObserved(ctx, poll)
}

func (l *quit041DeliveryLock) ReleaseObserved() (flock.ReleaseObservation, error) {
	*l.calls = append(*l.calls, "observed-release")
	view, err := l.Flock.ReleaseObserved()
	// These are explicitly synthetic adapter outcomes AFTER genuine release.
	// Native failed unlock/close bookkeeping remains the flock owner's tests.
	if l.unlockErr != nil || l.closeErr != nil {
		view.UnlockError, view.FileCloseError = l.unlockErr, l.closeErr
		view.Error = errors.Join(err, l.unlockErr, l.closeErr)
		view.Complete, view.Pending = false, true
		return view, view.Error
	}
	return view, err
}

// A passing diagnostic counterexample intentionally preserves legacy behavior.
// No future checked-entry method is referenced, so this compiles against B37.
func TestQuit041LegacyAppendLosesDeliveryCauses(t *testing.T) {
	for _, scenario := range []string{"sync", "file-close", "write-and-close", "short-write", "unlock"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			var calls []string
			writeCause := errors.New("fixture append write failure")
			syncCause := errors.New("fixture append sync failure")
			closeCause := errors.New("fixture append close failure")
			unlockCause := errors.New("fixture adapter unlock return failure")
			file := &quit041DeliveryFile{calls: &calls}
			effects := hubMcpLogProductionEffects()
			effects.legacyDir = func() (string, error) { return dir, nil }
			effects.lock = func(path string) hubMcpLogLock {
				lock := &quit041DeliveryLock{Flock: flock.New(path), calls: &calls}
				if scenario == "unlock" {
					lock.unlockErr = unlockCause
				}
				return lock
			}
			effects.open = func(string) (hubMcpLogAppendFile, error) {
				calls = append(calls, "open")
				return file, nil
			}
			effects.chmod = func(string) error { calls = append(calls, "chmod"); return nil }
			switch scenario {
			case "sync":
				file.syncErr = syncCause
			case "file-close":
				file.closeErr = closeCause
			case "write-and-close":
				file.writeErr, file.closeErr = writeCause, closeCause
			case "short-write":
				file.short = true
			}
			line, err := effects.marshal("warn", "fixture-quit041-delivery", map[string]any{"event": "cannot-replace-envelope"})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(line, []byte(`"event":"fixture-quit041-delivery"`)) {
				t.Fatal("production marshal did not preserve envelope precedence")
			}
			err = appendHubMcpLogLineCore(line, nil, effects, nil, nil)
			wantCalls := []string{"lock", "open", "chmod", "write", "sync", "file-close", "unlock"}
			if scenario == "write-and-close" {
				wantCalls = []string{"lock", "open", "chmod", "write", "file-close", "unlock"}
				if !errors.Is(err, writeCause) {
					t.Fatal("legacy primary write error was lost")
				}
			} else if err != nil {
				t.Fatalf("legacy projection changed: %v", err)
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("actual effect order = %v; want %v", calls, wantCalls)
			}
			for _, lost := range []error{syncCause, closeCause, unlockCause, io.ErrShortWrite} {
				if errors.Is(err, lost) {
					t.Fatal("legacy projection unexpectedly acquired a delivery cause")
				}
			}
			if !bytes.Equal(file.data, append(line, '\n')) {
				t.Fatal("shared append changed serialized payload")
			}
			t.Logf("existing shared append executed %v; selected delivery failure is absent from legacy result", calls)
		})
	}
}

// Genuine test-owned temporary-file/lock positive for the already supported
// observed pair. This is filesystem receiving, not native failure injection.
func TestQuit041OriginalLogLockObservedPair(t *testing.T) {
	effects := hubMcpLogProductionEffects()
	lock := effects.lock(t.TempDir() + "/fixture.lock")
	acquired, acquireErr := lock.TryLockContextObserved(context.Background(), time.Millisecond)
	released, releaseErr := lock.ReleaseObserved()
	if acquireErr != nil || !acquired.Attempted || !acquired.Returned || !acquired.Held || acquired.Generation == 0 {
		t.Fatalf("actual acquisition incomplete: %+v err=%v", acquired, acquireErr)
	}
	if releaseErr != nil || released.Generation != acquired.Generation || !released.Complete || !released.AllocatedFilesClosed || released.Pending {
		t.Fatalf("same acquisition release incomplete: %+v err=%v", released, releaseErr)
	}
}

func TestQuit041RequiredAppendReceivesActualSharedBodyCauses(t *testing.T) {
	for _, scenario := range []string{"sync", "file-close", "write-and-close", "short-write", "open-and-close", "unlock-and-lock-close"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			var calls []string
			writeCause, syncCause := errors.New("fixture write"), errors.New("fixture sync")
			closeCause, openCause := errors.New("fixture file close"), errors.New("fixture open")
			unlockCause, lockCloseCause := errors.New("fixture observed unlock"), errors.New("fixture observed lock-file close")
			file := &quit041DeliveryFile{calls: &calls}
			effects := hubMcpLogProductionEffects()
			effects.legacyDir = func() (string, error) { return dir, nil }
			var originalLock *quit041DeliveryLock
			effects.lock = func(path string) hubMcpLogLock {
				originalLock = &quit041DeliveryLock{Flock: flock.New(path), calls: &calls}
				if scenario == "unlock-and-lock-close" {
					originalLock.unlockErr, originalLock.closeErr = unlockCause, lockCloseCause
				}
				return originalLock
			}
			effects.open = func(string) (hubMcpLogAppendFile, error) {
				calls = append(calls, "open")
				if scenario == "open-and-close" {
					return file, openCause
				}
				return file, nil
			}
			effects.chmod = func(string) error { calls = append(calls, "chmod"); return nil }
			var want []error
			switch scenario {
			case "sync":
				file.syncErr, want = syncCause, []error{syncCause}
			case "file-close":
				file.closeErr, want = closeCause, []error{closeCause}
			case "write-and-close":
				file.writeErr, file.closeErr, want = writeCause, closeCause, []error{writeCause, closeCause}
			case "short-write":
				file.short, want = true, []error{io.ErrShortWrite}
			case "open-and-close":
				file.closeErr, want = closeCause, []error{openCause, closeCause}
			case "unlock-and-lock-close":
				want = []error{unlockCause, lockCloseCause}
			}
			err := logHubMcpEventRequiredWithEffects("warn", "fixture-required-delivery", nil, effects)
			for _, cause := range want {
				if !errors.Is(err, cause) {
					t.Errorf("lost acquired cause %v from %v", cause, err)
				}
			}
			wantCalls := []string{"observed-lock", "open", "chmod", "write", "sync", "file-close", "observed-release"}
			if scenario == "write-and-close" || scenario == "short-write" {
				wantCalls = []string{"observed-lock", "open", "chmod", "write", "file-close", "observed-release"}
			}
			if scenario == "open-and-close" {
				wantCalls = []string{"observed-lock", "open", "file-close", "observed-release"}
			}
			if !reflect.DeepEqual(calls, wantCalls) {
				t.Fatalf("actual effects=%v want=%v", calls, wantCalls)
			}
			var delivery *hubMcpLogDelivery
			if !errors.As(err, &delivery) {
				t.Fatal("missing original delivery result")
			}
			if file.closeErr != nil && delivery.pendingFile != file {
				t.Error("original uncertain file identity lost")
			}
			if scenario == "unlock-and-lock-close" && delivery.pendingLock != originalLock {
				t.Error("original uncertain lock identity lost")
			}
		})
	}
}

// Synthetic provider observations exercise partial-acquisition receiving only.
// They do not impersonate native flock acquisition/release evidence.
type quit041PartialLogLock struct {
	acquisition            flock.AcquisitionObservation
	release                flock.ReleaseObservation
	acquireErr, releaseErr error
	releases               int
}

func (l *quit041PartialLogLock) Lock() error   { return errors.New("unexpected legacy Lock") }
func (l *quit041PartialLogLock) Unlock() error { return errors.New("unexpected legacy Unlock") }
func (l *quit041PartialLogLock) TryLockContextObserved(context.Context, time.Duration) (flock.AcquisitionObservation, error) {
	return l.acquisition, l.acquireErr
}
func (l *quit041PartialLogLock) ReleaseObserved() (flock.ReleaseObservation, error) {
	l.releases++
	return l.release, l.releaseErr
}

func TestQuit041RequiredAppendReceivesPartialAcquisitionClosure(t *testing.T) {
	for _, missing := range []string{"partial-failure", "attempt", "return", "generation", "held"} {
		t.Run(missing, func(t *testing.T) {
			acquireCause, closeCause := errors.New("fixture partial acquire"), errors.New("fixture partial close")
			lock := &quit041PartialLogLock{
				acquisition: flock.AcquisitionObservation{Generation: 1, Attempted: true, Returned: true, Held: true},
				release:     flock.ReleaseObservation{Generation: 1, AcquisitionAttempted: true, AcquisitionReturned: true, AllocatedFilesClosed: false, Pending: true, FileCloseError: closeCause, Error: closeCause},
				releaseErr:  closeCause,
			}
			switch missing {
			case "partial-failure":
				lock.acquisition.Held, lock.acquireErr = false, acquireCause
			case "attempt":
				lock.acquisition.Attempted = false
			case "return":
				lock.acquisition.Returned = false
			case "generation":
				lock.acquisition.Generation = 0
			case "held":
				lock.acquisition.Held = false
			}
			effects := hubMcpLogProductionEffects()
			dir := t.TempDir()
			effects.legacyDir = func() (string, error) { return dir, nil }
			effects.lock = func(string) hubMcpLogLock { return lock }
			opened := false
			effects.open = func(string) (hubMcpLogAppendFile, error) { opened = true; return nil, errors.New("unexpected append") }
			err := logHubMcpEventRequiredWithEffects("warn", "fixture-partial", nil, effects)
			if err == nil || !errors.Is(err, closeCause) || lock.releases != 1 || opened {
				t.Fatalf("partial cause/closure lost or append admitted: err=%v releases=%d open=%t", err, lock.releases, opened)
			}
			if missing == "partial-failure" && !errors.Is(err, acquireCause) {
				t.Error("original acquisition cause lost")
			}
			var delivery *hubMcpLogDelivery
			if !errors.As(err, &delivery) || delivery.pendingLock != lock {
				t.Error("original partial owner identity lost")
			}
		})
	}
}

func TestQuit041RequiredAppendGenuineTemporaryFileAndLock(t *testing.T) {
	dir := t.TempDir()
	effects := hubMcpLogProductionEffects()
	effects.legacyDir = func() (string, error) { return dir, nil }
	if err := logHubMcpEventRequiredWithEffects("warn", "fixture-genuine-required", nil, effects); err != nil {
		t.Fatal(err)
	}
	line, err := os.ReadFile(filepath.Join(dir, hubMcpLogFileLeaf))
	if err != nil || !bytes.Contains(line, []byte(`"event":"fixture-genuine-required"`)) || !bytes.HasSuffix(line, []byte("\n")) {
		t.Fatalf("actual append not received: %v %q", err, line)
	}
	lock := effects.lock(filepath.Join(dir, hubMcpLogLockFileLeaf))
	acquired, acquireErr := lock.TryLockContextObserved(context.Background(), time.Millisecond)
	released, releaseErr := lock.ReleaseObserved()
	if acquireErr != nil || !acquired.Held || releaseErr != nil || !released.Complete {
		t.Fatal("required append left original temporary lock unavailable")
	}
}

func TestQuit041RequiredAppendEarlyFailureAndPolicyIsolation(t *testing.T) {
	effects := hubMcpLogProductionEffects()
	entered := false
	dirCause := errors.New("fixture logger resolver failure")
	effects.legacyDir = func() (string, error) { entered = true; return "", dirCause }
	if err := logHubMcpEventRequiredWithEffects("warn", "fixture-marshal", map[string]any{"invalid": make(chan struct{})}, effects); err == nil || entered {
		t.Fatal("marshal failure admitted filesystem work")
	}
	if err := logHubMcpEventRequiredWithEffects("warn", "fixture-resolver", nil, effects); !errors.Is(err, dirCause) || !entered {
		t.Fatal("original resolver failure was not received")
	}
	// No fabricated policy observation is constructed. The existing observed
	// route continues to require its genuine original completed operand.
	observation, err := logHubMcpEventObservedWithEffects(context.Background(), nil, "warn", "fixture-policy", nil, hubMcpLogProductionEffects())
	if err == nil || observation.policyReceived || observation.Snapshot().ResourcesComplete {
		t.Fatal("required projection weakened genuine policy-observed admission")
	}
}

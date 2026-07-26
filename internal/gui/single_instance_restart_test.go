package gui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/api/apitest"
)

type phaseEMarkerReaderFunc func() (*HandoffMarkerRecord, error)

func (f phaseEMarkerReaderFunc) Read() (*HandoffMarkerRecord, error) {
	return f()
}

type phaseEUnlockScriptFlock struct {
	unlockErrors []error
	unlockCalls  int
}

type phaseEUnlockFailingRealFlock struct {
	delegate    *flock.Flock
	unlockErr   error
	failCount   int // initial Unlock calls that fail before delegating to the real flock
	unlockCalls int
	closeCalls  int
}

func (f *phaseEUnlockScriptFlock) TryLock() (bool, error) {
	return true, nil
}

func (f *phaseEUnlockScriptFlock) Unlock() error {
	call := f.unlockCalls
	f.unlockCalls++
	if call < len(f.unlockErrors) {
		return f.unlockErrors[call]
	}
	return nil
}

func (f *phaseEUnlockScriptFlock) Close() error {
	return f.Unlock()
}

func (f *phaseEUnlockFailingRealFlock) TryLock() (bool, error) {
	return f.delegate.TryLock()
}

func (f *phaseEUnlockFailingRealFlock) Unlock() error {
	call := f.unlockCalls
	f.unlockCalls++
	if call < f.failCount {
		return f.unlockErr
	}
	return f.delegate.Unlock()
}

func (f *phaseEUnlockFailingRealFlock) Close() error {
	f.closeCalls++
	// Honest model of gofrs/flock: Close() delegates to Unlock() (flock.go:99),
	// so a Close after a persistent Unlock failure fails the SAME way and closes
	// no descriptor — it does NOT bypass the failure.
	return f.Unlock()
}

func phaseEMarkerErrorOnSecondRead(secondErr error) HandoffMarkerReader {
	reads := 0
	return phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) {
		reads++
		if reads == 1 {
			return nil, nil
		}
		return nil, secondErr
	})
}

func phaseEInProgressRecord(now time.Time) *HandoffMarkerRecord {
	return &HandoffMarkerRecord{
		Version:    handoffMarkerVersion,
		Generation: "phase-e-observation",
		Sequence:   7,
		Phase:      HandoffPhaseInProgress,
		Route:      HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     101,
		CreatedAt:  now.Add(-2 * time.Minute),
		UpdatedAt:  now.Add(-time.Minute),
		FreshUntil: now.Add(time.Minute),
	}
}

func phaseEReservedRecord(now time.Time, nonce []byte) *HandoffMarkerRecord {
	record := phaseEInProgressRecord(now)
	record.Sequence++
	record.Phase = HandoffPhaseReserved
	record.ChildPID = 202
	record.DesignatedChildHash = hashDesignatedChildNonce(nonce)
	record.ReservationExpiresAt = now.Add(time.Minute)
	return record
}

func TestSingleInstanceLock_ReleaseRecoversTransientUnlockFailure(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	pidportPath := filepath.Join(dir, "gui.pidport")
	realFlock := flock.New(pidportPath + ".lock")
	locked, err := realFlock.TryLock()
	if err != nil || !locked {
		t.Fatalf("hold real flock: locked=%v err=%v", locked, err)
	}

	transient := errors.New("injected transient unlock failure")
	// Fail Unlock for all-but-the-last bounded retry, then delegate to the real
	// flock: the retry must recover and RELEASE THE REAL OS LOCK before the
	// Close() fallback is ever reached.
	fl := &phaseEUnlockFailingRealFlock{
		delegate:  realFlock,
		unlockErr: transient,
		failCount: singleInstanceUnlockAttempts - 1,
	}
	lease := &SingleInstanceLock{pidport: pidportPath, fl: fl}

	if err := lease.release(); !errors.Is(err, transient) {
		t.Fatalf("release error = %v, want the transient Unlock error %v", err, transient)
	}
	if lease.fl != nil {
		t.Fatal("release retained a flock handle after recovery")
	}
	if fl.closeCalls != 0 {
		t.Fatalf("Close calls = %d, want 0 (retry recovered before the Close fallback)", fl.closeCalls)
	}
	// Proof the real OS lock was released by the delegated Unlock: a fresh
	// acquire on the same pidport wins.
	fresh, err := tryAcquireSingleInstanceLockAt(pidportPath)
	if err != nil {
		t.Fatalf("fresh acquire after transient-failure recovery: %v", err)
	}
	t.Cleanup(func() { _ = fresh.release() })
	if err := lease.release(); err != nil {
		t.Fatalf("idempotent release: %v", err)
	}
}

func TestSingleInstanceLock_ReleasePersistentUnlockFailureClearsHandleAndReports(t *testing.T) {
	// A PERSISTENT Unlock failure (modelling the gofrs/flock limitation where
	// Close()==Unlock() closes no descriptor on a syscall error) must still
	// exhaust the bounded retry + Close fallback, CLEAR the handle, report the
	// original error, and stay idempotent. It does NOT claim to release the OS
	// lock — that residual is bounded by process exit (see release() comment /
	// backlog 2026-07-18-flock-persistent-unlock-residual).
	persistent := errors.New("injected persistent unlock failure")
	errs := make([]error, singleInstanceUnlockAttempts+1)
	for i := range errs {
		errs[i] = persistent
	}
	fl := &phaseEUnlockScriptFlock{unlockErrors: errs}
	lease := &SingleInstanceLock{pidport: "unused.pidport", fl: fl}

	if err := lease.release(); !errors.Is(err, persistent) {
		t.Fatalf("release error = %v, want the persistent Unlock error %v", err, persistent)
	}
	if lease.fl != nil {
		t.Fatal("release must clear the handle even on a persistent Unlock failure")
	}
	if fl.unlockCalls != singleInstanceUnlockAttempts+1 {
		t.Fatalf("Unlock calls = %d, want %d retries + 1 Close-delegated Unlock", fl.unlockCalls, singleInstanceUnlockAttempts+1)
	}
	if err := lease.release(); err != nil {
		t.Fatalf("idempotent release after persistent failure: %v", err)
	}
}

func TestSingleInstanceLock_ReleaseReturnsFirstUnlockErrorAfterRetry(t *testing.T) {
	unlockErr := errors.New("injected unlock failure")
	fl := &phaseEUnlockScriptFlock{unlockErrors: []error{unlockErr, nil}}
	lease := &SingleInstanceLock{fl: fl}

	if err := lease.release(); !errors.Is(err, unlockErr) {
		t.Fatalf("first release error = %v, want %v", err, unlockErr)
	}
	if lease.fl != nil {
		t.Fatal("successful bounded retry retained the flock handle")
	}
	if err := lease.release(); err != nil {
		t.Fatalf("idempotent release after retry success: %v", err)
	}
	if fl.unlockCalls != 2 {
		t.Fatalf("Unlock calls = %d, want 2", fl.unlockCalls)
	}
}

// TestProbeSingleInstanceLockUnheld_UnreleasableProbeLeaseFailsClosed pins the
// review finding: acquiring the flock proves only that nobody ELSE held it at
// that instant. Reporting (true, nil) additionally asserts the lock is unheld
// NOW, which is FALSE while the probe's own lease is still held. The old code
// called the error-discarding Release(), so a lease whose bounded Unlock
// retries AND Close() fallback both failed still reported "definitively
// unheld"; residual 1(b)'s confirmation window then consumed its marker and
// launched a replacement GUI that lost acquisition to this very process and
// exited — while the tick logged a successful relaunch.
//
// MUTATION: swap `lease.release()` back to `lease.Release()` (discarding the
// error) in probeSingleInstanceLockUnheld — this test then fails with
// "unheld=true err=<nil>, want fail-closed".
func TestProbeSingleInstanceLockUnheld_UnreleasableProbeLeaseFailsClosed(t *testing.T) {
	persistent := errors.New("injected persistent probe-lease unlock failure")
	// Every bounded retry AND the Close() fallback fail — gofrs/flock's Close()
	// delegates to Unlock(), so the descriptor stays open and the OS lock stays
	// held until process exit.
	unlockErrors := make([]error, singleInstanceUnlockAttempts+1)
	for i := range unlockErrors {
		unlockErrors[i] = persistent
	}
	fl := &phaseEUnlockScriptFlock{unlockErrors: unlockErrors}
	acquired := &SingleInstanceLock{pidport: "probe.pidport", fl: fl}

	unheld, err := probeSingleInstanceLockUnheld("probe.pidport",
		func(string) (*SingleInstanceLock, error) { return acquired, nil })

	if unheld || err == nil {
		t.Fatalf("probe over an unreleasable lease = unheld=%v err=%v, want fail-closed (false, non-nil): a caller treats (true, nil) as proof no process holds the lock and relaunches the GUI against a lock THIS process still owns", unheld, err)
	}
	if !errors.Is(err, persistent) {
		t.Fatalf("probe error = %v, want it to wrap the underlying Unlock failure %v", err, persistent)
	}
	if fl.unlockCalls != singleInstanceUnlockAttempts+1 {
		t.Fatalf("Unlock calls = %d, want %d bounded retries + 1 Close-delegated Unlock", fl.unlockCalls, singleInstanceUnlockAttempts+1)
	}
}

// TestProbeSingleInstanceLockUnheld_ReleasableLeaseReportsUnheld pins the other
// half: the ordinary path still reports a definitively-unheld lock, so the
// fail-closed branch above cannot be satisfied by simply never returning true.
func TestProbeSingleInstanceLockUnheld_ReleasableLeaseReportsUnheld(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	pidportPath := filepath.Join(dir, "gui.pidport")

	unheld, err := ProbeSingleInstanceLockUnheld(pidportPath)
	if err != nil || !unheld {
		t.Fatalf("probe over a free lock = unheld=%v err=%v, want (true, nil)", unheld, err)
	}

	// And a live holder still reads as definitively held, with no error.
	held, err := tryAcquireSingleInstanceLockAt(pidportPath)
	if err != nil {
		t.Fatalf("hold the lock: %v", err)
	}
	t.Cleanup(func() { held.Release() })
	unheld, err = ProbeSingleInstanceLockUnheld(pidportPath)
	if err != nil || unheld {
		t.Fatalf("probe over a held lock = unheld=%v err=%v, want (false, nil)", unheld, err)
	}
}

func TestRestartV3_RejectedLeaseUnlockFailureIsUnknownAndReleased(t *testing.T) {
	unlockErr := errors.New("injected rejected-lease unlock failure")
	fl := &phaseEUnlockScriptFlock{unlockErrors: []error{unlockErr, nil}}
	lease := &SingleInstanceLock{fl: fl}

	err := releaseTentativeLeaseWithReason(lease, ErrHandoffReserved)
	if !errors.Is(err, ErrGUIOwnerLeaseUnknown) || !errors.Is(err, ErrHandoffReserved) || !errors.Is(err, unlockErr) {
		t.Fatalf("rejected lease release error = %v, want Unknown wrapping reservation and Unlock errors", err)
	}
	if lease.fl != nil {
		t.Fatal("rejected lease release retained the flock handle")
	}
	if err := lease.release(); err != nil {
		t.Fatalf("idempotent rejected lease release: %v", err)
	}
	if fl.unlockCalls != 2 {
		t.Fatalf("Unlock calls = %d, want 2", fl.unlockCalls)
	}
}

func TestRestartV3_DesignatedChildHashRequiresCanonicalSHA256(t *testing.T) {
	valid := hashDesignatedChildNonce([]byte("phase-e-canonical-hash"))
	tests := []struct {
		name  string
		hash  string
		valid bool
	}{
		{name: "canonical", hash: valid, valid: true},
		{name: "uppercase hex", hash: "sha256:" + strings.ToUpper(strings.TrimPrefix(valid, "sha256:"))},
		{name: "wrong prefix", hash: "sha512:" + strings.TrimPrefix(valid, "sha256:")},
		{name: "non hex", hash: "sha256:" + strings.Repeat("g", 64)},
		{name: "short", hash: "sha256:00"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCanonicalDesignatedChildHash(tc.hash); got != tc.valid {
				t.Fatalf("isCanonicalDesignatedChildHash(%q) = %t, want %t", tc.hash, got, tc.valid)
			}
		})
	}
}

func TestRestartV3_MalformedDesignatedChildHashRejectsDesignatedEntrant(t *testing.T) {
	now := time.Date(2026, 7, 17, 11, 30, 0, 0, time.UTC)
	nonce := []byte("phase-e-malformed-hash")
	record := phaseEReservedRecord(now, nonce)
	record.DesignatedChildHash = "sha256:" + strings.ToUpper(strings.TrimPrefix(record.DesignatedChildHash, "sha256:"))
	pidportPath := filepath.Join(apitest.HardenedTempDir(t), "gui.pidport")

	lease, err := AcquireSingleInstanceAt(pidportPath, 9125, SingleInstanceAcquireOptions{
		RestartV3Enabled:     true,
		MarkerStore:          phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) { return record, nil }),
		DesignatedChildNonce: nonce,
		Deadlines:            restartTestDeadlines(&now),
	})
	if lease != nil {
		lease.Release()
		t.Fatal("malformed designated child hash returned an owned lease")
	}
	if !errors.Is(err, ErrHandoffReserved) {
		t.Fatalf("malformed designated child hash error = %v, want ErrHandoffReserved", err)
	}
	assertSingleInstanceFlockAvailable(t, pidportPath)
}

func TestRestartV3_ReservationRejectsThirdEntrantAndDesignatedChildWins(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	deadlines := restartTestDeadlines(&now)
	stateDir := apitest.HardenedTempDir(t)
	pidportPath := filepath.Join(stateDir, "gui.pidport")
	store := NewHandoffMarkerStore(stateDir, deadlines)
	nonce := []byte("phase-e-owner-only-designated-child-nonce")

	inProgress, err := store.Begin(HandoffBegin{
		Generation: "phase-e-reservation",
		Route:      HandoffRouteSamePort,
		OldPort:    9125,
		NewPort:    9125,
		OldPID:     101,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Reserve(
		inProgress.Generation,
		inProgress.Sequence,
		now.Add(deadlines.Reservation),
		hashDesignatedChildNonce(nonce),
		202,
	); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	ordinaryOptions := SingleInstanceAcquireOptions{
		RestartV3Enabled: true,
		MarkerStore:      store,
		Deadlines:        deadlines,
	}
	if lease, err := AcquireSingleInstanceAt(pidportPath, 9125, ordinaryOptions); !errors.Is(err, ErrHandoffReserved) {
		if lease != nil {
			lease.Release()
		}
		t.Fatalf("ordinary entrant error = %v, want ErrHandoffReserved", err)
	}
	assertSingleInstanceFlockAvailable(t, pidportPath)

	wrongChildOptions := ordinaryOptions
	wrongChildOptions.DesignatedChildNonce = []byte("wrong-owner-only-nonce")
	if lease, err := AcquireSingleInstanceAt(pidportPath, 9125, wrongChildOptions); !errors.Is(err, ErrHandoffReserved) {
		if lease != nil {
			lease.Release()
		}
		t.Fatalf("wrong child error = %v, want ErrHandoffReserved", err)
	}
	assertSingleInstanceFlockAvailable(t, pidportPath)

	designatedOptions := ordinaryOptions
	designatedOptions.DesignatedChildNonce = nonce
	designatedLease, err := AcquireSingleInstanceAt(pidportPath, 9125, designatedOptions)
	if err != nil {
		t.Fatalf("designated child acquire: %v", err)
	}
	defer designatedLease.Release()
	assertSingleInstanceFlockHeld(t, pidportPath)

	raw, err := os.ReadFile(pidportPath)
	if err != nil {
		t.Fatalf("read designated child pidport: %v", err)
	}
	want := []byte(formatPidport(os.Getpid(), 9125))
	if string(raw) != string(want) {
		t.Fatalf("designated child pidport = %q, want %q", raw, want)
	}
}

func TestRestartV3_ReservedDeadlineOutlivesGenerationFreshness(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 30, 0, 0, time.UTC)
	deadlines := restartTestDeadlines(&now)
	pidportPath := filepath.Join(apitest.HardenedTempDir(t), "gui.pidport")
	nonce := []byte("phase-e-crossed-deadline")
	record := phaseEReservedRecord(now, nonce)
	record.FreshUntil = now.Add(-time.Second)
	reader := phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) { return record, nil })

	ordinaryLease, err := AcquireSingleInstanceAt(pidportPath, 9125, SingleInstanceAcquireOptions{
		RestartV3Enabled: true,
		MarkerStore:      reader,
		Deadlines:        deadlines,
	})
	if ordinaryLease != nil {
		ordinaryLease.Release()
		t.Fatal("ordinary entrant received a lease inside the reservation-only deadline")
	}
	if !errors.Is(err, ErrHandoffReserved) {
		t.Fatalf("ordinary entrant error = %v, want ErrHandoffReserved", err)
	}
	assertSingleInstanceFlockAvailable(t, pidportPath)

	probe := ProbeGUIOwnerLease(context.Background(), GUIOwnerLeaseProbeRequest{
		PidportPath: pidportPath,
		Record:      record,
		MarkerStore: reader,
		Deadlines:   deadlines,
	})
	if probe.State != GUIOwnerLeaseStateHeld || !errors.Is(probe.Reason, ErrHandoffReserved) || probe.Lease != nil {
		if probe.Lease != nil {
			probe.Lease.Release()
		}
		t.Fatalf("crossed-deadline probe = state %v lease %T reason %v, want Held reservation", probe.State, probe.Lease, probe.Reason)
	}
	assertSingleInstanceFlockAvailable(t, pidportPath)

	designatedLease, err := AcquireSingleInstanceAt(pidportPath, 9125, SingleInstanceAcquireOptions{
		RestartV3Enabled:     true,
		MarkerStore:          reader,
		DesignatedChildNonce: nonce,
		Deadlines:            deadlines,
	})
	if err != nil {
		t.Fatalf("designated child acquire inside reservation-only deadline: %v", err)
	}
	assertSingleInstanceFlockHeld(t, pidportPath)
	if err := designatedLease.release(); err != nil {
		t.Fatalf("release designated child lease: %v", err)
	}
	assertSingleInstanceFlockAvailable(t, pidportPath)
}

func TestRestartV3_RawReservedFreeFlockMapsHeldDuringWindow(t *testing.T) {
	now := time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC)
	deadlines := restartTestDeadlines(&now)
	stateDir := apitest.HardenedTempDir(t)
	pidportPath := filepath.Join(stateDir, "gui.pidport")
	store := NewHandoffMarkerStore(stateDir, deadlines)

	inProgress, err := store.Begin(HandoffBegin{
		Generation: "phase-e-raw-reserved",
		Route:      HandoffRoutePortChange,
		OldPort:    9125,
		NewPort:    9230,
		OldPID:     303,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	record, err := store.Reserve(
		inProgress.Generation,
		inProgress.Sequence,
		now.Add(deadlines.Reservation),
		hashDesignatedChildNonce([]byte("designated-child")),
		404,
	)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	// Prove the OS flock is momentarily free before the probe. The raw
	// reservation must nevertheless map to Held without spawning or returning
	// an owned Free lease.
	assertSingleInstanceFlockAvailable(t, pidportPath)
	result := ProbeGUIOwnerLease(context.Background(), GUIOwnerLeaseProbeRequest{
		PidportPath: pidportPath,
		Record:      record,
		MarkerStore: store,
		Deadlines:   deadlines,
	})
	if result.State != GUIOwnerLeaseStateHeld {
		if result.Lease != nil {
			result.Lease.Release()
		}
		t.Fatalf("probe state = %v, want Held (reason %v)", result.State, result.Reason)
	}
	if !errors.Is(result.Reason, ErrHandoffReserved) {
		t.Fatalf("probe reason = %v, want ErrHandoffReserved", result.Reason)
	}
	if result.Lease != nil {
		result.Lease.Release()
		t.Fatal("Held probe returned an owned lease")
	}
	assertSingleInstanceFlockAvailable(t, pidportPath)
}

func TestRestartV3_ProbeUnknownReleasesTentativeLease(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC)
	deadlines := restartTestDeadlines(&now)

	tests := []struct {
		name           string
		pidportPath    func(string) string
		ctx            func() context.Context
		markerStore    HandoffMarkerReader
		wantCause      error
		assertReleased bool
	}{
		{
			name:        "path uncertainty",
			pidportPath: func(string) string { return "" },
			ctx:         context.Background,
			markerStore: phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) { return nil, nil }),
		},
		{
			name:           "marker uncertainty",
			pidportPath:    func(dir string) string { return filepath.Join(dir, "marker.gui.pidport") },
			ctx:            context.Background,
			markerStore:    phaseEMarkerErrorOnSecondRead(errors.New("marker changed to corrupt during probe")),
			assertReleased: true,
		},
		{
			name:           "DACL uncertainty",
			pidportPath:    func(dir string) string { return filepath.Join(dir, "dacl.gui.pidport") },
			ctx:            context.Background,
			markerStore:    phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) { return nil, os.ErrPermission }),
			wantCause:      os.ErrPermission,
			assertReleased: true,
		},
		{
			name:        "flock uncertainty",
			pidportPath: func(dir string) string { return filepath.Join(dir, "missing", "gui.pidport") },
			ctx:         context.Background,
			markerStore: phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) { return nil, nil }),
		},
		{
			name:        "cancelled after tentative acquire",
			pidportPath: func(dir string) string { return filepath.Join(dir, "cancel.gui.pidport") },
			ctx: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			markerStore:    phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) { return nil, nil }),
			wantCause:      context.Canceled,
			assertReleased: true,
		},
		{
			name:        "timed out after tentative acquire",
			pidportPath: func(dir string) string { return filepath.Join(dir, "timeout.gui.pidport") },
			ctx: func() context.Context {
				ctx, cancel := context.WithDeadline(context.Background(), now.Add(-time.Second))
				t.Cleanup(cancel)
				return ctx
			},
			markerStore:    phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) { return nil, nil }),
			wantCause:      context.DeadlineExceeded,
			assertReleased: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := apitest.HardenedTempDir(t)
			pidportPath := tc.pidportPath(dir)
			result := ProbeGUIOwnerLease(tc.ctx(), GUIOwnerLeaseProbeRequest{
				PidportPath: pidportPath,
				MarkerStore: tc.markerStore,
				Deadlines:   deadlines,
			})
			if result.State != GUIOwnerLeaseStateUnknown {
				if result.Lease != nil {
					result.Lease.Release()
				}
				t.Fatalf("probe state = %v, want Unknown (reason %v)", result.State, result.Reason)
			}
			if !errors.Is(result.Reason, ErrGUIOwnerLeaseUnknown) {
				t.Fatalf("probe reason = %v, want ErrGUIOwnerLeaseUnknown", result.Reason)
			}
			if tc.wantCause != nil && !errors.Is(result.Reason, tc.wantCause) {
				t.Fatalf("probe reason = %v, want cause %v", result.Reason, tc.wantCause)
			}
			if result.Lease != nil {
				result.Lease.Release()
				t.Fatal("Unknown probe returned an owned lease")
			}
			if tc.assertReleased {
				assertSingleInstanceFlockAvailable(t, pidportPath)
			}
		})
	}
}

func TestRestartV3_ProbeMarkerObservationChangesAreUnknown(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 15, 0, 0, time.UTC)
	base := phaseEInProgressRecord(now)
	sequenceChanged := *base
	sequenceChanged.Sequence++
	fieldChanged := *base
	fieldChanged.NewPort = 9230
	reservationAppeared := phaseEReservedRecord(now, []byte("phase-e-probe-reservation"))

	tests := []struct {
		name               string
		requestRecord      *HandoffMarkerRecord
		firstRead          *HandoffMarkerRecord
		secondRead         *HandoffMarkerRecord
		wantReads          int
		wantReasonFragment string
		wantFlockUntouched bool
	}{
		{
			name:               "sequence changes after acquire",
			firstRead:          base,
			secondRead:         &sequenceChanged,
			wantReads:          2,
			wantReasonFragment: "handoff marker changed during owner probe",
		},
		{
			name:               "complete record field changes after acquire",
			firstRead:          base,
			secondRead:         &fieldChanged,
			wantReads:          2,
			wantReasonFragment: "handoff marker changed during owner probe",
		},
		{
			name:               "reservation appears after acquire",
			firstRead:          nil,
			secondRead:         reservationAppeared,
			wantReads:          2,
			wantReasonFragment: "handoff marker changed during owner probe",
		},
		{
			name:               "request record differs from first read",
			requestRecord:      base,
			firstRead:          &sequenceChanged,
			wantReads:          1,
			wantReasonFragment: "handoff marker changed before owner probe",
			wantFlockUntouched: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pidportPath := filepath.Join(apitest.HardenedTempDir(t), "gui.pidport")
			reads := 0
			reader := phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) {
				reads++
				switch reads {
				case 1:
					return tc.firstRead, nil
				case 2:
					return tc.secondRead, nil
				default:
					t.Fatalf("marker reads exceeded two-read seam: %d", reads)
					return nil, nil
				}
			})

			result := ProbeGUIOwnerLease(context.Background(), GUIOwnerLeaseProbeRequest{
				PidportPath: pidportPath,
				Record:      tc.requestRecord,
				MarkerStore: reader,
				Deadlines:   restartTestDeadlines(&now),
			})
			if result.State != GUIOwnerLeaseStateUnknown {
				if result.Lease != nil {
					result.Lease.Release()
				}
				t.Fatalf("probe state = %v, want Unknown (reason %v)", result.State, result.Reason)
			}
			if !errors.Is(result.Reason, ErrGUIOwnerLeaseUnknown) || !strings.Contains(result.Reason.Error(), tc.wantReasonFragment) {
				t.Fatalf("probe reason = %v, want Unknown containing %q", result.Reason, tc.wantReasonFragment)
			}
			if result.Lease != nil {
				result.Lease.Release()
				t.Fatal("changed marker probe returned an owned lease")
			}
			if reads != tc.wantReads {
				t.Fatalf("marker reads = %d, want %d", reads, tc.wantReads)
			}
			if tc.wantFlockUntouched {
				if _, err := os.Stat(pidportPath + ".lock"); !os.IsNotExist(err) {
					t.Fatalf("request mismatch touched flock path: stat error = %v", err)
				}
				return
			}
			assertSingleInstanceFlockAvailable(t, pidportPath)
		})
	}
}

func TestRestartV3_ProbeDACLUncertaintyIsUnknownEvenWhenFlockHeld(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 30, 0, 0, time.UTC)
	deadlines := restartTestDeadlines(&now)
	dir := apitest.HardenedTempDir(t)
	pidportPath := filepath.Join(dir, "gui.pidport")

	incumbent := flock.New(pidportPath + ".lock")
	locked, err := incumbent.TryLock()
	if err != nil || !locked {
		t.Fatalf("hold incumbent flock: locked=%v err=%v", locked, err)
	}
	defer func() {
		if err := incumbent.Unlock(); err != nil {
			t.Errorf("release incumbent flock: %v", err)
		}
	}()

	result := ProbeGUIOwnerLease(context.Background(), GUIOwnerLeaseProbeRequest{
		PidportPath: pidportPath,
		MarkerStore: phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) {
			return nil, os.ErrPermission
		}),
		Deadlines: deadlines,
	})
	if result.State != GUIOwnerLeaseStateUnknown {
		t.Fatalf("probe state = %v, want Unknown rather than Held", result.State)
	}
	if !errors.Is(result.Reason, ErrGUIOwnerLeaseUnknown) || !errors.Is(result.Reason, os.ErrPermission) {
		t.Fatalf("probe reason = %v, want Unknown wrapping DACL error", result.Reason)
	}
	if result.Lease != nil {
		result.Lease.Release()
		t.Fatal("Unknown DACL probe returned an owned lease")
	}
}

func TestRestartV3_ReservationAcquireUnknownReleasesTentativeLease(t *testing.T) {
	now := time.Date(2026, 7, 17, 15, 0, 0, 0, time.UTC)
	deadlines := restartTestDeadlines(&now)
	dir := apitest.HardenedTempDir(t)
	pidportPath := filepath.Join(dir, "gui.pidport")

	lease, err := AcquireSingleInstanceAt(pidportPath, 9125, SingleInstanceAcquireOptions{
		RestartV3Enabled: true,
		MarkerStore: phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) {
			return nil, os.ErrPermission
		}),
		Deadlines: deadlines,
	})
	if lease != nil {
		lease.Release()
		t.Fatal("unknown reservation acquire returned a lease")
	}
	if !errors.Is(err, ErrGUIOwnerLeaseUnknown) || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("acquire error = %v, want Unknown wrapping permission failure", err)
	}
	assertSingleInstanceFlockAvailable(t, pidportPath)
	if _, statErr := os.Stat(pidportPath); !os.IsNotExist(statErr) {
		t.Fatalf("unknown acquire wrote pidport: stat error = %v", statErr)
	}
}

func TestRestartV3_FreeProbeLeaseReleasedOnEveryConsumerPath(t *testing.T) {
	now := time.Date(2026, 7, 17, 16, 0, 0, 0, time.UTC)
	deadlines := restartTestDeadlines(&now)

	tests := []struct {
		name    string
		consume func(context.Context) error
	}{
		{name: "success", consume: func(context.Context) error { return nil }},
		{name: "error", consume: func(context.Context) error { return errors.New("consumer failure") }},
		{name: "cancel", consume: func(ctx context.Context) error {
			cancelCtx, cancel := context.WithCancel(ctx)
			cancel()
			return cancelCtx.Err()
		}},
		{name: "timeout", consume: func(context.Context) error {
			timeoutCtx, cancel := context.WithDeadline(context.Background(), now.Add(-time.Second))
			defer cancel()
			return timeoutCtx.Err()
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := apitest.HardenedTempDir(t)
			pidportPath := filepath.Join(dir, "gui.pidport")
			result := ProbeGUIOwnerLease(context.Background(), GUIOwnerLeaseProbeRequest{
				PidportPath: pidportPath,
				MarkerStore: phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) { return nil, nil }),
				Deadlines:   deadlines,
			})
			if result.State != GUIOwnerLeaseStateFree || result.Lease == nil {
				t.Fatalf("probe result = state %v lease %T reason %v, want Free owned lease", result.State, result.Lease, result.Reason)
			}

			_ = func() error {
				defer result.Lease.Release()
				// The guarded operation runs while this exact probe lease remains
				// held; there is no release followed by a second acquisition.
				assertSingleInstanceFlockHeld(t, pidportPath)
				return tc.consume(context.Background())
			}()

			assertSingleInstanceFlockAvailable(t, pidportPath)
		})
	}
}

func TestRestartV3_GateOffAcquireIsLegacyByteIdenticalAndReadsNoMarker(t *testing.T) {
	t.Run("gate off reads no marker", func(t *testing.T) {
		dir := apitest.HardenedTempDir(t)
		pidportPath := filepath.Join(dir, "gui.pidport")
		markerReads := 0

		lease, err := AcquireSingleInstanceAt(pidportPath, 9125, SingleInstanceAcquireOptions{
			RestartV3Enabled: false,
			MarkerStore: phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) {
				markerReads++
				return nil, errors.New("gate-off marker read must not happen")
			}),
			DesignatedChildNonce: []byte("ignored-while-gate-off"),
			Deadlines:            RestartDeadlines{},
		})
		if err != nil {
			t.Fatalf("gate-off acquire: %v", err)
		}
		defer lease.Release()
		if markerReads != 0 {
			t.Fatalf("gate-off marker reads = %d, want zero", markerReads)
		}
		assertLegacyPidportBytes(t, pidportPath, 9125)
	})

	t.Run("gate on absent marker keeps legacy pidport bytes", func(t *testing.T) {
		now := time.Date(2026, 7, 17, 17, 0, 0, 0, time.UTC)
		dir := apitest.HardenedTempDir(t)
		pidportPath := filepath.Join(dir, "gui.pidport")
		markerReads := 0

		lease, err := AcquireSingleInstanceAt(pidportPath, 9125, SingleInstanceAcquireOptions{
			RestartV3Enabled: true,
			MarkerStore: phaseEMarkerReaderFunc(func() (*HandoffMarkerRecord, error) {
				markerReads++
				return nil, nil
			}),
			Deadlines: restartTestDeadlines(&now),
		})
		if err != nil {
			t.Fatalf("gate-on absent-marker acquire: %v", err)
		}
		defer lease.Release()
		if markerReads != 1 {
			t.Fatalf("gate-on absent-marker reads = %d, want one", markerReads)
		}
		assertLegacyPidportBytes(t, pidportPath, 9125)
	})
}

func assertLegacyPidportBytes(t *testing.T, pidportPath string, port int) {
	t.Helper()
	raw, err := os.ReadFile(pidportPath)
	if err != nil {
		t.Fatalf("read pidport: %v", err)
	}
	want := []byte(formatPidport(os.Getpid(), port))
	if string(raw) != string(want) {
		t.Fatalf("pidport bytes = %q, want legacy bytes %q", raw, want)
	}
}

func assertSingleInstanceFlockAvailable(t *testing.T, pidportPath string) {
	t.Helper()
	fl := flock.New(pidportPath + ".lock")
	ok, err := fl.TryLock()
	if err != nil {
		t.Fatalf("probe raw flock availability: %v", err)
	}
	if !ok {
		t.Fatal("raw flock remains held, want available")
	}
	if err := fl.Unlock(); err != nil {
		t.Fatalf("release raw flock availability probe: %v", err)
	}
}

func assertSingleInstanceFlockHeld(t *testing.T, pidportPath string) {
	t.Helper()
	fl := flock.New(pidportPath + ".lock")
	ok, err := fl.TryLock()
	if err != nil {
		t.Fatalf("probe raw flock held state: %v", err)
	}
	if ok {
		_ = fl.Unlock()
		t.Fatal("raw flock was acquirable, want held by owned lease")
	}
}

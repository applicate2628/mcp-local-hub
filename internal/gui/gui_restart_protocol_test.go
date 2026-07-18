package gui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

type phaseFLease struct {
	mu       sync.Mutex
	releases int
}

func (l *phaseFLease) Release() {
	l.mu.Lock()
	l.releases++
	l.mu.Unlock()
}

func (l *phaseFLease) releaseCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releases
}

type phaseFMarkerStore struct {
	mu             sync.Mutex
	record         *HandoffMarkerRecord
	readErr        error
	commitErrs     []error
	interruptErr   error
	commitCalls    int
	interruptCalls int
	commitHook     func()
}

func (s *phaseFMarkerStore) Interrupt(generation, reasonCode, operatorAction string) (*HandoffMarkerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interruptCalls++
	if s.interruptErr != nil {
		return nil, s.interruptErr
	}
	if s.record == nil || s.record.Generation != generation || !s.record.Phase.nonterminal() {
		return nil, ErrHandoffMarkerStateMismatch
	}
	s.record.Phase = HandoffPhaseInterrupted
	s.record.ReasonCode = reasonCode
	s.record.OperatorAction = operatorAction
	s.record.Sequence++
	copy := *s.record
	return &copy, nil
}

func (s *phaseFMarkerStore) Read() (*HandoffMarkerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return nil, s.readErr
	}
	if s.record == nil {
		return nil, nil
	}
	copy := *s.record
	return &copy, nil
}

func (s *phaseFMarkerStore) Commit(generation string, boundPort int) (*HandoffMarkerRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitCalls++
	if s.commitHook != nil {
		s.commitHook()
	}
	if len(s.commitErrs) > 0 {
		err := s.commitErrs[0]
		s.commitErrs = s.commitErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if s.record == nil || s.record.Generation != generation || s.record.Phase != HandoffPhaseReserved {
		return nil, ErrHandoffMarkerStateMismatch
	}
	s.record.Phase = HandoffPhaseCommitted
	s.record.NewPort = boundPort
	s.record.Sequence++
	copy := *s.record
	return &copy, nil
}

func (s *phaseFMarkerStore) setRecord(record *HandoffMarkerRecord) {
	s.mu.Lock()
	s.record = record
	s.mu.Unlock()
}

func (s *phaseFMarkerStore) snapshot() (*HandoffMarkerRecord, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var record *HandoffMarkerRecord
	if s.record != nil {
		copy := *s.record
		record = &copy
	}
	return record, s.commitCalls, s.interruptCalls
}

type phaseFStandby struct {
	mu         sync.Mutex
	closes     int
	lastBudget time.Duration
}

func (s *phaseFStandby) CloseListener(ctx context.Context) error {
	s.mu.Lock()
	s.closes++
	if deadline, ok := ctx.Deadline(); ok {
		s.lastBudget = time.Until(deadline)
	}
	s.mu.Unlock()
	return nil
}

func (s *phaseFStandby) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

func (s *phaseFStandby) closeBudget() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastBudget
}

type phaseFEvents struct {
	mu     sync.Mutex
	events []Event
}

type phaseFParentDeathWatcher struct {
	done      chan struct{}
	deathOnce sync.Once
}

func newPhaseFParentDeathWatcher() *phaseFParentDeathWatcher {
	return &phaseFParentDeathWatcher{done: make(chan struct{})}
}

func (w *phaseFParentDeathWatcher) Done() <-chan struct{} { return w.done }

func (w *phaseFParentDeathWatcher) Close() error { return nil }

func (w *phaseFParentDeathWatcher) SignalDeath() {
	w.deathOnce.Do(func() { close(w.done) })
}

func (e *phaseFEvents) Publish(event Event) {
	e.mu.Lock()
	e.events = append(e.events, event)
	e.mu.Unlock()
}

func (e *phaseFEvents) types() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	types := make([]string, len(e.events))
	for i := range e.events {
		types[i] = e.events[i].Type
	}
	return types
}

func (e *phaseFEvents) snapshot() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Event(nil), e.events...)
}

func phaseFChild(stateDir string) *SpawnedGUIChild {
	return &SpawnedGUIChild{
		Handoff: SelfRestartHandoff{
			Version:    1,
			HandoffID:  "handoff-f",
			Generation: "generation-f",
			Sequence:   1,
			OldPort:    9125,
			TargetPort: 19125,
			ParentPID:  1111,
			NoncePath:  filepath.Join(stateDir, "gui-restart-nonce"),
		},
		PID:         4242,
		parentDeath: newPhaseFParentDeathWatcher(),
	}
}

func phaseFReservedRecord(now time.Time) *HandoffMarkerRecord {
	return &HandoffMarkerRecord{
		Version:              handoffMarkerVersion,
		Generation:           "generation-f",
		Sequence:             2,
		Phase:                HandoffPhaseReserved,
		Route:                HandoffRoutePortChange,
		OldPort:              9125,
		NewPort:              19125,
		OldPID:               1111,
		ChildPID:             4242,
		DesignatedChildHash:  hashDesignatedChildNonce(bytes.Repeat([]byte{0x5a}, 32)),
		CreatedAt:            now.Add(-time.Second),
		UpdatedAt:            now,
		FreshUntil:           now.Add(3 * time.Minute),
		ReservationExpiresAt: now.Add(10 * time.Second),
	}
}

func TestPhaseFChildNoncePathUsesTestTempDir(t *testing.T) {
	stateDir := t.TempDir()
	child := phaseFChild(stateDir)
	rel, err := filepath.Rel(stateDir, child.Handoff.NoncePath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("fixture nonce path %q is outside test temp dir %q (rel=%q err=%v)", child.Handoff.NoncePath, stateDir, rel, err)
	}
}

func phaseFDependencies(now *time.Time, marker *phaseFMarkerStore, lease *phaseFLease, standby *phaseFStandby, events *phaseFEvents) RestartChildDependencies {
	return RestartChildDependencies{
		MarkerStore: marker,
		Standby:     standby,
		Events:      events,
		Runtime:     NewRestartChildRuntimeSettlement(),
		Deadlines: RestartDeadlines{
			Now:         func() time.Time { return *now },
			Proof:       10 * time.Second,
			Reservation: 10 * time.Second,
		},
		RetryInterval: 10 * time.Millisecond,
		Wait: func(ctx context.Context, d time.Duration) error {
			*now = now.Add(d)
			return ctx.Err()
		},
		Acquire: func(context.Context) (SingleInstanceLease, error) { return lease, nil },
	}
}

func TestRestartV3_NonceRetainedHandleDefeatsPIDReuseAndNeverUsesEnvironment(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	noncePath := filepath.Join(stateDir, "gui-restart-nonce")
	nonce := bytes.Repeat([]byte{0x7c}, 32)
	if err := api.WriteStateFileBytesAtomic(noncePath, nonce); err != nil {
		t.Fatalf("write hardened nonce file: %v", err)
	}
	payload := SelfRestartHandoff{
		Version:    1,
		HandoffID:  "handoff-f",
		Generation: "generation-f",
		Sequence:   1,
		OldPort:    9125,
		TargetPort: 19125,
		ParentPID:  os.Getppid(),
		NoncePath:  noncePath,
	}
	raw, err := EncodeSelfRestartHandoff(payload)
	if err != nil {
		t.Fatalf("EncodeSelfRestartHandoff: %v", err)
	}
	decoded, err := decodeSelfRestartHandoff(raw)
	if err != nil || decoded.NoncePath != noncePath {
		t.Fatalf("handoff environment nonce path = %q, err=%v; want %q", decoded.NoncePath, err, noncePath)
	}
	if strings.Contains(raw, string(nonce)) || strings.Contains(strings.Join(os.Args, "\x00"), string(nonce)) {
		t.Fatal("raw nonce appeared in environment payload or argv")
	}

	child, err := NewSpawnedGUIChildFromEnvironment(raw, 4242, stateDir)
	if err != nil {
		t.Fatalf("NewSpawnedGUIChildFromEnvironment: %v", err)
	}
	t.Cleanup(child.Close)
	if _, err := os.Stat(noncePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("one-shot nonce file still exists after child consumption: %v", err)
	}

	proof, err := child.Readiness.proof("parent-challenge-f")
	if err != nil {
		t.Fatalf("readiness proof: %v", err)
	}
	expected := AuthenticatedReadinessIdentity{
		HandoffID: "handoff-f", Generation: "generation-f", Sequence: 1, PID: 4242, Port: 19125,
	}
	if !VerifyAuthenticatedReadiness(nonce, "parent-challenge-f", proof, expected) {
		t.Fatal("exact retained-child proof did not verify")
	}
	pidReuse := expected
	pidReuse.PID++
	if VerifyAuthenticatedReadiness(nonce, "parent-challenge-f", proof, pidReuse) {
		t.Fatal("same-port PID-reuse identity verified without the matching MAC-bound child")
	}
}

func TestRestartV3_MalformedNonceIsUnlinkedOnConsumeFailure(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	noncePath := filepath.Join(stateDir, "gui-restart-nonce")
	if err := api.WriteStateFileBytesAtomic(noncePath, []byte("short")); err != nil {
		t.Fatalf("write malformed nonce file: %v", err)
	}
	raw, err := EncodeSelfRestartHandoff(SelfRestartHandoff{
		Version: 1, HandoffID: "handoff-f", Generation: "generation-f", Sequence: 1,
		OldPort: 9125, TargetPort: 19125, ParentPID: os.Getppid(), NoncePath: noncePath,
	})
	if err != nil {
		t.Fatalf("EncodeSelfRestartHandoff: %v", err)
	}

	if _, err := NewSpawnedGUIChildFromEnvironment(raw, 4242, stateDir); err == nil {
		t.Fatal("malformed nonce unexpectedly produced a spawned child")
	}
	if _, err := os.Stat(noncePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("malformed one-shot nonce survived failed consumption: %v", err)
	}
}

func TestRestartV3_RejectsNonceOutsideCanonicalStateDirBeforeConsume(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	foreignDir := apitest.HardenedTempDir(t)
	foreignNoncePath := filepath.Join(foreignDir, "gui-restart-nonce")
	if err := api.WriteStateFileBytesAtomic(foreignNoncePath, bytes.Repeat([]byte{0x65}, 32)); err != nil {
		t.Fatalf("write foreign nonce file: %v", err)
	}
	raw, err := EncodeSelfRestartHandoff(SelfRestartHandoff{
		Version: 1, HandoffID: "handoff-f", Generation: "generation-f", Sequence: 1,
		OldPort: 9125, TargetPort: 19125, ParentPID: os.Getppid(), NoncePath: foreignNoncePath,
	})
	if err != nil {
		t.Fatalf("EncodeSelfRestartHandoff: %v", err)
	}

	if _, err := NewSpawnedGUIChildFromEnvironment(raw, 4242, stateDir); err == nil || !strings.Contains(err.Error(), "canonical state directory") {
		t.Fatalf("foreign nonce path error = %v, want canonical-state-directory refusal", err)
	}
	if _, err := os.Stat(foreignNoncePath); err != nil {
		t.Fatalf("foreign nonce was mutated before path authorization: %v", err)
	}
}

func TestRestartV3_SpawnedChildAcquirePassesDesignatedNonce(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	deadlines := DefaultRestartDeadlines()
	deadlines.Now = func() time.Time { return now }
	stateDir := apitest.HardenedTempDir(t)
	nonce := bytes.Repeat([]byte{0x4d}, 32)
	noncePath := filepath.Join(stateDir, "gui-restart-nonce")
	if err := api.WriteStateFileBytesAtomic(noncePath, nonce); err != nil {
		t.Fatalf("write hardened nonce file: %v", err)
	}
	store := NewHandoffMarkerStore(stateDir, deadlines)
	started, err := store.Begin(HandoffBegin{
		Generation: "generation-f", Route: HandoffRoutePortChange, OldPort: 9125, NewPort: 19125, OldPID: 1111,
	})
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := store.Reserve(started.Generation, started.Sequence, now.Add(deadlines.Reservation), hashDesignatedChildNonce(nonce), os.Getpid()); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	raw, err := EncodeSelfRestartHandoff(SelfRestartHandoff{
		Version: 1, HandoffID: "handoff-f", Generation: started.Generation, Sequence: started.Sequence,
		OldPort: 9125, TargetPort: 19125, ParentPID: 1111, NoncePath: noncePath,
	})
	if err != nil {
		t.Fatalf("EncodeSelfRestartHandoff: %v", err)
	}
	child, err := NewSpawnedGUIChildFromEnvironment(raw, os.Getpid(), stateDir)
	if err != nil {
		t.Fatalf("NewSpawnedGUIChildFromEnvironment: %v", err)
	}
	t.Cleanup(child.Close)

	lease, err := child.AcquireSingleInstanceAt(filepath.Join(stateDir, "gui.pidport"), 19125, store, deadlines)
	if err != nil {
		t.Fatalf("designated child acquire: %v", err)
	}
	lease.Release()
}

func TestRestartV3_ChildStandbyHasNoMutableSideEffectsBeforeFlockAcquisition(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	child := phaseFChild(t.TempDir())
	marker := &phaseFMarkerStore{record: phaseFReservedRecord(now)}
	lease := &phaseFLease{}
	standby := &phaseFStandby{}
	events := &phaseFEvents{}
	deps := phaseFDependencies(&now, marker, lease, standby, events)

	acquireCalls := 0
	mutable := struct{ hub, tray, browser, poller, mutator int }{}
	deps.Acquire = func(context.Context) (SingleInstanceLease, error) {
		acquireCalls++
		if mutable.hub+mutable.tray+mutable.browser+mutable.poller+mutable.mutator != 0 {
			t.Fatal("mutable runtime work occurred before flock acquisition")
		}
		if acquireCalls == 1 {
			return nil, ErrSingleInstanceBusy
		}
		return lease, nil
	}
	deps.Activate = func(context.Context, SingleInstanceLease) error {
		mutable.hub++
		mutable.tray++
		mutable.browser++
		mutable.poller++
		mutable.mutator++
		return nil
	}

	result, err := child.Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Activated || result.CommitErr != nil || acquireCalls != 2 {
		t.Fatalf("result=%+v acquireCalls=%d", result, acquireCalls)
	}
	if lease.releaseCount() != 0 {
		t.Fatal("activated runtime lease was released by the child protocol")
	}
	lease.Release()
}

func TestRestartV3_ChildActivatesImmediatelyOnFlockAcquisition(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	child := phaseFChild(t.TempDir())
	marker := &phaseFMarkerStore{record: phaseFReservedRecord(now)}
	lease := &phaseFLease{}
	standby := &phaseFStandby{}
	events := &phaseFEvents{}
	deps := phaseFDependencies(&now, marker, lease, standby, events)
	order := []string{}
	deps.Acquire = func(context.Context) (SingleInstanceLease, error) {
		order = append(order, "flock")
		return lease, nil
	}
	deps.Activate = func(context.Context, SingleInstanceLease) error {
		order = append(order, "activate")
		return nil
	}

	result, err := child.Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Activated {
		t.Fatal("child did not report activation")
	}
	if got, want := strings.Join(order, ","), "flock,activate"; got != want {
		t.Fatalf("activation order = %q, want %q (no parent signal/hub wait)", got, want)
	}
	if got, want := strings.Join(events.types(), ","), "gui-restart-lock-acquired,gui-restart-progress"; got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
	published := events.snapshot()
	if got := published[0].Body["reason_code"]; got != "gui-restart-lock-acquired" {
		t.Fatalf("lock-acquired reason_code = %v", got)
	}
	if got := published[1].Body["reason_code"]; got != "committed" {
		t.Fatalf("committed reason_code = %v", got)
	}
	lease.Release()
}

func TestRestartV3_CommitRetriesThenPublishesCommitted(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	marker := &phaseFMarkerStore{
		record:     phaseFReservedRecord(now),
		commitErrs: []error{errors.New("write one failed"), errors.New("write two failed"), nil},
	}
	lease := &phaseFLease{}
	events := &phaseFEvents{}
	deps := phaseFDependencies(&now, marker, lease, &phaseFStandby{}, events)
	deps.Activate = func(context.Context, SingleInstanceLease) error { return nil }

	result, err := phaseFChild(t.TempDir()).Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	record, calls, _ := marker.snapshot()
	if result.CommitErr != nil || calls != 3 || record.Phase != HandoffPhaseCommitted {
		t.Fatalf("result=%+v calls=%d record=%+v", result, calls, record)
	}
	if got := strings.Join(events.types(), ","); got != "gui-restart-lock-acquired,gui-restart-progress" {
		t.Fatalf("events = %q", got)
	}
	lease.Release()
}

func TestRestartV3_CommitFailureIsBoundedAndPublishesFailure(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	record := phaseFReservedRecord(now)
	record.ReservationExpiresAt = now.Add(25 * time.Millisecond)
	marker := &phaseFMarkerStore{record: record, commitErrs: []error{
		errors.New("write one failed"), errors.New("write two failed"), errors.New("write three failed"), errors.New("write four failed"),
	}}
	lease := &phaseFLease{}
	events := &phaseFEvents{}
	deps := phaseFDependencies(&now, marker, lease, &phaseFStandby{}, events)
	deps.Activate = func(context.Context, SingleInstanceLease) error { return nil }

	result, err := phaseFChild(t.TempDir()).Run(context.Background(), deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	terminal, calls, interruptCalls := marker.snapshot()
	if result.CommitErr == nil || calls != 4 {
		t.Fatalf("bounded commit result=%+v calls=%d, want terminal error after 4 attempts", result, calls)
	}
	if interruptCalls != 1 || terminal.Phase != HandoffPhaseInterrupted || terminal.ReasonCode != "gui-restart-commit-write-failed" {
		t.Fatalf("terminal settlement calls=%d record=%+v, want one interrupted commit-failure marker", interruptCalls, terminal)
	}
	if terminal.Phase.nonterminal() {
		t.Fatalf("commit-failure terminal phase %q remains recovery-eligible", terminal.Phase)
	}
	if got := strings.Join(events.types(), ","); got != "gui-restart-lock-acquired,gui-restart-commit-write-failed" {
		t.Fatalf("events = %q", got)
	}
	published := events.snapshot()
	if got := published[1].Body["reason_code"]; got != "gui-restart-commit-write-failed" {
		t.Fatalf("commit failure reason_code = %v", got)
	}
	if lease.releaseCount() != 0 {
		t.Fatal("healthy activated child released its lease after Commit exhaustion")
	}
	lease.Release()
}

func TestRestartV3_CommitDoesNotRunAfterRuntimeStops(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	marker := &phaseFMarkerStore{record: phaseFReservedRecord(now)}
	deps := phaseFDependencies(&now, marker, &phaseFLease{}, &phaseFStandby{}, &phaseFEvents{})
	deps.Runtime.Stop(errors.New("runtime stopped"))
	deps.Activate = func(context.Context, SingleInstanceLease) error { return nil }

	_, err := phaseFChild(t.TempDir()).Run(context.Background(), deps)
	if err == nil || !strings.Contains(err.Error(), "runtime stopped") {
		t.Fatalf("Run error = %v, want runtime-stop error", err)
	}
	_, commitCalls, interruptCalls := marker.snapshot()
	if commitCalls != 0 || interruptCalls != 0 {
		t.Fatalf("marker writes after runtime stop: commit=%d interrupt=%d, want 0/0", commitCalls, interruptCalls)
	}
}

func TestRestartV3_CommitDoesNotRunAfterContextCancellation(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	marker := &phaseFMarkerStore{record: phaseFReservedRecord(now)}
	deps := phaseFDependencies(&now, marker, &phaseFLease{}, &phaseFStandby{}, &phaseFEvents{})
	ctx, cancel := context.WithCancel(context.Background())
	deps.Activate = func(context.Context, SingleInstanceLease) error {
		cancel()
		return nil
	}

	_, err := phaseFChild(t.TempDir()).Run(ctx, deps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	_, commitCalls, interruptCalls := marker.snapshot()
	if commitCalls != 0 || interruptCalls != 0 {
		t.Fatalf("marker writes after context cancellation: commit=%d interrupt=%d, want 0/0", commitCalls, interruptCalls)
	}
}

func TestRestartV3_CommitPublicationSerializesWithRuntimeStop(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	commitStarted := make(chan struct{})
	releaseCommit := make(chan struct{})
	marker := &phaseFMarkerStore{
		record: phaseFReservedRecord(now),
		commitHook: func() {
			close(commitStarted)
			<-releaseCommit
		},
	}
	deps := phaseFDependencies(&now, marker, &phaseFLease{}, &phaseFStandby{}, &phaseFEvents{})
	deps.Activate = func(context.Context, SingleInstanceLease) error { return nil }

	runDone := make(chan error, 1)
	go func() {
		_, err := phaseFChild(t.TempDir()).Run(context.Background(), deps)
		runDone <- err
	}()
	select {
	case <-commitStarted:
	case <-time.After(time.Second):
		t.Fatal("Commit did not start")
	}
	stopDone := make(chan struct{})
	go func() {
		deps.Runtime.Stop(errors.New("runtime stopped"))
		close(stopDone)
	}()
	select {
	case <-stopDone:
		t.Fatal("runtime stop publication raced past an in-flight Commit")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseCommit)
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("runtime stop did not settle after Commit completed")
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not complete")
	}
}

func TestRestartV3_CommitStaleGenerationStopsWithoutTerminalOverwrite(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	marker := &phaseFMarkerStore{
		record:     phaseFReservedRecord(now),
		commitErrs: []error{ErrHandoffMarkerCASMismatch},
	}
	deps := phaseFDependencies(&now, marker, &phaseFLease{}, &phaseFStandby{}, &phaseFEvents{})
	deps.Activate = func(context.Context, SingleInstanceLease) error { return nil }

	result, err := phaseFChild(t.TempDir()).Run(context.Background(), deps)
	if !errors.Is(err, ErrHandoffMarkerCASMismatch) || !errors.Is(result.CommitErr, ErrHandoffMarkerCASMismatch) {
		t.Fatalf("Run result=%+v error=%v, want stale-generation CAS failure", result, err)
	}
	_, commitCalls, interruptCalls := marker.snapshot()
	if commitCalls != 1 || interruptCalls != 0 {
		t.Fatalf("stale generation writes: commit=%d interrupt=%d, want 1/0", commitCalls, interruptCalls)
	}
}

func TestRestartV3_CommitFailureTerminalizationFailureStopsRuntime(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	record := phaseFReservedRecord(now)
	record.ReservationExpiresAt = now.Add(time.Millisecond)
	marker := &phaseFMarkerStore{
		record:       record,
		commitErrs:   []error{errors.New("commit failed"), errors.New("commit failed again")},
		interruptErr: errors.New("terminal write failed"),
	}
	deps := phaseFDependencies(&now, marker, &phaseFLease{}, &phaseFStandby{}, &phaseFEvents{})
	deps.Activate = func(context.Context, SingleInstanceLease) error { return nil }

	result, err := phaseFChild(t.TempDir()).Run(context.Background(), deps)
	if err == nil || !strings.Contains(err.Error(), "terminal write failed") {
		t.Fatalf("Run result=%+v error=%v, want fail-closed terminal-write error", result, err)
	}
	terminal, _, interruptCalls := marker.snapshot()
	if interruptCalls != 1 || terminal.Phase != HandoffPhaseReserved {
		t.Fatalf("terminalization failure calls=%d record=%+v, want one failed attempt retaining reserved", interruptCalls, terminal)
	}
}

func TestRestartV3_ParentDeathWithoutReservationClosesStandbyAndExits(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	marker := &phaseFMarkerStore{record: phaseFReservedRecord(now)}
	marker.record.Phase = HandoffPhaseInProgress
	marker.record.ReservationExpiresAt = time.Time{}
	marker.record.DesignatedChildHash = ""
	lease := &phaseFLease{}
	standby := &phaseFStandby{}
	deps := phaseFDependencies(&now, marker, lease, standby, &phaseFEvents{})
	deps.Acquire = func(context.Context) (SingleInstanceLease, error) {
		// Mirrors Phase E: a free tentative lease without matching reserved is
		// released before ErrHandoffReserved reaches the child protocol.
		lease.Release()
		return nil, ErrHandoffReserved
	}
	deps.Activate = func(context.Context, SingleInstanceLease) error {
		t.Fatal("child activated without a matching reservation")
		return nil
	}

	_, err := phaseFChild(t.TempDir()).Run(context.Background(), deps)
	if !errors.Is(err, ErrHandoffReserved) {
		t.Fatalf("Run error = %v, want ErrHandoffReserved", err)
	}
	if lease.releaseCount() != 1 || standby.closeCount() != 1 {
		t.Fatalf("tentative release=%d standby close=%d, want 1/1", lease.releaseCount(), standby.closeCount())
	}
}

func TestRestartV3_ExactParentDeathSignalClosesStandbyImmediately(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	child := phaseFChild(t.TempDir())
	parentDeath := newPhaseFParentDeathWatcher()
	child.parentDeath = parentDeath
	t.Cleanup(child.Close)
	standby := &phaseFStandby{}
	deps := phaseFDependencies(&now, &phaseFMarkerStore{}, &phaseFLease{}, standby, &phaseFEvents{})
	acquireStarted := make(chan struct{})
	var acquireOnce sync.Once
	deps.Acquire = func(context.Context) (SingleInstanceLease, error) {
		acquireOnce.Do(func() { close(acquireStarted) })
		return nil, ErrSingleInstanceBusy
	}
	deps.Wait = waitRestartChild
	deps.RetryInterval = time.Minute
	deps.Activate = func(context.Context, SingleInstanceLease) error {
		t.Fatal("child activated after exact parent death")
		return nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := child.Run(context.Background(), deps)
		done <- err
	}()
	select {
	case <-acquireStarted:
	case <-time.After(time.Second):
		t.Fatal("standby acquire did not start")
	}
	parentDeath.SignalDeath()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRestartChildParentExited) {
			t.Fatalf("Run error = %v, want ErrRestartChildParentExited", err)
		}
	case <-time.After(time.Second):
		t.Fatal("exact parent death did not stop standby immediately")
	}
	if standby.closeCount() != 1 {
		t.Fatalf("standby close count = %d, want 1", standby.closeCount())
	}
}

func TestRestartV3_StandbyDeadlineExpiryClosesAndExits(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	standby := &phaseFStandby{}
	deps := phaseFDependencies(&now, &phaseFMarkerStore{}, &phaseFLease{}, standby, &phaseFEvents{})
	deps.Deadlines.Proof = 25 * time.Millisecond
	acquireCalls := 0
	deps.Acquire = func(context.Context) (SingleInstanceLease, error) {
		acquireCalls++
		return nil, ErrSingleInstanceBusy
	}
	deps.Activate = func(context.Context, SingleInstanceLease) error {
		t.Fatal("child activated after standby deadline")
		return nil
	}

	_, err := phaseFChild(t.TempDir()).Run(context.Background(), deps)
	if !errors.Is(err, ErrRestartChildStandbyExpired) {
		t.Fatalf("Run error = %v, want ErrRestartChildStandbyExpired", err)
	}
	if acquireCalls != 4 || standby.closeCount() != 1 {
		t.Fatalf("acquire calls=%d standby close=%d, want 4/1", acquireCalls, standby.closeCount())
	}
}

func TestRestartV3_StandbyCloseUsesOwnBudgetNotBindDeadline(t *testing.T) {
	standby := &phaseFStandby{}
	closeRestartChildStandby(RestartChildDependencies{
		Standby: standby,
		Deadlines: RestartDeadlines{
			Bind: 37 * time.Second,
		},
	})
	if got := standby.closeBudget(); got < 1500*time.Millisecond || got > 2500*time.Millisecond {
		t.Fatalf("standby close budget = %v, want dedicated 2s budget independent of 37s bind deadline", got)
	}
}

func TestGUIReadHeaderTimeoutIsSingleTenSecondOwner(t *testing.T) {
	if GUIReadHeaderTimeout != 10*time.Second {
		t.Fatalf("GUIReadHeaderTimeout = %v, want 10s", GUIReadHeaderTimeout)
	}
	normal := NewServer(Config{})
	restart := NewGUIListenerOwner(GUIReadHeaderTimeout)
	if got := normal.guiListener.server.ReadHeaderTimeout; got != GUIReadHeaderTimeout {
		t.Fatalf("normal Start owner ReadHeaderTimeout = %v, want %v", got, GUIReadHeaderTimeout)
	}
	if got := restart.server.ReadHeaderTimeout; got != GUIReadHeaderTimeout {
		t.Fatalf("restart child owner ReadHeaderTimeout = %v, want %v", got, GUIReadHeaderTimeout)
	}
}

func TestRestartV3_LateStandbyChildRejectsChangedMarkerAndReleases(t *testing.T) {
	now := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	marker := &phaseFMarkerStore{record: phaseFReservedRecord(now)}
	lease := &phaseFLease{}
	standby := &phaseFStandby{}
	deps := phaseFDependencies(&now, marker, lease, standby, &phaseFEvents{})
	activated := 0
	deps.Acquire = func(context.Context) (SingleInstanceLease, error) {
		changed := phaseFReservedRecord(now)
		changed.Phase = HandoffPhaseInterrupted
		changed.DesignatedChildHash = ""
		changed.ReservationExpiresAt = time.Time{}
		changed.ReasonCode = "expired-free"
		changed.OperatorAction = "mcphub gui"
		marker.setRecord(changed)
		return lease, nil
	}
	deps.Activate = func(context.Context, SingleInstanceLease) error {
		activated++
		return nil
	}

	_, err := phaseFChild(t.TempDir()).Run(context.Background(), deps)
	if !errors.Is(err, ErrRestartChildMarkerMismatch) {
		t.Fatalf("Run error = %v, want ErrRestartChildMarkerMismatch", err)
	}
	if lease.releaseCount() != 1 || standby.closeCount() != 1 || activated != 0 {
		t.Fatalf("release=%d standbyClose=%d activated=%d, want 1/1/0", lease.releaseCount(), standby.closeCount(), activated)
	}
}

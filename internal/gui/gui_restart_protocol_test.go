package gui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

type phaseGOrder struct {
	mu    sync.Mutex
	steps []string
}

func phaseGNow(now *time.Time) func() time.Time {
	return func() time.Time { return *now }
}

func phaseGDeadlines(now *time.Time) RestartDeadlines {
	return RestartDeadlines{
		Now: phaseGNow(now), RecordLock: time.Second, Freshness: 3 * time.Minute,
		Reservation: 10 * time.Second, Proof: 10 * time.Second, Bind: 2 * time.Second,
		Quiesce: 5 * time.Second, Rollback: 5 * time.Second, Grace: 5 * time.Second,
	}
}

func (o *phaseGOrder) add(step string) {
	o.mu.Lock()
	o.steps = append(o.steps, step)
	o.mu.Unlock()
}

func (o *phaseGOrder) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.steps...)
}

type phaseGLease struct {
	order      *phaseGOrder
	releaseCnt int
	reacquires int
	onRelease  func()
}

func (l *phaseGLease) Release() {
	l.releaseCnt++
	l.order.add("flock-release")
	if l.onRelease != nil {
		l.onRelease()
	}
}

type phaseGChildHandle struct {
	order        *phaseGOrder
	pid          int
	terminated   int
	detached     int
	terminateErr error
	onTerminate  func()
}

func (c *phaseGChildHandle) PID() int { return c.pid }

func (c *phaseGChildHandle) TerminateBeforeRelease(context.Context) error {
	c.terminated++
	c.order.add("child-terminate")
	if c.onTerminate != nil {
		c.onTerminate()
	}
	return c.terminateErr
}

func (c *phaseGChildHandle) DetachAtRelease() error {
	c.detached++
	c.order.add("child-detach")
	return nil
}

type phaseGListener struct {
	order           *phaseGOrder
	enterGraceErr   error
	closeErr        error
	bindErr         error
	restoreErr      error
	rebound         net.Listener
	servedFull      int
	restoredFull    int
	bindForRecovery func(context.Context, int) (net.Listener, error)
}

func (l *phaseGListener) EnterGrace(context.Context, http.Handler) error {
	l.order.add("enter-grace")
	return l.enterGraceErr
}

func (l *phaseGListener) CloseListener(context.Context) error {
	l.order.add("close-gui-listener")
	return l.closeErr
}

func (l *phaseGListener) BindForRecovery(ctx context.Context, port int) (net.Listener, error) {
	l.order.add("bind-for-recovery")
	if l.bindForRecovery != nil {
		return l.bindForRecovery(ctx, port)
	}
	if l.bindErr != nil {
		return nil, l.bindErr
	}
	return l.rebound, nil
}

func (l *phaseGListener) ServeFull(net.Listener, http.Handler) error {
	l.servedFull++
	l.order.add("serve-full")
	return nil
}

func (l *phaseGListener) RestoreFull(http.Handler) error {
	l.restoredFull++
	l.order.add("restore-full")
	return l.restoreErr
}

type phaseGMarkerStore struct {
	record       *HandoffMarkerRecord
	reserveErr   error
	interruptErr error
	clearErr     error
	clears       int
	interrupts   int
	onReserve    func()
}

func (s *phaseGMarkerStore) Begin(begin HandoffBegin) (*HandoffMarkerRecord, error) {
	s.record = &HandoffMarkerRecord{
		Generation: begin.Generation,
		Phase:      HandoffPhaseInProgress,
		Route:      begin.Route,
		OldPort:    begin.OldPort,
		NewPort:    begin.NewPort,
		OldPID:     begin.OldPID,
		Sequence:   1,
	}
	copy := *s.record
	if s.onReserve != nil {
		s.onReserve()
	}
	return &copy, nil
}

type phaseGPhysicalCloseWaitError struct {
	err error
}

func (e phaseGPhysicalCloseWaitError) Error() string                { return e.err.Error() }
func (e phaseGPhysicalCloseWaitError) Unwrap() error                { return e.err }
func (phaseGPhysicalCloseWaitError) ListenerPhysicallyClosed() bool { return true }

func (s *phaseGMarkerStore) Reserve(generation string, expectedSequence uint64, _ time.Time, _ string, childPID int) (*HandoffMarkerRecord, error) {
	if s.reserveErr != nil {
		return nil, s.reserveErr
	}
	if s.record == nil || s.record.Generation != generation || s.record.Sequence != expectedSequence {
		return nil, ErrHandoffMarkerStateMismatch
	}
	s.record.Phase = HandoffPhaseReserved
	s.record.ChildPID = childPID
	s.record.Sequence++
	copy := *s.record
	return &copy, nil
}

func (s *phaseGMarkerStore) Interrupt(generation, reasonCode, operatorAction string) (*HandoffMarkerRecord, error) {
	s.interrupts++
	if s.interruptErr != nil {
		return nil, s.interruptErr
	}
	if s.record == nil || s.record.Generation != generation {
		return nil, ErrHandoffMarkerStateMismatch
	}
	s.record.Phase = HandoffPhaseInterrupted
	s.record.ReasonCode = reasonCode
	s.record.OperatorAction = operatorAction
	s.record.Sequence++
	copy := *s.record
	return &copy, nil
}

func (s *phaseGMarkerStore) ClearAfterProvedPreReleaseRollback(generation string) error {
	if s.clearErr != nil {
		return s.clearErr
	}
	if s.record == nil || s.record.Generation != generation || s.record.Phase != HandoffPhaseInProgress {
		return ErrHandoffMarkerStateMismatch
	}
	s.clears++
	s.record = nil
	return nil
}

type phaseGNetListener struct{}

func (phaseGNetListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (phaseGNetListener) Close() error              { return nil }
func (phaseGNetListener) Addr() net.Addr            { return phaseGNetAddr{} }

type phaseGNetAddr struct{}

func (phaseGNetAddr) Network() string { return "tcp" }
func (phaseGNetAddr) String() string  { return "127.0.0.1:9125" }

func TestRestartV3_HubLifecycleBarrierJoinsPublishersBeforeRelease(t *testing.T) {
	t.Run("initializer", func(t *testing.T) {
		s := NewServer(Config{Port: 9125})
		s.hubEndpointGateFn = func(*api.API) bool { return true }
		producerStarted := make(chan struct{})
		allowProducer := make(chan struct{})
		producerStopped := make(chan struct{})
		var stopOnce sync.Once
		comp := &HubListenerComponents{listenerCancel: func() { stopOnce.Do(func() { close(producerStopped) }) }}
		comp.alive.Store(true)
		s.startHubMcpListenerFn = func(context.Context, bool, *api.API, startHubMcpListenerOptions) (*HubListenerComponents, error) {
			close(producerStarted)
			<-allowProducer // Deliberately ignore cancellation to engineer the join window.
			return comp, nil
		}

		serverCtx, stopServer := context.WithCancel(context.Background())
		serverDone := make(chan error, 1)
		go func() { serverDone <- s.runActivatedGUIListener(serverCtx, make(chan error)) }()
		select {
		case <-producerStarted:
		case <-time.After(time.Second):
			t.Fatal("hub initializer did not start")
		}

		closeDone := make(chan struct{})
		go func() {
			s.closeOwnHubListenerForRestart(context.Background())
			close(closeDone)
		}()
		returnedBeforeProducerSettled := false
		select {
		case <-closeDone:
			returnedBeforeProducerSettled = true
		case <-time.After(100 * time.Millisecond):
		}
		close(allowProducer)
		select {
		case <-closeDone:
		case <-time.After(time.Second):
			t.Fatal("restart hub close did not join the initializer")
		}

		deadline := time.After(time.Second)
		for s.hubMcpComp.Load() == nil {
			select {
			case <-producerStopped:
				goto initializerSettled
			case <-deadline:
				goto initializerSettled
			default:
				time.Sleep(time.Millisecond)
			}
		}
	initializerSettled:
		lateComp := s.hubMcpComp.Load()
		stopServer()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Fatalf("runActivatedGUIListener shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("runActivatedGUIListener did not stop")
		}
		if returnedBeforeProducerSettled {
			t.Fatal("restart hub close returned before the initializer settled")
		}
		if lateComp != nil {
			t.Fatalf("initializer published live component %p after restart hub close", lateComp)
		}
		select {
		case <-producerStopped:
		default:
			t.Fatal("initializer component was not shut down by the lifecycle owner")
		}
	})

	t.Run("restart-driver", func(t *testing.T) {
		s := NewServer(Config{Port: 9125})
		s.hubEndpointGateFn = func(*api.API) bool { return true }
		restartStarted := make(chan struct{})
		allowRestart := make(chan struct{})
		replacementStopped := make(chan struct{})
		var replacementStopOnce sync.Once
		old := &HubListenerComponents{port: 19125}
		old.alive.Store(true)
		replacement := &HubListenerComponents{
			port: 19125,
			listenerCancel: func() {
				replacementStopOnce.Do(func() { close(replacementStopped) })
			},
		}
		replacement.alive.Store(true)
		var startMu sync.Mutex
		startCalls := 0
		s.startHubMcpListenerFn = func(context.Context, bool, *api.API, startHubMcpListenerOptions) (*HubListenerComponents, error) {
			startMu.Lock()
			startCalls++
			call := startCalls
			startMu.Unlock()
			if call == 1 {
				return old, nil
			}
			close(restartStarted)
			<-allowRestart // Deliberately ignore cancellation to engineer the join window.
			return replacement, nil
		}

		serverCtx, stopServer := context.WithCancel(context.Background())
		serverDone := make(chan error, 1)
		go func() { serverDone <- s.runActivatedGUIListener(serverCtx, make(chan error)) }()
		deadline := time.Now().Add(time.Second)
		for s.hubMcpComp.Load() != old && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if s.hubMcpComp.Load() != old {
			stopServer()
			t.Fatal("initial hub component was not published")
		}
		s.enqueueHubListenerRestart(hubListenerRestartRequest{cause: hubListenerRestartCauseUnresponsive})
		select {
		case <-restartStarted:
		case <-time.After(time.Second):
			stopServer()
			t.Fatal("hub restart driver did not enter replacement start")
		}

		closeDone := make(chan struct{})
		go func() {
			s.closeOwnHubListenerForRestart(context.Background())
			close(closeDone)
		}()
		returnedBeforeProducerSettled := false
		select {
		case <-closeDone:
			returnedBeforeProducerSettled = true
		case <-time.After(100 * time.Millisecond):
		}
		close(allowRestart)
		select {
		case <-closeDone:
		case <-time.After(time.Second):
			t.Fatal("restart hub close did not join the restart driver")
		}

		deadline = time.Now().Add(time.Second)
		for s.hubMcpComp.Load() == nil && time.Now().Before(deadline) {
			select {
			case <-replacementStopped:
				goto restartDriverSettled
			default:
				time.Sleep(time.Millisecond)
			}
		}
	restartDriverSettled:
		lateComp := s.hubMcpComp.Load()
		stopServer()
		select {
		case err := <-serverDone:
			if err != nil {
				t.Fatalf("runActivatedGUIListener shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("runActivatedGUIListener did not stop")
		}
		if returnedBeforeProducerSettled {
			t.Fatal("restart hub close returned before the restart driver settled")
		}
		if lateComp != nil {
			t.Fatalf("restart driver published live component %p after restart hub close", lateComp)
		}
		select {
		case <-replacementStopped:
		default:
			t.Fatal("replacement component was not shut down by the lifecycle owner")
		}
	})
}

func TestRestartV3_HubProducerShutdownBoundedWhenProducerDoesNotReturn(t *testing.T) {
	barrier := &hubProducerShutdownBarrier{}
	producerCtx, began := barrier.begin(context.Background())
	if !began {
		t.Fatal("hub producer barrier did not begin")
	}
	producerStarted := make(chan struct{})
	allowProducerReturn := make(chan struct{})
	if !barrier.launch(func() {
		close(producerStarted)
		<-allowProducerReturn
		_ = producerCtx.Err()
	}) {
		t.Fatal("hub producer was not admitted")
	}
	<-producerStarted

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	shutdownDone := make(chan struct{})
	var slot atomic.Pointer[HubListenerComponents]
	go func() {
		barrier.shutdown(shutdownCtx, &slot)
		close(shutdownDone)
	}()

	returnedWithinBound := false
	select {
	case <-shutdownDone:
		returnedWithinBound = true
	case <-time.After(250 * time.Millisecond):
	}
	close(allowProducerReturn)
	if !returnedWithinBound {
		<-shutdownDone
		t.Fatal("hub producer shutdown ignored its bound while a producer was wedged")
	}
}

func TestRestartV3_ConfirmRetriesConnectionRefusedUntilChildBinds(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	order := &phaseGOrder{}
	lease := &phaseGLease{order: order}
	child := &phaseGChildHandle{order: order, pid: 4241}
	marker := &phaseGMarkerStore{}
	listener := &phaseGListener{order: order}
	ids := []string{"handoff-confirm-retry", "generation-confirm-retry"}
	attempts := 0
	deadlines := phaseGDeadlines(&now)
	deadlines.Bind = 500 * time.Millisecond
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: apitest.HardenedTempDir(t),
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 19125, nil }, ParentPID: 1111,
		Lease: lease, Listener: listener, FullHandler: http.NotFoundHandler(), MarkerStore: marker,
		Deadlines: deadlines,
		NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:  func() ([]byte, error) { return bytes.Repeat([]byte{0x70}, 32), nil },
		Spawn:     func(SelfRestartHandoff) (RestartParentChild, error) { return child, nil },
		Confirm: func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error {
			attempts++
			if attempts <= 3 {
				return fmt.Errorf("probe restart standby: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})
			}
			return nil
		},
		CloseHub:  func(context.Context) {},
		WaitGrace: func(context.Context, time.Duration) error { return nil },
		Exit:      func() {},
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}

	started, err := coordinator.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	started.AcknowledgeResponseFlushed()
	result := <-started.Done
	if result.Err != nil || !result.ParentLeaseReleased {
		t.Fatalf("result = %+v, want confirmed handoff without rollback", result)
	}
	if attempts != 4 {
		t.Fatalf("confirm attempts = %d, want 4 after three transient connection refusals", attempts)
	}
	if lease.releaseCnt != 1 || child.terminated != 0 || child.detached != 1 {
		t.Fatalf("release=%d terminate=%d detach=%d, want 1/0/1", lease.releaseCnt, child.terminated, child.detached)
	}
}

func TestRestartV3_SamePort_EnterGraceFailureRestoresLiveListenerWithoutRebind(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	order := &phaseGOrder{}
	lease := &phaseGLease{order: order}
	child := &phaseGChildHandle{order: order, pid: 4240}
	listener := &phaseGListener{order: order, enterGraceErr: errors.New("grace admission failed")}
	marker := &phaseGMarkerStore{}
	ids := []string{"handoff-live-listener-rollback", "generation-live-listener-rollback"}
	confirmCalls := 0
	exits := 0
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: apitest.HardenedTempDir(t),
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 9125, nil }, ParentPID: 1111,
		Lease: lease, Listener: listener, FullHandler: http.NotFoundHandler(), MarkerStore: marker,
		Deadlines: phaseGDeadlines(&now),
		NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:  func() ([]byte, error) { return bytes.Repeat([]byte{0x6f}, 32), nil },
		Spawn:     func(SelfRestartHandoff) (RestartParentChild, error) { order.add("spawn"); return child, nil },
		Confirm: func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error {
			confirmCalls++
			return nil
		},
		CloseHub: func(context.Context) {},
		Exit:     func() { exits++ },
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}

	started, err := coordinator.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	started.AcknowledgeResponseFlushed()
	result := <-started.Done
	if result.Err == nil || result.ParentLeaseReleased {
		t.Fatalf("result = %+v, want proved pre-release rollback with retained lease", result)
	}
	if listener.restoredFull != 1 || listener.servedFull != 0 {
		t.Fatalf("restore-full=%d serve-full=%d, want 1/0 for retained listener", listener.restoredFull, listener.servedFull)
	}
	if got := order.snapshot(); strings.Contains(","+strings.Join(got, ",")+",", ",bind-for-recovery,") {
		t.Fatalf("rollback order = %v, BindForRecovery must not run while original listener is live", got)
	}
	if lease.releaseCnt != 0 || lease.reacquires != 0 || exits != 0 || confirmCalls != 0 {
		t.Fatalf("release=%d reacquire=%d exits=%d confirm=%d, want all zero", lease.releaseCnt, lease.reacquires, exits, confirmCalls)
	}
	if marker.record != nil || marker.clears != 1 || child.terminated != 1 || child.detached != 0 {
		t.Fatalf("marker=%+v clears=%d terminate=%d detach=%d, want nil/1/1/0", marker.record, marker.clears, child.terminated, child.detached)
	}
}

func TestRestartV3_SamePort_CloseTimeoutAfterPhysicalCloseRebindsForRollback(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	order := &phaseGOrder{}
	lease := &phaseGLease{order: order}
	child := &phaseGChildHandle{order: order, pid: 4246}
	listener := &phaseGListener{
		order:    order,
		closeErr: phaseGPhysicalCloseWaitError{err: context.DeadlineExceeded},
		rebound:  phaseGNetListener{},
	}
	marker := &phaseGMarkerStore{}
	ids := []string{"handoff-close-timeout", "generation-close-timeout"}
	exits := 0
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: apitest.HardenedTempDir(t),
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 9125, nil }, ParentPID: 1111,
		Lease: lease, Listener: listener, FullHandler: http.NotFoundHandler(), MarkerStore: marker,
		Deadlines: phaseGDeadlines(&now),
		NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:  func() ([]byte, error) { return bytes.Repeat([]byte{0x71}, 32), nil },
		Spawn:     func(SelfRestartHandoff) (RestartParentChild, error) { return child, nil },
		Confirm: func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error {
			return errors.New("confirm must not run after close timeout")
		},
		CloseHub: func(context.Context) {},
		Exit:     func() { exits++ },
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}

	started, err := coordinator.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result := <-started.Done
	if result.Err == nil || result.ParentLeaseReleased {
		t.Fatalf("result = %+v, want proved rollback after physical close timeout", result)
	}
	if listener.servedFull != 1 || listener.restoredFull != 0 {
		t.Fatalf("serve-full=%d restore-full=%d, want recovery rebind 1/0", listener.servedFull, listener.restoredFull)
	}
	if child.terminated != 1 || lease.releaseCnt != 0 || exits != 0 || marker.record != nil || marker.clears != 1 {
		t.Fatalf("terminate=%d release=%d exits=%d marker=%+v clears=%d", child.terminated, lease.releaseCnt, exits, marker.record, marker.clears)
	}
}

func TestRestartV3_NoncePathIsGenerationBoundAndStaleGenerationCannotConsumeNext(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	stateDir := apitest.HardenedTempDir(t)
	order := &phaseGOrder{}
	ids := []string{"handoff-a", "generation-a", "handoff-b", "generation-b"}
	marker := &phaseGMarkerStore{}
	var handoffs []SelfRestartHandoff
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: stateDir,
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 19125, nil }, ParentPID: os.Getpid(),
		Lease: &phaseGLease{order: order}, Listener: &phaseGListener{order: order}, FullHandler: http.NotFoundHandler(), MarkerStore: marker,
		Deadlines: phaseGDeadlines(&now),
		NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:  func() ([]byte, error) { return bytes.Repeat([]byte{byte(len(handoffs) + 1)}, 32), nil },
		Spawn: func(handoff SelfRestartHandoff) (RestartParentChild, error) {
			handoffs = append(handoffs, handoff)
			return nil, errors.New("synthetic spawn failure")
		},
		Confirm:  func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error { return nil },
		CloseHub: func(context.Context) {},
		Exit:     func() {},
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}
	for generation := 0; generation < 2; generation++ {
		if _, err := coordinator.Start(); err == nil || !strings.Contains(err.Error(), "synthetic spawn failure") {
			t.Fatalf("generation %d Start error = %v, want synthetic spawn failure", generation, err)
		}
	}
	if len(handoffs) != 2 {
		t.Fatalf("captured handoffs = %d, want 2", len(handoffs))
	}
	if handoffs[0].NoncePath == handoffs[1].NoncePath {
		t.Errorf("generation A and B nonce paths are both %q; want distinct generation-bound leaves", handoffs[0].NoncePath)
	}

	staleNonce := bytes.Repeat([]byte{0xa1}, authenticatedReadinessNonceBytes)
	if err := api.WriteStateFileBytesAtomic(handoffs[0].NoncePath, staleNonce); err != nil {
		t.Fatalf("write stale generation-A nonce: %v", err)
	}
	staleForB := handoffs[1]
	staleForB.NoncePath = handoffs[0].NoncePath
	raw, err := EncodeSelfRestartHandoff(staleForB)
	if err != nil {
		t.Fatalf("EncodeSelfRestartHandoff: %v", err)
	}
	child, consumeErr := NewSpawnedGUIChildFromEnvironment(raw, os.Getpid(), stateDir)
	if child != nil {
		child.Close()
	}
	if consumeErr == nil {
		t.Error("generation B consumed generation A's stale nonce leaf; want canonical generation mismatch")
	}
	got, readErr := os.ReadFile(handoffs[0].NoncePath)
	if readErr != nil || !bytes.Equal(got, staleNonce) {
		t.Fatalf("stale generation-A nonce was consumed or altered: bytes=%x err=%v", got, readErr)
	}
}

func TestRestartV3_BeginSweepsStaleGenerationNonceAndLock(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	stateDir := apitest.HardenedTempDir(t)
	staleNoncePath := restartNoncePath(stateDir, "stale-generation")
	if err := api.WriteStateFileBytesAtomic(staleNoncePath, bytes.Repeat([]byte{0xa5}, authenticatedReadinessNonceBytes)); err != nil {
		t.Fatalf("write stale nonce generation: %v", err)
	}
	staleLockPath := staleNoncePath + ".lock"
	if _, err := os.Stat(staleNoncePath); err != nil {
		t.Fatalf("stale nonce precondition: %v", err)
	}
	if _, err := os.Stat(staleLockPath); err != nil {
		t.Fatalf("stale lock precondition: %v", err)
	}

	order := &phaseGOrder{}
	ids := []string{"handoff-sweep", "generation-sweep"}
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: stateDir,
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 19125, nil }, ParentPID: 1111,
		Lease: &phaseGLease{order: order}, Listener: &phaseGListener{order: order}, FullHandler: http.NotFoundHandler(), MarkerStore: &phaseGMarkerStore{},
		Deadlines: phaseGDeadlines(&now),
		NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:  func() ([]byte, error) { return bytes.Repeat([]byte{0xa6}, authenticatedReadinessNonceBytes), nil },
		Spawn: func(SelfRestartHandoff) (RestartParentChild, error) {
			return nil, errors.New("synthetic spawn failure after sweep")
		},
		Confirm:  func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error { return nil },
		CloseHub: func(context.Context) {},
		Exit:     func() {},
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}
	if _, err := coordinator.Start(); err == nil || !strings.Contains(err.Error(), "synthetic spawn failure after sweep") {
		t.Fatalf("Start error = %v, want post-sweep synthetic spawn failure", err)
	}
	for _, path := range []string{staleNoncePath, staleLockPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stale generation residue %q survived Begin: %v", path, err)
		}
	}
}

func TestRestartV3_PostBeginCleanupFailureTerminalizesMarkerBeforeRunReset(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	order := &phaseGOrder{}
	marker := &phaseGMarkerStore{clearErr: errors.New("synthetic marker removal failure")}
	ids := []string{"handoff-cleanup", "generation-cleanup"}
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: apitest.HardenedTempDir(t),
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 19125, nil }, ParentPID: 1111,
		Lease: &phaseGLease{order: order}, Listener: &phaseGListener{order: order}, FullHandler: http.NotFoundHandler(), MarkerStore: marker,
		Deadlines: phaseGDeadlines(&now),
		NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:  func() ([]byte, error) { return nil, errors.New("synthetic nonce creation failure") },
		Spawn:     func(SelfRestartHandoff) (RestartParentChild, error) { return nil, errors.New("spawn must not run") },
		Confirm:   func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error { return nil },
		CloseHub:  func(context.Context) {},
		Exit:      func() {},
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}

	if _, err := coordinator.Start(); err == nil || !strings.Contains(err.Error(), "synthetic marker removal failure") {
		t.Fatalf("Start error = %v, want surfaced marker cleanup failure", err)
	}
	if marker.record == nil || marker.record.Generation != "generation-cleanup" || marker.record.Phase != HandoffPhaseInterrupted || marker.record.Phase.nonterminal() {
		t.Fatalf("marker after cleanup failure = %+v, want terminal interrupted generation", marker.record)
	}
	if marker.interrupts != 1 || marker.record.ReasonCode != "gui-restart-pre-accept-cleanup-failed" {
		t.Fatalf("interrupts=%d marker=%+v, want one pre-accept cleanup terminalization", marker.interrupts, marker.record)
	}
	if _, err := coordinator.Start(); !errors.Is(err, ErrRestartAlreadyInProgress) {
		t.Fatalf("second Start error = %v, want in-memory guard retained after unproved residue cleanup", err)
	}
}

func TestRestartV3_PostBeginMarkerClearWithNonceCleanupFailureDoesNotWedgeRun(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	order := &phaseGOrder{}
	marker := &phaseGMarkerStore{}
	ids := []string{"handoff-cleanup-a", "generation-cleanup-a", "handoff-cleanup-b", "generation-cleanup-b"}
	removeErr := errors.New("synthetic nonce removal failure")
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: apitest.HardenedTempDir(t),
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 19125, nil }, ParentPID: 1111,
		Lease: &phaseGLease{order: order}, Listener: &phaseGListener{order: order}, FullHandler: http.NotFoundHandler(), MarkerStore: marker,
		Deadlines:   phaseGDeadlines(&now),
		NewID:       func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:    func() ([]byte, error) { return bytes.Repeat([]byte{0x74}, authenticatedReadinessNonceBytes), nil },
		RemoveNonce: func(string) error { return removeErr },
		Spawn: func(SelfRestartHandoff) (RestartParentChild, error) {
			return nil, errors.New("synthetic spawn failure")
		},
		Confirm:  func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error { return nil },
		CloseHub: func(context.Context) {},
		Exit:     func() {},
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}
	if _, err := coordinator.Start(); err == nil || !errors.Is(err, removeErr) {
		t.Fatalf("first Start error = %v, want nonce cleanup failure", err)
	}
	if marker.record != nil || marker.interrupts != 0 {
		t.Fatalf("marker after proved clear = %+v interrupts=%d, want no retained marker", marker.record, marker.interrupts)
	}
	if _, err := coordinator.Start(); errors.Is(err, ErrRestartAlreadyInProgress) {
		t.Fatalf("second Start remained permanently wedged: %v", err)
	}
}

func TestRestartV3_PortChange_ParentClosesHubBeforeFlockReleaseThenChildActivatesImmediately(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	stateDir := apitest.HardenedTempDir(t)
	order := &phaseGOrder{}
	child := &phaseGChildHandle{order: order, pid: 4242}
	lease := &phaseGLease{order: order}
	lease.onRelease = func() { order.add("child-activates") }
	ids := []string{"handoff-g1", "generation-g1"}
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context:     context.Background(),
		StateDir:    stateDir,
		OldPort:     func() int { return 9125 },
		TargetPort:  func(int) (int, error) { return 19125, nil },
		ParentPID:   1111,
		Lease:       lease,
		Listener:    &phaseGListener{order: order},
		FullHandler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		MarkerStore: NewHandoffMarkerStore(stateDir, RestartDeadlines{
			Now: phaseGNow(&now), RecordLock: time.Second, Freshness: 3 * time.Minute,
			Reservation: 10 * time.Second, Proof: 10 * time.Second, Bind: 2 * time.Second,
			Quiesce: 5 * time.Second, Rollback: 5 * time.Second, Grace: 5 * time.Second,
		}),
		Deadlines: RestartDeadlines{
			Now: phaseGNow(&now), RecordLock: time.Second, Freshness: 3 * time.Minute,
			Reservation: 10 * time.Second, Proof: 10 * time.Second, Bind: 2 * time.Second,
			Quiesce: 5 * time.Second, Rollback: 5 * time.Second, Grace: 5 * time.Second,
		},
		NewID: func() (string, error) {
			id := ids[0]
			ids = ids[1:]
			return id, nil
		},
		NewNonce: func() ([]byte, error) { return bytes.Repeat([]byte{0x71}, 32), nil },
		Spawn: func(handoff SelfRestartHandoff) (RestartParentChild, error) {
			order.add("spawn")
			if handoff.TargetPort != 19125 || handoff.OldPort != 9125 {
				t.Fatalf("spawn handoff ports = %d->%d, want 9125->19125", handoff.OldPort, handoff.TargetPort)
			}
			return child, nil
		},
		Confirm: func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error {
			order.add("confirm-authenticated-standby")
			return nil
		},
		Events: &phaseFEvents{},
		CloseHub: func(context.Context) {
			order.add("hub-close")
		},
		WaitGrace: func(context.Context, time.Duration) error {
			order.add("grace-finished")
			return nil
		},
		Exit: func() { order.add("process-exit") },
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}

	started, err := coordinator.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	started.AcknowledgeResponseFlushed()
	result := <-started.Done
	if result.Err != nil || !result.ParentLeaseReleased {
		t.Fatalf("coordinator result = %+v, want successful released handoff", result)
	}
	got := order.snapshot()
	want := []string{
		"spawn", "confirm-authenticated-standby", "enter-grace", "hub-close",
		"flock-release", "child-activates", "child-detach", "grace-finished",
		"close-gui-listener", "process-exit",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("handoff order = %v, want %v", got, want)
	}
	if lease.releaseCnt != 1 || child.detached != 1 || child.terminated != 0 {
		t.Fatalf("release=%d detach=%d terminate=%d, want 1/1/0", lease.releaseCnt, child.detached, child.terminated)
	}
}

func TestRestartV3_SamePort_PreReleaseRollbackRetainsLeaseAndRebindsWithoutReacquire(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	order := &phaseGOrder{}
	lease := &phaseGLease{order: order}
	child := &phaseGChildHandle{order: order, pid: 4243}
	listener := &phaseGListener{order: order, rebound: phaseGNetListener{}}
	marker := &phaseGMarkerStore{reserveErr: errors.New("reservation failed")}
	exits := 0
	ids := []string{"handoff-g2", "generation-g2"}
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: apitest.HardenedTempDir(t),
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 9125, nil }, ParentPID: 1111,
		Lease: lease, Listener: listener, FullHandler: http.NotFoundHandler(), MarkerStore: marker,
		Deadlines: phaseGDeadlines(&now),
		NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:  func() ([]byte, error) { return bytes.Repeat([]byte{0x72}, 32), nil },
		Spawn:     func(SelfRestartHandoff) (RestartParentChild, error) { order.add("spawn"); return child, nil },
		Confirm: func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error {
			order.add("confirm-authenticated-standby")
			return nil
		},
		CloseHub: func(context.Context) { order.add("hub-close") },
		Exit:     func() { exits++ },
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}
	started, err := coordinator.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	started.AcknowledgeResponseFlushed()
	result := <-started.Done
	if result.Err == nil || result.ParentLeaseReleased {
		t.Fatalf("result = %+v, want pre-release rollback error with retained lease", result)
	}
	if lease.releaseCnt != 0 || lease.reacquires != 0 {
		t.Fatalf("release=%d reacquire=%d, want 0/0", lease.releaseCnt, lease.reacquires)
	}
	if child.terminated != 1 || listener.servedFull != 1 || marker.clears != 1 || marker.record != nil {
		t.Fatalf("terminate=%d served-full=%d clears=%d marker=%+v", child.terminated, listener.servedFull, marker.clears, marker.record)
	}
	if exits != 0 {
		t.Fatalf("process exits = %d, want 0 after proved rollback", exits)
	}
	want := []string{"spawn", "enter-grace", "close-gui-listener", "confirm-authenticated-standby", "child-terminate", "bind-for-recovery", "serve-full"}
	if got := order.snapshot(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rollback order = %v, want %v", got, want)
	}
}

func TestRestartV3_SamePort_TerminalConfirmFailureTerminatesUnauthenticatedChildBeforeRecoveryBind(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	order := &phaseGOrder{}
	lease := &phaseGLease{order: order}
	childAlive := true
	child := &phaseGChildHandle{order: order, pid: 4247}
	listener := &phaseGListener{order: order}
	child.onTerminate = func() { childAlive = false }
	listener.bindForRecovery = func(context.Context, int) (net.Listener, error) {
		if childAlive {
			return nil, errors.New("standby child still owns old port")
		}
		return phaseGNetListener{}, nil
	}
	marker := &phaseGMarkerStore{}
	ids := []string{"handoff-terminal-confirm", "generation-terminal-confirm"}
	exits := 0
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: apitest.HardenedTempDir(t),
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 9125, nil }, ParentPID: 1111,
		Lease: lease, Listener: listener, FullHandler: http.NotFoundHandler(), MarkerStore: marker,
		Deadlines: phaseGDeadlines(&now),
		NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:  func() ([]byte, error) { return bytes.Repeat([]byte{0x75}, authenticatedReadinessNonceBytes), nil },
		Spawn:     func(SelfRestartHandoff) (RestartParentChild, error) { return child, nil },
		Confirm: func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error {
			return errors.New("terminal proof wire mismatch")
		},
		CloseHub: func(context.Context) {},
		Exit:     func() { exits++ },
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}
	started, err := coordinator.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	result := <-started.Done
	if result.Err == nil || result.ParentLeaseReleased {
		t.Fatalf("result = %+v, want proved pre-release rollback", result)
	}
	if child.terminated != 1 || childAlive || listener.servedFull != 1 || listener.restoredFull != 0 {
		t.Fatalf("terminate=%d childAlive=%v serve-full=%d restore-full=%d", child.terminated, childAlive, listener.servedFull, listener.restoredFull)
	}
	if lease.releaseCnt != 0 || exits != 0 || marker.record != nil || marker.clears != 1 {
		t.Fatalf("release=%d exits=%d marker=%+v clears=%d", lease.releaseCnt, exits, marker.record, marker.clears)
	}
}

func TestRestartV3_PreReleaseRollbackFailureInterruptsReleasesLeaseAndExits(t *testing.T) {
	for _, tc := range []struct {
		name          string
		interruptErr  error
		wantReason    string
		wantInterrupt int
	}{
		{name: "interrupted marker written", wantReason: "gui-restart-pre-release-rollback-failed", wantInterrupt: 1},
		{name: "interrupted marker write fails", interruptErr: errors.New("marker write failed"), wantReason: "gui-restart-interrupted-marker-write-failed", wantInterrupt: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
			order := &phaseGOrder{}
			lease := &phaseGLease{order: order}
			child := &phaseGChildHandle{order: order, pid: 4244}
			listener := &phaseGListener{order: order, restoreErr: errors.New("full handler restoration failed")}
			marker := &phaseGMarkerStore{reserveErr: errors.New("reservation failed"), interruptErr: tc.interruptErr}
			events := &phaseFEvents{}
			exits := 0
			ids := []string{"handoff-g3", "generation-g3"}
			coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
				Context: context.Background(), StateDir: apitest.HardenedTempDir(t),
				OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 19125, nil }, ParentPID: 1111,
				Lease: lease, Listener: listener, FullHandler: http.NotFoundHandler(), MarkerStore: marker,
				Deadlines: phaseGDeadlines(&now),
				NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
				NewNonce:  func() ([]byte, error) { return bytes.Repeat([]byte{0x73}, 32), nil },
				Spawn:     func(SelfRestartHandoff) (RestartParentChild, error) { order.add("spawn"); return child, nil },
				Confirm:   func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error { return nil },
				Events:    events,
				CloseHub:  func(context.Context) { order.add("hub-close") },
				Exit:      func() { exits++ },
			})
			if err != nil {
				t.Fatalf("NewRestartCoordinator: %v", err)
			}
			started, err := coordinator.Start()
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			started.AcknowledgeResponseFlushed()
			result := <-started.Done
			if result.Err == nil || !result.ParentLeaseReleased {
				t.Fatalf("result = %+v, want terminal rollback failure", result)
			}
			if lease.releaseCnt != 1 || child.terminated != 1 || child.detached != 1 || exits != 1 {
				t.Fatalf("release=%d terminate=%d detach=%d exits=%d, want 1/1/1/1", lease.releaseCnt, child.terminated, child.detached, exits)
			}
			if marker.interrupts != tc.wantInterrupt {
				t.Fatalf("interrupt writes = %d, want %d", marker.interrupts, tc.wantInterrupt)
			}
			if tc.interruptErr == nil && (marker.record == nil || marker.record.Phase != HandoffPhaseInterrupted) {
				t.Fatalf("marker = %+v, want interrupted", marker.record)
			}
			foundReason := false
			var terminalEvent Event
			for _, event := range events.snapshot() {
				if event.Body["reason_code"] == tc.wantReason {
					foundReason = true
					terminalEvent = event
				}
			}
			if !foundReason {
				t.Fatalf("events = %+v, want reason %q", events.snapshot(), tc.wantReason)
			}
			if terminalEvent.Type != "gui-restart-progress" || terminalEvent.Body["phase"] != HandoffPhaseInterrupted {
				t.Fatalf("terminal event = %#v, want gui-restart-progress/interrupted with body reason_code", terminalEvent)
			}
		})
	}
}

func TestRestartV3_ParentPerformsNoProtocolWriteWaitTerminateOrReclaimAfterRelease(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	order := &phaseGOrder{}
	released := false
	lease := &phaseGLease{order: order, onRelease: func() { released = true }}
	child := &phaseGChildHandle{order: order, pid: 4245}
	listener := &phaseGListener{order: order}
	marker := &phaseGMarkerStore{}
	postReleaseMarkerWrites := 0
	guardedMarker := &phaseGPostReleaseMarkerStore{
		inner:             marker,
		released:          func() bool { return released },
		postReleaseWrites: &postReleaseMarkerWrites,
	}
	postReleaseWaits := 0
	postReleaseBinds := 0
	ids := []string{"handoff-g4", "generation-g4"}
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: apitest.HardenedTempDir(t),
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 19125, nil }, ParentPID: 1111,
		Lease: lease, Listener: &phaseGPostReleaseListener{
			phaseGListener:   listener,
			released:         func() bool { return released },
			postReleaseBinds: &postReleaseBinds,
		}, FullHandler: http.NotFoundHandler(), MarkerStore: guardedMarker,
		Deadlines: phaseGDeadlines(&now),
		NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:  func() ([]byte, error) { return bytes.Repeat([]byte{0x74}, 32), nil },
		Spawn:     func(SelfRestartHandoff) (RestartParentChild, error) { return child, nil },
		Confirm: func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error {
			if released {
				postReleaseWaits++
			}
			return nil
		},
		CloseHub:  func(context.Context) {},
		WaitGrace: func(context.Context, time.Duration) error { return nil },
		Exit:      func() {},
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}
	started, err := coordinator.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	started.AcknowledgeResponseFlushed()
	result := <-started.Done
	if result.Err != nil || !result.ParentLeaseReleased {
		t.Fatalf("result = %+v, want successful handoff", result)
	}
	if postReleaseMarkerWrites != 0 || postReleaseWaits != 0 || postReleaseBinds != 0 || child.terminated != 0 || lease.reacquires != 0 {
		t.Fatalf("post-release marker=%d wait=%d bind=%d terminate=%d reacquire=%d, want all zero",
			postReleaseMarkerWrites, postReleaseWaits, postReleaseBinds, child.terminated, lease.reacquires)
	}
	if lease.releaseCnt != 1 || child.detached != 1 {
		t.Fatalf("release=%d detach=%d, want exactly 1/1", lease.releaseCnt, child.detached)
	}
}

type phaseGPostReleaseMarkerStore struct {
	inner             RestartCoordinatorMarkerStore
	released          func() bool
	postReleaseWrites *int
}

func (s *phaseGPostReleaseMarkerStore) count() {
	if s.released() {
		*s.postReleaseWrites++
	}
}

func (s *phaseGPostReleaseMarkerStore) Begin(begin HandoffBegin) (*HandoffMarkerRecord, error) {
	s.count()
	return s.inner.Begin(begin)
}

func (s *phaseGPostReleaseMarkerStore) Reserve(generation string, sequence uint64, expires time.Time, hash string, pid int) (*HandoffMarkerRecord, error) {
	s.count()
	return s.inner.Reserve(generation, sequence, expires, hash, pid)
}

func (s *phaseGPostReleaseMarkerStore) Interrupt(generation, reason, action string) (*HandoffMarkerRecord, error) {
	s.count()
	return s.inner.Interrupt(generation, reason, action)
}

func (s *phaseGPostReleaseMarkerStore) ClearAfterProvedPreReleaseRollback(generation string) error {
	s.count()
	return s.inner.ClearAfterProvedPreReleaseRollback(generation)
}

type phaseGPostReleaseListener struct {
	*phaseGListener
	released         func() bool
	postReleaseBinds *int
}

func (l *phaseGPostReleaseListener) BindForRecovery(ctx context.Context, port int) (net.Listener, error) {
	if l.released() {
		*l.postReleaseBinds++
	}
	return l.phaseGListener.BindForRecovery(ctx, port)
}

func TestRestartV3_GraceAllowlistRedirectAndMutatorRejection(t *testing.T) {
	eventsCalls := 0
	handler := newRestartGraceHandler("handoff-g6", 19125, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eventsCalls++
		w.WriteHeader(http.StatusNoContent)
	}))

	eventsReq := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	eventsRes := httptest.NewRecorder()
	handler.ServeHTTP(eventsRes, eventsReq)
	if eventsRes.Code != http.StatusNoContent || eventsCalls != 1 {
		t.Fatalf("events status=%d calls=%d, want 204/1", eventsRes.Code, eventsCalls)
	}

	preRelease := httptest.NewRecorder()
	handler.ServeHTTP(preRelease, httptest.NewRequest(http.MethodGet, "/api/gui/restart/redirect?handoff_id=handoff-g6&target_url=https://attacker.invalid/", nil))
	if preRelease.Code != http.StatusAccepted || !strings.Contains(preRelease.Body.String(), `"released":false`) {
		t.Fatalf("pre-release redirect = %d %s, want 202 released:false", preRelease.Code, preRelease.Body.String())
	}

	handler.released.Store(true)
	postRelease := httptest.NewRecorder()
	handler.ServeHTTP(postRelease, httptest.NewRequest(http.MethodGet, "/api/gui/restart/redirect?handoff_id=handoff-g6&target_url=https://attacker.invalid/", nil))
	if postRelease.Code != http.StatusOK || !strings.Contains(postRelease.Body.String(), `"target_url":"http://127.0.0.1:19125/"`) || strings.Contains(postRelease.Body.String(), "attacker.invalid") {
		t.Fatalf("post-release redirect = %d %s, want trusted loopback target only", postRelease.Code, postRelease.Body.String())
	}

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/events", strings.NewReader("{}")),
		httptest.NewRequest(http.MethodGet, "/api/health", nil),
		httptest.NewRequest(http.MethodGet, "/api/gui/restart/redirect?handoff_id=wrong", nil),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "GUI_RESTART_IN_PROGRESS") {
			t.Fatalf("%s %s = %d %q, want 503 GUI_RESTART_IN_PROGRESS", request.Method, request.URL, response.Code, response.Body.String())
		}
	}
}

func TestRestartV3_GraceNavigationIsBestEffortAndNeverClaimsCommit(t *testing.T) {
	events := &phaseFEvents{}
	coordinator := &RestartCoordinator{deps: RestartCoordinatorDependencies{Events: events}}
	start := RestartCoordinatorStart{
		HandoffID:  "handoff-navigation",
		Generation: "generation-navigation",
		OldPort:    19125,
		TargetPort: 19126,
	}
	coordinator.publishProgress(start, HandoffPhaseReserved, "")

	published := events.snapshot()
	if len(published) != 1 {
		t.Fatalf("published events = %d, want one reserved navigation event", len(published))
	}
	event := published[0]
	if event.Type != "gui-restart-progress" || event.Body["phase"] != HandoffPhaseReserved {
		t.Fatalf("navigation event = %#v, want gui-restart-progress/reserved", event)
	}
	if event.Body["old_port"] != 19125 || event.Body["new_port"] != 19126 || event.Body["same_port"] != false {
		t.Fatalf("navigation ports = %#v, want old=19125 new=19126 same_port=false", event.Body)
	}
	for _, forbidden := range []string{"committed", "child_committed", "activation_committed"} {
		if _, ok := event.Body[forbidden]; ok {
			t.Fatalf("best-effort navigation event asserts %q: %#v", forbidden, event.Body)
		}
	}

	handler := newRestartGraceHandler(start.HandoffID, start.TargetPort, http.NotFoundHandler())
	handler.released.Store(true)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/gui/restart/redirect?handoff_id="+start.HandoffID, nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"target_url":"http://127.0.0.1:19126/"`) {
		t.Fatalf("released redirect = %d %s, want trusted best-effort target", response.Code, response.Body.String())
	}
	if strings.Contains(strings.ToLower(response.Body.String()), "committed") {
		t.Fatalf("best-effort redirect claims child commit: %s", response.Body.String())
	}
}

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
			NoncePath:  restartNoncePath(stateDir, "generation-f"),
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
	noncePath := restartNoncePath(stateDir, "generation-f")
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
	noncePath := restartNoncePath(stateDir, "generation-f")
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
	foreignNoncePath := restartNoncePath(foreignDir, "generation-f")
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
	noncePath := restartNoncePath(stateDir, "generation-f")
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

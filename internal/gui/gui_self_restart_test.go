package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
)

type phaseGRestartStarter struct {
	start RestartCoordinatorStart
	err   error
	calls int
}

type phaseGBlockingFlushWriter struct {
	header       http.Header
	body         bytes.Buffer
	status       int
	writeStarted chan struct{}
	allowWrite   chan struct{}
	flushed      chan struct{}
	writeOnce    sync.Once
	flushOnce    sync.Once
}

type phaseGNoFlushWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *phaseGNoFlushWriter) Header() http.Header { return w.header }

func (w *phaseGNoFlushWriter) WriteHeader(status int) { w.status = status }

func (w *phaseGNoFlushWriter) Write(value []byte) (int, error) {
	return w.body.Write(value)
}

func newPhaseGBlockingFlushWriter() *phaseGBlockingFlushWriter {
	return &phaseGBlockingFlushWriter{
		header:       make(http.Header),
		writeStarted: make(chan struct{}),
		allowWrite:   make(chan struct{}),
		flushed:      make(chan struct{}),
	}
}

func (w *phaseGBlockingFlushWriter) Header() http.Header { return w.header }

func (w *phaseGBlockingFlushWriter) WriteHeader(status int) { w.status = status }

func (w *phaseGBlockingFlushWriter) Write(value []byte) (int, error) {
	w.writeOnce.Do(func() { close(w.writeStarted) })
	<-w.allowWrite
	return w.body.Write(value)
}

func (w *phaseGBlockingFlushWriter) Flush() {
	w.flushOnce.Do(func() { close(w.flushed) })
}

func (s *phaseGRestartStarter) Start() (RestartCoordinatorStart, error) {
	s.calls++
	return s.start, s.err
}

// swapSelfRestartSeams installs spawn + exit seams for the test scope and
// restores them on cleanup. The exit seam records whether the handler
// asked to exit (it must NOT call os.Exit in a test). The spawn seam
// returns the supplied (pid, err) without launching any real process.
func swapSelfRestartSeams(t *testing.T, pid int, spawnErr error) (exited *bool, waitExit func()) {
	t.Helper()
	origSpawn := selfRestartSpawnFn
	origExit := selfRestartExitFn
	exitCalled := make(chan struct{})
	t.Cleanup(func() {
		selfRestartSpawnFn = origSpawn
		selfRestartExitFn = origExit
	})
	ex := false
	exited = &ex
	selfRestartSpawnFn = func() (int, error) { return pid, spawnErr }
	selfRestartExitFn = func() {
		ex = true
		close(exitCalled)
	} // never os.Exit in a test
	waitExit = func() {
		t.Helper()
		select {
		case <-exitCalled:
		case <-time.After(selfRestartExitDelay + time.Second):
			t.Fatal("timed out waiting for self-restart exit seam")
		}
	}
	return exited, waitExit
}

// TestGUISelfRestart_SpawnSuccess: a successful spawn returns 200 with
// restarting:true and the spawned PID, and the handler schedules the exit
// (via the seam) so the lock is handed off.
func TestGUISelfRestart_SpawnSuccess(t *testing.T) {
	s := NewServer(Config{Port: 9})
	exited, waitExit := swapSelfRestartSeams(t, 4242, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/gui/restart", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9")
	rr := httptest.NewRecorder()
	s.guiSelfRestartHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp guiSelfRestartResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Spawned || resp.SpawnedPID != 4242 || !resp.Restarting {
		t.Fatalf("resp = %+v, want spawned=true pid=4242 restarting=true", resp)
	}
	if resp.SpawnError != "" {
		t.Fatalf("unexpected spawn_error %q", resp.SpawnError)
	}
	waitExit()
	if !*exited {
		t.Fatal("handler did not invoke self-restart exit seam")
	}
}

// TestGUISelfRestart_SpawnFailureNoExit: when the spawn fails the handler
// must NOT exit (the operator keeps the running GUI) and must surface the
// error in the body with spawned:false / restarting:false.
func TestGUISelfRestart_SpawnFailureNoExit(t *testing.T) {
	s := NewServer(Config{Port: 9})
	exited, _ := swapSelfRestartSeams(t, 0, errSelfRestartTest)

	req := httptest.NewRequest(http.MethodPost, "/api/gui/restart", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9")
	rr := httptest.NewRecorder()
	s.guiSelfRestartHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var resp guiSelfRestartResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Spawned || resp.Restarting {
		t.Fatalf("resp = %+v, want spawned=false restarting=false", resp)
	}
	if resp.SpawnError == "" {
		t.Fatalf("want non-empty spawn_error")
	}
	if *exited {
		t.Fatalf("handler must NOT exit when spawn fails")
	}
}

// TestGUISelfRestart_MethodNotAllowed: GET is rejected 405.
func TestGUISelfRestart_MethodNotAllowed(t *testing.T) {
	s := NewServer(Config{Port: 9})
	_, _ = swapSelfRestartSeams(t, 1, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/gui/restart", nil)
	rr := httptest.NewRecorder()
	s.guiSelfRestartHandler(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rr.Code)
	}
}

func TestRestartV3_API202RetainsRestartingField(t *testing.T) {
	coordinator := &phaseGRestartStarter{start: RestartCoordinatorStart{
		HandoffID: "handoff-g5", Generation: "generation-g5", Phase: HandoffPhaseInProgress,
		SpawnedPID: 4250, OldPort: 9125, TargetPort: 19125,
	}}
	s := NewServer(Config{Port: 9125, RestartV3Enabled: true})
	s.restartCoordinator = coordinator

	req := httptest.NewRequest(http.MethodPost, "/api/gui/restart", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	response := httptest.NewRecorder()
	s.guiSelfRestartHandler(response, req)

	if response.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.Code)
	}
	var body guiSelfRestartResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.Spawned || body.SpawnedPID != 4250 || !body.Restarting || body.Phase != HandoffPhaseInProgress {
		t.Fatalf("body = %+v, want spawned in-progress restarting response", body)
	}
	if body.HandoffID != "handoff-g5" || body.Generation != "generation-g5" || body.OldPort != 9125 || body.TargetPort != 19125 {
		t.Fatalf("body identity/ports = %+v", body)
	}
	if coordinator.calls != 1 {
		t.Fatalf("coordinator calls = %d, want 1", coordinator.calls)
	}
}

func TestRestartV3_API202FlushesBeforeCoordinatorCanExit(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	order := &phaseGOrder{}
	lease := &phaseGLease{order: order}
	child := &phaseGChildHandle{order: order, pid: 4251}
	ids := []string{"handoff-flush", "generation-flush"}
	exitCalled := make(chan struct{})
	exitBeforeFlush := make(chan struct{}, 1)
	writer := newPhaseGBlockingFlushWriter()
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: apitest.HardenedTempDir(t),
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 19125, nil }, ParentPID: 1111,
		Lease: lease, Listener: &phaseGListener{order: order}, FullHandler: http.NotFoundHandler(), MarkerStore: &phaseGMarkerStore{},
		Deadlines: phaseGDeadlines(&now),
		NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:  func() ([]byte, error) { return bytes.Repeat([]byte{0x6e}, 32), nil },
		Spawn:     func(SelfRestartHandoff) (RestartParentChild, error) { return child, nil },
		Confirm:   func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error { return nil },
		CloseHub:  func(context.Context) {},
		WaitGrace: func(context.Context, time.Duration) error { return nil },
		Exit: func() {
			select {
			case <-writer.flushed:
			default:
				exitBeforeFlush <- struct{}{}
			}
			close(exitCalled)
		},
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}
	s := NewServer(Config{Port: 9125, RestartV3Enabled: true})
	s.restartCoordinator = coordinator

	handlerDone := make(chan struct{})
	go func() {
		s.guiRestartV3Handler(writer)
		close(handlerDone)
	}()
	select {
	case <-writer.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("202 JSON body write did not start")
	}
	prematureExit := false
	select {
	case <-exitBeforeFlush:
		prematureExit = true
	case <-time.After(50 * time.Millisecond):
	}
	close(writer.allowWrite)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("restart handler did not return after body write was released")
	}
	select {
	case <-exitCalled:
	case <-time.After(time.Second):
		t.Fatal("coordinator exit did not run after the response flush")
	}
	if prematureExit {
		t.Fatal("coordinator exit ran before the 202 response body was flushed")
	}
	if writer.status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", writer.status)
	}
}

func TestRestartV3_API202WithoutFlusherStillAcknowledgesCoordinator(t *testing.T) {
	acknowledged := make(chan struct{})
	var acknowledgeOnce sync.Once
	coordinator := &phaseGRestartStarter{start: RestartCoordinatorStart{
		HandoffID: "handoff-no-flusher", Generation: "generation-no-flusher", Phase: HandoffPhaseInProgress,
		SpawnedPID: 4253, OldPort: 9125, TargetPort: 19125,
		responseFlushed: func() { acknowledgeOnce.Do(func() { close(acknowledged) }) },
	}}
	s := NewServer(Config{Port: 9125, RestartV3Enabled: true})
	s.restartCoordinator = coordinator
	writer := &phaseGNoFlushWriter{header: make(http.Header)}

	s.guiRestartV3Handler(writer)
	select {
	case <-acknowledged:
	case <-time.After(time.Second):
		t.Fatal("202 Encode completed without acknowledging the coordinator on a non-Flusher writer")
	}
	if writer.status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", writer.status)
	}
}

func TestRestartV3_ConcurrentRestartReturnsActive202(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	order := &phaseGOrder{}
	lease := &phaseGLease{order: order}
	child := &phaseGChildHandle{order: order, pid: 4252}
	ids := []string{"handoff-active", "generation-active"}
	spawnEntered := make(chan struct{})
	allowSpawn := make(chan struct{})
	confirmEntered := make(chan struct{})
	allowConfirm := make(chan struct{})
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: apitest.HardenedTempDir(t),
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 19125, nil }, ParentPID: 1111,
		Lease: lease, Listener: &phaseGListener{order: order}, FullHandler: http.NotFoundHandler(), MarkerStore: &phaseGMarkerStore{},
		Deadlines: phaseGDeadlines(&now),
		NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:  func() ([]byte, error) { return bytes.Repeat([]byte{0x6d}, 32), nil },
		Spawn: func(SelfRestartHandoff) (RestartParentChild, error) {
			close(spawnEntered)
			<-allowSpawn
			return child, nil
		},
		Confirm: func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error {
			close(confirmEntered)
			<-allowConfirm
			return nil
		},
		CloseHub:  func(context.Context) {},
		WaitGrace: func(context.Context, time.Duration) error { return nil },
		Exit:      func() {},
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}
	type startResult struct {
		start RestartCoordinatorStart
		err   error
	}
	firstResult := make(chan startResult, 1)
	go func() {
		start, startErr := coordinator.Start()
		firstResult <- startResult{start: start, err: startErr}
	}()
	select {
	case <-spawnEntered:
	case <-time.After(time.Second):
		t.Fatal("first handoff did not enter spawn")
	}

	s := NewServer(Config{Port: 9125, RestartV3Enabled: true})
	s.restartCoordinator = coordinator
	response := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		s.guiRestartV3Handler(response)
		close(secondDone)
	}()
	returnedBeforeAccepted := false
	select {
	case <-secondDone:
		returnedBeforeAccepted = true
	case <-time.After(25 * time.Millisecond):
	}
	close(allowSpawn)
	first := <-firstResult
	if first.err != nil {
		t.Fatalf("first Start: %v", first.err)
	}
	active := first.start
	active.AcknowledgeResponseFlushed()
	select {
	case <-confirmEntered:
	case <-time.After(time.Second):
		t.Fatal("first handoff did not enter confirmation")
	}
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("concurrent endpoint did not return the accepted active handoff")
	}
	close(allowConfirm)
	result := <-active.Done
	if result.Err != nil {
		t.Fatalf("first handoff completion: %v", result.Err)
	}

	var body guiSelfRestartResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode concurrent response: %v", err)
	}
	if response.Code != http.StatusAccepted || !body.Restarting || !body.Spawned || body.SpawnError != "" {
		t.Fatalf("concurrent response = status %d body %+v, want 202 active restarting handoff", response.Code, body)
	}
	if returnedBeforeAccepted {
		t.Fatal("concurrent request returned before the first handoff had an active descriptor")
	}
	if body.HandoffID != active.HandoffID || body.Generation != active.Generation || body.SpawnedPID != active.SpawnedPID {
		t.Fatalf("concurrent response identity = %+v, want active %+v", body, active)
	}
}

func TestRestartV3_ConcurrentRestartReportsReservedActivePhase(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	order := &phaseGOrder{}
	lease := &phaseGLease{order: order}
	child := &phaseGChildHandle{order: order, pid: 4254}
	reserved := make(chan struct{})
	marker := &phaseGMarkerStore{onReserve: func() { close(reserved) }}
	ids := []string{"handoff-reserved-active", "generation-reserved-active"}
	coordinator, err := NewRestartCoordinator(RestartCoordinatorDependencies{
		Context: context.Background(), StateDir: apitest.HardenedTempDir(t),
		OldPort: func() int { return 9125 }, TargetPort: func(int) (int, error) { return 19125, nil }, ParentPID: 1111,
		Lease: lease, Listener: &phaseGListener{order: order}, FullHandler: http.NotFoundHandler(), MarkerStore: marker,
		Deadlines: phaseGDeadlines(&now),
		NewID:     func() (string, error) { id := ids[0]; ids = ids[1:]; return id, nil },
		NewNonce:  func() ([]byte, error) { return bytes.Repeat([]byte{0x76}, authenticatedReadinessNonceBytes), nil },
		Spawn:     func(SelfRestartHandoff) (RestartParentChild, error) { return child, nil },
		Confirm:   func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error { return nil },
		CloseHub:  func(context.Context) {},
		WaitGrace: func(context.Context, time.Duration) error { return nil },
		Exit:      func() {},
	})
	if err != nil {
		t.Fatalf("NewRestartCoordinator: %v", err)
	}
	active, err := coordinator.Start()
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-reserved:
	case <-time.After(time.Second):
		t.Fatal("handoff did not reach Reserve")
	}
	phaseDeadline := time.Now().Add(time.Second)
	for {
		coordinator.mu.Lock()
		activePhase := coordinator.active.Phase
		coordinator.mu.Unlock()
		if activePhase == HandoffPhaseReserved {
			break
		}
		if time.Now().After(phaseDeadline) {
			t.Fatalf("coordinator active phase = %q after Reserve, want %q", activePhase, HandoffPhaseReserved)
		}
		time.Sleep(time.Millisecond)
	}

	s := NewServer(Config{Port: 9125, RestartV3Enabled: true})
	s.restartCoordinator = coordinator
	response := httptest.NewRecorder()
	s.guiRestartV3Handler(response)
	active.AcknowledgeResponseFlushed()
	result := <-active.Done
	if result.Err != nil {
		t.Fatalf("active handoff completion: %v", result.Err)
	}

	var body guiSelfRestartResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode concurrent response: %v", err)
	}
	if response.Code != http.StatusAccepted || body.Phase != HandoffPhaseReserved {
		t.Fatalf("concurrent response = status %d phase %q, want 202/%q", response.Code, body.Phase, HandoffPhaseReserved)
	}
}

func TestRestartV3_SpawnFailureReturns2xxNonRestartingBody(t *testing.T) {
	coordinator := &phaseGRestartStarter{err: errors.New("retained handle spawn failed")}
	s := NewServer(Config{Port: 9125, RestartV3Enabled: true})
	s.restartCoordinator = coordinator

	req := httptest.NewRequest(http.MethodPost, "/api/gui/restart", nil)
	req.Header.Set("Origin", "http://127.0.0.1:9125")
	response := httptest.NewRecorder()
	s.guiSelfRestartHandler(response, req)

	if response.Code < 200 || response.Code >= 300 || response.Code != http.StatusOK {
		t.Fatalf("status = %d, want frontend-readable 200/2xx", response.Code)
	}
	var body guiSelfRestartResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Spawned || body.SpawnedPID != 0 || body.Restarting || !strings.Contains(body.SpawnError, "retained handle spawn failed") {
		t.Fatalf("body = %+v, want non-restarting spawn failure", body)
	}
}

var errSelfRestartTest = errSelfRestart("boom")

type errSelfRestart string

func (e errSelfRestart) Error() string { return string(e) }

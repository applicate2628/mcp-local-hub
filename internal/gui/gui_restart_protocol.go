package gui

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
)

const (
	selfRestartHandoffVersion       = 1
	restartChildStandbyCloseTimeout = 2 * time.Second
	restartStandbyConfirmRetry      = 50 * time.Millisecond
)

var (
	ErrRestartChildStandbyExpired = errors.New("restart child standby deadline expired")
	ErrRestartChildMarkerMismatch = errors.New("restart child marker no longer matches its reservation")
	ErrRestartChildParentExited   = errors.New("restart child parent process exited during standby")
	ErrRestartAlreadyInProgress   = errors.New("GUI restart is already in progress")
)

// RestartParentChild is the parent's retained authority over exactly one
// spawned replacement process. Termination includes its bounded reap and is
// legal only before the parent releases the GUI lease. DetachAtRelease closes
// the retained local handle without terminating or waiting for the child.
type RestartParentChild interface {
	PID() int
	TerminateBeforeRelease(context.Context) error
	DetachAtRelease() error
}

type RestartCoordinatorMarkerStore interface {
	Begin(HandoffBegin) (*HandoffMarkerRecord, error)
	Reserve(string, uint64, time.Time, string, int) (*HandoffMarkerRecord, error)
	Interrupt(string, string, string) (*HandoffMarkerRecord, error)
	ClearAfterProvedPreReleaseRollback(string) error
}

type RestartCoordinatorListener interface {
	EnterGrace(context.Context, http.Handler) error
	CloseListener(context.Context) error
	BindForRecovery(context.Context, int) (net.Listener, error)
	ServeFull(net.Listener, http.Handler) error
	RestoreFull(http.Handler) error
}

// RestartCoordinatorDependencies are the parent protocol's injected resource
// owners. CLI composition supplies argv/process and process-exit operations;
// Server supplies its listener, handler, event bus, and owned hub close.
type RestartCoordinatorDependencies struct {
	Context     context.Context
	StateDir    string
	OldPort     func() int
	TargetPort  func(int) (int, error)
	ParentPID   int
	Lease       SingleInstanceLease
	Listener    RestartCoordinatorListener
	FullHandler http.Handler
	MarkerStore RestartCoordinatorMarkerStore
	Deadlines   RestartDeadlines
	NewID       func() (string, error)
	NewNonce    func() ([]byte, error)
	WriteNonce  func(string, []byte) error
	RemoveNonce func(string) error
	Spawn       func(SelfRestartHandoff) (RestartParentChild, error)
	Confirm     func(context.Context, int, []byte, AuthenticatedReadinessIdentity) error
	Events      RestartChildEventPublisher
	CloseHub    func(context.Context)
	WaitGrace   func(context.Context, time.Duration) error
	Exit        func()
}

type RestartCoordinatorStart struct {
	HandoffID       string
	Generation      string
	Phase           HandoffPhase
	SpawnedPID      int
	OldPort         int
	TargetPort      int
	Done            <-chan RestartCoordinatorResult
	responseFlushed func()
}

// AcknowledgeResponseFlushed opens the coordinator's irreversible continuation
// only after the accepting HTTP handler has encoded its 202 body and attempted
// Flush when the ResponseWriter supports it. Copies share one sync.Once-backed
// acknowledgement closure.
func (s RestartCoordinatorStart) AcknowledgeResponseFlushed() {
	if s.responseFlushed != nil {
		s.responseFlushed()
	}
}

type RestartCoordinatorResult struct {
	Err                 error
	ParentLeaseReleased bool
}

// RestartCoordinator owns the parent half of one restart-v3 generation.
// Start persists in-progress and spawns before returning the accepted response;
// the background continuation crosses one irreversible lease-release boundary.
type RestartCoordinator struct {
	deps           RestartCoordinatorDependencies
	mu             sync.Mutex
	run            bool
	active         RestartCoordinatorStart
	runReady       chan struct{}
	runReadyClosed bool
}

func NewRestartCoordinator(deps RestartCoordinatorDependencies) (*RestartCoordinator, error) {
	if err := validateRestartCoordinatorDependencies(deps); err != nil {
		return nil, err
	}
	if deps.NewID == nil {
		deps.NewID = newRestartCoordinatorID
	}
	if deps.NewNonce == nil {
		deps.NewNonce = newRestartCoordinatorNonce
	}
	if deps.WriteNonce == nil {
		deps.WriteNonce = api.WriteStateFileBytesAtomic
	}
	if deps.RemoveNonce == nil {
		deps.RemoveNonce = os.Remove
	}
	if deps.WaitGrace == nil {
		deps.WaitGrace = waitRestartCoordinatorDuration
	}
	return &RestartCoordinator{deps: deps}, nil
}

func (c *RestartCoordinator) Start() (RestartCoordinatorStart, error) {
	c.mu.Lock()
	if c.run {
		active := c.active
		ready := c.runReady
		c.mu.Unlock()
		if active.HandoffID == "" && ready != nil {
			select {
			case <-ready:
			case <-c.deps.Context.Done():
				return RestartCoordinatorStart{}, c.deps.Context.Err()
			}
			c.mu.Lock()
			if !c.run {
				c.mu.Unlock()
				return c.Start()
			}
			active = c.active
			c.mu.Unlock()
		}
		return active, ErrRestartAlreadyInProgress
	}
	c.run = true
	c.runReady = make(chan struct{})
	c.runReadyClosed = false
	c.mu.Unlock()

	oldPort := c.deps.OldPort()
	targetPort, err := c.deps.TargetPort(oldPort)
	if err != nil {
		c.resetBeforeSpawn()
		return RestartCoordinatorStart{}, err
	}
	route := HandoffRoutePortChange
	if targetPort == oldPort {
		route = HandoffRouteSamePort
	}
	handoffID, err := c.deps.NewID()
	if err != nil {
		c.resetBeforeSpawn()
		return RestartCoordinatorStart{}, fmt.Errorf("create restart handoff id: %w", err)
	}
	generation, err := c.deps.NewID()
	if err != nil {
		c.resetBeforeSpawn()
		return RestartCoordinatorStart{}, fmt.Errorf("create restart generation: %w", err)
	}
	if err := sweepRestartNonceResidue(c.deps.StateDir); err != nil {
		c.resetBeforeSpawn()
		return RestartCoordinatorStart{}, err
	}
	record, err := c.deps.MarkerStore.Begin(HandoffBegin{
		Generation: generation,
		Route:      route,
		OldPort:    oldPort,
		NewPort:    targetPort,
		OldPID:     c.deps.ParentPID,
	})
	if err != nil {
		c.resetBeforeSpawn()
		return RestartCoordinatorStart{}, err
	}
	nonce, err := c.deps.NewNonce()
	if err != nil {
		return RestartCoordinatorStart{}, c.failAfterBegin(generation, "", nil, fmt.Errorf("create restart nonce: %w", err))
	}
	if len(nonce) != authenticatedReadinessNonceBytes {
		zeroBytes(nonce)
		return RestartCoordinatorStart{}, c.failAfterBegin(generation, "", nil, fmt.Errorf("restart nonce length = %d, want %d", len(nonce), authenticatedReadinessNonceBytes))
	}
	noncePath := restartNoncePath(c.deps.StateDir, generation)
	if err := c.deps.WriteNonce(noncePath, nonce); err != nil {
		zeroBytes(nonce)
		return RestartCoordinatorStart{}, c.failAfterBegin(generation, noncePath, nil, fmt.Errorf("write restart nonce: %w", err))
	}
	handoff := SelfRestartHandoff{
		Version:    selfRestartHandoffVersion,
		HandoffID:  handoffID,
		Generation: generation,
		Sequence:   record.Sequence,
		OldPort:    oldPort,
		TargetPort: targetPort,
		ParentPID:  c.deps.ParentPID,
		NoncePath:  noncePath,
	}
	child, err := c.deps.Spawn(handoff)
	if err != nil {
		zeroBytes(nonce)
		return RestartCoordinatorStart{}, c.failAfterBegin(generation, noncePath, child, err)
	}
	if child == nil || child.PID() <= 0 {
		zeroBytes(nonce)
		return RestartCoordinatorStart{}, c.failAfterBegin(generation, noncePath, child, errors.New("restart spawn returned no retained child"))
	}

	done := make(chan RestartCoordinatorResult, 1)
	responseFlushed := make(chan struct{})
	var acknowledgeResponse sync.Once
	start := RestartCoordinatorStart{
		HandoffID: handoffID, Generation: generation, Phase: HandoffPhaseInProgress,
		SpawnedPID: child.PID(), OldPort: oldPort, TargetPort: targetPort, Done: done,
		responseFlushed: func() { acknowledgeResponse.Do(func() { close(responseFlushed) }) },
	}
	c.mu.Lock()
	c.active = start
	c.signalRunReadyLocked()
	c.mu.Unlock()
	c.publishProgress(start, HandoffPhaseInProgress, "")
	go c.continueHandoff(start, record, noncePath, nonce, child, responseFlushed, done)
	return start, nil
}

func (c *RestartCoordinator) resetBeforeSpawn() {
	c.mu.Lock()
	c.signalRunReadyLocked()
	c.run = false
	c.active = RestartCoordinatorStart{}
	c.runReady = nil
	c.mu.Unlock()
}

func (c *RestartCoordinator) signalRunReady() {
	c.mu.Lock()
	c.signalRunReadyLocked()
	c.mu.Unlock()
}

func (c *RestartCoordinator) signalRunReadyLocked() {
	if c.runReady != nil && !c.runReadyClosed {
		close(c.runReady)
		c.runReadyClosed = true
	}
}

func (c *RestartCoordinator) failAfterBegin(generation, noncePath string, child RestartParentChild, cause error) error {
	var handleErr error
	if child != nil {
		handleErr = child.DetachAtRelease()
	}
	nonceErr := c.removeNonce(noncePath)
	markerErr := c.deps.MarkerStore.ClearAfterProvedPreReleaseRollback(generation)
	if markerErr == nil {
		c.resetBeforeSpawn()
		return errors.Join(
			cause,
			wrapRestartCleanupError("close retained child handle", handleErr),
			wrapRestartCleanupError("remove restart nonce", nonceErr),
		)
	}

	_, terminalErr := c.deps.MarkerStore.Interrupt(
		generation,
		"gui-restart-pre-accept-cleanup-failed",
		"mcphub gui",
	)
	// Deliberately DO NOT reset the in-memory guard here: the marker cleanup
	// was unproved (the Clear failed), so the residue state is uncertain. The
	// guard is retained as a fail-safe so no further restart is attempted on
	// uncertain state until a GUI relaunch resets everything — codified by
	// TestRestartV3_PostBeginCleanupFailureTerminalizesMarkerBeforeRunReset.
	// (Contrast the proved-clean arm above, which resets.)
	c.signalRunReady()
	return errors.Join(
		cause,
		wrapRestartCleanupError("close retained child handle", handleErr),
		wrapRestartCleanupError("remove restart nonce", nonceErr),
		wrapRestartCleanupError("remove in-progress restart marker", markerErr),
		wrapRestartCleanupError("write interrupted restart marker", terminalErr),
	)
}

func wrapRestartCleanupError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func (c *RestartCoordinator) continueHandoff(start RestartCoordinatorStart, record *HandoffMarkerRecord, noncePath string, nonce []byte, child RestartParentChild, responseFlushed <-chan struct{}, done chan<- RestartCoordinatorResult) {
	parentLeaseReleased := false
	listenerClosed := false
	grace := newRestartGraceHandler(start.HandoffID, start.TargetPort, c.deps.FullHandler)
	finish := func(result RestartCoordinatorResult) {
		done <- result
		close(done)
	}

	confirm := func() error {
		confirmCtx, cancel := context.WithTimeout(c.deps.Context, c.deps.Deadlines.Bind)
		defer cancel()
		identity := AuthenticatedReadinessIdentity{
			HandoffID: start.HandoffID, Generation: start.Generation, Sequence: record.Sequence,
			PID: child.PID(), Port: start.TargetPort,
		}
		for {
			err := c.deps.Confirm(confirmCtx, start.TargetPort, nonce, identity)
			if err == nil {
				return nil
			}
			if !isTransientRestartStandbyConfirmError(err) {
				return err
			}
			if waitErr := waitRestartCoordinatorDuration(confirmCtx, restartStandbyConfirmRetry); waitErr != nil {
				return errors.Join(err, waitErr)
			}
		}
	}

	var prepareErr error
	if start.TargetPort == start.OldPort {
		quiesceCtx, cancel := context.WithTimeout(c.deps.Context, c.deps.Deadlines.Quiesce)
		prepareErr = c.deps.Listener.EnterGrace(quiesceCtx, grace)
		cancel()
		if prepareErr == nil {
			closeCtx, cancelClose := context.WithTimeout(c.deps.Context, c.deps.Deadlines.Bind)
			prepareErr = c.deps.Listener.CloseListener(closeCtx)
			cancelClose()
			listenerClosed = restartListenerPhysicallyClosed(prepareErr)
		}
		if prepareErr == nil {
			prepareErr = confirm()
		}
	} else {
		prepareErr = confirm()
		if prepareErr == nil {
			quiesceCtx, cancel := context.WithTimeout(c.deps.Context, c.deps.Deadlines.Quiesce)
			prepareErr = c.deps.Listener.EnterGrace(quiesceCtx, grace)
			cancel()
		}
	}
	if prepareErr != nil {
		zeroBytes(nonce)
		finish(c.rollbackBeforeRelease(start, noncePath, child, listenerClosed, parentLeaseReleased, prepareErr))
		return
	}
	if err := c.removeNonce(noncePath); err != nil {
		zeroBytes(nonce)
		finish(c.rollbackBeforeRelease(start, noncePath, child, listenerClosed, parentLeaseReleased, fmt.Errorf("remove confirmed restart nonce: %w", err)))
		return
	}

	now := c.deps.Deadlines.Now().UTC()
	reserved, err := c.deps.MarkerStore.Reserve(
		start.Generation,
		record.Sequence,
		now.Add(c.deps.Deadlines.Reservation),
		hashDesignatedChildNonce(nonce),
		child.PID(),
	)
	if err != nil {
		zeroBytes(nonce)
		finish(c.rollbackBeforeRelease(start, noncePath, child, listenerClosed, parentLeaseReleased, err))
		return
	}
	c.publishProgress(start, HandoffPhaseReserved, "")
	select {
	case <-responseFlushed:
	case <-c.deps.Context.Done():
		zeroBytes(nonce)
		finish(c.rollbackBeforeRelease(start, noncePath, child, listenerClosed, parentLeaseReleased, c.deps.Context.Err()))
		return
	}

	// The parent's own hub close and lease release are one ordered boundary.
	// No protocol decision is made after Release returns.
	hubCtx, cancelHub := context.WithTimeout(c.deps.Context, 5*time.Second)
	c.deps.CloseHub(hubCtx)
	cancelHub()
	c.deps.Lease.Release()
	parentLeaseReleased = true
	grace.released.Store(true)
	zeroBytes(nonce)
	_ = child.DetachAtRelease()

	if start.TargetPort != start.OldPort {
		_ = c.deps.WaitGrace(c.deps.Context, c.deps.Deadlines.Grace)
		closeCtx, cancelClose := context.WithTimeout(context.Background(), c.deps.Deadlines.Grace)
		_ = c.deps.Listener.CloseListener(closeCtx)
		cancelClose()
	}
	c.deps.Exit()
	finish(RestartCoordinatorResult{ParentLeaseReleased: parentLeaseReleased, Err: reservedResultError(reserved)})
}

func reservedResultError(record *HandoffMarkerRecord) error {
	if record == nil || record.Phase != HandoffPhaseReserved {
		return errors.New("restart reservation returned an invalid record")
	}
	return nil
}

func (c *RestartCoordinator) rollbackBeforeRelease(start RestartCoordinatorStart, noncePath string, child RestartParentChild, listenerClosed, parentLeaseReleased bool, cause error) RestartCoordinatorResult {
	if parentLeaseReleased {
		return RestartCoordinatorResult{Err: cause, ParentLeaseReleased: true}
	}
	rollbackCtx, cancelRollback := context.WithTimeout(c.deps.Context, c.deps.Deadlines.Rollback)
	defer cancelRollback()
	var cleanupErr error
	if child != nil {
		cleanupErr = child.TerminateBeforeRelease(rollbackCtx)
	}
	cleanupErr = errors.Join(cleanupErr, c.removeNonce(noncePath))
	var restoreErr error
	if start.TargetPort == start.OldPort && listenerClosed {
		var rebound net.Listener
		rebound, restoreErr = c.deps.Listener.BindForRecovery(rollbackCtx, start.OldPort)
		if restoreErr == nil {
			restoreErr = c.deps.Listener.ServeFull(rebound, c.deps.FullHandler)
		}
	} else {
		restoreErr = c.deps.Listener.RestoreFull(c.deps.FullHandler)
	}
	if cleanupErr == nil && restoreErr == nil {
		if err := c.deps.MarkerStore.ClearAfterProvedPreReleaseRollback(start.Generation); err == nil {
			c.resetBeforeSpawn()
			c.publishProgress(start, HandoffPhaseInterrupted, "gui-restart-pre-release-rollback")
			return RestartCoordinatorResult{Err: cause}
		} else {
			restoreErr = err
		}
	}

	terminalErr := c.interruptRollbackFailure(start, errors.Join(cause, cleanupErr, restoreErr))
	_ = child.DetachAtRelease()
	hubCtx, cancelHub := context.WithTimeout(context.Background(), 5*time.Second)
	c.deps.CloseHub(hubCtx)
	cancelHub()
	c.deps.Lease.Release()
	parentLeaseReleased = true
	c.deps.Exit()
	return RestartCoordinatorResult{Err: terminalErr, ParentLeaseReleased: parentLeaseReleased}
}

func (c *RestartCoordinator) interruptRollbackFailure(start RestartCoordinatorStart, cause error) error {
	_, err := c.deps.MarkerStore.Interrupt(start.Generation, "gui-restart-pre-release-rollback-failed", "mcphub gui")
	eventType := "gui-restart-pre-release-rollback-failed"
	if err != nil {
		eventType = "gui-restart-interrupted-marker-write-failed"
	}
	c.publishProgress(start, HandoffPhaseInterrupted, eventType)
	return errors.Join(cause, err)
}

func (c *RestartCoordinator) publishProgress(start RestartCoordinatorStart, phase HandoffPhase, reasonCode string) {
	c.mu.Lock()
	if c.run && c.active.Generation == start.Generation {
		c.active.Phase = phase
	}
	c.mu.Unlock()
	if c.deps.Events == nil {
		return
	}
	body := map[string]any{
		"handoff_id": start.HandoffID, "generation": start.Generation, "phase": phase,
		"old_port": start.OldPort, "same_port": start.OldPort == start.TargetPort,
	}
	if start.TargetPort != start.OldPort {
		body["new_port"] = start.TargetPort
	}
	if reasonCode != "" {
		body["reason_code"] = reasonCode
	}
	c.deps.Events.Publish(Event{Type: "gui-restart-progress", Body: body})
}

type restartGraceHandler struct {
	handoffID string
	targetURL string
	full      http.Handler
	released  atomic.Bool
}

func newRestartGraceHandler(handoffID string, targetPort int, full http.Handler) *restartGraceHandler {
	return &restartGraceHandler{
		handoffID: handoffID,
		targetURL: fmt.Sprintf("http://127.0.0.1:%d/", targetPort),
		full:      full,
	}
}

func (h *restartGraceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/api/events" {
		h.full.ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/gui/restart/redirect" && r.URL.Query().Get("handoff_id") == h.handoffID {
		w.Header().Set("Content-Type", "application/json")
		if !h.released.Load() {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"released": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"released": true, "target_url": h.targetURL})
		return
	}
	http.Error(w, "GUI_RESTART_IN_PROGRESS", http.StatusServiceUnavailable)
}

func (o *GUIListenerOwner) RestoreFull(fullHandler http.Handler) error {
	if o == nil {
		return errors.New("restore full GUI listener: nil owner")
	}
	o.mu.Lock()
	generation := o.current
	o.mu.Unlock()
	if generation == nil || generation.listener == nil {
		return errors.New("restore full GUI listener: no owned listener")
	}
	return o.ServeFull(generation.listener, fullHandler)
}

func validateRestartCoordinatorDependencies(deps RestartCoordinatorDependencies) error {
	if deps.Context == nil || deps.OldPort == nil || deps.TargetPort == nil || deps.ParentPID <= 0 {
		return errors.New("restart coordinator context, ports, and parent PID are required")
	}
	if strings.TrimSpace(deps.StateDir) == "" || deps.Lease == nil || deps.Listener == nil || deps.FullHandler == nil || deps.MarkerStore == nil {
		return errors.New("restart coordinator resource ownership is incomplete")
	}
	if deps.Spawn == nil || deps.Confirm == nil || deps.CloseHub == nil || deps.Exit == nil || deps.Deadlines.Now == nil {
		return errors.New("restart coordinator operation seams are incomplete")
	}
	for name, value := range map[string]time.Duration{
		"bind": deps.Deadlines.Bind, "quiesce": deps.Deadlines.Quiesce, "reservation": deps.Deadlines.Reservation,
		"rollback": deps.Deadlines.Rollback, "grace": deps.Deadlines.Grace,
	} {
		if value <= 0 {
			return fmt.Errorf("restart coordinator %s deadline must be positive", name)
		}
	}
	return nil
}

func newRestartCoordinatorID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func newRestartCoordinatorNonce() ([]byte, error) {
	nonce := make([]byte, authenticatedReadinessNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return nonce, nil
}

func waitRestartCoordinatorDuration(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *RestartCoordinator) removeNonce(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := c.deps.RemoveNonce(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove restart nonce %q: %w", path, err)
	}
	return nil
}

func sweepRestartNonceResidue(stateDir string) error {
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		return fmt.Errorf("read restart nonce state directory %q: %w", stateDir, err)
	}
	prefix := api.GUIRestartNonceFileLeaf + "-"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		path := filepath.Join(stateDir, entry.Name())
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale restart nonce residue %q: %w", path, err)
		}
	}
	return nil
}

func restartListenerPhysicallyClosed(err error) bool {
	if err == nil {
		return true
	}
	var state interface {
		ListenerPhysicallyClosed() bool
	}
	return errors.As(err, &state) && state.ListenerPhysicallyClosed()
}

// isTransientRestartStandbyConfirmError admits retry only for loopback network
// readiness failures. HTTP status, response decoding, identity, and message-
// authentication failures are hard protocol failures and never satisfy
// net.Error, so they fail immediately. The caller's Bind context is the single
// total timeout for all attempts and waits.
func isTransientRestartStandbyConfirmError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

// ConfirmAuthenticatedStandby performs one exact, non-redirecting loopback
// challenge against the retained child's standby listener. The caller owns
// the retry/deadline policy through ctx; this probe never follows a child-
// supplied host or URL.
func ConfirmAuthenticatedStandby(ctx context.Context, port int, nonce []byte, expected AuthenticatedReadinessIdentity) error {
	if ctx == nil {
		return errors.New("confirm restart standby: nil context")
	}
	if err := validateAuthenticatedReadinessIdentity(expected); err != nil {
		return fmt.Errorf("confirm restart standby identity: %w", err)
	}
	if expected.Port != port {
		return fmt.Errorf("confirm restart standby port %d does not match identity port %d", port, expected.Port)
	}
	if len(nonce) != authenticatedReadinessNonceBytes {
		return fmt.Errorf("confirm restart standby nonce length = %d, want %d", len(nonce), authenticatedReadinessNonceBytes)
	}
	challenge, err := newRestartCoordinatorID()
	if err != nil {
		return fmt.Errorf("create restart readiness challenge: %w", err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/api/ping?challenge=%s", port, challenge)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create restart readiness request: %w", err)
	}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("probe restart standby: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("probe restart standby status = %d, want 200", response.StatusCode)
	}
	var proof AuthenticatedReadinessProof
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8192))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proof); err != nil {
		return fmt.Errorf("decode restart standby proof: %w", err)
	}
	if !VerifyAuthenticatedReadiness(nonce, challenge, proof, expected) {
		return errors.New("restart standby authentication failed")
	}
	return nil
}

// SelfRestartHandoff is the non-secret child descriptor carried by
// SelfRestartHandoffEnv. NoncePath names a one-shot owner-only file; the nonce
// value is deliberately absent from this environment wire shape.
type SelfRestartHandoff struct {
	Version    int    `json:"version"`
	HandoffID  string `json:"handoff_id"`
	Generation string `json:"generation"`
	Sequence   uint64 `json:"sequence"`
	OldPort    int    `json:"old_port"`
	TargetPort int    `json:"target_port"`
	ParentPID  int    `json:"parent_pid"`
	NoncePath  string `json:"nonce_path"`
}

// EncodeSelfRestartHandoff encodes only the non-secret descriptor. The parent
// half creates the hardened nonce file separately and places only its path in
// this payload.
func EncodeSelfRestartHandoff(handoff SelfRestartHandoff) (string, error) {
	if err := validateSelfRestartHandoff(handoff); err != nil {
		return "", err
	}
	raw, err := json.Marshal(handoff)
	if err != nil {
		return "", fmt.Errorf("encode restart child handoff: %w", err)
	}
	return string(raw), nil
}

// DecodeSelfRestartHandoff validates the structured child descriptor before
// CLI composition uses its authoritative target port.
func DecodeSelfRestartHandoff(raw string) (SelfRestartHandoff, error) {
	return decodeSelfRestartHandoff(raw)
}

func decodeSelfRestartHandoff(raw string) (SelfRestartHandoff, error) {
	var handoff SelfRestartHandoff
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&handoff); err != nil {
		return SelfRestartHandoff{}, fmt.Errorf("decode restart child handoff: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return SelfRestartHandoff{}, fmt.Errorf("decode restart child handoff: %w", err)
	}
	if err := validateSelfRestartHandoff(handoff); err != nil {
		return SelfRestartHandoff{}, err
	}
	return handoff, nil
}

func validateSelfRestartHandoff(handoff SelfRestartHandoff) error {
	if handoff.Version != selfRestartHandoffVersion {
		return fmt.Errorf("restart child handoff version = %d, want %d", handoff.Version, selfRestartHandoffVersion)
	}
	if strings.TrimSpace(handoff.HandoffID) == "" || strings.TrimSpace(handoff.Generation) == "" {
		return errors.New("restart child handoff id and generation are required")
	}
	if handoff.Sequence == 0 {
		return errors.New("restart child handoff sequence must be positive")
	}
	if handoff.OldPort < 1024 || handoff.OldPort > 65535 || handoff.TargetPort < 1024 || handoff.TargetPort > 65535 {
		return fmt.Errorf("restart child ports old=%d target=%d must be within [1024,65535]", handoff.OldPort, handoff.TargetPort)
	}
	if handoff.ParentPID <= 0 {
		return errors.New("restart child parent PID must be positive")
	}
	if strings.TrimSpace(handoff.NoncePath) == "" || !filepath.IsAbs(handoff.NoncePath) {
		return errors.New("restart child nonce path must be absolute")
	}
	return nil
}

// SpawnedGUIChild is the child-side view of one parent-created restart
// generation. It owns the consumed in-memory readiness nonce until activation.
type SpawnedGUIChild struct {
	Handoff     SelfRestartHandoff
	PID         int
	Readiness   *AuthenticatedReadinessSession
	parentDeath restartParentDeathWatcher
}

// NewSpawnedGUIChildFromEnvironment consumes the one-shot hardened nonce file,
// removes it before serving readiness, and never copies the nonce into argv,
// environment, logs, or durable marker state.
func NewSpawnedGUIChildFromEnvironment(raw string, pid int, stateDir string) (*SpawnedGUIChild, error) {
	handoff, err := decodeSelfRestartHandoff(raw)
	if err != nil {
		return nil, err
	}
	if pid <= 0 {
		return nil, errors.New("restart child PID must be positive")
	}
	noncePath, err := canonicalRestartNoncePath(stateDir, handoff.Generation, handoff.NoncePath)
	if err != nil {
		return nil, err
	}
	handoff.NoncePath = noncePath
	nonce, err := api.ConsumeStateSecretFileInodeAnchored(handoff.NoncePath, authenticatedReadinessNonceBytes)
	if err != nil {
		return nil, fmt.Errorf("consume restart child nonce file: %w", err)
	}
	defer zeroBytes(nonce)
	if len(nonce) != authenticatedReadinessNonceBytes {
		return nil, fmt.Errorf("restart child nonce file length = %d, want %d bytes", len(nonce), authenticatedReadinessNonceBytes)
	}
	readiness, err := NewAuthenticatedReadinessSession(AuthenticatedReadinessIdentity{
		HandoffID:  handoff.HandoffID,
		Generation: handoff.Generation,
		Sequence:   handoff.Sequence,
		PID:        pid,
		Port:       handoff.TargetPort,
	}, nonce)
	if err != nil {
		return nil, err
	}
	parentDeath, err := newRestartParentDeathWatcher(handoff.ParentPID)
	if err != nil {
		readiness.Close()
		return nil, fmt.Errorf("retain restart parent process: %w", err)
	}
	return &SpawnedGUIChild{Handoff: handoff, PID: pid, Readiness: readiness, parentDeath: parentDeath}, nil
}

func restartNonceFileLeaf(generation string) string {
	sum := sha256.Sum256([]byte(generation))
	return api.GUIRestartNonceFileLeaf + "-" + hex.EncodeToString(sum[:])
}

func restartNoncePath(stateDir, generation string) string {
	return filepath.Join(stateDir, restartNonceFileLeaf(generation))
}

func canonicalRestartNoncePath(stateDir, generation, supplied string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return "", errors.New("restart child canonical state directory is required")
	}
	stateAbs, err := filepath.Abs(filepath.Clean(stateDir))
	if err != nil {
		return "", fmt.Errorf("resolve restart child canonical state directory: %w", err)
	}
	nonceAbs, err := filepath.Abs(filepath.Clean(supplied))
	if err != nil {
		return "", fmt.Errorf("resolve restart child nonce path: %w", err)
	}
	wantLeaf := restartNonceFileLeaf(generation)
	rel, err := filepath.Rel(stateAbs, nonceAbs)
	if err != nil || rel != wantLeaf {
		return "", fmt.Errorf("restart child nonce path %q is not the canonical state directory generation leaf %q", supplied, filepath.Join(stateAbs, wantLeaf))
	}
	return filepath.Join(stateAbs, wantLeaf), nil
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

// Close erases the readiness nonce. It does not own an activated runtime's
// flock or listener; those resources transfer through the activation barrier.
func (c *SpawnedGUIChild) Close() {
	if c == nil {
		return
	}
	if c.parentDeath != nil {
		_ = c.parentDeath.Close()
	}
	if c.Readiness != nil {
		c.Readiness.Close()
	}
}

// AcquireSingleInstanceAt passes a short-lived nonce copy directly into the
// reservation-aware acquire. The copy is erased on return and never crosses an
// argv, environment, logging, or durable-state boundary.
func (c *SpawnedGUIChild) AcquireSingleInstanceAt(pidportPath string, port int, markerStore HandoffMarkerReader, deadlines RestartDeadlines) (*SingleInstanceLock, error) {
	if c == nil || c.Readiness == nil {
		return nil, errors.New("restart child readiness session is unavailable")
	}
	nonce, err := c.Readiness.nonceCopy()
	if err != nil {
		return nil, err
	}
	defer zeroBytes(nonce)
	return AcquireSingleInstanceAt(pidportPath, port, SingleInstanceAcquireOptions{
		RestartV3Enabled:     true,
		MarkerStore:          markerStore,
		DesignatedChildNonce: nonce,
		Deadlines:            deadlines,
	})
}

type RestartChildMarkerStore interface {
	Read() (*HandoffMarkerRecord, error)
	Commit(generation string, boundPort int) (*HandoffMarkerRecord, error)
	Interrupt(generation, reasonCode, operatorAction string) (*HandoffMarkerRecord, error)
}

type RestartChildStandby interface {
	CloseListener(context.Context) error
}

type RestartChildEventPublisher interface {
	Publish(Event)
}

// RestartChildRuntimeSettlement is the single linearization owner for marker
// publication versus runtime stop. WhileAlive holds the settlement lock across
// one marker write; Stop cannot publish runtime death until that write has
// completed, and no later write can begin after Stop.
type RestartChildRuntimeSettlement struct {
	mu      sync.Mutex
	done    chan struct{}
	stopped bool
	err     error
}

func NewRestartChildRuntimeSettlement() *RestartChildRuntimeSettlement {
	return &RestartChildRuntimeSettlement{done: make(chan struct{})}
}

func (s *RestartChildRuntimeSettlement) Stop(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	s.stopped = true
	s.err = err
	close(s.done)
}

func (s *RestartChildRuntimeSettlement) Done() <-chan struct{} {
	if s == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return s.done
}

func (s *RestartChildRuntimeSettlement) Err() error {
	if s == nil {
		return errors.New("restart child runtime settlement is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *RestartChildRuntimeSettlement) WhileAlive(ctx context.Context, write func() error) error {
	if s == nil || write == nil {
		return errors.New("restart child runtime settlement is incomplete")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.stopped {
		if s.err != nil {
			return s.err
		}
		return errors.New("restart child runtime stopped during marker settlement")
	}
	return write()
}

// RestartChildDependencies are the child protocol's resource seams. Activate
// is the single barrier: on nil it owns the lease for the full runtime; on
// error the protocol releases the lease and closes standby.
type RestartChildDependencies struct {
	Acquire       func(context.Context) (SingleInstanceLease, error)
	MarkerStore   RestartChildMarkerStore
	Standby       RestartChildStandby
	Activate      func(context.Context, SingleInstanceLease) error
	Events        RestartChildEventPublisher
	Runtime       *RestartChildRuntimeSettlement
	Deadlines     RestartDeadlines
	RetryInterval time.Duration
	Wait          func(context.Context, time.Duration) error
}

type RestartChildResult struct {
	Activated bool
	CommitErr error
}

// Run waits only for the reservation-aware flock acquisition. A matching
// reservation activates immediately; no parent signal or hub state is an
// activation input. A successful Activate call takes ownership of the lease.
func (c *SpawnedGUIChild) Run(ctx context.Context, deps RestartChildDependencies) (RestartChildResult, error) {
	if err := validateRestartChildDependencies(c, ctx, deps); err != nil {
		return RestartChildResult{}, err
	}
	now := deps.Deadlines.Now().UTC()
	standbyExpiresAt := now.Add(deps.Deadlines.Proof)
	standbyCtx, cancelStandby := context.WithCancelCause(ctx)
	parentWatchDone := make(chan struct{})
	go func() {
		defer close(parentWatchDone)
		select {
		case <-c.parentDeath.Done():
			cancelStandby(ErrRestartChildParentExited)
		case <-standbyCtx.Done():
		}
	}()
	var stopStandbyWatch sync.Once
	stopWatchingParent := func() {
		stopStandbyWatch.Do(func() {
			cancelStandby(context.Canceled)
			<-parentWatchDone
			_ = c.parentDeath.Close()
		})
	}
	defer stopWatchingParent()

	var lease SingleInstanceLease
	for {
		if err := context.Cause(standbyCtx); err != nil {
			closeRestartChildStandby(deps)
			return RestartChildResult{}, err
		}
		acquired, err := deps.Acquire(standbyCtx)
		if err == nil {
			if acquired == nil {
				closeRestartChildStandby(deps)
				return RestartChildResult{}, errors.New("restart child acquire returned a nil lease")
			}
			lease = acquired
			if err := context.Cause(standbyCtx); err != nil {
				lease.Release()
				closeRestartChildStandby(deps)
				return RestartChildResult{}, err
			}
			stopWatchingParent()
			break
		}
		if !errors.Is(err, ErrSingleInstanceBusy) {
			closeRestartChildStandby(deps)
			return RestartChildResult{}, err
		}
		now = deps.Deadlines.Now().UTC()
		if !now.Before(standbyExpiresAt) {
			closeRestartChildStandby(deps)
			return RestartChildResult{}, ErrRestartChildStandbyExpired
		}
		wait := deps.RetryInterval
		if remaining := standbyExpiresAt.Sub(now); wait > remaining {
			wait = remaining
		}
		if err := deps.Wait(standbyCtx, wait); err != nil {
			closeRestartChildStandby(deps)
			if cause := context.Cause(standbyCtx); cause != nil {
				return RestartChildResult{}, cause
			}
			return RestartChildResult{}, err
		}
	}

	record, err := deps.MarkerStore.Read()
	if err != nil {
		lease.Release()
		closeRestartChildStandby(deps)
		return RestartChildResult{}, err
	}
	now = deps.Deadlines.Now().UTC()
	if !c.matchesReservedMarker(record, now) {
		lease.Release()
		closeRestartChildStandby(deps)
		return RestartChildResult{}, ErrRestartChildMarkerMismatch
	}

	deps.Events.Publish(Event{Type: "gui-restart-lock-acquired", Body: c.eventBody(HandoffPhaseReserved, "gui-restart-lock-acquired")})
	if err := deps.Activate(ctx, lease); err != nil {
		lease.Release()
		closeRestartChildStandby(deps)
		return RestartChildResult{}, err
	}
	lease = nil // ownership transferred to the activated runtime
	c.Close()

	lastErr := error(nil)
	for {
		if err := ctx.Err(); err != nil {
			return RestartChildResult{Activated: true, CommitErr: lastErr}, err
		}
		if stopped, runtimeErr := restartChildRuntimeStopped(deps.Runtime); stopped {
			return RestartChildResult{Activated: true, CommitErr: lastErr}, runtimeErr
		}
		var committed *HandoffMarkerRecord
		err := deps.Runtime.WhileAlive(ctx, func() error {
			var commitErr error
			committed, commitErr = deps.MarkerStore.Commit(c.Handoff.Generation, c.Handoff.TargetPort)
			return commitErr
		})
		if err == nil {
			body := c.eventBody(HandoffPhaseCommitted, "committed")
			body["sequence"] = committed.Sequence
			deps.Events.Publish(Event{Type: "gui-restart-progress", Body: body})
			return RestartChildResult{Activated: true}, nil
		}
		lastErr = err
		if errors.Is(err, ErrHandoffMarkerCASMismatch) || errors.Is(err, ErrHandoffMarkerStateMismatch) {
			return RestartChildResult{Activated: true, CommitErr: lastErr}, err
		}
		if stopped, runtimeErr := restartChildRuntimeStopped(deps.Runtime); stopped {
			return RestartChildResult{Activated: true, CommitErr: lastErr}, runtimeErr
		}
		now = deps.Deadlines.Now().UTC()
		if !now.Before(record.ReservationExpiresAt) || ctx.Err() != nil {
			break
		}
		wait := deps.RetryInterval
		if remaining := record.ReservationExpiresAt.Sub(now); wait > remaining {
			wait = remaining
		}
		if err := deps.Wait(ctx, wait); err != nil {
			lastErr = errors.Join(lastErr, err)
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return RestartChildResult{Activated: true, CommitErr: lastErr}, err
	}
	if stopped, runtimeErr := restartChildRuntimeStopped(deps.Runtime); stopped {
		return RestartChildResult{Activated: true, CommitErr: lastErr}, runtimeErr
	}
	// Exhaustion settles to interrupted, not reserved. Phase I recovery admits
	// only expired in-progress/reserved markers, so a healthy activated child
	// that still owns the flock cannot be mislabeled as a wedged holder.
	var terminal *HandoffMarkerRecord
	terminalErr := deps.Runtime.WhileAlive(ctx, func() error {
		var err error
		terminal, err = deps.MarkerStore.Interrupt(
			c.Handoff.Generation,
			"gui-restart-commit-write-failed",
			"mcphub gui",
		)
		return err
	})
	body := c.eventBody(HandoffPhaseInterrupted, "gui-restart-commit-write-failed")
	body["error"] = lastErr.Error()
	if terminal != nil {
		body["sequence"] = terminal.Sequence
	}
	deps.Events.Publish(Event{Type: "gui-restart-commit-write-failed", Body: body})
	if terminalErr != nil {
		return RestartChildResult{Activated: true, CommitErr: lastErr}, fmt.Errorf("persist commit-failure terminal marker: %w", terminalErr)
	}
	return RestartChildResult{Activated: true, CommitErr: lastErr}, nil
}

func restartChildRuntimeStopped(runtime *RestartChildRuntimeSettlement) (bool, error) {
	select {
	case <-runtime.Done():
		runtimeErr := runtime.Err()
		if runtimeErr == nil {
			runtimeErr = errors.New("restart child runtime stopped during marker settlement")
		}
		return true, runtimeErr
	default:
		return false, nil
	}
}

func validateRestartChildDependencies(c *SpawnedGUIChild, ctx context.Context, deps RestartChildDependencies) error {
	if c == nil {
		return errors.New("restart child is nil")
	}
	if ctx == nil {
		return errors.New("restart child context is nil")
	}
	if err := validateSelfRestartHandoff(c.Handoff); err != nil {
		return err
	}
	if c.PID <= 0 || c.parentDeath == nil || deps.Acquire == nil || deps.MarkerStore == nil || deps.Standby == nil || deps.Activate == nil || deps.Events == nil || deps.Runtime == nil {
		return errors.New("restart child dependencies are incomplete")
	}
	if deps.Deadlines.Now == nil || deps.Deadlines.Proof <= 0 {
		return errors.New("restart child clock and proof deadline are required")
	}
	if deps.RetryInterval <= 0 || deps.Wait == nil {
		return errors.New("restart child retry interval and wait function are required")
	}
	return nil
}

func (c *SpawnedGUIChild) matchesReservedMarker(record *HandoffMarkerRecord, now time.Time) bool {
	return record != nil &&
		record.Phase == HandoffPhaseReserved &&
		record.Generation == c.Handoff.Generation &&
		record.Sequence == c.Handoff.Sequence+1 &&
		record.OldPort == c.Handoff.OldPort &&
		record.NewPort == c.Handoff.TargetPort &&
		record.OldPID == c.Handoff.ParentPID &&
		record.ChildPID == c.PID &&
		!record.ReservationExpiresAt.IsZero() &&
		now.Before(record.ReservationExpiresAt)
}

func (c *SpawnedGUIChild) eventBody(phase HandoffPhase, reasonCode string) map[string]any {
	return map[string]any{
		"handoff_id":  c.Handoff.HandoffID,
		"generation":  c.Handoff.Generation,
		"phase":       phase,
		"reason_code": reasonCode,
		"old_port":    c.Handoff.OldPort,
		"new_port":    c.Handoff.TargetPort,
		"same_port":   c.Handoff.OldPort == c.Handoff.TargetPort,
		"pid":         c.PID,
	}
}

func closeRestartChildStandby(deps RestartChildDependencies) {
	ctx, cancel := context.WithTimeout(context.Background(), restartChildStandbyCloseTimeout)
	defer cancel()
	_ = deps.Standby.CloseListener(ctx)
}

func waitRestartChild(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// DefaultRestartChildDependencies fills only the timing mechanics shared
// by the CLI composition root; resource owners remain explicitly injected.
func DefaultRestartChildDependencies(deadlines RestartDeadlines) RestartChildDependencies {
	return RestartChildDependencies{
		Deadlines:     deadlines,
		RetryInterval: 100 * time.Millisecond,
		Wait:          waitRestartChild,
	}
}

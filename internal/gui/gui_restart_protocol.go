package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

const (
	selfRestartHandoffVersion       = 1
	restartChildStandbyCloseTimeout = 2 * time.Second
)

var (
	ErrRestartChildStandbyExpired = errors.New("restart child standby deadline expired")
	ErrRestartChildMarkerMismatch = errors.New("restart child marker no longer matches its reservation")
	ErrRestartChildParentExited   = errors.New("restart child parent process exited during standby")
)

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
	noncePath, err := canonicalRestartNoncePath(stateDir, handoff.NoncePath)
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

func canonicalRestartNoncePath(stateDir, supplied string) (string, error) {
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
	rel, err := filepath.Rel(stateAbs, nonceAbs)
	if err != nil || rel != api.GUIRestartNonceFileLeaf {
		return "", fmt.Errorf("restart child nonce path %q is not the canonical state directory leaf %q", supplied, filepath.Join(stateAbs, api.GUIRestartNonceFileLeaf))
	}
	return filepath.Join(stateAbs, api.GUIRestartNonceFileLeaf), nil
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

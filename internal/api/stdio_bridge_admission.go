package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/mcpcompat/readinesswire"
	"mcp-local-hub/internal/process"
)

const provisionalStdioBridgeCleanupDeadline = 5 * time.Second

// FrozenStdioBridgeAdmissionRequest is the exact supervisor descriptor the
// apply path will later persist. It is intentionally derived from the already
// frozen plan rather than rebuilding a command from mutable manifest state.
type FrozenStdioBridgeAdmissionRequest struct {
	ManifestName  string
	DaemonName    string
	ManifestHash  string
	Command       string
	Args          []string
	WorkingDir    string
	Port          int
	StartDeadline time.Duration
}

// frozenStdioBridgeAdmissionFn is a narrow test override around the only
// process-owning admission operation. Production leaves it nil and runs the contained
// initialize -> tools/list probe below. Nil certifies successful admission and
// complete provisional cleanup; an error carries no cleanup proof unless the
// process owner explicitly returns stdioBridgeAdmissionReapedError.
var frozenStdioBridgeAdmissionFn func(context.Context, []FrozenStdioBridgeAdmissionRequest) error

// startStdioBridgeProvisionalFn permits owner-level startup failures in tests
// while retaining the admission probe and its cleanup-proof classification.
var startStdioBridgeProvisionalFn = process.StartProvisional

var settleStdioBridgeProvisionalFn = (*process.ProvisionalProcess).TerminateAndWait

type stdioBridgeAdmissionOwner struct {
	task       string
	request    FrozenStdioBridgeAdmissionRequest
	child      *process.ProvisionalProcess
	active     bool
	settling   bool
	cause      error
	cleanupErr error
}

type stdioBridgeAdmissionCustody struct {
	mu      sync.Mutex
	closed  bool
	entries map[string]*stdioBridgeAdmissionOwner
	changed chan struct{}
}

func (c *stdioBridgeAdmissionCustody) notifyLocked() {
	if c.changed != nil {
		close(c.changed)
	}
	c.changed = make(chan struct{})
}

// stdioBridgeAdmissionReapedError preserves the admission failure while
// certifying that the owner created no process or TerminateAndWait completed.
// Arbitrary start or cleanup failures do not carry this authority.
type stdioBridgeAdmissionReapedError struct{ cause error }

func (e *stdioBridgeAdmissionReapedError) Error() string { return e.cause.Error() }
func (e *stdioBridgeAdmissionReapedError) Unwrap() error { return e.cause }

// Unsettled errors retain the API lifetime owner through the creator-process
// boundary. Settle/Close on that same handle actively own cleanup; formatting
// an error never substitutes for the lifetime owner.
type stdioBridgeAdmissionUnsettledError struct {
	cause error
	owner *API
}

func (e *stdioBridgeAdmissionUnsettledError) Error() string {
	return "stdio bridge admission: unsettled original-child custody: " + e.cause.Error()
}
func (e *stdioBridgeAdmissionUnsettledError) Unwrap() error { return e.cause }

func (a *API) unsettledStdioBridgeAdmission(cause error) error {
	return &stdioBridgeAdmissionUnsettledError{cause: cause, owner: a}
}

// SettleStdioBridgeAdmissions retries only original-child cleanup, never an
// install or a new spawn. A refusal retains the identical child/reservation.
func (a *API) SettleStdioBridgeAdmissions(ctx context.Context) error {
	return a.settleStdioBridgeAdmissions(ctx, nil)
}

// CloseStdioBridgeAdmissions is terminal: it closes admission atomically before
// settling, including starters that already reserved but have not returned.
func (a *API) CloseStdioBridgeAdmissions(ctx context.Context) error {
	if a == nil {
		return errors.New("stdio bridge admission: nil API lifetime owner")
	}
	c := &a.stdioBridgeAdmissions
	c.mu.Lock()
	c.closed = true
	c.notifyLocked()
	c.mu.Unlock()
	return a.settleStdioBridgeAdmissions(ctx, nil)
}

func (a *API) settleStdioBridgeAdmissions(ctx context.Context, tasks map[string]bool) error {
	if a == nil {
		return errors.New("stdio bridge admission: nil API lifetime owner")
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, provisionalStdioBridgeCleanupDeadline)
	defer cancel()
	deadline, _ := cleanupCtx.Deadline()
	attempted := make(map[*stdioBridgeAdmissionOwner]bool)
	var refusals []error
	c := &a.stdioBridgeAdmissions
	for {
		c.mu.Lock()
		var keys []string
		for key := range c.entries {
			if tasks == nil || tasks[key] {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		var candidate *stdioBridgeAdmissionOwner
		pending := false
		var pendingOwners []*stdioBridgeAdmissionOwner
		for _, key := range keys {
			entry := c.entries[key]
			if entry.active || entry.settling {
				pending = true
				pendingOwners = append(pendingOwners, entry)
				continue
			}
			if !attempted[entry] {
				candidate = entry
				entry.settling = true
				break
			}
		}
		changed := c.changed
		c.mu.Unlock()
		if candidate == nil {
			if !pending {
				return errors.Join(refusals...)
			}
			select {
			case <-changed:
				continue
			case <-cleanupCtx.Done():
				causes := append([]error(nil), refusals...)
				c.mu.Lock()
				for _, entry := range pendingOwners {
					causes = append(causes, entry.cause, entry.cleanupErr)
				}
				c.mu.Unlock()
				return a.unsettledStdioBridgeAdmission(errors.Join(append(causes, cleanupCtx.Err())...))
			}
		}
		attempted[candidate] = true
		remaining := time.Until(deadline).Truncate(time.Millisecond)
		var cleanupErr error
		if remaining <= 0 || cleanupCtx.Err() != nil {
			cleanupErr = cleanupCtx.Err()
			if cleanupErr == nil {
				cleanupErr = context.DeadlineExceeded
			}
		} else if candidate.child != nil {
			cleanupErr = settleStdioBridgeProvisionalFn(candidate.child, remaining)
		}
		c.mu.Lock()
		candidate.settling = false
		candidate.cleanupErr = cleanupErr
		if cleanupErr == nil {
			delete(c.entries, candidate.task)
		} else {
			refusals = append(refusals, a.unsettledStdioBridgeAdmission(errors.Join(candidate.cause, fmt.Errorf("reap original task %s: %w", candidate.task, cleanupErr))))
		}
		c.notifyLocked()
		c.mu.Unlock()
	}
}

func (a *API) reserveStdioBridgeAdmissions(ctx context.Context, requests []FrozenStdioBridgeAdmissionRequest) ([]*stdioBridgeAdmissionOwner, error) {
	tasks := make(map[string]bool, len(requests))
	for _, request := range requests {
		if err := validateFrozenStdioBridgeAdmissionRequest(request); err != nil {
			return nil, err
		}
		task := supervisorTaskNameForManifestDaemon(request.ManifestName, request.DaemonName)
		if tasks[task] {
			return nil, fmt.Errorf("stdio bridge admission: duplicate logical task %s", task)
		}
		tasks[task] = true
	}
	// Cleanup is independent of a canceled probe context and precedes provider
	// starts. One aggregate budget covers every prior selected task.
	if err := a.settleStdioBridgeAdmissions(context.Background(), tasks); err != nil {
		return nil, err
	}
	c := &a.stdioBridgeAdmissions
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("stdio bridge admission: API admissions are closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for task := range tasks {
		if entry := c.entries[task]; entry != nil {
			return nil, a.unsettledStdioBridgeAdmission(errors.Join(entry.cause, entry.cleanupErr, fmt.Errorf("logical task %s is already reserved", task)))
		}
	}
	if c.entries == nil {
		c.entries = make(map[string]*stdioBridgeAdmissionOwner)
	}
	owners := make([]*stdioBridgeAdmissionOwner, 0, len(requests))
	for _, request := range requests {
		request.Args = append([]string(nil), request.Args...)
		entry := &stdioBridgeAdmissionOwner{task: supervisorTaskNameForManifestDaemon(request.ManifestName, request.DaemonName), request: request, active: true}
		c.entries[entry.task] = entry
		owners = append(owners, entry)
	}
	c.notifyLocked()
	return owners, nil
}

func (a *API) releaseStdioBridgeReservations(owners []*stdioBridgeAdmissionOwner) {
	c := &a.stdioBridgeAdmissions
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range owners {
		if c.entries[entry.task] == entry && entry.child == nil {
			delete(c.entries, entry.task)
		}
	}
	c.notifyLocked()
}

func (a *API) stdioBridgeAdmissionClosed() bool {
	c := &a.stdioBridgeAdmissions
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// admitFrozenStdioBridgeBeforeMutation is the fail-before-advertise gate for
// selected global stdio-bridge daemons. Dry-runs never call it.
func (a *API) admitFrozenStdioBridgeBeforeMutation(ctx context.Context, m *config.ServerManifest, plan *Plan, opts InstallOpts) error {
	if opts.DryRun || m == nil || m.Kind != config.KindGlobal || m.Transport != config.TransportStdioBridge {
		return nil
	}
	requests, err := frozenStdioBridgeAdmissionRequests(m, plan, opts.DaemonFilter)
	if err != nil {
		return err
	}
	if len(requests) == 0 {
		return nil
	}
	if opts.requireExistingStdioBridge {
		for _, request := range requests {
			existing, err := existingFrozenStdioBridgeGeneration(request)
			if err != nil {
				return err
			}
			if !existing {
				return ErrSupervisorCapabilityIncomplete
			}
		}
	}
	owners, err := a.reserveStdioBridgeAdmissions(ctx, requests)
	if err != nil {
		return err
	}
	defer a.releaseStdioBridgeReservations(owners)
	// Pending provider recovery checks durable history, so the start barrier
	// must precede handing control to the process-owning probe. Ordinary servers
	// keep their existing admission path without provider phase mutations.
	rec, found, err := ReadAdoptProvenance(m.Name)
	if rec != nil && rec.ProjectService != nil {
		return errGraphifyServiceOperationRequired
	}
	if err != nil {
		return err
	}
	startedHere := false
	if found && rec.ProviderSource != nil {
		for _, request := range requests {
			started, err := startProviderInstallForTask(m.Name, supervisorTaskNameForManifestDaemon(m.Name, request.DaemonName))
			if err != nil {
				return err
			}
			startedHere = startedHere || started
		}
	}
	if frozenStdioBridgeAdmissionFn != nil {
		err = frozenStdioBridgeAdmissionFn(ctx, requests)
	} else {
		for i, request := range requests {
			err = a.admitFrozenStdioBridgeRequest(ctx, request, owners[i])
			if err != nil {
				break
			}
		}
	}
	// Require proof for the complete returned outcome, not a sentinel found
	// inside a join that can also contain unconfirmed cleanup. A successful
	// probe's nil result certifies reaping by the admission owner's contract.
	_, reaped := err.(*stdioBridgeAdmissionReapedError)
	if startedHere && (err == nil || reaped) {
		// No install writer has run yet. Restore only the not_started history
		// this call claimed, so later admission/audit failures stay recoverable
		// and the existing client rollback owner can still identify a fresh run.
		err = errors.Join(err, settleProviderInstallStarted(m.Name, providerInstallPhaseNotStarted))
	}
	return err
}

func frozenStdioBridgeAdmissionRequests(m *config.ServerManifest, plan *Plan, daemonFilter string) ([]FrozenStdioBridgeAdmissionRequest, error) {
	if m == nil || plan == nil {
		return nil, errors.New("stdio bridge admission: nil manifest or frozen plan")
	}
	entries := make(map[string][]SupervisorIntentEntry, len(plan.SupervisorIntent))
	for _, entry := range plan.SupervisorIntent {
		entries[entry.Name] = append(entries[entry.Name], entry)
	}
	requests := make([]FrozenStdioBridgeAdmissionRequest, 0, len(m.Daemons))
	for _, daemon := range m.Daemons {
		if daemonFilter != "" && daemon.Name != daemonFilter {
			continue
		}
		name := supervisorTaskNameForManifestDaemon(m.Name, daemon.Name)
		matches := entries[name]
		if len(matches) != 1 {
			return nil, fmt.Errorf("stdio bridge admission: selected daemon %q requires exactly one frozen supervisor descriptor (got %d)", daemon.Name, len(matches))
		}
		entry := matches[0]
		if entry.Command == "" || len(entry.Args) == 0 || entry.WorkingDir == "" || daemon.Port <= 0 || entry.StartupBindDeadlineSeconds <= 0 {
			return nil, fmt.Errorf("stdio bridge admission: selected daemon %q has an incomplete frozen runtime descriptor", daemon.Name)
		}
		requests = append(requests, FrozenStdioBridgeAdmissionRequest{
			ManifestName: m.Name, DaemonName: daemon.Name, ManifestHash: entry.manifestHash,
			Command: entry.Command, Args: append([]string(nil), entry.Args...), WorkingDir: entry.WorkingDir,
			Port: daemon.Port, StartDeadline: time.Duration(entry.StartupBindDeadlineSeconds) * time.Second,
		})
	}
	return requests, nil
}

func (a *API) admitFrozenStdioBridgeRequest(ctx context.Context, request FrozenStdioBridgeAdmissionRequest, owner *stdioBridgeAdmissionOwner) (err error) {
	if a.stdioBridgeAdmissionClosed() {
		return errors.New("stdio bridge admission: API admissions are closed")
	}
	if err := validateFrozenStdioBridgeAdmissionRequest(request); err != nil {
		return err
	}
	existing, err := existingFrozenStdioBridgeGeneration(request)
	if err != nil {
		return err
	}
	if existing {
		probeCtx, cancel := context.WithTimeout(ctx, request.StartDeadline)
		defer cancel()
		return probeFrozenStdioBridgePort(probeCtx, request)
	}

	probeCtx, cancel := context.WithTimeout(ctx, request.StartDeadline)
	defer cancel()
	cmd := exec.Command(request.Command, request.Args...)
	cmd.Dir = request.WorkingDir
	child, startErr := startStdioBridgeProvisionalFn(cmd)
	if startErr != nil && child == nil {
		cause := fmt.Errorf("stdio bridge admission: start provisional %s/%s: %w", request.ManifestName, request.DaemonName, startErr)
		// Classify the whole owner outcome before wrapping it. A matching cause
		// nested in a join cannot rule out an unknown process or cleanup failure.
		if noProcess, ok := startErr.(*process.ProvisionalNoProcessError); ok && noProcess != nil {
			return &stdioBridgeAdmissionReapedError{cause: cause}
		}
		return cause
	}
	if child == nil {
		return errors.New("stdio bridge admission: provisional starter returned no owner or error")
	}
	c := &a.stdioBridgeAdmissions
	c.mu.Lock()
	owner.child = child
	closed := c.closed
	c.notifyLocked()
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.closed && err == nil {
			err = errors.New("stdio bridge admission: API closed before admission completed")
		}
		owner.cause = err
		owner.active = false
		c.notifyLocked()
		c.mu.Unlock()
		cleanupErr := a.settleStdioBridgeAdmissions(context.Background(), map[string]bool{owner.task: true})
		if cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
			return
		}
		if portInUse(request.Port) {
			err = errors.Join(err, fmt.Errorf("stdio bridge admission: provisional %s/%s left port %d in use", request.ManifestName, request.DaemonName, request.Port))
			return
		}
		if err != nil {
			err = &stdioBridgeAdmissionReapedError{cause: err}
		}
	}()
	if closed {
		return errors.Join(startErr, errors.New("stdio bridge admission: API closed during provisional acquisition"))
	}
	if startErr != nil {
		return fmt.Errorf("stdio bridge admission: start provisional %s/%s: %w", request.ManifestName, request.DaemonName, startErr)
	}
	if err := waitFrozenStdioBridgePort(probeCtx, request.Port); err != nil {
		return fmt.Errorf("stdio bridge admission: provisional %s/%s did not bind port %d: %w", request.ManifestName, request.DaemonName, request.Port, err)
	}
	return probeFrozenStdioBridgePort(probeCtx, request)
}

func validateFrozenStdioBridgeAdmissionRequest(request FrozenStdioBridgeAdmissionRequest) error {
	if request.ManifestName == "" || request.DaemonName == "" || request.Command == "" || len(request.Args) == 0 || request.Port <= 0 || request.StartDeadline <= 0 {
		return errors.New("stdio bridge admission: incomplete frozen runtime descriptor")
	}
	return nil
}

func existingFrozenStdioBridgeGeneration(request FrozenStdioBridgeAdmissionRequest) (bool, error) {
	stateDir, err := daemonStateDirReadOnly()
	if err != nil {
		return false, fmt.Errorf("stdio bridge admission: resolve existing intent: %w", err)
	}
	intent, err := ReadSupervisorIntent(joinStateFilePath(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stdio bridge admission: read existing intent: %w", err)
	}
	var matches []SupervisorDaemon
	for _, daemon := range intent.Daemons {
		if daemon.Server == request.ManifestName && daemon.Daemon == request.DaemonName {
			matches = append(matches, daemon)
		}
	}
	if len(matches) == 0 {
		return false, nil
	}
	if len(matches) != 1 {
		return false, fmt.Errorf("stdio bridge admission: existing generation for %s/%s is ambiguous", request.ManifestName, request.DaemonName)
	}
	current := matches[0]
	if current.Port != request.Port || current.Command != request.Command || !slices.Equal(current.Args, request.Args) || !sameFrozenWorkingDir(current.WorkingDir, request.WorkingDir) || !sameFrozenStartDeadline(current.StartupBindDeadlineSeconds, request.StartDeadline) || request.ManifestHash == "" || current.ManifestHash != request.ManifestHash {
		return false, fmt.Errorf("stdio bridge admission: existing generation for %s/%s does not exactly match the frozen plan", request.ManifestName, request.DaemonName)
	}
	return true, nil
}

func sameFrozenWorkingDir(current, frozen string) bool {
	if current == "" || frozen == "" {
		return current == frozen
	}
	return filepath.Clean(current) == filepath.Clean(frozen)
}

func sameFrozenStartDeadline(currentSeconds int, frozen time.Duration) bool {
	return currentSeconds > 0 && frozen > 0 && time.Duration(currentSeconds)*time.Second == frozen
}

func waitFrozenStdioBridgePort(ctx context.Context, port int) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if portInUse(port) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func probeFrozenStdioBridgePort(ctx context.Context, request FrozenStdioBridgeAdmissionRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	probe := singleHealthProbeContext(ctx, request.Port)
	if err := ctx.Err(); err != nil {
		return err
	}
	if probe != nil && probe.OK && probe.Readiness.State == MCPReadinessReady {
		return nil
	}
	if probe != nil && probe.Readiness.FailureID != "" {
		return &MCPReadinessError{Result: probe.Readiness}
	}
	return &MCPReadinessError{Result: MCPReadinessResult{
		SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageHost,
		FailureID: readinesswire.FailureReadinessHostUnavailable,
	}}
}

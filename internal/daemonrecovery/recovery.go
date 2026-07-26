package daemonrecovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

// PortOwnerCheck is the only port-owner detail allowed on the GUI wire.
type PortOwnerCheck string

const (
	PortOwnerReaped                 PortOwnerCheck = "reaped"
	PortOwnerAlreadyExited          PortOwnerCheck = "already_exited"
	PortOwnerTerminationUnconfirmed PortOwnerCheck = "termination_unconfirmed"
	PortOwnerUnbound                PortOwnerCheck = "unbound"
	PortOwnerTrackedChild           PortOwnerCheck = "tracked_child"
	PortOwnerPortUnresolvable       PortOwnerCheck = "port_unresolvable"
	PortOwnerProbeUnavailable       PortOwnerCheck = "probe_unavailable"
)

// Valid reports whether the value belongs to the safe response enum.
func (p PortOwnerCheck) Valid() bool {
	switch p {
	case PortOwnerReaped, PortOwnerAlreadyExited, PortOwnerTerminationUnconfirmed, PortOwnerUnbound, PortOwnerTrackedChild,
		PortOwnerPortUnresolvable, PortOwnerProbeUnavailable:
		return true
	default:
		return false
	}
}

// PortWaitOutcome reports the best-effort post-termination port-release observation.
// It contains no process-controlled detail and is safe for the GUI wire.
type PortWaitOutcome string

const (
	PortWaitNotRequired      PortWaitOutcome = "not_required"
	PortWaitReleased         PortWaitOutcome = "released"
	PortWaitStillBound       PortWaitOutcome = "still_bound"
	PortWaitProbeUnavailable PortWaitOutcome = "probe_unavailable"
)

// Valid reports whether the value belongs to the safe response enum.
func (p PortWaitOutcome) Valid() bool {
	switch p {
	case PortWaitNotRequired, PortWaitReleased, PortWaitStillBound, PortWaitProbeUnavailable:
		return true
	default:
		return false
	}
}

// Result means the supervisor accepted one force-respawn request. It does not
// assert that the daemon is Running yet.
type Result struct {
	TaskName        string
	Reaped          bool
	PortOwnerCheck  PortOwnerCheck
	PortWaitOutcome PortWaitOutcome
}

// FailureKind is the stable internal outcome the CLI and HTTP adapters map to
// their respective exit-code and status-code contracts.
type FailureKind string

const (
	FailureInvalidArgs               FailureKind = "invalid_args"
	FailureConfirmationRequired      FailureKind = "confirmation_required"
	FailureStateRead                 FailureKind = "state_read_failed"
	FailureUnknownTask               FailureKind = "unknown_task"
	FailureRefusedPortOwner          FailureKind = "refused_port_owner"
	FailureRespawnFailed             FailureKind = "respawn_failed"
	FailureSupervisorUnavailable     FailureKind = "supervisor_unavailable"
	FailureRequestCanceled           FailureKind = "request_canceled"
	FailureBoundaryProbeTimeout      FailureKind = "boundary_probe_timeout"
	FailureRespawnBudgetInsufficient FailureKind = "respawn_budget_insufficient"
)

// OperationError carries adapter-only diagnostics. HTTP handlers must redact it
// and serialize only their fixed error code.
type OperationError struct {
	Kind       FailureKind
	TaskName   string
	Cause      error
	KnownTasks []string
	Candidate  *ReapCandidate
	Respawn    api.RespawnResult
}

// ErrSupervisorTrackedChild reports that the destructive-boundary state read
// found the candidate registered as the target task's current child.
var ErrSupervisorTrackedChild = errors.New("target is a supervisor-tracked child")

// ErrInsufficientRespawnBudget reports a pre-kill refusal when the configured
// completion budget cannot preserve the mandatory detached respawn slice.
var ErrInsufficientRespawnBudget = errors.New("insufficient budget to reserve a post-termination respawn")

func (e *OperationError) Error() string {
	if e == nil {
		return "daemon recovery failed"
	}
	if e.Cause != nil {
		return fmt.Sprintf("daemon recovery %s for %s: %v", e.Kind, e.TaskName, e.Cause)
	}
	if e.Respawn.Code != "" {
		return fmt.Sprintf("daemon recovery %s for %s: %s: %s", e.Kind, e.TaskName, e.Respawn.Code, e.Respawn.Message)
	}
	return fmt.Sprintf("daemon recovery %s for %s", e.Kind, e.TaskName)
}

func (e *OperationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ReapCandidate is the fresh identity proof shown only to the CLI adapter. The
// GUI route never serializes it.
type ReapCandidate struct {
	TaskName string
	Port     int
	PID      int
	Verdict  Verdict
	Identity process.ProcessIdentity
}

// NotificationKind identifies non-fatal progress details for the CLI adapter.
type NotificationKind string

const (
	NotificationPortUnresolvable       NotificationKind = "port_unresolvable"
	NotificationProbeUnavailable       NotificationKind = "probe_unavailable"
	NotificationTrackedChild           NotificationKind = "tracked_child"
	NotificationReapCandidate          NotificationKind = "reap_candidate"
	NotificationReaped                 NotificationKind = "reaped"
	NotificationTerminationUnconfirmed NotificationKind = "termination_unconfirmed"
	NotificationAlreadyExited          NotificationKind = "already_exited"
	NotificationPortWaitTimeout        NotificationKind = "port_wait_timeout"
)

// Notification contains process details for local CLI output only.
type Notification struct {
	Kind      NotificationKind
	TaskName  string
	Port      int
	PID       int
	Duration  time.Duration
	Cause     error
	Candidate *ReapCandidate
}

// Options contains caller-owned confirmation and local progress hooks.
type Options struct {
	Confirmed   bool
	ConfirmReap func(ReapCandidate) bool
	Notify      func(Notification)
}

// Dependencies are the operation's injected system boundaries. There is no
// retry at this layer: state reads and port probes are point-in-time decisions;
// the identity reader and IPC client retain their own bounded policies.
type Dependencies struct {
	StateDir          func() (string, error)
	ReadIntent        func(path string) (*api.SupervisorIntentFile, error)
	ReadState         func(path string) (*api.SupervisorStateFile, error)
	PortOwner         func(context.Context, int) (pid int, ok bool, err error)
	SelfPID           func() int
	LookupIdentity    func(context.Context, int) (process.ProcessIdentity, error)
	ExecutableMatches func(pid int, expectedPath string) bool
	HoldProcess       func(pid int) (process.HeldPIDGeneration, error)
	ProbeSupervisor   func(context.Context) error
	Respawn           func(context.Context, string, bool) (api.RespawnResult, error)
	Now               func() time.Time
	Sleep             func(context.Context, time.Duration) error
	PortPollInterval  time.Duration
	PortWaitTimeout   time.Duration
	PostKillTimeout   time.Duration
	RespawnReserve    time.Duration
	AuditEmitTimeout  time.Duration
}

const (
	defaultPortPollInterval      = 250 * time.Millisecond
	defaultPortWaitTimeout       = 10 * time.Second
	defaultPostKillFinishTimeout = 30 * time.Second
	defaultCommittedAuditTimeout = 2 * time.Second
	// DialSupervisorIPCRespawn uses a 15-second request timeout plus a 5-second
	// client allowance when it owns the deadline, so reserve the same 20 seconds.
	defaultRespawnReserve         = 20 * time.Second
	defaultSupervisorProbeTimeout = 5 * time.Second
)

// ProductionDependencies wires the shared operation to its owning API/process
// surfaces. The status probe is a single no-retry call bounded to five seconds;
// respawn retains its 15-second request timeout inside the reserved 20 seconds.
func ProductionDependencies() Dependencies {
	return Dependencies{
		StateDir:          api.DaemonStateDir,
		ReadIntent:        api.ReadSupervisorIntent,
		ReadState:         api.ReadSupervisorState,
		PortOwner:         api.LoopbackPortOwnerPIDContext,
		SelfPID:           os.Getpid,
		LookupIdentity:    productionIdentityLookup(),
		ExecutableMatches: process.PIDExecutableMatches,
		HoldProcess:       process.HoldPIDForTermination,
		ProbeSupervisor: func(ctx context.Context) error {
			probeCtx, cancel := context.WithTimeout(ctx, defaultSupervisorProbeTimeout)
			defer cancel()
			_, err := api.DialSupervisorIPCStatus(probeCtx)
			return err
		},
		Respawn: func(ctx context.Context, taskName string, force bool) (api.RespawnResult, error) {
			return api.DialSupervisorIPCRespawn(ctx, taskName, force, 15000)
		},
		Now:              time.Now,
		Sleep:            sleepContext,
		PortPollInterval: defaultPortPollInterval,
		PortWaitTimeout:  defaultPortWaitTimeout,
		PostKillTimeout:  defaultPostKillFinishTimeout,
		RespawnReserve:   defaultRespawnReserve,
		AuditEmitTimeout: defaultCommittedAuditTimeout,
	}
}

// Execute runs recovery through production dependencies.
func Execute(ctx context.Context, taskName string, options Options) (Result, error) {
	return ExecuteWithDependencies(ctx, taskName, options, ProductionDependencies())
}

// ExecuteWithDependencies is the single recovery authority shared by CLI and
// GUI adapters. It is exported so both adapters can preserve hermetic tests.
func ExecuteWithDependencies(ctx context.Context, taskName string, options Options, deps Dependencies) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	normalized := CanonicalTaskName(strings.TrimSpace(taskName))
	if normalized == "" || api.IsMaintenanceTaskName(normalized) {
		return Result{}, &OperationError{Kind: FailureInvalidArgs, TaskName: normalized}
	}
	if !options.Confirmed && options.ConfirmReap == nil {
		return Result{}, &OperationError{Kind: FailureConfirmationRequired, TaskName: normalized}
	}
	if err := validateDependencies(deps); err != nil {
		return Result{}, &OperationError{Kind: FailureStateRead, TaskName: normalized, Cause: err}
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Sleep == nil {
		deps.Sleep = sleepContext
	}
	if deps.PortPollInterval <= 0 {
		deps.PortPollInterval = defaultPortPollInterval
	}
	if deps.PortWaitTimeout <= 0 {
		deps.PortWaitTimeout = defaultPortWaitTimeout
	}
	if deps.PostKillTimeout <= 0 {
		deps.PostKillTimeout = defaultPostKillFinishTimeout
	}
	if deps.RespawnReserve <= 0 {
		deps.RespawnReserve = defaultRespawnReserve
	}
	if deps.AuditEmitTimeout <= 0 {
		deps.AuditEmitTimeout = defaultCommittedAuditTimeout
	}
	if deps.PostKillTimeout <= deps.RespawnReserve {
		cause := fmt.Errorf("%w: completion budget %s does not exceed reservation %s",
			ErrInsufficientRespawnBudget, deps.PostKillTimeout, deps.RespawnReserve)
		return Result{}, &OperationError{Kind: FailureRespawnBudgetInsufficient, TaskName: normalized, Cause: cause}
	}
	respawnCtx := ctx
	portWaitOutcome := PortWaitNotRequired
	terminationCommitted := false
	var postCommitNotifications []Notification
	var postCommitAudits []func()

	stateDir, err := deps.StateDir()
	if err != nil {
		return Result{}, &OperationError{Kind: FailureStateRead, TaskName: normalized, Cause: err}
	}
	intent, err := deps.ReadIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil || intent == nil {
		if err == nil {
			err = errors.New("supervisor intent is nil")
		}
		return Result{}, &OperationError{Kind: FailureStateRead, TaskName: normalized, Cause: err}
	}
	descriptor, known := findDescriptor(intent, normalized)
	if descriptor == nil {
		return Result{}, &OperationError{Kind: FailureUnknownTask, TaskName: normalized, KnownTasks: known}
	}
	tracked, stateErr := trackedEntries(deps, stateDir, normalized)
	if stateErr != nil {
		return Result{}, &OperationError{Kind: FailureStateRead, TaskName: normalized, Cause: stateErr}
	}

	check := PortOwnerUnbound
	reaped := false
	if port, ok := api.EffectiveDaemonPort(*descriptor); ok && port > 0 {
		descriptor.Port = port
		ownerPID, bound, probeErr := deps.PortOwner(ctx, port)
		switch {
		case ctx.Err() != nil:
			return Result{}, canceledOperationError(stateDir, *descriptor, ownerPID, "initial_port_probe", ctx.Err(), nil, false)
		case probeErr != nil:
			check = PortOwnerProbeUnavailable
			notify(options, Notification{Kind: NotificationProbeUnavailable, TaskName: normalized, Port: port, Cause: probeErr})
		case !bound:
			check = PortOwnerUnbound
		case ownerPID <= 0:
			candidate := ReapCandidate{TaskName: normalized, Port: port, PID: ownerPID, Verdict: VerdictUnverified}
			emitRecoverAudit(stateDir, "daemon-port-squatter-unverified", VerdictUnverified, *descriptor, ownerPID, process.ProcessIdentity{}, map[string]any{
				"note": "operator recover refused: port is bound but its owner PID is unavailable; NOT killed",
			})
			return Result{}, &OperationError{Kind: FailureRefusedPortOwner, TaskName: normalized, Candidate: &candidate}
		default:
			if entry, ok := tracked[normalized]; ok && ownerPID == entry.CurrentPID {
				check = PortOwnerTrackedChild
				notify(options, Notification{Kind: NotificationTrackedChild, TaskName: normalized, Port: port, PID: ownerPID})
				break
			}
			if deps.LookupIdentity == nil {
				candidate := ReapCandidate{TaskName: normalized, Port: port, PID: ownerPID, Verdict: VerdictUnverified}
				emitRecoverAudit(stateDir, "daemon-port-squatter-unverified", VerdictUnverified, *descriptor, ownerPID, process.ProcessIdentity{}, map[string]any{
					"note": "operator recover refused: process identity lookup unavailable; NOT killed",
				})
				return Result{}, &OperationError{Kind: FailureRefusedPortOwner, TaskName: normalized, Candidate: &candidate}
			}
			if err := ctx.Err(); err != nil {
				return Result{}, canceledOperationError(stateDir, *descriptor, ownerPID, "before_generation_hold", err, nil, false)
			}
			generation, holdErr := deps.HoldProcess(ownerPID)
			if holdErr != nil {
				candidate := ReapCandidate{TaskName: normalized, Port: port, PID: ownerPID, Verdict: VerdictUnverified}
				emitRecoverAudit(stateDir, "daemon-port-squatter-unverified", VerdictUnverified, *descriptor, ownerPID, process.ProcessIdentity{}, map[string]any{
					"note": "operator recover refused: process generation could not be held; NOT killed",
				})
				return Result{}, &OperationError{Kind: FailureRefusedPortOwner, TaskName: normalized, Cause: holdErr, Candidate: &candidate}
			}
			defer func() { _ = generation.Close() }()
			verdict, identity := ClassifyPortOwner(*descriptor, ownerPID, deps.SelfPID(), tracked, ClassifierDependencies{
				LookupIdentity: func(pid int) (process.ProcessIdentity, error) {
					return deps.LookupIdentity(ctx, pid)
				},
				ExecutableMatches: deps.ExecutableMatches,
				Generation:        generation,
			})
			candidate := ReapCandidate{TaskName: normalized, Port: port, PID: ownerPID, Verdict: verdict, Identity: identity}
			if err := ctx.Err(); err != nil {
				return Result{}, canceledOperationError(stateDir, *descriptor, ownerPID, "identity_classification", err, &candidate, false)
			}
			if verdict != VerdictOwnTask {
				emitRecoverAudit(stateDir, "daemon-port-squatter-"+verdict.String(), verdict, *descriptor, ownerPID, identity, map[string]any{
					"note": "operator recover refused: port owner is not a verified disowned child of this task; NOT killed",
				})
				return Result{}, &OperationError{Kind: FailureRefusedPortOwner, TaskName: normalized, Candidate: &candidate}
			}
			notify(options, Notification{Kind: NotificationReapCandidate, TaskName: normalized, Port: port, PID: ownerPID, Candidate: &candidate})
			if !options.Confirmed && !options.ConfirmReap(candidate) {
				emitRecoverAudit(stateDir, "daemon-port-squatter-confirmation-declined", verdict, *descriptor, ownerPID, process.ProcessIdentity{}, map[string]any{
					"note": "operator declined the verified-own reap; NOT killed",
				})
				return Result{}, &OperationError{Kind: FailureRefusedPortOwner, TaskName: normalized, Candidate: &candidate}
			}

			proof := KillProof(identity)
			if verifyErr := generation.VerifyIdentity(proof); verifyErr != nil {
				emitRecoverAudit(stateDir, "daemon-port-squatter-unverified", VerdictUnverified, *descriptor, ownerPID, identity, map[string]any{
					"note": "operator recover refused: held process identity changed before reap; NOT killed",
				})
				return Result{}, &OperationError{Kind: FailureRefusedPortOwner, TaskName: normalized, Cause: verifyErr, Candidate: &candidate}
			}
			if err := ctx.Err(); err != nil {
				return Result{}, canceledOperationError(stateDir, *descriptor, ownerPID, "before_boundary_probe", err, &candidate, false)
			}

			boundaryPID, stillBound, boundaryErr := deps.PortOwner(ctx, port)
			if err := ctx.Err(); err != nil {
				return Result{}, canceledOperationError(stateDir, *descriptor, ownerPID, "boundary_port_probe", err, &candidate, true)
			}
			if errors.Is(boundaryErr, context.Canceled) || errors.Is(boundaryErr, context.DeadlineExceeded) {
				return Result{}, canceledOperationError(stateDir, *descriptor, ownerPID, "boundary_port_probe", boundaryErr, &candidate, true)
			}
			if boundaryErr != nil || (stillBound && boundaryPID != ownerPID) {
				cause := boundaryErr
				if cause == nil {
					cause = fmt.Errorf("port owner changed from PID %d to PID %d", ownerPID, boundaryPID)
				}
				emitRecoverAudit(stateDir, "daemon-port-squatter-unverified", VerdictUnverified, *descriptor, ownerPID, identity, map[string]any{
					"note": "operator recover refused: port ownership changed before reap; NOT killed",
				})
				return Result{}, &OperationError{Kind: FailureRefusedPortOwner, TaskName: normalized, Cause: cause, Candidate: &candidate}
			}
			if !stillBound {
				check = PortOwnerUnbound
				break
			}
			if boundaryPID <= 0 {
				emitRecoverAudit(stateDir, "daemon-port-squatter-unverified", VerdictUnverified, *descriptor, ownerPID, process.ProcessIdentity{}, map[string]any{
					"note": "operator recover refused: port remained bound but its owner PID became unavailable; NOT killed",
				})
				return Result{}, &OperationError{Kind: FailureRefusedPortOwner, TaskName: normalized, Candidate: &candidate}
			}

			boundaryTracked, stateErr := trackedEntries(deps, stateDir, normalized)
			if stateErr != nil {
				return Result{}, &OperationError{Kind: FailureStateRead, TaskName: normalized, Cause: stateErr}
			}
			if entry := boundaryTracked[normalized]; entry.CurrentPID == ownerPID {
				cause := fmt.Errorf("%w: PID %d is current for %s", ErrSupervisorTrackedChild, ownerPID, normalized)
				emitRecoverAudit(stateDir, "daemon-port-squatter-tracked-child", VerdictUnverified, *descriptor, ownerPID, process.ProcessIdentity{}, map[string]any{
					"note": "operator recover refused: target became a supervisor-tracked child before the destructive boundary; NOT killed",
				})
				return Result{}, &OperationError{Kind: FailureRefusedPortOwner, TaskName: normalized, Cause: cause, Candidate: &candidate}
			}

			if err := ctx.Err(); err != nil {
				return Result{}, canceledOperationError(stateDir, *descriptor, ownerPID, "before_terminate", err, &candidate, false)
			}
			// This status IPC probe intentionally precedes the destructive call. It is
			// inherently TOCTOU: the supervisor can still die after the probe, but it
			// converts the common already-dead case from kill-without-restoration into
			// a refusal that destroys nothing.
			preKillProbeStarted := deps.Now()
			probeErr := deps.ProbeSupervisor(ctx)
			if err := ctx.Err(); err != nil {
				return Result{}, canceledOperationError(stateDir, *descriptor, ownerPID, "supervisor_reachability_probe", err, &candidate, false)
			}
			if probeErr != nil {
				emitRecoverAudit(stateDir, "daemon-recovery-supervisor-unavailable", verdict, *descriptor, ownerPID, identity, map[string]any{
					"err":  BoundEventField(probeErr.Error()),
					"note": "operator recover refused before termination because supervisor IPC was unreachable; NOT killed",
				})
				return Result{}, &OperationError{Kind: FailureSupervisorUnavailable, TaskName: normalized, Cause: probeErr, Candidate: &candidate}
			}
			// The detached respawn does not inherit the request deadline. Charge only
			// this bounded pre-kill reachability probe against the configured recovery
			// budget; elapsed identity/state work on the GUI request clock cannot shrink
			// the fresh post-commit respawn reservation.
			remainingBeforeTerminate := deps.PostKillTimeout - deps.Now().Sub(preKillProbeStarted)
			if remainingBeforeTerminate <= deps.RespawnReserve {
				cause := fmt.Errorf("%w: remaining budget %s does not exceed reservation %s",
					ErrInsufficientRespawnBudget, remainingBeforeTerminate, deps.RespawnReserve)
				emitRecoverAudit(stateDir, "daemon-recovery-budget-insufficient", verdict, *descriptor, ownerPID, identity, map[string]any{
					"remaining_budget": remainingBeforeTerminate.String(),
					"respawn_reserve":  deps.RespawnReserve.String(),
					"note":             "operator recover refused before termination because mandatory respawn time could not be reserved; NOT killed",
				})
				return Result{}, &OperationError{Kind: FailureRespawnBudgetInsufficient, TaskName: normalized, Cause: cause, Candidate: &candidate}
			}
			// Point of no return: this is the final cancellation check immediately
			// before Terminate. Cancellation after this check can race the destructive
			// call; if termination commits, recovery must use the detached budget and
			// attempt the mandatory respawn.
			// The post-kill budget starts at the destructive call itself. Pre-kill
			// identity, boundary reads, and the supervisor probe are intentionally not
			// charged against the bounded termination/port-release phase.
			postKillStarted := deps.Now()
			committed, terminateErr := generation.Terminate()
			if !committed && errors.Is(terminateErr, process.ErrProcessAlreadyExited) {
				check = PortOwnerAlreadyExited
				notify(options, Notification{Kind: NotificationAlreadyExited, TaskName: normalized, Port: port, PID: ownerPID})
				postCommitAudits = append(postCommitAudits, func() {
					emitRecoverAudit(stateDir, "daemon-port-squatter-already-exited", verdict, *descriptor, ownerPID, identity, map[string]any{
						"note": "operator recover: verified-own port owner had already exited; no reap was performed, forcing a respawn",
					})
				})
				break
			}
			if !committed {
				if terminateErr == nil {
					terminateErr = errors.New("process termination returned without committing or reporting an error")
				}
				emitRecoverAudit(stateDir, "daemon-port-squatter-reap-failed", verdict, *descriptor, ownerPID, identity, map[string]any{
					"err":  BoundEventField(terminateErr.Error()),
					"note": "operator recover: identity-gated reap failed",
				})
				return Result{}, &OperationError{Kind: FailureRefusedPortOwner, TaskName: normalized, Cause: terminateErr, Candidate: &candidate}
			}
			terminationCommitted = true
			var committedAudit api.SupervisorEvent
			if terminateErr == nil {
				reaped = true
				check = PortOwnerReaped
				postCommitNotifications = append(postCommitNotifications, Notification{Kind: NotificationReaped, TaskName: normalized, Port: port, PID: ownerPID})
				committedAudit = auditEvent("daemon-port-squatter-reaped", "recover", verdict, *descriptor, ownerPID, identity, map[string]any{
					"note": "operator recover: verified-own port squatter exit confirmed, forcing a respawn",
				})
			} else {
				check = PortOwnerTerminationUnconfirmed
				postCommitNotifications = append(postCommitNotifications, Notification{Kind: NotificationTerminationUnconfirmed, TaskName: normalized, Port: port, PID: ownerPID, Cause: terminateErr})
				committedAudit = auditEvent("daemon-port-squatter-termination-unconfirmed", "recover", verdict, *descriptor, ownerPID, identity, map[string]any{
					"err":  BoundEventField(terminateErr.Error()),
					"note": "operator recover: termination committed but process exit was not confirmed; forcing a respawn",
				})
			}
			// Charge the bounded pre-respawn audit only to the non-reserved
			// post-kill slice. Any time it consumes reduces the subsequent port
			// wait; the fresh RespawnReserve context below remains untouched.
			auditBudget := boundedPortWaitBudget(deps.AuditEmitTimeout, deps.PostKillTimeout, deps.RespawnReserve, deps.Now().Sub(postKillStarted))
			if auditBudget <= 0 {
				queueIdempotentAuditFallback(&postCommitAudits, stateDir, committedAudit, nil)
			} else if pending, emitErr := emitRecoverAuditEventWithTimeoutTracked(stateDir, committedAudit, auditBudget); emitErr != nil {
				queueIdempotentAuditFallback(&postCommitAudits, stateDir, committedAudit, pending)
			}
			terminationElapsed := deps.Now().Sub(postKillStarted)
			waitBudget := boundedPortWaitBudget(deps.PortWaitTimeout, deps.PostKillTimeout, deps.RespawnReserve, terminationElapsed)
			var waitErr error
			if waitBudget > 0 {
				waitCtx, waitCancel := context.WithTimeout(context.WithoutCancel(ctx), waitBudget)
				portWaitOutcome, waitErr = waitForPortFree(waitCtx, deps, port, waitBudget)
				waitCancel()
			} else {
				portWaitOutcome = PortWaitProbeUnavailable
				waitErr = fmt.Errorf("port %d release wait skipped to preserve the mandatory respawn reservation", port)
			}
			if waitErr != nil {
				postCommitNotifications = append(postCommitNotifications, Notification{Kind: NotificationPortWaitTimeout, TaskName: normalized, Port: port, Duration: waitBudget, Cause: waitErr})
			}
			finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), deps.RespawnReserve)
			defer finishCancel()
			respawnCtx = finishCtx
		}
	} else {
		check = PortOwnerPortUnresolvable
		notify(options, Notification{Kind: NotificationPortUnresolvable, TaskName: normalized})
	}

	if !terminationCommitted {
		if err := ctx.Err(); err != nil {
			return Result{}, canceledOperationError(stateDir, *descriptor, 0, "before_respawn", err, nil, false)
		}
	}
	respawn, respawnErr := deps.Respawn(respawnCtx, normalized, true)
	// A blocking audit flock must never strand recovery before restart delivery.
	// Only bounded-attempt fallbacks and the no-kill already-exited audit remain
	// queued here after the injected supervisor-dispatch boundary.
	for _, emitAudit := range postCommitAudits {
		emitAudit()
	}
	for _, notification := range postCommitNotifications {
		notify(options, notification)
	}
	result := Result{TaskName: normalized, Reaped: reaped, PortOwnerCheck: check, PortWaitOutcome: portWaitOutcome}
	if respawnErr != nil {
		if !terminationCommitted && (errors.Is(respawnErr, context.Canceled) || errors.Is(respawnErr, context.DeadlineExceeded)) {
			return result, canceledOperationError(stateDir, *descriptor, 0, "respawn", respawnErr, nil, false)
		}
		if errors.Is(respawnErr, api.ErrRespawnSetupFailure) {
			return result, &OperationError{Kind: FailureStateRead, TaskName: normalized, Cause: respawnErr}
		}
		return result, &OperationError{Kind: FailureSupervisorUnavailable, TaskName: normalized, Cause: respawnErr}
	}
	if respawn.Code == "SUPERVISOR_UNAVAILABLE" {
		return result, &OperationError{Kind: FailureSupervisorUnavailable, TaskName: normalized, Respawn: respawn}
	}
	if !respawn.Success {
		return result, &OperationError{Kind: FailureRespawnFailed, TaskName: normalized, Respawn: respawn}
	}
	return result, nil
}

func validateDependencies(deps Dependencies) error {
	switch {
	case deps.StateDir == nil:
		return errors.New("StateDir dependency is nil")
	case deps.ReadIntent == nil:
		return errors.New("ReadIntent dependency is nil")
	case deps.ReadState == nil:
		return errors.New("ReadState dependency is nil")
	case deps.PortOwner == nil:
		return errors.New("PortOwner dependency is nil")
	case deps.SelfPID == nil:
		return errors.New("SelfPID dependency is nil")
	case deps.ExecutableMatches == nil:
		return errors.New("ExecutableMatches dependency is nil")
	case deps.HoldProcess == nil:
		return errors.New("HoldProcess dependency is nil")
	case deps.ProbeSupervisor == nil:
		return errors.New("ProbeSupervisor dependency is nil")
	case deps.Respawn == nil:
		return errors.New("Respawn dependency is nil")
	default:
		return nil
	}
}

func findDescriptor(intent *api.SupervisorIntentFile, normalized string) (*api.SupervisorDaemon, []string) {
	for i := range intent.Daemons {
		name := CanonicalTaskName(intent.Daemons[i].TaskName)
		if name == normalized {
			return &intent.Daemons[i], knownTaskNames(intent)
		}
	}
	return nil, knownTaskNames(intent)
}

func knownTaskNames(intent *api.SupervisorIntentFile) []string {
	known := make([]string, 0, len(intent.Daemons))
	for _, descriptor := range intent.Daemons {
		known = append(known, CanonicalTaskName(descriptor.TaskName))
	}
	sort.Strings(known)
	return known
}

func trackedEntries(deps Dependencies, stateDir, targetTask string) (map[string]RuntimeEntry, error) {
	tracked := map[string]RuntimeEntry{}
	state, err := deps.ReadState(filepath.Join(stateDir, "supervisor-state.json"))
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, errors.New("supervisor state is nil")
	}
	for taskName, daemon := range state.Daemons {
		tracked[CanonicalTaskName(taskName)] = RuntimeEntry{CurrentPID: daemon.CurrentPID, OrphanPID: daemon.OrphanPID}
	}
	if _, ok := tracked[targetTask]; !ok {
		return nil, fmt.Errorf("supervisor state is missing target row %q", targetTask)
	}
	return tracked, nil
}

func boundedPortWaitBudget(configuredWait, totalPostKill, respawnReserve, elapsed time.Duration) time.Duration {
	available := totalPostKill - respawnReserve - elapsed
	if available <= 0 {
		return 0
	}
	if configuredWait < available {
		return configuredWait
	}
	return available
}

func waitForPortFree(ctx context.Context, deps Dependencies, port int, timeout time.Duration) (PortWaitOutcome, error) {
	deadline := deps.Now().Add(timeout)
	outcome := PortWaitStillBound
	for deps.Now().Before(deadline) {
		_, bound, err := deps.PortOwner(ctx, port)
		if err == nil && !bound {
			return PortWaitReleased, nil
		}
		if err != nil {
			outcome = PortWaitProbeUnavailable
		} else {
			outcome = PortWaitStillBound
		}
		if err := deps.Sleep(ctx, deps.PortPollInterval); err != nil {
			return outcome, err
		}
	}
	return outcome, fmt.Errorf("port %d remained bound after %s", port, timeout)
}

func canceledOperationError(
	stateDir string,
	descriptor api.SupervisorDaemon,
	ownerPID int,
	stage string,
	cause error,
	candidate *ReapCandidate,
	boundaryProbe bool,
) *OperationError {
	reason := "canceled"
	kind := FailureRequestCanceled
	if errors.Is(cause, context.DeadlineExceeded) {
		reason = "deadline_exceeded"
		if boundaryProbe {
			kind = FailureBoundaryProbeTimeout
		}
	}
	emitRecoverAudit(stateDir, "daemon-recovery-canceled", VerdictUnverified, descriptor, ownerPID, process.ProcessIdentity{}, map[string]any{
		"stage":  stage,
		"reason": reason,
		"note":   "operator recover stopped before any process termination was committed",
	})
	return &OperationError{
		Kind:      kind,
		TaskName:  CanonicalTaskName(descriptor.TaskName),
		Cause:     cause,
		Candidate: candidate,
	}
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func notify(options Options, notification Notification) {
	if options.Notify != nil {
		options.Notify(notification)
	}
}

func emitRecoverAudit(
	stateDir, event string,
	verdict Verdict,
	d api.SupervisorDaemon,
	ownerPID int,
	identity process.ProcessIdentity,
	extra map[string]any,
) {
	emitRecoverAuditEvent(stateDir, auditEvent(event, "recover", verdict, d, ownerPID, identity, extra))
}

func emitRecoverAuditEvent(stateDir string, event api.SupervisorEvent) {
	logger, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	_ = logger.Emit(event)
}

// emitRecoverAuditEventWithTimeoutTracked replaces the prior
// emitRecoverAuditEventWithTimeout (residual 2 review fix; removed — it had
// no other callers once this file's one bounded pre-respawn attempt was
// widened to track its worker). On a timeout it ALSO returns a
// *api.PendingSupervisorEventEmit exposing the abandoned worker's eventual
// completion, so queueIdempotentAuditFallback can check the SAME write
// instead of ever enqueuing an independent duplicate.
func emitRecoverAuditEventWithTimeoutTracked(stateDir string, event api.SupervisorEvent, timeout time.Duration) (*api.PendingSupervisorEventEmit, error) {
	logger, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		return nil, err
	}
	defer func() { _ = logger.Close() }()
	return logger.EmitWithTimeoutTracked(event, timeout)
}

// queueIdempotentAuditFallback appends the ONE closure that decides, at
// post-respawn execution time, whether a physical write is still needed for
// committedAudit (round 3 consolidation of the residual-2 review fix). The
// prior shape (round 2) waited a SECOND, independently-invented grace
// window (daemonRecoveryLateAuditGrace, 2s) for the abandoned worker before
// giving up and firing an unconditional duplicate write. That was flagged
// PILED: two writers were enforcing the same audit outcome with no shared
// commit protocol — reproduced both ways: releasing the stalled worker
// after the grace window produced two rows for one event, and holding it
// forever left the fallback's own unbounded blocking Emit call hung, so
// recovery itself never returned.
//
// The fix folds the fallback into the ALREADY-tracked operation instead of
// layering a second one. There is exactly one physical writer per event:
//
//   - pending == nil: no worker was ever spawned for this event — either
//     the bounded attempt's own configured budget (the "outer deadline",
//     deps.AuditEmitTimeout as bounded by boundedPortWaitBudget) was already
//     exhausted before the call could even try, or the call failed before
//     acquiring both locks (log-open/marshal failure). This closure is the
//     ONLY possible writer, so it fires unconditionally — unchanged from
//     round 2.
//   - pending != nil: a worker IS already in flight holding both locks.
//     writeEventLine has no cancellable syscall surface (round 2's own
//     finding), so that worker is guaranteed to eventually finish this
//     exact append on its own, independent of anything this closure does.
//     A single NON-BLOCKING peek (Wait(0)) is enough to catch the case
//     where it already landed in the sliver of time since the tracked call
//     gave up. If it is still unsettled, this closure adds NO further wait
//     of its own — the tracked call already spent the full outer deadline,
//     so there is nothing left to wait for, and inventing a second wait
//     here is exactly the piling this consolidation removes. The abandoned
//     worker keeps running and commits the row whenever the stall clears;
//     the only accepted residual is that a one-shot CLI process that exits
//     immediately after recover returns can end this goroutine before that
//     happens (an existing property of the abandon-and-hope design, not a
//     new one — see EmitWithTimeoutTracked's own doc). Only a DEFINITE
//     (non-timeout) failure from the worker — meaning the row is confirmed
//     absent rather than merely unconfirmed — makes a fresh write from this
//     closure legitimate rather than a race against an unknown outcome.
func queueIdempotentAuditFallback(postCommitAudits *[]func(), stateDir string, audit api.SupervisorEvent, pending *api.PendingSupervisorEventEmit) {
	*postCommitAudits = append(*postCommitAudits, func() {
		if pending != nil {
			switch waitErr := pending.Wait(0); {
			case waitErr == nil:
				return // the tracked worker's write already landed -- the one physical row exists.
			case errors.Is(waitErr, api.ErrSupervisorEventEmitTimeout):
				// Still unsettled, and by design not waited on further: see
				// the doc comment above. Do NOT write again here.
				return
			case errors.Is(waitErr, api.ErrSupervisorEventReleaseFailed):
				// The worker could not release the cross-process flock. That
				// says nothing about the ROW — the append phase ran, and may
				// well have succeeded — so this branch's precondition ("no row
				// exists and none is coming") is NOT met and a write here would
				// re-introduce exactly the two-writers-one-outcome duplicate
				// this function exists to prevent. Fail closed: skip the
				// fallback. Losing one audit row while the event log's lock is
				// already stuck is strictly better than a duplicate row, and
				// the release failure is itself reported to the emit caller.
				return
			}
			// A genuine (non-timeout) write failure: the tracked attempt is
			// DEFINITELY settled as failed (no row exists and none is
			// coming from it), so writing once here is a fresh attempt, not
			// a race against an unknown outcome.
		}
		emitRecoverAuditEvent(stateDir, audit)
	})
}

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

// AuditHandoff reports whether the committed-recovery audit handoff could
// confirm the RELEASE of the cross-process supervisor-event-log flock it took.
// It contains no process-controlled detail and is safe for the GUI wire.
//
// It is a WARNING channel, deliberately NOT an outcome channel.
// AuditHandoffReleaseUnconfirmed never downgrades the recovery verdict, because
// on that path the audit row IS durable (PersistPending established the carrier)
// and both the termination and the respawn committed. What it reports is a
// PROCESS-scoped condition: this process may still hold the flock on
// supervisor-events.log, blocking every other emitter — the supervisor, the
// install CLI — until it exits. The remediation is "restart this process", never
// "retry this recovery"; folding it into the error would invite an operator to
// re-run a destructive recovery that already completed.
type AuditHandoff string

const (
	// AuditHandoffNotRequired means no committed-recovery audit was staged
	// (no termination was committed, so there is no handoff to make).
	AuditHandoffNotRequired AuditHandoff = "not_required"
	// AuditHandoffDurable means the audit row is durable and every event-log
	// flock THIS PROCESS took on that log was confirmed released.
	//
	// The scope is deliberately the process, not this one recovery: the
	// condition being reported is process-scoped (see the type doc above), and
	// scoping the verdict per-recovery is what let an unenumerated outcome
	// report "confirmed released" while an abandoned writer still held the lock.
	AuditHandoffDurable AuditHandoff = "durable"
	// AuditHandoffReleasePending means the audit row is durable and a
	// bounded-emit worker in THIS PROCESS still owns the event-log flock.
	//
	// TRANSIENT. It clears by itself when the worker finishes its write, and it
	// is the normal state of every in-flight bounded emit in the process — the
	// worker is spawned before the deadline is evaluated, so a healthy
	// sub-millisecond emit passes through this state too
	// (api.SupervisorEventLockOutstanding, and its
	// TestSupervisorEventLockStateOutstandingDuringHealthyBoundedEmit guard).
	//
	// The operator remedy is "wait", NEVER "restart this process". It is
	// reported rather than folded into Durable because a warning channel must
	// fail closed: a wedged writer sits in this same state indefinitely, and the
	// reader cannot tell the two apart at the moment it reads.
	AuditHandoffReleasePending AuditHandoff = "release_pending"
	// AuditHandoffReleaseUnconfirmed means the audit row is durable and a
	// cross-process event-log flock release was ATTEMPTED AND FAILED.
	//
	// PERMANENT for this process's lifetime (api.SupervisorEventLockStranded is
	// never cleared). This process holds the flock until it exits, blocking
	// every other emitter — the supervisor, the install CLI. The only remedy is
	// restarting this process, and it is never "retry this recovery".
	//
	// SCOPE: this value used to also carry the transient AuditHandoffReleasePending
	// case above. Collapsing the two meant a consumer applying the permanent
	// remedy ("restart mcphub") to a healthy concurrent emit. They are separate
	// values precisely so each consumer can state the truthful remedy.
	AuditHandoffReleaseUnconfirmed AuditHandoff = "release_unconfirmed"
)

// Valid reports whether the value belongs to the safe response enum.
func (a AuditHandoff) Valid() bool {
	switch a {
	case AuditHandoffNotRequired, AuditHandoffDurable, AuditHandoffReleasePending, AuditHandoffReleaseUnconfirmed:
		return true
	default:
		return false
	}
}

// Result means the supervisor accepted one force-respawn request. It does not
// assert that the daemon is Running yet.
type Result struct {
	TaskName             string
	Reaped               bool
	PortOwnerCheck       PortOwnerCheck
	PortWaitOutcome      PortWaitOutcome
	AuditHandoff         AuditHandoff
	TerminationCommitted bool
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
	FailureAuditDurability           FailureKind = "audit_durability_failed"
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
	var committedAudit *committedAuditFinalizer

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
			var committedEvent api.SupervisorEvent
			if terminateErr == nil {
				reaped = true
				check = PortOwnerReaped
				postCommitNotifications = append(postCommitNotifications, Notification{Kind: NotificationReaped, TaskName: normalized, Port: port, PID: ownerPID})
				committedEvent = auditEvent("daemon-port-squatter-reaped", "recover", verdict, *descriptor, ownerPID, identity, map[string]any{
					"note": "operator recover: verified-own port squatter exit confirmed, forcing a respawn",
				})
			} else {
				check = PortOwnerTerminationUnconfirmed
				postCommitNotifications = append(postCommitNotifications, Notification{Kind: NotificationTerminationUnconfirmed, TaskName: normalized, Port: port, PID: ownerPID, Cause: terminateErr})
				committedEvent = auditEvent("daemon-port-squatter-termination-unconfirmed", "recover", verdict, *descriptor, ownerPID, identity, map[string]any{
					"err":  BoundEventField(terminateErr.Error()),
					"note": "operator recover: termination committed but process exit was not confirmed; forcing a respawn",
				})
			}
			committedEvent.TS = postKillStarted.UTC().Format(time.RFC3339Nano)
			prepared, prepareErr := api.PrepareSupervisorEvent(committedEvent)
			committedAudit = &committedAuditFinalizer{
				stateDir:   stateDir,
				prepared:   prepared,
				prepareErr: prepareErr,
			}
			// Charge the bounded pre-respawn audit only to the non-reserved
			// post-kill slice. Any time it consumes reduces the subsequent port
			// wait; the fresh RespawnReserve context below remains untouched.
			auditBudget := boundedPortWaitBudget(deps.AuditEmitTimeout, deps.PostKillTimeout, deps.RespawnReserve, deps.Now().Sub(postKillStarted))
			if prepareErr == nil && auditBudget > 0 {
				committedAudit.attempted = true
				committedAudit.pending, committedAudit.emitErr = emitRecoverPreparedAuditWithTimeoutTracked(stateDir, prepared, auditBudget)
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
	// The committed audit establishes its durable handoff after the injected
	// supervisor-dispatch boundary; the no-kill already-exited audit remains an
	// independent best-effort closure.
	// finalize is nil-receiver safe: with no committed audit it reports
	// AuditHandoffNotRequired and no error.
	auditHandoff, auditDurabilityErr := committedAudit.finalize()
	for _, emitAudit := range postCommitAudits {
		emitAudit()
	}
	for _, notification := range postCommitNotifications {
		notify(options, notification)
	}
	result := Result{
		TaskName:             normalized,
		Reaped:               reaped,
		PortOwnerCheck:       check,
		PortWaitOutcome:      portWaitOutcome,
		AuditHandoff:         auditHandoff,
		TerminationCommitted: terminationCommitted,
	}
	if auditDurabilityErr != nil {
		cause := auditDurabilityErr
		if respawnErr != nil {
			cause = errors.Join(auditDurabilityErr, respawnErr)
		}
		return result, &OperationError{
			Kind:     FailureAuditDurability,
			TaskName: normalized,
			Cause:    cause,
			Respawn:  respawn,
		}
	}
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

// emitRecoverPreparedAuditWithTimeoutTracked preserves the exact prepared
// bytes while exposing a timed-out worker's eventual outcome to the
// post-respawn durability finalizer.
func emitRecoverPreparedAuditWithTimeoutTracked(stateDir string, prepared api.PreparedSupervisorEvent, timeout time.Duration) (*api.PendingSupervisorEventEmit, error) {
	logger, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		return nil, err
	}
	defer func() { _ = logger.Close() }()
	return logger.EmitPreparedWithTimeoutTracked(prepared, timeout)
}

// committedAuditFinalizer owns one normalized committed-recovery record across
// the bounded pre-respawn attempt and the post-respawn durability boundary.
// It never remarshals the event, and it never reacquires the event-log flock
// after a release failure.
type committedAuditFinalizer struct {
	stateDir   string
	prepared   api.PreparedSupervisorEvent
	prepareErr error
	attempted  bool
	pending    *api.PendingSupervisorEventEmit
	emitErr    error
}

// logPath is the single place this finalizer names the supervisor event log it
// writes to and reads lock health for. Both must key off the SAME path or the
// verdict would be read from a different lock than the one taken.
func (f *committedAuditFinalizer) logPath() string {
	return filepath.Join(f.stateDir, api.SupervisorEventLogFileLeaf)
}

// handoff reads the SINGLE owner of "can this process still be holding the
// supervisor-events.log flock" and translates it into the wire enum.
//
// It is a READ, never a derivation. That is the whole point: the previous shape
// started from an optimistic `AuditHandoffDurable` and DOWNGRADED it in
// enumerated release-failure branches, so any outcome nobody had enumerated
// silently inherited "confirmed released". The abandoned-worker outcome was
// exactly such an outcome — `Wait(0)` returning ErrSupervisorEventEmitTimeout
// means the worker still owns BOTH locks — and it reported `durable` while the
// flock was provably held. Starting from an owner read has no optimistic
// default to inherit.
//
// SCOPE NOTE: this answers a PROCESS-scoped question, not a per-recovery one.
// A recovery can report release_unconfirmed because an UNRELATED emitter in
// this process stranded the lock. That is deliberate and fail-closed: the
// operator remedy ("restart this process") is identical either way, and the
// per-recovery scoping was the category error that produced this defect class.
// Decision: work-items/decisions/2026-07-27-supervisor-event-flock-release-single-owner.md
// The mapping is 1:1 with the owner's three states and deliberately loses
// nothing. An earlier shape collapsed Outstanding and Stranded into
// AuditHandoffReleaseUnconfirmed. That was defensible as fail-closed AT THIS
// layer, but the only long-lived consumer — the GUI Dashboard — has to pick a
// remedy from the value alone, and the two states have OPPOSITE remedies
// ("wait" vs "restart this process"). A consumer given one value for both must
// either understate a permanent strand or raise a permanent, undismissable
// alarm for a healthy concurrent emit; it chose the latter. Restoring the
// distinction here is what lets every consumer state the truthful remedy
// instead of compensating for a lossy field.
func (f *committedAuditFinalizer) handoff() AuditHandoff {
	switch api.SupervisorEventLockStateForPath(f.logPath()) {
	case api.SupervisorEventLockReleased:
		return AuditHandoffDurable
	case api.SupervisorEventLockOutstanding:
		return AuditHandoffReleasePending
	case api.SupervisorEventLockStranded:
		return AuditHandoffReleaseUnconfirmed
	default:
		// A state this mapping does not know cannot be claimed as released.
		// Fail closed onto the stronger of the two warnings.
		return AuditHandoffReleaseUnconfirmed
	}
}

// finalize returns the audit handoff verdict alongside the durability error.
//
// The two results answer DIFFERENT questions and must not be collapsed. The
// error answers "is the audit row durable?" — a non-nil error is a genuine
// FailureAuditDurability. The AuditHandoff answers "can this process still be
// holding the cross-process event-log flock?" — an unconfirmed release says
// nothing about the row (it is durable) and everything about the LOCK, which
// this process may hold for its whole lifetime because SupervisorEventLog.Close
// is a no-op that does not unlock.
//
// The handoff verdict is NOT computed here. It is read from the single
// process-scoped owner in internal/api (see handoff above). What stays local is
// the one genuinely per-call decision: whether an opportunistic replay may
// reacquire the flock. Keeping BOTH a local enumeration and the owner read
// would be the fix-layering this change exists to remove.
func (f *committedAuditFinalizer) finalize() (AuditHandoff, error) {
	if f == nil {
		return AuditHandoffNotRequired, nil
	}
	if f.prepareErr != nil {
		return AuditHandoffNotRequired, fmt.Errorf("prepare committed recovery audit: %w", f.prepareErr)
	}

	// A release failure (confirmed, or still unresolved in an abandoned worker)
	// means this process may still hold the flock, so an opportunistic replay
	// must not try to take it again.
	replay := true
	switch {
	case !f.attempted:
		// No writer exists. Establish the durable carrier below.
	case f.pending == nil && f.emitErr == nil:
		return f.handoff(), nil
	case f.pending == nil && errors.Is(f.emitErr, api.ErrSupervisorEventReleaseFailed):
		replay = false
	case f.pending != nil:
		switch waitErr := f.pending.Wait(0); {
		case waitErr == nil:
			return f.handoff(), nil
		case errors.Is(waitErr, api.ErrSupervisorEventReleaseFailed),
			errors.Is(waitErr, api.ErrSupervisorEventEmitTimeout):
			// EmitTimeout here is NOT "nothing happened": Wait's non-blocking
			// probe returns it precisely while the abandoned worker is still
			// inside its write holding both locks.
			replay = false
		}
	}

	logger, err := api.OpenSupervisorEventLog(f.logPath())
	if err != nil {
		return f.handoff(), fmt.Errorf("open committed recovery audit handoff: %w", err)
	}
	defer func() { _ = logger.Close() }()
	if err := logger.PersistPending(f.prepared); err != nil {
		return f.handoff(), fmt.Errorf("persist committed recovery audit handoff: %w", err)
	}
	if replay {
		// Persistence is the acknowledgement boundary. Replay stays deliberately
		// opportunistic: contention or a replay I/O error leaves the carrier for
		// a later process and does not undo established durability, so it is
		// still discarded. Its RELEASE outcome is not discarded — TryReplayPending
		// reports that to the same owner f.handoff() reads below.
		_ = logger.TryReplayPending()
	}
	return f.handoff(), nil
}

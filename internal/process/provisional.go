package process

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

const provisionalStartupFailureCleanupBudget = 5 * time.Second

// ProvisionalProcess owns the at-create containment and original process until
// current members, its direct wait and all handles have been settled.
type ProvisionalProcess struct {
	cmd   *exec.Cmd
	job   *Job
	owner containedChild

	mu                sync.Mutex
	waitOnce          sync.Once
	waitDone          chan struct{}
	waitResult        containedWaitResult
	closeAttempted    bool
	closeReturned     bool
	identityInFlight  bool
	closureInFlight   bool
	closeErr          error
	settled           bool
	creatorSettlement *CreatorSettlement
	creatorTerminal   *CreatorTerminalResult
	terminalStarted   bool
	terminalInFlight  bool
}

type CreatorRootExitSnapshot struct {
	Known             bool
	OriginalPID       int
	OriginalStartedAt string
	ExitCode          int
	RawExitError      *exec.ExitError
}

type creatorTerminalAction string

const (
	creatorTerminalNotRequested     creatorTerminalAction = "not-requested"
	creatorTerminalRequestedUnknown creatorTerminalAction = "requested-unknown"
	creatorTerminalRequestSucceeded creatorTerminalAction = "request-succeeded"
	creatorTerminalRequestFailed    creatorTerminalAction = "request-failed"
)

type CreatorTerminalSnapshot struct {
	Root                     CreatorRootExitSnapshot
	Action                   string
	CurrentMembersExited     bool
	HandlesClosed            bool
	ActionError              error
	CloseError               error
	Error                    error
	StartupReleaseKnown      bool
	StartupResourcesReleased bool
	StartupReleaseError      error
	Native                   CreatorJobObservation
}

// CreatorJobObservation is captured by the original Job owner before handles
// close. It is current-cohort evidence, not reconstructed historical ancestry.
type CreatorJobObservation struct {
	Version                uint32                     `json:"version"`
	InitialAccountingKnown bool                       `json:"initial_accounting_known"`
	InitialTotal           uint32                     `json:"initial_total"`
	InitialActive          uint32                     `json:"initial_active"`
	MemberListKnown        bool                       `json:"member_list_known"`
	AssignedMembers        uint32                     `json:"assigned_members"`
	MemberListQueryKnown   bool                       `json:"member_list_query_known"`
	RawAssignedMembers     uint32                     `json:"raw_assigned_members"`
	CapturedMemberPIDs     []uint32                   `json:"captured_member_pids"`
	Members                []CreatorMemberObservation `json:"members"`
	OriginalRootWaitKnown  bool                       `json:"original_root_wait_known"`
	OriginalRootWaitResult uint32                     `json:"original_root_wait_result"`
	FinalAccountingKnown   bool                       `json:"final_accounting_known"`
	FinalTotal             uint32                     `json:"final_total"`
	FinalActive            uint32                     `json:"final_active"`
	MemberHandlesClosed    bool                       `json:"member_handles_closed"`
}

type CreatorMemberObservation struct {
	PID                    uint32 `json:"pid"`
	OriginalJobMemberKnown bool   `json:"original_job_member_known"`
	WaitKnown              bool   `json:"wait_known"`
	WaitResult             uint32 `json:"wait_result"`
	HandleClosed           bool   `json:"handle_closed"`
	ControlledFailure      string `json:"controlled_failure"`
}

func copyCreatorJobObservation(value CreatorJobObservation) CreatorJobObservation {
	if value.CapturedMemberPIDs != nil {
		value.CapturedMemberPIDs = append(make([]uint32, 0, len(value.CapturedMemberPIDs)), value.CapturedMemberPIDs...)
	}
	if value.Members != nil {
		value.Members = append(make([]CreatorMemberObservation, 0, len(value.Members)), value.Members...)
	}
	return value
}

type creatorStartupError struct {
	owner       *ProvisionalProcess
	stage       string
	cause       error
	observation CreatorTerminalSnapshot
}

func (e *creatorStartupError) Error() string { return e.cause.Error() }
func (e *creatorStartupError) Unwrap() error { return e.cause }
func (e *creatorStartupError) Snapshot() CreatorTerminalSnapshot {
	if e == nil || e.owner == nil {
		return CreatorTerminalSnapshot{}
	}
	s := e.observation
	s.Root = copyCreatorRootExitSnapshot(s.Root)
	return s
}

type provisionalCreatorStartupObserver interface {
	creatorStartupSnapshot() (CreatorRootExitSnapshot, error)
}

func (p *ProvisionalProcess) captureStartupIdentity() (CreatorTerminalSnapshot, error) {
	observer, ok := p.owner.(provisionalCreatorStartupObserver)
	if !ok {
		return CreatorTerminalSnapshot{}, creatorClosureUnavailable(errors.New("original startup identity observation is unsupported"))
	}
	root, err := observer.creatorStartupSnapshot()
	return CreatorTerminalSnapshot{Root: copyCreatorRootExitSnapshot(root)}, err
}

func (p *ProvisionalProcess) closeStartupFailure(stage string, cause error) error {
	p.mu.Lock()
	view, identityErr := p.captureStartupIdentity()
	firstClose := !p.closeAttempted
	p.mu.Unlock()
	closeErr := p.close()
	if !firstClose {
		closeErr = errors.Join(closeErr, creatorClosureUnavailable(errors.New("cached closure cannot establish startup release")))
	}
	view.StartupReleaseKnown = firstClose
	view.StartupResourcesReleased = firstClose && closeErr == nil
	view.StartupReleaseError = closeErr
	view.Error = errors.Join(cause, identityErr, closeErr)
	return &creatorStartupError{owner: p, stage: stage, cause: view.Error, observation: view}
}

// CreatorTerminalResult retains action evidence separately from natural closure.
// Only this original process owner can populate its private proof.
type CreatorTerminalResult struct {
	owner       *ProvisionalProcess
	observation CreatorTerminalSnapshot
	closure     *CreatorSettlement
}

func copyCreatorRootExitSnapshot(s CreatorRootExitSnapshot) CreatorRootExitSnapshot {
	s.RawExitError = copyCreatorRawExitError(s.RawExitError)
	return s
}

func copyCreatorRawExitError(raw *exec.ExitError) *exec.ExitError {
	if raw == nil {
		return nil
	}
	copied := *raw
	copied.Stderr = append([]byte(nil), raw.Stderr...)
	return &copied
}

func (r *CreatorTerminalResult) Snapshot() CreatorTerminalSnapshot {
	if r == nil || r.owner == nil {
		return CreatorTerminalSnapshot{}
	}
	s := r.observation
	s.Root = copyCreatorRootExitSnapshot(s.Root)
	s.Native = copyCreatorJobObservation(s.Native)
	return s
}

func (r *CreatorTerminalResult) Closure() *CreatorSettlement {
	if r == nil || r.owner == nil || r.observation.Error != nil {
		return nil
	}
	return r.closure
}

type creatorTerminalObservation struct {
	action        creatorTerminalAction
	actionErr     error
	membersExited bool
	native        CreatorJobObservation
}

type provisionalCreatorTerminalObserver interface {
	completeCreatorMembers(context.Context) (creatorTerminalObservation, error)
}

type provisionalCreatorShutdownObserver interface {
	terminateCreatorMembers(context.Context) (creatorTerminalObservation, error)
}

// The Windows Job supplies this private preparation seam. Other platforms
// refuse the additive start without changing their ordinary starters.
type provisionalCreatorStarter interface {
	prepareCreatorStart(*exec.Cmd) (containedChild, func() error, error)
}

// StartProvisionalWithJob consumes the caller's dedicated fresh Job only after
// authoritative empty admission. Retention precedes native process creation;
// every subsequent error returns the same owner, including callback failure.
func StartProvisionalWithJob(job *Job, cmd *exec.Cmd, retain func(*ProvisionalProcess) error) (*ProvisionalProcess, error) {
	return startProvisionalWithJobContext(nil, job, cmd, retain)
}

// Context-aware admission shares the original retained creator. A canceled
// acquisition keeps partial custody for the retirement owner instead of
// starting another cleanup interval after the shutdown seal.
func StartProvisionalWithJobContext(ctx context.Context, job *Job, cmd *exec.Cmd, retain func(*ProvisionalProcess) error) (*ProvisionalProcess, error) {
	if ctx == nil {
		return nil, &ProvisionalNoProcessError{cause: errors.New("original acquisition context required")}
	}
	if err := ctx.Err(); err != nil {
		return nil, &ProvisionalNoProcessError{cause: err}
	}
	return startProvisionalWithJobContext(ctx, job, cmd, retain)
}

func startProvisionalWithJobContext(ctx context.Context, job *Job, cmd *exec.Cmd, retain func(*ProvisionalProcess) error) (*ProvisionalProcess, error) {
	if job == nil || cmd == nil || cmd.Path == "" || cmd.Process != nil || cmd.ProcessState != nil || retain == nil {
		return nil, &ProvisionalNoProcessError{cause: errors.New("retained creator start requires a fresh dedicated Job, command and retention callback")}
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		return nil, &ProvisionalNoProcessError{cause: creatorClosureUnavailable(errors.New("configured standard streams are unsupported by retained creator start"))}
	}
	starter, ok := any(job).(provisionalCreatorStarter)
	if !ok {
		return nil, &ProvisionalNoProcessError{cause: creatorClosureUnavailable(errors.New("retained creator start is unsupported on this platform"))}
	}
	child, start, err := starter.prepareCreatorStart(cmd)
	if err != nil {
		return nil, &ProvisionalNoProcessError{cause: err}
	}
	p := &ProvisionalProcess{cmd: cmd, job: job, owner: child, waitDone: make(chan struct{})}
	retentionErr := func() (err error) {
		defer func() {
			if value := recover(); value != nil {
				if cause, ok := value.(error); ok {
					err = fmt.Errorf("creator retention callback panic: %w", cause)
				} else {
					err = fmt.Errorf("creator retention callback panic: %v", value)
				}
			}
		}()
		return retain(p)
	}()
	if retentionErr != nil {
		return p, p.closeStartupFailure("retention", retentionErr)
	}
	if ctx != nil && ctx.Err() != nil {
		return p, p.closeStartupFailure("pre-create", ctx.Err())
	}
	prepareProcessGroup(cmd)
	startCause := start()
	if ctx != nil {
		startCause = errors.Join(startCause, ctx.Err())
	}
	if err := startCause; err != nil {
		startErr := fmt.Errorf("start retained provisional process: %w", err)
		if cmd.Process == nil && !errors.Is(err, ErrSpawnPostCreate) {
			return p, p.closeStartupFailure("pre-create", startErr)
		}
		p.mu.Lock()
		view, identityErr := p.captureStartupIdentity()
		p.mu.Unlock()
		if ctx != nil {
			if ctx.Err() != nil {
				view.Error = errors.Join(startErr, identityErr, ctx.Err())
				return p, &creatorStartupError{owner: p, stage: "post-create", cause: view.Error, observation: view}
			}
			cleanupCtx, cancel := context.WithTimeout(ctx, provisionalStartupFailureCleanupBudget)
			result, cleanupErr := p.TerminateCreatorAndWait(cleanupCtx)
			cancel()
			if result != nil {
				view = result.Snapshot()
				view.StartupReleaseKnown = result.Closure() != nil
				view.StartupResourcesReleased = view.StartupReleaseKnown
			}
			view.StartupReleaseError = cleanupErr
			view.Error = errors.Join(startErr, identityErr, cleanupErr)
			return p, &creatorStartupError{owner: p, stage: "post-create", cause: view.Error, observation: view}
		}
		observed, cleanupErr := p.terminateAndWaitObserved(provisionalStartupFailureCleanupBudget)
		if !observed && cleanupErr == nil {
			cleanupErr = creatorClosureUnavailable(errors.New("cached cleanup cannot establish startup release"))
		}
		view.StartupReleaseKnown = observed
		view.StartupResourcesReleased = observed && cleanupErr == nil
		view.StartupReleaseError = cleanupErr
		view.Error = errors.Join(startErr, identityErr, cleanupErr)
		return p, &creatorStartupError{owner: p, stage: "post-create", cause: view.Error, observation: view}
	}
	return p, nil
}

// WaitCreatorRoot claims the original wait in the calling daemon waiter. The
// claim is released before the blocking wait, so finite observers remain bounded.
func (p *ProvisionalProcess) WaitCreatorRoot() (CreatorRootExitSnapshot, error) {
	if p == nil || p.cmd == nil || p.job == nil || p.owner == nil || p.waitDone == nil {
		return CreatorRootExitSnapshot{}, creatorClosureUnavailable(errors.New("missing original root wait owner"))
	}
	observer, ok := p.owner.(provisionalCreatorObserver)
	if !ok {
		return CreatorRootExitSnapshot{}, creatorClosureUnavailable(errors.New("original root observation is unsupported"))
	}
	p.mu.Lock()
	if p.creatorTerminal != nil {
		snapshot := copyCreatorRootExitSnapshot(p.creatorTerminal.observation.Root)
		p.mu.Unlock()
		if !snapshot.Known {
			return snapshot, creatorClosureUnavailable(errors.New("cached original root exit is unknown"))
		}
		if snapshot.RawExitError != nil {
			return snapshot, snapshot.RawExitError
		}
		return snapshot, nil
	}
	if p.closeAttempted {
		p.mu.Unlock()
		return CreatorRootExitSnapshot{}, creatorClosureUnavailable(errors.New("original root custody has been released"))
	}
	binding, err := observer.creatorBinding()
	p.mu.Unlock()
	if err != nil || binding.pid <= 0 || binding.startedAt == "" {
		return CreatorRootExitSnapshot{}, creatorClosureUnavailable(errors.Join(err, errors.New("original root binding is unavailable")))
	}
	claimed := false
	p.waitOnce.Do(func() { claimed = true })
	if claimed {
		p.publishWait()
	}
	<-p.waitDone
	exit := observer.creatorExitObservation()
	snapshot := copyCreatorRootExitSnapshot(CreatorRootExitSnapshot{Known: exit.known && p.waitResult.exited && p.waitResult.err == nil && exit.err == nil, OriginalPID: binding.pid, OriginalStartedAt: binding.startedAt, ExitCode: exit.code, RawExitError: exit.raw})
	if !snapshot.Known {
		return snapshot, errors.Join(p.waitResult.err, exit.err, creatorClosureUnavailable(errors.New("original root exit is unknown")))
	}
	if snapshot.RawExitError != nil {
		return snapshot, snapshot.RawExitError
	}
	return snapshot, nil
}

func (p *ProvisionalProcess) lockCreatorContext(ctx context.Context) error {
	for {
		if err := creatorClosureContext(ctx); err != nil {
			return err
		}
		if p.mu.TryLock() {
			return nil
		}
		deadline, _ := ctx.Deadline()
		pause := time.Until(deadline)
		if pause > creatorClosureAdmissionInterval {
			pause = creatorClosureAdmissionInterval
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

// CompleteCreatorAfterExit applies only the original creator's positive child
// terminal policy, after genuine root completion, and captures before release.
func (p *ProvisionalProcess) CompleteCreatorAfterExit(ctx context.Context, applyChildTerminalPolicy bool) (*CreatorTerminalResult, error) {
	if err := creatorClosureContext(ctx); err != nil {
		return nil, err
	}
	if !applyChildTerminalPolicy || p == nil || p.cmd == nil || p.job == nil || p.owner == nil || p.waitDone == nil {
		return nil, creatorClosureUnavailable(errors.New("positive terminal policy and original owner are required"))
	}
	observer, ok := p.owner.(provisionalCreatorObserver)
	terminal, supported := p.owner.(provisionalCreatorTerminalObserver)
	if !ok || !supported {
		return nil, creatorClosureUnavailable(errors.New("original terminal observation is unsupported"))
	}
	if err := p.lockCreatorContext(ctx); err != nil {
		return nil, err
	}
	held := true
	defer func() {
		if held {
			p.mu.Unlock()
		}
	}()
	if err := creatorClosureContext(ctx); err != nil {
		return nil, err
	}
	if p.creatorTerminal != nil {
		return p.creatorTerminal, p.creatorTerminal.observation.Error
	}
	if p.terminalStarted || p.terminalInFlight || p.closureInFlight {
		return nil, creatorClosureUnavailable(errors.New("previous terminal policy has no completed outcome; repeating the request is forbidden"))
	}
	if p.closeAttempted {
		return nil, creatorClosureUnavailable(errors.Join(p.closeErr, errors.New("released custody has no terminal observation")))
	}
	select {
	case <-p.waitDone:
	default:
		return nil, creatorClosureUnavailable(errors.New("original root wait is pending"))
	}
	binding, err := observer.creatorBinding()
	if err != nil || binding.pid <= 0 || binding.startedAt == "" {
		return nil, creatorClosureUnavailable(errors.Join(err, errors.New("original terminal binding is unavailable")))
	}
	exit := observer.creatorExitObservation()
	if !p.waitResult.exited || p.waitResult.err != nil || exit.err != nil || !exit.known {
		return nil, creatorClosureUnavailable(errors.Join(p.waitResult.err, exit.err, errors.New("original root exit is unknown")))
	}
	if err := creatorClosureContext(ctx); err != nil {
		return nil, err
	}
	p.mu.Unlock()
	held = false
	permit, admissionErr := beginOriginalOwnerTerminal(ctx, p.owner)
	if admissionErr != nil {
		return nil, admissionErr
	}
	defer permit.finish()
	ctx = withOriginalMemberTerminalPermit(ctx, permit)
	p.mu.Lock()
	if p.terminalStarted || p.terminalInFlight || p.closureInFlight || p.closeAttempted {
		p.mu.Unlock()
		return nil, creatorClosureUnavailable(errors.New("concurrent terminal owner already entered"))
	}
	p.terminalInFlight = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.terminalInFlight = false; p.mu.Unlock() }()
	observation, terminalErr := terminal.completeCreatorMembers(ctx)
	if !permit.nativeWasEntered() {
		return nil, errors.Join(terminalErr, creatorClosureUnavailable(errors.New("original terminal native admission did not enter")))
	}
	p.mu.Lock()
	p.terminalStarted = true
	p.mu.Unlock()
	view := CreatorTerminalSnapshot{Root: copyCreatorRootExitSnapshot(CreatorRootExitSnapshot{Known: true, OriginalPID: binding.pid, OriginalStartedAt: binding.startedAt, ExitCode: exit.code, RawExitError: exit.raw}), Action: string(observation.action), ActionError: observation.actionErr, CurrentMembersExited: observation.membersExited, Error: terminalErr, Native: copyCreatorJobObservation(observation.native)}
	if terminalErr == nil && (!observation.membersExited || (observation.action != creatorTerminalNotRequested && observation.action != creatorTerminalRequestSucceeded)) {
		view.Error = creatorClosureUnavailable(errors.New("terminal member/action evidence is incomplete"))
	}
	if view.Error == nil {
		view.Error = creatorClosureContext(ctx)
	}
	if view.Error == nil {
		view.CloseError = p.closeWithMemberPermit(permit)
		view.HandlesClosed = view.CloseError == nil
		view.Error = view.CloseError
	}
	result := &CreatorTerminalResult{owner: p, observation: view}
	if view.Error == nil {
		snapshot := CreatorSettlementSnapshot{OriginalPID: binding.pid, OriginalStartedAt: binding.startedAt, DirectWaitComplete: true, CurrentMembersExited: true, HandlesClosed: true, OriginalExitCodeKnown: true, OriginalExitCode: exit.code, RawExitError: exit.raw}
		digest := sha256.Sum256([]byte(fmt.Sprintf("creator-terminal-v1|%d|%s|%d|%s", binding.pid, binding.startedAt, exit.code, observation.action)))
		snapshot.EvidenceSHA256 = fmt.Sprintf("%x", digest[:])
		result.closure = &CreatorSettlement{owner: p, observation: copyCreatorSettlementSnapshot(snapshot)}
	}
	p.mu.Lock()
	p.creatorTerminal = result
	p.mu.Unlock()
	if err := creatorClosureContext(ctx); err != nil {
		return nil, errors.Join(err, view.Error)
	}
	return result, view.Error
}

// TerminateCreatorAndWait consumes only the original at-create Job capability
// under an admitted full-supervisor shutdown. The original waiter and terminal
// observer share this cached one-shot result; a partial action never replays.
func (p *ProvisionalProcess) TerminateCreatorAndWait(ctx context.Context) (*CreatorTerminalResult, error) {
	if err := creatorClosureContext(ctx); err != nil {
		return nil, err
	}
	if p == nil || p.cmd == nil || p.job == nil || p.owner == nil || p.waitDone == nil {
		return nil, creatorClosureUnavailable(errors.New("original shutdown creator is unavailable"))
	}
	observer, ok := p.owner.(provisionalCreatorObserver)
	shutdown, supported := p.owner.(provisionalCreatorShutdownObserver)
	if !ok || !supported {
		return nil, creatorClosureUnavailable(errors.New("original shutdown observation is unsupported"))
	}
	if err := p.lockCreatorContext(ctx); err != nil {
		return nil, err
	}
	select {
	case <-p.waitDone:
		p.mu.Unlock()
		return p.CompleteCreatorAfterExit(ctx, true)
	default:
	}
	held := true
	defer func() {
		if held {
			p.mu.Unlock()
		}
	}()
	if p.creatorTerminal != nil {
		return p.creatorTerminal, p.creatorTerminal.observation.Error
	}
	if p.terminalStarted || p.terminalInFlight || p.closureInFlight || p.closeAttempted {
		return nil, creatorClosureUnavailable(errors.Join(p.closeErr, errors.New("prior incomplete action or released custody cannot replay shutdown")))
	}
	binding, err := observer.creatorBinding()
	if err != nil || binding.pid <= 0 || binding.startedAt == "" {
		return nil, creatorClosureUnavailable(errors.Join(err, errors.New("original shutdown binding unavailable")))
	}
	p.mu.Unlock()
	held = false
	permit, admissionErr := beginOriginalOwnerTerminal(ctx, p.owner)
	if admissionErr != nil {
		return nil, admissionErr
	}
	defer permit.finish()
	ctx = withOriginalMemberTerminalPermit(ctx, permit)
	p.mu.Lock()
	if p.terminalStarted || p.terminalInFlight || p.closureInFlight || p.closeAttempted {
		p.mu.Unlock()
		return nil, creatorClosureUnavailable(errors.New("concurrent terminal owner already entered"))
	}
	p.terminalInFlight = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.terminalInFlight = false; p.mu.Unlock() }()
	view := CreatorTerminalSnapshot{Root: CreatorRootExitSnapshot{OriginalPID: binding.pid, OriginalStartedAt: binding.startedAt}, Action: string(creatorTerminalRequestedUnknown)}
	waitDone := p.wait()
	native, actionErr := shutdown.terminateCreatorMembers(ctx)
	if !permit.nativeWasEntered() {
		return nil, errors.Join(actionErr, creatorClosureUnavailable(errors.New("original terminal native admission did not enter")))
	}
	p.mu.Lock()
	p.terminalStarted = true
	p.mu.Unlock()
	view.Action, view.ActionError, view.CurrentMembersExited, view.Native = string(native.action), native.actionErr, native.membersExited, copyCreatorJobObservation(native.native)
	view.Error = actionErr
	if view.Error == nil && (native.action != creatorTerminalRequestSucceeded || !native.membersExited) {
		view.Error = creatorClosureUnavailable(errors.New("original shutdown action/member proof incomplete"))
	}
	if view.Error == nil {
		select {
		case <-waitDone:
		case <-ctx.Done():
			view.Error = ctx.Err()
		}
	}
	if view.Error == nil {
		exit := observer.creatorExitObservation()
		view.Root = copyCreatorRootExitSnapshot(CreatorRootExitSnapshot{Known: p.waitResult.exited && p.waitResult.err == nil && exit.known && exit.err == nil, OriginalPID: binding.pid, OriginalStartedAt: binding.startedAt, ExitCode: exit.code, RawExitError: exit.raw})
		if !view.Root.Known {
			view.Error = creatorClosureUnavailable(errors.Join(p.waitResult.err, exit.err, errors.New("original shutdown root wait incomplete")))
		}
	}
	if view.Error == nil {
		view.Error = creatorClosureContext(ctx)
	}
	if view.Error == nil {
		view.CloseError = p.closeWithMemberPermit(permit)
		view.HandlesClosed = view.CloseError == nil
		view.Error = view.CloseError
	}
	result := &CreatorTerminalResult{owner: p, observation: view}
	if view.Error == nil {
		snapshot := CreatorSettlementSnapshot{OriginalPID: binding.pid, OriginalStartedAt: binding.startedAt, DirectWaitComplete: true, CurrentMembersExited: true, HandlesClosed: true, OriginalExitCodeKnown: true, OriginalExitCode: view.Root.ExitCode, RawExitError: view.Root.RawExitError}
		digest := sha256.Sum256([]byte(fmt.Sprintf("creator-shutdown-v1|%d|%s|%d|%s", binding.pid, binding.startedAt, view.Root.ExitCode, view.Action)))
		snapshot.EvidenceSHA256 = fmt.Sprintf("%x", digest[:])
		result.closure = &CreatorSettlement{owner: p, observation: copyCreatorSettlementSnapshot(snapshot)}
	}
	p.mu.Lock()
	p.creatorTerminal = result
	p.mu.Unlock()
	if err := creatorClosureContext(ctx); err != nil {
		return result, errors.Join(err, view.Error)
	}
	return result, view.Error
}

type CreatorSettlementSnapshot struct {
	OriginalPID           int
	OriginalStartedAt     string
	DirectWaitComplete    bool
	CurrentMembersExited  bool
	HandlesClosed         bool
	OriginalExitCodeKnown bool
	OriginalExitCode      int
	RawExitError          *exec.ExitError
	EvidenceSHA256        string
}

// CreatorSettlement has no public authority-bearing fields or constructor.
// Its snapshot describes resource closure, not application success/readiness.
type CreatorSettlement struct {
	owner       *ProvisionalProcess
	observation CreatorSettlementSnapshot
}

func copyCreatorSettlementSnapshot(s CreatorSettlementSnapshot) CreatorSettlementSnapshot {
	s.RawExitError = copyCreatorRawExitError(s.RawExitError)
	return s
}

func (s *CreatorSettlement) Snapshot() CreatorSettlementSnapshot {
	if s == nil || s.owner == nil {
		return CreatorSettlementSnapshot{}
	}
	return copyCreatorSettlementSnapshot(s.observation)
}

type creatorClosureBinding struct {
	pid       int
	startedAt string
}
type creatorExitObservation struct {
	known bool
	code  int
	raw   *exec.ExitError
	err   error
}

// Only the retained Windows child implements this production capability.
type provisionalCreatorObserver interface {
	creatorBinding() (creatorClosureBinding, error)
	observeCreatorMembers(context.Context) error
	creatorExitObservation() creatorExitObservation
}

type creatorClosureUnavailableError struct{ cause error }

func (e *creatorClosureUnavailableError) Error() string {
	return "creator closure unavailable: " + e.cause.Error()
}
func (e *creatorClosureUnavailableError) Unwrap() error { return e.cause }
func creatorClosureUnavailable(cause error) error {
	return &creatorClosureUnavailableError{cause: cause}
}

func creatorClosureContext(ctx context.Context) error {
	if ctx == nil {
		return creatorClosureUnavailable(errors.New("finite context is required"))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return creatorClosureUnavailable(errors.New("context deadline is required"))
	}
	if !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

const creatorClosureAdmissionInterval = time.Millisecond

// WaitCreatorClosure observes only this original owner's natural/graceful
// closure. It never requests termination or reconstructs historical custody.
func (p *ProvisionalProcess) WaitCreatorClosure(ctx context.Context) (*CreatorSettlement, error) {
	if err := creatorClosureContext(ctx); err != nil {
		return nil, err
	}
	if p == nil || p.cmd == nil || p.job == nil || p.owner == nil || p.waitDone == nil {
		return nil, creatorClosureUnavailable(errors.New("missing original creator custody"))
	}
	observer, ok := p.owner.(provisionalCreatorObserver)
	if !ok {
		return nil, creatorClosureUnavailable(errors.New("original creator observation is unsupported"))
	}
	if err := p.lockCreatorContext(ctx); err != nil {
		return nil, err
	}
	held := true
	defer func() {
		if held {
			p.mu.Unlock()
		}
	}()
	if err := creatorClosureContext(ctx); err != nil {
		return nil, err
	}
	if p.creatorSettlement != nil {
		return p.creatorSettlement, nil
	}
	if p.terminalStarted {
		return nil, creatorClosureUnavailable(errors.New("terminal child policy cannot supply natural closure evidence"))
	}
	if p.closeAttempted {
		if p.closeErr != nil {
			return nil, p.closeErr
		}
		return nil, creatorClosureUnavailable(errors.New("legacy closure has no creator observation"))
	}
	binding, err := observer.creatorBinding()
	if err != nil {
		return nil, creatorClosureUnavailable(err)
	}
	if binding.pid <= 0 || binding.startedAt == "" {
		return nil, creatorClosureUnavailable(errors.New("incomplete original creation identity"))
	}
	if err := creatorClosureContext(ctx); err != nil {
		return nil, err
	}
	p.mu.Unlock()
	held = false
	permit, admissionErr := beginOriginalOwnerTerminal(ctx, p.owner)
	if admissionErr != nil {
		return nil, admissionErr
	}
	defer permit.finish()
	ctx = withOriginalMemberTerminalPermit(ctx, permit)
	p.mu.Lock()
	if p.terminalStarted || p.closeAttempted || p.closureInFlight {
		p.mu.Unlock()
		return nil, creatorClosureUnavailable(errors.New("concurrent terminal owner already entered"))
	}
	p.closureInFlight = true
	p.mu.Unlock()
	defer func() { p.mu.Lock(); p.closureInFlight = false; p.mu.Unlock() }()
	select {
	case <-p.wait():
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// waitDone publishes both the existing result and the Windows raw observation.
	var exitErr *exec.ExitError
	if p.waitResult.err != nil && !errors.As(p.waitResult.err, &exitErr) {
		return nil, p.waitResult.err
	}
	if !p.waitResult.exited {
		return nil, creatorClosureUnavailable(errors.New("original direct wait did not prove exit"))
	}
	exit := observer.creatorExitObservation()
	if exit.err != nil {
		return nil, exit.err
	}
	if !exit.known {
		return nil, creatorClosureUnavailable(errors.New("original ProcessState exit observation is missing"))
	}
	// A nil *ExitError must remain a nil error interface; otherwise Join keeps
	// a typed nil whose Error method dereferences the absent ExitError.
	var exitCause error
	if exit.raw != nil {
		exitCause = exit.raw
	}
	if err := creatorClosureContext(ctx); err != nil {
		return nil, errors.Join(err, exitCause)
	}
	if err := observer.observeCreatorMembers(ctx); err != nil {
		return nil, errors.Join(err, exitCause)
	}
	if err := creatorClosureContext(ctx); err != nil {
		return nil, errors.Join(err, exitCause)
	}
	closeErr := p.closeWithMemberPermit(permit)
	if closeErr == nil {
		snapshot := CreatorSettlementSnapshot{OriginalPID: binding.pid, OriginalStartedAt: binding.startedAt, DirectWaitComplete: true, CurrentMembersExited: true, HandlesClosed: true, OriginalExitCodeKnown: true, OriginalExitCode: exit.code, RawExitError: exit.raw}
		digest := sha256.Sum256([]byte(fmt.Sprintf("creator-closure-v1|%d|%s|%d", binding.pid, binding.startedAt, exit.code)))
		snapshot.EvidenceSHA256 = fmt.Sprintf("%x", digest[:])
		p.mu.Lock()
		p.creatorSettlement = &CreatorSettlement{owner: p, observation: copyCreatorSettlementSnapshot(snapshot)}
		p.mu.Unlock()
	}
	if err := creatorClosureContext(ctx); err != nil {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return p.creatorSettlement, nil
}

// ProvisionalNoProcessError certifies a refusal before invoking the starter.
// A nested matching cause cannot certify another joined outcome.
type ProvisionalNoProcessError struct{ cause error }

func (e *ProvisionalNoProcessError) Error() string { return e.cause.Error() }
func (e *ProvisionalNoProcessError) Unwrap() error { return e.cause }

// StartProvisional returns an unsettled owner together with a startup error if
// bounded startup-failure cleanup cannot certify settlement. Callers must keep
// that owner's custody and join its cleanup outcome before discarding it.
func StartProvisional(cmd *exec.Cmd) (*ProvisionalProcess, error) {
	return startProvisional(cmd, NewKillOnCloseJob)
}

func startProvisional(cmd *exec.Cmd, newJob func() (*Job, error)) (*ProvisionalProcess, error) {
	return startProvisionalWithStarter(cmd, newJob, startProvisionalWithJob)
}

func startProvisionalWithStarter(cmd *exec.Cmd, newJob func() (*Job, error), start func(*Job, *exec.Cmd) (containedChild, error)) (*ProvisionalProcess, error) {
	if cmd == nil || cmd.Path == "" {
		return nil, &ProvisionalNoProcessError{cause: errors.New("start provisional process: command is nil or has an empty path")}
	}
	job, err := newJob()
	if err != nil || job == nil {
		if err == nil {
			err = errors.New("job creation returned nil job")
		}
		return nil, &ProvisionalNoProcessError{cause: fmt.Errorf("start provisional process containment: %w", err)}
	}
	p := &ProvisionalProcess{cmd: cmd, job: job, waitDone: make(chan struct{})}
	prepareProcessGroup(cmd)
	p.owner, err = start(job, cmd)
	if err == nil {
		return p, nil
	}
	startErr := fmt.Errorf("start provisional process: %w", err)
	if cmd.Process == nil && !errors.Is(err, ErrSpawnPostCreate) {
		closeErr := p.close()
		if closeErr == nil {
			return nil, startErr
		}
		return p, errors.Join(startErr, closeErr)
	}
	cleanupErr := p.TerminateAndWait(provisionalStartupFailureCleanupBudget)
	if cleanupErr == nil {
		return nil, startErr
	}
	return p, errors.Join(startErr, cleanupErr)
}

func (p *ProvisionalProcess) wait() <-chan struct{} {
	claimed := false
	p.waitOnce.Do(func() { claimed = true })
	if claimed {
		go p.publishWait()
	}
	return p.waitDone
}

func (p *ProvisionalProcess) publishWait() {
	p.waitResult = p.owner.wait()
	close(p.waitDone)
}

func (p *ProvisionalProcess) close() error {
	permit, err := beginOriginalOwnerTerminal(nil, p.owner)
	if err != nil {
		return err
	}
	defer permit.finish()
	return p.closeWithMemberPermit(permit)
}

// TerminateAndWait uses the current-tree projection, never strict historical
// coverage. An unresolved error retains the owner; confirmed cleanup is stable
// on repeated calls. Expected termination exits are not wait failures.
func (p *ProvisionalProcess) TerminateAndWait(timeout time.Duration) error {
	_, err := p.terminateAndWaitObserved(timeout)
	return err
}

// observed is true only at this call's completed original wait/current-member
// and first remembered-close branch. Legacy cached returns remain unchanged.
func (p *ProvisionalProcess) terminateAndWaitObserved(timeout time.Duration) (observed bool, err error) {
	if p == nil || p.cmd == nil || p.job == nil {
		return false, errors.New("terminate provisional process: invalid owner")
	}
	if timeout < time.Millisecond || timeout > maxContainedCleanupTimeout || timeout%time.Millisecond != 0 {
		return false, fmt.Errorf("terminate provisional process: %w", ErrInvalidCleanupTimeout)
	}
	p.mu.Lock()
	held := true
	defer func() {
		if held {
			p.mu.Unlock()
		}
	}()
	if p.closeAttempted {
		return false, p.closeErr
	}
	if p.owner == nil {
		return false, errors.New("terminate provisional process: missing creator binding")
	}
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var permit *originalMemberTerminalPermit
	if _, participating := p.owner.(originalMemberTerminalOwner); participating {
		p.mu.Unlock()
		held = false
		var admissionErr error
		permit, admissionErr = beginOriginalOwnerTerminal(ctx, p.owner)
		if admissionErr != nil {
			return false, admissionErr
		}
		defer permit.finish()
		ctx = withOriginalMemberTerminalPermit(ctx, permit)
	}
	waitDone := p.wait()
	var terminateErr error
	if original, ok := p.owner.(interface{ terminateOriginalWithPermit(context.Context) error }); ok {
		terminateErr = original.terminateOriginalWithPermit(ctx)
	} else if deadlineOwner, ok := p.owner.(containedDeadlineChild); ok {
		terminateErr = deadlineOwner.terminateBy(deadline)
	} else {
		terminateErr = p.owner.terminate(uint32(timeout / time.Millisecond))
	}
	if terminateErr != nil {
		return false, terminateErr
	}
	result, completed := containedWaitResult{}, false
	if deferred, ok := p.owner.(containedDeferredReapChild); ok {
		result, completed = deferred.reapBy(deadline)
	}
	if !completed {
		select {
		case <-waitDone:
			result, completed = p.waitResult, true
		default:
		}
	}
	if !completed {
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		select {
		case <-waitDone:
			result, completed = p.waitResult, true
		case <-timer.C:
			select {
			case <-waitDone:
				result, completed = p.waitResult, true
			default:
			}
		}
	}
	if !completed {
		return false, errors.Join(ErrCleanupTimeout, errors.New("provisional creator wait remains unsettled"))
	}
	var exitErr *exec.ExitError
	if result.err != nil && !errors.As(result.err, &exitErr) {
		return false, result.err
	}
	if !result.exited {
		return false, errors.New("provisional creator exit remains unproved")
	}
	if held {
		return true, p.closeLegacyMetadataHeld()
	}
	return true, p.closeWithMemberPermit(permit)
}

// Nonparticipating legacy cleanup retains its original serialized contract.
func (p *ProvisionalProcess) closeLegacyMetadataHeld() error {
	return p.closeWithMemberPermitMode(nil, true)
}

package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"mcp-local-hub/internal/process"
)

// The two existing one-second Stop phases share this absolute bound. A
// lifetime context never supplies a timeout for a healthy stdio service.
const stdioOriginalCleanupTimeout = 2 * time.Second

// StdioHostSettlement is a receiving view of this host's retained attempt.
// It is not a native capability or a constructor for process-settlement proof.
type StdioHostSettlement struct {
	OriginalIdentity                                                      process.ContainedRootIdentity
	RootWait                                                              process.OriginalRootWait
	NativeSettlement                                                      *process.ContainedProcessSettlement
	Deadline                                                              time.Time
	PrimaryError, CleanupError                                            error
	NoNativeAttempt, JobClosed, FilesClosed, ReadersJoined, WritersJoined bool
	WatcherJoined, LifetimeJoined, LogClosed, Pending                     bool
}

// Error callers retain the SAME host and attempt even when terminal evidence
// is incomplete. Garbage collection is not a substitute for handle receiving.
type stdioOriginalSettlementError struct {
	host       *StdioHost
	settlement StdioHostSettlement
	cause      error
}

func (e *stdioOriginalSettlementError) Error() string { return e.cause.Error() }
func (e *stdioOriginalSettlementError) Unwrap() error { return e.cause }

func (h *StdioHost) originalSettlementError(result StdioHostSettlement, cause error) error {
	if cause == nil {
		return nil
	}
	return &stdioOriginalSettlementError{host: h, settlement: result, cause: cause}
}

func (s StdioHostSettlement) Complete() bool {
	return !s.Pending && s.CleanupError == nil && s.JobClosed && s.FilesClosed &&
		s.ReadersJoined && s.WritersJoined && s.WatcherJoined &&
		s.LifetimeJoined && s.LogClosed && (s.NoNativeAttempt || s.NativeSettlement.Complete())
}

type stdioOriginalFile struct {
	file       *os.File
	closeOnce  sync.Once
	closeError error
}

func (f *stdioOriginalFile) close() error {
	if f == nil || f.file == nil {
		return nil
	}
	f.closeOnce.Do(func() { f.closeError = f.file.Close() })
	return f.closeError
}

// stdin admission and its writers belong to the same host lifetime. Closing
// the actual pipe interrupts a blocked Write before receiving its worker.
type stdioOriginalInput struct {
	file     *stdioOriginalFile
	mu       sync.Mutex
	retired  bool
	writers  sync.WaitGroup
	joinOnce sync.Once
	joined   chan struct{}
}

func (s *stdioOriginalInput) Write(b []byte) (int, error) {
	s.mu.Lock()
	if s.retired {
		s.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	s.writers.Add(1)
	s.mu.Unlock()
	defer s.writers.Done()
	return s.file.file.Write(b)
}

func (s *stdioOriginalInput) Close() error {
	s.mu.Lock()
	s.retired = true
	s.mu.Unlock()
	err := s.file.close()
	s.joinOnce.Do(func() { go func() { s.writers.Wait(); close(s.joined) }() })
	return err
}

type stdioOriginalLifecycle struct {
	observeOriginal                                             func(*process.AtCreateJobProcess, context.Context, bool) (process.ContainedNativeSnapshot, error)
	original                                                    *process.AtCreateJobProcess
	job                                                         *process.Job
	files                                                       [6]*stdioOriginalFile // stdin read/write, stdout read/write, stderr read/write
	input                                                       *stdioOriginalInput
	rootDone, stdoutDone, stderrDone, watcherDone, lifetimeDone chan struct{}
	root                                                        process.OriginalRootWait
	stdoutError, stderrError                                    error
	nativeAttempted                                             bool
	identity                                                    process.ContainedRootIdentity
	primary                                                     error
	stopMu                                                      sync.Mutex
	stopStarted                                                 bool
	stopDone                                                    chan struct{}
	doneOnce                                                    sync.Once
	childExitedOnce                                             sync.Once
	terminal                                                    StdioHostSettlement
}

// Per-host preparation seam. Defaults use only the existing pipe/Job/creator
// owners; tests inject finite acquisition refusal or a reader barrier here.
type stdioOriginalLaunchOps struct {
	observeOriginal func(*process.AtCreateJobProcess, context.Context, bool) (process.ContainedNativeSnapshot, error)
	pipe            func() (*os.File, *os.File, error)
	job             func(process.JobOptions) (*process.Job, error)
	start           func(*process.Job, *exec.Cmd, *os.File, *os.File, *os.File) (*process.AtCreateJobProcess, error)
	beforeStdout    func()
}

func newStdioOriginalLifecycle() *stdioOriginalLifecycle {
	return &stdioOriginalLifecycle{
		rootDone: make(chan struct{}), stdoutDone: make(chan struct{}),
		stderrDone: make(chan struct{}), watcherDone: make(chan struct{}),
		lifetimeDone: make(chan struct{}), stopDone: make(chan struct{}),
	}
}

func originalJoin(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// originalMemberProcess selects this host's existing read capability under the
// same host/stop lock order as terminal admission. The process member gate owns
// any subsequent observation/stop race; no host lock spans a native query.
func (h *StdioHost) originalMemberProcess(ctx context.Context) (*process.AtCreateJobProcess, error) {
	if h == nil || ctx == nil {
		return nil, errors.New("STDIOHOST_ORIGINAL_MEMBER_SOURCE_UNAVAILABLE")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.original
	if !h.started || h.cfg.LaunchCapability != nil || p == nil || p.original == nil {
		return nil, errors.New("STDIOHOST_ORIGINAL_MEMBER_SOURCE_UNAVAILABLE")
	}
	p.stopMu.Lock()
	defer p.stopMu.Unlock()
	if p.stopStarted {
		return nil, errors.New("STDIOHOST_ORIGINAL_MEMBER_SOURCE_UNAVAILABLE")
	}
	return p.original, nil
}

// OriginalMemberSource exposes only this live host's opaque original read
// source. It does not transfer process-control or raw-handle authority.
func (h *StdioHost) OriginalMemberSource(ctx context.Context) (process.OriginalJobMemberSource, error) {
	owner, err := h.originalMemberProcess(ctx)
	if err != nil {
		return nil, err
	}
	return owner.OriginalMemberSource(ctx)
}

// ObserveOriginalMembers preserves a partial observation together with its
// error. The caller receives its source-bound CloseObserved before retirement.
func (h *StdioHost) ObserveOriginalMembers(ctx context.Context) (*process.OriginalJobMemberObservation, error) {
	owner, err := h.originalMemberProcess(ctx)
	if err != nil {
		return nil, err
	}
	return owner.ObserveOriginalMembers(ctx)
}

// NativeExit receives actual original wait diagnostics. Legacy callers retain
// ExitState; no fabricated os.ProcessState crosses either path.
func (h *StdioHost) NativeExit() (process.ContainedRootIdentity, process.OriginalRootWait, bool) {
	h.mu.Lock()
	p := h.original
	h.mu.Unlock()
	if p == nil {
		return process.ContainedRootIdentity{}, process.OriginalRootWait{}, false
	}
	select {
	case <-p.rootDone:
		return p.identity, p.root, true
	default:
	}
	return p.identity, process.OriginalRootWait{}, true
}

// StopObserved joins one host-owned attempt. An expired receiver never replaces
// the retained terminal outcome or starts another action with a fresh budget.
func (h *StdioHost) StopObserved(ctx context.Context) (StdioHostSettlement, error) {
	if ctx == nil {
		result := StdioHostSettlement{Pending: true}
		return result, h.originalSettlementError(result, errors.New("stdio stop context missing"))
	}
	h.mu.Lock()
	p := h.original
	h.mu.Unlock()
	if p == nil {
		return StdioHostSettlement{}, h.Stop()
	}
	if ctx.Err() == nil {
		h.beginOriginalStop(ctx, nil)
	}
	select {
	case <-p.stopDone:
		return p.terminal, h.originalSettlementError(p.terminal, errors.Join(p.terminal.RootWait.Error, p.terminal.CleanupError))
	default:
	}
	select {
	case <-p.stopDone:
		return p.terminal, h.originalSettlementError(p.terminal, errors.Join(p.terminal.RootWait.Error, p.terminal.CleanupError))
	case <-ctx.Done():
		p.stopMu.Lock()
		v := p.terminal
		p.stopMu.Unlock()
		v.OriginalIdentity = p.identity
		v.Pending = true
		return v, h.originalSettlementError(v, errors.Join(v.PrimaryError, v.CleanupError, ctx.Err()))
	}
}

func (h *StdioHost) beginOriginalStop(ctx context.Context, cause error) {
	h.mu.Lock()
	p := h.original
	if p == nil {
		h.mu.Unlock()
		return
	}
	p.stopMu.Lock()
	if p.stopStarted {
		p.stopMu.Unlock()
		h.mu.Unlock()
		return
	}
	p.stopStarted = true
	natural := false
	select {
	case <-p.rootDone:
		natural = p.root.Known
	default:
	}
	budget := stdioOriginalCleanupTimeout
	if natural {
		budget = pipeDrainTimeout
	}
	deadline := time.Now().Add(budget)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	p.terminal = StdioHostSettlement{OriginalIdentity: p.identity, Deadline: deadline, PrimaryError: errors.Join(p.primary, cause), Pending: true}
	p.stopMu.Unlock()
	h.stopped = true
	h.sessionMu.Lock()
	h.sessions = nil
	h.sessionMu.Unlock()
	if !natural {
		p.doneOnce.Do(func() { close(h.done) })
	}
	h.mu.Unlock()
	// The finalizer is an owned worker: its result and completion remain in p.
	// Its cleanup context is independent of a canceled service start context.
	go h.finishOriginalStop(p, deadline)
}

func (h *StdioHost) finishOriginalStop(p *stdioOriginalLifecycle, deadline time.Time) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	p.stopMu.Lock()
	result := p.terminal
	p.stopMu.Unlock()
	add := func(err error) { result.CleanupError = errors.Join(result.CleanupError, err) }
	if p.input != nil {
		add(p.input.Close())
	}
	if p.original != nil {
		natural := false
		select {
		case <-p.rootDone:
			natural = p.root.Known
		default:
		}
		native, err := p.observeOriginal(p.original, ctx, natural)
		add(err)
		if native.NoChildKnown {
			result.NativeSettlement, err = p.original.ReleaseOriginal()
			add(err)
		} else if err := originalJoin(ctx, p.rootDone); err != nil {
			add(fmt.Errorf("stdio original wait: %w", err))
		} else {
			result.RootWait = p.root
			add(p.root.Error)
			result.NativeSettlement, err = p.original.ReleaseOriginal()
			add(err)
		}
	} else {
		result.NoNativeAttempt = !p.nativeAttempted
		result.JobClosed = p.job == nil
		if p.job != nil {
			err := p.job.Close()
			add(err)
			result.JobClosed = err == nil
		}
	}
	if p.original != nil {
		result.JobClosed = result.NativeSettlement.Complete()
	}
	// Readers get natural EOF first. Closing them only after that bounded join
	// fails cannot disguise truncated/unreceived protocol output as clean EOF.
	result.ReadersJoined = true
	for _, reader := range []struct {
		done <-chan struct{}
		name string
	}{{p.stdoutDone, "stdout"}, {p.stderrDone, "stderr"}} {
		if err := originalJoin(ctx, reader.done); err != nil {
			result.ReadersJoined = false
			add(fmt.Errorf("stdio %s reader join: %w", reader.name, err))
		}
	}
	result.FilesClosed = true
	for _, f := range p.files {
		if err := f.close(); err != nil {
			result.FilesClosed = false
			add(err)
		}
	}
	if result.ReadersJoined {
		add(p.stdoutError)
		add(p.stderrError)
	}
	// Publishing exit after reader receipt preserves the final reply preference.
	// A bounded incomplete drain remains an error; it is never EOF evidence.
	p.childExitedOnce.Do(func() { close(h.childExited) })
	p.doneOnce.Do(func() { close(h.done) })
	result.WritersJoined = p.input == nil
	if p.input != nil {
		err := originalJoin(ctx, p.input.joined)
		result.WritersJoined = err == nil
		add(err)
	}
	for _, worker := range []struct {
		done   <-chan struct{}
		joined *bool
		name   string
	}{
		{p.watcherDone, &result.WatcherJoined, "watcher"}, {p.lifetimeDone, &result.LifetimeJoined, "lifetime"},
	} {
		err := originalJoin(ctx, worker.done)
		*worker.joined = err == nil
		if err != nil {
			add(fmt.Errorf("stdio %s join: %w", worker.name, err))
		}
	}
	result.LogClosed = h.logFile == nil
	if h.logFile != nil && result.ReadersJoined {
		err := h.logFile.Close()
		add(err)
		result.LogClosed = err == nil
	}
	result.Pending = !(result.JobClosed && result.FilesClosed && result.ReadersJoined && result.WritersJoined &&
		result.WatcherJoined && result.LifetimeJoined && result.LogClosed &&
		(result.NoNativeAttempt || result.NativeSettlement.Complete()))
	if result.Pending {
		add(errors.New("stdio original custody settlement incomplete"))
	}
	p.stopMu.Lock()
	p.terminal = result
	p.stopMu.Unlock()
	close(p.stopDone)
}

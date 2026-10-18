package process

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"time"
)

var ErrContainedOutputLimit = errors.New("contained output limit exceeded")

type ContainedRootIdentity struct {
	Known               bool
	PID                 int
	StartedAt           string
	NativeBirthFILETIME uint64
}
type ContainedNativeSnapshot struct {
	RootIdentity       ContainedRootIdentity
	NoChildKnown       bool
	RootWaitKnown      bool
	RootExitCode       int
	TerminalAction     string
	MembersExited      bool
	AccountingKnown    bool
	InitialTotal       uint32
	FinalActive        uint32
	CapturedMemberPIDs []uint32
	HandlesClosed      bool
	Error              error
}

// Only the same contained runner can construct this original-child receipt.
// A copied snapshot is evidence, not a constructor for native authority.
type ContainedProcessSettlement struct {
	owner    containedChild
	snapshot ContainedNativeSnapshot
}

func (s *ContainedProcessSettlement) Snapshot() ContainedNativeSnapshot {
	if s == nil {
		return ContainedNativeSnapshot{}
	}
	v := s.snapshot
	v.CapturedMemberPIDs = append([]uint32(nil), v.CapturedMemberPIDs...)
	return v
}
func (s *ContainedProcessSettlement) Complete() bool {
	if s == nil || s.owner == nil {
		return false
	}
	v := s.snapshot
	return v.Error == nil && v.HandlesClosed && ((v.RootIdentity.Known && v.RootWaitKnown && v.MembersExited && v.AccountingKnown && v.FinalActive == 0) || v.NoChildKnown)
}

type ContainedStreamSlot struct {
	Acquired       bool
	CloseAttempted bool
	Closed         bool
	CloseError     error
}
type ContainedIOStream struct {
	WorkerStarted  bool
	WorkerComplete bool
	EOF            bool
	Bytes          int64
	Limit          int64
	LimitExceeded  bool
	Error          error
}
type ContainedIOSettlement struct {
	Stdin, StdoutRead, StdoutWrite, StderrRead, StderrWrite ContainedStreamSlot
	Stdout, Stderr                                          ContainedIOStream
}

func (s ContainedIOSettlement) Complete() bool {
	for _, v := range []ContainedStreamSlot{s.Stdin, s.StdoutRead, s.StdoutWrite, s.StderrRead, s.StderrWrite} {
		if v.Acquired && (!v.CloseAttempted || !v.Closed || v.CloseError != nil) {
			return false
		}
	}
	complete := func(v ContainedIOStream) bool {
		return !v.WorkerStarted || (v.WorkerComplete && v.EOF && v.Error == nil && !v.LimitExceeded)
	}
	return complete(s.Stdout) && complete(s.Stderr)
}

type ContainedStreamReceiptV1 struct {
	Stage                      ContainedRunStage
	StartAttempted             bool
	RootIdentity               ContainedRootIdentity
	RootWaitKnown              bool
	RootExitCode               int
	RootExitError              error
	TerminalAction             string
	NativeSettlement           *ContainedProcessSettlement
	IOSettlement               ContainedIOSettlement
	PrimaryError, CleanupError error
}
type containedObservedChild interface {
	prepareContainedObservation()
	containedCreated() bool
	containedStartLive(context.Context) (bool, error)
	closeContainedJobBackstop() error
	containedRootIdentity() (ContainedRootIdentity, error)
	settleContained(context.Context, bool) (ContainedNativeSnapshot, error)
}

// RunContainedStreamObserved shares the existing acquisition/wait/IO owner.
// The caller owns and closes the borrowed output sinks after this return.
func RunContainedStreamObserved(ctx context.Context, cmd *exec.Cmd, options ContainedStreamOptions, consumeStdout func(io.Reader) error) (ContainedStreamReceiptV1, error) {
	var receipt ContainedStreamReceiptV1
	err := runContainedStreamCore(ctx, cmd, options, consumeStdout, defaultContainedStreamDependencies(), &receipt)
	return receipt, err
}

// One reader owns each counter; snapshots also remain safe on timeout when a
// blocked borrowed sink has not returned. A cap failure is never EOF proof.
type containedCountingReader struct {
	reader io.Reader
	mu     sync.Mutex
	state  ContainedIOStream
}

func (r *containedCountingReader) Read(dst []byte) (int, error) {
	r.mu.Lock()
	remaining := r.state.Limit - r.state.Bytes
	r.mu.Unlock()
	if remaining <= 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		r.mu.Lock()
		defer r.mu.Unlock()
		if n > 0 {
			r.state.LimitExceeded = true
			return 0, ErrContainedOutputLimit
		}
		if err == io.EOF {
			r.state.EOF = true
		}
		return n, err
	}
	if int64(len(dst)) > remaining {
		dst = dst[:remaining]
	}
	n, err := r.reader.Read(dst)
	r.mu.Lock()
	r.state.Bytes += int64(n)
	if err == io.EOF {
		r.state.EOF = true
	}
	r.mu.Unlock()
	return n, err
}
func (r *containedCountingReader) snapshot(complete bool, err error) ContainedIOStream {
	if r == nil {
		return ContainedIOStream{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.state
	v.WorkerComplete = complete
	v.Error = err
	return v
}
func containedCleanupContext(deadline time.Time) (context.Context, context.CancelFunc) {
	return context.WithDeadline(context.Background(), deadline)
}

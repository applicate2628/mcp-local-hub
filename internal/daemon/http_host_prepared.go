package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/process"
)

// The API injects the same package/intent admission for global and workspace
// consumers. HTTPHost never imports API or interprets its ready/reference files.
type HTTPHostPreparedInput struct {
	Command           string
	Args, Environment []string
	WorkingDir        string
}

// The assembly prearms partial-start cleanup using the existing host policy.
// This adds no timeout or independent shutdown owner.
func PreparedHTTPHostStartupCleanupDeadline(start context.Context) (time.Time, error) {
	if start == nil {
		return time.Time{}, errors.New("HTTPHOST_PREPARED_START_CONTEXT_MISSING")
	}
	end, finite := start.Deadline()
	if !finite {
		return time.Time{}, errors.New("HTTPHOST_PREPARED_START_DEADLINE_MISSING")
	}
	return end.Add(httpHostCleanupTimeout), nil
}

type HTTPHostPreparedAdmission interface {
	BeforeCreate(context.Context, HTTPHostPreparedInput) (HTTPHostPreparedTicket, error)
}
type HTTPHostPreparedTicket interface {
	Started(context.Context, HTTPHostStartReceipt) error
	Terminal(context.Context, HTTPHostTerminalReceipt) error
	ObserveOriginalListener(context.Context, *HTTPHost, *HTTPHostStartReceipt) (MCPOriginalHostEndpointEvidence, error)
}
type OriginalListenerNotListeningEvidence interface {
	ValidateOriginalNotListening(context.Context, *HTTPHost, *HTTPHostStartReceipt) error
}
type HTTPHostStartReceipt struct {
	OriginalStart    *process.OriginalStartObservation
	owner            *HTTPHost
	OriginalIdentity process.ContainedRootIdentity
	StartAttempted   bool
	Ready            bool
	OriginalLive     bool
	Error            error
	endpoint         MCPOriginalHostEndpointEvidence
}

func (r *HTTPHostStartReceipt) OriginalEndpoint(ctx context.Context) (MCPOriginalHostEndpointEvidence, error) {
	if r == nil || r.owner == nil || r.endpoint == nil || !r.Ready || r.Error != nil || ctx == nil || ctx.Err() != nil {
		return nil, errors.New("HTTPHOST_ORIGINAL_ENDPOINT_UNAVAILABLE")
	}
	return r.endpoint, nil
}

// ExpectedEndpoint is configured intent only; original member/TCP receiving
// remains mandatory before it can authorize any HTTP bytes.
func (r *HTTPHostStartReceipt) ExpectedEndpoint(ctx context.Context) (string, error) {
	if r == nil || r.owner == nil || !r.OriginalCreated() {
		return "", errors.New("HTTPHOST_ENDPOINT_INTENT_UNAVAILABLE")
	}
	if err := r.ValidateOriginalHost(ctx, r.owner); err != nil {
		return "", err
	}
	return r.owner.upstreamURL, nil
}

func (h *HTTPHost) OriginalMemberSource(ctx context.Context) (process.OriginalJobMemberSource, error) {
	h.mu.Lock()
	p := h.prepared
	h.mu.Unlock()
	if p == nil || p.original == nil {
		return nil, errors.New("HTTPHOST_ORIGINAL_MEMBER_SOURCE_UNAVAILABLE")
	}
	return p.original.OriginalMemberSource(ctx)
}
func (h *HTTPHost) ObserveOriginalMembers(ctx context.Context) (*process.OriginalJobMemberObservation, error) {
	h.mu.Lock()
	p := h.prepared
	h.mu.Unlock()
	if p == nil || p.original == nil {
		return nil, errors.New("HTTPHOST_ORIGINAL_MEMBER_SOURCE_UNAVAILABLE")
	}
	return p.original.ObserveOriginalMembers(ctx)
}

func (r HTTPHostStartReceipt) OriginalCreated() bool {
	return r.owner != nil && r.StartAttempted && r.OriginalStart.Complete()
}

func (r *HTTPHostStartReceipt) ValidateOriginalHost(ctx context.Context, h *HTTPHost) error {
	if r == nil || ctx == nil || h == nil || r.owner != h {
		return errors.New("HTTPHOST_PREPARED_START_SOURCE_MISMATCH")
	}
	return ctx.Err()
}

type HTTPHostTerminalReceipt struct {
	ioObservation                *HTTPHostIOObservation
	completedInBound             bool
	owner                        *HTTPHost
	NativeSettlement             *process.ContainedProcessSettlement
	RootWait                     process.OriginalRootWait
	IOSettlement                 process.ContainedIOSettlement
	LogCloseAttempted, LogClosed bool
	LogCloseError                error
	RequestsJoined               bool
	NoNativeAcquisition          bool
	Error                        error
	qualificationOrder           HTTPHostQualificationEventOrder
	EndpointRelease              *HTTPHostEndpointReleaseObservation
}
type HTTPHostQualificationEventOrder struct {
	ProtocolReceived, OwnedActionStarted, OwnedActionCompleted, ResourcesClosed uint64
	OwnedActionReceiptSHA256                                                    string
}

func (r *HTTPHostTerminalReceipt) QualificationEventOrder(h *HTTPHost) (HTTPHostQualificationEventOrder, error) {
	if r == nil || h == nil || r.owner != h {
		return HTTPHostQualificationEventOrder{}, errors.New("HTTPHOST_QUALIFICATION_EVENT_SOURCE_MISMATCH")
	}
	return r.qualificationOrder, nil
}
func (o *HTTPHostIOObservation) ReceiptSHA256() string {
	if o == nil || o.owner == nil || !o.io.Complete() || !o.logClosed || !o.requestsJoined || o.err != nil {
		return ""
	}
	// This is the actual retained IO producer, not a caller's summary flags.
	raw, err := json.Marshal(struct {
		Schema                    string
		IO                        process.ContainedIOSettlement
		LogClosed, RequestsJoined bool
	}{"http-host-original-io-v1", o.io, o.logClosed, o.requestsJoined})
	if err != nil {
		return ""
	}
	return qualificationSHA(raw)
}

// Only the host finalizer creates this observation, after its actual stream,
// tee/log and request-worker outcomes have settled. Snapshot flags cannot
// construct it or become an API consumer-release grant.
type HTTPHostIOObservation struct {
	owner                     *HTTPHost
	io                        process.ContainedIOSettlement
	logClosed, requestsJoined bool
	err                       error
}

func (o *HTTPHostIOObservation) ValidateOriginalHost(ctx context.Context, h *HTTPHost) error {
	if o == nil || ctx == nil || h == nil || o.owner != h || !o.io.Complete() || !o.logClosed || !o.requestsJoined || o.err != nil {
		return errors.New("HTTPHOST_PREPARED_IO_SOURCE_UNSETTLED")
	}
	return ctx.Err()
}
func (r *HTTPHostTerminalReceipt) OriginalIOObservation() *HTTPHostIOObservation {
	if r == nil {
		return nil
	}
	return r.ioObservation
}
func (r *HTTPHostTerminalReceipt) ValidateOriginalHost(ctx context.Context, h *HTTPHost) error {
	if r == nil || ctx == nil || h == nil || r.owner != h || !r.Complete() {
		return errors.New("HTTPHOST_PREPARED_TERMINAL_SOURCE_UNSETTLED")
	}
	return r.ioObservation.ValidateOriginalHost(ctx, h)
}

func (r *HTTPHostTerminalReceipt) ValidateNoNativeFactoryRelease(ctx context.Context, h *HTTPHost) error {
	if err := r.ValidateOriginalHost(ctx, h); err != nil {
		return err
	}
	h.mu.Lock()
	p := h.prepared
	h.mu.Unlock()
	if p == nil || p.nativeAcquisitionAttempted || p.original != nil || !r.NoNativeAcquisition {
		return errors.New("HTTPHOST_PREPARED_NATIVE_FACTORY_WAS_ENTERED")
	}
	select {
	case <-p.acquisitionDone:
	default:
		return errors.New("HTTPHOST_PREPARED_START_STILL_IN_FLIGHT")
	}
	return nil
}

func (r HTTPHostTerminalReceipt) Complete() bool {
	return r.owner != nil && r.completedInBound && r.EndpointRelease.complete() && (r.NativeSettlement.Complete() || r.NoNativeAcquisition) && r.IOSettlement.Complete() && r.LogClosed && r.LogCloseError == nil && r.RequestsJoined
}

type httpHostPreparedSlot struct {
	file      *os.File
	attempted bool
	closed    bool
	err       error
}

func (s *httpHostPreparedSlot) close() error {
	if s == nil || s.file == nil || s.attempted {
		return nil
	}
	s.attempted = true
	s.err = s.file.Close()
	s.closed = s.err == nil
	return s.err
}
func (s *httpHostPreparedSlot) snapshot() process.ContainedStreamSlot {
	if s == nil {
		return process.ContainedStreamSlot{}
	}
	return process.ContainedStreamSlot{Acquired: s.file != nil, CloseAttempted: s.attempted, Closed: s.closed, CloseError: s.err}
}

type httpHostPreparedStream struct {
	started bool
	done    chan struct{}
	bytes   atomic.Int64
	eof     atomic.Bool
	err     error
}

func (s *httpHostPreparedStream) snapshot() process.ContainedIOStream {
	v := process.ContainedIOStream{WorkerStarted: s.started, Bytes: s.bytes.Load()}
	if !s.started {
		return v
	}
	select {
	case <-s.done:
		v.WorkerComplete = true
		v.Error = s.err
		v.EOF = s.eof.Load()
	default:
	}
	return v
}

type httpHostPreparedState struct {
	endpointDone                                chan struct{}
	endpointDoneOnce                            sync.Once
	endpointAttempted                           bool
	endpoint                                    MCPOriginalHostEndpointEvidence
	endpointStart                               *HTTPHostStartReceipt
	endpointReleaseOnce                         sync.Once
	endpointRelease                             *HTTPHostEndpointReleaseObservation
	startDelivered                              bool
	acquisitionDone                             chan struct{}
	ticket                                      HTTPHostPreparedTicket
	original                                    *process.AtCreateJobProcess
	stdin, outRead, outWrite, errRead, errWrite *httpHostPreparedSlot
	out, stderr                                 httpHostPreparedStream
	rootWait                                    process.OriginalRootWait
	stopOnce                                    sync.Once
	stopDone                                    chan struct{}
	terminal                                    HTTPHostTerminalReceipt
	startupSucceeded                            bool
	nativeAcquisitionAttempted                  bool
	qualificationClock                          atomic.Uint64
	qualificationProtocol                       *MCPHTTPQualificationObservation
	qualificationBinding                        MCPHTTPQualificationBinding
	qualificationProtocolSequence               uint64
}

// Qualifying protocol evidence comes from the actual SAME relay receipt.
// Only this original host records its receiving order relative to its action.
func (h *HTTPHost) ReceiveQualificationProtocol(ctx context.Context, observation *MCPHTTPQualificationObservation, binding MCPHTTPQualificationBinding) error {
	if err := observation.Validate(ctx, binding); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.prepared
	if p == nil || p.original == nil || h.stopped || p.qualificationProtocol != nil {
		return errors.New("HTTPHOST_QUALIFICATION_PROTOCOL_OWNER_UNAVAILABLE")
	}
	p.qualificationProtocol = observation
	p.qualificationBinding = binding
	p.qualificationProtocolSequence = p.qualificationClock.Add(1)
	return nil
}

func preparedHostStream(reader *httpHostPreparedSlot, writer io.Writer, state *httpHostPreparedStream) {
	state.started = true
	state.done = make(chan struct{})
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := reader.file.Read(buf)
			if n > 0 {
				written, writeErr := writer.Write(buf[:n])
				state.bytes.Add(int64(written))
				if writeErr != nil {
					state.err = writeErr
					break
				}
				if written != n {
					state.err = io.ErrShortWrite
					break
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					state.eof.Store(true)
				} else {
					state.err = err
				}
				break
			}
		}
		close(state.done)
	}()
}

func (h *HTTPHost) StartObserved(ctx context.Context) (receipt HTTPHostStartReceipt, err error) {
	receipt.owner = h
	if ctx == nil || h.cfg.PreparedAdmission == nil || h.cfg.PreparedStartupCleanupDeadline.IsZero() {
		return receipt, errors.New("HTTPHOST_PREPARED_ADMISSION_REQUIRED")
	}
	h.mu.Lock()
	if h.started || h.prepared != nil {
		h.mu.Unlock()
		return receipt, errors.New("already started")
	}
	p := &httpHostPreparedState{acquisitionDone: make(chan struct{}), stopDone: make(chan struct{}), endpointDone: make(chan struct{})}
	h.prepared = p
	h.mu.Unlock()
	defer p.finishEndpointAcquisition()
	input := HTTPHostPreparedInput{Command: h.cfg.Command, Args: append([]string(nil), h.cfg.Args...), Environment: composeChildEnv(h.cfg.Env, h.cfg.UnsetEnv), WorkingDir: h.cfg.WorkingDir}
	if h.cfg.PreparedClosedEnvironment != nil {
		input.Environment = append([]string(nil), h.cfg.PreparedClosedEnvironment...)
	}
	p.ticket, err = h.cfg.PreparedAdmission.BeforeCreate(ctx, input)
	if err != nil || p.ticket == nil {
		close(p.acquisitionDone)
		p.finishEndpointAcquisition()
		if p.ticket != nil {
			cleanup, cancel := context.WithDeadline(context.Background(), h.cfg.PreparedStartupCleanupDeadline)
			terminal, cleanupErr := h.StopObserved(cleanup)
			cancel()
			err = errors.Join(err, cleanupErr, terminal.Error)
		}
		return receipt, errors.Join(err, errors.New("HTTPHOST_PREPARED_BEFORE_CREATE_REFUSED"))
	}
	defer func() {
		if err != nil {
			p.finishEndpointAcquisition()
			receipt.Error = err
			if p.original != nil {
				receipt.OriginalStart, _ = p.original.ObserveOriginalStart(ctx)
			}
			if !p.startDelivered {
				p.startDelivered = true
				err = errors.Join(err, p.ticket.Started(ctx, receipt))
			}
			cleanup, cancel := context.WithDeadline(context.Background(), h.cfg.PreparedStartupCleanupDeadline)
			defer cancel()
			terminal, cleanupErr := h.StopObserved(cleanup)
			err = errors.Join(err, cleanupErr, terminal.Error)
			receipt.Error = err
		}
	}()
	stdout, stderr, log := h.openLogWriters()
	h.logCloser = log
	if h.cfg.LogPath != "" && log == nil {
		close(p.acquisitionDone)
		return receipt, errors.New("HTTPHOST_PREPARED_LOG_UNAVAILABLE")
	}
	acquirePipe := func() (r, w *httpHostPreparedSlot, e error) {
		a, b, e := os.Pipe()
		if a != nil {
			r = &httpHostPreparedSlot{file: a}
		}
		if b != nil {
			w = &httpHostPreparedSlot{file: b}
		}
		return r, w, e
	}
	p.outRead, p.outWrite, err = acquirePipe()
	if err != nil {
		close(p.acquisitionDone)
		return receipt, err
	}
	p.errRead, p.errWrite, err = acquirePipe()
	if err != nil {
		close(p.acquisitionDone)
		return receipt, err
	}
	stdin, openErr := os.Open(os.DevNull)
	if stdin != nil {
		p.stdin = &httpHostPreparedSlot{file: stdin}
	}
	if openErr != nil {
		close(p.acquisitionDone)
		return receipt, openErr
	}
	p.nativeAcquisitionAttempted = true
	job, jobErr := process.NewKillOnCloseJob()
	if jobErr != nil {
		close(p.acquisitionDone)
		return receipt, jobErr
	}
	cmd := &exec.Cmd{Path: input.Command, Args: append([]string{input.Command}, input.Args...), Env: input.Environment, Dir: input.WorkingDir}
	receipt.StartAttempted = true
	p.original, err = process.StartWithJobFilesOriginal(job, cmd, p.stdin.file, p.outWrite.file, p.errWrite.file)
	if p.original == nil {
		close(p.acquisitionDone)
		return receipt, errors.Join(err, job.Close())
	}
	h.mu.Lock()
	h.cmd = cmd
	h.job = job
	h.started = true
	h.mu.Unlock()
	err = errors.Join(err, p.stdin.close(), p.outWrite.close(), p.errWrite.close())
	identity, identityErr := p.original.Identity()
	receipt.OriginalIdentity = identity
	err = errors.Join(err, identityErr)
	preparedHostStream(p.outRead, stdout, &p.out)
	preparedHostStream(p.errRead, stderr, &p.stderr)
	if p.original.Created() {
		go func() { p.rootWait = p.original.WaitOriginal(); close(h.childExited) }()
	}
	close(p.acquisitionDone)
	if err != nil {
		return receipt, err
	}
	receipt.OriginalStart, err = p.original.ObserveOriginalStart(ctx)
	if err != nil {
		return receipt, err
	}
	if err = h.waitForPreparedOriginalListener(ctx, p, &receipt); err != nil {
		return receipt, err
	}
	receipt.OriginalLive, err = p.original.OriginalLive(ctx)
	if err != nil || !receipt.OriginalLive {
		return receipt, errors.Join(err, errors.New("HTTPHOST_PREPARED_ORIGINAL_NOT_LIVE"))
	}
	receipt.Ready = true
	p.startDelivered = true
	if err = p.ticket.Started(ctx, receipt); err != nil {
		return receipt, err
	}
	p.startupSucceeded = true
	return receipt, nil
}

func (h *HTTPHost) StopObserved(ctx context.Context) (HTTPHostTerminalReceipt, error) {
	h.mu.Lock()
	p := h.prepared
	h.mu.Unlock()
	if p == nil || ctx == nil {
		return HTTPHostTerminalReceipt{}, errors.New("HTTPHOST_PREPARED_OWNER_UNAVAILABLE")
	}
	cleanup, cancel, contextErr := h.PreparedStopContext(ctx)
	if contextErr != nil {
		return HTTPHostTerminalReceipt{}, contextErr
	}
	defer cancel()
	select {
	case <-p.acquisitionDone:
	case <-cleanup.Done():
		return HTTPHostTerminalReceipt{}, cleanup.Err()
	}
	p.stopOnce.Do(func() {
		defer close(p.stopDone)
		r := HTTPHostTerminalReceipt{owner: h, LogClosed: h.logCloser == nil, NoNativeAcquisition: !p.nativeAcquisitionAttempted}
		r.EndpointRelease, r.Error = h.ReleaseOriginalEndpointObserved(cleanup)
		h.mu.Lock()
		if !h.stopped {
			h.stopped = true
			close(h.done)
		}
		h.mu.Unlock()
		var native process.ContainedNativeSnapshot
		if p.original != nil {
			natural := false
			select {
			case <-h.childExited:
				natural = p.rootWait.Known
			default:
			}
			var nativeErr error
			if !natural && p.qualificationProtocolSequence != 0 {
				r.qualificationOrder.OwnedActionStarted = p.qualificationClock.Add(1)
			}
			native, nativeErr = p.original.ObserveOriginal(cleanup, natural)
			if !natural && nativeErr == nil && native.TerminalAction == "request-succeeded" && r.qualificationOrder.OwnedActionStarted != 0 {
				r.qualificationOrder.OwnedActionCompleted = p.qualificationClock.Add(1)
				raw, encodeErr := json.Marshal(struct {
					Schema                         string
					Root                           process.ContainedRootIdentity
					Action                         string
					MembersExited, AccountingKnown bool
					FinalActive                    uint32
				}{"http-host-owned-action-v1", native.RootIdentity, native.TerminalAction, native.MembersExited, native.AccountingKnown, native.FinalActive})
				if encodeErr != nil {
					nativeErr = errors.Join(nativeErr, encodeErr)
				} else {
					r.qualificationOrder.OwnedActionReceiptSHA256 = qualificationSHA(raw)
				}
			}
			r.Error = errors.Join(r.Error, nativeErr)
			if native.NoChildKnown {
				r.NativeSettlement, nativeErr = p.original.ReleaseOriginal()
				r.Error = errors.Join(r.Error, nativeErr)
			} else {
				select {
				case <-h.childExited:
					r.RootWait = p.rootWait
					r.NativeSettlement, nativeErr = p.original.ReleaseOriginal()
					r.Error = errors.Join(r.Error, nativeErr)
				case <-cleanup.Done():
					r.Error = errors.Join(r.Error, cleanup.Err())
				}
			}
		}
		r.Error = errors.Join(r.Error, p.stdin.close(), p.outWrite.close(), p.errWrite.close())
		for _, s := range []*httpHostPreparedStream{&p.out, &p.stderr} {
			if s.started {
				select {
				case <-s.done:
				case <-cleanup.Done():
					r.Error = errors.Join(r.Error, cleanup.Err())
				}
			}
		}
		r.Error = errors.Join(r.Error, p.outRead.close(), p.errRead.close())
		joined := make(chan struct{})
		go func() { h.wg.Wait(); close(joined) }()
		select {
		case <-joined:
			r.RequestsJoined = true
		case <-cleanup.Done():
			r.Error = errors.Join(r.Error, cleanup.Err())
		}
		if h.logCloser != nil && (!p.out.started || p.out.snapshot().WorkerComplete) && (!p.stderr.started || p.stderr.snapshot().WorkerComplete) {
			r.LogCloseAttempted = true
			r.LogCloseError = h.logCloser.Close()
			r.LogClosed = r.LogCloseError == nil
			r.Error = errors.Join(r.Error, r.LogCloseError)
		}
		r.IOSettlement = process.ContainedIOSettlement{Stdin: p.stdin.snapshot(), StdoutRead: p.outRead.snapshot(), StdoutWrite: p.outWrite.snapshot(), StderrRead: p.errRead.snapshot(), StderrWrite: p.errWrite.snapshot(), Stdout: p.out.snapshot(), Stderr: p.stderr.snapshot()}
		r.completedInBound = cleanup.Err() == nil
		r.ioObservation = &HTTPHostIOObservation{owner: h, io: r.IOSettlement, logClosed: r.LogClosed, requestsJoined: r.RequestsJoined, err: r.LogCloseError}
		r.qualificationOrder.ProtocolReceived = p.qualificationProtocolSequence
		if r.Complete() && p.qualificationProtocolSequence != 0 {
			r.qualificationOrder.ResourcesClosed = p.qualificationClock.Add(1)
		}
		if p.ticket != nil {
			r.Error = errors.Join(r.Error, p.ticket.Terminal(cleanup, r))
		}
		p.terminal = r
	})
	select {
	case <-p.stopDone:
		return p.terminal, p.terminal.Error
	case <-cleanup.Done():
		return HTTPHostTerminalReceipt{}, cleanup.Err()
	}
}

// The wrapper and host share one original existing shutdown bound. Calling
// StopObserved with this context clips to it rather than renewing the policy.
func (h *HTTPHost) PreparedStopContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if h == nil || parent == nil {
		return nil, nil, errors.New("HTTPHOST_PREPARED_STOP_CONTEXT_MISSING")
	}
	end := time.Now().Add(httpHostCleanupTimeout)
	if d, finite := parent.Deadline(); finite && d.Before(end) {
		end = d
	}
	ctx, cancel := context.WithDeadline(context.WithoutCancel(parent), end)
	return ctx, cancel, nil
}

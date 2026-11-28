package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"time"
)

type IsolatedStdioRole uint8

const (
	IsolatedStdioDenied IsolatedStdioRole = iota
	IsolatedStdioHub
	IsolatedStdioDiscovery
)

// IsolatedStdioAuthorization is issued by the protected front, never decoded
// from a header. Session is the original front-session lifetime, not the
// lifetime of one HTTP request. Its identity and cancellation bind all private
// operations, including late initialization, to that exact front session.
type IsolatedStdioAuthorization struct {
	Role    IsolatedStdioRole
	Session context.Context
}

func (a IsolatedStdioAuthorization) valid() bool {
	return (a.Role == IsolatedStdioHub || a.Role == IsolatedStdioDiscovery) &&
		a.Session != nil && reflect.TypeOf(a.Session).Comparable() &&
		a.Session.Done() != nil && a.Session.Err() == nil
}

// IsolatedStdioLimits is one qualified listener generation's allocation.
// Its issuer takes account of the launcher tree as well as the provider.
type IsolatedStdioLimits struct {
	TreeCapacity, TreeCost    int
	RetryAfterSeconds         int
	MaxRequestIDs, MaxIDBytes int
	Startup, Operation, Idle  time.Duration
	Teardown, EOFGrace        time.Duration
}

func (l IsolatedStdioLimits) valid() bool {
	return l.TreeCost > 0 && l.TreeCapacity >= l.TreeCost &&
		l.RetryAfterSeconds > 0 && l.MaxRequestIDs > 0 &&
		l.MaxIDBytes > 0 && l.MaxIDBytes <= int(maxMCPPostBodyBytes) &&
		l.Startup > 0 && l.Operation > 0 && l.Idle > 0 &&
		l.EOFGrace > 0 && l.Teardown > l.EOFGrace
}

// IsolatedStdioSessions owns the binding and capacity of one protected
// generation. The protected listener supplies current, connection-bound role
// verification and revokes this instance before exposing a successor. The
// verifier reads the listener's current authority without blocking or reentry;
// it is consulted under the same gate as reservation and write admission.
type IsolatedStdioSessions struct {
	launch    *StdioLaunchContext
	cfg       HostConfig
	limits    IsolatedStdioLimits
	authorize func(*http.Request) IsolatedStdioAuthorization
	observe   func(IsolatedStdioSettlementDiagnostic)
	protocol  *StdioHost

	mu           sync.Mutex
	closed       bool
	reserved     int
	nextSequence uint64
	sessions     map[string]*isolatedStdioSession
	changed      chan struct{}
	stopLifetime func() bool
}

type isolatedStdioSession struct {
	owner        *IsolatedStdioSessions
	host         *StdioHost
	sid          string
	grant        IsolatedStdioAuthorization
	stopSession  func() bool
	version      string
	sequence     uint64
	initializeID isolatedStdioID

	expires       time.Time
	stopDeadline  time.Time
	graceDeadline time.Time
	startCtx      context.Context
	cancelStart   context.CancelFunc
	launchDone    chan struct{}
	initDone      chan struct{}
	leaseChanged  chan struct{}
	revoked       chan struct{}
	written       chan struct{}
	receiptDone   chan struct{}
	receipt       StdioHostSettlement
	receiptErr    error
	requests      map[isolatedStdioID]*isolatedStdioOperation
}

func (s *StdioLaunchContext) NewIsolatedStdioSessions(
	cfg HostConfig,
	limits IsolatedStdioLimits,
	authorize func(*http.Request) IsolatedStdioAuthorization,
) (*IsolatedStdioSessions, error) {
	return s.NewObservedIsolatedStdioSessions(cfg, limits, authorize, nil)
}

// IsolatedStdioSettlementDiagnostic is a redacted terminal observation, not an authority receipt.
type IsolatedStdioSettlementDiagnostic struct {
	Sequence              uint64
	Complete              bool
	OriginalPID           int
	OriginalBirthFILETIME uint64
}

// NewObservedIsolatedStdioSessions supplies an optional observation after receipt
// publication and reservation accounting. The observer does not decide cleanup.
func (s *StdioLaunchContext) NewObservedIsolatedStdioSessions(
	cfg HostConfig,
	limits IsolatedStdioLimits,
	authorize func(*http.Request) IsolatedStdioAuthorization,
	observe func(IsolatedStdioSettlementDiagnostic),
) (*IsolatedStdioSessions, error) {
	// Only Windows currently supplies the existing at-create native owner.
	if s.valid() != nil || s.ctx.Err() != nil || s.ctx.Done() == nil ||
		runtime.GOOS != "windows" || cfg.LaunchCapability != nil ||
		!limits.valid() || authorize == nil {
		return nil, ErrIsolatedStdioUnavailable
	}
	if cfg.WorkingDir == "" {
		cfg.WorkingDir = s.cwd
	}
	host, err := s.NewStdioHost(cfg)
	if err != nil {
		return nil, ErrIsolatedStdioUnavailable
	}
	p := &IsolatedStdioSessions{
		launch: s, cfg: host.cfg, limits: limits, authorize: authorize, observe: observe, protocol: host,
		sessions: make(map[string]*isolatedStdioSession), changed: make(chan struct{}),
	}
	p.stopLifetime = context.AfterFunc(s.ctx, p.revoke)
	return p, nil
}

// HTTPHandler belongs exclusively behind the current protected listener.
// Every operation checks the grant before reading or allocating anything.
func (p *IsolatedStdioSessions) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", p.serveMCP)
	return mux
}

func (p *IsolatedStdioSessions) allowed(request *http.Request, grant IsolatedStdioAuthorization) bool {
	current := p.authorize(request)
	return !p.closed && p.launch.ctx.Err() == nil && current.valid() && current == grant
}

func (p *IsolatedStdioSessions) serveMCP(w http.ResponseWriter, r *http.Request) {
	if rejectUnsafeLoopbackRequest(w, r) {
		return
	}
	p.mu.Lock()
	grant := p.authorize(r)
	allowed := !p.closed && p.launch.ctx.Err() == nil && grant.valid()
	p.mu.Unlock()
	if !allowed {
		http.Error(w, "protected provider unavailable", http.StatusForbidden)
		return
	}
	version, ok := p.protocol.validateRequestProtocolVersion(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodPost:
		post, ok := parseStdioPOST(w, r, true)
		if !ok {
			return
		}
		if post.method == "initialize" {
			p.initialize(w, r, grant, post)
			return
		}
		s := p.current(w, r, grant, version, false)
		if s == nil {
			return
		}
		if grant.Role == IsolatedStdioDiscovery && post.method != "notifications/initialized" && post.method != "tools/list" {
			http.Error(w, "method forbidden for discovery session", http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), p.limits.Operation)
		defer cancel()
		s.host.handleParsedPOST(w, r.WithContext(ctx), post)
	case http.MethodGet:
		s := p.current(w, r, grant, version, false)
		if s != nil {
			if grant.Role != IsolatedStdioHub {
				http.Error(w, "stream forbidden for discovery session", http.StatusForbidden)
				return
			}
			s.host.serveStdioSSE(w, r)
		}
	case http.MethodDelete:
		if p.current(w, r, grant, version, true) != nil {
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (p *IsolatedStdioSessions) current(w http.ResponseWriter, r *http.Request, grant IsolatedStdioAuthorization, version string, retire bool) *isolatedStdioSession {
	sid := r.Header.Get("Mcp-Session-Id")
	if sid == "" {
		http.Error(w, "missing session id", http.StatusBadRequest)
		return nil
	}
	p.mu.Lock()
	s := p.sessions[sid]
	if s == nil || s.version == "" || s.grant != grant || !s.activeLocked() || !p.allowed(r, grant) {
		p.mu.Unlock()
		http.Error(w, "unknown session id", http.StatusNotFound)
		return nil
	}
	if version != "" && version != s.version {
		p.mu.Unlock()
		http.Error(w, "protocol version does not match session", http.StatusBadRequest)
		return nil
	}
	if retire {
		s.retireLocked(time.Now())
	} else {
		s.refreshLocked()
	}
	p.mu.Unlock()
	return s
}

func (p *IsolatedStdioSessions) initialize(w http.ResponseWriter, r *http.Request, grant IsolatedStdioAuthorization, post stdioPOST) {
	id, ok := stdioPrivateID(post.msg["id"], p.limits.MaxIDBytes)
	if !ok ||
		r.Header.Get("Mcp-Session-Id") != "" || string(post.msg["jsonrpc"]) != `"2.0"` {
		http.Error(w, "invalid initialize request", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	if !p.allowed(r, grant) || r.Context().Err() != nil {
		p.mu.Unlock()
		http.Error(w, "protected provider unavailable", http.StatusForbidden)
		return
	}
	if p.reserved > p.limits.TreeCapacity-p.limits.TreeCost {
		p.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(p.limits.RetryAfterSeconds))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, `{"code":"MCP_STDIO_ISOLATED_CAPACITY"}`)
		return
	}
	sid, err := randomSessionID()
	if err != nil || p.sessions[sid] != nil {
		p.mu.Unlock()
		http.Error(w, "protected provider unavailable", http.StatusServiceUnavailable)
		return
	}
	host, err := p.launch.NewStdioHost(p.cfg)
	if err != nil {
		p.mu.Unlock()
		http.Error(w, "protected provider unavailable", http.StatusServiceUnavailable)
		return
	}
	startCtx, cancel := context.WithTimeout(r.Context(), p.limits.Startup)
	p.nextSequence++
	s := &isolatedStdioSession{
		owner: p, host: host, sid: sid, grant: grant, sequence: p.nextSequence, startCtx: startCtx, cancelStart: cancel,
		initializeID: id,
		launchDone:   make(chan struct{}), initDone: make(chan struct{}), leaseChanged: make(chan struct{}, 1),
		revoked: make(chan struct{}), receiptDone: make(chan struct{}), written: make(chan struct{}, 1),
		requests: make(map[isolatedStdioID]*isolatedStdioOperation),
	}
	host.private, host.stderrMode, host.testStdout = s, stdioStderrPrivate, nil
	p.sessions[sid] = s
	p.reserved += p.limits.TreeCost
	s.stopSession = context.AfterFunc(grant.Session, s.retire)
	p.mu.Unlock()

	go s.run()
	defer func() {
		p.mu.Lock()
		if s.version == "" {
			s.retireLocked(time.Now())
		}
		close(s.initDone)
		p.mu.Unlock()
		cancel()
	}()
	err = host.Start(startCtx)
	close(s.launchDone)
	if err != nil {
		http.Error(w, "protected provider start failed", http.StatusBadGateway)
		return
	}
	host.handleParsedPOST(w, r.WithContext(startCtx), post)
}

func (s *isolatedStdioSession) publish(w http.ResponseWriter, r *http.Request, protocol string) bool {
	p := s.owner
	p.mu.Lock()
	defer p.mu.Unlock()
	if !s.activeLocked() || s.version != "" || s.startCtx.Err() != nil || !p.allowed(r, s.grant) {
		return false
	}
	s.version = protocol
	s.refreshLocked()
	w.Header().Set("Mcp-Session-Id", s.sid)
	return true
}

func (s *isolatedStdioSession) activeLocked() bool {
	if !s.stopDeadline.IsZero() {
		return false
	}
	if s.grant.Session.Err() != nil {
		s.retireLocked(time.Now())
		return false
	}
	select {
	case <-s.host.procExited:
		s.retireLocked(time.Now())
		return false
	default:
	}
	if !s.expires.IsZero() && !time.Now().Before(s.expires) {
		s.retireLocked(time.Now())
		return false
	}
	return true
}

func (s *isolatedStdioSession) refreshLocked() {
	s.expires = time.Now().Add(s.owner.limits.Idle)
	select {
	case s.leaseChanged <- struct{}{}:
	default:
	}
}

func (s *isolatedStdioSession) retireLocked(now time.Time) {
	if !s.stopDeadline.IsZero() {
		return
	}
	s.stopDeadline = now.Add(s.owner.limits.Teardown)
	s.graceDeadline = now.Add(s.owner.limits.EOFGrace)
	close(s.revoked)
	s.cancelStart()
}

func (s *isolatedStdioSession) retire() {
	s.owner.mu.Lock()
	s.retireLocked(time.Now())
	s.owner.mu.Unlock()
}

func (s *isolatedStdioSession) run() {
	p := s.owner
	select {
	case <-s.startCtx.Done():
	case <-s.host.procExited:
	case <-s.revoked:
	case <-s.initDone:
	}
	p.mu.Lock()
	if s.version == "" {
		s.retireLocked(time.Now())
	}
	for s.activeLocked() {
		timer := time.NewTimer(time.Until(s.expires))
		p.mu.Unlock()
		select {
		case <-timer.C:
		case <-s.leaseChanged:
		case <-s.revoked:
		case <-s.host.procExited:
		}
		timer.Stop()
		p.mu.Lock()
	}
	p.mu.Unlock()

	s.stopSession()
	// A canceled startup still owns any native result acquired on its
	// original launch. The reserved slot remains held until that call returns.
	<-s.launchDone
	s.host.mu.Lock()
	original := s.host.original
	s.host.mu.Unlock()
	if original != nil {
		s.host.beginOriginalStop(context.Background(), nil)
		s.receipt, s.receiptErr = s.host.StopObserved(context.Background())
	} else {
		s.receipt = StdioHostSettlement{
			NoNativeAttempt: true, JobClosed: true, FilesClosed: true, ReadersJoined: true,
			WritersJoined: true, WatcherJoined: true, LifetimeJoined: true, LogClosed: true,
		}
	}
	terminalAction := "unreceived"
	if s.receipt.NativeSettlement != nil {
		terminalAction = s.receipt.NativeSettlement.Snapshot().TerminalAction
	}
	fmt.Fprintf(daemonDiagWriter(),
		"stdio mode=private stage=settlement session=%d no_start=%t pending=%t native_action=%s exit_known=%t exit_code=%d stderr_bytes=%d stderr_eof=%t stderr_read_failed=%t stdout_read_failed=%t stdout_protocol_invalid=%t\n",
		s.sequence, s.receipt.NoNativeAttempt, s.receipt.Pending, terminalAction, s.receipt.RootWait.Known, s.receipt.RootWait.ExitCode,
		s.receipt.PrivateStderrBytes, s.receipt.PrivateStderrEOF, s.receipt.PrivateStderrReadFailed, s.receipt.StdoutReadFailed, s.receipt.StdoutProtocolInvalid)
	p.mu.Lock()
	close(s.receiptDone)
	if s.receipt.Complete() {
		p.reserved -= p.limits.TreeCost
		delete(p.sessions, s.sid)
	}
	close(p.changed)
	p.changed = make(chan struct{})
	p.mu.Unlock()
	if p.observe != nil {
		p.observe(isolatedStdioSettlementDiagnostic(s.sequence, s.receipt))
	}
}

func isolatedStdioSettlementDiagnostic(sequence uint64, receipt StdioHostSettlement) IsolatedStdioSettlementDiagnostic {
	d := IsolatedStdioSettlementDiagnostic{Sequence: sequence, Complete: receipt.Complete()}
	original := receipt.OriginalIdentity
	if !receipt.NoNativeAttempt && original.Known && original.PID > 0 && original.NativeBirthFILETIME > 0 {
		d.OriginalPID = original.PID
		d.OriginalBirthFILETIME = original.NativeBirthFILETIME
	}
	return d
}

// Retirement returns the existing receipt holder for an exact SID. The
// discovery owner captures it before DELETE and observes it after revocation.
func (p *IsolatedStdioSessions) Retirement(sid string) *IsolatedStdioRetirement {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.sessions[sid]; s != nil {
		return &IsolatedStdioRetirement{s: s}
	}
	return nil
}

// RetirementForSession borrows the receipt for the original front lifetime.
// It creates no process, no handle and no retirement authority of its own.
func (p *IsolatedStdioSessions) RetirementForSession(session context.Context) *IsolatedStdioRetirement {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sessions {
		if s.grant.Session == session {
			return &IsolatedStdioRetirement{s: s}
		}
	}
	return nil
}

// Retirements captures existing original receipt holders without exposing
// private session identifiers or acquiring another process handle. Capturing
// does not revoke sessions; a holder remains usable after its row is retired.
func (p *IsolatedStdioSessions) Retirements() []*IsolatedStdioRetirement {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := make([]*IsolatedStdioRetirement, 0, len(p.sessions))
	for _, session := range p.sessions {
		result = append(result, &IsolatedStdioRetirement{s: session})
	}
	return result
}

type IsolatedStdioRetirement struct{ s *isolatedStdioSession }

// Session is the original front lifetime, not a process identifier or a new
// capability. The composing listener uses it to match its own authenticated
// scope when receiving revocation, including after a front SID was deleted.
func (r *IsolatedStdioRetirement) Session() context.Context {
	if r == nil || r.s == nil {
		return nil
	}
	return r.s.grant.Session
}

func (r *IsolatedStdioRetirement) Await(ctx context.Context) (StdioHostSettlement, error) {
	if r == nil || r.s == nil || ctx == nil {
		return StdioHostSettlement{Pending: true}, ErrIsolatedStdioUnavailable
	}
	if err := originalJoin(ctx, r.s.receiptDone); err != nil {
		return StdioHostSettlement{Pending: true}, err
	}
	return r.s.receipt, r.s.receiptErr
}

func (p *IsolatedStdioSessions) revoke() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	now := time.Now()
	for _, s := range p.sessions {
		s.retireLocked(now)
	}
}

// Close revokes this generation and observes its original children without
// renewing their individual teardown deadlines. An incomplete result retains
// the original custody here, so the listener owner keeps this instance.
func (p *IsolatedStdioSessions) Close(ctx context.Context) error {
	p.revoke()
	p.stopLifetime()
	if ctx == nil {
		return ErrIsolatedStdioUnavailable
	}
	for {
		p.mu.Lock()
		remaining, changed := len(p.sessions), p.changed
		pendingTerminal := 0
		for _, s := range p.sessions {
			select {
			case <-s.receiptDone:
				pendingTerminal++
			default:
			}
		}
		p.mu.Unlock()
		if remaining == 0 {
			return nil
		}
		if pendingTerminal == remaining {
			return ErrIsolatedStdioUnavailable
		}
		if err := originalJoin(ctx, changed); err != nil {
			return errors.Join(ErrIsolatedStdioUnavailable, err)
		}
	}
}

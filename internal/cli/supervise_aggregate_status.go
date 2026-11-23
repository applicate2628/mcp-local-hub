package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

const routeAggregateLaunchEnv = "MCPHUB_ROUTE_AGGREGATE_SUPERVISOR"

type routeAggregateSupervisor struct {
	ctx        context.Context
	launch     api.RouteAggregateLaunch
	launchEnv  string
	binding    *routeAggregateBinding // daemonCreatorRecords.mu
	changed    chan struct{}          // daemonCreatorRecords.mu
	entryInput windowsCanonicalEntryCommandInput

	beforeDetach    func(*routeAggregateBinding) // fixture barrier; nil in production
	beforeApplyWait func()                       // fixture barrier; nil in production
}

func (s *routeAggregateSupervisor) notifyChangedLocked() {
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

func newRouteAggregateSupervisor(ctx context.Context, stateDir string, owner api.SupervisorLockOwner) (*routeAggregateSupervisor, error) {
	identity, err := api.ObserveSupervisorProcessIdentityV1(os.Getpid())
	if err != nil {
		return nil, err
	}
	if err := api.ValidateSupervisorStatusServerIdentityV1(owner, identity, identity.UserSID, identity.SessionID, identity.ImagePath); err != nil {
		return nil, err
	}
	launch := api.RouteAggregateLaunch{StateDir: stateDir, Owner: owner, Peer: identity}
	payload, err := json.Marshal(launch)
	if err != nil {
		return nil, err
	}
	return &routeAggregateSupervisor{
		ctx: ctx, launch: launch, launchEnv: routeAggregateLaunchEnv + "=" + string(payload),
		changed: make(chan struct{}),
	}, nil
}

// Bind launch data after merging ordinary daemon environment. Only the
// exact built-in route descriptor for this executable receives it.
func (r *daemonCreatorRecords) prepareRouteAggregateLaunch(operation *daemonCreatorOperation, d api.SupervisorDaemon, cmd *exec.Cmd) {
	env := cmd.Env[:0]
	for _, entry := range cmd.Env {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.EqualFold(key, routeAggregateLaunchEnv) {
			env = append(env, entry)
		}
	}
	cmd.Env = env
	if r == nil || r.routeAggregate == nil || operation == nil ||
		!reflect.DeepEqual(d, api.BuildBuiltinRouteDaemon(d.Command, d.Port)) ||
		!samePath(d.Command, r.routeAggregate.launch.Peer.ImagePath) {
		return
	}
	r.mu.Lock()
	operation.routeAggregateStarted = make(chan struct{})
	operation.routeDescriptorSHA256, _ = mcpFrontRouteDescriptorDigest(d)
	r.mu.Unlock()
	cmd.Env = append(cmd.Env, r.routeAggregate.launchEnv)
}

func (r *daemonCreatorRecords) routeAggregateOperation() *daemonCreatorOperation {
	for i := len(r.operations) - 1; i >= 0; i-- {
		if operation := r.operations[i]; operation.task == api.BuiltinRouteTaskName {
			return operation
		}
	}
	return nil
}

func (r *daemonCreatorRecords) observeRouteAggregatePeer(ctx context.Context, conn net.Conn, tracker *DaemonRuntimeTracker) (*daemonCreatorOperation, int, error) {
	return r.observeRouteAggregatePeerWithReceipt(ctx, conn, tracker, nil, "")
}

func (r *daemonCreatorRecords) observeRouteAggregatePeerWithReceipt(ctx context.Context, conn net.Conn, tracker *DaemonRuntimeTracker,
	phases *routeAdmissionNativeObservations, phase string) (*daemonCreatorOperation, int, error) {
	refused := errors.New("current original route peer required")
	if r == nil || r.routeAggregate == nil || tracker == nil {
		return nil, 0, refused
	}
	var peer api.SupervisorProcessIdentityV1
	var err error
	if phases != nil {
		peer, _, err = observeRouteAdmissionPeer(conn, phases, phase+"-pipe")
	} else {
		peer, err = observeAggregateIPCClient(conn)
	}
	if err != nil {
		return nil, 0, err
	}
	r.mu.Lock()
	operation := r.routeAggregateOperation()
	if operation == nil || operation.routeAggregateStarted == nil || operation.owner == nil {
		r.mu.Unlock()
		return nil, 0, refused
	}
	creator, started := operation.owner, operation.routeAggregateStarted
	r.mu.Unlock()

	var identity *process.CreatorIdentity
	if phases != nil {
		var observed *process.CreatorIdentityObservation
		identity, observed, err = creator.ObserveCreatorIdentityObserved(ctx)
		phases.add(phase+"-creator", observed, err)
	} else {
		identity, err = creator.ObserveCreatorIdentity(ctx)
	}
	if err != nil {
		return nil, 0, err
	}
	observed := identity.Snapshot()
	expected := r.routeAggregate.launch.Peer
	if !observed.RootAlive || !observed.CurrentJobMember || !observed.OriginalJobBound ||
		!samePath(observed.ExecutablePath, expected.ImagePath) ||
		!strings.EqualFold(observed.OwnerSID, expected.UserSID) {
		return nil, 0, refused
	}
	original := api.SupervisorLockOwner{PID: observed.OriginalPID, StartedAt: observed.OriginalStartedAt}
	if err := api.ValidateSupervisorStatusServerIdentityV1(original, peer, observed.OwnerSID, expected.SessionID, observed.ExecutablePath); err != nil {
		return nil, 0, errors.Join(refused, err)
	}

	// The child can establish its pipe while the spawn owner is still
	// publishing the tracker generation. Wait only on this verified original.
	select {
	case <-started:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	row, current := tracker.Get(api.BuiltinRouteTaskName)
	birth, err := time.Parse(time.RFC3339Nano, observed.OriginalStartedAt)
	if err != nil || !current || r.routeAggregateOperation() != operation || operation.starting ||
		operation.startupErr != nil || operation.terminal != nil ||
		operation.generation <= 0 || row.PIDGeneration != operation.generation ||
		row.CurrentPID != observed.OriginalPID || !row.StartedAt.Equal(birth) {
		return nil, 0, refused
	}
	return operation, operation.generation, ctx.Err()
}

// aggregateIPCConn serializes native handle inspection with the existing
// ipcSession close. go-winio's raw Fd does not synchronize with Close.
type aggregateIPCConn struct {
	net.Conn
	nativeMu sync.Mutex
	closing  *aggregateIPCClose
}

type aggregateIPCClose struct {
	done chan struct{}
	err  error // published by close(done)
}

func (c *aggregateIPCConn) Close() error {
	actual := c.startClose()
	<-actual.done
	return actual.err
}

// Publish the original close owner before invoking it. A later caller can
// receive the same actual attempt without performing or waiting for another
// physical close. The raw Fd is unavailable to new observers after this cut.
func (c *aggregateIPCConn) startClose() *aggregateIPCClose {
	c.nativeMu.Lock()
	if c.closing != nil {
		actual := c.closing
		c.nativeMu.Unlock()
		return actual
	}
	actual := &aggregateIPCClose{done: make(chan struct{})}
	c.closing = actual
	c.nativeMu.Unlock()
	actual.err = c.Conn.Close()
	close(actual.done)
	return actual
}

func closeAcceptedIPCConn(conn net.Conn) {
	if owned, ok := conn.(*aggregateIPCConn); ok {
		owned.startClose()
		return
	}
	_ = conn.Close()
}

func observeAggregateIPCClient(conn net.Conn) (api.SupervisorProcessIdentityV1, error) {
	if owned, ok := conn.(*aggregateIPCConn); ok {
		owned.nativeMu.Lock()
		defer owned.nativeMu.Unlock()
		if owned.closing != nil {
			return api.SupervisorProcessIdentityV1{}, net.ErrClosed
		}
		conn = owned.Conn
	}
	return api.ObserveSupervisorIPCClientIdentityV1(conn)
}

func observeRouteAdmissionPeer(conn net.Conn, phases *routeAdmissionNativeObservations, stage string) (api.SupervisorProcessIdentityV1, routeAdmissionNativeEvidence, error) {
	if owned, ok := conn.(*aggregateIPCConn); ok {
		owned.nativeMu.Lock()
		defer owned.nativeMu.Unlock()
		if owned.closing != nil {
			phases.addNotEntered(stage, net.ErrClosed)
			return api.SupervisorProcessIdentityV1{}, nil, net.ErrClosed
		}
		conn = owned.Conn
	}
	identity, observation, err := observeRouteAdmissionNativePipePeer(conn)
	phases.add(stage, observation, err)
	return identity, observation, err
}

type routeAggregateBinding struct {
	conn       net.Conn
	operation  *daemonCreatorOperation
	generation int
	done       chan struct{}
	closeOnce  sync.Once
	ready      bool                 // daemonCreatorRecords.mu
	pending    *routeAggregateQuery // daemonCreatorRecords.mu
	err        error                // published by closing done
}

type routeAggregateQuery struct {
	challenge api.RouteAggregateStatusChallenge
	reply     chan api.RouteAggregateStatusProof
	finished  chan struct{}
	deadline  time.Time                // set when writing begins, under daemonCreatorRecords.mu
	received  bool                     // daemonCreatorRecords.mu
	status    api.RouteAggregateStatus // published by closing finished
	err       error                    // published by closing finished
	writeErr  error                    // original write error, even when the proof succeeds
}

var (
	errAggregateUnavailable = errors.New("no attached current route")
	errAggregateBusy        = errors.New("a live query is already in progress")
	errAggregatePeerChanged = errors.New("original route generation changed")
	errAggregateMalformed   = errors.New("route reply did not match the live query")
)

func (b *routeAggregateBinding) close() {
	b.closeOnce.Do(func() { _ = b.conn.Close() })
}

// Revoke before closing: the route may consume its one reattach immediately
// after peer loss. Only the matching handle/generation may leave the slot.
func (r *daemonCreatorRecords) revokeRouteAggregateBinding(b *routeAggregateBinding) {
	r.mu.Lock()
	if r.routeAggregate.binding == b {
		r.routeAggregate.binding = nil
		r.routeAggregate.notifyChangedLocked()
	}
	r.mu.Unlock()
	b.close()
}

func (r *daemonCreatorRecords) retireRouteAggregateBinding(operation *daemonCreatorOperation) {
	if r.routeAggregate == nil || operation.task != api.BuiltinRouteTaskName {
		return
	}
	r.mu.Lock()
	b := r.routeAggregate.binding
	if b != nil && b.operation == operation {
		r.routeAggregate.binding = nil
	} else {
		b = nil
	}
	r.routeAggregate.notifyChangedLocked()
	r.mu.Unlock()
	if b != nil {
		b.close()
	}
}

func writeAggregateFailure(conn net.Conn, id int64, code, message string) error {
	return writeIPCFrame(conn, api.IPCResponse{
		ID: id, Final: true,
		Error: &api.IPCErr{Code: code, Message: message, Retryable: code == "OPERATOR_AGGREGATE_BUSY"},
	})
}

func serveRouteAggregateStatus(conn net.Conn, deps ipcDispatchDeps) error {
	creators := deps.daemonCreators
	ctx, cancel := context.WithTimeout(aggregateIPCContext(creators), api.SupervisorAggregateStatusTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	original, generation, err := creators.observeRouteAggregatePeer(ctx, conn, deps.runtimeTracker)
	if err != nil {
		return writeAggregateFailure(conn, 0, "ROUTE_AGGREGATE_PEER_DENIED", "current original route peer required")
	}
	// No application body is read until the kernel peer and creator agree.
	req, err := api.ReadRouteAggregateStatusOpen(conn)
	if err != nil {
		return writeAggregateFailure(conn, 0, "ROUTE_AGGREGATE_INVALID", "invalid bounded route attach")
	}
	b := &routeAggregateBinding{conn: conn, operation: original, generation: generation, done: make(chan struct{})}
	creators.mu.Lock()
	s := creators.routeAggregate
	prior := s.binding
	if original != creators.routeAggregateOperation() || original.terminal != nil {
		creators.mu.Unlock()
		return writeAggregateFailure(conn, req.ID, "ROUTE_AGGREGATE_PEER_DENIED", "original route generation changed")
	}
	if prior != nil && prior.operation == original {
		creators.mu.Unlock()
		return writeAggregateFailure(conn, req.ID, "ROUTE_AGGREGATE_BUSY", "current route already attached")
	}
	s.binding = b
	creators.mu.Unlock()
	if prior != nil {
		prior.close()
	}
	defer func() {
		if s.beforeDetach != nil {
			s.beforeDetach(b)
		}
		creators.revokeRouteAggregateBinding(b)
		close(b.done)
		creators.mu.Lock()
		pending := b.pending
		creators.mu.Unlock()
		if pending != nil {
			<-pending.finished
		}
	}()
	if err := writeIPCFrame(conn, api.IPCResponse{
		ID: req.ID, OK: true, Result: api.RouteAggregateControlAttached{Generation: generation}, Final: true,
	}); err != nil {
		b.err = err
		return err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		b.err = err
		return err
	}
	creators.mu.Lock()
	if s.binding == b {
		b.ready = true
		s.notifyChangedLocked()
	}
	creators.mu.Unlock()

	// This same reader observes EOF in idle and active periods. Unknown,
	// duplicate and mismatched proofs revoke only this binding.
	for {
		proof, err := api.ReadRouteAggregateStatusProof(conn)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				err = errors.Join(errAggregateMalformed, err)
			}
			b.err = err
			return err
		}
		creators.mu.Lock()
		q := b.pending
		valid := s.binding == b && q != nil && !q.deadline.IsZero() && !q.received &&
			proof.ID == q.challenge.ID && proof.Challenge == q.challenge.Challenge &&
			proof.Generation == q.challenge.Generation && proof.Status().Valid()
		if valid {
			q.received = true
			q.reply <- proof
		}
		creators.mu.Unlock()
		if !valid {
			b.err = errAggregateMalformed
			return b.err
		}
	}
}

func aggregateIPCContext(r *daemonCreatorRecords) context.Context {
	if r == nil || r.routeAggregate == nil {
		return context.Background()
	}
	return r.routeAggregate.ctx
}

// waitRouteAggregateApplyLocked receives one original operation's readiness.
// It returns with mu held so the caller can reserve that exact slot.
func (r *daemonCreatorRecords) waitRouteAggregateApplyLocked(ctx context.Context, tracker *DaemonRuntimeTracker, original *daemonCreatorOperation, next int) error {
	if tracker == nil {
		return errAggregateUnavailable
	}
	deadline, _ := ctx.Deadline()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return context.DeadlineExceeded
		}
		// A close-and-replace notification can represent several publications.
		// If there was no original, take the first new Route row in order.
		for original == nil && next < len(r.operations) {
			operation := r.operations[next]
			next++
			if operation.task == api.BuiltinRouteTaskName {
				original = operation
			}
		}
		if original != nil {
			if latest := r.routeAggregateOperation(); latest != original {
				return errAggregatePeerChanged
			}
			if original.root.Known || original.terminal != nil || original.terminalErr != nil {
				return errors.Join(errAggregateUnavailable, original.rootErr, original.terminalErr)
			}
			if !original.starting {
				if original.startupErr != nil || original.unsupported || original.routeAggregateStarted == nil || original.owner == nil {
					return errors.Join(errAggregateUnavailable, original.startupErr)
				}
				row, current := tracker.Get(api.BuiltinRouteTaskName)
				if !current || row.CurrentPID <= 0 || original.generation <= 0 || row.PIDGeneration != original.generation {
					return errAggregatePeerChanged
				}
				b := r.routeAggregate.binding
				if b != nil && b.ready && b.operation == original && b.generation == original.generation {
					return nil
				}
			}
		}
		changed := r.routeAggregate.changed
		r.mu.Unlock()
		if beforeWait := r.routeAggregate.beforeApplyWait; beforeWait != nil {
			beforeWait()
		}
		select {
		case <-changed:
		case <-ctx.Done():
		}
		r.mu.Lock()
	}
}

func (r *daemonCreatorRecords) exchangeRouteAggregate(ctx context.Context, id int64, tracker *DaemonRuntimeTracker, apply *api.RouteAggregateApply) (api.RouteAggregateStatus, error) {
	var zero api.RouteAggregateStatus
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if r == nil || r.routeAggregate == nil {
		return zero, errAggregateUnavailable
	}
	var original *daemonCreatorOperation
	var next int
	if apply != nil {
		// The server request already carries its absolute deadline. This also
		// provides the same bound for internal callers without one.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, api.SupervisorAggregateStatusTimeout)
		defer cancel()
		r.mu.Lock()
		original = r.routeAggregateOperation()
		next = len(r.operations)
		r.mu.Unlock()
	}
	var nonce [api.RouteAggregateChallengeBytes]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return zero, err
	}
	r.mu.Lock()
	if apply != nil {
		if err := r.waitRouteAggregateApplyLocked(ctx, tracker, original, next); err != nil {
			r.mu.Unlock()
			return zero, err
		}
		// Observe cancellation and the clock again at slot reservation.
		deadline, _ := ctx.Deadline()
		if err := ctx.Err(); err != nil || !time.Now().Before(deadline) {
			r.mu.Unlock()
			if err != nil {
				return zero, err
			}
			return zero, context.DeadlineExceeded
		}
	}
	b := r.routeAggregate.binding
	if b == nil || !b.ready {
		r.mu.Unlock()
		return zero, errAggregateUnavailable
	}
	if b.pending != nil {
		r.mu.Unlock()
		return zero, errAggregateBusy
	}
	q := &routeAggregateQuery{
		challenge: api.RouteAggregateStatusChallenge{ID: id, Challenge: hex.EncodeToString(nonce[:]), Generation: b.generation, Apply: apply},
		reply:     make(chan api.RouteAggregateStatusProof, 1),
		finished:  make(chan struct{}),
	}
	b.pending = q
	r.mu.Unlock()

	// The original attachment owns the one exchange once its native write
	// starts. Its reader still receives the proof if the operator leaves.
	go func() {
		q.status, q.err = r.completeRouteAggregateQuery(ctx, b, q, tracker)
		r.mu.Lock()
		b.pending = nil
		close(q.finished)
		r.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-q.finished:
		if cause := ctx.Err(); cause != nil {
			return zero, cause
		}
		return q.status, q.err
	}
}

func (r *daemonCreatorRecords) completeRouteAggregateQuery(operator context.Context, b *routeAggregateBinding, q *routeAggregateQuery, tracker *DaemonRuntimeTracker) (api.RouteAggregateStatus, error) {
	var zero api.RouteAggregateStatus
	if cause := operator.Err(); cause != nil {
		return zero, cause
	}
	current, generation, err := r.observeRouteAggregatePeer(operator, b.conn, tracker)
	if cause := operator.Err(); cause != nil {
		return zero, cause
	}
	if err != nil || current != b.operation || generation != b.generation {
		r.revokeRouteAggregateBinding(b)
		return zero, errors.Join(errAggregatePeerChanged, err)
	}
	frame, err := json.Marshal(api.IPCResponse{ID: q.challenge.ID, OK: true, Result: q.challenge})
	if err != nil {
		return zero, errors.Join(errAggregateUnavailable, err)
	}
	frame = append(frame, '\n')
	var ctx context.Context
	var cancel context.CancelFunc
	if q.challenge.Apply != nil {
		deadline, _ := operator.Deadline()
		ctx, cancel = context.WithDeadline(aggregateIPCContext(r), deadline)
	} else {
		ctx, cancel = context.WithTimeout(aggregateIPCContext(r), api.SupervisorAggregateStatusTimeout)
	}
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := b.conn.SetWriteDeadline(deadline); err != nil {
		if cause := operator.Err(); cause != nil {
			return zero, cause
		}
		r.revokeRouteAggregateBinding(b)
		return zero, errors.Join(errAggregateUnavailable, err)
	}

	r.mu.Lock()
	if cause := operator.Err(); cause != nil {
		r.mu.Unlock()
		return zero, cause
	}
	if cause := ctx.Err(); cause != nil {
		r.mu.Unlock()
		return zero, cause
	}
	if r.routeAggregate.binding != b {
		r.mu.Unlock()
		return zero, errAggregatePeerChanged
	}
	if q.challenge.Apply != nil {
		row, current := tracker.Get(api.BuiltinRouteTaskName)
		if r.routeAggregateOperation() != b.operation || !current || row.CurrentPID <= 0 || row.PIDGeneration != b.generation {
			r.mu.Unlock()
			return zero, errAggregatePeerChanged
		}
		if !time.Now().Before(deadline) {
			r.mu.Unlock()
			return zero, context.DeadlineExceeded
		}
	}
	q.deadline = deadline
	r.mu.Unlock()
	sent, writeErr := b.conn.Write(frame)
	remaining := len(frame) - sent
	if writeErr == nil && remaining != 0 {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		q.writeErr = fmt.Errorf("write route aggregate challenge (%d sent, %d remaining): %w", sent, remaining, writeErr)
	}
	if errors.Is(writeErr, io.EOF) || errors.Is(writeErr, net.ErrClosed) || errors.Is(writeErr, os.ErrClosed) ||
		sent > 0 && remaining > 0 {
		// A broken transport or an unterminated frame cannot carry another query.
		r.revokeRouteAggregateBinding(b)
		return zero, errors.Join(errAggregateUnavailable, q.writeErr)
	}
	if sent == 0 && errors.Is(writeErr, io.ErrShortWrite) {
		// Even a confirmed zero-byte write leaves the binding reusable only
		// when this operation can still confirm the original peer.
		peerErr := r.checkRouteAggregateBinding(ctx, b, tracker)
		if peerErr != nil {
			r.revokeRouteAggregateBinding(b)
		}
		return zero, errors.Join(errAggregateUnavailable, q.writeErr, peerErr)
	}

	// A full or uncertain write can already have reached the original peer.
	// Only its correlated proof can complete the request; the original budget
	// still bounds the reader and worker after the operator leaves.
	select {
	case <-ctx.Done():
		r.revokeRouteAggregateBinding(b)
		return zero, errors.Join(ctx.Err(), q.writeErr)
	case <-b.done:
		return zero, errors.Join(errAggregateUnavailable, b.err, q.writeErr)
	case proof := <-q.reply:
		if err := r.checkRouteAggregateBinding(ctx, b, tracker); err != nil {
			r.revokeRouteAggregateBinding(b)
			return zero, errors.Join(err, q.writeErr)
		}
		return proof.Status(), nil
	}
}

func (r *daemonCreatorRecords) checkRouteAggregateBinding(ctx context.Context, b *routeAggregateBinding, tracker *DaemonRuntimeTracker) error {
	return r.checkRouteAggregateBindingWithReceipt(ctx, b, tracker, nil)
}

func (r *daemonCreatorRecords) checkRouteAggregateBindingWithReceipt(ctx context.Context, b *routeAggregateBinding,
	tracker *DaemonRuntimeTracker, phases *routeAdmissionNativeObservations) error {
	current, generation, err := r.observeRouteAggregatePeerWithReceipt(ctx, b.conn, tracker, phases, "binding")
	if cause := ctx.Err(); cause != nil {
		return errors.Join(cause, err)
	}
	r.mu.Lock()
	stillBound := r.routeAggregate.binding == b
	r.mu.Unlock()
	if !stillBound || err != nil || current != b.operation || generation != b.generation {
		return errors.Join(errAggregatePeerChanged, err)
	}
	return nil
}

func serveOperatorAggregateStatus(conn net.Conn, deps ipcDispatchDeps) (retErr error) {
	creators := deps.daemonCreators
	ctx, cancel := context.WithTimeout(aggregateIPCContext(creators), api.SupervisorAggregateStatusTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	if creators == nil || creators.routeAggregate == nil ||
		!admitAggregateStatusOperator(conn, creators.routeAggregate.launch.Peer) {
		return writeAggregateFailure(conn, 0, "OPERATOR_AGGREGATE_PEER_DENIED", "verified local operator required")
	}
	req, err := api.ReadOperatorAggregateStatusOpen(conn)
	if err != nil {
		return writeAggregateFailure(conn, 0, "OPERATOR_AGGREGATE_INVALID", "invalid bounded aggregate-status query")
	}
	if req.Cmd == api.RouteActivationAdmissionOpenCommand {
		return serveRouteActivationAdmission(ctx, conn, req, deps)
	}

	if req.Cmd == api.SupervisorAggregateVerifyCommand {
		generation, nonce, err := api.DecodeRouteAggregateVerification(req)
		original, current, observed := creators.observeRouteAggregatePeer(ctx, conn, deps.runtimeTracker)
		if err != nil || observed != nil || generation != current {
			return writeAggregateFailure(conn, req.ID, "OPERATOR_AGGREGATE_PEER_CHANGED", "original Route apply changed")
		}
		var applyDeadline time.Time
		creators.mu.Lock()
		b := creators.routeAggregate.binding
		if b != nil && b.ready && b.operation == original && b.generation == generation {
			q := b.pending
			if q != nil && q.challenge.Apply != nil && q.challenge.Challenge == nonce && time.Now().Before(q.deadline) {
				applyDeadline = q.deadline
			}
		}
		creators.mu.Unlock()
		if applyDeadline.IsZero() {
			return writeAggregateFailure(conn, req.ID, "OPERATOR_AGGREGATE_PEER_CHANGED", "original Route apply changed")
		}
		if applyDeadline.Before(deadline) {
			if err := conn.SetWriteDeadline(applyDeadline); err != nil {
				return err
			}
		}
		return writeIPCFrame(conn, api.IPCResponse{ID: req.ID, OK: true,
			Result: api.RouteAggregateControlAttached{Generation: generation}, Final: true})
	}
	var apply *api.RouteAggregateApply
	if req.Cmd == api.SupervisorAggregateApplyCommand {
		selection, err := api.DecodeRouteAggregateApply(req)
		if err != nil {
			return writeAggregateFailure(conn, req.ID, "OPERATOR_AGGREGATE_INVALID", "invalid original Product apply")
		}
		apply = &selection
	}

	// From the bounded request's final LF to transport close, this is
	// the only reader. EOF or an extra byte stops the operator's wait.
	var ending atomic.Bool
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		var extra [1]byte
		n, readErr := conn.Read(extra[:])
		if apply != nil && n == 0 && !time.Now().Before(deadline) {
			var timeout interface{ Timeout() bool }
			if errors.Is(readErr, os.ErrDeadlineExceeded) || (errors.As(readErr, &timeout) && timeout.Timeout()) {
				// The native pipe and request timers share this deadline.
				// Preserve DeadlineExceeded when the pipe timer wins.
				return
			}
		}
		if ending.CompareAndSwap(false, true) {
			cancel()
		}
	}()
	defer func() {
		retErr = errors.Join(retErr, conn.Close())
		<-readerDone
	}()
	result, err := creators.exchangeRouteAggregate(ctx, req.ID, deps.runtimeTracker, apply)
	if cause := ctx.Err(); cause != nil {
		return cause
	}
	if !ending.CompareAndSwap(false, true) {
		return context.Canceled
	}
	if err != nil {
		switch {
		case errors.Is(err, errAggregateMalformed):
			return writeAggregateFailure(conn, req.ID, "OPERATOR_AGGREGATE_MALFORMED", "malformed route reply")
		case errors.Is(err, errAggregateBusy):
			return writeAggregateFailure(conn, req.ID, "OPERATOR_AGGREGATE_BUSY", "another live query is in progress")
		case errors.Is(err, errAggregatePeerChanged):
			return writeAggregateFailure(conn, req.ID, "OPERATOR_AGGREGATE_PEER_CHANGED", "original route generation changed")
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
			return writeAggregateFailure(conn, req.ID, "OPERATOR_AGGREGATE_TIMEOUT", "live route query timed out")
		default:
			return writeAggregateFailure(conn, req.ID, "OPERATOR_AGGREGATE_UNAVAILABLE", "no live route reply available")
		}
	}
	return writeIPCFrame(conn, api.IPCResponse{ID: req.ID, OK: true, Result: result, Final: true})
}

func startRouteAggregateStatus(ctx context.Context, diagnostics io.Writer, control func(api.RouteAggregateLaunch) api.RouteAggregateControl) func() error {
	encoded, issued := os.LookupEnv(routeAggregateLaunchEnv)
	if !issued {
		return func() error { return nil }
	}
	var launch api.RouteAggregateLaunch
	if err := json.Unmarshal([]byte(encoded), &launch); err != nil {
		fmt.Fprintln(diagnostics, "route: invalid aggregate-status supervisor launch")
		return func() error { return nil }
	}
	receiver := control(launch)
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var outcome api.RouteAggregateControlExit
	// runRoute returns the joined outcome to the command's error handler.
	go func() {
		defer close(done)
		outcome = api.ServeRouteAggregateControlObserved(workerCtx, launch, receiver)
	}()

	type stopEpisode struct {
		done chan struct{}
		err  error
	}
	var stopMu sync.Mutex
	var current *stopEpisode
	return func() error {
		stopMu.Lock()
		first := current == nil
		if current != nil {
			select {
			case <-current.done:
			default:
				old := current
				stopMu.Unlock()
				<-old.done
				return old.err
			}
		}
		workerJoined := false
		select {
		case <-done:
			workerJoined = true
		default:
		}
		episode := &stopEpisode{done: make(chan struct{})}
		current = episode
		stopMu.Unlock()

		cancel()
		<-done
		var advanceErr error
		// A stop that canceled the worker, or overlaps the Route's original
		// cancellation, receives that detach episode. Only a later admitted
		// stop can advance the same retained H owner with a fresh stop budget.
		if workerJoined && (!first || !outcome.RouteStopped) && !outcome.Retirement.Settled() {
			retireCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), api.SupervisorAggregateStatusTimeout)
			advanceErr = outcome.AdvanceOriginalRetirement(retireCtx)
			finish()
		}
		final := outcome
		if workerCtx.Err() != nil && final.IPCError == workerCtx.Err() {
			final.IPCError = nil
		}
		episode.err = errors.Join(final.Failure(), advanceErr)
		close(episode.done)
		return episode.err
	}
}

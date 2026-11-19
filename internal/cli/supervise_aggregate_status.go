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
)

const routeAggregateLaunchEnv = "MCPHUB_ROUTE_AGGREGATE_SUPERVISOR"

type routeAggregateSupervisor struct {
	ctx       context.Context
	launch    api.RouteAggregateLaunch
	launchEnv string
	binding   *routeAggregateBinding // daemonCreatorRecords.mu

	beforeDetach func(*routeAggregateBinding) // fixture barrier; nil in production
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
	return &routeAggregateSupervisor{ctx: ctx, launch: launch, launchEnv: routeAggregateLaunchEnv + "=" + string(payload)}, nil
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
	refused := errors.New("current original route peer required")
	if r == nil || r.routeAggregate == nil || tracker == nil {
		return nil, 0, refused
	}
	peer, err := observeAggregateIPCClient(conn)
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

	identity, err := creator.ObserveCreatorIdentity(ctx)
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
}

func (c *aggregateIPCConn) Close() error {
	c.nativeMu.Lock()
	defer c.nativeMu.Unlock()
	return c.Conn.Close()
}

func observeAggregateIPCClient(conn net.Conn) (api.SupervisorProcessIdentityV1, error) {
	if owned, ok := conn.(*aggregateIPCConn); ok {
		owned.nativeMu.Lock()
		defer owned.nativeMu.Unlock()
		conn = owned.Conn
	}
	return api.ObserveSupervisorIPCClientIdentityV1(conn)
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
	received  bool // daemonCreatorRecords.mu
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
	}
	r.mu.Unlock()
	b.close()
}

func (r *daemonCreatorRecords) retireRouteAggregateBinding(operation *daemonCreatorOperation) {
	if r.routeAggregate == nil {
		return
	}
	r.mu.Lock()
	b := r.routeAggregate.binding
	r.mu.Unlock()
	if b != nil && b.operation == operation {
		r.revokeRouteAggregateBinding(b)
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
	b.ready = s.binding == b
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
		valid := s.binding == b && q != nil && !q.received &&
			proof.ID == q.challenge.ID && proof.Challenge == q.challenge.Challenge &&
			proof.Generation == q.challenge.Generation && proof.Ownership == api.RouteAggregateNotOwner
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

func (r *daemonCreatorRecords) queryRouteAggregateStatus(ctx context.Context, id int64, tracker *DaemonRuntimeTracker) (api.RouteAggregateStatus, error) {
	var zero api.RouteAggregateStatus
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if r == nil || r.routeAggregate == nil {
		return zero, errAggregateUnavailable
	}
	var nonce [api.RouteAggregateChallengeBytes]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return zero, err
	}
	r.mu.Lock()
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
		challenge: api.RouteAggregateStatusChallenge{ID: id, Challenge: hex.EncodeToString(nonce[:]), Generation: b.generation},
		reply:     make(chan api.RouteAggregateStatusProof, 1),
	}
	b.pending = q
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		b.pending = nil
		r.mu.Unlock()
	}()

	current, generation, err := r.observeRouteAggregatePeer(ctx, b.conn, tracker)
	if err != nil || current != b.operation || generation != b.generation {
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		r.revokeRouteAggregateBinding(b)
		<-b.done
		return zero, errAggregatePeerChanged
	}
	sent, err := writeRouteAggregateQuery(ctx, b.conn, q.challenge)
	if err != nil {
		if sent || ctx.Err() == nil {
			r.revokeRouteAggregateBinding(b)
			<-b.done
		}
		if cause := ctx.Err(); cause != nil {
			return zero, cause
		}
		return zero, errors.Join(errAggregateUnavailable, err)
	}
	select {
	case <-ctx.Done():
		r.revokeRouteAggregateBinding(b)
		<-b.done
		return zero, ctx.Err()
	case <-b.done:
		return zero, errors.Join(errAggregateUnavailable, b.err)
	case proof := <-q.reply:
		current, generation, err := r.observeRouteAggregatePeer(ctx, b.conn, tracker)
		if cause := ctx.Err(); cause != nil {
			r.revokeRouteAggregateBinding(b)
			<-b.done
			return zero, cause
		}
		r.mu.Lock()
		stillBound := r.routeAggregate.binding == b
		r.mu.Unlock()
		if !stillBound || err != nil || current != b.operation || generation != b.generation {
			r.revokeRouteAggregateBinding(b)
			<-b.done
			return zero, errAggregatePeerChanged
		}
		return api.RouteAggregateStatus{Ownership: proof.Ownership, Generation: generation}, nil
	}
}

func writeRouteAggregateQuery(ctx context.Context, conn net.Conn, challenge api.RouteAggregateStatusChallenge) (bool, error) {
	deadline, _ := ctx.Deadline()
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return false, err
	}
	wakeup := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(wakeup)
		_ = conn.SetWriteDeadline(time.Now())
	})
	defer func() {
		if !stop() {
			<-wakeup
		}
	}()
	frame, err := json.Marshal(api.IPCResponse{ID: challenge.ID, OK: true, Result: challenge})
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	frame = append(frame, '\n')
	n, err := conn.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return n != 0, err
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

	// From the bounded request's final LF to transport close, this is
	// the only reader. EOF or an extra byte cancels the live query.
	var ending atomic.Bool
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		var extra [1]byte
		_, _ = conn.Read(extra[:])
		if ending.CompareAndSwap(false, true) {
			cancel()
		}
	}()
	defer func() {
		retErr = errors.Join(retErr, conn.Close())
		<-readerDone
	}()
	result, err := creators.queryRouteAggregateStatus(ctx, req.ID, deps.runtimeTracker)
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

func startRouteAggregateStatus(ctx context.Context, diagnostics io.Writer, observe func() string) func() {
	encoded, issued := os.LookupEnv(routeAggregateLaunchEnv)
	if !issued {
		return func() {}
	}
	var launch api.RouteAggregateLaunch
	if err := json.Unmarshal([]byte(encoded), &launch); err != nil {
		fmt.Fprintln(diagnostics, "route: invalid aggregate-status supervisor launch")
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := api.ServeRouteAggregateStatus(ctx, launch, observe); err != nil && ctx.Err() == nil {
			fmt.Fprintf(diagnostics, "route: aggregate-status unavailable: %v\n", err)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

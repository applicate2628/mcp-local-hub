package cli

import (
	"bufio"
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
	"time"

	"mcp-local-hub/internal/api"
)

const routeAggregateLaunchEnv = "MCPHUB_ROUTE_AGGREGATE_SUPERVISOR"

type routeAggregateSupervisor struct {
	ctx       context.Context
	launch    api.RouteAggregateLaunch
	launchEnv string
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
	peer, err := api.ObserveSupervisorIPCClientIdentityV1(conn)
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

func serveRouteAggregateStatus(conn net.Conn, deps ipcDispatchDeps) error {
	var requestID int64
	refuse := func(code, message string) error {
		return writeIPCFrame(conn, api.IPCResponse{ID: requestID, Error: &api.IPCErr{Code: code, Message: message}, Final: true})
	}
	creators := deps.daemonCreators
	parent := context.Background()
	if creators != nil && creators.routeAggregate != nil {
		parent = creators.routeAggregate.ctx
	}
	ctx, cancel := context.WithTimeout(parent, api.SupervisorAggregateStatusTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	original, generation, err := creators.observeRouteAggregatePeer(ctx, conn, deps.runtimeTracker)
	if err != nil {
		return refuse("ROUTE_AGGREGATE_PEER_DENIED", "current original route peer required")
	}

	// This is the first application-body read. The marker used a single raw
	// connection byte and no buffered I/O took place before native admission.
	req, err := api.ReadRouteAggregateStatusOpen(conn)
	if err != nil {
		return refuse("ROUTE_AGGREGATE_INVALID", "invalid bounded aggregate-status v1 query")
	}
	requestID = req.ID
	if deps.events != nil {
		_ = deps.events.TryEmit(api.SupervisorEvent{
			Severity: "info", Source: "ipc", Event: "ipc-command",
			Body: map[string]any{"cmd": req.Cmd, "id": req.ID},
		})
	}

	var nonce [api.RouteAggregateChallengeBytes]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	challenge := api.RouteAggregateStatusChallenge{Challenge: hex.EncodeToString(nonce[:]), Generation: generation}
	if err := writeIPCFrame(conn, api.IPCResponse{ID: req.ID, OK: true, Result: challenge}); err != nil {
		return err
	}
	// The challenge reply is a single small control frame on this connection.
	// It must fit the existing IPC reader buffer and end with LF.
	line, err := bufio.NewReader(conn).ReadSlice('\n')
	if err != nil {
		return err
	}
	var proof api.RouteAggregateStatusProof
	if err := json.Unmarshal(line, &proof); err != nil || proof.ID != req.ID ||
		proof.Challenge != challenge.Challenge || proof.Generation != generation || proof.Effective != api.RouteAggregateOff {
		return refuse("ROUTE_AGGREGATE_PROOF_INVALID", "aggregate-status proof did not match this exchange")
	}
	current, currentGeneration, err := creators.observeRouteAggregatePeer(ctx, conn, deps.runtimeTracker)
	if err != nil || current != original || currentGeneration != generation {
		return refuse("ROUTE_AGGREGATE_PEER_DENIED", "original route generation changed")
	}
	return writeIPCFrame(conn, api.IPCResponse{
		ID: req.ID, OK: true, Result: api.RouteAggregateStatus{Effective: proof.Effective, Generation: generation}, Final: true,
	})
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
		status, err := api.DialRouteAggregateStatus(ctx, launch, observe)
		if err != nil {
			if ctx.Err() == nil {
				fmt.Fprintf(diagnostics, "route: aggregate-status refused: %v\n", err)
			}
			return
		}
		fmt.Fprintf(diagnostics, "route: aggregate-status effective=%s generation=%d\n", status.Effective, status.Generation)
	}()
	return func() {
		cancel()
		<-done
	}
}

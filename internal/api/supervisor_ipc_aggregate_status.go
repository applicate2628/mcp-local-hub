package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const SupervisorAggregateStatusCommand = "aggregate-status"
const SupervisorAggregateStatusTimeout = 5 * time.Second

// RouteAggregateStatusMarker selects the private mode at the beginning of
// this existing stream. A raw record separator cannot start a JSON value.
const RouteAggregateStatusMarker byte = 0x1e
const OperatorAggregateStatusMarker byte = 0x1f

const RouteAggregateControlAttachCommand = "aggregate-control-attach"

// ReadRouteAggregateStatusOpen reads the attach after native route admission.
func ReadRouteAggregateStatusOpen(source io.Reader) (IPCRequest, error) {
	return readAggregateStatusRequest(source, RouteAggregateControlAttachCommand)
}

// ReadOperatorAggregateStatusOpen reads one query after native operator admission.
func ReadOperatorAggregateStatusOpen(source io.Reader) (IPCRequest, error) {
	return readAggregateStatusRequest(source, SupervisorAggregateStatusCommand)
}

func readAggregateStatusRequest(source io.Reader, command string) (IPCRequest, error) {
	// Compact v1 request with no args, the largest positive ID and final LF.
	const fixedBytes = len(`{"version":1,"id":9223372036854775807,"cmd":""}`) + 1
	line, err := readSupervisorIPCLine(source, fixedBytes+len(command))
	if err != nil {
		return IPCRequest{}, err
	}
	var req IPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return IPCRequest{}, err
	}
	if req.Version != 1 || req.ID <= 0 || req.Cmd != command || len(req.Args) != 0 {
		return IPCRequest{}, fmt.Errorf("aggregate-status requires the explicit v1 query")
	}
	return req, nil
}

// RouteAggregateLaunch identifies the issuing supervisor. It carries no
// credential and cannot confer route authority on a different process.
type RouteAggregateLaunch struct {
	StateDir string                      `json:"state_dir"`
	Owner    SupervisorLockOwner         `json:"owner"`
	Peer     SupervisorProcessIdentityV1 `json:"peer"`
}

type RouteAggregateStatusChallenge struct {
	ID         int64  `json:"id"`
	Challenge  string `json:"challenge"`
	Generation int    `json:"generation"`
}

type RouteAggregateStatusProof struct {
	ID         int64  `json:"id"`
	Challenge  string `json:"challenge"`
	Generation int    `json:"generation"`
	Ownership  string `json:"ownership"`
}

type RouteAggregateStatus struct {
	Ownership  string `json:"ownership"`
	Generation int    `json:"generation"`
}

const (
	RouteAggregateNotOwner       = "route-not-owner"
	RouteAggregateChallengeBytes = 16
)

type RouteAggregateControlAttached struct {
	Generation int `json:"generation"`
}

// ReadRouteAggregateStatusProof reads exactly one proof, leaving later bytes
// to the same connection reader. The limit follows the closed r2 wire shape.
func ReadRouteAggregateStatusProof(source io.Reader) (RouteAggregateStatusProof, error) {
	const fixedBytes = len(`{"id":9223372036854775807,"challenge":"","generation":,"ownership":""}`) + 1
	const generationBytes = 10 + (strconv.IntSize-32)*9/32
	var proof RouteAggregateStatusProof
	line, err := readSupervisorIPCLine(source, fixedBytes+2*RouteAggregateChallengeBytes+generationBytes+len(RouteAggregateNotOwner))
	if err != nil {
		return proof, err
	}
	err = json.Unmarshal(line, &proof)
	return proof, err
}

// ServeRouteAggregateStatus attaches the original route to the issuing
// supervisor and answers read-only queries from the route's local composition.
// The route lifetime owns this connection and any one eligible re-attach.
func ServeRouteAggregateStatus(ctx context.Context, launch RouteAggregateLaunch, observe func() string) error {
	if ctx == nil || observe == nil || !filepath.IsAbs(launch.StateDir) {
		return errors.New("route aggregate status: original launch and observer required")
	}
	if err := validateSupervisorControlPeerExpectation(launch.Owner, launch.Peer); err != nil {
		return err
	}
	attached, err := serveRouteAggregateAttachment(ctx, launch, observe)
	if attached && ctx.Err() == nil && aggregateRoutePeerClosed(err) {
		_, err = serveRouteAggregateAttachment(ctx, launch, observe)
	}
	return err
}

func serveRouteAggregateAttachment(ctx context.Context, launch RouteAggregateLaunch, observe func() string) (attached bool, retErr error) {
	openCtx, cancel := context.WithTimeout(ctx, SupervisorAggregateStatusTimeout)
	defer cancel()
	conn, err := dialSupervisorIPC(openCtx, SupervisorIPCAddress(launch.StateDir))
	if err != nil {
		return false, err
	}
	defer func() {
		retErr = errors.Join(retErr, conn.Close())
	}()
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(closed)
		_ = conn.SetDeadline(time.Now())
	})
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	deadline, _ := openCtx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return false, err
	}
	if err := ValidateSupervisorControlPeerConnectionV1(conn, launch.Owner, launch.Peer); err != nil {
		return false, err
	}
	if err := validateSupervisorIPCHello(conn, launch.Owner); err != nil {
		return false, err
	}
	if err := openCtx.Err(); err != nil {
		return false, err
	}
	open := IPCRequest{Version: 1, ID: time.Now().UnixNano(), Cmd: RouteAggregateControlAttachCommand}
	if err := writeSupervisorIPCRequest(conn, open, RouteAggregateStatusMarker); err != nil {
		return false, err
	}
	frame, err := readSupervisorIPCResponse(conn)
	if err != nil {
		return false, err
	}
	if !frame.OK && frame.Final && frame.Error != nil && (frame.ID == 0 || frame.ID == open.ID) {
		return false, frame.Error
	}
	var ack RouteAggregateControlAttached
	if frame.ID != open.ID || !frame.OK || !frame.Final || frame.Error != nil ||
		json.Unmarshal(frame.Result, &ack) != nil || ack.Generation <= 0 {
		return false, errors.New("route aggregate status: invalid attach acknowledgement")
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return false, err
	}
	cancel()
	// Resetting the open deadline cannot leave a canceled route blocked in Read.
	if err := ctx.Err(); err != nil {
		return true, err
	}
	for ctx.Err() == nil {
		frame, err := readSupervisorIPCResponse(conn)
		if err != nil {
			return true, err
		}
		var challenge RouteAggregateStatusChallenge
		if !frame.OK || frame.Final || frame.Error != nil || json.Unmarshal(frame.Result, &challenge) != nil ||
			frame.ID != challenge.ID || challenge.ID <= 0 || challenge.Generation != ack.Generation {
			return true, errors.New("route aggregate status: invalid query")
		}
		nonce, err := hex.DecodeString(challenge.Challenge)
		if err != nil || len(nonce) != RouteAggregateChallengeBytes {
			return true, errors.New("route aggregate status: invalid query nonce")
		}
		if err := ctx.Err(); err != nil {
			return true, err
		}
		proof := RouteAggregateStatusProof{
			ID: challenge.ID, Challenge: challenge.Challenge, Generation: challenge.Generation, Ownership: observe(),
		}
		if err := conn.SetWriteDeadline(time.Now().Add(SupervisorAggregateStatusTimeout)); err != nil {
			return true, err
		}
		if err := ctx.Err(); err != nil {
			return true, err
		}
		if err := json.NewEncoder(conn).Encode(proof); err != nil {
			return true, err
		}
	}
	return true, ctx.Err()
}

// QuerySupervisorAggregateStatus sends one query. The caller must hold the
// installed file and running-process observations until checkCurrent and the
// connection close complete. Only the current route supplies the result.
func QuerySupervisorAggregateStatus(ctx context.Context, stateDir string, owner SupervisorLockOwner, peer SupervisorProcessIdentityV1, checkCurrent func(context.Context) error) (status RouteAggregateStatus, retErr error) {
	if ctx == nil || checkCurrent == nil || !filepath.IsAbs(stateDir) {
		return status, ErrSupervisorIPCUnavailable
	}
	if err := validateSupervisorControlPeerExpectation(owner, peer); err != nil {
		return status, ErrSupervisorControlPeerIdentity
	}
	ctx, cancel := context.WithTimeout(ctx, SupervisorAggregateStatusTimeout)
	defer cancel()
	conn, err := dialSupervisorIPC(ctx, SupervisorIPCAddress(stateDir))
	if err != nil {
		return status, aggregateStatusTransportError(ctx, err)
	}
	defer func() {
		if err := conn.Close(); retErr == nil && err != nil {
			retErr = ErrSupervisorIPCUnavailable
		}
		if retErr != nil {
			status = RouteAggregateStatus{}
		}
	}()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return status, ErrSupervisorIPCUnavailable
	}
	wakeup := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(wakeup)
		_ = conn.SetDeadline(time.Now())
	})
	defer func() {
		if !stop() {
			<-wakeup
		}
	}()
	if err := ValidateSupervisorControlPeerConnectionV1(conn, owner, peer); err != nil {
		return status, ErrSupervisorControlPeerIdentity
	}
	if err := validateSupervisorIPCHello(conn, owner); err != nil {
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		return status, ErrSupervisorControlPeerIdentity
	}
	if err := ctx.Err(); err != nil {
		return status, err
	}
	req := IPCRequest{Version: 1, ID: time.Now().UnixNano(), Cmd: SupervisorAggregateStatusCommand}
	if err := writeSupervisorIPCRequest(conn, req, OperatorAggregateStatusMarker); err != nil {
		return status, aggregateStatusTransportError(ctx, err)
	}
	resp, err := readSupervisorIPCResponse(conn)
	if err != nil {
		return status, aggregateStatusTransportError(ctx, err)
	}
	if !resp.OK && resp.Final && resp.Error != nil && (resp.ID == req.ID || resp.ID == 0) {
		switch resp.Error.Code {
		case "OPERATOR_AGGREGATE_PEER_DENIED", "OPERATOR_AGGREGATE_INVALID":
			return status, errors.New(resp.Error.Code)
		case "OPERATOR_AGGREGATE_BUSY", "OPERATOR_AGGREGATE_MALFORMED", "OPERATOR_AGGREGATE_PEER_CHANGED",
			"OPERATOR_AGGREGATE_TIMEOUT", "OPERATOR_AGGREGATE_UNAVAILABLE":
			if resp.ID == req.ID {
				return status, errors.New(resp.Error.Code)
			}
		}
	}
	if resp.ID != req.ID || !resp.OK || !resp.Final || resp.Error != nil ||
		json.Unmarshal(resp.Result, &status) != nil || status.Ownership != RouteAggregateNotOwner || status.Generation <= 0 {
		return status, errors.New("OPERATOR_AGGREGATE_MALFORMED")
	}
	if err := checkCurrent(ctx); err != nil {
		if ctx.Err() != nil {
			return status, ctx.Err()
		}
		return status, ErrSupervisorControlPeerIdentity
	}
	if err := ValidateSupervisorControlPeerConnectionV1(conn, owner, peer); err != nil {
		return status, ErrSupervisorControlPeerIdentity
	}
	return status, ctx.Err()
}

func aggregateStatusTransportError(ctx context.Context, err error) error {
	if cause := ctx.Err(); cause != nil {
		return cause
	}
	var timeout interface{ Timeout() bool }
	if errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return context.DeadlineExceeded
	}
	return ErrSupervisorIPCUnavailable
}

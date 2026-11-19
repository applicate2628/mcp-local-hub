package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"time"
)

const SupervisorAggregateStatusCommand = "aggregate-status"
const SupervisorAggregateStatusTimeout = 5 * time.Second

// RouteAggregateStatusMarker selects the private mode at the beginning of
// this existing stream. A raw record separator cannot start a JSON value.
const RouteAggregateStatusMarker byte = 0x1e

// The bounded v1 query is the compact IPCRequest with no args, the fixed
// command, and the greatest positive int64 ID. Include the terminating LF.
const routeAggregateStatusOpenBytes = len(`{"version":1,"id":9223372036854775807,"cmd":""}`) + len(SupervisorAggregateStatusCommand) + 1

// ReadRouteAggregateStatusOpen reads the query after native route admission.
// Peer authentication and the initial marker belong to the connection owner.
func ReadRouteAggregateStatusOpen(source io.Reader) (IPCRequest, error) {
	line, err := readSupervisorIPCLine(source, routeAggregateStatusOpenBytes)
	if err != nil {
		return IPCRequest{}, err
	}
	var req IPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return IPCRequest{}, err
	}
	if req.Version != 1 || req.ID <= 0 || req.Cmd != SupervisorAggregateStatusCommand || len(req.Args) != 0 {
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
	Challenge  string `json:"challenge"`
	Generation int    `json:"generation"`
}

type RouteAggregateStatusProof struct {
	ID         int64  `json:"id"`
	Challenge  string `json:"challenge"`
	Generation int    `json:"generation"`
	Effective  string `json:"effective"`
}

type RouteAggregateStatus struct {
	Effective  string `json:"effective"`
	Generation int    `json:"generation"`
}

const (
	RouteAggregateOff            = "off"
	RouteAggregateChallengeBytes = 16
)

// DialRouteAggregateStatus performs one read-only exchange. observe samples
// the route's local aggregate component only after native peer verification.
func DialRouteAggregateStatus(ctx context.Context, launch RouteAggregateLaunch, observe func() string) (result RouteAggregateStatus, retErr error) {
	if ctx == nil || observe == nil || !filepath.IsAbs(launch.StateDir) {
		return result, fmt.Errorf("route aggregate status: original launch and observer required")
	}
	if err := validateSupervisorControlPeerExpectation(launch.Owner, launch.Peer); err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, SupervisorAggregateStatusTimeout)
	defer cancel()
	conn, err := dialSupervisorIPC(ctx, SupervisorIPCAddress(launch.StateDir))
	if err != nil {
		return result, fmt.Errorf("route aggregate status: %w", err)
	}
	defer func() {
		if retErr = errors.Join(retErr, conn.Close()); retErr != nil {
			result = RouteAggregateStatus{}
		}
	}()
	return exchangeRouteAggregateStatus(ctx, conn, launch, observe)
}

func exchangeRouteAggregateStatus(ctx context.Context, conn net.Conn, launch RouteAggregateLaunch, observe func() string) (result RouteAggregateStatus, err error) {
	deadline, finite := ctx.Deadline()
	if !finite {
		return result, fmt.Errorf("route aggregate status: original exchange deadline required")
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return result, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if err := ValidateSupervisorControlPeerConnectionV1(conn, launch.Owner, launch.Peer); err != nil {
		return result, err
	}
	if err := validateSupervisorIPCHello(conn, launch.Owner); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	req := IPCRequest{Version: 1, ID: time.Now().UnixNano(), Cmd: SupervisorAggregateStatusCommand}
	if err := writeSupervisorIPCRequest(conn, req, RouteAggregateStatusMarker); err != nil {
		return result, err
	}
	reply, err := readRouteAggregateFrame(conn, req.ID, false)
	if err != nil {
		return result, err
	}
	var challenge RouteAggregateStatusChallenge
	if err := json.Unmarshal(reply, &challenge); err != nil {
		return result, err
	}
	nonce, err := hex.DecodeString(challenge.Challenge)
	if err != nil || len(nonce) != RouteAggregateChallengeBytes || challenge.Generation <= 0 {
		return result, fmt.Errorf("route aggregate status: invalid challenge")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	effective := observe()
	if effective != RouteAggregateOff {
		return result, fmt.Errorf("route aggregate status: local aggregate is not confirmed off")
	}
	proof := RouteAggregateStatusProof{
		ID: req.ID, Challenge: challenge.Challenge, Generation: challenge.Generation, Effective: effective,
	}
	if err := json.NewEncoder(conn).Encode(proof); err != nil {
		return result, err
	}
	reply, err = readRouteAggregateFrame(conn, req.ID, true)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(reply, &result); err != nil {
		return RouteAggregateStatus{}, err
	}
	if result.Generation != challenge.Generation || result.Effective != effective {
		return RouteAggregateStatus{}, fmt.Errorf("route aggregate status: final result differs from original proof")
	}
	return result, ctx.Err()
}

func readRouteAggregateFrame(conn net.Conn, id int64, final bool) (json.RawMessage, error) {
	frame, err := readSupervisorIPCResponse(conn)
	if err != nil {
		return nil, err
	}
	// The supervisor can refuse before reading any request bytes and before
	// learning a correlation ID. This is terminal only in the opening phase.
	if !final && frame.ID == 0 && frame.Final && !frame.OK && frame.Error != nil {
		switch frame.Error.Code {
		case "ROUTE_AGGREGATE_PEER_DENIED", "ROUTE_AGGREGATE_INVALID":
			return nil, frame.Error
		}
	}
	if frame.ID != id {
		return nil, fmt.Errorf("route aggregate status: response id mismatch")
	}
	if frame.Error != nil {
		return nil, frame.Error
	}
	if !frame.OK || frame.Final != final || len(frame.Result) == 0 {
		return nil, fmt.Errorf("route aggregate status: invalid response phase")
	}
	return frame.Result, nil
}

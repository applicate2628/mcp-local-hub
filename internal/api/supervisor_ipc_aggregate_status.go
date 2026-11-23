package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const SupervisorAggregateStatusCommand = "aggregate-status"
const SupervisorAggregateApplyCommand = "aggregate-apply"
const SupervisorAggregateVerifyCommand = "aggregate-verify-generation"
const RouteActivationAdmissionOpenCommand = "aggregate-route-admission"
const RouteActivationAdmissionConfirmCommand = "aggregate-route-admit"
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

// ReadOperatorAggregateStatusOpen selects one bounded operation after native
// operator admission. Status retains its existing empty argument shape.
func ReadOperatorAggregateStatusOpen(source io.Reader) (IPCRequest, error) {
	const limit = len(`{"version":1,"id":9223372036854775807,"cmd":"aggregate-apply","args":{"receipt_sha256":"","endpoint_sha256":""}}`) + 2*64 + 1
	line, err := readSupervisorIPCLine(source, limit)
	if err != nil {
		return IPCRequest{}, err
	}
	var req IPCRequest
	if err := json.Unmarshal(line, &req); err != nil || req.Version != 1 || req.ID <= 0 {
		return IPCRequest{}, errors.New("invalid aggregate control envelope")
	}
	if req.Cmd == SupervisorAggregateStatusCommand && len(req.Args) == 0 &&
		len(line) <= len(`{"version":1,"id":9223372036854775807,"cmd":"aggregate-status"}`) {
		return req, nil
	}
	if req.Cmd == RouteActivationAdmissionOpenCommand && len(req.Args) == 0 {
		canonical, err := json.Marshal(req)
		if err == nil && bytes.Equal(canonical, line) {
			return req, nil
		}
		return IPCRequest{}, errors.New("invalid Route activation admission open")
	}
	if req.Cmd == SupervisorAggregateVerifyCommand {
		if _, _, err := DecodeRouteAggregateVerification(req); err != nil {
			return IPCRequest{}, err
		}
	} else if _, err := DecodeRouteAggregateApply(req); err != nil {
		return IPCRequest{}, err
	}
	canonical, err := json.Marshal(req)
	if err != nil || !bytes.Equal(canonical, line) {
		return IPCRequest{}, errors.New("aggregate apply requires its exact typed envelope")
	}
	return req, nil
}

type RouteActivationAdmissionChallenge struct {
	Challenge string                   `json:"challenge"`
	Selection RouteActivationSelection `json:"selection"`
}

// ReadRouteActivationAdmissionConfirm consumes only the second frame on the
// already admitted operator stream. Neither request carries path or PID input.
func ReadRouteActivationAdmissionConfirm(source io.Reader, id int64, challenge string) error {
	if id <= 0 || len(challenge) != 2*RouteAggregateChallengeBytes {
		return errors.New("original Route activation admission challenge required")
	}
	const fixedBytes = len(`{"version":1,"id":9223372036854775807,"cmd":"aggregate-route-admit","args":{"challenge":""}}`) + 1
	line, err := readSupervisorIPCLine(source, fixedBytes+2*RouteAggregateChallengeBytes)
	if err != nil {
		return err
	}
	var req IPCRequest
	if json.Unmarshal(line, &req) != nil || req.Version != 1 || req.ID != id ||
		req.Cmd != RouteActivationAdmissionConfirmCommand || len(req.Args) != 1 || req.Args["challenge"] != challenge {
		return errors.New("invalid Route activation admission confirmation")
	}
	canonical, err := json.Marshal(req)
	if err != nil || !bytes.Equal(canonical, line) {
		return errors.New("Route activation admission requires its exact typed envelope")
	}
	return nil
}

type RouteAggregateApply struct {
	ReceiptSHA256  string `json:"receipt_sha256"`
	EndpointSHA256 string `json:"endpoint_sha256"`
}

func (a RouteAggregateApply) Valid() bool {
	for _, text := range []string{a.ReceiptSHA256, a.EndpointSHA256} {
		if decoded, err := hex.DecodeString(text); err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != text {
			return false
		}
	}
	return true
}

func DecodeRouteAggregateApply(req IPCRequest) (RouteAggregateApply, error) {
	var result RouteAggregateApply
	if req.Cmd == SupervisorAggregateApplyCommand && len(req.Args) == 2 {
		result.ReceiptSHA256, _ = req.Args["receipt_sha256"].(string)
		result.EndpointSHA256, _ = req.Args["endpoint_sha256"].(string)
		if result.Valid() {
			return result, nil
		}
	}
	return RouteAggregateApply{}, errors.New("invalid aggregate apply selection")
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

func DecodeRouteAggregateVerification(req IPCRequest) (int, string, error) {
	generation, _ := req.Args["generation"].(string)
	gen, parseErr := strconv.Atoi(generation)
	nonce, _ := req.Args["challenge"].(string)
	if req.Cmd == SupervisorAggregateVerifyCommand && len(req.Args) == 2 &&
		parseErr == nil && gen > 0 && strconv.Itoa(gen) == generation {
		if decoded, err := hex.DecodeString(nonce); err == nil &&
			len(decoded) == RouteAggregateChallengeBytes && hex.EncodeToString(decoded) == nonce {
			return gen, nonce, nil
		}
	}
	return 0, "", errors.New("invalid aggregate generation verification")
}

// RouteAggregateLaunch identifies the issuing supervisor. It carries no
// credential and cannot confer route authority on a different process.
type RouteAggregateLaunch struct {
	StateDir string                      `json:"state_dir"`
	Owner    SupervisorLockOwner         `json:"owner"`
	Peer     SupervisorProcessIdentityV1 `json:"peer"`
}

type RouteAggregateStatusChallenge struct {
	ID         int64                `json:"id"`
	Challenge  string               `json:"challenge"`
	Generation int                  `json:"generation"`
	Apply      *RouteAggregateApply `json:"apply,omitempty"`
}

type RouteAggregateStatusProof struct {
	ID         int64  `json:"id"`
	Challenge  string `json:"challenge"`
	Generation int    `json:"generation"`
	Ownership  string `json:"ownership"`
	Revision   uint64 `json:"revision,omitempty"`
	Effective  string `json:"effective,omitempty"`
}

type RouteAggregateStatus struct {
	Ownership  string                   `json:"ownership"`
	Generation int                      `json:"generation"`
	Revision   uint64                   `json:"revision,omitempty"`
	Effective  string                   `json:"effective,omitempty"`
	Activation *RouteActivationOriginal `json:"route_activation,omitempty"`
}

func (s RouteAggregateStatus) Valid() bool {
	if s.Generation <= 0 || s.Activation != nil && (!s.Activation.Valid() || s.Activation.Generation != s.Generation) {
		return false
	}
	switch s.Ownership {
	case RouteAggregateNotOwner:
		return s.Revision == 0 && s.Effective == ""
	case RouteAggregateOwned:
		switch s.Effective {
		case "starting", "ready", "off", "error":
			return s.Revision > 0
		}
	}
	return false
}

// RouteActivationSelection binds one prepared V5 reconcile to the incumbent
// Supervisor and the next original creator of the exact saved descriptor.
type RouteActivationSelection struct {
	FromEpoch        MCPFrontRoutingTargetSnapshot `json:"from_epoch"`
	ToEpoch          MCPFrontRoutingTargetSnapshot `json:"to_epoch"`
	DescriptorSHA256 string                        `json:"descriptor_sha256"`
	Supervisor       SupervisorLockOwner           `json:"supervisor"`
	PriorGeneration  int                           `json:"prior_generation"`
}

func (s RouteActivationSelection) Valid() bool {
	return s.FromEpoch.State == MCPFrontRoutingTargetFront && s.ToEpoch.State == MCPFrontRoutingTargetFrontPreparing &&
		s.FromEpoch.Generation > 0 && s.FromEpoch.Generation < int(^uint(0)>>1) && s.ToEpoch.Generation == s.FromEpoch.Generation+1 &&
		s.FromEpoch.Port > 0 && s.FromEpoch.Port <= 65535 && s.ToEpoch.Port > 0 && s.ToEpoch.Port <= 65535 &&
		s.ToEpoch.Port != s.FromEpoch.Port && isLowerHex64(s.DescriptorSHA256) &&
		validRouteActivationBirth(s.Supervisor) && s.PriorGeneration > 0 && s.PriorGeneration < int(^uint(0)>>1)
}

// RouteActivationOriginal is supplied only by the Supervisor's operator
// status response. The closed child proof has no field for this receipt.
type RouteActivationOriginal struct {
	Selection  RouteActivationSelection `json:"selection"`
	Creator    SupervisorLockOwner      `json:"creator"`
	Generation int                      `json:"generation"`
}

func (r RouteActivationOriginal) Valid() bool {
	return r.Selection.Valid() && validRouteActivationBirth(r.Creator) && r.Creator.PID != r.Selection.Supervisor.PID &&
		r.Generation == r.Selection.PriorGeneration+1
}

func validRouteActivationBirth(owner SupervisorLockOwner) bool {
	started, err := time.Parse(time.RFC3339Nano, owner.StartedAt)
	return owner.PID > 0 && err == nil && !started.IsZero()
}

func (p RouteAggregateStatusProof) Status() RouteAggregateStatus {
	return RouteAggregateStatus{Ownership: p.Ownership, Generation: p.Generation, Revision: p.Revision, Effective: p.Effective}
}

const (
	RouteAggregateNotOwner       = "route-not-owner"
	RouteAggregateOwned          = "route-owned"
	RouteAggregateChallengeBytes = 16
)

type RouteAggregateControlAttached struct {
	Generation int `json:"generation"`
}

// ReadRouteAggregateStatusProof reads exactly one proof, leaving later bytes
// to the same connection reader. The limit follows the closed r2 wire shape.
func ReadRouteAggregateStatusProof(source io.Reader) (RouteAggregateStatusProof, error) {
	const fixedBytes = len(`{"id":9223372036854775807,"challenge":"","generation":,"ownership":"","revision":18446744073709551615,"effective":"starting"}`) + 1
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
	if observe == nil {
		return errors.New("route aggregate observer required")
	}
	return ServeRouteAggregateControl(ctx, launch, RouteAggregateControl{
		Status: func() RouteAggregateStatus { return RouteAggregateStatus{Ownership: observe()} },
	})
}

// RouteAggregateRetirement keeps physical settlement independent of the
// original operation error. Pending names the outstanding original resource.
type RouteAggregateRetirement struct {
	Pending string
	Err     error
}

func (r RouteAggregateRetirement) Settled() bool { return r.Pending == "" }

func (r RouteAggregateRetirement) Failure() error {
	if r.Settled() {
		return r.Err
	}
	return errors.Join(fmt.Errorf("retirement unresolved: %s", r.Pending), r.Err)
}

type RouteAggregateControl struct {
	Status func() RouteAggregateStatus
	Apply  func(operation, attached context.Context, challenge RouteAggregateStatusChallenge) RouteAggregateStatus
	Retire func(context.Context) RouteAggregateRetirement
}

type RouteAggregateControlExit struct {
	IPCError        error
	RevokeError     error
	Retirement      RouteAggregateRetirement
	RouteStopped    bool
	attachmentClose *routeAggregateAttachmentClose
}

func (e RouteAggregateControlExit) Failure() error {
	return errors.Join(e.IPCError, e.RevokeError, e.Retirement.Failure())
}

// AdvanceOriginalRetirement is called by the serialized Route stop owner after
// it joins the attachment worker. It receives only the original reader and
// advances the original attached control's retirement.
func (e *RouteAggregateControlExit) AdvanceOriginalRetirement(ctx context.Context) error {
	settlement := e.attachmentClose
	if e.Retirement.Settled() || settlement == nil || settlement.retireOriginal == nil {
		return nil
	}
	if err := settlement.joinReader(ctx); err != nil {
		return err
	}
	if !settlement.nativeObserved || settlement.nativeError != nil || settlement.observerError != nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	history := e.Retirement.Err
	e.Retirement = settlement.retireOriginal(ctx)
	if !errors.Is(e.Retirement.Err, history) {
		e.Retirement.Err = errors.Join(history, e.Retirement.Err)
	}
	return nil
}

func ServeRouteAggregateControl(ctx context.Context, launch RouteAggregateLaunch, control RouteAggregateControl) error {
	return ServeRouteAggregateControlObserved(ctx, launch, control).Failure()
}

// ServeRouteAggregateControlObserved receives the exact original transport and
// retirement outcomes for the Route owner that joins this attachment worker.
func ServeRouteAggregateControlObserved(ctx context.Context, launch RouteAggregateLaunch, control RouteAggregateControl) (out RouteAggregateControlExit) {
	if ctx == nil || control.Status == nil || !filepath.IsAbs(launch.StateDir) {
		out.IPCError = errors.New("route aggregate status: original launch and observer required")
		return out
	}
	if err := validateSupervisorControlPeerExpectation(launch.Owner, launch.Peer); err != nil {
		out.IPCError = err
		return out
	}
	attached, cause := serveRouteAggregateAttachment(ctx, launch, control, &out)
	if attached && ctx.Err() == nil && out.RevokeError == nil && out.Retirement.Settled() && aggregateRoutePeerClosed(cause) {
		history := out.Retirement.Err
		out = RouteAggregateControlExit{}
		_, cause = serveRouteAggregateAttachment(ctx, launch, control, &out)
		out.Retirement.Err = errors.Join(history, out.Retirement.Err)
	}
	out.IPCError = cause
	return out
}

const routeAggregateAttachmentTransport = "original Route attachment transport"

var errRouteAggregateNativeCloseUnobserved = errors.New("original Route attachment native close unobserved")

type routeAggregateAttachmentCloseReceipt interface{ NativeResult() (error, bool) }

type routeAggregateAttachmentClose struct {
	receipt        routeAggregateAttachmentCloseReceipt
	observerError  error
	nativeError    error
	nativeObserved bool
	readerDone     <-chan struct{}
	readerJoined   bool
	readerError    error
	retireOriginal func(context.Context) RouteAggregateRetirement
}

func (s *routeAggregateAttachmentClose) joinReader(ctx context.Context) error {
	if s.readerJoined {
		return nil
	}
	select {
	case <-s.readerDone:
	default:
		select {
		case <-s.readerDone:
		case <-ctx.Done():
			return errors.Join(ctx.Err(), errors.New("original Route attachment reader join pending"))
		}
	}
	s.readerJoined = true
	return nil
}

// finishRouteAggregateAttachment receives only the original connection's
// close and reader. The separate server binding still owns its own revoke.
func finishRouteAggregateAttachment(ctx context.Context, out *RouteAggregateControlExit, attached bool,
	receipt routeAggregateAttachmentCloseReceipt, observerErr error, readerDone <-chan struct{}, retire func(context.Context) RouteAggregateRetirement) {
	settlement := &routeAggregateAttachmentClose{receipt: receipt, observerError: observerErr, readerDone: readerDone, readerJoined: readerDone == nil}
	out.attachmentClose = settlement
	if attached {
		settlement.retireOriginal = retire
	}
	if receipt != nil {
		settlement.nativeError, settlement.nativeObserved = receipt.NativeResult()
	}
	settlement.readerError = settlement.joinReader(ctx)
	out.RevokeError = errors.Join(settlement.observerError, settlement.nativeError, settlement.readerError)
	if !settlement.nativeObserved {
		out.RevokeError = errors.Join(out.RevokeError, errRouteAggregateNativeCloseUnobserved)
	}
	if out.RevokeError != nil {
		out.Retirement.Pending = routeAggregateAttachmentTransport
		return
	}
	if settlement.retireOriginal != nil {
		out.Retirement = settlement.retireOriginal(ctx)
	}
}

func serveRouteAggregateAttachment(ctx context.Context, launch RouteAggregateLaunch, control RouteAggregateControl, out *RouteAggregateControlExit) (attached bool, retErr error) {
	openCtx, cancel := context.WithTimeout(ctx, SupervisorAggregateStatusTimeout)
	defer cancel()
	conn, err := dialRouteAggregateAttachment(openCtx, SupervisorIPCAddress(launch.StateDir))
	if err != nil {
		return false, err
	}
	var detachOnce sync.Once
	var detachDeadline time.Time
	var routeStopped bool
	beginDetach := func() time.Time {
		detachOnce.Do(func() {
			detachDeadline = time.Now().Add(SupervisorAggregateStatusTimeout)
			routeStopped = ctx.Err() != nil
		})
		return detachDeadline
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(closed)
		beginDetach()
		_ = conn.SetDeadline(time.Now())
	})
	var readerDone chan struct{}
	var endAttachment context.CancelCauseFunc
	defer func() {
		if !stop() {
			<-closed
		}
		deadline := beginDetach()
		if attached {
			out.RouteStopped = routeStopped
		}
		retireCtx, finish := context.WithDeadline(context.WithoutCancel(ctx), deadline)
		defer finish()
		if endAttachment != nil {
			endAttachment(nil)
			// Wake the sole old reader before closing and joining its transport.
			_ = conn.SetReadDeadline(time.Now())
		}
		receipt, closeErr := closeRouteAggregateAttachmentObserved(retireCtx, conn)
		finishRouteAggregateAttachment(retireCtx, out, attached, receipt, closeErr, readerDone, control.Retire)
	}()
	// Take ownership before receiving a cancellation that followed pipe acquisition.
	if err := openCtx.Err(); err != nil {
		return false, err
	}
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
	var attachedCtx context.Context
	attachedCtx, endAttachment = context.WithCancelCause(ctx)
	incoming := make(chan supervisorIPCRawResponse)
	readerDone = make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			frame, err := readSupervisorIPCResponse(conn)
			if err != nil {
				beginDetach()
				endAttachment(err)
				return
			}
			select {
			case incoming <- frame:
			case <-attachedCtx.Done():
				return
			}
		}
	}()
	for attachedCtx.Err() == nil {
		var frame supervisorIPCRawResponse
		select {
		case <-attachedCtx.Done():
			return true, context.Cause(attachedCtx)
		case frame = <-incoming:
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
		if err := attachedCtx.Err(); err != nil {
			return true, context.Cause(attachedCtx)
		}
		var status RouteAggregateStatus
		if challenge.Apply == nil {
			status = control.Status()
		} else {
			if !challenge.Apply.Valid() || control.Apply == nil || ValidateSupervisorControlPeerConnectionV1(conn, launch.Owner, launch.Peer) != nil {
				return true, errors.New("route aggregate apply authority unavailable")
			}
			operation, finish := context.WithTimeout(attachedCtx, SupervisorAggregateStatusTimeout)
			status = control.Apply(operation, attachedCtx, challenge)
			finish()
		}
		status.Generation = challenge.Generation
		if !status.Valid() {
			return true, errors.New("invalid route aggregate status")
		}
		proof := RouteAggregateStatusProof{
			ID: challenge.ID, Challenge: challenge.Challenge, Generation: challenge.Generation,
			Ownership: status.Ownership, Revision: status.Revision, Effective: status.Effective,
		}
		if err := conn.SetWriteDeadline(time.Now().Add(SupervisorAggregateStatusTimeout)); err != nil {
			return true, err
		}
		if err := attachedCtx.Err(); err != nil {
			return true, context.Cause(attachedCtx)
		}
		if err := json.NewEncoder(conn).Encode(proof); err != nil {
			return true, err
		}
	}
	return true, context.Cause(attachedCtx)
}

// QuerySupervisorAggregateStatus sends one query. The caller must hold the
// installed file and running-process observations until checkCurrent and the
// connection close complete. Only the current route supplies the result.
func QuerySupervisorAggregateStatus(ctx context.Context, stateDir string, owner SupervisorLockOwner, peer SupervisorProcessIdentityV1, checkCurrent func(context.Context) error) (RouteAggregateStatus, error) {
	return readSupervisorAggregate(ctx, stateDir, owner, peer, checkCurrent, IPCRequest{Cmd: SupervisorAggregateStatusCommand})
}

func ApplySupervisorAggregate(ctx context.Context, stateDir string, owner SupervisorLockOwner, peer SupervisorProcessIdentityV1, checkCurrent func(context.Context) error, apply RouteAggregateApply) (RouteAggregateStatus, error) {
	if !apply.Valid() {
		return RouteAggregateStatus{}, errors.New("invalid aggregate apply selection")
	}
	return readSupervisorAggregate(ctx, stateDir, owner, peer, checkCurrent, IPCRequest{
		Cmd:  SupervisorAggregateApplyCommand,
		Args: map[string]any{"receipt_sha256": apply.ReceiptSHA256, "endpoint_sha256": apply.EndpointSHA256},
	})
}

// VerifyRouteAggregateGeneration rechecks the pending apply's nonce and the
// native current Route generation at the existing Supervisor immediately
// before bind. It has no effect and cannot issue another apply.
func VerifyRouteAggregateGeneration(ctx context.Context, launch RouteAggregateLaunch, challenge RouteAggregateStatusChallenge) error {
	check := func(ctx context.Context) error {
		owner, err := ReadOccupiedSupervisorLockOwner(launch.StateDir)
		if err != nil || owner != launch.Owner {
			return ErrSupervisorControlPeerIdentity
		}
		return ctx.Err()
	}
	raw, err := exchangeSupervisorAggregate(ctx, launch.StateDir, launch.Owner, launch.Peer, check, IPCRequest{
		Cmd:  SupervisorAggregateVerifyCommand,
		Args: map[string]any{"generation": strconv.Itoa(challenge.Generation), "challenge": challenge.Challenge},
	})
	if err != nil {
		return err
	}
	var accepted RouteAggregateControlAttached
	if json.Unmarshal(raw, &accepted) != nil || accepted.Generation != challenge.Generation {
		return ErrSupervisorControlPeerIdentity
	}
	return nil
}

func readSupervisorAggregate(ctx context.Context, stateDir string, owner SupervisorLockOwner, peer SupervisorProcessIdentityV1, checkCurrent func(context.Context) error, req IPCRequest) (RouteAggregateStatus, error) {
	raw, err := exchangeSupervisorAggregate(ctx, stateDir, owner, peer, checkCurrent, req)
	if err != nil {
		return RouteAggregateStatus{}, err
	}
	var status RouteAggregateStatus
	if json.Unmarshal(raw, &status) != nil || !status.Valid() ||
		status.Activation != nil && status.Activation.Selection.Supervisor != owner {
		return RouteAggregateStatus{}, errors.New("OPERATOR_AGGREGATE_MALFORMED")
	}
	return status, nil
}

func exchangeSupervisorAggregate(ctx context.Context, stateDir string, owner SupervisorLockOwner, peer SupervisorProcessIdentityV1, checkCurrent func(context.Context) error, req IPCRequest) (status json.RawMessage, retErr error) {
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
			status = nil
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
	req.Version, req.ID = 1, time.Now().UnixNano()
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
	if resp.ID != req.ID || !resp.OK || !resp.Final || resp.Error != nil {
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
	return resp.Result, ctx.Err()
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

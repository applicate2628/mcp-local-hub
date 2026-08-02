package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const (
	// MCPFrontRoutingTargetSettingKey is the public, read-only enum setting.
	// Its generation sibling is private state owned by this file; both fields
	// are persisted atomically in the canonical settings map.
	MCPFrontRoutingTargetSettingKey = "mcp_front.routing_target"
	mcpFrontRoutingGenerationKey    = "mcp_front.routing_generation"
	mcpFrontRoutingAdmittedPortKey  = "mcp_front.routing_admitted_port"

	MCPFrontTransitionActiveCode   = "MCP_FRONT_TRANSITION_ACTIVE"
	MCPFrontTargetConflictCode     = "MCP_FRONT_TARGET_CONFLICT"
	MCPFrontTargetInvalidCode      = "MCP_FRONT_TARGET_INVALID"
	MCPFrontRoutingPortUnboundCode = "MCP_FRONT_ROUTING_PORT_UNBOUND"

	mcpFrontRoutingTargetSettledEvent = "mcp-front-routing-target-settled"
	mcpFrontRoutingLeaseRetryDelay    = 10 * time.Millisecond
)

// MCPFrontRoutingTarget is the durable state-machine value.
type MCPFrontRoutingTarget string

const (
	MCPFrontRoutingTargetGUI            MCPFrontRoutingTarget = "gui"
	MCPFrontRoutingTargetFrontPreparing MCPFrontRoutingTarget = "front-preparing"
	MCPFrontRoutingTargetFront          MCPFrontRoutingTarget = "front"
	MCPFrontRoutingTargetGUIRestoring   MCPFrontRoutingTarget = "gui-restoring"
)

// ClientRoutingTarget is the only target ordinary client-config writers may
// consume. Generation is zero only for the stable legacy-compatible GUI state.
type ClientRoutingTarget struct {
	Mode       MCPFrontRoutingTarget
	Port       int
	Generation int
}

// MCPFrontRoutingTargetSnapshot includes transitional states for the explicit
// reconcile transaction. Ordinary writers use ResolveClientRoutingTarget,
// which rejects those states.
type MCPFrontRoutingTargetSnapshot struct {
	State      MCPFrontRoutingTarget `json:"state"`
	Generation int                   `json:"generation"`
	Port       int                   `json:"admitted_port"`
}

var ErrMCPFrontTransitionActive = errors.New(MCPFrontTransitionActiveCode)
var ErrMCPFrontTargetConflict = errors.New(MCPFrontTargetConflictCode)
var ErrMCPFrontTargetInvalid = errors.New(MCPFrontTargetInvalidCode)
var ErrMCPFrontRoutingPortUnbound = errors.New(MCPFrontRoutingPortUnboundCode)

type MCPFrontTransitionActiveError struct {
	State      MCPFrontRoutingTarget
	Generation int
}

func (e *MCPFrontTransitionActiveError) Error() string {
	return fmt.Sprintf("%s: routing target is %s (journal generation %d); ordinary client-config writers are blocked", MCPFrontTransitionActiveCode, e.State, e.Generation)
}
func (e *MCPFrontTransitionActiveError) Unwrap() error { return ErrMCPFrontTransitionActive }

type MCPFrontTargetConflictError struct {
	Expected         MCPFrontRoutingTarget
	Actual           MCPFrontRoutingTarget
	Generation       int
	ActualGeneration int
	ExpectedPort     int
	ActualPort       int
}

func (e *MCPFrontTargetConflictError) Error() string {
	return fmt.Sprintf("%s: expected state=%s generation=%d admitted_port=%d, actual state=%s generation=%d admitted_port=%d", MCPFrontTargetConflictCode, e.Expected, e.Generation, e.ExpectedPort, e.Actual, e.ActualGeneration, e.ActualPort)
}
func (e *MCPFrontTargetConflictError) Unwrap() error { return ErrMCPFrontTargetConflict }

type MCPFrontRoutingPortUnboundError struct {
	State      MCPFrontRoutingTarget
	Generation int
}

func (e *MCPFrontRoutingPortUnboundError) Error() string {
	return fmt.Sprintf("%s: routing state=%s generation=%d has no durable admitted port", MCPFrontRoutingPortUnboundCode, e.State, e.Generation)
}
func (e *MCPFrontRoutingPortUnboundError) Unwrap() error { return ErrMCPFrontRoutingPortUnbound }

type MCPFrontTargetInvalidError struct {
	Detail string
	Cause  error
}

func (e *MCPFrontTargetInvalidError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", MCPFrontTargetInvalidCode, e.Detail, e.Cause)
	}
	return fmt.Sprintf("%s: %s", MCPFrontTargetInvalidCode, e.Detail)
}
func (e *MCPFrontTargetInvalidError) Unwrap() error {
	if e.Cause != nil {
		return errors.Join(ErrMCPFrontTargetInvalid, e.Cause)
	}
	return ErrMCPFrontTargetInvalid
}

func (a *API) ResolveClientRoutingTarget() (ClientRoutingTarget, error) {
	return a.ResolveClientRoutingTargetIn(SettingsPath())
}

type clientRoutingAuthorityKind uint8

const (
	clientRoutingAuthorityCanonical clientRoutingAuthorityKind = iota
	clientRoutingAuthorityExact
	clientRoutingAuthorityStableGUICompatibility
)

// ClientRoutingAuthorityRequest states only what durable routing authority an
// ordinary mutation requires. It deliberately carries no legacy endpoint:
// explicit ports and pidport discovery remain owned by their source-specific
// writer while this owner fences routing mode/generation for the whole
// mutation and readback lifetime.
type ClientRoutingAuthorityRequest struct {
	kind  clientRoutingAuthorityKind
	exact ClientRoutingTarget
}

func CanonicalAtAdmission() ClientRoutingAuthorityRequest {
	return ClientRoutingAuthorityRequest{kind: clientRoutingAuthorityCanonical}
}

func ExactTarget(target ClientRoutingTarget) ClientRoutingAuthorityRequest {
	return ClientRoutingAuthorityRequest{kind: clientRoutingAuthorityExact, exact: target}
}

func StableGUICompatibility() ClientRoutingAuthorityRequest {
	return ClientRoutingAuthorityRequest{kind: clientRoutingAuthorityStableGUICompatibility}
}

// WithClientRoutingAuthorityLease admits one ordinary client-config mutation.
// The shared cross-process lease spans canonical target resolution, the
// caller's complete mutation, and its durable readback. Routing transitions
// take the exclusive side of the same lease, so a transition cannot begin or
// settle while an admitted ordinary write is still capable of committing.
// The callback must not invoke a routing transition (shared-to-exclusive
// upgrade is deliberately unsupported).
func (a *API) WithClientRoutingAuthorityLease(
	ctx context.Context,
	request ClientRoutingAuthorityRequest,
	mutate func(ClientRoutingTarget) error,
) error {
	return a.WithClientRoutingAuthorityLeaseIn(ctx, SettingsPath(), request, mutate)
}

func (a *API) WithClientRoutingAuthorityLeaseIn(
	ctx context.Context,
	path string,
	request ClientRoutingAuthorityRequest,
	mutate func(ClientRoutingTarget) error,
) error {
	if mutate == nil {
		return &MCPFrontTargetInvalidError{Detail: "ordinary routing mutation callback is nil"}
	}
	return withMCPFrontRoutingFileLease(ctx, path, false, func() error {
		target, err := a.ResolveClientRoutingTargetIn(path)
		if err != nil {
			return err
		}
		switch request.kind {
		case clientRoutingAuthorityCanonical:
		case clientRoutingAuthorityExact:
			if err := ValidateClientRoutingTarget(request.exact); err != nil {
				return err
			}
			if request.exact != target {
				return fmt.Errorf("%w: requested target %+v no longer matches canonical target %+v", ErrMCPFrontTargetConflict, request.exact, target)
			}
		case clientRoutingAuthorityStableGUICompatibility:
			if target.Mode != MCPFrontRoutingTargetGUI || target.Generation != 0 {
				return &MCPFrontTargetConflictError{
					Expected: MCPFrontRoutingTargetGUI, Actual: target.Mode,
					ActualGeneration: target.Generation,
				}
			}
		default:
			return &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("unknown client routing authority kind %d", request.kind)}
		}
		return mutate(target)
	})
}

func withMCPFrontRoutingFileLease(ctx context.Context, settingsPath string, exclusive bool, work func() error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		return fmt.Errorf("mkdir routing lease dir: %w", err)
	}
	leasePath := settingsPath + ".routing-target.lock"
	lease := flock.New(leasePath, flock.SetPermissions(0o600))
	locked := false
	if exclusive {
		locked, err = lease.TryLockContext(ctx, mcpFrontRoutingLeaseRetryDelay)
	} else {
		locked, err = lease.TryRLockContext(ctx, mcpFrontRoutingLeaseRetryDelay)
	}
	if err != nil {
		_ = lease.Close()
		return fmt.Errorf("routing target lease %s: %w", leasePath, err)
	}
	if !locked {
		_ = lease.Close()
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("routing target lease %s was not acquired", leasePath)
	}
	defer func() {
		err = errors.Join(err, lease.Unlock(), lease.Close())
	}()
	if work == nil {
		return nil
	}
	return work()
}

// ResolveClientRoutingTargetIn reads one raw settings snapshot. Missing target
// state is stable GUI for pre-PR compatibility; corrupt or transitional state
// fails closed and never substitutes either port.
func (a *API) ResolveClientRoutingTargetIn(path string) (ClientRoutingTarget, error) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	raw, err := readRawSettingsMap(path)
	if err != nil {
		return ClientRoutingTarget{}, &MCPFrontTargetInvalidError{Detail: "read routing settings", Cause: err}
	}
	snapshot, err := mcpFrontRoutingSnapshotFromRaw(raw)
	if err != nil {
		return ClientRoutingTarget{}, err
	}
	switch snapshot.State {
	case MCPFrontRoutingTargetGUI:
		port, portErr := routingPortFromRaw(raw, "gui_server.port")
		if portErr != nil {
			return ClientRoutingTarget{}, portErr
		}
		return ClientRoutingTarget{Mode: snapshot.State, Port: port}, nil
	case MCPFrontRoutingTargetFront:
		return ClientRoutingTarget{Mode: snapshot.State, Port: snapshot.Port, Generation: snapshot.Generation}, nil
	case MCPFrontRoutingTargetFrontPreparing, MCPFrontRoutingTargetGUIRestoring:
		return ClientRoutingTarget{}, &MCPFrontTransitionActiveError{State: snapshot.State, Generation: snapshot.Generation}
	default:
		return ClientRoutingTarget{}, &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("unknown routing state %q", snapshot.State)}
	}
}

func (a *API) MCPFrontRoutingTargetSnapshot() (MCPFrontRoutingTargetSnapshot, error) {
	return a.MCPFrontRoutingTargetSnapshotIn(SettingsPath())
}

func (a *API) MCPFrontRoutingTargetSnapshotIn(path string) (MCPFrontRoutingTargetSnapshot, error) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	raw, err := readRawSettingsMap(path)
	if err != nil {
		return MCPFrontRoutingTargetSnapshot{}, &MCPFrontTargetInvalidError{Detail: "read routing settings", Cause: err}
	}
	return mcpFrontRoutingSnapshotFromRaw(raw)
}

// MCPFrontRoutingTargetSnapshotForMigration exposes an unbound early-D2 epoch
// only to the CLI migration owner. Ordinary routing resolution and the normal
// snapshot remain strict and return MCP_FRONT_ROUTING_PORT_UNBOUND.
func (a *API) MCPFrontRoutingTargetSnapshotForMigration() (MCPFrontRoutingTargetSnapshot, error) {
	return a.MCPFrontRoutingTargetSnapshotForMigrationIn(SettingsPath())
}

func (a *API) MCPFrontRoutingTargetSnapshotForMigrationIn(path string) (MCPFrontRoutingTargetSnapshot, error) {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	raw, err := readRawSettingsMap(path)
	if err != nil {
		return MCPFrontRoutingTargetSnapshot{}, &MCPFrontTargetInvalidError{Detail: "read routing settings", Cause: err}
	}
	return mcpFrontRoutingEpochFromRaw(raw, true)
}

func routingPortFromRaw(raw map[string]string, key string) (int, error) {
	def := findDef(key)
	if def == nil {
		return 0, &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("routing port setting %q is not registered", key)}
	}
	value := def.Default
	if persisted, ok := raw[key]; ok {
		value = persisted
	}
	if err := validate(def, value); err != nil {
		return 0, &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("routing port %s=%q is invalid", key, value), Cause: err}
	}
	port, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || port <= 0 || port > 65535 {
		return 0, &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("routing port %s=%q is invalid", key, value), Cause: err}
	}
	return port, nil
}

func mcpFrontRoutingSnapshotFromRaw(raw map[string]string) (MCPFrontRoutingTargetSnapshot, error) {
	return mcpFrontRoutingEpochFromRaw(raw, false)
}

func mcpFrontRoutingEpochFromRaw(raw map[string]string, allowUnbound bool) (MCPFrontRoutingTargetSnapshot, error) {
	value, present := raw[MCPFrontRoutingTargetSettingKey]
	if !present {
		if generation := strings.TrimSpace(raw[mcpFrontRoutingGenerationKey]); generation != "" && generation != "0" {
			return MCPFrontRoutingTargetSnapshot{}, &MCPFrontTargetInvalidError{Detail: "routing generation exists without routing target"}
		}
		if port := strings.TrimSpace(raw[mcpFrontRoutingAdmittedPortKey]); port != "" && port != "0" {
			return MCPFrontRoutingTargetSnapshot{}, &MCPFrontTargetInvalidError{Detail: "routing admitted port exists without routing target"}
		}
		return MCPFrontRoutingTargetSnapshot{State: MCPFrontRoutingTargetGUI}, nil
	}
	state := MCPFrontRoutingTarget(strings.TrimSpace(value))
	switch state {
	case MCPFrontRoutingTargetGUI:
		if generation := strings.TrimSpace(raw[mcpFrontRoutingGenerationKey]); generation != "" && generation != "0" {
			return MCPFrontRoutingTargetSnapshot{}, &MCPFrontTargetInvalidError{Detail: "stable gui state carries a journal generation"}
		}
		if port := strings.TrimSpace(raw[mcpFrontRoutingAdmittedPortKey]); port != "" && port != "0" {
			return MCPFrontRoutingTargetSnapshot{}, &MCPFrontTargetInvalidError{Detail: "stable gui state carries an admitted routing port"}
		}
		return MCPFrontRoutingTargetSnapshot{State: state}, nil
	case MCPFrontRoutingTargetFrontPreparing, MCPFrontRoutingTargetFront, MCPFrontRoutingTargetGUIRestoring:
		generation, err := strconv.Atoi(strings.TrimSpace(raw[mcpFrontRoutingGenerationKey]))
		if err != nil || generation <= 0 {
			return MCPFrontRoutingTargetSnapshot{}, &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("state %s has invalid journal generation %q", state, raw[mcpFrontRoutingGenerationKey]), Cause: err}
		}
		portValue := strings.TrimSpace(raw[mcpFrontRoutingAdmittedPortKey])
		if portValue == "" || portValue == "0" {
			if allowUnbound {
				return MCPFrontRoutingTargetSnapshot{State: state, Generation: generation}, nil
			}
			return MCPFrontRoutingTargetSnapshot{}, &MCPFrontRoutingPortUnboundError{State: state, Generation: generation}
		}
		port, portErr := strconv.Atoi(portValue)
		if portErr != nil || port <= 0 || port > 65535 {
			return MCPFrontRoutingTargetSnapshot{}, &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("state %s has invalid admitted routing port %q", state, portValue), Cause: portErr}
		}
		return MCPFrontRoutingTargetSnapshot{State: state, Generation: generation, Port: port}, nil
	default:
		return MCPFrontRoutingTargetSnapshot{}, &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("invalid persisted routing target %q", value)}
	}
}

func (a *API) TransitionMCPFrontRoutingEpoch(expected, next MCPFrontRoutingTargetSnapshot) error {
	return a.TransitionMCPFrontRoutingEpochContext(context.Background(), expected, next)
}

func (a *API) TransitionMCPFrontRoutingEpochContext(ctx context.Context, expected, next MCPFrontRoutingTargetSnapshot) error {
	return a.TransitionMCPFrontRoutingEpochInContext(ctx, SettingsPath(), expected, next)
}

func (a *API) TransitionMCPFrontRoutingEpochIn(path string, expected, next MCPFrontRoutingTargetSnapshot) error {
	return a.TransitionMCPFrontRoutingEpochInContext(context.Background(), path, expected, next)
}

// TransitionMCPFrontRoutingEpochInContext is the sole routing storage writer.
// It performs one exact epoch compare-and-set inside the existing exclusive
// operation-lifetime lease and settings lock, then verifies the persisted epoch
// before publishing the one committed event.
func (a *API) TransitionMCPFrontRoutingEpochInContext(ctx context.Context, path string, expected, next MCPFrontRoutingTargetSnapshot) error {
	if err := validateMCPFrontRoutingEpoch(expected, true); err != nil {
		return fmt.Errorf("expected routing epoch: %w", err)
	}
	if err := validateMCPFrontRoutingEpoch(next, false); err != nil {
		return fmt.Errorf("next routing epoch: %w", err)
	}
	if !validMCPFrontRoutingEpochTransition(expected, next) {
		return &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("routing epoch transition %+v -> %+v is not allowed", expected, next)}
	}
	return withMCPFrontRoutingFileLease(ctx, path, true, func() error {
		return mutateRawSettingsMapLockedThen(path, func(raw map[string]string) error {
			actual, err := mcpFrontRoutingEpochFromRaw(raw, true)
			if err != nil {
				return err
			}
			if actual != expected {
				return &MCPFrontTargetConflictError{
					Expected: expected.State, Actual: actual.State,
					Generation: expected.Generation, ActualGeneration: actual.Generation,
					ExpectedPort: expected.Port, ActualPort: actual.Port,
				}
			}
			raw[MCPFrontRoutingTargetSettingKey] = string(next.State)
			if next.State == MCPFrontRoutingTargetGUI {
				delete(raw, mcpFrontRoutingGenerationKey)
				delete(raw, mcpFrontRoutingAdmittedPortKey)
			} else {
				raw[mcpFrontRoutingGenerationKey] = strconv.Itoa(next.Generation)
				raw[mcpFrontRoutingAdmittedPortKey] = strconv.Itoa(next.Port)
			}
			return nil
		}, func(persisted map[string]string) error {
			settled, err := mcpFrontRoutingSnapshotFromRaw(persisted)
			if err != nil {
				return err
			}
			if settled != next {
				return &MCPFrontTargetConflictError{
					Expected: next.State, Actual: settled.State,
					Generation: next.Generation, ActualGeneration: settled.Generation,
					ExpectedPort: next.Port, ActualPort: settled.Port,
				}
			}
			_ = LogHubMcpEvent("info", mcpFrontRoutingTargetSettledEvent, map[string]any{
				"old_state": expected.State, "new_state": next.State,
				"old_generation": expected.Generation, "new_generation": next.Generation,
				"admitted_port": next.Port,
			})
			return nil
		})
	})
}

func validateMCPFrontRoutingEpoch(epoch MCPFrontRoutingTargetSnapshot, allowUnbound bool) error {
	if epoch.State == MCPFrontRoutingTargetGUI {
		if epoch.Generation != 0 || epoch.Port != 0 {
			return &MCPFrontTargetInvalidError{Detail: "gui routing epoch must have generation 0 and admitted port 0"}
		}
		return nil
	}
	switch epoch.State {
	case MCPFrontRoutingTargetFrontPreparing, MCPFrontRoutingTargetFront, MCPFrontRoutingTargetGUIRestoring:
	default:
		return &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("unknown routing epoch state %q", epoch.State)}
	}
	if epoch.Generation <= 0 {
		return &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("routing epoch %s requires a positive generation", epoch.State)}
	}
	if epoch.Port == 0 && allowUnbound {
		return nil
	}
	if epoch.Port <= 0 || epoch.Port > 65535 {
		return &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("routing epoch admitted port %d is outside 1..65535", epoch.Port)}
	}
	return nil
}

func validMCPFrontRoutingEpochTransition(expected, next MCPFrontRoutingTargetSnapshot) bool {
	if expected.State == next.State && expected.Generation == next.Generation && expected.Port == 0 && next.Port > 0 && expected.State != MCPFrontRoutingTargetGUI {
		return true // early-D2 admitted-port binding
	}
	if (expected.State == MCPFrontRoutingTargetFrontPreparing || expected.State == MCPFrontRoutingTargetFront) &&
		next.State == MCPFrontRoutingTargetFrontPreparing && expected.Port > 0 && next.Port > 0 &&
		next.Port != expected.Port && expected.Generation < int(^uint(0)>>1) && next.Generation == expected.Generation+1 {
		return true // exact N+1 generation rebase
	}
	sameEpoch := expected.Generation == next.Generation && expected.Port == next.Port
	switch expected.State {
	case MCPFrontRoutingTargetGUI:
		return (next.State == MCPFrontRoutingTargetFrontPreparing || next.State == MCPFrontRoutingTargetGUIRestoring) && next.Generation > 0 && next.Port > 0
	case MCPFrontRoutingTargetFrontPreparing:
		return sameEpoch && (next.State == MCPFrontRoutingTargetFront || next.State == MCPFrontRoutingTargetGUIRestoring)
	case MCPFrontRoutingTargetFront:
		return sameEpoch && next.State == MCPFrontRoutingTargetGUIRestoring
	case MCPFrontRoutingTargetGUIRestoring:
		return next.State == MCPFrontRoutingTargetGUI
	default:
		return false
	}
}

// ValidateClientRoutingTarget validates an already-frozen target passed by a
// composition root or the journaled front transaction.
func ValidateClientRoutingTarget(target ClientRoutingTarget) error {
	if target.Port <= 0 || target.Port > 65535 {
		return &MCPFrontTargetInvalidError{Detail: fmt.Sprintf("routing target port %d is outside 1..65535", target.Port)}
	}
	switch target.Mode {
	case MCPFrontRoutingTargetGUI:
		if target.Generation != 0 {
			return &MCPFrontTargetInvalidError{Detail: "gui routing target must not carry a generation"}
		}
	case MCPFrontRoutingTargetFront:
		if target.Generation <= 0 {
			return &MCPFrontTargetInvalidError{Detail: "front routing target requires a positive generation"}
		}
	default:
		return &MCPFrontTransitionActiveError{State: target.Mode, Generation: target.Generation}
	}
	return nil
}

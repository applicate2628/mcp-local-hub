//go:build windows

// Package cli — Windows-only production wiring for the `mcphub install
// --upgrade` cold-restart flow (v0.5.x → v0.5.x rename-aside + IPC handoff).
//
// The cross-platform dispatcher has one optional managed-transaction adapter.
// This file installs the Windows implementation; unsupported platforms keep it
// nil and fail closed before mutation.
//
// v0.6 Phase F NOTE: the v0.4.x→v0.5.0 forward-migration engine and the
// `mcphub install --rollback-to-legacy` demotion path were deleted in Phase F
// (the internal/migration package is gone). This file used to also wire
// forwardMigrationFn / rollbackMigrationFn / enumerateAllMcphubTasksFn into the
// migration package; those are removed. Only the cold-restart upgrade wiring
// (RunInstallUpgrade) remains.
package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/scheduler"
)

func init() {
	v5UpgradeFn = runV5UpgradeWindows
}

type upgradeGUIFencedDeps struct {
	Classify         func(string, string, string) (upgradeGUIProductOutcome, error)
	PublishCurrent   func(string, string, string, WindowsProductPairMode) (bool, error)
	OnNewTransaction func() error
	ExitAndReserve   func(context.Context, string, string) (gui.OwnedSingleInstanceLease, error)
	ObservePair      func(context.Context, string, string) (*upgradeGUIHeldTopology, error)
	ExitPair         func(context.Context, *upgradeGUIHeldTopology, *WindowsProductTxn) (gui.OwnedSingleInstanceLease, error)
	ObserveAfterPair func(context.Context, string, string, []windowsProductIntentPortV1) (windowsProductQuiescenceObservationV1, error)
	RecoverPriorPair func(windowsProductRecoveryJournalV3) error
	AfterCommitPair  func(windowsProductRecoveryJournalV3, func() error) error
}

type upgradeGUIProcessIdentity struct {
	PID       int
	Role      string
	Image     string
	StartedAt time.Time
	ParentPID int
	SameUser  bool
}

type upgradeGUIIdentitySnapshot struct {
	Target             string
	GUI                upgradeGUIProcessIdentity
	Tray               upgradeGUIProcessIdentity
	Supervisor         upgradeGUIProcessIdentity
	SupervisorInAnyJob bool
}

type upgradeGUIHeldTopology struct {
	inputs          upgradeGUIObservationInputs
	StateDir        string
	Identity        upgradeGUIIdentitySnapshot
	PidportPath     string
	OwnerRecord     gui.GUIOwnerRecord
	SupervisorOwner api.SupervisorLockOwner
	TaskActions     []upgradeGUITaskAction
	IntentHash      [32]byte
	FrozenPorts     []windowsProductIntentPortV1
	PortOwners      []upgradeGUIPortOwner
	GUI             process.HeldPIDGeneration
	Tray            process.HeldPIDGeneration
	Supervisor      process.HeldPIDJobObservation
}

// upgradeGUIObservationInputs binds only the impure observation leaves of the
// existing admission path. A complete value is required for every invocation;
// a missing test leaf can never silently fall through to the live OS.
type upgradeGUIObservationInputs struct {
	OwnerPath        func() (string, error)
	VerifyListener   func(context.Context, string, int, int) error
	ProbeLeaseUnheld func(string) (bool, error)
	FindGUIs         func(string) ([]api.ProcessInfo, error)
	ListProcesses    func([]string) ([]api.ProcessInfo, error)
	HoldTermination  func(int) (process.HeldPIDGeneration, error)
	HoldJob          func(int) (process.HeldPIDJobObservation, error)
	CurrentIdentity  func(int) (process.PIDHandleIdentity, error)
	CommandIdentity  func(context.Context, int) (process.ProcessIdentity, error)
	SIDMatches       func(int) (bool, error)
	ParentPID        func(int) (int, error)
	SupervisorOwner  func(string) (api.SupervisorLockOwner, error)
	TaskActions      func(context.Context, string) ([]upgradeGUITaskAction, error)
	PortOwners       func(context.Context, string, string, int) ([32]byte, []windowsProductIntentPortV1, []upgradeGUIPortOwner, error)
	PendingRuntime   *pendingRuntimeReceiving
}

func (in upgradeGUIObservationInputs) complete() bool {
	return in.OwnerPath != nil && in.VerifyListener != nil && in.ProbeLeaseUnheld != nil &&
		in.FindGUIs != nil && in.ListProcesses != nil && in.HoldTermination != nil && in.HoldJob != nil &&
		in.CurrentIdentity != nil && in.CommandIdentity != nil && in.SIDMatches != nil && in.ParentPID != nil &&
		in.SupervisorOwner != nil && in.TaskActions != nil && in.PortOwners != nil
}

func productionUpgradeGUIObservationInputs() upgradeGUIObservationInputs {
	return upgradeGUIObservationInputs{
		OwnerPath:        resolveUpgradeGUIOwnerPath,
		VerifyListener:   gui.VerifyGUIOwnerListener,
		ProbeLeaseUnheld: gui.ProbeSingleInstanceLockUnheld,
		FindGUIs:         func(target string) ([]api.ProcessInfo, error) { return findRunningGUIsOnTarget(api.NewAPI(), target) },
		ListProcesses: func(patterns []string) ([]api.ProcessInfo, error) {
			return api.NewAPI().ListMatchingProcesses(patterns)
		},
		HoldTermination: process.HoldPIDForTermination,
		HoldJob:         process.HoldPIDForJobObservation,
		CurrentIdentity: process.LookupPIDHandleIdentity,
		CommandIdentity: process.LookupProcessIdentityContext,
		SIDMatches:      process.ProcessOwnerSIDMatchesCurrent,
		ParentPID:       retainedGenerationParentPID,
		SupervisorOwner: api.ReadSupervisorLockOwner,
		TaskActions:     captureUpgradeGUITaskActions,
		PortOwners:      captureUpgradeGUIPortOwners,
	}
}

type upgradeGUITaskAction struct {
	Name, Command string
	XMLSHA256     [32]byte
}
type upgradeGUIPortOwner struct {
	TaskName             string
	Port, PID, ParentPID int
	StartedAt            time.Time
	ArgvSHA256           string
}

func captureUpgradeGUITaskActions(ctx context.Context, target string) ([]upgradeGUITaskAction, error) {
	refs, err := scheduler.ScanOwnedEntrypointTaskReferences(ctx, scheduler.WindowsLegacyEntrypointPath(target), target)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, errors.New("no exact-owned Product task actions were found")
	}
	sch, err := scheduler.New()
	if err != nil {
		return nil, err
	}
	rows := make([]upgradeGUITaskAction, 0, len(refs))
	for _, ref := range refs {
		raw, err := sch.ExportXML(ref.TaskName)
		if err != nil {
			return nil, err
		}
		rows = append(rows, upgradeGUITaskAction{Name: ref.TaskName, Command: ref.Command, XMLSHA256: sha256.Sum256(raw)})
	}
	return rows, nil
}

func captureUpgradeGUIPortOwners(ctx context.Context, stateDir, target string, supervisorPID int) ([32]byte, []windowsProductIntentPortV1, []upgradeGUIPortOwner, error) {
	return captureUpgradeGUIPortOwnersWithInputs(ctx, stateDir, target, supervisorPID, productionUpgradeGUIObservationInputs(), api.LoopbackPortOwnerPIDContext, process.LookupProcessIdentitiesContext)
}

func captureUpgradeGUIPortOwnersWithInputs(ctx context.Context, stateDir, target string, supervisorPID int, in upgradeGUIObservationInputs, portOwner func(context.Context, int) (int, bool, error), commandIdentities func(context.Context, []int) (map[int]process.ProcessIdentity, error)) ([32]byte, []windowsProductIntentPortV1, []upgradeGUIPortOwner, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return [32]byte{}, nil, nil, err
	}
	if in.CurrentIdentity == nil || in.ParentPID == nil || in.SIDMatches == nil || portOwner == nil || commandIdentities == nil {
		return [32]byte{}, nil, nil, errors.New("managed capture observation inputs unavailable")
	}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil || intent == nil || len(intent.Daemons) == 0 {
		return [32]byte{}, nil, nil, fmt.Errorf("daemon-bearing supervisor intent unavailable: %v", err)
	}
	raw, err := api.ReadStateFileInodeAnchored(intentPath)
	if err != nil {
		return [32]byte{}, nil, nil, err
	}
	frozen := make([]windowsProductIntentPortV1, 0, len(intent.Daemons))
	for _, daemon := range intent.Daemons {
		if port, ok := api.EffectiveDaemonPort(daemon); ok && port > 0 {
			frozen = append(frozen, windowsProductIntentPortV1{TaskName: daemon.TaskName, Port: port, ArgvSHA256: windowsProductArgvSHA256(append([]string{daemon.Command}, daemon.Args...))})
		}
	}
	return captureUpgradeGUIPortOwnersFromSnapshot(ctx, target, supervisorPID, sha256.Sum256(raw), frozen, in, portOwner, commandIdentities)
}

func captureUpgradeGUIPortOwnersFromSnapshot(ctx context.Context, target string, supervisorPID int, intentHash [32]byte, frozen []windowsProductIntentPortV1, in upgradeGUIObservationInputs, portOwner func(context.Context, int) (int, bool, error), commandIdentities func(context.Context, []int) (map[int]process.ProcessIdentity, error)) ([32]byte, []windowsProductIntentPortV1, []upgradeGUIPortOwner, error) {
	observed, anchors, err := observeWindowsProductIntentPortsWithInputs(ctx, target, frozen, false, portOwner, in.CurrentIdentity)
	if err != nil {
		return [32]byte{}, nil, nil, err
	}
	pids := make([]int, 0, len(observed))
	seen := make(map[int]struct{}, len(observed))
	for _, row := range observed {
		if row.PrePID > 0 {
			if _, exists := seen[row.PrePID]; !exists {
				seen[row.PrePID] = struct{}{}
				pids = append(pids, row.PrePID)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return [32]byte{}, nil, nil, err
	}
	identities, err := commandIdentities(ctx, pids)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return [32]byte{}, nil, nil, errors.Join(err, ctxErr)
	}
	if err != nil {
		return [32]byte{}, nil, nil, err
	}
	owners := make([]upgradeGUIPortOwner, 0, len(observed))
	for i, row := range observed {
		owner := upgradeGUIPortOwner{TaskName: frozen[i].TaskName, Port: frozen[i].Port, PID: row.PrePID}
		if row.PrePID > 0 {
			if err := ctx.Err(); err != nil {
				return [32]byte{}, nil, nil, err
			}
			identity, present := identities[row.PrePID]
			if !present {
				return [32]byte{}, nil, nil, fmt.Errorf("managed port %d owner absent: %w", owner.Port, process.ErrProcessNotFound)
			}
			owner.ArgvSHA256 = windowsProductArgvSHA256(process.TokenizeWindowsCommandLine(identity.CommandLine))
			if owner.ArgvSHA256 != frozen[i].ArgvSHA256 {
				return [32]byte{}, nil, nil, fmt.Errorf("foreign listener command on managed port %d", owner.Port)
			}
			if err := ctx.Err(); err != nil {
				return [32]byte{}, nil, nil, err
			}
			handleID, err := in.CurrentIdentity(row.PrePID)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return [32]byte{}, nil, nil, errors.Join(err, ctxErr)
			}
			if err != nil {
				return [32]byte{}, nil, nil, fmt.Errorf("managed port %d generation unverifiable: %w", owner.Port, err)
			}
			if handleID.PID != row.PrePID || handleID.StartedAt.IsZero() || !samePath(handleID.ExecutablePath, target) || !samePath(handleID.ExecutablePath, anchors[i].ExecutablePath) || !handleID.StartedAt.Equal(anchors[i].StartedAt) || identity.CreationDateUnix != handleID.StartedAt.Unix() {
				return [32]byte{}, nil, nil, fmt.Errorf("managed port %d generation unverifiable", owner.Port)
			}
			owner.StartedAt = handleID.StartedAt
			if err := ctx.Err(); err != nil {
				return [32]byte{}, nil, nil, err
			}
			owner.ParentPID, err = in.ParentPID(row.PrePID)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return [32]byte{}, nil, nil, errors.Join(err, ctxErr)
			}
			if err != nil {
				return [32]byte{}, nil, nil, fmt.Errorf("managed port %d parent differs: %w", owner.Port, err)
			}
			if owner.ParentPID != supervisorPID {
				return [32]byte{}, nil, nil, fmt.Errorf("managed port %d parent differs", owner.Port)
			}
			if err := ctx.Err(); err != nil {
				return [32]byte{}, nil, nil, err
			}
			sameUser, err := in.SIDMatches(row.PrePID)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return [32]byte{}, nil, nil, errors.Join(err, ctxErr)
			}
			if err != nil {
				return [32]byte{}, nil, nil, fmt.Errorf("managed port %d user identity differs: %w", owner.Port, err)
			}
			if !sameUser {
				return [32]byte{}, nil, nil, fmt.Errorf("managed port %d user identity differs", owner.Port)
			}
		}
		owners = append(owners, owner)
	}
	if err := ctx.Err(); err != nil {
		return [32]byte{}, nil, nil, err
	}
	return intentHash, frozen, owners, nil
}

func verifyUpgradeGUIHeldIdentity(held process.HeldPIDIdentity, pid int, target string, startedAt time.Time, lookupCurrent func(int) (process.PIDHandleIdentity, error)) (process.PIDHandleIdentity, error) {
	handleID, err := held.Identity()
	if pendingRuntimeStopsRecovery(err) {
		return process.PIDHandleIdentity{}, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: held identity cleanup unconfirmed: %w", err)
	}
	if err != nil || handleID.PID != pid || !samePath(handleID.ExecutablePath, target) || (!startedAt.IsZero() && !handleID.StartedAt.Equal(startedAt.UTC())) {
		return process.PIDHandleIdentity{}, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: held process generation differs from recorded identity: %v", err)
	}
	current, err := lookupCurrent(pid)
	if pendingRuntimeStopsRecovery(err) {
		return process.PIDHandleIdentity{}, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: current identity cleanup unconfirmed: %w", err)
	}
	if err != nil || current.PID != handleID.PID || !samePath(current.ExecutablePath, handleID.ExecutablePath) || !current.StartedAt.Equal(handleID.StartedAt) {
		return process.PIDHandleIdentity{}, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: PID differs from held generation: %v", err)
	}
	return handleID, nil
}

func heldUpgradeGUIIdentity(held any, pid int, target string, startedAt time.Time, lookupCurrent func(int) (process.PIDHandleIdentity, error)) (process.PIDHandleIdentity, error) {
	reader, ok := held.(process.HeldPIDIdentity)
	if !ok {
		return process.PIDHandleIdentity{}, errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: process hold lacks exact handle identity")
	}
	return verifyUpgradeGUIHeldIdentity(reader, pid, target, startedAt, lookupCurrent)
}

func holdUpgradeGUIRole(ctx context.Context, pid int, target string, startedAt time.Time, role string, in upgradeGUIObservationInputs) (result upgradeGUIProcessIdentity, held process.HeldPIDGeneration, retErr error) {
	if err := ctx.Err(); err != nil {
		return result, nil, err
	}
	held, err := in.HoldTermination(pid)
	if err != nil {
		if held != nil && in.PendingRuntime != nil {
			err = pendingRuntimeCloseError(err, in.PendingRuntime, held.Close())
		}
		return result, nil, err
	}
	if held == nil {
		return result, nil, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: %s hold returned no process generation", role)
	}
	owned := held
	defer func() {
		if retErr != nil {
			closeErr := owned.Close()
			if in.PendingRuntime != nil {
				retErr = pendingRuntimeCloseError(retErr, in.PendingRuntime, closeErr)
			} else {
				retErr = errors.Join(retErr, closeErr)
			}
			held = nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, nil, err
	}
	handleID, err := heldUpgradeGUIIdentity(held, pid, target, startedAt, in.CurrentIdentity)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, nil, fmt.Errorf("%s kernel image identity: %w", role, errors.Join(err, ctxErr))
	}
	if err != nil {
		return result, nil, fmt.Errorf("%s kernel image identity: %w", role, err)
	}
	observed, err := in.CommandIdentity(ctx, pid)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, nil, fmt.Errorf("%s command generation differs: %w", role, errors.Join(err, ctxErr))
	}
	if err != nil {
		return result, nil, fmt.Errorf("%s command generation differs: %w", role, err)
	}
	if !samePath(observed.ExecutablePath, target) || observed.CreationDateUnix != handleID.StartedAt.Unix() {
		return result, nil, fmt.Errorf("%s command generation differs", role)
	}
	actualRole := supervisorCommandLineSubcommand(observed.CommandLine)
	if role == "gui" {
		if !cmdlineIsGUIOnTarget(observed.CommandLine, target) {
			return result, nil, errors.New("GUI command role differs")
		}
	} else if actualRole != role {
		return result, nil, fmt.Errorf("%s command role differs", role)
	}
	sameUser, err := in.SIDMatches(pid)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return result, nil, fmt.Errorf("%s user identity differs: %w", role, errors.Join(err, ctxErr))
	}
	if err != nil || !sameUser {
		return result, nil, fmt.Errorf("%s user identity differs: %v", role, err)
	}
	if _, err := heldUpgradeGUIIdentity(held, pid, target, handleID.StartedAt, in.CurrentIdentity); err != nil {
		return result, nil, err
	}
	if err := ctx.Err(); err != nil {
		return result, nil, err
	}
	return upgradeGUIProcessIdentity{PID: pid, Role: actualRole, Image: handleID.ExecutablePath, StartedAt: handleID.StartedAt, SameUser: true}, held, nil
}

func observeUpgradeGUIHeldTopology(ctx context.Context, stateDir, target string) (result *upgradeGUIHeldTopology, retErr error) {
	return observeUpgradeGUIHeldTopologyWithInputs(ctx, stateDir, target, productionUpgradeGUIObservationInputs())
}

func observeUpgradeGUIHeldTopologyWithInputs(ctx context.Context, stateDir, target string, in upgradeGUIObservationInputs) (result *upgradeGUIHeldTopology, retErr error) {
	return observeUpgradeGUIHeldTopologyWithInputsMode(ctx, stateDir, target, in, false)
}

func observeUpgradeGUIOwnedPairHeldTopologyWithInputs(ctx context.Context, stateDir, target string, in upgradeGUIObservationInputs) (*upgradeGUIHeldTopology, error) {
	return observeUpgradeGUIHeldTopologyWithInputsMode(ctx, stateDir, target, in, true)
}

func observeUpgradeGUIHeldTopologyWithInputsMode(ctx context.Context, stateDir, target string, in upgradeGUIObservationInputs, pair bool) (result *upgradeGUIHeldTopology, retErr error) {
	if !in.complete() {
		return nil, errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: incomplete held-topology observation inputs")
	}
	path, err := in.OwnerPath()
	if err != nil {
		return nil, err
	}
	record, err := gui.ReadGUIOwnerRecord(path)
	if err != nil || record.Legacy || record.State != "active" || record.PID <= 0 {
		return nil, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: active GUI owner record unavailable: %v", err)
	}
	if err := in.VerifyListener(ctx, path, record.PID, record.Port); err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: GUI listener proof: %w", err)
	}
	unheld, err := in.ProbeLeaseUnheld(path)
	if err != nil || unheld {
		return nil, fmt.Errorf("UPGRADE_GUI_LOCK_PATH_MISMATCH: incumbent GUI lease is not held at its owner path: %v", err)
	}
	guis, err := in.FindGUIs(target)
	if err != nil || len(guis) != 1 || guis[0].PID != record.PID {
		return nil, fmt.Errorf("UPGRADE_GUI_DUPLICATE: expected one exact incumbent GUI; count=%d error=%v", len(guis), err)
	}
	held := &upgradeGUIHeldTopology{inputs: in, StateDir: stateDir, PidportPath: path, OwnerRecord: record}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, held.Close())
		}
	}()
	held.Identity.Target = target
	held.Identity.GUI, held.GUI, err = holdUpgradeGUIRole(ctx, record.PID, target, record.StartTime, "gui", in)
	if err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: hold GUI: %w", err)
	}
	all, err := in.ListProcesses([]string{"mcphub.exe"})
	if err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: enumerate tray: %w", err)
	}
	trayPID := 0
	var trayStartedAt time.Time
	for _, p := range all {
		if supervisorCommandLineSubcommand(p.Cmdline) != "tray" {
			continue
		}
		id, idErr := in.CurrentIdentity(p.PID)
		if idErr != nil {
			return nil, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: tray image uncertain: %w", idErr)
		}
		if samePath(id.ExecutablePath, target) {
			if trayPID != 0 {
				return nil, errors.New("UPGRADE_GUI_DUPLICATE: multiple target tray processes")
			}
			trayPID = p.PID
			trayStartedAt = id.StartedAt
		}
	}
	if trayPID <= 0 {
		return nil, errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: bound tray process is missing")
	}
	held.Identity.Tray, held.Tray, err = holdUpgradeGUIRole(ctx, trayPID, target, trayStartedAt, "tray", in)
	if err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: hold tray: %w", err)
	}
	held.Identity.Tray.ParentPID, err = in.ParentPID(trayPID)
	if err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: tray parent: %w", err)
	}
	if _, err := heldUpgradeGUIIdentity(held.Tray, trayPID, target, held.Identity.Tray.StartedAt, in.CurrentIdentity); err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: tray generation changed during parent read: %w", err)
	}
	owner, err := in.SupervisorOwner(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil || owner.PID <= 0 {
		return nil, fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: lock owner: %v", err)
	}
	held.SupervisorOwner = owner
	held.Supervisor, err = in.HoldJob(owner.PID)
	if err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: hold supervisor: %w", err)
	}
	if held.Supervisor == nil {
		return nil, errors.New("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor hold returned no process generation")
	}
	ownerStarted, err := time.Parse(time.RFC3339Nano, owner.StartedAt)
	if err != nil || (!pair && !ownerStarted.Before(record.StartTime)) || (pair && !record.StartTime.Before(ownerStarted)) {
		return nil, fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor chronology: %v", err)
	}
	supervisorID, err := heldUpgradeGUIIdentity(held.Supervisor, owner.PID, target, ownerStarted, in.CurrentIdentity)
	if err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor kernel identity: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor role: %w", err)
	}
	detail, err := in.CommandIdentity(ctx, owner.PID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor role: %w", errors.Join(err, ctxErr))
	}
	if err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor role: %w", err)
	}
	if supervisorCommandLineSubcommand(detail.CommandLine) != "supervise" || !samePath(detail.ExecutablePath, target) || detail.CreationDateUnix != supervisorID.StartedAt.Unix() {
		return nil, errors.New("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor role differs")
	}
	sameUser, err := in.SIDMatches(owner.PID)
	if err != nil || !sameUser {
		return nil, fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor user: %v", err)
	}
	if _, err := heldUpgradeGUIIdentity(held.Supervisor, owner.PID, target, ownerStarted, in.CurrentIdentity); err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor generation changed during metadata: %w", err)
	}
	if err := held.Supervisor.VerifyIdentity(process.PIDIdentityProof{PID: owner.PID, ExecutablePath: target, StartedAt: owner.StartedAt, StartTolerance: time.Nanosecond}); err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: verify held supervisor: %w", err)
	}
	inJob, err := held.Supervisor.InAnyJob()
	if err != nil || inJob != pair {
		return nil, fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: any-Job query member=%v error=%v", inJob, err)
	}
	held.Identity.Supervisor = upgradeGUIProcessIdentity{PID: owner.PID, Role: "supervise", Image: supervisorID.ExecutablePath, StartedAt: supervisorID.StartedAt, SameUser: true}
	if pair {
		held.Identity.Supervisor.ParentPID, err = in.ParentPID(owner.PID)
		if err != nil {
			return nil, fmt.Errorf("UPGRADE_GUI_PAIR_TOPOLOGY_UNPROVEN: supervisor parent: %w", err)
		}
	}
	held.Identity.SupervisorInAnyJob = inJob
	validate := validateUpgradeGUIIdentitySnapshot
	if pair {
		validate = validateUpgradeGUIOwnedPairIdentitySnapshot
	}
	if err := validate(held.Identity); err != nil {
		return nil, err
	}
	held.TaskActions, err = in.TaskActions(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_TASK_DRIFT: pre-exit task actions: %w", err)
	}
	held.IntentHash, held.FrozenPorts, held.PortOwners, err = in.PortOwners(ctx, stateDir, target, owner.PID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: managed intent/listeners: %w", errors.Join(err, ctxErr))
	}
	if err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: managed listener preimage: %w", err)
	}
	for _, port := range held.PortOwners {
		if port.PID == held.Identity.GUI.PID || port.PID == held.Identity.Tray.PID {
			return nil, errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: GUI/tray overlaps a managed listener")
		}
	}
	return held, nil
}

func recheckUpgradeGUISupervisorAndInputs(ctx context.Context, held *upgradeGUIHeldTopology) error {
	if held == nil || !held.inputs.complete() {
		return errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: incomplete held-topology recheck inputs")
	}
	in := held.inputs
	owner, err := in.SupervisorOwner(filepath.Join(held.StateDir, "supervisor.lock"))
	if err != nil || owner != held.SupervisorOwner {
		return fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor lock generation changed: %v", err)
	}
	if _, err := heldUpgradeGUIIdentity(held.Supervisor, owner.PID, held.Identity.Target, held.Identity.Supervisor.StartedAt, in.CurrentIdentity); err != nil {
		return fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: held generation changed: %w", err)
	}
	if err := held.Supervisor.VerifyIdentity(process.PIDIdentityProof{PID: owner.PID, ExecutablePath: held.Identity.Target, StartedAt: owner.StartedAt, StartTolerance: time.Nanosecond}); err != nil {
		return fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: held generation changed: %w", err)
	}
	inJob, err := held.Supervisor.InAnyJob()
	if err != nil || inJob != held.Identity.SupervisorInAnyJob {
		return fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: any-Job query member=%v error=%v", inJob, err)
	}
	if held.Identity.SupervisorInAnyJob {
		parent, err := in.ParentPID(owner.PID)
		if err != nil || parent != held.Identity.GUI.PID {
			return fmt.Errorf("UPGRADE_GUI_PAIR_TOPOLOGY_UNPROVEN: supervisor parent changed: %v", err)
		}
		if err := validateUpgradeGUIOwnedPairIdentitySnapshot(held.Identity); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor role changed: %w", err)
	}
	detail, err := in.CommandIdentity(ctx, owner.PID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor role changed: %w", errors.Join(err, ctxErr))
	}
	if err != nil {
		return fmt.Errorf("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor role changed: %w", err)
	}
	if supervisorCommandLineSubcommand(detail.CommandLine) != "supervise" || !samePath(detail.ExecutablePath, held.Identity.Target) || detail.CreationDateUnix != held.Identity.Supervisor.StartedAt.Unix() {
		return errors.New("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor role changed")
	}
	if _, err := heldUpgradeGUIIdentity(held.Supervisor, owner.PID, held.Identity.Target, held.Identity.Supervisor.StartedAt, in.CurrentIdentity); err != nil {
		return err
	}
	tasks, err := in.TaskActions(ctx, held.Identity.Target)
	if err != nil || !reflect.DeepEqual(tasks, held.TaskActions) {
		return fmt.Errorf("UPGRADE_GUI_TASK_DRIFT: exact task actions changed: %v", err)
	}
	intentHash, frozen, ports, err := in.PortOwners(ctx, held.StateDir, held.Identity.Target, owner.PID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: managed intent/listeners changed: %w", errors.Join(err, ctxErr))
	}
	if err != nil {
		return fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: managed intent/listeners changed: %w", err)
	}
	if intentHash != held.IntentHash || !reflect.DeepEqual(frozen, held.FrozenPorts) || !reflect.DeepEqual(ports, held.PortOwners) {
		return errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: managed intent/listeners changed")
	}
	return nil
}

func recheckUpgradeGUIBeforeExit(ctx context.Context, held *upgradeGUIHeldTopology) error {
	return recheckUpgradeGUIBeforeExitWithJournal(ctx, held, nil)
}

func recheckUpgradeGUIBeforeExitWithJournal(ctx context.Context, held *upgradeGUIHeldTopology, pending *windowsProductRecoveryJournalV3) error {
	if held == nil || !held.inputs.complete() {
		return errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: incomplete held-topology recheck inputs")
	}
	in := held.inputs
	if pending == nil {
		if present, err := inspectWindowsProductRecoveryLeaves(held.StateDir); err != nil || present {
			return fmt.Errorf("UPGRADE_GUI_PENDING_RECOVERY: exact journal state changed: present=%v error=%v", present, err)
		}
	} else {
		raw, err := os.ReadFile(filepath.Join(held.StateDir, WindowsProductRecoveryFileLeafV3))
		if err != nil {
			return fmt.Errorf("UPGRADE_GUI_PAIR_PREIMAGE_UNRECOVERABLE: read pending V3 journal: %w", err)
		}
		current, err := decodeWindowsProductRecoveryJournalV3(raw)
		if err != nil || !reflect.DeepEqual(current, *pending) || current.Phase != WindowsProductRecoveryGUIOwnedStopPending {
			return fmt.Errorf("UPGRADE_GUI_PAIR_PREIMAGE_UNRECOVERABLE: pending V3 journal changed: %v", err)
		}
	}
	record, err := gui.ReadGUIOwnerRecord(held.PidportPath)
	if err != nil || record != held.OwnerRecord {
		return fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: GUI owner record changed: %v", err)
	}
	if err := in.VerifyListener(ctx, held.PidportPath, record.PID, record.Port); err != nil {
		return err
	}
	unheld, err := in.ProbeLeaseUnheld(held.PidportPath)
	if err != nil || unheld {
		return fmt.Errorf("UPGRADE_GUI_LOCK_PATH_MISMATCH: incumbent lease changed: %v", err)
	}
	guis, err := in.FindGUIs(held.Identity.Target)
	if err != nil || len(guis) != 1 || guis[0].PID != record.PID {
		return fmt.Errorf("UPGRADE_GUI_DUPLICATE: GUI population changed count=%d error=%v", len(guis), err)
	}
	if _, err := heldUpgradeGUIIdentity(held.GUI, record.PID, held.Identity.Target, record.StartTime, in.CurrentIdentity); err != nil {
		return err
	}
	if _, err := heldUpgradeGUIIdentity(held.Tray, held.Identity.Tray.PID, held.Identity.Target, held.Identity.Tray.StartedAt, in.CurrentIdentity); err != nil {
		return err
	}
	parent, err := in.ParentPID(held.Identity.Tray.PID)
	if err != nil || parent != record.PID {
		return fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: tray parent changed: %v", err)
	}
	if _, err := heldUpgradeGUIIdentity(held.Tray, held.Identity.Tray.PID, held.Identity.Target, held.Identity.Tray.StartedAt, in.CurrentIdentity); err != nil {
		return err
	}
	return recheckUpgradeGUISupervisorAndInputs(ctx, held)
}

func recheckUpgradeGUIAfterExit(ctx context.Context, held *upgradeGUIHeldTopology) error {
	if held == nil || !held.inputs.complete() {
		return errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: incomplete held-topology recheck inputs")
	}
	in := held.inputs
	guis, err := in.FindGUIs(held.Identity.Target)
	if err != nil || len(guis) != 0 {
		return fmt.Errorf("UPGRADE_GUI_RELAUNCH_RACE: GUI process remains count=%d error=%v", len(guis), err)
	}
	all, err := in.ListProcesses([]string{"mcphub.exe"})
	if err != nil {
		return err
	}
	for _, p := range all {
		if supervisorCommandLineSubcommand(p.Cmdline) != "tray" {
			continue
		}
		id, idErr := in.CurrentIdentity(p.PID)
		if idErr != nil {
			return fmt.Errorf("UPGRADE_GUI_RELAUNCH_RACE: tray identity uncertain: %w", idErr)
		}
		if samePath(id.ExecutablePath, held.Identity.Target) {
			return errors.New("UPGRADE_GUI_RELAUNCH_RACE: target tray process remains")
		}
	}
	if held.Identity.SupervisorInAnyJob {
		return nil
	}
	return recheckUpgradeGUISupervisorAndInputs(ctx, held)
}

func exitAndReserveUpgradeGUIWithStage(ctx context.Context, stateDir, target string, emitExitRequested func() error) (gui.OwnedSingleInstanceLease, error) {
	return exitAndReserveUpgradeGUIWithOps(ctx, stateDir, target, upgradeGUIExitOps{
		Observe:         observeUpgradeGUIHeldTopology,
		PreExitCheck:    recheckUpgradeGUIBeforeExit,
		OnExitRequested: emitExitRequested,
		Reserve: func(observedPath string) (gui.OwnedSingleInstanceLease, error) {
			path, err := gui.PidportPath()
			if err != nil {
				return nil, err
			}
			if !samePath(path, observedPath) {
				return nil, errors.New("UPGRADE_GUI_LOCK_PATH_MISMATCH: reservation path differs from incumbent owner path")
			}
			return gui.AcquireSingleInstanceLockOnlyAt(path)
		},
		PostExitCheck: recheckUpgradeGUIAfterExit,
	})
}

func exitAndReserveUpgradeGUIOwnedPair(ctx context.Context, held *upgradeGUIHeldTopology, txn *WindowsProductTxn, emitExitRequested func() error) (gui.OwnedSingleInstanceLease, error) {
	if held == nil || txn == nil || txn.prepared == nil || txn.prepared.journal.Phase != WindowsProductRecoveryGUIOwnedStopPending {
		return nil, errors.New("UPGRADE_GUI_PAIR_PREIMAGE_UNRECOVERABLE: durable pending Product transaction is missing")
	}
	return exitAndReserveUpgradeGUIWithOps(ctx, held.StateDir, held.Identity.Target, upgradeGUIExitOps{
		AllowPair: true,
		Observe:   func(context.Context, string, string) (*upgradeGUIHeldTopology, error) { return held, nil },
		PreExitCheck: func(ctx context.Context, held *upgradeGUIHeldTopology) error {
			return recheckUpgradeGUIBeforeExitWithJournal(ctx, held, &txn.prepared.journal)
		},
		OnExitRequested: emitExitRequested,
		Reserve: func(observedPath string) (gui.OwnedSingleInstanceLease, error) {
			path, err := gui.PidportPath()
			if err != nil {
				return nil, err
			}
			if !samePath(path, observedPath) {
				return nil, errors.New("UPGRADE_GUI_LOCK_PATH_MISMATCH: reservation path differs from incumbent owner path")
			}
			return gui.AcquireSingleInstanceLockOnlyAt(path)
		},
		PostExitCheck: recheckUpgradeGUIAfterExit,
	})
}

func (h *upgradeGUIHeldTopology) Close() error {
	if h == nil {
		return nil
	}
	var errs []error
	if h.GUI != nil {
		errs = append(errs, h.GUI.Close())
	}
	if h.Tray != nil {
		errs = append(errs, h.Tray.Close())
	}
	if h.Supervisor != nil {
		errs = append(errs, h.Supervisor.Close())
	}
	closeErr := errors.Join(errs...)
	if h.inputs.PendingRuntime != nil {
		return pendingRuntimeCloseError(nil, h.inputs.PendingRuntime, closeErr)
	}
	return closeErr
}

type upgradeGUIExitOps struct {
	AllowPair       bool
	Observe         func(context.Context, string, string) (*upgradeGUIHeldTopology, error)
	PreExitCheck    func(context.Context, *upgradeGUIHeldTopology) error
	OnExitRequested func() error
	Reserve         func(string) (gui.OwnedSingleInstanceLease, error)
	PostExitCheck   func(context.Context, *upgradeGUIHeldTopology) error
}

func resolveUpgradeGUIOwnerPath() (string, error) {
	if os.Getenv("MCPHUB_GUI_TEST_PIDPORT_DIR") != "" {
		return "", errors.New("UPGRADE_GUI_LOCK_PATH_MISMATCH: GUI pidport override cannot bind the installed owner")
	}
	path, err := gui.PidportPathNoCreate()
	if err != nil {
		return "", fmt.Errorf("UPGRADE_GUI_LOCK_PATH_MISMATCH: resolve incumbent GUI owner path: %w", err)
	}
	for _, leaf := range []string{filepath.Dir(path), path, path + ".lock"} {
		info, err := os.Lstat(leaf)
		if err != nil {
			return "", fmt.Errorf("UPGRADE_GUI_LOCK_PATH_MISMATCH: inspect GUI lease path: %w", err)
		}
		if (leaf == filepath.Dir(path) && !info.IsDir()) || (leaf != filepath.Dir(path) && !info.Mode().IsRegular()) || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("UPGRADE_GUI_LOCK_PATH_MISMATCH: GUI lease path is not ordinary")
		}
		resolved, err := process.CanonicalizePathStrict(leaf)
		if err != nil || !samePath(resolved, leaf) {
			return "", fmt.Errorf("UPGRADE_GUI_LOCK_PATH_MISMATCH: GUI lease path reparse or resolution drift: %v", err)
		}
	}
	return path, nil
}

func exitAndReserveUpgradeGUIWithOps(ctx context.Context, stateDir, target string, ops upgradeGUIExitOps) (lease gui.OwnedSingleInstanceLease, retErr error) {
	if ctx == nil || ops.Observe == nil || ops.PreExitCheck == nil || ops.Reserve == nil || ops.PostExitCheck == nil {
		return nil, errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: exit observation dependencies are incomplete")
	}
	held, err := ops.Observe(ctx, stateDir, target)
	if err != nil {
		if held != nil {
			err = errors.Join(err, held.Close())
		}
		return nil, err
	}
	if held == nil {
		return nil, errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: no held topology returned")
	}
	defer func() { retErr = errors.Join(retErr, held.Close()) }()
	if held.GUI == nil || held.Tray == nil || held.Supervisor == nil || held.GUI.PID() != held.Identity.GUI.PID || held.Tray.PID() != held.Identity.Tray.PID {
		return nil, errors.New("UPGRADE_GUI_IDENTITY_AMBIGUOUS: held process generations differ from admission")
	}
	validate := validateUpgradeGUIIdentitySnapshot
	if ops.AllowPair {
		validate = validateUpgradeGUIOwnedPairIdentitySnapshot
	}
	if err := validate(held.Identity); err != nil {
		return nil, err
	}
	if err := ops.PreExitCheck(ctx, held); err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: pre-exit generation drift: %w", err)
	}
	if ops.OnExitRequested != nil {
		if err := ops.OnExitRequested(); err != nil {
			return nil, fmt.Errorf("UPGRADE_GUI_EXIT_UNCONFIRMED: report force-exit stage before termination: %w", err)
		}
	}
	if committed, err := held.GUI.Terminate(); err != nil {
		return nil, fmt.Errorf("UPGRADE_GUI_EXIT_UNCONFIRMED: GUI termination committed=%v: %w", committed, err)
	}
	if committed, err := held.Tray.Terminate(); err != nil && !errors.Is(err, process.ErrProcessAlreadyExited) {
		return nil, fmt.Errorf("UPGRADE_GUI_EXIT_UNCONFIRMED: tray termination committed=%v: %w", committed, err)
	}
	lease, err = ops.Reserve(held.PidportPath)
	if err != nil {
		if lease != nil {
			if releaseErr := lease.ReleaseErr(); releaseErr != nil {
				err = errors.Join(err, fmt.Errorf("UPGRADE_GUI_LEASE_RELEASE_UNCONFIRMED: tentative reservation release: %w", releaseErr))
			}
		}
		return nil, fmt.Errorf("UPGRADE_GUI_RELAUNCH_RACE: reserve incumbent GUI lease: %w", err)
	}
	if lease == nil {
		return nil, errors.New("UPGRADE_GUI_RESERVATION_HELD: GUI lease was not returned")
	}
	if err := ops.PostExitCheck(ctx, held); err != nil {
		return lease, fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: post-exit generation drift: %w", err)
	}
	return lease, nil
}

func validateUpgradeGUIIdentitySnapshot(s upgradeGUIIdentitySnapshot) error {
	invalid := func(reason string) error { return fmt.Errorf("UPGRADE_GUI_IDENTITY_AMBIGUOUS: %s", reason) }
	if s.Target == "" || s.GUI.PID <= 0 || s.Tray.PID <= 0 || s.Supervisor.PID <= 0 ||
		s.GUI.PID == s.Tray.PID || s.GUI.PID == s.Supervisor.PID || s.Tray.PID == s.Supervisor.PID {
		return invalid("target or process generations overlap")
	}
	if !s.GUI.SameUser || !s.Tray.SameUser || !s.Supervisor.SameUser ||
		(s.GUI.Role != "gui" && s.GUI.Role != "") || s.Tray.Role != "tray" || s.Supervisor.Role != "supervise" ||
		!samePath(s.GUI.Image, s.Target) || !samePath(s.Tray.Image, s.Target) || !samePath(s.Supervisor.Image, s.Target) {
		return invalid("process role, owner, or image differs")
	}
	if s.GUI.StartedAt.IsZero() || s.Tray.StartedAt.IsZero() || s.Supervisor.StartedAt.IsZero() ||
		!s.Supervisor.StartedAt.Before(s.GUI.StartedAt) || s.Tray.StartedAt.Before(s.GUI.StartedAt) || s.Tray.ParentPID != s.GUI.PID {
		return invalid("process chronology or tray parent differs")
	}
	if s.SupervisorInAnyJob {
		return errors.New("UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN: supervisor belongs to a Windows Job")
	}
	return nil
}

func validateUpgradeGUIOwnedPairIdentitySnapshot(s upgradeGUIIdentitySnapshot) error {
	invalid := func(reason string) error { return fmt.Errorf("UPGRADE_GUI_PAIR_TOPOLOGY_UNPROVEN: %s", reason) }
	if s.Target == "" || s.GUI.PID <= 0 || s.Tray.PID <= 0 || s.Supervisor.PID <= 0 ||
		s.GUI.PID == s.Tray.PID || s.GUI.PID == s.Supervisor.PID || s.Tray.PID == s.Supervisor.PID {
		return invalid("target or process generations overlap")
	}
	if !s.GUI.SameUser || !s.Tray.SameUser || !s.Supervisor.SameUser ||
		(s.GUI.Role != "gui" && s.GUI.Role != "") || s.Tray.Role != "tray" || s.Supervisor.Role != "supervise" ||
		!samePath(s.GUI.Image, s.Target) || !samePath(s.Tray.Image, s.Target) || !samePath(s.Supervisor.Image, s.Target) {
		return invalid("process role, owner, or image differs")
	}
	if s.GUI.StartedAt.IsZero() || s.Tray.StartedAt.IsZero() || s.Supervisor.StartedAt.IsZero() ||
		!s.GUI.StartedAt.Before(s.Supervisor.StartedAt) || s.Tray.StartedAt.Before(s.GUI.StartedAt) ||
		s.Tray.ParentPID != s.GUI.PID || s.Supervisor.ParentPID != s.GUI.PID || !s.SupervisorInAnyJob {
		return invalid("GUI child chronology, parent, or Job membership differs")
	}
	return nil
}

func prepareUpgradeGUIOwnedPairTransaction(txn *WindowsProductTxn, held *upgradeGUIHeldTopology) error {
	if txn == nil || held == nil || txn.prepared != nil || txn.Opts.Mode != WindowsProductPairModeUpgrade ||
		!samePath(txn.Opts.ProductPath, held.Identity.Target) {
		return errors.New("UPGRADE_GUI_PAIR_PREIMAGE_UNRECOVERABLE: Product transaction or held pair is unavailable")
	}
	if err := validateUpgradeGUIOwnedPairIdentitySnapshot(held.Identity); err != nil {
		return err
	}
	if len(held.FrozenPorts) != len(held.PortOwners) {
		return errors.New("UPGRADE_GUI_PAIR_TOPOLOGY_UNPROVEN: frozen intent and listener populations differ")
	}
	ports := make([]windowsProductPortSettlementV1, 0, len(held.FrozenPorts))
	for i, frozen := range held.FrozenPorts {
		owner := held.PortOwners[i]
		if frozen.TaskName != owner.TaskName || frozen.Port != owner.Port || frozen.Port <= 0 || frozen.ArgvSHA256 == "" {
			return errors.New("UPGRADE_GUI_PAIR_TOPOLOGY_UNPROVEN: frozen intent/listener identity differs")
		}
		row := windowsProductPortSettlementV1{TaskName: frozen.TaskName, Port: frozen.Port, PreState: windowsProductPortUnbound}
		if owner.PID != 0 {
			if owner.ParentPID != held.Identity.Supervisor.PID || owner.StartedAt.IsZero() || owner.ArgvSHA256 != frozen.ArgvSHA256 {
				return errors.New("UPGRADE_GUI_PAIR_TOPOLOGY_UNPROVEN: running daemon owner proof differs")
			}
			row.PreState, row.PrePID = windowsProductPortCanonicalOwned, owner.PID
		}
		ports = append(ports, row)
	}
	txn.Opts.FrozenPorts = append([]windowsProductIntentPortV1(nil), held.FrozenPorts...)
	txn.Opts.PreSettlement = &windowsProductQuiescenceObservationV1{
		Supervisor: windowsProductSupervisorSettlementV1{State: windowsProductSupervisorGracefullySettled,
			Generation: &windowsProductSupervisorGenerationV1{PID: held.Identity.Supervisor.PID, StartedAt: held.SupervisorOwner.StartedAt, IPCRequestVersion: 0}},
		Ports: ports, GUIOwner: windowsProductGUIOwnerConfirmedPresent,
	}
	if err := txn.Prepare(); err != nil {
		return fmt.Errorf("UPGRADE_GUI_PAIR_PREIMAGE_UNRECOVERABLE: %w", err)
	}
	return nil
}

func validateUpgradeGUIOwnedPriorHealth(journal windowsProductRecoveryJournalV3, held *upgradeGUIHeldTopology) error {
	if held == nil || held.Identity.GUI.PID <= 0 || held.Identity.Tray.PID <= 0 || held.Identity.Supervisor.PID <= 0 ||
		journal.PreSettlement == nil || journal.PreSettlement.GUIOwner != windowsProductGUIOwnerConfirmedPresent ||
		len(journal.FrozenPorts) != len(held.FrozenPorts) || len(journal.FrozenPorts) != len(held.PortOwners) ||
		len(journal.FrozenPorts) != len(journal.PreSettlement.Ports) || len(journal.Tasks.Backups) != len(held.TaskActions) {
		return errors.New("UPGRADE_GUI_PAIR_RECOVERY_UNCONFIRMED: GUI, task, or fleet population differs")
	}
	if err := validateGUIOwnedIndependentFleet(journal, held.FrozenPorts, held.PortOwners, held.Identity.Supervisor.PID); err != nil {
		return fmt.Errorf("UPGRADE_GUI_PAIR_RECOVERY_UNCONFIRMED: %w", err)
	}
	return validateGUIOwnedPriorTasks(journal, held.TaskActions)
}

func validateGUIOwnedPriorTasks(journal windowsProductRecoveryJournalV3, actions []upgradeGUITaskAction) error {
	if len(journal.Tasks.Backups) != len(actions) {
		return errors.New("UPGRADE_GUI_PAIR_RECOVERY_UNCONFIRMED: task population differs")
	}
	backups := make(map[string]string, len(journal.Tasks.Backups))
	for _, backup := range journal.Tasks.Backups {
		if _, exists := backups[backup.TaskName]; exists {
			return errors.New("UPGRADE_GUI_PAIR_RECOVERY_UNCONFIRMED: duplicate frozen task")
		}
		backups[backup.TaskName] = backup.SHA256
	}
	for _, action := range actions {
		want, exists := backups[action.Name]
		if !exists || want != hex.EncodeToString(action.XMLSHA256[:]) {
			return errors.New("UPGRADE_GUI_PAIR_RECOVERY_UNCONFIRMED: exact task XML differs")
		}
		delete(backups, action.Name)
	}
	if len(backups) != 0 {
		return errors.New("UPGRADE_GUI_PAIR_RECOVERY_UNCONFIRMED: frozen task missing")
	}
	return nil
}

func validateGUIOwnedIndependentFleet(journal windowsProductRecoveryJournalV3, observed []windowsProductIntentPortV1, owners []upgradeGUIPortOwner, supervisorPID int) error {
	if journal.PreSettlement == nil || supervisorPID <= 0 || len(journal.FrozenPorts) != len(observed) ||
		len(observed) != len(owners) || len(observed) != len(journal.PreSettlement.Ports) {
		return errors.New("frozen independent fleet population differs")
	}
	pre := make(map[string]windowsProductPortSettlementV1, len(journal.PreSettlement.Ports))
	for _, row := range journal.PreSettlement.Ports {
		key := fmt.Sprintf("%s\x00%d", row.TaskName, row.Port)
		if _, exists := pre[key]; exists {
			return errors.New("duplicate frozen prior port")
		}
		pre[key] = row
	}
	want := make(map[string]api.SupervisorFrozenPort, len(journal.FrozenPorts))
	for _, row := range journal.FrozenPorts {
		key := fmt.Sprintf("%s\x00%d", row.TaskName, row.Port)
		if _, exists := want[key]; exists {
			return errors.New("duplicate frozen intent port")
		}
		want[key] = row
	}
	for i, row := range observed {
		key := fmt.Sprintf("%s\x00%d", row.TaskName, row.Port)
		frozen, found := want[key]
		prior, present := pre[key]
		owner := owners[i]
		if !found || !present || frozen.ArgvSHA256 != row.ArgvSHA256 || owner.TaskName != row.TaskName || owner.Port != row.Port {
			return errors.New("frozen independent fleet identity differs")
		}
		switch prior.PreState {
		case windowsProductPortCanonicalOwned:
			if prior.PrePID <= 0 || owner.PID <= 0 || owner.ParentPID != supervisorPID || owner.ArgvSHA256 != frozen.ArgvSHA256 {
				return errors.New("prior running daemon is not independently owned")
			}
		case windowsProductPortUnbound:
			if prior.PrePID != 0 || owner.PID != 0 {
				return errors.New("intentionally stopped daemon became bound")
			}
		default:
			return errors.New("unsupported prior daemon port state")
		}
		delete(want, key)
		delete(pre, key)
	}
	if len(want) != 0 || len(pre) != 0 {
		return errors.New("frozen independent fleet row missing")
	}
	return nil
}

type guiOwnedPriorSupervisorState string

const (
	guiOwnedPriorSupervisorSame   guiOwnedPriorSupervisorState = "same"
	guiOwnedPriorSupervisorAbsent guiOwnedPriorSupervisorState = "absent"
)

type guiOwnedPriorReservationOps struct {
	ProveNoGUIOrTray func(context.Context) error
	AcquireExact     func() (gui.OwnedSingleInstanceLease, error)
}

func acquireGUIOwnedPriorReservation(ctx context.Context, owned gui.OwnedSingleInstanceLease, ops guiOwnedPriorReservationOps) (gui.OwnedSingleInstanceLease, error) {
	if ctx == nil || ops.ProveNoGUIOrTray == nil || ops.AcquireExact == nil {
		return nil, fmt.Errorf("%w: GUI absence/reservation owner is incomplete", ErrWindowsProductRecoveryPending)
	}
	if err := ops.ProveNoGUIOrTray(ctx); err != nil {
		return nil, &upgradeGUIPriorSettlementError{operation: upgradeGUIPriorReservation, reservationStep: upgradeGUIReservationFirstAbsence,
			cause: fmt.Errorf("%w: exact GUI/tray absence unproven: %w", ErrWindowsProductRecoveryPending, err)}
	}
	if owned != nil {
		return owned, nil
	}
	lease, err := ops.AcquireExact()
	if err != nil || lease == nil {
		return nil, &upgradeGUIPriorSettlementError{operation: upgradeGUIPriorReservation, reservationStep: upgradeGUIReservationAcquireExact,
			cause: errors.Join(fmt.Errorf("%w: exact GUI reservation unavailable", ErrWindowsProductRecoveryPending), err)}
	}
	if err := ops.ProveNoGUIOrTray(ctx); err != nil {
		releaseErr := lease.ReleaseErr()
		return nil, &upgradeGUIPriorSettlementError{operation: upgradeGUIPriorReservation, reservationStep: upgradeGUIReservationSecondAbsence,
			cause:              errors.Join(fmt.Errorf("%w: GUI/tray appeared during reservation: %w", ErrWindowsProductRecoveryPending, err), releaseErr),
			reservationRelease: releaseErr}
	}
	return lease, nil
}

// The GUI record owns state interpretation; this existing consumer operation
// owns only the original active endpoint observation after GUI/tray absence.
func proveUpgradeGUIOwnerEndpointAbsent(ctx context.Context, record gui.GUIOwnerRecord, observe func(context.Context, int) (int, bool, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	port, terminal, err := record.ReservationEndpoint()
	if err != nil {
		return err
	}
	if terminal {
		return nil
	}
	pid, bound, err := observe(ctx, port)
	if err != nil {
		return err
	}
	if bound {
		return fmt.Errorf("GUI listener port remains bound by PID %d", pid)
	}
	return nil
}

func readbackGUIOwnedPriorProduct(journal windowsProductRecoveryJournalV3) error {
	for _, artifact := range []struct {
		path  string
		prior windowsProductPreimageV1
	}{
		{journal.Product.Target, windowsProductPreimageV1{Present: journal.Product.PriorPresent, SHA256: journal.Product.PriorSHA256}},
		{journal.LegacyWindowless.Target, windowsProductPreimageV1{Present: journal.LegacyWindowless.PriorPresent, SHA256: journal.LegacyWindowless.PriorSHA256}},
	} {
		if !cleanAbsolutePath(artifact.path) {
			return errors.New("UPGRADE_GUI_PAIR_RECOVERY_UNCONFIRMED: prior Product path is invalid")
		}
		snapshot, err := snapshotProductPairFile(artifact.path)
		if err != nil || windowsProductPreimage(snapshot) != artifact.prior {
			return fmt.Errorf("UPGRADE_GUI_PAIR_RECOVERY_UNCONFIRMED: prior Product preimage changed at %s: %v", artifact.path, err)
		}
	}
	for _, receipt := range []windowsProductPairRecoveryReceipt{journal.Receipts.V3, journal.Receipts.V2} {
		if !cleanAbsolutePath(receipt.Path) {
			return errors.New("UPGRADE_GUI_PAIR_RECOVERY_UNCONFIRMED: prior receipt path is invalid")
		}
		raw, err := os.ReadFile(receipt.Path)
		if errors.Is(err, os.ErrNotExist) && !receipt.PriorPresent {
			continue
		}
		if err != nil || !receipt.PriorPresent || !bytes.Equal(raw, receipt.PriorBytes) {
			return fmt.Errorf("UPGRADE_GUI_PAIR_RECOVERY_UNCONFIRMED: prior receipt changed at %s: %v", receipt.Path, err)
		}
	}
	return nil
}

type guiOwnedPriorRecoveryOps struct {
	Readback            func(windowsProductRecoveryJournalV3) error
	ObserveHealthy      func(context.Context, windowsProductRecoveryJournalV3) error
	Reservation         func(context.Context) (gui.OwnedSingleInstanceLease, error)
	SupervisorState     func(context.Context, windowsProductRecoveryJournalV3) (guiOwnedPriorSupervisorState, error)
	SettleSame          func(context.Context, windowsProductRecoveryJournalV3) error
	ProveAbsent         func(context.Context, windowsProductRecoveryJournalV3) error
	StartSupervisor     func() (SupervisorLaunch, error)
	SupervisorExpected  SupervisorControlExpectation
	WaitSupervisorReady func(context.Context) error
	ProveIndependent    func(context.Context, windowsProductRecoveryJournalV3) error
	StartGUI            func() error
	PollInterval        time.Duration
}

type upgradeGUIPriorOperation string

const (
	upgradeGUIPriorInput              upgradeGUIPriorOperation = "INPUT"
	upgradeGUIPriorReadback           upgradeGUIPriorOperation = "READBACK"
	upgradeGUIPriorReservation        upgradeGUIPriorOperation = "RESERVATION"
	upgradeGUIPriorSupervisorState    upgradeGUIPriorOperation = "SUPERVISOR_STATE"
	upgradeGUIPriorSettleSame         upgradeGUIPriorOperation = "SETTLE_SAME"
	upgradeGUIPriorAbsence            upgradeGUIPriorOperation = "ABSENCE"
	upgradeGUIPriorSupervisorStart    upgradeGUIPriorOperation = "SUPERVISOR_START"
	upgradeGUIPriorControl            upgradeGUIPriorOperation = "CONTROL"
	upgradeGUIPriorReadiness          upgradeGUIPriorOperation = "READINESS"
	upgradeGUIPriorIndependentFleet   upgradeGUIPriorOperation = "INDEPENDENT_FLEET"
	upgradeGUIPriorReservationRelease upgradeGUIPriorOperation = "RESERVATION_RELEASE"
	upgradeGUIPriorGUIStart           upgradeGUIPriorOperation = "GUI_START"
	upgradeGUIPriorFinalReadback      upgradeGUIPriorOperation = "FINAL_READBACK"
	upgradeGUIPriorFinalHealth        upgradeGUIPriorOperation = "FINAL_HEALTH"
	upgradeGUIPriorHandoff            upgradeGUIPriorOperation = "HANDOFF"
)

func isUpgradeGUIPriorOperation(operation upgradeGUIPriorOperation) bool {
	switch operation {
	case upgradeGUIPriorInput, upgradeGUIPriorReadback, upgradeGUIPriorReservation,
		upgradeGUIPriorSupervisorState, upgradeGUIPriorSettleSame, upgradeGUIPriorAbsence,
		upgradeGUIPriorSupervisorStart, upgradeGUIPriorControl, upgradeGUIPriorReadiness,
		upgradeGUIPriorIndependentFleet, upgradeGUIPriorReservationRelease, upgradeGUIPriorGUIStart,
		upgradeGUIPriorFinalReadback, upgradeGUIPriorFinalHealth, upgradeGUIPriorHandoff:
		return true
	default:
		return false
	}
}

type upgradeGUIPriorReservationStep string

const (
	upgradeGUIReservationFirstAbsence  upgradeGUIPriorReservationStep = "FIRST_ABSENCE"
	upgradeGUIReservationAcquireExact  upgradeGUIPriorReservationStep = "ACQUIRE_EXACT"
	upgradeGUIReservationSecondAbsence upgradeGUIPriorReservationStep = "SECOND_ABSENCE"
)

func isUpgradeGUIPriorReservationStep(step upgradeGUIPriorReservationStep) bool {
	switch step {
	case upgradeGUIReservationFirstAbsence, upgradeGUIReservationAcquireExact, upgradeGUIReservationSecondAbsence:
		return true
	default:
		return false
	}
}

// The terminal chain keeps its primary and cleanup ordering. Health observations
// are prerequisites retained only when the fallback ultimately fails.
type upgradeGUIPriorSettlementError struct {
	reservationStep           upgradeGUIPriorReservationStep
	reservationRelease        error
	operation                 upgradeGUIPriorOperation
	cause                     error
	initialHealth, lastHealth error
}

func (*upgradeGUIPriorSettlementError) Error() string {
	return "E_WINDOWS_PRODUCT_RECOVERY_PENDING: prior GUI settlement unconfirmed"
}
func (upgradeGUIPriorSettlementError) String() string {
	return "prior GUI settlement diagnostic (private)"
}
func (e upgradeGUIPriorSettlementError) GoString() string { return e.String() }
func (e *upgradeGUIPriorSettlementError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return []error{e.cause, e.initialHealth, e.lastHealth}
}

func recoverGUIOwnedPriorSettlementWithOps(ctx context.Context, journal windowsProductRecoveryJournalV3, ops guiOwnedPriorRecoveryOps) (retErr error) {
	operation := upgradeGUIPriorInput
	var initialHealth, lastHealth error
	defer func() {
		if retErr != nil {
			diagnostic := &upgradeGUIPriorSettlementError{operation: operation, cause: retErr, initialHealth: initialHealth, lastHealth: lastHealth}
			var reservation *upgradeGUIPriorSettlementError
			if operation == upgradeGUIPriorReservation && errors.As(retErr, &reservation) && reservation.operation == upgradeGUIPriorReservation {
				diagnostic.reservationStep = reservation.reservationStep
				diagnostic.reservationRelease = reservation.reservationRelease
			}
			retErr = diagnostic
		}
	}()
	if ctx == nil || journal.PreSettlement == nil || journal.PreSettlement.GUIOwner != windowsProductGUIOwnerConfirmedPresent ||
		ops.Readback == nil || ops.ObserveHealthy == nil || ops.Reservation == nil || ops.SupervisorState == nil ||
		ops.SettleSame == nil || ops.ProveAbsent == nil || ops.StartSupervisor == nil ||
		ops.WaitSupervisorReady == nil || ops.ProveIndependent == nil || ops.StartGUI == nil {
		return fmt.Errorf("%w: GUI-owned prior recovery inputs are incomplete", ErrWindowsProductRecoveryPending)
	}
	operation = upgradeGUIPriorReadback
	if err := ops.Readback(journal); err != nil {
		return fmt.Errorf("%w: prior Product or receipt readback: %w", ErrWindowsProductRecoveryPending, err)
	}
	initialHealth = ops.ObserveHealthy(ctx, journal)
	if initialHealth == nil {
		return nil
	}
	if pendingRuntimeStopsRecovery(initialHealth) {
		return initialHealth
	}
	operation = upgradeGUIPriorReservation
	lease, err := ops.Reservation(ctx)
	if err != nil || lease == nil {
		return errors.Join(fmt.Errorf("%w: GUI absence/reservation unproven", ErrWindowsProductRecoveryPending), err)
	}
	released := false
	defer func() {
		if !released {
			retErr = errors.Join(retErr, lease.ReleaseErr())
		}
	}()
	operation = upgradeGUIPriorSupervisorState
	state, err := ops.SupervisorState(ctx, journal)
	if err != nil || (state != guiOwnedPriorSupervisorSame && state != guiOwnedPriorSupervisorAbsent) {
		return errors.Join(fmt.Errorf("%w: original supervisor state is ambiguous", ErrWindowsProductRecoveryPending), err)
	}
	if state == guiOwnedPriorSupervisorSame {
		operation = upgradeGUIPriorSettleSame
		if err := ops.SettleSame(ctx, journal); err != nil {
			return fmt.Errorf("%w: exact original supervisor reap unconfirmed: %w", ErrWindowsProductRecoveryPending, err)
		}
	}
	operation = upgradeGUIPriorAbsence
	if err := ops.ProveAbsent(ctx, journal); err != nil {
		return fmt.Errorf("%w: prior supervisor lock/ports remain: %w", ErrWindowsProductRecoveryPending, err)
	}
	operation = upgradeGUIPriorSupervisorStart
	launch, startErr := ops.StartSupervisor()
	if startErr != nil || launch == nil {
		return errors.Join(fmt.Errorf("%w: independent prior supervisor start unconfirmed: %w", ErrWindowsProductRecoveryPending, errors.Join(startErr, missingSupervisorLaunch(launch))), abortStartedSupervisor(launch))
	}
	handedOff := false
	defer func() {
		if !handedOff {
			retErr = errors.Join(retErr, abortStartedSupervisor(launch))
		}
	}()
	operation = upgradeGUIPriorControl
	binding, err := launch.BindControl(ctx, ops.SupervisorExpected)
	if err != nil {
		return fmt.Errorf("%w: prior supervisor control unconfirmed: %w", ErrWindowsProductRecoveryPending, err)
	}
	operation = upgradeGUIPriorReadiness
	if err := ops.WaitSupervisorReady(ctx); err != nil {
		return fmt.Errorf("%w: prior supervisor readiness unconfirmed: %w", ErrWindowsProductRecoveryPending, err)
	}
	operation = upgradeGUIPriorIndependentFleet
	if err := ops.ProveIndependent(ctx, journal); err != nil {
		return fmt.Errorf("%w: independent prior fleet health unconfirmed: %w", ErrWindowsProductRecoveryPending, err)
	}
	operation = upgradeGUIPriorReservationRelease
	if err := lease.ReleaseErr(); err != nil {
		return fmt.Errorf("%w: release GUI reservation before canonical task: %w", ErrWindowsProductRecoveryPending, err)
	}
	released = true
	operation = upgradeGUIPriorGUIStart
	if err := ops.StartGUI(); err != nil {
		return fmt.Errorf("%w: start canonical GUI task: %w", ErrWindowsProductRecoveryPending, err)
	}
	interval := ops.PollInterval
	if interval <= 0 {
		interval = 200 * time.Millisecond
	}
	limit := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		limit, cancel = context.WithTimeout(ctx, defaultSupervisorLockReleaseTimeout)
		defer cancel()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastObservation error
	for {
		operation = upgradeGUIPriorFinalReadback
		if readbackErr := ops.Readback(journal); readbackErr == nil {
			operation = upgradeGUIPriorFinalHealth
			if healthErr := ops.ObserveHealthy(limit, journal); healthErr == nil {
				operation = upgradeGUIPriorHandoff
				if err := launch.Handoff(limit, binding); err != nil {
					return fmt.Errorf("%w: prior supervisor managed handoff: %w", ErrWindowsProductRecoveryPending, err)
				}
				handedOff = true
				return nil
			} else {
				lastHealth, lastObservation = healthErr, healthErr
				if pendingRuntimeStopsRecovery(healthErr) {
					return healthErr
				}
			}
		} else {
			lastObservation = readbackErr
			if pendingRuntimeStopsRecovery(readbackErr) {
				return readbackErr
			}
		}
		select {
		case <-limit.Done():
			return fmt.Errorf("%w: prior GUI/fleet readback remained unconfirmed: %w", ErrWindowsProductRecoveryPending, errors.Join(lastObservation, limit.Err()))
		case <-ticker.C:
		}
	}
}

func upgradeGUIPreparedStageReporter(w io.Writer, recorded *error) func() error {
	const message = "UPGRADE_GUI_PREPARED: Product, receipt and exact task XML snapshot plus V3 journal read back"
	return func() error {
		n, err := fmt.Fprintln(w, message)
		if err == nil && n != len(message)+1 {
			err = io.ErrShortWrite
		}
		if err != nil {
			*recorded = fmt.Errorf("UPGRADE_GUI_STAGE_OUTPUT_UNCONFIRMED: Product transaction will settle before reporting failure: %w", err)
		}
		return nil
	}
}

func writeUpgradeGUICompletion(w io.Writer, message string) error {
	n, err := fmt.Fprintln(w, message)
	if err == nil && n != len(message)+1 {
		err = io.ErrShortWrite
	}
	if err != nil {
		return fmt.Errorf("UPGRADE_GUI_COMPLETION_OUTPUT_UNCONFIRMED: Product state is settled; completion output failed: %w", err)
	}
	return nil
}

// runUpgradeGUIFencedPrelude is the no-journal window between one held Product
// fence and the unchanged transaction. The caller owns any returned GUI lease,
// including on a post-exit error, and releases it before releasing the fence.
func runUpgradeGUIFencedPrelude(ctx context.Context, fence *api.UpgradeFenceLease, stateDir, exe, target string, deps upgradeGUIFencedDeps) (gui.OwnedSingleInstanceLease, upgradeGUIProductOutcome, error) {
	if ctx == nil || fence == nil || deps.Classify == nil || deps.PublishCurrent == nil || deps.ExitAndReserve == nil {
		return nil, "", errors.New("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: incomplete fenced upgrade window")
	}
	checkJournals := func(afterExit bool) error {
		present, err := inspectWindowsProductRecoveryLeaves(stateDir)
		if err != nil {
			if afterExit {
				return fmt.Errorf("UPGRADE_GUI_JOURNAL_DRIFT_AFTER_EXIT: journal identity unknown after GUI exit: %w", err)
			}
			return fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: exact journal observation: %w", err)
		}
		if present {
			if afterExit {
				return errors.New("UPGRADE_GUI_JOURNAL_DRIFT_AFTER_EXIT: recovery journal appeared after GUI exit")
			}
			return errors.New("UPGRADE_GUI_PENDING_RECOVERY: exact Product recovery journal exists")
		}
		return nil
	}
	if err := checkJournals(false); err != nil {
		return nil, "", err
	}
	outcome, err := deps.Classify(stateDir, exe, target)
	if err != nil {
		return nil, "", err
	}
	if err := checkJournals(false); err != nil {
		return nil, "", err
	}
	if outcome == upgradeGUIAlreadyCurrent {
		matched, err := deps.PublishCurrent(stateDir, exe, target, WindowsProductPairModeUpgrade)
		if err != nil {
			return nil, "", fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: reconcile committed Product event: %w", err)
		}
		if !matched {
			return nil, "", errors.New("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: committed Product changed before event readback")
		}
		return nil, outcome, nil
	}
	if outcome != upgradeGUINewTransactionRequired {
		return nil, "", fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: unexpected Product outcome %q", outcome)
	}
	if deps.OnNewTransaction != nil {
		if err := deps.OnNewTransaction(); err != nil {
			return nil, "", fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: report new transaction before GUI exit: %w", err)
		}
	}
	lease, err := deps.ExitAndReserve(ctx, stateDir, target)
	if err != nil {
		return lease, "", err
	}
	if lease == nil {
		return nil, "", errors.New("UPGRADE_GUI_RESERVATION_HELD: no GUI absence lease was returned")
	}
	if err := checkJournals(true); err != nil {
		return lease, "", err
	}
	return lease, outcome, nil
}

var runInstallUpgradeWindowsFn = RunInstallUpgrade

// runV5UpgradeWindows wires cli.RunInstallUpgrade with the production
// rename-aside + IPC + supervisor-spawn callbacks. The caller already verified
// supervisor-intent.json is present (the routing branch's discriminator), so
// this path is the "v0.5.x → v0.5.x same-version upgrade" flow per spec
// §"Upgrade sequence".
func runV5UpgradeWindows(cmd *cobra.Command) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: resolve executable: %w", err)
	}
	target, err := setupTargetPath()
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: resolve canonical target: %w", err)
	}
	return runV5UpgradeWindowsWithPaths(cmd, exe, target)
}

// runV5UpgradeWindowsWithPaths is the internal, hermetic entry point for the
// upgrade sequence. Production resolves exe and target through the canonical
// owners above; focused tests provide an admitted GUI PE and a temporary target
// so they can reach post-staging failure paths without touching the running
// test binary or the installed product.
func runV5UpgradeWindowsWithPaths(cmd *cobra.Command, exe, target string) error {
	return runV5UpgradeWindowsWithPathsAndDeps(cmd, exe, target, upgradeGUIFencedDepsForCommand(cmd))
}

func upgradeGUIFencedDepsForCommand(cmd *cobra.Command) upgradeGUIFencedDeps {
	return upgradeGUIFencedDeps{
		Classify:       inspectWindowsProductUpgradeReceipt,
		PublishCurrent: reconcileWindowsProductReceipt,
		OnNewTransaction: func() error {
			_, err := fmt.Fprintln(cmd.ErrOrStderr(), "UPGRADE_GUI_NEW_TRANSACTION_REQUIRED: requested candidate differs from verified installed Product")
			return err
		},
		ExitAndReserve: func(ctx context.Context, stateDir, target string) (gui.OwnedSingleInstanceLease, error) {
			return exitAndReserveUpgradeGUIWithStage(ctx, stateDir, target, func() error {
				_, err := fmt.Fprintln(cmd.ErrOrStderr(), "UPGRADE_GUI_EXIT_REQUESTED: force-exiting exact GUI/tray; unsaved GUI-only work may be lost")
				return err
			})
		},
		ObservePair: func(ctx context.Context, stateDir, target string) (*upgradeGUIHeldTopology, error) {
			return observeUpgradeGUIOwnedPairHeldTopologyWithInputs(ctx, stateDir, target, productionUpgradeGUIObservationInputs())
		},
		ExitPair: func(ctx context.Context, held *upgradeGUIHeldTopology, txn *WindowsProductTxn) (gui.OwnedSingleInstanceLease, error) {
			return exitAndReserveUpgradeGUIOwnedPair(ctx, held, txn, func() error {
				_, err := fmt.Fprintln(cmd.ErrOrStderr(), "UPGRADE_GUI_EXIT_REQUESTED: force-exiting exact GUI/tray with durable GUI-owned fleet recovery pending")
				return err
			})
		},
		ObserveAfterPair: observeWindowsProductPairQuiescence,
		AfterCommitPair: func(journal windowsProductRecoveryJournalV3, release func() error) error {
			return restoreGUIOwnedForward(context.Background(), journal, release)
		},
	}
}

func runV5UpgradeWindowsWithPathsAndDeps(cmd *cobra.Command, exe, target string, guiDeps upgradeGUIFencedDeps) (retErr error) {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: resolve state-dir: %w", err)
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	optIn := upgradeGUIRequested(cmd)
	pairOptIn := upgradeGUIOwnedFleetStopRequested(cmd)
	if pairOptIn && !optIn {
		return errors.New("--stop-gui-owned-fleet-for-upgrade requires --terminate-gui-for-upgrade")
	}
	if optIn && (guiDeps.Classify == nil || guiDeps.PublishCurrent == nil || guiDeps.OnNewTransaction == nil || guiDeps.ExitAndReserve == nil) {
		return errors.New("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: GUI upgrade dependencies are incomplete")
	}
	if pairOptIn && (guiDeps.ObservePair == nil || guiDeps.ExitPair == nil || guiDeps.ObserveAfterPair == nil || guiDeps.AfterCommitPair == nil) {
		return errors.New("UPGRADE_GUI_PAIR_TOPOLOGY_UNPROVEN: GUI-owned pair dependencies are incomplete")
	}
	candidateSHA256 := ""
	if optIn {
		candidate, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{Path: exe, Role: binaryadmission.WindowsArtifactRoleProduct})
		if err != nil {
			return fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: admit requested Product before fence: %w", err)
		}
		candidateSHA256 = candidate.SHA256
	}
	fenceCtx := ctx
	if optIn {
		var cancel context.CancelFunc
		fenceCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	fence, err := api.AcquireUpgradeFence(fenceCtx, stateDir)
	if err != nil {
		if optIn {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("UPGRADE_GUI_FENCE_TIMEOUT: %w", err)
			}
			return fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: acquire upgrade fence: %w", err)
		}
		return fmt.Errorf("v0.5 upgrade: acquire upgrade transaction fence: %w", err)
	}
	var guiLease gui.OwnedSingleInstanceLease
	completionText := ""
	projectCurrentAutorunOff := func() error {
		return (&api.API{}).WithGUIOwnerControl(func(control api.GUIOwnerControl) error {
			if control.AutorunEnabled == nil || *control.AutorunEnabled {
				return nil
			}
			backend, err := autostartBackendFactoryFn()
			if err != nil {
				return err
			}
			return autostart.ProjectAutorunDisabled(control.AutorunEnabled, backend, autostart.Options{MCPHubPath: target})
		})
	}
	defer func() {
		// All later transaction/rollback/task-close defers have unwound. Apply
		// current OFF before releasing the outer exclusion or reporting success.
		if projectionErr := projectCurrentAutorunOff(); projectionErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("v0.5 upgrade: current autorun OFF projection: %w", projectionErr))
		}
		if guiLease != nil {
			if releaseErr := guiLease.ReleaseErr(); releaseErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("UPGRADE_GUI_LEASE_RELEASE_UNCONFIRMED: %w", releaseErr))
			}
		}
		if optIn {
			if releaseErr := fence.Release(); releaseErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("UPGRADE_FENCE_RELEASE_UNCONFIRMED: %w", releaseErr))
			}
		} else if retErr != nil {
			api.ReleaseAndJoin(&retErr, fence.Release, "release upgrade transaction fence")
		} else if releaseErr := fence.Release(); releaseErr != nil {
			retErr = fmt.Errorf("v0.5 upgrade: release upgrade transaction fence: %w", releaseErr)
		}
		if retErr == nil && completionText != "" {
			if optIn {
				retErr = writeUpgradeGUICompletion(cmd.OutOrStdout(), completionText)
			} else {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), completionText)
			}
		}
	}()
	if optIn {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "UPGRADE_GUI_FENCE_HELD: one upgrade fence acquired"); err != nil {
			return fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: report held fence: %w", err)
		}
	}
	legacyWindowlessTarget := scheduler.WindowsLegacyEntrypointPath(target)
	var pairHeld *upgradeGUIHeldTopology
	if pairOptIn {
		present, journalErr := inspectWindowsProductRecoveryLeaves(stateDir)
		if journalErr != nil || present {
			return fmt.Errorf("UPGRADE_GUI_PENDING_RECOVERY: pair-stop admission journal present=%v error=%v", present, journalErr)
		}
		outcome, classifyErr := guiDeps.Classify(stateDir, exe, target)
		if classifyErr != nil {
			return fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: classify Product before pair stop: %w", classifyErr)
		}
		if outcome == upgradeGUIAlreadyCurrent {
			matched, publishErr := guiDeps.PublishCurrent(stateDir, exe, target, WindowsProductPairModeUpgrade)
			if publishErr != nil || !matched {
				return fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: current Product event readback matched=%v error=%v", matched, publishErr)
			}
			completionText = string(outcome)
			return nil
		}
		if outcome != upgradeGUINewTransactionRequired {
			return fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: unexpected Product outcome %q", outcome)
		}
		if err := guiDeps.OnNewTransaction(); err != nil {
			return fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: report pair transaction: %w", err)
		}
		pairHeld, err = guiDeps.ObservePair(ctx, stateDir, target)
		if err != nil {
			return fmt.Errorf("UPGRADE_GUI_PAIR_TOPOLOGY_UNPROVEN: hold exact pair: %w", err)
		}
		if pairHeld == nil {
			return errors.New("UPGRADE_GUI_PAIR_TOPOLOGY_UNPROVEN: pair observer returned no holds")
		}
		defer func() {
			if pairHeld != nil {
				retErr = errors.Join(retErr, pairHeld.Close())
			}
		}()
	} else if optIn {
		var outcome upgradeGUIProductOutcome
		guiLease, outcome, err = runUpgradeGUIFencedPrelude(ctx, fence, stateDir, exe, target, guiDeps)
		if err != nil {
			return err
		}
		if outcome == upgradeGUIAlreadyCurrent {
			completionText = string(upgradeGUIAlreadyCurrent)
			return nil
		}
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "UPGRADE_GUI_RESERVATION_HELD: exact incumbent GUI lease reserved"); err != nil {
			return fmt.Errorf("UPGRADE_GUI_RESERVATION_HELD: report GUI reservation: %w", err)
		}
	} else {
		settled, err := reconcileWiredWindowsProductRecovery(windowsProductRecoveryRuntimeRequest(ctx, stateDir, target, legacyWindowlessTarget))
		if err != nil {
			return ordinaryWindowsProductRecoveryError(err)
		}
		if settled != nil && settled.Outcome == WindowsProductPairRecoverySettledCommit {
			return nil
		}
		if reconciled, err := reconcileWindowsProductReceipt(stateDir, exe, target, WindowsProductPairModeUpgrade); err != nil {
			return fmt.Errorf("v0.5 upgrade: reconcile committed Windows product: %w", err)
		} else if reconciled {
			return nil
		}
	}

	stagedProduct, err := stageV5UpgradeBinary(exe, target)
	if err != nil {
		if optIn {
			return fmt.Errorf("UPGRADE_GUI_PREPARE_FAILED: stage owned candidate: %w", err)
		}
		return fmt.Errorf("v0.5 upgrade: stage product beside canonical target: %w", err)
	}
	if optIn {
		defer func() {
			retErr = errors.Join(retErr, cleanupUpgradeStagedProduct(stateDir, stagedProduct, candidateSHA256))
		}()
	} else {
		defer os.Remove(stagedProduct)
	}
	deps := buildV5UpgradeDeps(target, stateDir)

	// Resolve expected daemon ports from supervisor-intent.json so the
	// post-force-kill verification (codex-r2-c-p1-8 fix) can prove no
	// zombie children survived.
	//
	// Codex round-3 Lane C P1 #3: the routing layer chose this path
	// because supervisor-intent.json existed (`os.Stat` returned
	// success). A subsequent ReadSupervisorIntent failure here is NOT
	// best-effort — it means the file the routing discriminator
	// relied on is now unreadable (corrupt JSON, EBUSY race, permission
	// drift). Returning a no-verify upgrade in that case would let the
	// new supervisor start without proving the old daemon ports
	// unbound, which is exactly the zombie-children regression the
	// codex-r2-c-p1-8 fix exists to prevent. Fail closed.
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: supervisor-intent.json present but unreadable at %s: %w (routing chose v5 upgrade because the file existed; refusing to skip post-force-kill port verification)", intentPath, err)
	}
	if intent == nil {
		return fmt.Errorf("v0.5 upgrade: supervisor-intent.json at %s decoded to nil (corrupt envelope); refusing to skip post-force-kill port verification", intentPath)
	}
	var expectedPorts []int
	var frozenPorts []windowsProductIntentPortV1
	for _, d := range intent.Daemons {
		// Resolve the EFFECTIVE port through the owner, not the raw field: a legacy
		// Port=0 row (no longer backfilled since F5's deletion) still declares a
		// manifest port that its daemon binds, and this list gates the post-
		// force-terminate unbound check (ROLLBACK_DETACHED_DAEMONS_REMAIN). Reading
		// the raw field would silently drop every legacy row from verification, so a
		// transient that survived /T and still holds the port goes undetected and the
		// new supervisor spawns a duplicate → EADDRINUSE cycle (commission fable-F1).
		if port, ok := api.EffectiveDaemonPort(d); ok && port > 0 {
			expectedPorts = append(expectedPorts, port)
			frozenPorts = append(frozenPorts, windowsProductIntentPortV1{TaskName: d.TaskName, Port: port, ArgvSHA256: windowsProductArgvSHA256(append([]string{d.Command}, d.Args...))})
		}
	}
	productTxn, err := newWindowsProductTxnFn(windowsProductTxnRequest{
		Context:              ctx,
		StateDir:             stateDir,
		ProductPath:          target,
		LegacyWindowlessPath: legacyWindowlessTarget,
		StagedProduct:        stagedProduct,
		Mode:                 WindowsProductPairModeUpgrade,
		StartSupervisor:      deps.StartSupervisor,
		RestartPrior:         deps.StartSupervisor,
		SettleSuccessor: func(string) error {
			err := deps.ForceKillSupervisor(deps.pipePath)
			if isAlreadyExitedError(err) {
				return nil
			}
			return err
		},
		WaitSupervisorReady: func(ctx context.Context, cliPath string, product binaryadmission.WindowsArtifact) error {
			candidate := UpgradeCandidateV1{
				Admission: UpgradeAdmissionLocalProduct,
				Version:   product.Version,
				Commit:    product.Commit,
				BuildDate: product.BuildDate,
				SHA256:    product.SHA256,
			}
			return deps.WaitSupervisorReady(ctx, defaultSupervisorLockReleaseTimeout, cliPath, candidate)
		},
	})
	if err != nil {
		if optIn {
			return fmt.Errorf("UPGRADE_GUI_PREPARE_FAILED: construct Product/task transaction: %w", err)
		}
		return fmt.Errorf("v0.5 upgrade: construct Windows product transaction: %w", err)
	}
	txnEntered := false
	if optIn {
		defer func() {
			if !txnEntered {
				retErr = errors.Join(retErr, productTxn.Close())
			}
		}()
	}
	if pairOptIn {
		publish := productTxn.Opts.Deps.PublishCommitted
		productTxn.Opts.Deps.PublishCommitted = func(event WindowsProductCommitEvent) (string, error) {
			digest, err := publish(event)
			if err != nil {
				return digest, err
			}
			journal := productTxn.prepared.journal
			err = guiDeps.AfterCommitPair(journal, func() error {
				if guiLease == nil {
					return nil
				}
				releaseErr := guiLease.ReleaseErr()
				if releaseErr == nil {
					guiLease = nil
				}
				return releaseErr
			})
			// The temporary lease handoff above is needed to start/prove RUNNING.
			// Reconcile OFF only after that proof returns, while the outer fence
			// remains held; preserve both the original proof and policy outcomes.
			err = errors.Join(err, projectCurrentAutorunOff())
			if err != nil {
				return digest, fmt.Errorf("%w: GUI/tray forward proof: %w", ErrWindowsProductRecoveryPending, err)
			}
			return digest, nil
		}
		productTxn.Opts.Deps.RecoverPriorSettlement = func(journal windowsProductRecoveryJournalV3) error {
			if guiDeps.RecoverPriorPair != nil {
				return guiDeps.RecoverPriorPair(journal)
			}
			return recoverGUIOwnedPriorSettlement(context.Background(), stateDir, target, journal, guiLease)
		}
		if err := prepareUpgradeGUIOwnedPairTransaction(productTxn, pairHeld); err != nil {
			return err
		}
		pairPrepared := true
		defer func() {
			if pairPrepared && retErr != nil {
				retErr = productTxn.rollback(retErr)
			}
		}()
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "UPGRADE_GUI_PREPARED: GUI-owned Product/task/receipt/fleet V3 preimage read back before exit"); err != nil {
			return fmt.Errorf("UPGRADE_GUI_PAIR_PREIMAGE_UNRECOVERABLE: report prepared preimage: %w", err)
		}
		guiLease, err = guiDeps.ExitPair(ctx, pairHeld, productTxn)
		pairHeld = nil
		if err != nil {
			return err
		}
		if guiLease == nil {
			return errors.New("UPGRADE_GUI_RELAUNCH_RACE: pair exit returned no reserved GUI lease")
		}
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "UPGRADE_GUI_RESERVATION_HELD: exact incumbent GUI lease reserved"); err != nil {
			return fmt.Errorf("UPGRADE_GUI_RESERVATION_HELD: report pair reservation: %w", err)
		}
		defer func() {
			if txnEntered {
				pairPrepared = false
			}
		}()
	} else if optIn {
		present, journalErr := inspectWindowsProductRecoveryLeaves(stateDir)
		if journalErr != nil || present {
			return fmt.Errorf("UPGRADE_GUI_JOURNAL_DRIFT_AFTER_EXIT: pre-transaction journal present=%v error=%v", present, journalErr)
		}
		outcome, receiptErr := guiDeps.Classify(stateDir, exe, target)
		if receiptErr != nil || outcome != upgradeGUINewTransactionRequired {
			return fmt.Errorf("UPGRADE_GUI_PRIOR_STATE_UNVERIFIABLE: Product outcome changed after GUI exit: outcome=%q error=%v", outcome, receiptErr)
		}
	}

	var onProductPrepared func() error
	var preparedOutputErr error
	if optIn && !pairOptIn {
		onProductPrepared = upgradeGUIPreparedStageReporter(cmd.ErrOrStderr(), &preparedOutputErr)
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "UPGRADE_GUI_TXN_ENTERED: entering existing Product transaction"); err != nil {
			return fmt.Errorf("UPGRADE_GUI_PREPARE_FAILED: report transaction entry: %w", err)
		}
	}
	txnEntered = true
	observePair := func(ctx context.Context, target string, ports []windowsProductIntentPortV1) (windowsProductQuiescenceObservationV1, error) {
		if pairOptIn {
			return guiDeps.ObserveAfterPair(ctx, stateDir, target, ports)
		}
		return observeWindowsProductPairQuiescence(ctx, stateDir, target, ports)
	}
	txnErr := runInstallUpgradeWindowsFn(ctx, UpgradeOpts{
		WindowsProduct:    productTxn,
		StateDir:          stateDir,
		GUIOwnedPending:   pairOptIn,
		OnProductPrepared: onProductPrepared,
		WindowsProductPairLifecycle: &windowsProductPairLifecycleHooks{
			FrozenPorts: frozenPorts,
			Observe: func(ctx context.Context, target string, ports []windowsProductIntentPortV1) (windowsProductQuiescenceObservationV1, error) {
				return observePair(ctx, target, ports)
			},
			Retained: &retainedGenerationLifecycleHooks{
				RequestVersion: 0,
				Observe: func(ctx context.Context, cliPath, windowlessPath string, ports []windowsProductIntentPortV1) (*retainedGenerationObservationV1, error) {
					return observeRetainedGenerationWithDeps(ctx, stateDir, cliPath, windowlessPath, ports, productionRetainedGenerationObservationDeps())
				},
				HoldSupervisor: process.HoldPIDForTermination,
				Send: func(ctx context.Context, owner api.SupervisorLockOwner, version int, cmd string, args map[string]any, timeoutMs int) (retainedGenerationIPCExchangeV1, error) {
					return sendRetainedGenerationIPC(ctx, deps.pipePath, owner, version, cmd, args, timeoutMs)
				},
				PublishSettled: retainedGenerationEventPublisher(stateDir),
			},
			Now: func() time.Time { return time.Now().UTC() },
		},
		BinaryPath:                 target,
		PipePath:                   deps.pipePath,
		Deps:                       deps,
		ExpectedPorts:              expectedPorts,
		VerifyPortsUnbound:         verifyPortsUnboundForUpgrade,
		WaitSupervisorLockReleased: deps.WaitSupervisorLockReleased,
		AdmitPrior:                 admitV5UpgradePrior,
		VerifyPrior:                verifyV5UpgradePrior,
		WithRollbackStopSettlementFence: func(ctx context.Context, critical func() error) error {
			return api.WithEmptyStopSettlementFence(ctx, filepath.Join(stateDir, "supervisor-state.json"), critical)
		},
	})
	if txnErr != nil {
		if pairOptIn && productTxn.prepared != nil && productTxn.prepared.journal.Phase == WindowsProductRecoveryCommitDecided {
			return errors.Join(fmt.Errorf("%w: committed Product requires forward GUI/tray recovery", ErrWindowsProductRecoveryPending), txnErr)
		}
		if optIn && productTxn.prepared == nil {
			return errors.Join(fmt.Errorf("UPGRADE_GUI_PREPARE_FAILED: %w", txnErr), preparedOutputErr)
		}
		return errors.Join(fmt.Errorf("v0.5 upgrade: %w", txnErr), preparedOutputErr)
	}
	if preparedOutputErr != nil {
		return preparedOutputErr
	}
	if optIn {
		completionText = "UPGRADE_GUI_TXN_SETTLED: requested Product transaction complete"
	} else {
		completionText = "v0.5 upgrade complete."
	}
	return nil
}

func retainedGenerationEventPublisher(stateDir string) func(retainedGenerationSettledEventV1) (string, error) {
	return func(event retainedGenerationSettledEventV1) (string, error) {
		portRows := make([]map[string]any, 0, len(event.Ports))
		for _, row := range event.Ports {
			portRows = append(portRows, map[string]any{
				"task_name": row.TaskName, "port": row.Port, "pid": row.PID, "started_at": row.StartedAt,
				"image_path_sha256": row.ImagePathSHA256, "argv_sha256": row.ArgvSHA256,
			})
		}
		log, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
		if err != nil {
			return "", err
		}
		defer log.Close()
		prepared, err := api.PrepareSupervisorEvent(api.SupervisorEvent{
			SchemaVersion: api.SupervisorEventSchemaVersion,
			TS:            event.SettledAtUTC.Format(time.RFC3339Nano),
			Severity:      api.SupervisorEventSeverityInfo,
			Source:        api.SupervisorEventSourceMigration,
			Event:         "windows-product-generation-quiescence-settled",
			Body: map[string]any{
				"reason_id": event.ReasonID, "observation_receipt_sha256": event.ObservationSHA256,
				"supervisor_pid": event.SupervisorPID, "supervisor_started_at": event.SupervisorStartedAt,
				"supervisor_image_path_sha256": event.SupervisorImageSHA256,
				"supervisor_command_sha256":    event.SupervisorCommandSHA256,
				"ports":                        portRows,
			},
		})
		if err != nil {
			return "", err
		}
		digest, err := log.PersistPendingVerified(prepared)
		if err != nil {
			return digest, err
		}
		_ = log.TryReplayPending()
		return digest, nil
	}
}

type retainedGenerationObservationDeps struct {
	TrySupervisorAbsent func(string) (bool, error)
	ReadSupervisorOwner func(string) (api.SupervisorLockOwner, error)
	LookupIdentity      func(int) (process.ProcessIdentity, error)
	OwnerMatchesCurrent func(int) (bool, error)
	ConfirmGUIAbsent    func(string) error
	HandleIdentity      func(int) (process.PIDHandleIdentity, error)
	ParentPID           func(int) (int, error)
	PortOwner           func(context.Context, int) (int, bool, error)
	CurrentPairReceipt  func(string) (windowsProductPreimageV1, error)
	Now                 func() time.Time
}

func productionRetainedGenerationObservationDeps() retainedGenerationObservationDeps {
	return retainedGenerationObservationDeps{
		TrySupervisorAbsent: func(lockPath string) (bool, error) {
			lock, err := api.AcquireSupervisorLockQuiet(lockPath)
			if err != nil {
				return false, nil
			}
			lock.Release()
			return true, nil
		},
		ReadSupervisorOwner: api.ReadSupervisorLockOwner,
		LookupIdentity:      processLookupIdentityFn,
		OwnerMatchesCurrent: process.ProcessOwnerMatchesCurrent,
		ConfirmGUIAbsent: func(target string) error {
			processes, err := findRunningGUIsOnTarget(api.NewAPI(), target)
			if err != nil {
				return err
			}
			if len(processes) != 0 {
				return fmt.Errorf("%d GUI owner process(es) still use the canonical target", len(processes))
			}
			return nil
		},
		HandleIdentity: process.LookupPIDHandleIdentity,
		ParentPID:      retainedGenerationParentPID,
		PortOwner:      api.LoopbackPortOwnerPIDContext,
		CurrentPairReceipt: func(path string) (windowsProductPreimageV1, error) {
			snapshot, err := snapshotProductPairFile(path)
			if err != nil {
				return windowsProductPreimageV1{}, err
			}
			return windowsProductPreimage(snapshot), nil
		},
		Now: func() time.Time { return time.Now().UTC() },
	}
}

func observeRetainedGenerationWithDeps(ctx context.Context, stateDir, cliPath, windowlessPath string, frozen []windowsProductIntentPortV1, d retainedGenerationObservationDeps) (*retainedGenerationObservationV1, error) {
	if d.ConfirmGUIAbsent == nil {
		return nil, errors.New("E_UPGRADE_GUI_OWNER_PRESENT: retained-generation GUI owner probe is unavailable")
	}
	if d.TrySupervisorAbsent == nil || d.ReadSupervisorOwner == nil || d.LookupIdentity == nil || d.OwnerMatchesCurrent == nil || d.HandleIdentity == nil || d.ParentPID == nil || d.PortOwner == nil || d.CurrentPairReceipt == nil || d.Now == nil {
		return nil, fmt.Errorf("%w: retained-generation identity dependencies are incomplete", ErrRetainedGenerationChanged)
	}
	if err := d.ConfirmGUIAbsent(cliPath); err != nil {
		return nil, fmt.Errorf("E_UPGRADE_GUI_OWNER_PRESENT: %w", err)
	}
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	absent, err := d.TrySupervisorAbsent(lockPath)
	if err != nil {
		return nil, fmt.Errorf("%w: supervisor lock absence probe: %v", ErrRetainedGenerationChanged, err)
	}
	if absent {
		return nil, nil
	}
	owner, err := d.ReadSupervisorOwner(lockPath)
	if err != nil {
		return nil, fmt.Errorf("%w: supervisor lock generation: %v", ErrRetainedGenerationChanged, err)
	}
	supervisorIdentity, err := d.LookupIdentity(owner.PID)
	if err != nil {
		return nil, fmt.Errorf("%w: supervisor WMI identity: %v", ErrRetainedGenerationChanged, err)
	}
	ownerMatch, err := d.OwnerMatchesCurrent(owner.PID)
	if err != nil || !ownerMatch {
		return nil, fmt.Errorf("%w: supervisor owner is not the current user", ErrRetainedGenerationChanged)
	}
	supervisorHandleIdentity, err := d.HandleIdentity(owner.PID)
	if err != nil {
		return nil, fmt.Errorf("%w: supervisor handle identity: %v", ErrRetainedGenerationChanged, err)
	}
	if supervisorHandleIdentity.PID != owner.PID || !cleanAbsolutePath(supervisorHandleIdentity.ExecutablePath) || supervisorHandleIdentity.StartedAt.IsZero() || supervisorHandleIdentity.StartedAt.Location() != time.UTC {
		return nil, fmt.Errorf("%w: supervisor handle identity is invalid", ErrRetainedGenerationChanged)
	}
	supervisorImagePath := supervisorHandleIdentity.ExecutablePath
	if samePath(supervisorImagePath, cliPath) {
		return nil, nil
	}
	ownerStartedUnix, ok := parseSidecarStartedAtUnix(owner.StartedAt)
	if !ok || supervisorIdentity.CreationDateUnix <= 0 || supervisorIdentity.CreationDateUnix > ownerStartedUnix || supervisorCommandLineSubcommand(supervisorIdentity.CommandLine) != "supervise" {
		return nil, fmt.Errorf("%w: supervisor WMI generation differs from lock owner", ErrRetainedGenerationChanged)
	}
	currentReceipt, err := d.CurrentPairReceipt(filepath.Join(stateDir, UpgradeReceiptSchemaV2+".json"))
	if err != nil {
		return nil, fmt.Errorf("%w: current pair receipt preimage: %v", ErrRetainedGenerationChanged, err)
	}
	observedAt := d.Now().UTC()
	rows := make([]retainedGenerationPortObservationV1, 0, len(frozen))
	for _, intent := range frozen {
		pid, bound, err := d.PortOwner(ctx, intent.Port)
		if err != nil {
			return nil, fmt.Errorf("%w: probe %s/%d: %v", ErrRetainedGenerationPortPopulationMismatch, intent.TaskName, intent.Port, err)
		}
		if !bound {
			rows = append(rows, retainedGenerationPortObservationV1{TaskName: intent.TaskName, Port: intent.Port, State: windowsProductPortUnbound})
			continue
		}
		identity, err := d.LookupIdentity(pid)
		if err != nil {
			return nil, fmt.Errorf("%w: listener %s/%d identity: %v", ErrRetainedGenerationPortPopulationMismatch, intent.TaskName, intent.Port, err)
		}
		ownerMatch, err := d.OwnerMatchesCurrent(pid)
		if err != nil || !ownerMatch {
			return nil, fmt.Errorf("%w: listener %s/%d owner is not the current user", ErrRetainedGenerationPortPopulationMismatch, intent.TaskName, intent.Port)
		}
		handleIdentity, err := d.HandleIdentity(pid)
		if err != nil {
			return nil, fmt.Errorf("%w: listener %s/%d handle identity: %v", ErrRetainedGenerationChanged, intent.TaskName, intent.Port, err)
		}
		if handleIdentity.PID != pid || !cleanAbsolutePath(handleIdentity.ExecutablePath) || handleIdentity.StartedAt.IsZero() || handleIdentity.StartedAt.Location() != time.UTC {
			return nil, fmt.Errorf("%w: listener %s/%d handle identity is invalid", ErrRetainedGenerationChanged, intent.TaskName, intent.Port)
		}
		imagePath := handleIdentity.ExecutablePath
		parentPID, err := d.ParentPID(pid)
		if err != nil {
			return nil, fmt.Errorf("%w: listener %s/%d parent: %v", ErrRetainedGenerationParentMismatch, intent.TaskName, intent.Port, err)
		}
		state := windowsProductPortStateV1("")
		switch {
		case samePath(imagePath, cliPath):
			state = windowsProductPortCanonicalOwned
		case samePath(imagePath, supervisorImagePath):
			state = windowsProductPortRetainedOwned
		default:
			return nil, fmt.Errorf("%w: listener %s/%d image differs from canonical and retained paths", ErrRetainedGenerationImageMismatch, intent.TaskName, intent.Port)
		}
		rows = append(rows, retainedGenerationPortObservationV1{
			TaskName: intent.TaskName, Port: intent.Port, State: state, PID: pid,
			StartedAt: handleIdentity.StartedAt.UTC().Format(time.RFC3339Nano),
			ParentPID: parentPID, ObservedImagePath: imagePath,
			ArgvSHA256: windowsProductArgvSHA256(process.TokenizeWindowsCommandLine(identity.CommandLine)),
		})
	}
	observation := &retainedGenerationObservationV1{
		CanonicalCLIPath: cliPath, CanonicalWindowlessPath: windowlessPath,
		SupervisorPID: owner.PID, SupervisorStartedAt: owner.StartedAt, SupervisorImagePath: supervisorImagePath,
		SupervisorCommandSHA256: windowsProductArgvSHA256(process.TokenizeWindowsCommandLine(supervisorIdentity.CommandLine)),
		GUIOwner:                windowsProductGUIOwnerConfirmedAbsent,
		CurrentPairReceipt:      currentReceipt, Ports: rows, ObservedAtUTC: observedAt,
	}
	observation.ReceiptID = "r-" + retainedGenerationObservationSHA256(*observation)
	if err := validateRetainedGenerationObservation(*observation, frozen, cliPath, windowlessPath); err != nil {
		return nil, err
	}
	return observation, nil
}

func windowsProductArgvSHA256(argv []string) string {
	raw, _ := json.Marshal(argv)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func retainedGenerationParentPID(pid int) (int, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return 0, err
	}
	for {
		if int(entry.ProcessID) == pid {
			return int(entry.ParentProcessID), nil
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return 0, process.ErrProcessNotFound
			}
			return 0, err
		}
	}
}

func sendRetainedGenerationIPC(ctx context.Context, pipePath string, owner api.SupervisorLockOwner, version int, cmdName string, args map[string]any, timeoutMs int) (retainedGenerationIPCExchangeV1, error) {
	if version != 0 && version != 1 {
		return retainedGenerationIPCExchangeV1{}, ErrRetainedGenerationProtocolUnsupported
	}
	if cmdName != "quiesce-timers" && cmdName != "exit" {
		return retainedGenerationIPCExchangeV1{}, ErrRetainedGenerationProtocolUnsupported
	}
	timeout := time.Duration(timeoutMs+5000) * time.Millisecond
	conn, err := winio.DialPipe(pipePath, durPtr(timeout))
	if err != nil {
		return retainedGenerationIPCExchangeV1{}, err
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(timeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return retainedGenerationIPCExchangeV1{}, err
	}
	line, err := readLine(conn, 4096)
	if err != nil {
		return retainedGenerationIPCExchangeV1{}, err
	}
	var helloEnvelope struct {
		Hello api.IPCHello `json:"hello"`
	}
	if err := json.Unmarshal(line, &helloEnvelope); err != nil {
		return retainedGenerationIPCExchangeV1{}, fmt.Errorf("decode retained-generation hello: %w", err)
	}
	if helloEnvelope.Hello.Version != 1 || !api.ValidateHandshake(helloEnvelope.Hello, owner) {
		return retainedGenerationIPCExchangeV1{Hello: helloEnvelope.Hello}, ErrRetainedGenerationProtocolUnsupported
	}
	request := api.IPCRequest{Version: version, ID: time.Now().UnixNano(), Cmd: cmdName, Args: args}
	if err := api.ValidateRequestEnvelope(request); err != nil {
		return retainedGenerationIPCExchangeV1{}, err
	}
	if err := writeFrame(conn, request); err != nil {
		return retainedGenerationIPCExchangeV1{}, err
	}
	for {
		response, err := readFrame(conn)
		exchange := retainedGenerationIPCExchangeV1{Request: request, Hello: helloEnvelope.Hello, Response: response}
		if err != nil {
			return exchange, err
		}
		if response.ID != request.ID || !response.OK || response.Error != nil {
			return exchange, ErrRetainedGenerationProtocolUnsupported
		}
		if response.Final {
			return exchange, nil
		}
	}
}

func observeWindowsProductPairQuiescence(ctx context.Context, stateDir, target string, frozen []windowsProductIntentPortV1) (windowsProductQuiescenceObservationV1, error) {
	guiProcesses, err := findRunningGUIsOnTarget(api.NewAPI(), target)
	if err != nil {
		return windowsProductQuiescenceObservationV1{}, fmt.Errorf("E_UPGRADE_GUI_OWNER_PRESENT: GUI owner absence is unverified: %w", err)
	}
	if len(guiProcesses) != 0 {
		return windowsProductQuiescenceObservationV1{}, fmt.Errorf("E_UPGRADE_GUI_OWNER_PRESENT: %d GUI owner process(es) still use the canonical target", len(guiProcesses))
	}

	lockPath := filepath.Join(stateDir, "supervisor.lock")
	lock, lockErr := api.AcquireSupervisorLockQuiet(lockPath)
	if lockErr == nil {
		lock.Release()
		ports, err := observeWindowsProductIntentPorts(ctx, target, frozen, true)
		if err != nil {
			return windowsProductQuiescenceObservationV1{}, err
		}
		return windowsProductQuiescenceObservationV1{
			Supervisor:             windowsProductSupervisorSettlementV1{State: windowsProductSupervisorKnownAbsent},
			Ports:                  ports,
			GUIOwner:               windowsProductGUIOwnerConfirmedAbsent,
			SupervisorLockReleased: true,
		}, nil
	}

	owner, err := api.ReadSupervisorLockOwner(lockPath)
	if err != nil {
		return windowsProductQuiescenceObservationV1{}, fmt.Errorf("supervisor lock is held or unavailable but its generation is unverified: %w", err)
	}
	if owner.PID <= 0 || strings.TrimSpace(owner.StartedAt) == "" {
		return windowsProductQuiescenceObservationV1{}, errors.New("supervisor lock owner generation is invalid")
	}
	ports, err := observeWindowsProductIntentPorts(ctx, target, frozen, false)
	if err != nil {
		return windowsProductQuiescenceObservationV1{}, err
	}
	return windowsProductQuiescenceObservationV1{
		Supervisor: windowsProductSupervisorSettlementV1{
			State:      windowsProductSupervisorGracefullySettled,
			Generation: &windowsProductSupervisorGenerationV1{PID: owner.PID, StartedAt: owner.StartedAt, IPCRequestVersion: 0},
		},
		Ports:    ports,
		GUIOwner: windowsProductGUIOwnerConfirmedAbsent,
	}, nil
}

func observeWindowsProductIntentPorts(ctx context.Context, target string, frozen []windowsProductIntentPortV1, requireUnbound bool) ([]windowsProductPortSettlementV1, error) {
	rows, _, err := observeWindowsProductIntentPortsWithInputs(ctx, target, frozen, requireUnbound, api.LoopbackPortOwnerPIDContext, process.LookupPIDHandleIdentity)
	return rows, err
}

// Exact native anchors are local observation data, not persisted settlements or
// termination authority. Every image-only sibling uses this one ordered loop.
func observeWindowsProductIntentPortsWithInputs(ctx context.Context, target string, frozen []windowsProductIntentPortV1, requireUnbound bool, portOwner func(context.Context, int) (int, bool, error), nativeIdentity func(int) (process.PIDHandleIdentity, error)) ([]windowsProductPortSettlementV1, []process.PIDHandleIdentity, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if portOwner == nil || nativeIdentity == nil {
		return nil, nil, errors.New("image-only observation inputs unavailable")
	}
	rows := make([]windowsProductPortSettlementV1, 0, len(frozen))
	anchors := make([]process.PIDHandleIdentity, 0, len(frozen))
	for _, intent := range frozen {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		pid, bound, err := portOwner(ctx, intent.Port)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, errors.Join(err, ctxErr)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("probe frozen intent port %s/%d: %w", intent.TaskName, intent.Port, err)
		}
		if !bound {
			rows = append(rows, windowsProductPortSettlementV1{TaskName: intent.TaskName, Port: intent.Port, PreState: windowsProductPortUnbound, PostUnbound: requireUnbound})
			anchors = append(anchors, process.PIDHandleIdentity{})
			continue
		}
		if requireUnbound {
			return nil, nil, fmt.Errorf("known-absent supervisor has bound frozen intent port %s/%d owned by PID %d", intent.TaskName, intent.Port, pid)
		}
		if pid <= 0 {
			return nil, nil, fmt.Errorf("frozen intent port %s/%d returned an invalid owner PID %d", intent.TaskName, intent.Port, pid)
		}
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		identity, err := nativeIdentity(pid)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, errors.Join(err, ctxErr)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("resolve frozen intent port %s/%d owner PID %d: %w", intent.TaskName, intent.Port, pid, err)
		}
		if identity.PID != pid || identity.StartedAt.IsZero() {
			return nil, nil, fmt.Errorf("frozen intent port %s/%d owner generation unavailable", intent.TaskName, intent.Port)
		}
		if !samePath(identity.ExecutablePath, target) {
			return nil, nil, fmt.Errorf("frozen intent port %s/%d owner PID %d image %s differs from canonical target %s", intent.TaskName, intent.Port, pid, identity.ExecutablePath, target)
		}
		rows = append(rows, windowsProductPortSettlementV1{TaskName: intent.TaskName, Port: intent.Port, PreState: windowsProductPortCanonicalOwned, PrePID: pid})
		anchors = append(anchors, identity)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return rows, anchors, nil
}

func admitV5UpgradeCandidate(path string) (UpgradeCandidateV1, error) {
	if err := binaryadmission.AdmitWindowsGUI(path); err != nil {
		return UpgradeCandidateV1{}, fmt.Errorf("admit Windows product PE: %w", err)
	}
	version, commit, buildDate, err := legacyRuntimeUpgradeMetadata()
	if err != nil {
		return UpgradeCandidateV1{}, err
	}
	hash, err := hashFile(path)
	if err != nil {
		return UpgradeCandidateV1{}, fmt.Errorf("hash staged product binary: %w", err)
	}
	return UpgradeCandidateV1{
		Admission: UpgradeAdmissionLocalProduct,
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
		SHA256:    hex.EncodeToString(hash),
	}, nil
}

// legacyRuntimeUpgradeMetadata is confined to the historical single-image
// compatibility control path. New Windows product-pair admission never calls
// it and derives all receipt identity from the two candidate PE resources.
func legacyRuntimeUpgradeMetadata() (version, commit, buildDate string, err error) {
	version, commit, buildDate = buildinfo.Get()
	for _, field := range []struct{ name, value string }{
		{name: "version", value: version}, {name: "commit", value: commit}, {name: "build_date", value: buildDate},
	} {
		trimmed := strings.TrimSpace(field.value)
		if trimmed == "" || strings.EqualFold(trimmed, "dev") || strings.EqualFold(trimmed, "unknown") {
			return "", "", "", fmt.Errorf("admit legacy runtime upgrade: %s is placeholder %q", field.name, field.value)
		}
	}
	return version, commit, buildDate, nil
}

func verifyV5UpgradeCanonical(path string, candidate UpgradeCandidateV1) error {
	if err := binaryadmission.AdmitWindowsGUI(path); err != nil {
		return fmt.Errorf("re-admit promoted Windows product PE: %w", err)
	}
	hash, err := hashFile(path)
	if err != nil {
		return fmt.Errorf("hash promoted canonical binary: %w", err)
	}
	if got := hex.EncodeToString(hash); got != candidate.SHA256 {
		return fmt.Errorf("canonical SHA-256 mismatch: got %s want %s", got, candidate.SHA256)
	}
	return nil
}

func admitV5UpgradePrior(path string) (string, error) {
	if err := binaryadmission.AdmitWindowsUpgradePrior(path); err != nil {
		return "", err
	}
	hash, err := hashFile(path)
	if err != nil {
		return "", fmt.Errorf("hash prior canonical binary: %w", err)
	}
	return hex.EncodeToString(hash), nil
}

func verifyV5UpgradePrior(path, expectedSHA256 string) error {
	if err := binaryadmission.AdmitWindowsUpgradePrior(path); err != nil {
		return err
	}
	hash, err := hashFile(path)
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(hash); got != expectedSHA256 {
		return fmt.Errorf("prior SHA-256 mismatch: got %s want %s", got, expectedSHA256)
	}
	return nil
}

// stageV5UpgradeBinary copies the running candidate into the canonical target
// directory before rename-aside. MoveFileEx cannot move a file across Windows
// volumes, while RenameAsideReplace's contract requires newSrc to be a sibling
// staged by the caller. copyExe provides the existing tempfile+atomic-rename
// pipeline, so a failed copy never exposes a partial .new file.
func stageV5UpgradeBinary(exe, target string) (string, error) {
	staged := target + ".new"
	if err := copySingleBinaryPlatformArtifact(exe, staged); err != nil {
		return "", err
	}
	return staged, nil
}

func cleanupUpgradeStagedProduct(stateDir, staged, expectedSHA256 string) error {
	present, err := inspectWindowsProductRecoveryLeaves(stateDir)
	if err != nil {
		return fmt.Errorf("UPGRADE_GUI_STAGE_CLEANUP_UNCONFIRMED: journal state unknown; preserve staged Product: %w", err)
	}
	if present {
		return nil
	} // A durable Product recovery journal may need the staged image.
	info, err := os.Lstat(staged)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("UPGRADE_GUI_STAGE_CLEANUP_UNCONFIRMED: staged Product identity unknown: %v", err)
	}
	actual, err := hashFile(staged)
	if err != nil || expectedSHA256 == "" || hex.EncodeToString(actual) != expectedSHA256 {
		return fmt.Errorf("UPGRADE_GUI_STAGE_CLEANUP_UNCONFIRMED: staged Product SHA-256 changed: %v", err)
	}
	if err := os.Remove(staged); err != nil {
		return fmt.Errorf("UPGRADE_GUI_STAGE_CLEANUP_UNCONFIRMED: remove exact staged Product: %w", err)
	}
	return nil
}

// buildV5UpgradeDeps constructs the production v5UpgradeDeps for the
// `mcphub install --upgrade` cold-restart flow.
//
// SEC-F1: the IPC dial path is the SID-based canonical resolver
// api.SupervisorIPCAddress(stateDir) — the SAME pipe the supervisor LISTENS
// on (supervise_pipe_windows.go → api.SupervisorIPCAddress) and the same pipe
// the status/exit clients dial. The previous wiring built
// `\\.\pipe\mcphub-supervisor-<USERNAME>` via superviseIPCPipePath, but the
// listener keys the pipe NAME off the kernel-authoritative user SID
// (S-1-5-21-…), not the USERNAME env var (PR #212 r3 SID-consistency
// migration). USERNAME ≠ SID, so every quiesce-timers/exit{graceful} handshake
// dialed a pipe no supervisor listened on, timed out, and fell through to the
// force-kill fallback — bypassing the graceful drain and opening the
// orphan-daemon window the quiesce path exists to avoid (it surfaced in the
// field as "supervisor won't restart after install --upgrade; recovery needs
// manual schtasks /Run").
//
// stateDir is passed through so the test-isolation discriminator
// (api.EnableSupervisorIPCTestPipeIsolation) can redirect the dial onto a per-
// test pipe; production ignores the arg and always derives the SID.
func buildV5UpgradeDeps(canonicalTarget, stateDir string) *v5UpgradeDeps {
	return &v5UpgradeDeps{
		// Force-kill identity is transaction-bound to the canonical target,
		// never to the updater image returned by os.Executable.
		exePath:             canonicalTarget,
		supervisorLockDir:   filepath.Join(stateDir, "supervisor.lock"),
		pipePath:            api.SupervisorIPCAddress(stateDir),
		readSupervisorOwner: api.ReadSupervisorLockOwner,
		sendIPC:             sendIPCWithResponse,
		sendIPCPeer:         sendIPCWithResponseForPeer,
	}
}

func windowsProductPairRecoveryRuntimeRequest(ctx context.Context, stateDir, cliPath, windowlessPath string) windowsProductPairRecoveryRequest {
	deps := buildV5UpgradeDeps(cliPath, stateDir)
	return windowsProductPairRecoveryRequest{
		Context:        ctx,
		StateDir:       stateDir,
		CLIPath:        cliPath,
		WindowlessPath: windowlessPath,
		SettleSuccessor: func(string) error {
			err := deps.ForceKillSupervisor(deps.pipePath)
			if isAlreadyExitedError(err) {
				return nil
			}
			return err
		},
		ObservePriorReady: func(ctx context.Context, binaryPath, sha256 string) (bool, error) {
			ready, err := deps.probeUpgradeReadyOnce(binaryPath, UpgradeCandidateV1{SHA256: sha256})
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, process.ErrProcessNotFound) {
				return false, nil
			}
			return ready, err
		},
		RestartPrior: deps.StartSupervisor,
		WaitPriorReady: func(ctx context.Context, binaryPath, sha256 string) error {
			return deps.WaitSupervisorReady(ctx, defaultSupervisorLockReleaseTimeout, binaryPath, UpgradeCandidateV1{SHA256: sha256})
		},
	}
}

type pendingRuntimeCause string

const (
	pendingRuntimeUnavailable pendingRuntimeCause = "CURRENT_TARGET_UNAVAILABLE"
	pendingRuntimeInvalid     pendingRuntimeCause = "CURRENT_TARGET_INVALID"
	pendingRuntimeChanged     pendingRuntimeCause = "CURRENT_INTENT_CHANGED"
	pendingRuntimeMismatch    pendingRuntimeCause = "CURRENT_TARGET_MISMATCH"
	pendingRuntimeMaterial    pendingRuntimeCause = "PRIOR_MATERIAL_MISMATCH"
	pendingRuntimeReadiness   pendingRuntimeCause = "PRIOR_READINESS_FAILED"
	pendingRuntimeHistorical  pendingRuntimeCause = "HISTORICAL_COHORT_UNCONFIRMED"
	pendingRuntimeCleanup     pendingRuntimeCause = "RUNTIME_CLEANUP_FAILED"
)

type pendingRuntimeReceiving struct {
	intentSHA256, stopSHA256, guiSHA256 string
	generation                          uint64
	observedAt                          time.Time
}

func (r *pendingRuntimeReceiving) projection() string {
	if r == nil || !validLowerHexSHA256(r.intentSHA256) || !validLowerHexSHA256(r.stopSHA256) || !validLowerHexSHA256(r.guiSHA256) || r.observedAt.IsZero() {
		return "; runtime_basis=current-intent; runtime_snapshot=UNAVAILABLE"
	}
	return fmt.Sprintf("; runtime_basis=current-intent; runtime_intent_sha256=%s; runtime_stop_sha256=%s; runtime_gui_sha256=%s; runtime_intent_generation=%d; runtime_observed_at=%s", r.intentSHA256, r.stopSHA256, r.guiSHA256, r.generation, r.observedAt.UTC().Format(time.RFC3339Nano))
}

type pendingRuntimeError struct {
	kind      pendingRuntimeCause
	receiving *pendingRuntimeReceiving
	cause     error
}

func (e *pendingRuntimeError) Error() string {
	return "E_WINDOWS_PRODUCT_RECOVERY_PENDING: " + currentPendingRuntimeStage + pendingRuntimeProjection(e, e.receiving)
}
func (e *pendingRuntimeError) GoString() string { return e.Error() }
func (e *pendingRuntimeError) Unwrap() error {
	return errors.Join(ErrWindowsProductRecoveryPending, e.cause)
}

// Closed cause values are standard errors.Is targets; matching traverses every
// retained Join branch instead of selecting the first error of this type.
func (kind pendingRuntimeCause) Error() string { return string(kind) }
func (e *pendingRuntimeError) Is(target error) bool {
	kind, ok := target.(pendingRuntimeCause)
	return ok && e != nil && e.kind == kind
}

func pendingRuntimeFailure(kind pendingRuntimeCause, receiving *pendingRuntimeReceiving, cause error) error {
	return &pendingRuntimeError{kind: kind, receiving: receiving, cause: cause}
}

func pendingRuntimeStopsRecovery(err error) bool {
	return errors.Is(err, pendingRuntimeChanged) || errors.Is(err, pendingRuntimeCleanup)
}

// The acquiring owner retains the primary first and classifies actual cleanup
// before any higher-level wrapper can obscure its fatal meaning.
func pendingRuntimeCloseError(primary error, receiving *pendingRuntimeReceiving, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	if !errors.Is(cleanup, pendingRuntimeCleanup) {
		cleanup = pendingRuntimeFailure(pendingRuntimeCleanup, receiving, cleanup)
	}
	return errors.Join(primary, cleanup)
}

type pendingRuntimeExpectation struct {
	intent     *api.SupervisorRuntimeIntent
	receiving  *pendingRuntimeReceiving
	now        func() time.Time
	observe    func(context.Context, time.Time) (*api.SupervisorRuntimeIntent, error)
	observeGUI func() (api.GUIOwnerControl, error)
}

func pendingGUIControlSnapshot() (api.GUIOwnerControl, error) {
	var control api.GUIOwnerControl
	err := api.NewAPI().WithGUIOwnerControl(func(current api.GUIOwnerControl) error {
		control = current
		if current.AutorunEnabled != nil {
			enabled := *current.AutorunEnabled
			control.AutorunEnabled = &enabled
		}
		return nil
	})
	return control, err
}

func capturePendingRuntimeExpectation(ctx context.Context, now func() time.Time, observe func(context.Context, time.Time) (*api.SupervisorRuntimeIntent, error), observeGUI func() (api.GUIOwnerControl, error), receiving *pendingRuntimeReceiving) (*pendingRuntimeExpectation, error) {
	if ctx == nil || now == nil || observe == nil || observeGUI == nil || receiving == nil {
		return nil, pendingRuntimeFailure(pendingRuntimeUnavailable, receiving, nil)
	}
	// Settings is observed and released before acquiring intent; no lock spans IPC.
	control, err := observeGUI()
	if err != nil {
		return nil, pendingRuntimeFailure(pendingRuntimeUnavailable, receiving, err)
	}
	intent, err := observe(ctx, now())
	if errors.Is(err, api.ErrSupervisorRuntimeIntentUnavailable) {
		return nil, pendingRuntimeFailure(pendingRuntimeUnavailable, receiving, err)
	}
	if err != nil || intent == nil {
		return nil, pendingRuntimeFailure(pendingRuntimeInvalid, receiving, err)
	}
	if !validLowerHexSHA256(intent.SHA256) || !validLowerHexSHA256(intent.StopSHA256) || intent.Ports == nil || intent.ObservedAt.IsZero() {
		return nil, pendingRuntimeFailure(pendingRuntimeInvalid, receiving, nil)
	}
	guiRaw, err := json.Marshal(control)
	if err != nil {
		return nil, pendingRuntimeFailure(pendingRuntimeInvalid, receiving, err)
	}
	guiHash := sha256.Sum256(guiRaw)
	*receiving = pendingRuntimeReceiving{intentSHA256: intent.SHA256, stopSHA256: intent.StopSHA256, guiSHA256: hex.EncodeToString(guiHash[:]), generation: intent.Generation, observedAt: intent.ObservedAt}
	if err := control.ValidateAutomaticStart(); err != nil {
		return nil, pendingRuntimeFailure(pendingRuntimeMismatch, receiving, err)
	}
	return &pendingRuntimeExpectation{intent: intent, receiving: receiving, now: now, observe: observe, observeGUI: observeGUI}, nil
}

func (e *pendingRuntimeExpectation) validate(ctx context.Context) error {
	if e == nil || e.intent == nil || e.receiving == nil {
		return pendingRuntimeFailure(pendingRuntimeUnavailable, nil, nil)
	}
	current, err := capturePendingRuntimeExpectation(ctx, e.now, e.observe, e.observeGUI, new(pendingRuntimeReceiving))
	if err != nil {
		return pendingRuntimeFailure(pendingRuntimeChanged, e.receiving, err)
	}
	if e.receiving.intentSHA256 != current.receiving.intentSHA256 || e.receiving.stopSHA256 != current.receiving.stopSHA256 || e.receiving.guiSHA256 != current.receiving.guiSHA256 || e.receiving.generation != current.receiving.generation {
		return pendingRuntimeFailure(pendingRuntimeChanged, e.receiving, nil)
	}
	return nil
}

func (e *pendingRuntimeExpectation) validateFleet(observed []windowsProductIntentPortV1, owners []upgradeGUIPortOwner, supervisorPID int) error {
	if e == nil || e.receiving == nil {
		return pendingRuntimeFailure(pendingRuntimeUnavailable, nil, nil)
	}
	if e.intent == nil || supervisorPID <= 0 || len(e.intent.Ports) != len(observed) || len(observed) != len(owners) {
		return pendingRuntimeFailure(pendingRuntimeMismatch, e.receiving, nil)
	}
	want := make(map[string]api.SupervisorFrozenPort, len(e.intent.Ports))
	for _, row := range e.intent.Ports {
		want[row.TaskName] = row
	}
	for i, row := range observed {
		expected, ok := want[row.TaskName]
		owner := owners[i]
		if !ok || expected.Port != row.Port || expected.ArgvSHA256 != row.ArgvSHA256 || owner.TaskName != row.TaskName || owner.Port != row.Port {
			return pendingRuntimeFailure(pendingRuntimeMismatch, e.receiving, nil)
		}
		if e.intent.Stopped[row.TaskName] {
			if owner.PID != 0 {
				return pendingRuntimeFailure(pendingRuntimeMismatch, e.receiving, nil)
			}
		} else if owner.PID <= 0 || owner.StartedAt.IsZero() || owner.ParentPID != supervisorPID || owner.ArgvSHA256 != row.ArgvSHA256 {
			return pendingRuntimeFailure(pendingRuntimeMismatch, e.receiving, nil)
		}
		delete(want, row.TaskName)
	}
	if len(want) != 0 {
		return pendingRuntimeFailure(pendingRuntimeMismatch, e.receiving, nil)
	}
	return nil
}

// The existing query/synchronize owner separates failure to acquire from
// identity or close failure. Only original OpenProcess absence with no acquired
// resource is absence; a close error can never be reclassified as process exit.
func lookupPendingHistoricalIdentity(pid int) (process.PIDHandleIdentity, error) {
	return lookupPendingHistoricalIdentityWithHold(pid, process.HoldPIDForObservation)
}

func lookupPendingHistoricalIdentityWithHold(pid int, hold func(int) (process.HeldPIDObservation, error)) (process.PIDHandleIdentity, error) {
	if pid <= 0 || hold == nil {
		return process.PIDHandleIdentity{}, errors.New("historical PID/observation owner unavailable")
	}
	held, err := hold(pid)
	if held == nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return process.PIDHandleIdentity{}, process.ErrProcessNotFound
		}
		return process.PIDHandleIdentity{}, errors.Join(err, errors.New("historical query handle unavailable"))
	}
	if err != nil {
		return process.PIDHandleIdentity{}, pendingRuntimeCloseError(err, nil, held.Close())
	}
	identity, identityErr := held.Identity()
	closeErr := held.Close()
	if identityErr != nil || closeErr != nil {
		return process.PIDHandleIdentity{}, pendingRuntimeCloseError(identityErr, nil, closeErr)
	}
	return identity, nil
}

// An old port becoming free is not a process-closure proof, nor is a later
// occupant a stop target. A reused historical daemon PID without creation
// evidence remains unknown. Only actual absent PIDs or the genuinely observed
// original supervisor/current managed cohort can discharge these obligations.
func validatePendingHistoricalCohort(ctx context.Context, journal windowsProductRecoveryJournalV3, supervisor api.SupervisorLockOwner, owners []upgradeGUIPortOwner, lookup func(int) (process.PIDHandleIdentity, error)) error {
	if ctx == nil || journal.PreSettlement == nil || lookup == nil {
		return errors.New("historical cohort observation unavailable")
	}
	// The original owner already acquired and bound closure before promotion.
	// Reuse that receipt; later PID/port reuse cannot invalidate completed work.
	if journal.PriorFleetReaped {
		if err := validateWindowsProductRecoveryJournalV3(journal); err != nil {
			return err
		}
		return nil
	}
	original := journal.PreSettlement.Supervisor.Generation
	if original == nil {
		return errors.New("historical supervisor generation unavailable")
	}
	originalStarted, parseErr := time.Parse(time.RFC3339Nano, original.StartedAt)
	if parseErr != nil || originalStarted.IsZero() {
		return errors.New("historical supervisor creation time unavailable")
	}
	for _, row := range journal.PreSettlement.Ports {
		if err := ctx.Err(); err != nil {
			return err
		}
		if row.PreState == windowsProductPortUnbound && row.PrePID == 0 {
			continue
		}
		if row.PreState != windowsProductPortCanonicalOwned || row.PrePID <= 0 {
			return errors.New("historical port state unavailable")
		}
		identity, err := lookup(row.PrePID)
		if errors.Is(err, process.ErrProcessNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if identity.PID != row.PrePID || identity.StartedAt.IsZero() || identity.ExecutablePath == "" {
			return errors.New("historical daemon native identity unavailable")
		}
		matched := false
		if supervisor.PID == original.PID && supervisor.StartedAt == original.StartedAt {
			for _, owner := range owners {
				if owner.TaskName == row.TaskName && owner.Port == row.Port && owner.PID == identity.PID && owner.ParentPID == supervisor.PID && owner.StartedAt.Equal(identity.StartedAt) && samePath(identity.ExecutablePath, journal.Product.Target) {
					matched = true
					break
				}
			}
		}
		if !matched {
			return errors.New("historical daemon generation or closure unconfirmed")
		}
	}
	if supervisor.PID == original.PID && supervisor.StartedAt == original.StartedAt {
		return nil
	}
	identity, err := lookup(original.PID)
	if errors.Is(err, process.ErrProcessNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if identity.PID != original.PID || identity.StartedAt.IsZero() || identity.ExecutablePath == "" {
		return errors.New("historical supervisor native identity unavailable")
	}
	if !identity.StartedAt.Equal(originalStarted) {
		return nil
	}
	return errors.New("historical supervisor generation remains")
}

func windowsProductRecoveryRuntimeRequest(ctx context.Context, stateDir, productPath, legacyWindowlessPath string) windowsProductRecoveryRequest {
	deps := buildV5UpgradeDeps(productPath, stateDir)
	legacy := windowsProductPairRecoveryRuntimeRequest(ctx, stateDir, productPath, legacyWindowlessPath)
	return windowsProductRecoveryRequest{
		Context: ctx, StateDir: stateDir, ProductPath: productPath, PendingRuntime: new(pendingRuntimeReceiving),
		LegacyWindowlessPath: legacyWindowlessPath, Legacy: legacy,
		PreparePendingRuntime: func(ctx context.Context, journal windowsProductRecoveryJournalV3, receiving *pendingRuntimeReceiving) (func(context.Context, windowsProductRecoveryJournalV3) error, error) {
			expectation, err := capturePendingRuntimeExpectation(ctx, time.Now, func(ctx context.Context, now time.Time) (*api.SupervisorRuntimeIntent, error) {
				return api.ObserveSupervisorRuntimeIntent(ctx, filepath.Join(stateDir, "supervisor-intent.json"), now)
			}, pendingGUIControlSnapshot, receiving)
			if err != nil {
				return nil, err
			}
			if journal.PreSettlement.GUIOwner != windowsProductGUIOwnerConfirmedPresent {
				return nil, pendingRuntimeFailure(pendingRuntimeMismatch, receiving, errors.New("current runtime recovery requires its GUI-owned settlement owner"))
			}
			return func(ctx context.Context, journal windowsProductRecoveryJournalV3) error {
				return recoverGUIOwnedPriorSettlementForExpectation(ctx, stateDir, productPath, journal, nil, expectation)
			}, nil
		},
		Deps: windowsProductRecoveryDeps{PublishCommitted: func(event WindowsProductCommitEvent) (string, error) {
			digest, err := windowsProductEventPublisher(stateDir)(event)
			if err != nil {
				return digest, err
			}
			raw, err := os.ReadFile(filepath.Join(stateDir, WindowsProductRecoveryFileLeafV3))
			if err != nil {
				return digest, err
			}
			journal, err := decodeWindowsProductRecoveryJournalV3(raw)
			if err != nil {
				return digest, err
			}
			if journal.PreSettlement != nil && journal.PreSettlement.GUIOwner == windowsProductGUIOwnerConfirmedPresent {
				if err := restoreGUIOwnedForward(ctx, journal, nil); err != nil {
					return digest, err
				}
			}
			return digest, nil
		}},
		ReconcileCommitted: func(journal windowsProductRecoveryJournalV3) error {
			matched, err := reconcileWindowsProductReceipt(stateDir, productPath, productPath, journal.Mode)
			if err != nil || !matched {
				return fmt.Errorf("committed Product receipt no longer matches: matched=%v error=%v", matched, err)
			}
			if journal.PreSettlement != nil && journal.PreSettlement.GUIOwner == windowsProductGUIOwnerConfirmedPresent {
				return restoreGUIOwnedForward(ctx, journal, nil)
			}
			return nil
		},
		RecoverPriorSettlement: func(ctx context.Context, journal windowsProductRecoveryJournalV3) error {
			if journal.Mode != WindowsProductPairModeUpgrade || journal.PreSettlement == nil {
				return nil
			}
			if journal.PreSettlement.GUIOwner == windowsProductGUIOwnerConfirmedPresent {
				return recoverGUIOwnedPriorSettlement(ctx, stateDir, productPath, journal, nil)
			}
			frozen := expandWindowsProductFrozenPorts(journal.FrozenPorts)
			current, err := observeWindowsProductPairQuiescence(ctx, stateDir, productPath, frozen)
			if err != nil {
				return err
			}
			pre := journal.PreSettlement
			switch pre.Supervisor.State {
			case windowsProductSupervisorKnownAbsent:
				if err := validateWindowsProductQuiescenceObservation(current, frozen); err != nil ||
					current.Supervisor.State != windowsProductSupervisorKnownAbsent {
					return fmt.Errorf("%w: prior known-absent state changed", ErrWindowsProductRecoveryIdentity)
				}
				return nil
			case windowsProductSupervisorGracefullySettled:
				recorded := pre.Supervisor.Generation
				if recorded == nil {
					return fmt.Errorf("%w: recorded prior generation is missing", ErrWindowsProductRecoveryIdentity)
				}
				if current.Supervisor.State == windowsProductSupervisorGracefullySettled {
					live := current.Supervisor.Generation
					if live == nil || live.PID != recorded.PID || live.StartedAt != recorded.StartedAt {
						return fmt.Errorf("%w: a different supervisor generation is live", ErrWindowsProductRecoveryIdentity)
					}
					held, err := process.HoldPIDForTermination(recorded.PID)
					if err != nil {
						return fmt.Errorf("%w: hold recorded prior generation: %v", ErrWindowsProductRecoveryIdentity, err)
					}
					defer held.Close()
					if err := held.VerifyIdentity(process.PIDIdentityProof{
						PID: recorded.PID, ExecutablePath: productPath, StartedAt: recorded.StartedAt,
					}); err != nil {
						return fmt.Errorf("%w: verify recorded prior generation: %v", ErrWindowsProductRecoveryIdentity, err)
					}
					if _, err := held.Terminate(); err != nil {
						return fmt.Errorf("settle recorded prior generation: %w", err)
					}
				} else if current.Supervisor.State != windowsProductSupervisorKnownAbsent {
					return fmt.Errorf("%w: prior generation state is ambiguous", ErrWindowsProductRecoveryIdentity)
				}
				if err := deps.WaitSupervisorLockReleased(ctx, defaultSupervisorLockReleaseTimeout); err != nil {
					return err
				}
				ports := make([]int, 0, len(frozen))
				for _, row := range frozen {
					ports = append(ports, row.Port)
				}
				if len(ports) != 0 {
					if err := verifyPortsUnboundForUpgrade(ports, defaultPostForceKillPortVerifyTimeout); err != nil {
						return err
					}
				}
				launch, err := deps.StartSupervisor(productPath)
				if err != nil {
					return errors.Join(err, abortStartedSupervisor(launch))
				}
				return receiveStartedSupervisor(ctx, launch, supervisorExpectation(stateDir, productPath, journal.Product.PriorSHA256), func() error {
					return deps.WaitSupervisorReady(ctx, defaultSupervisorLockReleaseTimeout, productPath, UpgradeCandidateV1{SHA256: journal.Product.PriorSHA256})
				})
			default:
				return fmt.Errorf("%w: unsupported prior supervisor state", ErrWindowsProductRecoveryIdentity)
			}
		},
		ObserveSuccessorReady: func(ctx context.Context, path, digest string) (bool, error) {
			return deps.probeUpgradeReadyOnce(path, UpgradeCandidateV1{SHA256: digest})
		},
		StartSupervisor: deps.StartSupervisor,
		WaitSupervisorReady: func(ctx context.Context, path, digest string) error {
			return deps.WaitSupervisorReady(ctx, defaultSupervisorLockReleaseTimeout, path, UpgradeCandidateV1{SHA256: digest})
		},
	}
}

type guiOwnedExactPriorReap struct {
	deps     *v5UpgradeDeps
	expected api.SupervisorLockOwner
}

func (r guiOwnedExactPriorReap) send(ctx context.Context, pipePath, command string, args map[string]any, timeoutMs int) (api.IPCResponse, error) {
	owner, err := r.deps.ownerReader()(r.deps.supervisorLockDir)
	if err != nil || owner != r.expected {
		return api.IPCResponse{}, fmt.Errorf("original supervisor generation changed before %s: %v", command, err)
	}
	return r.deps.ipcSender()(ctx, pipePath, r.expected, command, args, time.Duration(timeoutMs+5000)*time.Millisecond)
}

func (r guiOwnedExactPriorReap) QuiesceTimers(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error) {
	return r.send(ctx, pipePath, "quiesce-timers", map[string]any{"timeout_ms": timeoutMs}, timeoutMs)
}

func (r guiOwnedExactPriorReap) ExitGraceful(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error) {
	return r.send(ctx, pipePath, "exit", map[string]any{"graceful": true, "timeout_ms": timeoutMs}, timeoutMs)
}

func (r guiOwnedExactPriorReap) ForceKillSupervisor(string) error {
	return r.deps.forceKillSupervisorForOwner(r.expected)
}

func recoverGUIOwnedPriorSettlement(ctx context.Context, stateDir, productPath string, journal windowsProductRecoveryJournalV3, ownedLease gui.OwnedSingleInstanceLease) error {
	return recoverGUIOwnedPriorSettlementForExpectation(ctx, stateDir, productPath, journal, ownedLease, nil)
}

func recoverGUIOwnedPriorSettlementForExpectation(ctx context.Context, stateDir, productPath string, journal windowsProductRecoveryJournalV3, ownedLease gui.OwnedSingleInstanceLease, expectation *pendingRuntimeExpectation) error {
	deps := buildV5UpgradeDeps(productPath, stateDir)
	frozen := expandWindowsProductFrozenPorts(journal.FrozenPorts)
	if expectation != nil {
		frozen = expandWindowsProductFrozenPorts(expectation.intent.Ports)
	}
	observationInputs := productionUpgradeGUIObservationInputs()
	if expectation != nil {
		observationInputs.PendingRuntime = expectation.receiving
		observationInputs.CurrentIdentity = lookupPendingHistoricalIdentity
	}
	capture := func(ctx context.Context, supervisorPID int) ([32]byte, []windowsProductIntentPortV1, []upgradeGUIPortOwner, error) {
		if expectation == nil {
			return captureUpgradeGUIPortOwners(ctx, stateDir, productPath, supervisorPID)
		}
		if err := expectation.validate(ctx); err != nil {
			return [32]byte{}, nil, nil, err
		}
		digestBytes, err := hex.DecodeString(expectation.intent.SHA256)
		if err != nil || len(digestBytes) != sha256.Size {
			return [32]byte{}, nil, nil, pendingRuntimeFailure(pendingRuntimeInvalid, expectation.receiving, err)
		}
		var digest [32]byte
		copy(digest[:], digestBytes)
		return captureUpgradeGUIPortOwnersFromSnapshot(ctx, productPath, supervisorPID, digest, frozen, observationInputs, api.LoopbackPortOwnerPIDContext, process.LookupProcessIdentitiesContext)
	}
	proveHistorical := func(ctx context.Context, supervisor api.SupervisorLockOwner, owners []upgradeGUIPortOwner) error {
		if expectation == nil {
			return nil
		}
		if err := validatePendingHistoricalCohort(ctx, journal, supervisor, owners, lookupPendingHistoricalIdentity); err != nil {
			return pendingRuntimeFailure(pendingRuntimeHistorical, expectation.receiving, err)
		}
		return nil
	}
	return recoverGUIOwnedPriorSettlementWithOps(ctx, journal, guiOwnedPriorRecoveryOps{
		Readback: func(journal windowsProductRecoveryJournalV3) error {
			if expectation != nil {
				if err := expectation.validate(ctx); err != nil {
					return err
				}
			}
			if err := readbackGUIOwnedPriorProduct(journal); err != nil {
				if expectation != nil {
					return pendingRuntimeFailure(pendingRuntimeMaterial, expectation.receiving, err)
				}
				return err
			}
			return nil
		},
		ObserveHealthy: func(ctx context.Context, journal windowsProductRecoveryJournalV3) (retErr error) {
			in := observationInputs
			if expectation != nil {
				in.PortOwners = func(ctx context.Context, _, _ string, pid int) ([32]byte, []windowsProductIntentPortV1, []upgradeGUIPortOwner, error) {
					return capture(ctx, pid)
				}
			}
			held, pairErr := observeUpgradeGUIOwnedPairHeldTopologyWithInputs(ctx, stateDir, productPath, in)
			if pairErr != nil {
				if pendingRuntimeStopsRecovery(pairErr) {
					return pairErr
				}
				var independentErr error
				held, independentErr = observeUpgradeGUIHeldTopologyWithInputs(ctx, stateDir, productPath, in)
				if independentErr != nil {
					return errors.Join(pairErr, independentErr)
				}
			}
			defer func() {
				closeErr := held.Close()
				if expectation != nil && closeErr != nil {
					retErr = pendingRuntimeCloseError(retErr, expectation.receiving, closeErr)
				}
			}()
			if expectation == nil {
				return validateUpgradeGUIOwnedPriorHealth(journal, held)
			}
			if err := validateGUIOwnedPriorTasks(journal, held.TaskActions); err != nil {
				return pendingRuntimeFailure(pendingRuntimeMaterial, expectation.receiving, err)
			}
			if err := expectation.validateFleet(held.FrozenPorts, held.PortOwners, held.Identity.Supervisor.PID); err != nil {
				return err
			}
			if err := proveHistorical(ctx, held.SupervisorOwner, held.PortOwners); err != nil {
				return err
			}
			return expectation.validate(ctx)
		},
		Reservation: func(ctx context.Context) (gui.OwnedSingleInstanceLease, error) {
			return acquireGUIOwnedPriorReservation(ctx, ownedLease, guiOwnedPriorReservationOps{
				ProveNoGUIOrTray: func(ctx context.Context) error {
					processes, err := findRunningGUIsOnTarget(api.NewAPI(), productPath)
					if err != nil {
						return err
					}
					if len(processes) != 0 {
						return errors.New("target GUI process remains")
					}
					all, err := api.NewAPI().ListMatchingProcesses([]string{"mcphub.exe"})
					if err != nil {
						return err
					}
					for _, candidate := range all {
						if supervisorCommandLineSubcommand(candidate.Cmdline) != "tray" {
							continue
						}
						identity, err := process.LookupPIDHandleIdentity(candidate.PID)
						if err != nil {
							return err
						}
						if samePath(identity.ExecutablePath, productPath) {
							return errors.New("target tray process remains")
						}
					}
					path, err := resolveUpgradeGUIOwnerPath()
					if err != nil {
						return err
					}
					record, err := gui.ReadGUIOwnerRecord(path)
					if err != nil {
						return err
					}
					return proveUpgradeGUIOwnerEndpointAbsent(ctx, record, api.LoopbackPortOwnerPIDContext)
				},
				AcquireExact: func() (gui.OwnedSingleInstanceLease, error) {
					path, err := resolveUpgradeGUIOwnerPath()
					if err != nil {
						return nil, err
					}
					return gui.AcquireSingleInstanceLockOnlyAt(path)
				},
			})
		},
		SupervisorState: func(ctx context.Context, journal windowsProductRecoveryJournalV3) (result guiOwnedPriorSupervisorState, retErr error) {
			lockPath := filepath.Join(stateDir, "supervisor.lock")
			lock, err := api.AcquireSupervisorLockQuiet(lockPath)
			if err == nil {
				lock.Release()
				if _, portErr := observeWindowsProductIntentPorts(ctx, productPath, frozen, true); portErr != nil {
					return "", portErr
				}
				if err := proveHistorical(ctx, api.SupervisorLockOwner{}, nil); err != nil {
					return "", err
				}
				return guiOwnedPriorSupervisorAbsent, nil
			}
			owner, err := api.ReadSupervisorLockOwner(lockPath)
			if err != nil {
				return "", err
			}
			recorded := journal.PreSettlement.Supervisor.Generation
			if recorded == nil || owner.PID != recorded.PID || owner.StartedAt != recorded.StartedAt {
				return "", errors.New("original supervisor generation changed")
			}
			held, err := process.HoldPIDForJobObservation(owner.PID)
			if err != nil {
				return "", err
			}
			defer func() {
				closeErr := held.Close()
				if expectation != nil {
					retErr = pendingRuntimeCloseError(retErr, expectation.receiving, closeErr)
				}
			}()
			if err := held.VerifyIdentity(process.PIDIdentityProof{PID: owner.PID, ExecutablePath: productPath, StartedAt: owner.StartedAt, StartTolerance: time.Nanosecond}); err != nil {
				return "", err
			}
			inJob, err := held.InAnyJob()
			if err != nil || !inJob {
				return "", fmt.Errorf("original supervisor Job membership unconfirmed: member=%v error=%v", inJob, err)
			}
			sameUser, err := process.ProcessOwnerSIDMatchesCurrent(owner.PID)
			if err != nil || !sameUser {
				return "", fmt.Errorf("original supervisor user changed: %v", err)
			}
			identity, err := process.LookupProcessIdentity(owner.PID)
			if err != nil || !samePath(identity.ExecutablePath, productPath) || supervisorCommandLineSubcommand(identity.CommandLine) != "supervise" {
				return "", fmt.Errorf("original supervisor image or role changed: %v", err)
			}
			return guiOwnedPriorSupervisorSame, nil
		},
		SettleSame: func(ctx context.Context, journal windowsProductRecoveryJournalV3) error {
			recorded := journal.PreSettlement.Supervisor.Generation
			if recorded == nil {
				return errors.New("original supervisor generation is missing")
			}
			expected := api.SupervisorLockOwner{PID: recorded.PID, StartedAt: recorded.StartedAt}
			ports := make([]int, 0, len(frozen))
			for _, row := range frozen {
				ports = append(ports, row.Port)
			}
			return api.ReapSupervisor(ctx, api.SupervisorReapOpts{
				PipePath: deps.pipePath, ExpectedPorts: ports,
				VerifyPortsUnbound: verifyPortsUnboundForUpgrade,
				IsAlreadyExited:    isAlreadyExitedError,
				Deps:               guiOwnedExactPriorReap{deps: deps, expected: expected},
			})
		},
		ProveAbsent: func(ctx context.Context, _ windowsProductRecoveryJournalV3) error {
			lock, err := api.AcquireSupervisorLockQuiet(filepath.Join(stateDir, "supervisor.lock"))
			if err != nil {
				return fmt.Errorf("supervisor lock remains held or unknown: %w", err)
			}
			lock.Release()
			if err := proveHistorical(ctx, api.SupervisorLockOwner{}, nil); err != nil {
				return err
			}
			_, err = observeWindowsProductIntentPorts(ctx, productPath, frozen, true)
			return err
		},
		StartSupervisor: func() (SupervisorLaunch, error) {
			if expectation != nil {
				if err := expectation.validate(ctx); err != nil {
					return nil, err
				}
			}
			return deps.StartSupervisor(productPath)
		},
		SupervisorExpected: supervisorExpectation(stateDir, productPath, journal.Product.PriorSHA256),
		WaitSupervisorReady: func(ctx context.Context) error {
			if expectation != nil {
				if err := expectation.validate(ctx); err != nil {
					return err
				}
			}
			if err := deps.WaitSupervisorReady(ctx, defaultSupervisorLockReleaseTimeout, productPath, UpgradeCandidateV1{SHA256: journal.Product.PriorSHA256}); err != nil {
				if expectation != nil {
					return pendingRuntimeFailure(pendingRuntimeReadiness, expectation.receiving, err)
				}
				return err
			}
			if expectation != nil {
				return expectation.validate(ctx)
			}
			return nil
		},
		ProveIndependent: func(ctx context.Context, journal windowsProductRecoveryJournalV3) error {
			limit, cancel := context.WithTimeout(ctx, defaultSupervisorLockReleaseTimeout)
			defer cancel()
			var lastErr error
			for {
				owner, err := api.ReadSupervisorLockOwner(filepath.Join(stateDir, "supervisor.lock"))
				if err != nil {
					return err
				}
				if owner.PID <= 0 || (journal.PreSettlement.Supervisor.Generation != nil && owner.PID == journal.PreSettlement.Supervisor.Generation.PID && owner.StartedAt == journal.PreSettlement.Supervisor.Generation.StartedAt) {
					return errors.New("independent prior supervisor generation was not created")
				}
				held, err := process.HoldPIDForJobObservation(owner.PID)
				if err != nil {
					return err
				}
				identityErr := held.VerifyIdentity(process.PIDIdentityProof{PID: owner.PID, ExecutablePath: productPath, StartedAt: owner.StartedAt, StartTolerance: time.Nanosecond})
				inJob, jobErr := held.InAnyJob()
				closeErr := held.Close()
				if identityErr != nil || jobErr != nil || inJob || closeErr != nil {
					if expectation != nil {
						primary := errors.Join(identityErr, jobErr)
						if inJob {
							primary = errors.Join(primary, errors.New("independent supervisor remains in a Job"))
						}
						if primary != nil {
							primary = pendingRuntimeFailure(pendingRuntimeMismatch, expectation.receiving, primary)
						}
						return pendingRuntimeCloseError(primary, expectation.receiving, closeErr)
					}
					return fmt.Errorf("independent supervisor identity/Job unconfirmed: identity=%v job=%v member=%v close=%v", identityErr, jobErr, inJob, closeErr)
				}
				sameUser, err := process.ProcessOwnerSIDMatchesCurrent(owner.PID)
				if err != nil || !sameUser {
					return fmt.Errorf("independent supervisor user unconfirmed: %v", err)
				}
				identity, err := process.LookupProcessIdentity(owner.PID)
				if err != nil || !samePath(identity.ExecutablePath, productPath) || supervisorCommandLineSubcommand(identity.CommandLine) != "supervise" {
					return fmt.Errorf("independent supervisor image/role unconfirmed: %v", err)
				}
				_, observedFrozen, owners, err := capture(limit, owner.PID)
				if err == nil {
					if expectation == nil {
						err = validateGUIOwnedIndependentFleet(journal, observedFrozen, owners, owner.PID)
					} else {
						err = expectation.validateFleet(observedFrozen, owners, owner.PID)
						if err == nil {
							err = proveHistorical(limit, owner, owners)
						}
						if err == nil {
							err = expectation.validate(limit)
						}
					}
				}
				if err == nil {
					return nil
				}
				if pendingRuntimeStopsRecovery(err) {
					return err
				}
				lastErr = err
				select {
				case <-limit.Done():
					return fmt.Errorf("independent prior fleet did not match frozen running/stopped rows: %v: %w", lastErr, limit.Err())
				case <-time.After(200 * time.Millisecond):
				}
			}
		},
		StartGUI: func() error {
			if expectation != nil {
				if err := expectation.validate(ctx); err != nil {
					return err
				}
			}
			return autostart.StartOwnerForExpectedProduct(ctx, productPath)
		},
	})
}

func restoreGUIOwnedForward(ctx context.Context, journal windowsProductRecoveryJournalV3, releaseLease func() error) error {
	stateDir := filepath.Dir(journal.Receipts.V3.Path)
	productPath := journal.Product.Target
	return restoreGUIOwnedForwardWithObserver(ctx, journal, releaseLease, func(limit context.Context) error {
		in := productionUpgradeGUIObservationInputs()
		held, pairErr := observeUpgradeGUIOwnedPairHeldTopologyWithInputs(limit, stateDir, productPath, in)
		if pairErr != nil {
			var independentErr error
			held, independentErr = observeUpgradeGUIHeldTopologyWithInputs(limit, stateDir, productPath, in)
			if independentErr != nil {
				return errors.Join(pairErr, independentErr)
			}
		}
		defer held.Close()
		forward := journal
		forward.Tasks.Backups = make([]WindowsProductPairTaskBackup, 0, len(held.TaskActions))
		for _, task := range held.TaskActions {
			forward.Tasks.Backups = append(forward.Tasks.Backups, WindowsProductPairTaskBackup{TaskName: task.Name, SHA256: hex.EncodeToString(task.XMLSHA256[:])})
		}
		return validateUpgradeGUIOwnedPriorHealth(forward, held)
	})
}

func restoreGUIOwnedForwardWithObserver(ctx context.Context, journal windowsProductRecoveryJournalV3, releaseLease func() error, observe func(context.Context) error) error {
	if journal.PreSettlement == nil || journal.PreSettlement.GUIOwner != windowsProductGUIOwnerConfirmedPresent ||
		(journal.Phase != WindowsProductRecoveryCommitDecided && journal.Phase != WindowsProductRecoverySettledCommit) {
		return fmt.Errorf("%w: forward GUI recovery has no committed GUI-owned obligation", ErrWindowsProductRecoveryPending)
	}
	if observe == nil {
		return fmt.Errorf("%w: forward GUI health observer unavailable", ErrWindowsProductRecoveryPending)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	limit, cancel := context.WithTimeout(ctx, defaultSupervisorLockReleaseTimeout)
	defer cancel()
	productPath := journal.Product.Target
	if digest, err := hashRecoveryFile(productPath); err != nil || digest != journal.Product.NewSHA256 {
		return fmt.Errorf("%w: committed Product readback differs: %v", ErrWindowsProductRecoveryPending, err)
	}
	lastHealthErr := observe(limit)
	if lastHealthErr == nil {
		return nil
	}
	processes, err := findRunningGUIsOnTarget(api.NewAPI(), productPath)
	if err != nil || len(processes) != 0 {
		return &upgradeGUIForwardHealthError{
			predicate: upgradeGUIForwardHealthPredicate(lastHealthErr),
			summary:   "GUI present without forward fleet health or owner unknown",
			observer:  lastHealthErr,
		}
	}
	if releaseLease != nil {
		if err := releaseLease(); err != nil {
			return fmt.Errorf("%w: release GUI reservation: %v", ErrWindowsProductRecoveryPending, err)
		}
	}
	path, err := resolveUpgradeGUIOwnerPath()
	if err != nil {
		return fmt.Errorf("%w: GUI lease path: %v", ErrWindowsProductRecoveryPending, err)
	}
	free, err := gui.ProbeSingleInstanceLockUnheld(path)
	if err != nil || !free {
		return fmt.Errorf("%w: GUI lease not free before canonical task: %v", ErrWindowsProductRecoveryPending, err)
	}
	if err := autostart.StartOwnerForExpectedProduct(limit, productPath); err != nil {
		return fmt.Errorf("%w: start canonical GUI task: %v", ErrWindowsProductRecoveryPending, err)
	}
	return waitUpgradeGUIForwardHealth(limit, func() error { return observe(limit) })
}

func waitUpgradeGUIForwardHealth(limit context.Context, observe func() error) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		lastHealthErr := observe()
		if lastHealthErr == nil {
			return nil
		}
		select {
		case <-limit.Done():
			return &upgradeGUIForwardHealthError{
				predicate: upgradeGUIForwardHealthPredicate(lastHealthErr),
				summary:   "GUI/tray/fleet forward health unconfirmed",
				observer:  lastHealthErr,
				deadline:  limit.Err(),
			}
		case <-ticker.C:
		}
	}
}

// upgradeGUIForwardHealthError keeps private observer details in the error
// chain while exposing only the classified failure predicate in its text.
type upgradeGUIForwardHealthError struct {
	predicate string
	summary   string
	observer  error
	deadline  error
}

func (e *upgradeGUIForwardHealthError) Error() string {
	message := ErrWindowsProductRecoveryPending.Error() + ": " + e.summary
	if isUpgradeGUIForwardHealthPredicate(e.predicate) {
		message += "; last_failed_predicate=" + e.predicate
	}
	return message
}

func (e *upgradeGUIForwardHealthError) Unwrap() []error {
	return []error{ErrWindowsProductRecoveryPending, e.observer, e.deadline}
}

const upgradeGUIForwardHealthUnclassified = "GUI_FORWARD_HEALTH_UNCLASSIFIED"

func upgradeGUIForwardHealthPredicateIDs() [7]string {
	return [7]string{
		"UPGRADE_GUI_IDENTITY_AMBIGUOUS", "UPGRADE_GUI_LOCK_PATH_MISMATCH",
		"UPGRADE_GUI_DUPLICATE", "UPGRADE_GUI_SUPERVISOR_INDEPENDENCE_UNPROVEN",
		"UPGRADE_GUI_PAIR_TOPOLOGY_UNPROVEN", "UPGRADE_GUI_TASK_DRIFT",
		"UPGRADE_GUI_PAIR_RECOVERY_UNCONFIRMED",
	}
}

func isUpgradeGUIForwardHealthPredicate(predicate string) bool {
	if predicate == upgradeGUIForwardHealthUnclassified {
		return true
	}
	for _, code := range upgradeGUIForwardHealthPredicateIDs() {
		if predicate == code {
			return true
		}
	}
	return false
}

// Only an exact ID on the last outer observation crosses the diagnostic
// boundary; nested probe text can contain other IDs and private values.
func upgradeGUIForwardHealthPredicate(err error) string {
	if err == nil {
		return "NONE"
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var last error
		for _, child := range joined.Unwrap() {
			if child != nil {
				last = child
			}
		}
		if last == nil {
			return upgradeGUIForwardHealthUnclassified
		}
		err = last
	}
	message := err.Error()
	for _, candidate := range upgradeGUIForwardHealthPredicateIDs() {
		if strings.HasPrefix(message, candidate+": ") {
			return candidate
		}
	}
	return upgradeGUIForwardHealthUnclassified
}

// ---------------------------------------------------------------------------
// Kill-target identity gate (shared by v5UpgradeDeps.ForceKillSupervisor).
// ---------------------------------------------------------------------------

// processLookupIdentityFn is a test seam over process.LookupProcessIdentity.
// Production wires it to the real Windows implementation; tests inject a fake.
// It backs supervisorPIDIsLiveMcphubSupervisor's kill-target identity gate (bot
// PR #276 r3 P2).
var processLookupIdentityFn = process.LookupProcessIdentity

// killPIDViaTaskkillFn is a test seam over killPIDViaTaskkill so the
// supervisor-reaper identity-gate tests (bot PR #276 r3 P2) can observe WHICH
// PID — if any — the reaper would force-kill WITHOUT ever shelling `taskkill`
// against a real process. Production wires the real helper; a test swaps it to
// record the call. The interlock/reuse hazard tests must never actually kill
// anything (CLAUDE.md: the developer runs ~21 live production daemons under
// their supervisor), so the kill is mediated through this seam.
var killPIDViaTaskkillFn = killPIDViaTaskkill

// supervisorReapInstallDirFn is the test seam for the install-dir anchor of the
// kill-target identity gate's path check (Gate 4). Production resolves it to the
// directory of the running mcphub binary. Tests inject a deterministic dir so
// the path gate can be exercised without touching the real executable layout.
// An empty result disables Gate 4 — fail-open on the path axis only, never on
// the identity axes.
var supervisorReapInstallDirFn = defaultSupervisorReapInstallDir

// reapOwnerSIDMatchesCurrentFn is the test seam for the SEC-F3 owner-SID arm
// (Gate 5) of the kill-target identity gate. Production delegates to the single
// shared owner-SID helper process.ProcessOwnerSIDMatchesCurrent so the SID
// logic has one owner across api + cli + gui. Tests inject same-SID /
// different-SID / unverifiable verdicts without touching real process tokens.
var reapOwnerSIDMatchesCurrentFn = process.ProcessOwnerSIDMatchesCurrent

func defaultSupervisorReapInstallDir() string {
	exe := canonicalMcphubPath()
	if exe == "" {
		return ""
	}
	return filepath.Dir(exe)
}

// supervisorPIDIsLiveMcphubSupervisor validates that the PID recorded in
// supervisor.lock.owner.json is actually a LIVE mcphub supervisor process —
// the SAME process the sidecar was written for — before any caller
// force-kills it (bot PR #276 r3 P2; hardened to four-gate parity per the
// fable-5 #276 security review). It is the kill-target identity gate the
// supervisor reaper (v5UpgradeDeps.ForceKillSupervisor) consults.
//
// WHY this gate exists: the owner sidecar is best-effort and SURVIVES a
// supervisor crash (an OS-killed supervisor never tidies it). If that crashed
// supervisor's PID is later REUSED by an unrelated OS process, a reaper that
// blindly trusts the sidecar PID would `taskkill /F /T` that unrelated process.
// So the reaper must NAME and VALIDATE its target and refuse to kill anything
// that fails, treating a stale/reused/unrelated PID as "no supervisor to reap"
// (no-op), exactly as a genuinely-absent supervisor is treated.
//
// The four gates:
//
//	Gate 1 (image basename)  — mcphub(.exe), case-insensitive.
//	Gate 2 (argv token)      — argv[1] == "supervise" EXACTLY (token, not
//	                           substring).
//	Gate 3 (creation-time)   — the process's CreationDateUnix must PRECEDE the
//	                           StartedAt the sidecar recorded. A PID created AFTER
//	                           the sidecar write cannot be the process the sidecar
//	                           was written for, so it is a reuse — refuse.
//	Gate 4 (executable path)  — ExecutablePath under the mcphub install dir,
//	                           anchoring against a same-user attacker who spoofs
//	                           name+argv from another directory.
//	Gate 5 (owner SID)        — SEC-F3: the target's owner SID must equal the
//	                           current user's SID. A different-owner supervisor
//	                           (a multi-user host where an admin-token mcphub
//	                           could otherwise taskkill another user's
//	                           supervisor) is refused; an unverifiable owner
//	                           propagates as a reap failure. Mirrors the POSIX
//	                           reaper's UID gate (supervise_reaper_posix.go).
//
// Tri-state return:
//
//	(true,  nil) — proven live supervisor → kill.
//	(false, nil) — PROVEN not the supervisor (or a different-owner SID) → benign
//	               no-op.
//	(false, err) — UNPROVABLE: a transient lookup error OR an unverifiable owner
//	               SID means the reaper cannot prove the supervisor is gone, so
//	               it must propagate as a reap FAILURE rather than silently
//	               report success.
func supervisorPIDIsLiveMcphubSupervisor(pid int, sidecarStartedAt string, transactionInstallDir ...string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	ident, err := processLookupIdentityFn(pid)
	if err != nil {
		if errors.Is(err, process.ErrProcessNotFound) {
			return false, errUpgradeSupervisorAlreadyExited
		}
		// Transient / probe error. We CANNOT prove the recorded PID is dead, so
		// we must NOT report "nothing to kill". Propagate as a reap failure.
		return false, fmt.Errorf("supervisor kill-target identity probe failed for PID %d: %w", pid, err)
	}
	// Gate 1: image basename is mcphub(.exe), case-insensitive.
	base := strings.TrimSpace(ident.Basename)
	if !strings.EqualFold(base, "mcphub.exe") && !strings.EqualFold(base, "mcphub") {
		return false, nil
	}
	// Gate 2: argv[1] is EXACTLY "supervise" (token, not substring).
	if supervisorCommandLineSubcommand(ident.CommandLine) != "supervise" {
		return false, nil
	}
	// Gate 3: the process creation time PRECEDES the StartedAt the sidecar
	// recorded. An empty/unparseable StartedAt cannot anchor this defense →
	// fail closed (no-op).
	startedAtUnix, ok := parseSidecarStartedAtUnix(sidecarStartedAt)
	if !ok {
		return false, nil
	}
	if ident.CreationDateUnix == 0 || ident.CreationDateUnix > startedAtUnix {
		return false, nil
	}
	// Gate 4: ExecutablePath under the mcphub install dir. When the install
	// dir cannot be resolved (empty) the gate is skipped — fail-open on the
	// path axis only, never on the identity axes above.
	installDir := ""
	if len(transactionInstallDir) > 0 {
		installDir = strings.TrimSpace(transactionInstallDir[0])
	} else {
		installDir = supervisorReapInstallDirFn()
	}
	if installDir != "" {
		absInstall, _ := filepath.Abs(installDir)
		absExe, _ := filepath.Abs(ident.ExecutablePath)
		if !supervisorPathHasPrefix(absExe, absInstall) {
			return false, nil
		}
	}
	// Gate 5 (SEC-F3 owner SID): refuse to taskkill a supervisor owned by a
	// DIFFERENT user even when its image/argv/creation-time/path all match.
	//   - SID matches              → proceed (fall through to the live verdict).
	//   - proven different-owner    → benign no-op (false, nil): not OUR
	//     supervisor to reap, so treat it like an identity mismatch.
	//   - target GONE (pr301 r4 Finding 2): the supervisor exited between Gate
	//     1's identity probe and this gate's OpenProcess (a TOCTOU window). The
	//     SID gate returns ErrProcessAlreadyExited. Treat it as a benign no-op
	//     (false, nil) — IDENTICAL to Gate 1's ErrProcessNotFound branch above:
	//     a gone supervisor is "nothing to reap", so the reaper reports success
	//     by skipping it rather than ABORTING install --upgrade.
	//   - owner unverifiable (err)  → propagate as a reap failure (false, err),
	//     consistent with the transient-probe-error contract above.
	// Error-first: resolve the already-dead sentinel BEFORE the generic error
	// path so a gone target is never misread as an unverifiable live one.
	match, sidErr := reapOwnerSIDMatchesCurrentFn(pid)
	if sidErr != nil {
		if errors.Is(sidErr, process.ErrProcessAlreadyExited) {
			return false, errUpgradeSupervisorAlreadyExited
		}
		return false, fmt.Errorf("supervisor kill-target owner-SID gate failed for PID %d: %w", pid, sidErr)
	}
	if !match {
		return false, nil
	}
	return true, nil
}

// supervisorCommandLineSubcommand extracts argv[1] (the cobra subcommand token)
// from a process command-line, honoring quoted-image paths so a
// `"C:\Program Files\mcphub.exe" supervise` form yields "supervise". Returns ""
// when there is no argv[1]. Keys on the precise daemon/command shape rather than
// a substring (fable-5 #276 Finding 3).
func supervisorCommandLineSubcommand(cmdline string) string {
	rest := strings.TrimSpace(cmdline)
	if rest == "" {
		return ""
	}
	// Strip the image (argv[0]). A leading double-quote means the image path
	// is quoted and may contain spaces — consume up to the closing quote.
	if strings.HasPrefix(rest, `"`) {
		if end := strings.IndexByte(rest[1:], '"'); end >= 0 {
			rest = rest[1+end+1:]
		} else {
			// Unterminated quote — no parseable argv[1].
			return ""
		}
	} else if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		rest = rest[idx:]
	} else {
		// Image only, no argv[1].
		return ""
	}
	rest = strings.TrimLeft(rest, " \t")
	if rest == "" {
		return ""
	}
	// argv[1] is the next whitespace-delimited token.
	if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		return rest[:idx]
	}
	return rest
}

// parseSidecarStartedAtUnix parses SupervisorLockOwner.StartedAt (RFC3339Nano
// UTC) into Unix SECONDS so it compares against
// process.ProcessIdentity.CreationDateUnix (also Unix seconds). Returns
// (0, false) on empty or unparseable input so the caller fails closed.
func parseSidecarStartedAtUnix(startedAt string) (int64, bool) {
	s := strings.TrimSpace(startedAt)
	if s == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

// supervisorPathHasPrefix reports whether path is prefix itself or any nested
// child, case-insensitively (Windows).
func supervisorPathHasPrefix(path, prefix string) bool {
	cleanPath := filepath.Clean(path)
	cleanPrefix := filepath.Clean(prefix)
	if strings.EqualFold(cleanPath, cleanPrefix) {
		return true
	}
	if !strings.HasSuffix(cleanPrefix, string(filepath.Separator)) {
		cleanPrefix += string(filepath.Separator)
	}
	if len(cleanPath) < len(cleanPrefix) {
		return false
	}
	return strings.EqualFold(cleanPath[:len(cleanPrefix)], cleanPrefix)
}

// killPIDViaTaskkill kills a process by PID via `taskkill /F /T /PID`.
// /T also kills the process tree so child npx-cache node.exe instances
// (legitimate child of mcphub.exe daemon, per CLAUDE.md
// feedback_kosyak_npx_cache_processes_can_be_active_daemons.md) are
// reaped together.
func killPIDViaTaskkill(pid int) error {
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	process.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill PID %d: %w: %s", pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// verifyPortsUnboundForUpgrade is the production wiring for
// cli.UpgradeOpts.VerifyPortsUnbound. After a force-kill fallback in
// the upgrade flow, every prior-daemon listening port must release
// before the new supervisor spawns (otherwise zombie children would
// collide with the supervisor's reconcile loop). Polls each port with
// portBindWaitForRelease; returns the first per-port timeout error.
func verifyPortsUnboundForUpgrade(ports []int, perPortTimeout time.Duration) error {
	for _, p := range ports {
		if err := portBindWaitForRelease(p, perPortTimeout); err != nil {
			return err
		}
	}
	return nil
}

// portBindWaitForRelease blocks until 127.0.0.1:port is unbound OR
// timeout elapses. Polling cadence: 100 ms; total budget bounded by
// timeout. Returns nil on unbound, non-nil error on timeout (or hard
// listen failure other than EADDRINUSE).
func portBindWaitForRelease(port int, timeout time.Duration) error {
	if port <= 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			// Port was unbound — close immediately so the daemon can re-bind.
			_ = l.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("port %d not released within %s: %w", port, timeout, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Supervisor spawn wiring.
// ---------------------------------------------------------------------------

// newInstallSupervisorCmd builds the detached `<exe> supervise [--strict-mode]`
// command for the install/upgrade path. Package-level (rather than an inline
// closure) so the spawn CONFIGURATION can be asserted without starting a
// supervisor.
//
// Process detachment preserves supervisor lifetime independently of the caller.
// process.NoConsole adds the shared CREATE_NO_WINDOW child contract, and the
// product's ordinary startup policy never attaches or allocates a console.
// Every degraded retry rebuilds through this function and therefore reapplies
// the same no-window contract without environment markers or argv propagation.
func newInstallSupervisorCmd(exePath string, args []string) *exec.Cmd {
	c := exec.Command(exePath, args...)
	c.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	process.NoConsole(c)
	return c
}

// installSupervisorCmdBuilder returns the builder the spawn path hands to
// startSupervisorDetachedBreakaway. Package-level so a test can invoke the
// builder REPEATEDLY and assert that each rebuild — not merely the first
// attempt — carries the no-window child contract.
func installSupervisorCmdBuilder(exePath string, strictMode bool) func() *exec.Cmd {
	args := []string{"supervise"}
	if strictMode {
		args = append(args, "--strict-mode")
	}
	return func() *exec.Cmd { return newInstallSupervisorCmd(exePath, args) }
}

// ---------------------------------------------------------------------------
// v5UpgradeDeps: production wiring for cli.UpgradeDeps.
// ---------------------------------------------------------------------------

// v5UpgradeDeps is the production implementation of UpgradeDeps that
// drives the rename-aside + IPC + supervisor-spawn flow on Windows.
// Field semantics match the cli.UpgradeOpts shape exactly.
type supervisorPeerIPCSender func(context.Context, string, api.SupervisorLockOwner, api.SupervisorProcessIdentityV1, string, map[string]any, time.Duration) (api.IPCResponse, error)

var errSupervisorPeerIPCSenderRequired = errors.New("original-created supervisor requires a strict peer IPC sender")

type v5UpgradeDeps struct {
	// exePath is the canonical transaction target used to start and identify
	// the managed supervisor. It is deliberately not the updater executable.
	exePath             string
	supervisorLockDir   string
	pipePath            string
	readSupervisorOwner func(string) (api.SupervisorLockOwner, error)
	sendIPC             func(context.Context, string, api.SupervisorLockOwner, string, map[string]any, time.Duration) (api.IPCResponse, error)
	sendIPCPeer         supervisorPeerIPCSender
}

func (d *v5UpgradeDeps) RenameAsideBinary(target, newSrc string) (api.RenameAsideResult, error) {
	if err := binaryadmission.AdmitWindowsUpgradePrior(target); err != nil {
		return api.RenameAsideResult{}, fmt.Errorf("admit prior canonical binary: %w", err)
	}
	if err := binaryadmission.AdmitWindowsGUI(newSrc); err != nil {
		return api.RenameAsideResult{}, fmt.Errorf("admit staged successor binary: %w", err)
	}
	priorHash, err := hashFile(target)
	if err != nil {
		return api.RenameAsideResult{}, fmt.Errorf("hash prior canonical binary: %w", err)
	}
	result, err := api.RenameAsideReplaceWithResult(target, newSrc)
	if err != nil {
		return result, err
	}
	retainedHash, err := hashFile(result.RetainedPrior)
	if err != nil {
		return result, fmt.Errorf("hash exact retained prior %s after promotion: %w", result.RetainedPrior, err)
	}
	if !bytes.Equal(retainedHash, priorHash) {
		return result, fmt.Errorf("exact retained prior SHA-256 mismatch after promotion")
	}
	return result, nil
}

func (d *v5UpgradeDeps) RestoreRetainedBinary(target, retainedPrior string) error {
	return copyExeWithWindowsAdmission(retainedPrior, target, binaryadmission.AdmitWindowsUpgradePrior)
}

func (d *v5UpgradeDeps) QuiesceTimers(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error) {
	owner, err := observedV5UpgradeIPCOwner("quiesce-timers", d.supervisorLockDir, d.ownerReader())
	if err != nil {
		return api.IPCResponse{}, err
	}
	return d.ipcSender()(ctx, pipePath, owner, "quiesce-timers", map[string]any{"timeout_ms": timeoutMs}, time.Duration(timeoutMs+5000)*time.Millisecond)
}

func (d *v5UpgradeDeps) ExitGraceful(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error) {
	owner, err := observedV5UpgradeIPCOwner("exit", d.supervisorLockDir, d.ownerReader())
	if err != nil {
		return api.IPCResponse{}, err
	}
	return d.ipcSender()(ctx, pipePath, owner, "exit", map[string]any{"graceful": true, "timeout_ms": timeoutMs}, time.Duration(timeoutMs+5000)*time.Millisecond)
}

func (d *v5UpgradeDeps) ownerReader() func(string) (api.SupervisorLockOwner, error) {
	if d != nil && d.readSupervisorOwner != nil {
		return d.readSupervisorOwner
	}
	return api.ReadSupervisorLockOwner
}

func (d *v5UpgradeDeps) ipcSender() func(context.Context, string, api.SupervisorLockOwner, string, map[string]any, time.Duration) (api.IPCResponse, error) {
	if d != nil && d.sendIPC != nil {
		return d.sendIPC
	}
	return sendIPCWithResponse
}

func (d *v5UpgradeDeps) peerIPCSender() (supervisorPeerIPCSender, error) {
	if d == nil {
		return nil, errSupervisorPeerIPCSenderRequired
	}
	if d.sendIPCPeer != nil {
		return d.sendIPCPeer, nil
	}
	if d.sendIPC != nil {
		return nil, errSupervisorPeerIPCSenderRequired
	}
	return sendIPCWithResponseForPeer, nil
}

func observedV5UpgradeIPCOwner(operation, lockDir string, readSupervisorOwner func(string) (api.SupervisorLockOwner, error)) (api.SupervisorLockOwner, error) {
	owner, err := readSupervisorOwner(lockDir)
	if err != nil {
		return api.SupervisorLockOwner{}, fmt.Errorf("%s: read supervisor lock owner: %w", operation, err)
	}
	if owner.PID <= 0 || strings.TrimSpace(owner.StartedAt) == "" {
		return api.SupervisorLockOwner{}, fmt.Errorf("%s: supervisor lock owner is invalid", operation)
	}
	return owner, nil
}

func (d *v5UpgradeDeps) ForceKillSupervisor(pipePath string) error {
	// Codex round-4 Lane C P1 (codex-r4-c-p1): the historical
	// implementation collapsed every ReadSupervisorLockOwner error
	// onto benign nil — including permission denied, corrupt JSON,
	// and non-positive PID values. Under the now-strict
	// RunInstallUpgrade path that swallow hides real failures from
	// the orchestrator (which relies on a non-nil return to escalate
	// to verifyPortsUnbound / abort). Only the os.IsNotExist branch
	// represents a proven "no supervisor running" condition; every
	// other read failure or corrupt-sidecar shape must propagate.
	owner, err := api.ReadSupervisorLockOwner(d.supervisorLockDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: supervisor lock owner is absent", errUpgradeSupervisorAlreadyExited)
		}
		return fmt.Errorf("force-kill: read supervisor lock owner: %w", err)
	}
	if owner.PID <= 0 {
		return fmt.Errorf("force-kill: supervisor.lock.owner.json has invalid PID %d (corrupt sidecar)", owner.PID)
	}
	return d.forceKillSupervisorForOwner(owner)
}

// forceKillSupervisorForOwner permits compatibility handoffs to bind the
// exact authenticated legacy generation. A different sidecar owner means the
// old supervisor already yielded; never kill the replacement generation.
func (d *v5UpgradeDeps) forceKillSupervisorForOwner(expected api.SupervisorLockOwner) error {
	current, err := api.ReadSupervisorLockOwner(d.supervisorLockDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: supervisor lock owner is absent", errUpgradeSupervisorAlreadyExited)
		}
		return fmt.Errorf("force-kill: read supervisor lock owner: %w", err)
	}
	if current != expected {
		return fmt.Errorf("force-kill: refusing generation change: got pid=%d started_at=%s want pid=%d started_at=%s", current.PID, current.StartedAt, expected.PID, expected.StartedAt)
	}
	// Kill-target identity gate (bot PR #276 r3 P2; hardened to four-gate
	// parity per fable-5 #276). The owner sidecar survives a supervisor crash
	// and its PID can be REUSED by an unrelated process. Validate the PID is the
	// live mcphub supervisor the sidecar names before force-killing it:
	//   - process-gone → typed already-exited outcome consumed by orchestration.
	//   - identity mismatch → explicit refusal; never report a kill.
	//   - transient probe error → propagate (fable-5 #276 Finding 2).
	installDir := ""
	if strings.TrimSpace(d.exePath) != "" {
		installDir = filepath.Dir(d.exePath)
	}
	live, err := supervisorPIDIsLiveMcphubSupervisor(expected.PID, expected.StartedAt, installDir)
	if err != nil {
		return fmt.Errorf("force-kill: %w", err)
	}
	if !live {
		return fmt.Errorf("force-kill: refusing PID %d because its live process identity does not match the transaction canonical target %s", expected.PID, d.exePath)
	}
	return killPIDViaTaskkillFn(expected.PID)
}

func (d *v5UpgradeDeps) supervisorLaunchDependencies() (supervisorLaunchDependencies, error) {
	sender, err := d.peerIPCSender()
	if err != nil {
		return supervisorLaunchDependencies{}, err
	}
	lockPath := d.supervisorLockDir
	deps := supervisorLaunchDependencies{
		exePath:            d.exePath,
		supervisorLockPath: lockPath,
		pipePath:           d.pipePath,
		readOwner:          d.ownerReader(),
		sendIPCPeer:        sender,
		waitLockReleased: func(ctx context.Context, timeout time.Duration) error {
			return waitV5SupervisorLockReleased(ctx, timeout, lockPath)
		},
	}
	if err := deps.validate(); err != nil {
		return supervisorLaunchDependencies{}, err
	}
	return deps, nil
}

func (d *v5UpgradeDeps) StartSupervisor(binaryPath string) (SupervisorLaunch, error) {
	launchDeps, err := d.supervisorLaunchDependencies()
	if err != nil {
		return nil, err
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return nil, fmt.Errorf("resolve state-dir: %w", err)
	}
	if !samePath(stateDir, filepath.Dir(launchDeps.supervisorLockPath)) || !samePath(binaryPath, launchDeps.exePath) {
		return nil, errors.New("supervisor launch namespace/target differs from adapter")
	}
	ports, err := supervisorCompatExpectedPorts(stateDir)
	if err != nil {
		return nil, err
	}
	strictMode, _ := readPreMigrationStrictMode(stateDir)
	build := installSupervisorCmdBuilder(binaryPath, strictMode)
	started, err := startSupervisorDetachedBreakaway(build(), build, nil, process.BreakawayRequired)
	if err != nil {
		return nil, fmt.Errorf("spawn supervisor: %w", err)
	}
	launch, err := newOwnedSupervisorLaunch(started, launchDeps, ports)
	if err != nil {
		// Even a failed first identity observation leaves the actual creator
		// with its caller, which owns bounded settlement of that start.
		return launch, err
	}
	return launch, nil
}

func (d *v5UpgradeDeps) WaitSupervisorLockReleased(ctx context.Context, timeout time.Duration) error {
	return waitV5SupervisorLockReleased(ctx, timeout, d.supervisorLockDir)
}

func waitV5SupervisorLockReleased(ctx context.Context, timeout time.Duration, lockPath string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		lk, err := api.AcquireSupervisorLockQuiet(lockPath)
		if err == nil {
			lk.Release()
			return nil
		}
		lastErr = err
		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("%w (last lock probe: %v)", waitCtx.Err(), lastErr)
			}
			return waitCtx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (d *v5UpgradeDeps) WaitSupervisorReady(ctx context.Context, timeout time.Duration, binaryPath string, candidate UpgradeCandidateV1) error {
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		ready, err := d.probeUpgradeReadyOnce(binaryPath, candidate)
		if err == nil && ready {
			return nil
		}
		lastErr = err
		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("%w (last IPC status probe: %v)", waitCtx.Err(), lastErr)
			}
			return fmt.Errorf("%w (supervisor status never reported reconcile_ready=true)", waitCtx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (d *v5UpgradeDeps) probeUpgradeReadyOnce(binaryPath string, candidate UpgradeCandidateV1) (bool, error) {
	owner, err := api.ReadSupervisorLockOwner(d.supervisorLockDir)
	if err != nil {
		return false, fmt.Errorf("read successor supervisor lock owner: %w", err)
	}
	if err := d.validateUpgradeSupervisorIdentity(binaryPath, candidate, owner); err != nil {
		return false, err
	}
	return probeReconcileReadyOnceWithOwner(d.pipePath, &owner)
}

func (d *v5UpgradeDeps) validateUpgradeSupervisorIdentity(binaryPath string, candidate UpgradeCandidateV1, owner api.SupervisorLockOwner) error {
	return validateSupervisorImageIdentity(binaryPath, candidate.SHA256, owner)
}

// sendIPCWithResponse is the response-returning IPC sender used by
// v5UpgradeDeps. Returns the final-frame response (or the only frame for
// single-frame commands).
func sendIPCWithResponse(ctx context.Context, pipePath string, owner api.SupervisorLockOwner, cmdName string, args map[string]any, timeout time.Duration) (api.IPCResponse, error) {
	return sendIPCWithResponseProjection(ctx, pipePath, owner, nil, cmdName, args, timeout)
}

func sendIPCWithResponseForPeer(ctx context.Context, pipePath string, owner api.SupervisorLockOwner, expected api.SupervisorProcessIdentityV1, cmdName string, args map[string]any, timeout time.Duration) (api.IPCResponse, error) {
	return sendIPCWithResponseProjection(ctx, pipePath, owner, &expected, cmdName, args, timeout)
}

// supervisorIPCDispatchError describes only its producing strict invocation.
// Only the immediate result of a direct production sender call is evidence;
// custom callbacks, matching operands and errors.As over causal history cannot
// establish that this observation belongs to their current invocation.
type supervisorIPCDispatchError struct {
	owner               api.SupervisorLockOwner
	peer                api.SupervisorProcessIdentityV1
	command             string
	requestWriteEntered bool
	cause               error
}

func (e *supervisorIPCDispatchError) Error() string { return e.cause.Error() }
func (e *supervisorIPCDispatchError) Unwrap() error { return e.cause }

func sendIPCWithResponseProjection(ctx context.Context, pipePath string, owner api.SupervisorLockOwner, peer *api.SupervisorProcessIdentityV1, cmdName string, args map[string]any, timeout time.Duration) (api.IPCResponse, error) {
	var dispatch *supervisorIPCDispatchError
	if peer != nil {
		dispatch = &supervisorIPCDispatchError{owner: owner, peer: *peer, command: cmdName}
		if ctx == nil || timeout <= 0 {
			dispatch.cause = errors.New("strict supervisor IPC requires context and positive timeout")
			return api.IPCResponse{}, dispatch
		}
		if err := ctx.Err(); err != nil {
			dispatch.cause = err
			return api.IPCResponse{}, dispatch
		}
	}
	conn, err := winio.DialPipe(pipePath, durPtr(timeout))
	if err != nil {
		if dispatch != nil {
			dispatch.cause = fmt.Errorf("DialPipe: %w", err)
			return api.IPCResponse{}, dispatch
		}
		return api.IPCResponse{}, fmt.Errorf("DialPipe: %w", err)
	}
	return exchangeSupervisorIPCResponse(ctx, conn, owner, peer, cmdName, args, timeout)
}

// This existing exchange owns the passed actual connection on every return.
// Dial projections, including the strict Product sender, share its sole body.
func exchangeSupervisorIPCResponse(ctx context.Context, conn net.Conn, owner api.SupervisorLockOwner, peer *api.SupervisorProcessIdentityV1, cmdName string, args map[string]any, timeout time.Duration) (response api.IPCResponse, retErr error) {
	var dispatch *supervisorIPCDispatchError
	if peer == nil {
		defer conn.Close()
	} else {
		dispatch = &supervisorIPCDispatchError{owner: owner, peer: *peer, command: cmdName}
		defer func() {
			if err := errors.Join(retErr, conn.Close()); err != nil {
				dispatch.cause = err
				retErr = dispatch
			}
		}()
		if ctx == nil || timeout <= 0 {
			return api.IPCResponse{}, errors.New("strict supervisor IPC requires context and positive timeout")
		}
		if err := ctx.Err(); err != nil {
			return api.IPCResponse{}, err
		}
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(timeout)
	}
	if err := conn.SetDeadline(deadline); peer != nil && err != nil {
		return api.IPCResponse{}, fmt.Errorf("set supervisor IPC deadline: %w", err)
	}
	if peer != nil {
		if err := api.ValidateSupervisorControlPeerConnectionV1(conn, owner, *peer); err != nil {
			return api.IPCResponse{}, err
		}
		if err := ctx.Err(); err != nil {
			return api.IPCResponse{}, err
		}
	}

	if err := verifyHelloFrame(conn, owner); err != nil {
		return api.IPCResponse{}, fmt.Errorf("hello: %w", err)
	}
	if peer != nil {
		if err := ctx.Err(); err != nil {
			return api.IPCResponse{}, err
		}
	}
	req := api.IPCRequest{ID: time.Now().UnixNano(), Cmd: cmdName, Args: args}
	if err := writeFrameWithDispatch(conn, req, dispatch); err != nil {
		return api.IPCResponse{}, fmt.Errorf("send %s: %w", cmdName, err)
	}
	// Loop for the final frame.
	for {
		resp, err := readFrame(conn)
		if err != nil {
			if peer != nil {
				return response, fmt.Errorf("read %s: %w", cmdName, err)
			}
			return api.IPCResponse{}, fmt.Errorf("read %s: %w", cmdName, err)
		}
		if peer != nil {
			response = resp
		}
		if resp.Final {
			return resp, nil
		}
		if !resp.OK && resp.Error != nil {
			return resp, fmt.Errorf("%s: %s: %s", cmdName, resp.Error.Code, resp.Error.Message)
		}
		// Continue: this was the immediate accepted-frame for the
		// two-frame command shape.
	}
}

// ---------------------------------------------------------------------------
// Reconcile-ready IPC poll (shared with migrate_serena_restart_windows.go).
// ---------------------------------------------------------------------------

// waitReconcileReadyViaIPC returns a closure that polls supervisor IPC
// `status` until `reconcile_ready: true` is observed in the response result
// map OR the timeout elapses. Used by the serena migrate START driver
// (migrate_serena_restart_windows.go) after it (re)starts the supervisor.
//
// Production note: the supervisor's IPC `status` handler includes a
// `reconcile_ready` bool that transitions from false → true after the first
// successful reconcile pass. The poll interval here is 200 ms.
func waitReconcileReadyViaIPC(pipePath string) func(timeout time.Duration) error {
	return func(timeout time.Duration) error {
		deadline := time.Now().Add(timeout)
		var lastErr error
		for {
			ready, err := probeReconcileReadyOnce(pipePath)
			if err == nil && ready {
				return nil
			}
			lastErr = err
			if time.Now().After(deadline) {
				if lastErr != nil {
					return fmt.Errorf("reconcile-ready poll timed out after %s; last error: %w", timeout, lastErr)
				}
				return fmt.Errorf("reconcile-ready poll timed out after %s (supervisor never reported reconcile_ready=true)", timeout)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// probeReconcileReadyOnce dials the supervisor IPC pipe and issues one
// `status` request, returning (ready, err). `err` reports transport-level
// failure (dial, JSON, timeout); ready reports the supervisor's reported
// state. The caller's poll loop tolerates non-nil errors during the
// supervisor's startup window.
func probeReconcileReadyOnce(pipePath string) (bool, error) {
	return probeReconcileReadyOnceWithOwner(pipePath, nil)
}

func probeReconcileReadyOnceWithOwner(pipePath string, expectedOwner *api.SupervisorLockOwner) (bool, error) {
	conn, err := winio.DialPipe(pipePath, durPtr(2*time.Second))
	if err != nil {
		return false, fmt.Errorf("DialPipe: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	var helloErr error
	if expectedOwner != nil {
		helloErr = verifyHelloFrame(conn, *expectedOwner)
	} else {
		helloErr = skipHelloFrame(conn)
	}
	if helloErr != nil {
		return false, fmt.Errorf("hello: %w", helloErr)
	}
	// Send status request.
	req := api.IPCRequest{ID: 1, Cmd: "status"}
	if err := writeFrame(conn, req); err != nil {
		return false, fmt.Errorf("send status: %w", err)
	}
	resp, err := readFrame(conn)
	if err != nil {
		return false, fmt.Errorf("read response: %w", err)
	}
	if !resp.OK {
		return false, nil
	}
	result, _ := resp.Result.(map[string]any)
	if result == nil {
		return false, nil
	}
	if v, ok := result["reconcile_ready"].(bool); ok {
		return v, nil
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// Low-level IPC frame I/O.
// ---------------------------------------------------------------------------

// skipHelloFrame reads + discards the supervisor's hello frame at
// connection start.
func skipHelloFrame(conn net.Conn) error {
	var buf [4096]byte
	for i := 0; i < 4096; i++ {
		n, err := conn.Read(buf[i : i+1])
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if buf[i] == '\n' {
			return nil
		}
	}
	return fmt.Errorf("hello frame exceeded 4 KiB")
}

// verifyHelloFrame reads the hello frame and validates it against the
// expected supervisor.lock owner. Mismatch returns an error; the
// caller closes the connection.
func verifyHelloFrame(conn net.Conn, expected api.SupervisorLockOwner) error {
	line, err := readLine(conn, 4096)
	if err != nil {
		return err
	}
	var env struct {
		Hello api.IPCHello `json:"hello"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return fmt.Errorf("decode hello: %w (raw=%q)", err, string(line))
	}
	if expected.PID == 0 && expected.StartedAt == "" {
		// No owner sidecar to compare against — best-effort, accept.
		return nil
	}
	if !api.ValidateHandshake(env.Hello, expected) {
		return fmt.Errorf("hello mismatch: got pid=%d started_at=%s expected pid=%d started_at=%s",
			env.Hello.PID, env.Hello.StartedAt, expected.PID, expected.StartedAt)
	}
	return nil
}

// writeFrame JSON-encodes req + appends a trailing newline (the
// supervisor's frame delimiter per spec §"Wire format").
func writeFrame(conn net.Conn, req api.IPCRequest) error {
	return writeFrameWithDispatch(conn, req, nil)
}

func writeFrameWithDispatch(conn net.Conn, req api.IPCRequest, dispatch *supervisorIPCDispatchError) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	raw = append(raw, '\n')
	if dispatch != nil {
		dispatch.requestWriteEntered = true
	}
	if _, err := conn.Write(raw); err != nil {
		return err
	}
	return nil
}

// readFrame reads one JSON line from conn and decodes it into an
// IPCResponse.
func readFrame(conn net.Conn) (api.IPCResponse, error) {
	line, err := readLine(conn, api.SupervisorIPCResponseMaxBytes)
	if err != nil {
		return api.IPCResponse{}, err
	}
	var resp api.IPCResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return api.IPCResponse{}, fmt.Errorf("decode response: %w (raw=%q)", err, string(line))
	}
	return resp, nil
}

// readLine reads bytes until '\n' or max is hit. Returns the line
// WITHOUT the trailing newline. Returns error on max exceeded.
func readLine(conn net.Conn, max int) ([]byte, error) {
	buf := make([]byte, 0, 256)
	for i := 0; i < max; i++ {
		var b [1]byte
		n, err := conn.Read(b[:])
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		if b[0] == '\n' {
			return buf, nil
		}
		buf = append(buf, b[0])
	}
	return buf, fmt.Errorf("line exceeded %d bytes", max)
}

// ---------------------------------------------------------------------------
// Misc helpers.
// ---------------------------------------------------------------------------

// SEC-F1 removed the USERNAME-based superviseIPCPipePath + currentWindowsUsername
// helpers. Every IPC dial in the upgrade/migrate flow now uses the SID-based
// canonical resolver api.SupervisorIPCAddress (the path the supervisor LISTENS
// on), closing the PR #212 r3 SID-consistency propagation gap.

// readPreMigrationStrictMode reads strict_mode from supervisor-intent.json
// if present. Returns false when the file is missing.
func readPreMigrationStrictMode(stateDir string) (bool, error) {
	path := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(path)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		// Surface other errors so the caller can decide whether to
		// abort or proceed with strict_mode=false.
		return false, err
	}
	return intent.StrictMode, nil
}

// durPtr returns a pointer to d. winio.DialPipe takes a *time.Duration
// (nil = no timeout); the helper saves the callsite from a local var.
func durPtr(d time.Duration) *time.Duration {
	return &d
}

package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/mcpcompat/readinesswire"
	"mcp-local-hub/internal/process"
)

const provisionalStdioBridgeCleanupDeadline = 5 * time.Second

// FrozenStdioBridgeAdmissionRequest is the exact supervisor descriptor the
// apply path will later persist. It is intentionally derived from the already
// frozen plan rather than rebuilding a command from mutable manifest state.
type FrozenStdioBridgeAdmissionRequest struct {
	ManifestName  string
	DaemonName    string
	ManifestHash  string
	Command       string
	Args          []string
	WorkingDir    string
	Port          int
	StartDeadline time.Duration
}

// frozenStdioBridgeAdmissionFn is a narrow test seam around the only
// process-owning admission operation. Production always runs the contained
// initialize -> tools/list probe below.
var frozenStdioBridgeAdmissionFn = admitFrozenStdioBridgeRequests

// admitFrozenStdioBridgeBeforeMutation is the fail-before-advertise gate for
// selected global stdio-bridge daemons. Dry-runs never call it.
func admitFrozenStdioBridgeBeforeMutation(ctx context.Context, m *config.ServerManifest, plan *Plan, opts InstallOpts) error {
	if opts.DryRun || m == nil || m.Kind != config.KindGlobal || m.Transport != config.TransportStdioBridge {
		return nil
	}
	requests, err := frozenStdioBridgeAdmissionRequests(m, plan, opts.DaemonFilter)
	if err != nil {
		return err
	}
	if len(requests) == 0 {
		return nil
	}
	return frozenStdioBridgeAdmissionFn(ctx, requests)
}

func frozenStdioBridgeAdmissionRequests(m *config.ServerManifest, plan *Plan, daemonFilter string) ([]FrozenStdioBridgeAdmissionRequest, error) {
	if m == nil || plan == nil {
		return nil, errors.New("stdio bridge admission: nil manifest or frozen plan")
	}
	entries := make(map[string][]SupervisorIntentEntry, len(plan.SupervisorIntent))
	for _, entry := range plan.SupervisorIntent {
		entries[entry.Name] = append(entries[entry.Name], entry)
	}
	requests := make([]FrozenStdioBridgeAdmissionRequest, 0, len(m.Daemons))
	for _, daemon := range m.Daemons {
		if daemonFilter != "" && daemon.Name != daemonFilter {
			continue
		}
		name := supervisorTaskNameForManifestDaemon(m.Name, daemon.Name)
		matches := entries[name]
		if len(matches) != 1 {
			return nil, fmt.Errorf("stdio bridge admission: selected daemon %q requires exactly one frozen supervisor descriptor (got %d)", daemon.Name, len(matches))
		}
		entry := matches[0]
		if entry.Command == "" || len(entry.Args) == 0 || entry.WorkingDir == "" || daemon.Port <= 0 || entry.StartupBindDeadlineSeconds <= 0 {
			return nil, fmt.Errorf("stdio bridge admission: selected daemon %q has an incomplete frozen runtime descriptor", daemon.Name)
		}
		requests = append(requests, FrozenStdioBridgeAdmissionRequest{
			ManifestName: m.Name, DaemonName: daemon.Name, ManifestHash: entry.manifestHash,
			Command: entry.Command, Args: append([]string(nil), entry.Args...), WorkingDir: entry.WorkingDir,
			Port: daemon.Port, StartDeadline: time.Duration(entry.StartupBindDeadlineSeconds) * time.Second,
		})
	}
	return requests, nil
}

func admitFrozenStdioBridgeRequests(ctx context.Context, requests []FrozenStdioBridgeAdmissionRequest) error {
	for _, request := range requests {
		if err := admitFrozenStdioBridgeRequest(ctx, request); err != nil {
			return err
		}
	}
	return nil
}

func admitFrozenStdioBridgeRequest(ctx context.Context, request FrozenStdioBridgeAdmissionRequest) (err error) {
	if err := validateFrozenStdioBridgeAdmissionRequest(request); err != nil {
		return err
	}
	existing, err := existingFrozenStdioBridgeGeneration(request)
	if err != nil {
		return err
	}
	if existing {
		probeCtx, cancel := context.WithTimeout(ctx, request.StartDeadline)
		defer cancel()
		return probeFrozenStdioBridgePort(probeCtx, request)
	}

	probeCtx, cancel := context.WithTimeout(ctx, request.StartDeadline)
	defer cancel()
	cmd := exec.Command(request.Command, request.Args...)
	cmd.Dir = request.WorkingDir
	child, err := process.StartProvisional(cmd)
	if err != nil {
		return fmt.Errorf("stdio bridge admission: start provisional %s/%s: %w", request.ManifestName, request.DaemonName, err)
	}
	defer func() {
		cleanupErr := child.TerminateAndWait(provisionalStdioBridgeCleanupDeadline)
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("stdio bridge admission: reap provisional %s/%s: %w", request.ManifestName, request.DaemonName, cleanupErr))
			return
		}
		if portInUse(request.Port) {
			err = errors.Join(err, fmt.Errorf("stdio bridge admission: provisional %s/%s left port %d in use", request.ManifestName, request.DaemonName, request.Port))
		}
	}()
	if err := waitFrozenStdioBridgePort(probeCtx, request.Port); err != nil {
		return fmt.Errorf("stdio bridge admission: provisional %s/%s did not bind port %d: %w", request.ManifestName, request.DaemonName, request.Port, err)
	}
	return probeFrozenStdioBridgePort(probeCtx, request)
}

func validateFrozenStdioBridgeAdmissionRequest(request FrozenStdioBridgeAdmissionRequest) error {
	if request.ManifestName == "" || request.DaemonName == "" || request.Command == "" || len(request.Args) == 0 || request.Port <= 0 || request.StartDeadline <= 0 {
		return errors.New("stdio bridge admission: incomplete frozen runtime descriptor")
	}
	return nil
}

func existingFrozenStdioBridgeGeneration(request FrozenStdioBridgeAdmissionRequest) (bool, error) {
	stateDir, err := daemonStateDirReadOnly()
	if err != nil {
		return false, fmt.Errorf("stdio bridge admission: resolve existing intent: %w", err)
	}
	intent, err := ReadSupervisorIntent(joinStateFilePath(stateDir, supervisorIntentFileLeaf))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stdio bridge admission: read existing intent: %w", err)
	}
	var matches []SupervisorDaemon
	for _, daemon := range intent.Daemons {
		if daemon.Server == request.ManifestName && daemon.Daemon == request.DaemonName {
			matches = append(matches, daemon)
		}
	}
	if len(matches) == 0 {
		return false, nil
	}
	if len(matches) != 1 {
		return false, fmt.Errorf("stdio bridge admission: existing generation for %s/%s is ambiguous", request.ManifestName, request.DaemonName)
	}
	current := matches[0]
	if current.Port != request.Port || current.Command != request.Command || !slices.Equal(current.Args, request.Args) || !sameFrozenWorkingDir(current.WorkingDir, request.WorkingDir) || !sameFrozenStartDeadline(current.StartupBindDeadlineSeconds, request.StartDeadline) || request.ManifestHash == "" || current.ManifestHash != request.ManifestHash {
		return false, fmt.Errorf("stdio bridge admission: existing generation for %s/%s does not exactly match the frozen plan", request.ManifestName, request.DaemonName)
	}
	return true, nil
}

func sameFrozenWorkingDir(current, frozen string) bool {
	if current == "" || frozen == "" {
		return current == frozen
	}
	return filepath.Clean(current) == filepath.Clean(frozen)
}

func sameFrozenStartDeadline(currentSeconds int, frozen time.Duration) bool {
	return currentSeconds > 0 && frozen > 0 && time.Duration(currentSeconds)*time.Second == frozen
}

func waitFrozenStdioBridgePort(ctx context.Context, port int) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if portInUse(port) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func probeFrozenStdioBridgePort(ctx context.Context, request FrozenStdioBridgeAdmissionRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	probe := singleHealthProbeContext(ctx, request.Port)
	if err := ctx.Err(); err != nil {
		return err
	}
	if probe != nil && probe.OK && probe.Readiness.State == MCPReadinessReady {
		return nil
	}
	if probe != nil && probe.Readiness.FailureID != "" {
		return &MCPReadinessError{Result: probe.Readiness}
	}
	return &MCPReadinessError{Result: MCPReadinessResult{
		SchemaVersion: "mcp-readiness-v1", State: MCPReadinessUnready, Stage: MCPReadinessStageInitialize,
		FailureID: readinesswire.FailureReadinessHostUnavailable,
	}}
}

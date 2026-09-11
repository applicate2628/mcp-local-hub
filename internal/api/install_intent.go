// Package api — Task 10 intent + audit wiring for existing commands
// (watchdog plan v13 §8, §11, §24, §51, §62, §65, Task 10).
//
// Task 10 wires Task 2's WriteDaemonIntent + Task 3's AppendIntentAudit
// into the existing mcphub install / stop / restart / uninstall /
// register paths so the watchdog driver (Task 9) sees the operator's
// intended state. Per the plan §65 acceptance gates the wiring honors
// these per-command timing + fail-handling rules:
//
//	mcphub stop --server X            BEFORE kill   fail-closed both
//	                                                ways (intent OR
//	                                                audit fail incl.
//	                                                ErrIdentityOversize)
//	mcphub stop --server X --force    skip intent   fail-closed if
//	                                                audit fails (incl.
//	                                                ErrIdentityOversize)
//	mcphub install <s>                AUDIT-FIRST   fail-closed; install
//	                                  (§62 v12)     rejected if audit
//	                                                fails; end state
//	                                                identical to never-
//	                                                attempted install
//	mcphub register <ws> <lang>       AFTER PASS    log warning + cont.
//	mcphub restart                    AFTER /Run    log warning + cont.
//	mcphub uninstall                  BEFORE delete log + proceed
//
// Every command-side audit entry uses the canonical Action label
// constants below (AuditAction*) so plan filters and status renders
// can pivot on Action without parsing free-form text.
//
// This file does NOT touch scheduler.New() — fail-closed semantics
// are implemented BEFORE any scheduler call so a rejected audit can
// abort without leaking partial scheduler state. Test path uses the
// daemonStateRootOverride seam (Task 1) + appendIntentAuditFn seam
// (Task 0/3) to redirect persistence and inject deterministic
// failures.
package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"mcp-local-hub/internal/config"
)

// ---------------------------------------------------------------------------
// Canonical audit actions (plan §10, §51, §62, §65).
// ---------------------------------------------------------------------------

const AuditActionUserStop = "user-stop"
const AuditActionForcedStopWithoutIntent = "forced-stop-without-intent"
const AuditActionServerInstall = "server-install"
const AuditActionServerRestarted = "server-restarted"
const AuditActionServerUninstalled = "server-uninstalled"
const AuditActionWorkspaceRegistered = "workspace-registered"
const AuditActionWatchdogInstallElevatedOverride = "watchdog-install-elevated-override"
const AuditActionStopFailedNoKill = "stop-failed-no-kill"

const (
	auditWhoMcphubStop         = "mcphub stop"
	auditWhoMcphubStopAll      = "mcphub stop --all"
	auditWhoMcphubStopForce    = "mcphub stop --force"
	auditWhoMcphubInstall      = "mcphub install"
	auditWhoMcphubRestart      = "mcphub restart"
	auditWhoMcphubUninstall    = "mcphub uninstall"
	auditWhoMcphubRegister     = "mcphub register"
	auditWhoMcphubSetup        = "mcphub setup"
	auditWhoMcphubWatchdogInst = "mcphub watchdog install"
)

const AuditWhoMcphubSetup = auditWhoMcphubSetup
const AuditWhoMcphubWatchdogInstall = auditWhoMcphubWatchdogInst

type StopOpts struct {
	Server       string
	DaemonFilter string
	Force        bool
}

func (a *API) StopWithOpts(opts StopOpts) ([]RestartResult, error) {
	taskNames, err := stopIntentTaskNamesForServer(opts.Server, opts.DaemonFilter)
	if err != nil {
		recordStopFailedNoKill(opts.Server, opts.Force, err)
		return nil, err
	}
	if err := a.recordStopIntent(taskNames, opts.Force); err != nil {
		return nil, err
	}
	if opts.Force {
		supResults, supervisorHandled, err := stopForceKillSupervisorOwned(context.Background(), opts.Server, opts.DaemonFilter)
		if err != nil {
			return nil, err
		}
		handledTasks := schedulerBlockedRestartTaskNames(supResults)
		killResults, err := a.stopKillCore(opts.Server, opts.DaemonFilter, handledTasks)
		if err != nil {
			if supervisorHandled && schedulerUnavailableError(err) {
				return supResults, nil
			}
			return nil, err
		}
		return append(supResults, killResults...), nil
	}
	return a.stopSupervisorAwareKill(opts.Server, opts.DaemonFilter)
}

func (a *API) recordStopIntent(taskNames []string, force bool) error {
	return a.recordStopIntentAs(taskNames, force, auditWhoMcphubStop)
}

func (a *API) recordStopIntentAs(taskNames []string, force bool, who string) error {
	now := time.Now().UTC()
	for _, tn := range taskNames {
		canonical := canonicalIntentTaskKey(tn)
		if force {
			entry := NewIntentAuditEntry(
				WithAction(AuditActionForcedStopWithoutIntent),
				WithTask(canonical),
				WithWho(auditWhoMcphubStopForce),
				WithPriority("high"),
				WithReason("operator forced stop without recording intent"),
			)
			if err := emitCommandAudit(entry); err != nil {
				return fmt.Errorf("forced-stop audit failed for %s: %w", canonical, err)
			}
			continue
		}
		intent := DaemonIntent{
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUserStop,
			UpdatedAt: now,
		}
		if err := a.WriteStopIntent(canonical, intent, who); err != nil {
			return fmt.Errorf("stop intent failed for %s: %w", canonical, err)
		}
		entry := NewIntentAuditEntry(
			WithAction(AuditActionUserStop),
			WithTask(canonical),
			WithWho(who),
			WithPriority("high"),
			WithReason(IntentReasonUserStop),
		)
		if err := emitCommandAudit(entry); err != nil {
			return fmt.Errorf("user-stop audit failed for %s: %w", canonical, err)
		}
	}
	return nil
}

func emitCommandAudit(entry IntentAuditEntry) error {
	if appendIntentAuditFn == nil {
		return nil
	}
	return appendIntentAuditFn(entry)
}

func recordStopFailedNoKill(server string, force bool, cause error) {
	who := auditWhoMcphubStop
	if force {
		who = auditWhoMcphubStopForce
	}
	reason := fmt.Sprintf("server=%s: %v", server, cause)
	entry := NewIntentAuditEntry(
		WithAction(AuditActionStopFailedNoKill),
		WithTask(""),
		WithWho(who),
		WithPriority("high"),
		WithReason(reason),
	)
	_ = emitCommandAudit(entry)
}

func installAuditTaskNames(m *config.ServerManifest, daemonFilter string) []string {
	out := make([]string, 0, len(m.Daemons))
	for _, d := range m.Daemons {
		if daemonFilter != "" && d.Name != daemonFilter {
			continue
		}
		out = append(out, "mcp-local-hub-"+m.Name+"-"+d.Name)
	}
	return out
}

func (a *API) recordInstallAuditPreMutation(m *config.ServerManifest, daemonFilter string) error {
	return a.recordInstallAuditForTasks(installAuditTaskNames(m, daemonFilter))
}

// recordInstallAuditForTasks emits every fail-closed server-install audit first.
// Only after the complete audit set succeeds does it mark a matching provider
// adoption as Install-started. The marker write is itself fail-closed and runs
// before installPlan enters executeInstallTo, so no scheduler/client/intent
// mutation can occur while durable provider history still says not_started.
func (a *API) recordInstallAuditForTasks(taskNames []string) error {
	for _, tn := range taskNames {
		canonical := canonicalIntentTaskKey(tn)
		entry := NewIntentAuditEntry(
			WithAction(AuditActionServerInstall),
			WithTask(canonical),
			WithWho(auditWhoMcphubInstall),
			WithReason(IntentReasonInstall),
		)
		if err := emitCommandAudit(entry); err != nil {
			return fmt.Errorf("install audit failed for %s (refusing to proceed; manifest may have malicious oversized identifier): %w", canonical, err)
		}
	}
	for _, tn := range taskNames {
		canonical := canonicalIntentTaskKey(tn)
		if err := markProviderInstallStartedForTask(canonical); err != nil {
			return fmt.Errorf("provider install phase failed for %s (refusing to proceed before install mutation): %w", canonical, err)
		}
	}
	return nil
}

func installAuditTaskNamesOrOverride(m *config.ServerManifest, daemonFilter string, override []string) []string {
	if len(override) > 0 {
		return override
	}
	return installAuditTaskNames(m, daemonFilter)
}

func (a *API) recordInstallIntentPostSuccess(m *config.ServerManifest, daemonFilter string, w io.Writer) {
	now := time.Now().UTC()
	for _, tn := range installAuditTaskNames(m, daemonFilter) {
		canonical := canonicalIntentTaskKey(tn)
		intent := DaemonIntent{
			Desired:   IntentDesiredRunning,
			Reason:    IntentReasonInstall,
			UpdatedAt: now,
		}
		if err := a.WriteStopIntent(canonical, intent, auditWhoMcphubInstall); err != nil {
			if w != nil {
				fmt.Fprintf(w, "warning: write install intent for %s: %v\n", canonical, err)
			}
		}
	}
}

func (a *API) recordRestartIntentForTask(taskName string, w io.Writer) {
	now := time.Now().UTC()
	canonical := canonicalIntentTaskKey(taskName)
	intent := DaemonIntent{
		Desired:   IntentDesiredRunning,
		Reason:    IntentReasonInstall,
		UpdatedAt: now,
	}
	if err := a.WriteStopIntent(canonical, intent, auditWhoMcphubRestart); err != nil {
		if w != nil {
			fmt.Fprintf(w, "warning: write restart intent for %s: %v\n", canonical, err)
		}
	}
	entry := NewIntentAuditEntry(
		WithAction(AuditActionServerRestarted),
		WithTask(canonical),
		WithWho(auditWhoMcphubRestart),
		WithReason("operator-initiated restart"),
	)
	if err := emitCommandAudit(entry); err != nil {
		if w != nil {
			fmt.Fprintf(w, "warning: write restart audit for %s: %v\n", canonical, err)
		}
	}
}

func (a *API) recordUninstallIntentForTasks(taskNames []string, w io.Writer) {
	now := time.Now().UTC()
	for _, tn := range taskNames {
		canonical := canonicalIntentTaskKey(tn)
		intent := DaemonIntent{
			Desired:   IntentDesiredStopped,
			Reason:    IntentReasonUninstalled,
			UpdatedAt: now,
		}
		if err := a.WriteStopIntent(canonical, intent, auditWhoMcphubUninstall); err != nil {
			if w != nil {
				fmt.Fprintf(w, "warning: write uninstall intent for %s: %v\n", canonical, err)
			}
		}
		entry := NewIntentAuditEntry(
			WithAction(AuditActionServerUninstalled),
			WithTask(canonical),
			WithWho(auditWhoMcphubUninstall),
			WithReason(IntentReasonUninstalled),
		)
		if err := emitCommandAudit(entry); err != nil {
			if w != nil {
				fmt.Fprintf(w, "warning: write uninstall audit for %s: %v\n", canonical, err)
			}
		}
	}
}

func enrollRegisterWorkspaceAudit(transaction *registrationTransaction, taskName string) {
	canonical := canonicalIntentTaskKey(taskName)
	entry := NewIntentAuditEntry(
		WithAction(AuditActionWorkspaceRegistered),
		WithTask(canonical),
		WithWho(auditWhoMcphubRegister),
		WithReason(IntentReasonRegister),
	)
	transaction.AddAfterCommit("workspace-registered "+canonical, func() error {
		return emitCommandAudit(entry)
	})
}

func (a *API) writeRegisterRunningIntentForTask(
	taskName string,
	enroll stopIntentCompensationSink,
) (string, error) {
	canonical := canonicalIntentTaskKey(taskName)
	intent := DaemonIntent{
		Desired:   IntentDesiredRunning,
		Reason:    IntentReasonRegister,
		UpdatedAt: time.Now().UTC(),
	}
	return canonical, a.writeStopIntentWithCompensation(
		canonical,
		intent,
		auditWhoMcphubRegister,
		enroll,
	)
}

func stopTaskNamesForServer(server, daemonFilter string) ([]string, error) {
	if server == "" {
		return nil, errors.New("stop: server is required")
	}
	if server == BuiltinRouteServer {
		if isBuiltinRouteTargetSelector(server, daemonFilter) {
			return []string{BuiltinRouteTaskName}, nil
		}
		return nil, fmt.Errorf("stop: built-in route daemon %q is not available (only %q)", daemonFilter, BuiltinRouteDaemonName)
	}
	data, err := loadManifestYAMLEmbedFirst(server)
	if err != nil {
		return nil, fmt.Errorf("stop: load manifest %s: %w", server, err)
	}
	m, err := parseManifestForName(server, data)
	if err != nil {
		return nil, fmt.Errorf("stop: parse manifest %s: %w", server, err)
	}
	if m.Kind == config.KindWorkspaceScoped {
		regPath, regErr := DefaultRegistryPath()
		if regErr != nil {
			return nil, fmt.Errorf("stop: resolve workspace registry path for %s: %w", server, regErr)
		}
		reg := NewRegistry(regPath)
		if err := reg.Load(); err != nil {
			return nil, fmt.Errorf("stop: load workspace registry for %s: %w", server, err)
		}
		var out []string
		for _, e := range reg.Workspaces {
			tn := e.TaskName
			if tn == "" {
				continue
			}
			if e.Language == SerenaLanguageSentinel {
				continue
			}
			if daemonFilter != "" && e.Language != daemonFilter {
				continue
			}
			out = append(out, tn)
		}
		return out, nil
	}
	var out []string
	for _, d := range m.Daemons {
		if daemonFilter != "" && d.Name != daemonFilter {
			continue
		}
		out = append(out, "mcp-local-hub-"+m.Name+"-"+d.Name)
	}
	return out, nil
}

func stopIntentTaskNamesForServer(server, daemonFilter string) ([]string, error) {
	taskNames, err := stopTaskNamesForServer(server, daemonFilter)
	if err != nil {
		return nil, err
	}
	targets, err := loadSupervisorOwnedTargets(server, daemonFilter)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(taskNames)+len(targets))
	out := make([]string, 0, len(taskNames)+len(targets))
	for _, name := range taskNames {
		key := strings.TrimPrefix(canonicalIntentTaskKey(name), `\`)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	for _, d := range targets {
		key := strings.TrimPrefix(canonicalIntentTaskKey(d.TaskName), `\`)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out, nil
}

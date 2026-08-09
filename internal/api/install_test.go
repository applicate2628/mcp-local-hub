package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// genericMultiDaemonManifest returns a generic manifest with the same daemon
// shape many install planner tests need:
// 3 daemons, weekly refresh, and client bindings where Claude, Gemini,
// Antigravity, Cursor, VS Code, and Qwen share the claude-compatible daemon
// while Codex keeps its own daemon.
func genericMultiDaemonManifest() *config.ServerManifest {
	return &config.ServerManifest{
		Name:      "multidaemon-fixture",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		Daemons: []config.DaemonSpec{
			{Name: "claude", Port: 9121},
			{Name: "codex", Port: 9122},
			{Name: "antigravity", Port: 9123},
		},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "claude", URLPath: "/mcp"},
			{Client: "codex-cli", Daemon: "codex", URLPath: "/mcp"},
			{Client: "antigravity", Daemon: "claude", URLPath: "/mcp"},
			{Client: "gemini-cli", Daemon: "claude", URLPath: "/mcp"},
			{Client: "cursor", Daemon: "claude", URLPath: "/mcp"},
			{Client: "vscode", Daemon: "claude", URLPath: "/mcp"},
			{Client: "qwen-cli", Daemon: "claude", URLPath: "/mcp"},
		},
		WeeklyRefresh: true,
	}
}

func serenaManifest() *config.ServerManifest {
	m := genericMultiDaemonManifest()
	m.Name = "serena"
	return m
}

func TestBuildPlanWithOpts_SerenaInstallWritePlaneUsesRouterURL(t *testing.T) {
	m := serenaManifest()
	wantURL := SerenaRouterClientURL(9125)

	p, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true, GUIPort: 9125})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts: %v", err)
	}
	if len(p.ClientUpdates) == 0 {
		t.Fatal("ClientUpdates empty; want serena client writes through the router URL")
	}
	for _, u := range p.ClientUpdates {
		if u.URL != wantURL {
			t.Fatalf("client %s URL = %q, want %q", u.Client, u.URL, wantURL)
		}
		if strings.Contains(u.URL, ":9121") {
			t.Fatalf("client %s URL contains legacy 9121 port: %q", u.Client, u.URL)
		}
		if u.Client == "antigravity" && u.RelayURL != wantURL {
			t.Fatalf("antigravity RelayURL = %q, want %q", u.RelayURL, wantURL)
		}
	}
}

func TestBuildPlanWithOpts_SerenaInstallWritePlaneSkipsWhenGUIPortUnknown(t *testing.T) {
	m := serenaManifest()

	p, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true, GUIPort: 0})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts: %v", err)
	}
	if len(p.ClientUpdates) != 0 {
		t.Fatalf("ClientUpdates = %+v, want no serena client writes without a live GUI port", p.ClientUpdates)
	}
	if len(p.Notices) != 1 {
		t.Fatalf("Notices = %v, want one serena deferred notice", p.Notices)
	}
	if !strings.Contains(p.Notices[0], "serena client entry deferred") {
		t.Fatalf("notice = %q, want serena deferred text", p.Notices[0])
	}
}

func TestBuildPlanWithOpts_NonSerenaIgnoresGUIPort(t *testing.T) {
	withoutPort := genericMultiDaemonManifest()
	withPort := genericMultiDaemonManifest()

	p0, err := BuildPlanWithOpts(withoutPort, BuildPlanOpts{IncludeAllClients: true, GUIPort: 0})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts(GUIPort 0): %v", err)
	}
	p9125, err := BuildPlanWithOpts(withPort, BuildPlanOpts{IncludeAllClients: true, GUIPort: 9125})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts(GUIPort 9125): %v", err)
	}
	if len(p0.ClientUpdates) != len(p9125.ClientUpdates) {
		t.Fatalf("ClientUpdates len with GUIPort 0 = %d, with 9125 = %d", len(p0.ClientUpdates), len(p9125.ClientUpdates))
	}
	for i := range p0.ClientUpdates {
		if p0.ClientUpdates[i].URL != p9125.ClientUpdates[i].URL {
			t.Fatalf("ClientUpdates[%d].URL with GUIPort 0 = %q, with 9125 = %q", i, p0.ClientUpdates[i].URL, p9125.ClientUpdates[i].URL)
		}
		if p9125.ClientUpdates[i].URL == SerenaRouterClientURL(9125) {
			t.Fatalf("non-serena client %s leaked serena router URL %q", p9125.ClientUpdates[i].Client, p9125.ClientUpdates[i].URL)
		}
	}
}

func TestBuildPlanWithOpts_SerenaRouterEntryNotRevertedToLegacyURL(t *testing.T) {
	m := serenaManifest()
	existingRouterURL := SerenaRouterClientURL(9125)

	p, err := BuildPlanWithOpts(m, BuildPlanOpts{ClientsInclude: []string{"claude-code"}, GUIPort: 9125})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts: %v", err)
	}
	if len(p.ClientUpdates) != 1 {
		t.Fatalf("ClientUpdates = %+v, want one claude-code update", p.ClientUpdates)
	}
	if got := p.ClientUpdates[0].URL; got != existingRouterURL {
		t.Fatalf("planned URL = %q, want existing router URL %q", got, existingRouterURL)
	}
	if strings.Contains(p.ClientUpdates[0].URL, ":9121") {
		t.Fatalf("planned URL reverted to legacy 9121 form: %q", p.ClientUpdates[0].URL)
	}
}

func TestExecuteInstallToPreservesPendingRollbackBackupsUntilRollback(t *testing.T) {
	entry := "rollbackkeep"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, `[mcp_servers.rollbackkeep]
command = "go"
args = ["orig"]
`)
	m := &config.ServerManifest{Name: entry}
	plan := &Plan{
		ClientUpdates: []ClientUpdatePlan{
			{Client: "codex-cli", URL: "http://127.0.0.1:9310/mcp"},
			{Client: "codex-cli", URL: "http://127.0.0.1:9311/mcp"},
		},
	}

	var out bytes.Buffer
	err := executeInstallTo(&out, m, plan, 1, false, func() (func(), error) {
		return nil, errors.New("synthetic post-client failure")
	}, true, true)
	if err == nil || !strings.Contains(err.Error(), "synthetic post-client failure") {
		t.Fatalf("executeInstallTo err = %v, want synthetic post-client failure\noutput:\n%s", err, out.String())
	}

	after, readErr := os.ReadFile(codexPath)
	if readErr != nil {
		t.Fatalf("read codex config after rollback: %v", readErr)
	}
	configText := string(after)
	for _, want := range []string{"command", "go", "args", "orig"} {
		if !strings.Contains(configText, want) {
			t.Fatalf("rollback did not restore original codex entry; missing %q:\n%s\noutput:\n%s", want, configText, out.String())
		}
	}
	if strings.Contains(configText, "startup_timeout_sec") {
		t.Fatalf("rollback did not restore original codex entry:\n%s\noutput:\n%s", configText, out.String())
	}
	if strings.Contains(configText, "9310") || strings.Contains(configText, "9311") {
		t.Fatalf("rollback left installed URL after failure:\n%s\noutput:\n%s", configText, out.String())
	}
}

func TestInstallPruneReleaseFailureIsPostCommitWarning(t *testing.T) {
	entry := "prune-warning"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, "[mcp_servers.prune-warning]\ncommand = \"go\"\nargs = [\"orig\"]\n")
	releaseCause := errors.New("injected config lock release failure")
	fake := newInstallFakeScheduler()
	var events []string
	fake.runHook = func(name string) { events = append(events, "run:"+name) }
	installFakeScheduler(t, fake)
	previousPrune := pruneBackupsForBackupPath
	pruneBackupsForBackupPath = func(string, int) error {
		if len(fake.createdSpecs) != 1 {
			t.Fatalf("prune warning observed before exactly one planned task was created: %+v", fake.createdSpecs)
		}
		events = append(events, "create:"+fake.createdSpecs[0].Name, "prune-warning")
		return errors.Join(clients.ErrConfigLockReleaseUnconfirmed, releaseCause)
	}
	t.Cleanup(func() { pruneBackupsForBackupPath = previousPrune })

	m := &config.ServerManifest{Name: entry}
	taskName := "mcp-local-hub-prune-warning-default"
	plan := &Plan{
		SchedulerTasks: []ScheduledTaskPlan{{Name: taskName, Command: "go", Args: []string{"serve"}, Trigger: "At logon"}},
		ClientUpdates:  []ClientUpdatePlan{{Client: "codex-cli", URL: "http://127.0.0.1:9310/mcp"}},
	}
	var out bytes.Buffer
	if err := executeInstallTo(&out, m, plan, 1, true, nil, false, false); err != nil {
		t.Fatalf("executeInstallTo returned post-commit prune failure: %v\noutput:\n%s", err, out.String())
	}
	committed, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(committed, []byte("9310")) {
		t.Fatalf("client bytes were rolled back after retention warning:\n%s", committed)
	}
	if got := out.String(); !strings.Contains(got, "warning: backup retention lock lifecycle failed; install changes remain committed:") ||
		!strings.Contains(got, releaseCause.Error()) ||
		!strings.Contains(got, "Install complete with warnings.") ||
		strings.Contains(got, "\nInstall complete.\n") {
		t.Fatalf("post-commit warning output mismatch:\n%s", got)
	}
	wantEvents := []string{"create:" + taskName, "prune-warning", "run:" + taskName}
	if got := strings.Join(events, "|"); got != strings.Join(wantEvents, "|") {
		t.Fatalf("post-commit event order = %q, want %q", got, strings.Join(wantEvents, "|"))
	}
}

func classifyInstallReleaseForTest(t *testing.T, releaseCause error) {
	t.Helper()
	previous := classifyInstallClientMutation
	classifyInstallClientMutation = func(err error) clients.ClientMutationSettlement {
		if errors.Is(err, releaseCause) {
			return clients.ClientMutationAppliedReleaseUnconfirmed
		}
		return clients.ClassifyClientMutation(err)
	}
	t.Cleanup(func() { classifyInstallClientMutation = previous })
}

func TestInstallAppliedReleaseUnconfirmedForwardCommitsIntent(t *testing.T) {
	entry := "install-forward-release"
	codexPath, cursorPath, _, _ := seedTwoPresentClients(t, entry)
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntryAppliedReleaseUnconfirmed},
	})
	classifyInstallReleaseForTest(t, inducedAddEntryReleaseUnconfirmed)

	fake := newInstallFakeScheduler()
	installFakeScheduler(t, fake)
	previousPrune := pruneBackupsForBackupPath
	pruneCalls := 0
	pruneBackupsForBackupPath = func(string, int) error {
		pruneCalls++
		return nil
	}
	t.Cleanup(func() { pruneBackupsForBackupPath = previousPrune })
	taskName := "mcp-local-hub-install-forward-release-default"
	plan := &Plan{
		SchedulerTasks: []ScheduledTaskPlan{{Name: taskName, Command: "go", Args: []string{"serve"}, Trigger: "At logon"}},
		ClientUpdates: []ClientUpdatePlan{
			{Client: "codex-cli", URL: "http://127.0.0.1:9312/mcp"},
			{Client: "cursor", URL: "http://127.0.0.1:9312/mcp"},
		},
	}
	intentPath := filepath.Join(t.TempDir(), "supervisor-intent.json")
	intentCalls := 0
	undoCalled := false
	intermediate := func() (func(), error) {
		intentCalls++
		if err := os.WriteFile(intentPath, []byte("committed"), 0o600); err != nil {
			return nil, err
		}
		return func() {
			undoCalled = true
			_ = os.Remove(intentPath)
		}, nil
	}

	var out bytes.Buffer
	err := executeInstallTo(&out, &config.ServerManifest{Name: entry}, plan, 1, false, intermediate, true, false)
	if !errors.Is(err, clients.ErrConfigLockReleaseUnconfirmed) || !errors.Is(err, inducedAddEntryReleaseUnconfirmed) {
		t.Fatalf("executeInstallTo error = %v, want applied release lifecycle error", err)
	}
	if !strings.Contains(err.Error(), "configuration applied; lock release unconfirmed") {
		t.Fatalf("executeInstallTo error lacks forward-only context: %v", err)
	}
	var forwardCommitted *InstallForwardCommittedError
	if !errors.As(err, &forwardCommitted) || forwardCommitted.Client != "codex-cli" {
		t.Fatalf("forward outcome = %#v err=%v, want codex-cli InstallForwardCommittedError", forwardCommitted, err)
	}
	var rollbackIncomplete *InstallClientRollbackIncompleteError
	if errors.As(err, &rollbackIncomplete) {
		t.Fatalf("forward-only applied mutation was reported rollback-incomplete: %v", err)
	}
	if got := seam.writeCount(codexPath); got != 1 {
		t.Fatalf("codex write count = %d, want one applied AddEntry and no same-leaf restore", got)
	}
	if got := seam.writeCount(cursorPath); got != 0 {
		t.Fatalf("later optional cursor mutation ran after poisoned codex leaf: writes=%d", got)
	}
	if raw, readErr := os.ReadFile(codexPath); readErr != nil || !bytes.Contains(raw, []byte("9312")) {
		t.Fatalf("applied codex target = %q err=%v, want committed URL", raw, readErr)
	}
	if intentCalls != 1 || undoCalled {
		t.Fatalf("mandatory intent settlement calls=%d undoCalled=%v, want one committed write and no compensation", intentCalls, undoCalled)
	}
	if raw, readErr := os.ReadFile(intentPath); readErr != nil || string(raw) != "committed" {
		t.Fatalf("committed intent = %q err=%v", raw, readErr)
	}
	if !fake.tasks[taskName] {
		t.Fatalf("scheduler task was rolled back after applied client target: tasks=%v", fake.tasks)
	}
	if pruneCalls != 0 || fake.runCount != 0 {
		t.Fatalf("optional post-commit work ran after poisoned leaf: prune=%d run=%d", pruneCalls, fake.runCount)
	}
}

func TestInstallAppliedReleaseUnconfirmedIntentFailureReturnsBothWithoutRollback(t *testing.T) {
	entry := "install-forward-intent-failure"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\ncommand = \"go\"\nargs = [\"orig\"]\n")
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntryAppliedReleaseUnconfirmed},
	})
	classifyInstallReleaseForTest(t, inducedAddEntryReleaseUnconfirmed)
	fake := newInstallFakeScheduler()
	installFakeScheduler(t, fake)
	taskName := "mcp-local-hub-install-forward-intent-failure-default"
	plan := &Plan{
		SchedulerTasks: []ScheduledTaskPlan{{Name: taskName, Command: "go", Args: []string{"serve"}, Trigger: "At logon"}},
		ClientUpdates:  []ClientUpdatePlan{{Client: "codex-cli", URL: "http://127.0.0.1:9315/mcp"}},
	}
	intentCause := errors.New("induced mandatory intent failure")

	var out bytes.Buffer
	err := executeInstallTo(&out, &config.ServerManifest{Name: entry}, plan, 1, false, func() (func(), error) {
		return nil, intentCause
	}, true, false)
	if !errors.Is(err, inducedAddEntryReleaseUnconfirmed) || !errors.Is(err, intentCause) {
		t.Fatalf("executeInstallTo error = %v, want client release and intent causes", err)
	}
	var forwardCommitted *InstallForwardCommittedError
	if !errors.As(err, &forwardCommitted) || forwardCommitted.Client != "codex-cli" {
		t.Fatalf("forward outcome = %#v err=%v, want codex-cli InstallForwardCommittedError", forwardCommitted, err)
	}
	if got := seam.writeCount(codexPath); got != 1 {
		t.Fatalf("codex write count = %d, want no rollback after intent failure", got)
	}
	if raw, readErr := os.ReadFile(codexPath); readErr != nil || !bytes.Contains(raw, []byte("9315")) {
		t.Fatalf("applied codex target = %q err=%v, want committed URL", raw, readErr)
	}
	if !fake.tasks[taskName] {
		t.Fatalf("scheduler task was rolled back after mandatory intent failure: tasks=%v", fake.tasks)
	}
}

func TestInstallRollbackRestoreAppliedReleaseUnconfirmedIsReconciled(t *testing.T) {
	entry := "install-restore-release"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\ncommand = \"go\"\nargs = [\"orig\"]\n")
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntrySucceed, restore: restoreAppliedReleaseUnconfirmed},
	})
	classifyInstallReleaseForTest(t, inducedRestoreReleaseUnconfirmed)
	intermediateCause := errors.New("induced post-client intent failure")
	plan := &Plan{ClientUpdates: []ClientUpdatePlan{{Client: "codex-cli", URL: "http://127.0.0.1:9313/mcp"}}}

	var out bytes.Buffer
	err := executeInstallTo(&out, &config.ServerManifest{Name: entry}, plan, 1, false, func() (func(), error) {
		return nil, intermediateCause
	}, true, true)
	if !errors.Is(err, intermediateCause) || !errors.Is(err, clients.ErrConfigLockReleaseUnconfirmed) || !errors.Is(err, inducedRestoreReleaseUnconfirmed) {
		t.Fatalf("executeInstallTo error = %v, want intent and restore lifecycle causes", err)
	}
	var rollbackIncomplete *InstallClientRollbackIncompleteError
	if errors.As(err, &rollbackIncomplete) {
		t.Fatalf("applied restore was falsely reported rollback-incomplete: %v", err)
	}
	var forwardCommitted *InstallForwardCommittedError
	if errors.As(err, &forwardCommitted) {
		t.Fatalf("reconciled rollback restore was falsely reported forward-committed: %v", err)
	}
	if got := seam.writeCount(codexPath); got != 2 {
		t.Fatalf("codex write count = %d, want AddEntry plus exactly one restore", got)
	}
	if raw, readErr := os.ReadFile(codexPath); readErr != nil || !bytes.Contains(raw, []byte("orig")) || bytes.Contains(raw, []byte("9313")) {
		t.Fatalf("rollback restore bytes = %q err=%v, want original entry", raw, readErr)
	}
}

func TestInstallAddEntryBodyErrorStillRunsPrearmedRestore(t *testing.T) {
	entry := "install-body-error-restore"
	codexPath, _, _ := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\ncommand = \"go\"\nargs = [\"orig\"]\n")
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{
		codexPath: {addEntry: addEntryFailMutated, restore: restoreSucceed},
	})
	plan := &Plan{ClientUpdates: []ClientUpdatePlan{{Client: "codex-cli", URL: "http://127.0.0.1:9314/mcp"}}}

	var out bytes.Buffer
	err := executeInstallTo(&out, &config.ServerManifest{Name: entry}, plan, 1, false, nil, true, true)
	if err == nil || !strings.Contains(err.Error(), "induced mutated add-entry failure") {
		t.Fatalf("executeInstallTo error = %v, want ordinary mutated body failure", err)
	}
	var rollbackIncomplete *InstallClientRollbackIncompleteError
	if errors.As(err, &rollbackIncomplete) {
		t.Fatalf("ordinary body error restored cleanly but reported rollback-incomplete: %v", err)
	}
	var forwardCommitted *InstallForwardCommittedError
	if errors.As(err, &forwardCommitted) {
		t.Fatalf("ordinary body error was falsely reported forward-committed: %v", err)
	}
	if got := seam.writeCount(codexPath); got != 2 {
		t.Fatalf("codex write count = %d, want failed AddEntry plus pre-armed restore", got)
	}
	if raw, readErr := os.ReadFile(codexPath); readErr != nil || !bytes.Contains(raw, []byte("orig")) || bytes.Contains(raw, []byte("9314")) {
		t.Fatalf("ordinary body-error rollback bytes = %q err=%v, want original entry", raw, readErr)
	}
}

func TestPrintPlanTo_SerenaDryRunShowsRouterURLAndDeferredNotice(t *testing.T) {
	m := serenaManifest()
	wantURL := SerenaRouterClientURL(9125)

	withPort, err := BuildPlanWithOpts(m, BuildPlanOpts{ClientsInclude: []string{"claude-code"}, GUIPort: 9125})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts(GUIPort): %v", err)
	}
	var withPortOut bytes.Buffer
	if err := printPlanTo(&withPortOut, withPort); err != nil {
		t.Fatalf("printPlanTo(GUIPort): %v", err)
	}
	if !strings.Contains(withPortOut.String(), wantURL) {
		t.Fatalf("dry-run output missing serena router URL %q:\n%s", wantURL, withPortOut.String())
	}
	if strings.Contains(withPortOut.String(), ":9121") {
		t.Fatalf("dry-run output contains legacy serena client port:\n%s", withPortOut.String())
	}

	withoutPort, err := BuildPlanWithOpts(m, BuildPlanOpts{ClientsInclude: []string{"claude-code"}, GUIPort: 0})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts(no GUIPort): %v", err)
	}
	var withoutPortOut bytes.Buffer
	if err := printPlanTo(&withoutPortOut, withoutPort); err != nil {
		t.Fatalf("printPlanTo(no GUIPort): %v", err)
	}
	if !strings.Contains(withoutPortOut.String(), "serena client entry deferred: live hub router not resolvable") {
		t.Fatalf("dry-run output missing deferred notice:\n%s", withoutPortOut.String())
	}
}

func deferredSerenaNoticePlan() (*config.ServerManifest, *Plan) {
	m := &config.ServerManifest{
		Name:      serenaEntryName,
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
	}
	p := &Plan{
		Server:      m.Name,
		FullInstall: false,
		Notices:     []string{"serena client entry deferred: live hub router not resolvable"},
	}
	return m, p
}

func TestInstallPlan_SerenaRealInstallSurfacesDeferredNotice(t *testing.T) {
	preparePreflightBinaryChecks(t)
	m, p := deferredSerenaNoticePlan()

	var out bytes.Buffer
	if err := NewAPI().installPlan(context.Background(), m, p, installPlanOpts{Writer: &out, DryRun: false}); err != nil {
		t.Fatalf("installPlan(real): %v", err)
	}
	if !strings.Contains(out.String(), "serena client entry deferred: live hub router not resolvable") {
		t.Fatalf("real install output missing deferred notice:\n%s", out.String())
	}
	if n := strings.Count(out.String(), "serena client entry deferred"); n != 1 {
		t.Fatalf("deferred notice printed %d times on real install, want exactly 1:\n%s", n, out.String())
	}
}

func TestInstallPlan_SerenaDryRunDoesNotDoublePrintDeferredNotice(t *testing.T) {
	m, p := deferredSerenaNoticePlan()

	var out bytes.Buffer
	if err := NewAPI().installPlan(context.Background(), m, p, installPlanOpts{Writer: &out, DryRun: true}); err != nil {
		t.Fatalf("installPlan(dry-run): %v", err)
	}
	if n := strings.Count(out.String(), "serena client entry deferred"); n != 1 {
		t.Fatalf("deferred notice printed %d times on dry-run, want exactly 1:\n%s", n, out.String())
	}
}

func TestSchedulerUnavailableErrorRequiresTypedSentinel(t *testing.T) {
	stringOnly := errors.New("scheduler operation failed after hook said not implemented by policy")
	if SchedulerUnavailableError(stringOnly) {
		t.Fatalf("string-only scheduler error was treated as unavailable: %v", stringOnly)
	}
	if !SchedulerUnavailableError(fmt.Errorf("scheduler.New: %w", scheduler.ErrNotImplemented)) {
		t.Fatal("wrapped scheduler.ErrNotImplemented was not treated as unavailable")
	}
}

func preparePreflightBinaryChecks(t *testing.T) {
	t.Helper()
	origCanonical := testCanonicalMcphubPathOverride
	origShort := mcphubShortName
	t.Cleanup(func() {
		testCanonicalMcphubPathOverride = origCanonical
		mcphubShortName = origShort
	})
	binDir := t.TempDir()
	canonical := filepath.Join(binDir, "mcphub-test")
	if err := os.WriteFile(canonical, []byte(""), 0755); err != nil {
		t.Fatalf("write fake canonical mcphub: %v", err)
	}
	testCanonicalMcphubPathOverride = canonical
	mcphubShortName = "go"
}

func TestBuildPlan_NoFilter_FullInstall(t *testing.T) {
	m := genericMultiDaemonManifest()
	p, err := BuildPlanWithOpts(m, BuildPlanOpts{GUIPort: 9125})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// 3 daemon tasks + 1 weekly refresh = 4 scheduler tasks.
	if len(p.SchedulerTasks) != 4 {
		t.Errorf("len(SchedulerTasks) = %d, want 4", len(p.SchedulerTasks))
	}
	// Default install targets only safe/default clients.
	if len(p.ClientUpdates) != 2 {
		t.Errorf("len(ClientUpdates) = %d, want 2", len(p.ClientUpdates))
	}
	gotClients := map[string]bool{}
	for _, u := range p.ClientUpdates {
		gotClients[u.Client] = true
	}
	for _, want := range []string{"claude-code", "codex-cli"} {
		if !gotClients[want] {
			t.Errorf("default client %q missing from plan: %+v", want, p.ClientUpdates)
		}
	}
	// cursor is now OPT-IN (like vscode/gemini-cli) — a bare install must NOT
	// touch it; it is reachable only via --clients cursor / --all-clients.
	for _, optIn := range []string{"cursor", "gemini-cli", "antigravity", "qwen-cli", "vscode"} {
		if gotClients[optIn] {
			t.Errorf("opt-in client %q should not be in default plan: %+v", optIn, p.ClientUpdates)
		}
	}
	// Weekly refresh present.
	var sawWeekly bool
	for _, s := range p.SchedulerTasks {
		if strings.Contains(s.Name, "weekly-refresh") {
			sawWeekly = true
		}
	}
	if !sawWeekly {
		t.Error("weekly-refresh task missing in full install")
	}
}

func TestBuildPlan_AllClientsIncludesOptInClients(t *testing.T) {
	m := genericMultiDaemonManifest()
	p, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true, GUIPort: 9125})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts: %v", err)
	}
	if len(p.ClientUpdates) != 7 {
		t.Fatalf("len(ClientUpdates) = %d, want 7: %+v", len(p.ClientUpdates), p.ClientUpdates)
	}
	got := map[string]bool{}
	for _, u := range p.ClientUpdates {
		got[u.Client] = true
	}
	for _, want := range []string{"claude-code", "codex-cli", "cursor", "vscode", "gemini-cli", "qwen-cli", "antigravity"} {
		if !got[want] {
			t.Errorf("client %q missing from all-clients plan", want)
		}
	}
}

func TestBuildPlan_ClientFilterOnlyIncludesRequestedClients(t *testing.T) {
	m := genericMultiDaemonManifest()
	p, err := BuildPlanWithOpts(m, BuildPlanOpts{ClientsInclude: []string{"qwen-cli", "vscode"}, GUIPort: 9125})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts: %v", err)
	}
	if len(p.ClientUpdates) != 2 {
		t.Fatalf("len(ClientUpdates) = %d, want 2: %+v", len(p.ClientUpdates), p.ClientUpdates)
	}
	got := map[string]bool{}
	for _, u := range p.ClientUpdates {
		got[u.Client] = true
		if u.DaemonName != "claude" {
			t.Errorf("client %s daemon = %q, want claude", u.Client, u.DaemonName)
		}
	}
	if !got["qwen-cli"] || !got["vscode"] {
		t.Fatalf("filtered clients missing: %+v", p.ClientUpdates)
	}
	if got["gemini-cli"] || got["antigravity"] || got["claude-code"] || got["cursor"] || got["codex-cli"] {
		t.Fatalf("unexpected client in filtered plan: %+v", p.ClientUpdates)
	}
}

func TestBuildPlan_SingleDaemonFilter_SkipsOthersAndWeeklyRefresh(t *testing.T) {
	m := genericMultiDaemonManifest()
	p, err := BuildPlanWithOpts(m, BuildPlanOpts{DaemonFilter: "codex", GUIPort: 9125})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// Only the codex scheduler task; weekly refresh is skipped for partial installs.
	if len(p.SchedulerTasks) != 1 {
		t.Errorf("len(SchedulerTasks) = %d, want 1 (got: %+v)", len(p.SchedulerTasks), p.SchedulerTasks)
	}
	if len(p.SchedulerTasks) >= 1 && !strings.HasSuffix(p.SchedulerTasks[0].Name, "-codex") {
		t.Errorf("task name %q, want suffix -codex", p.SchedulerTasks[0].Name)
	}
	// Only codex-cli binding (it's the only binding referencing daemon codex).
	if len(p.ClientUpdates) != 1 {
		t.Errorf("len(ClientUpdates) = %d, want 1 (got: %+v)", len(p.ClientUpdates), p.ClientUpdates)
	}
	if len(p.ClientUpdates) >= 1 && p.ClientUpdates[0].Client != "codex-cli" {
		t.Errorf("client = %q, want codex-cli", p.ClientUpdates[0].Client)
	}
	if len(p.ClientUpdates) >= 1 && !strings.Contains(p.ClientUpdates[0].URL, ":9122/") {
		t.Errorf("url = %q, want port 9122", p.ClientUpdates[0].URL)
	}
}

func TestBuildPlan_SharedDaemonFilter_IncludesAllReferencingBindings(t *testing.T) {
	m := genericMultiDaemonManifest()
	// claude daemon is referenced by every non-Codex binding; all-clients mode
	// preserves that relationship when explicitly requested.
	p, err := BuildPlanWithOpts(m, BuildPlanOpts{DaemonFilter: "claude", IncludeAllClients: true, GUIPort: 9125})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(p.SchedulerTasks) != 1 {
		t.Errorf("len(SchedulerTasks) = %d, want 1", len(p.SchedulerTasks))
	}
	if len(p.ClientUpdates) != 6 {
		t.Errorf("len(ClientUpdates) = %d, want 6 (all non-Codex clients share claude daemon)", len(p.ClientUpdates))
	}
	saw := map[string]bool{}
	for _, u := range p.ClientUpdates {
		saw[u.Client] = true
	}
	for _, want := range []string{"claude-code", "gemini-cli", "antigravity", "cursor", "vscode", "qwen-cli"} {
		if !saw[want] {
			t.Errorf("expected %s binding; got: %+v", want, p.ClientUpdates)
		}
	}
}

func TestBuildPlan_UnknownDaemonFilter_Errors(t *testing.T) {
	m := genericMultiDaemonManifest()
	_, err := BuildPlan(m, "does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown daemon filter, got nil")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should mention the unknown daemon name, got: %v", err)
	}
}

func TestBuildPlan_InvalidClientURLPath_Errors(t *testing.T) {
	m := genericMultiDaemonManifest()
	m.ClientBindings[0].URLPath = "@evil.com/mcp"

	_, err := BuildPlan(m, "")
	if err == nil {
		t.Fatal("expected error for invalid url_path, got nil")
	}
	if !strings.Contains(err.Error(), "invalid url_path") {
		t.Fatalf("error = %v, want mention of invalid url_path", err)
	}
}

// TestPreflight_RespectsDaemonFilter ensures --daemon filter keeps Preflight
// from checking ports of unrelated daemons that may legitimately be occupied
// by a previous partial install.
//
// Setup: two daemons pointing at the SAME occupied port. With filter="second",
// the first daemon must be skipped and the error must reference only "second".
// With no filter, the first daemon is checked first and the error references
// "first".
func TestPreflight_RespectsDaemonFilter(t *testing.T) {
	preparePreflightBinaryChecks(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	occupiedPort := ln.Addr().(*net.TCPAddr).Port

	m := &config.ServerManifest{
		Name:      "testsrv",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "go", // on PATH whenever `go test` runs
		Daemons: []config.DaemonSpec{
			{Name: "first", Port: occupiedPort},
			{Name: "second", Port: occupiedPort},
		},
	}

	// Filter="second" — "first" must be skipped; error should mention only "second".
	err = Preflight(m, "second")
	if err == nil {
		t.Fatal("Preflight(m, 'second') = nil, want error (port occupied)")
	}
	if !strings.Contains(err.Error(), "second") {
		t.Errorf("error should reference 'second' daemon: %v", err)
	}
	if strings.Contains(err.Error(), "first") {
		t.Errorf("error should NOT reference filtered-out 'first' daemon: %v", err)
	}

	// No filter — "first" is checked first, must be in the message.
	err = Preflight(m, "")
	if err == nil {
		t.Fatal("Preflight(m, '') = nil, want error")
	}
	if !strings.Contains(err.Error(), "first") {
		t.Errorf("unfiltered error should reference 'first' daemon (iteration order): %v", err)
	}
}

// TestPreflight_ChecksInternalPortForNativeHTTP verifies that a native-http
// manifest fails preflight when the internal port (external + offset) is
// already bound, even if the external port itself is free. Without this
// check, install would persist scheduler/client config and then crash at
// runtime when HTTPHost tries to spawn its upstream.
func TestPreflight_ChecksInternalPortForNativeHTTP(t *testing.T) {
	preparePreflightBinaryChecks(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	occupiedInternal := ln.Addr().(*net.TCPAddr).Port
	// Pick an external port such that internal = external + offset hits
	// the occupied port. Working backward: external = occupied - offset.
	// We still need the external port itself to be free — allocate it
	// transiently and close before calling Preflight to confirm it's free.
	external := occupiedInternal - config.NativeHTTPInternalPortOffset
	if external < 1024 {
		t.Skipf("could not construct test ports from occupied=%d offset=%d", occupiedInternal, config.NativeHTTPInternalPortOffset)
	}

	m := &config.ServerManifest{
		Name:      "testsrv",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "default", Port: external}},
	}

	err = Preflight(m, "")
	if err == nil {
		t.Fatal("expected preflight error when internal port is bound")
	}
	if !strings.Contains(err.Error(), "internal port") {
		t.Errorf("error should mention 'internal port': %v", err)
	}
}

// TestPreflight_StdioBridgeIgnoresInternalPort asserts that the internal-port
// check is scoped to native-http. stdio-bridge transports have no second
// port and must not be rejected for something outside their scope.
func TestPreflight_StdioBridgeIgnoresInternalPort(t *testing.T) {
	preparePreflightBinaryChecks(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	occupied := ln.Addr().(*net.TCPAddr).Port
	external := occupied - config.NativeHTTPInternalPortOffset
	if external < 1024 {
		t.Skipf("could not construct test ports")
	}

	m := &config.ServerManifest{
		Name:      "testsrv",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "default", Port: external}},
	}

	if err := Preflight(m, ""); err != nil {
		t.Errorf("stdio-bridge preflight should pass (internal-port check is native-http only): %v", err)
	}
}

// TestPreflight_MissingSecretDoesNotBlockInstall verifies the OPTIONAL-secret
// policy (install-and-it-works): a manifest whose env declares a `secret:<key>`
// absent from the vault must NOT fail preflight. The daemon spawns best-effort
// (the unset env var is omitted, see daemonEnvWithOverlay) and the server
// reports its own missing-key; CheckServerReadiness surfaces the unset secret
// as an advisory inline-prompt field. Previously this HARD-BLOCKED install, so
// no secret-declaring server (wolfram etc.) could be installed without setting
// the key first — the reported operator bug.
func TestPreflight_MissingSecretDoesNotBlockInstall(t *testing.T) {
	preparePreflightBinaryChecks(t)
	t.Setenv("LOCALAPPDATA", t.TempDir())  // Windows path
	t.Setenv("XDG_DATA_HOME", t.TempDir()) // Linux path

	m := &config.ServerManifest{
		Name:      "secretless-server",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Env:       map[string]string{"API_KEY": "secret:nonexistent_key"},
		// A valid free fixed port — Preflight now range-checks fixed ports, so
		// the prior Port:0 placeholder would be rejected (Codex #377 r16).
		Daemons: []config.DaemonSpec{{Name: "default", Port: 51000}},
	}

	if err := Preflight(m, ""); err != nil {
		t.Fatalf("preflight must NOT block install on an optional missing secret; got: %v", err)
	}
}

func TestPreflight_MissingRequiredBinaryBlocks(t *testing.T) {
	preparePreflightBinaryChecks(t)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	// command (go) is on PATH so the launcher check passes, but the server-level
	// required_binary is absent. CLI/API installs call Preflight directly (never
	// CheckServerReadiness), so Preflight must block here BEFORE committing
	// client/supervisor state rather than letting the daemon fail at spawn
	// (Codex #377 r13 — Preflight↔readiness dependency parity, shared predicate
	// binaryAvailable).
	m := &config.ServerManifest{
		Name:             "needs-tool",
		Kind:             config.KindGlobal,
		Transport:        config.TransportStdioBridge,
		Command:          "go",
		RequiredBinaries: []string{"definitely-not-on-path-zzz"},
		Daemons:          []config.DaemonSpec{{Name: "default", Port: 0}},
	}
	err := Preflight(m, "")
	if err == nil {
		t.Fatal("Preflight must block install when a server-level required_binary is missing")
	}
	if !strings.Contains(err.Error(), "definitely-not-on-path-zzz") {
		t.Errorf("error should name the missing binary; got: %v", err)
	}
}

// TestPreflight_NoSecretsNeeded confirms manifests without any secret:
// references preflight cleanly even when no vault exists (fresh
// machine, user has not run `mcphub secrets init`).
func TestPreflight_NoSecretsNeeded(t *testing.T) {
	preparePreflightBinaryChecks(t)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // must be free for preflight

	m := &config.ServerManifest{
		Name:      "plain-server",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Env:       map[string]string{"PORT": "literal", "OTHER": "$MY_ENV_VAR_UNSET"},
		Daemons:   []config.DaemonSpec{{Name: "default", Port: port}},
	}

	// Preflight should succeed despite the $VAR ref because it is only
	// the secret: refs that are gated here (the $VAR check happens at
	// daemon launch where the contract is different).
	if err := Preflight(m, ""); err != nil {
		t.Errorf("preflight unexpectedly failed with no secret refs: %v", err)
	}
}

// TestPreflight_UnknownCommand ensures the command check runs regardless of filter.
func TestPreflight_UnknownCommand(t *testing.T) {
	m := &config.ServerManifest{
		Name:    "testsrv",
		Command: "this-binary-definitely-does-not-exist-mcp-local-hub",
		Daemons: []config.DaemonSpec{{Name: "x", Port: 1}},
	}
	if err := Preflight(m, "x"); err == nil {
		t.Error("expected error for missing command")
	}
}

// TestPreflight_MissingLauncherErrorCarriesFix verifies SEAM-B: a blocking
// preflight finding now returns a typed *AdmissionError whose Fix is the
// actionable guided fix (not just the cryptic Reason). The Reason is preserved
// as the Error() prefix so existing substring-asserting callers still pass.
func TestPreflight_MissingLauncherErrorCarriesFix(t *testing.T) {
	m := &config.ServerManifest{
		Name:    "missing-launcher",
		Command: "this-binary-definitely-does-not-exist-mcp-local-hub",
		Daemons: []config.DaemonSpec{{Name: "x", Port: 1}},
	}
	err := Preflight(m, "x")
	if err == nil {
		t.Fatal("Preflight must block on a missing launcher")
	}
	var ae *AdmissionError
	if !errors.As(err, &ae) {
		t.Fatalf("Preflight error is %T, want *AdmissionError", err)
	}
	if ae.Fix == "" {
		t.Fatalf("AdmissionError.Fix is empty; want the actionable guided fix; err=%v", err)
	}
	// LauncherGuidance owns the fix wording for the generic/unknown launcher;
	// assert the typed Fix matches that single owner.
	_, wantFix := LauncherGuidance(m.Command)
	if ae.Fix != wantFix {
		t.Fatalf("AdmissionError.Fix = %q, want %q", ae.Fix, wantFix)
	}
	// Error() must still carry BOTH the Reason (legacy substring callers) and
	// the appended Fix (the new actionable surface).
	if !strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("Error() lost the Reason substring; got: %v", err)
	}
	if !strings.Contains(err.Error(), wantFix) {
		t.Fatalf("Error() did not append the Fix; got: %v", err)
	}
}

// TestInstallAllInstallsEverything spawns a tempdir with two fake manifests
// and asserts Install is invoked for each (dry-run mode so no scheduler/
// client writes). Verifies InstallAllFrom returns one result per manifest.
//
// Ports must be OS-allocated (`net.Listen(":0")` via pickFreeLocalPort)
// rather than literal 9130/9131: dev workstations frequently have those
// ports in TIME_WAIT from prior test runs (or held by an installed
// daemon), and the install preflight rejects manifests whose port is
// already in use. Using pickFreeLocalPort matches the sibling test
// TestInstallAllFrom_PortConflictFailsThatServer below.
func TestInstallAllInstallsEverything(t *testing.T) {
	tmp := t.TempDir()
	fooPort := pickFreeLocalPort(t)
	barPort := pickFreeLocalPort(t)
	makeFakeManifest(t, filepath.Join(tmp, "foo"), "foo", fooPort)
	makeFakeManifest(t, filepath.Join(tmp, "bar"), "bar", barPort)
	preparePreflightBinaryChecks(t)

	a := NewAPI()
	var buf bytes.Buffer
	results := a.InstallAllFrom(InstallAllOpts{
		ManifestDir: tmp,
		DryRun:      true,
		Writer:      &buf,
	})
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("server %s: unexpected error %v", r.Server, r.Err)
		}
	}
}

func TestInstallAllFrom_PortConflictFailsThatServer(t *testing.T) {
	tmp := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	occupied := ln.Addr().(*net.TCPAddr).Port
	makeFakeManifest(t, filepath.Join(tmp, "busy"), "busy", occupied)
	makeFakeManifest(t, filepath.Join(tmp, "free"), "free", occupied+1)
	preparePreflightBinaryChecks(t)

	a := NewAPI()
	results := a.InstallAllFrom(InstallAllOpts{
		ManifestDir: tmp,
		DryRun:      true,
		Writer:      &bytes.Buffer{},
	})
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	byServer := map[string]error{}
	for _, r := range results {
		byServer[r.Server] = r.Err
	}
	if byServer["busy"] == nil {
		t.Fatalf("expected busy server to fail preflight for occupied port")
	}
	if !strings.Contains(byServer["busy"].Error(), "already in use") {
		t.Fatalf("busy error should mention occupied port, got: %v", byServer["busy"])
	}
	if byServer["free"] != nil {
		t.Fatalf("expected free server to pass, got: %v", byServer["free"])
	}
}

func TestInstallFromManifestDirRejectsYAMLNameMismatch(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "demo")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	body := `name: other
kind: global
transport: stdio-bridge
command: go
daemons:
  - name: default
    port: 0
client_bindings: []
weekly_refresh: false
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	err := NewAPI().installFromManifestDir(InstallOpts{
		Server: "demo",
		DryRun: true,
		Writer: &bytes.Buffer{},
	}, tmp)
	if err == nil {
		t.Fatal("expected YAML name mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), `manifest yaml name "other" must match requested server "demo"`) {
		t.Fatalf("error = %v, want YAML/requested name mismatch", err)
	}
}

func makeFakeManifest(t *testing.T, dir, name string, port int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// 'go' is guaranteed to be on PATH in every Go test environment.
	// Previously the fixture used 'echo', which works under Unix shells
	// but not on Windows where echo is a cmd.exe builtin, not a PE file
	// — exec.LookPath fails and install preflight rejects the manifest.
	body := fmt.Sprintf(`name: %s
kind: global
transport: stdio-bridge
command: go
daemons:
  - name: default
    port: %d
client_bindings: []
weekly_refresh: false
`, name, port)
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

// pickFreeLocalPort returns a 127.0.0.1 port that net.Listen succeeded
// on (and immediately closed). The kernel is unlikely to reuse the
// exact port within a few microseconds for a different listener, so
// the freshly-closed port is a reasonable "free" probe target. Tests
// that need the port held open should re-Listen before probing.
func pickFreeLocalPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// TestWaitForPortFree_FreePortReturnsImmediately asserts the DM-3
// helper returns nil on the first probe when nothing is listening —
// no spurious sleep delay in the common Restart path.
func TestWaitForPortFree_FreePortReturnsImmediately(t *testing.T) {
	port := pickFreeLocalPort(t)
	start := time.Now()
	if err := waitForPortFree(port, 3*time.Second); err != nil {
		t.Fatalf("expected nil on free port, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("free-port path was slow: elapsed=%v (must succeed on first probe)", elapsed)
	}
}

// TestWaitForPortFree_HeldPortTimesOut asserts that when the port
// stays bound, waitForPortFree returns an error after roughly the
// configured timeout. A daemon that fails to release would otherwise
// trigger a new daemon's bind to fail too — surfacing the wait error
// to the operator is more informative than dropping straight into
// `schtasks /Run` and letting it record last_result=1.
func TestWaitForPortFree_HeldPortTimesOut(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	start := time.Now()
	err = waitForPortFree(port, 300*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected timeout error on held port, got nil")
	}
	if !strings.Contains(err.Error(), "still in use") {
		t.Errorf("error must mention 'still in use'; got: %v", err)
	}
	// Lower bound: the loop must wait at least one full timeout window.
	if elapsed < 250*time.Millisecond {
		t.Errorf("timed out too soon: elapsed=%v, want >=250ms", elapsed)
	}
	// Upper bound: a generous tolerance for slow CI; primary assertion
	// is that we don't block forever.
	if elapsed > 3*time.Second {
		t.Errorf("timed out too late: elapsed=%v, want <3s", elapsed)
	}
}

// TestWaitForPortFree_PortReleasedDuringWait simulates the realistic
// TIME_WAIT race: the helper starts probing while the port is still
// held, the listener releases mid-wait, and the helper succeeds before
// the timeout. This is the entire reason DM-3 added the wait — the
// new daemon's bind would otherwise race the kernel's socket cleanup
// and lose.
func TestWaitForPortFree_PortReleasedDuringWait(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port

	// Release the port asynchronously after a short hold.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = l.Close()
	}()

	start := time.Now()
	if err := waitForPortFree(port, 3*time.Second); err != nil {
		t.Fatalf("expected port to free during wait, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("returned suspiciously fast: elapsed=%v (port was held %v)",
			elapsed, 150*time.Millisecond)
	}
}

// TestPreflight_RemoteHTTPGatedPending pins bot r2 P1 closure
// (PR #169): the new transport=remote-http schema validates (G6
// sub-PR 1 lands the validator), but the install pipeline can't
// process daemonless / command-less manifests yet. Preflight
// REJECTS with a clear "implementation pending" message so
// operators don't hit a confusing exec.LookPath failure further
// down. Sub-PR 2 of G6 wires the install branch and removes the
// gate.
// TestPreflight_RemoteHTTPAcceptsCanonicalMcphub pins G6 sub-PR 2:
// the Preflight gate for remote-http SHORT-CIRCUITS past command/
// port/scheduler checks but still requires the canonical mcphub
// binary (client configs reference its rotated-token reload path).
// No daemon allocation, no port checks, no command resolution.
func TestPreflight_RemoteHTTPAcceptsCanonicalMcphub(t *testing.T) {
	preparePreflightBinaryChecks(t)
	m := &config.ServerManifest{
		Name:      "ctx7",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://mcp.context7.com/mcp",
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code"},
		},
	}
	if err := Preflight(m, ""); err != nil {
		t.Fatalf("remote-http Preflight should succeed: %v", err)
	}
}

// TestPreflight_RemoteHTTPDoesNotRejectAntigravityAtPreflight pins
// bot r3 P2 closure on PR #170: the antigravity adapter rejection
// has moved from Preflight to buildRemoteHTTPPlan so the gate fires
// only against bindings ACTUALLY in scope for the install (after
// the includeClient predicate runs). This lets filtered installs
// of mixed-binding manifests (`--clients claude-code`) succeed even
// when the manifest also declares an antigravity binding.
func TestPreflight_RemoteHTTPDoesNotRejectAntigravityAtPreflight(t *testing.T) {
	preparePreflightBinaryChecks(t)
	m := &config.ServerManifest{
		Name:      "ctx7",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://mcp.context7.com/mcp",
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code"},
			{Client: "antigravity"},
		},
	}
	if err := Preflight(m, ""); err != nil {
		t.Errorf("Preflight must not reject mixed-binding manifest; antigravity gate belongs in BuildPlan: %v", err)
	}
}

// TestBuildPlanWithOpts_RemoteHTTPFilteredInstallSkipsAntigravity
// pins the filtered-install path: when `--clients claude-code`
// excludes antigravity from the install scope, BuildPlan succeeds
// and produces a plan for the supported clients only.
func TestBuildPlanWithOpts_RemoteHTTPFilteredInstallSkipsAntigravity(t *testing.T) {
	m := &config.ServerManifest{
		Name:      "ctx7",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://mcp.context7.com/mcp",
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code"},
			{Client: "antigravity"},
		},
	}
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{
		ClientsInclude: []string{"claude-code"},
	})
	if err != nil {
		t.Fatalf("filtered remote-http install excluding antigravity should succeed: %v", err)
	}
	if len(plan.ClientUpdates) != 1 {
		t.Fatalf("expected 1 client update (claude-code only); got %d", len(plan.ClientUpdates))
	}
	if plan.ClientUpdates[0].Client != "claude-code" {
		t.Errorf("client update = %q; want claude-code", plan.ClientUpdates[0].Client)
	}
}

// TestBuildPlanWithOpts_RemoteHTTPNoDaemonsNoSchedulerTasks pins
// the G6 sub-PR 2 install plan shape: zero scheduler tasks, one
// ClientUpdate per binding (excluding antigravity), URL +
// expanded Headers populated, no DaemonName.
func TestBuildPlanWithOpts_RemoteHTTPNoDaemonsNoSchedulerTasks(t *testing.T) {
	preparePreflightBinaryChecks(t)
	m := &config.ServerManifest{
		Name:      "ctx7",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://mcp.context7.com/mcp",
		Headers:   map[string]string{"X-Tenant": "acme"},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code"},
			{Client: "codex-cli"},
		},
	}
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts: %v", err)
	}
	if len(plan.SchedulerTasks) != 0 {
		t.Errorf("remote-http plan must have NO scheduler tasks; got %d", len(plan.SchedulerTasks))
	}
	if len(plan.ClientUpdates) != 2 {
		t.Fatalf("expected 2 client updates (claude-code + codex-cli); got %d", len(plan.ClientUpdates))
	}
	for _, u := range plan.ClientUpdates {
		if u.URL != "https://mcp.context7.com/mcp" {
			t.Errorf("client %q URL = %q; want manifest URL verbatim", u.Client, u.URL)
		}
		if u.Headers["X-Tenant"] != "acme" {
			t.Errorf("client %q X-Tenant header = %q; want acme", u.Client, u.Headers["X-Tenant"])
		}
		if u.DaemonName != "" {
			t.Errorf("client %q has DaemonName=%q on remote-http plan; want empty", u.Client, u.DaemonName)
		}
		if u.EntryName != "ctx7" {
			t.Errorf("client %q EntryName=%q; want manifest name 'ctx7'", u.Client, u.EntryName)
		}
	}
}

// TestBuildPlanWithOpts_RemoteHTTPMissingSecretFailsFast pins G6
// §"Install path" step 2: ${secret:KEY} expansion at install time
// fails BEFORE any client config is touched if a secret is missing.
func TestBuildPlanWithOpts_RemoteHTTPMissingSecretFailsFast(t *testing.T) {
	m := &config.ServerManifest{
		Name:      "ctx7",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://mcp.context7.com/mcp",
		Headers:   map[string]string{"Authorization": "Bearer ${secret:MISSING_KEY}"},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code"},
		},
	}
	// Route the lookup through DefaultSecretLookup which will hit
	// the (empty) production vault — MISSING_KEY won't be there.
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	_, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true})
	if err == nil {
		t.Fatal("expected missing-secret rejection at plan-build time; got nil")
	}
	if !strings.Contains(err.Error(), "MISSING_KEY") {
		t.Errorf("error must name the missing key; got %v", err)
	}
}

// TestBuildPlanWithOpts_RemoteHTTPRejectsDaemonFilter pins bot
// r1 P2 closure on PR #170: --daemon X is invalid on a remote-http
// manifest (no daemons exist). The plan-builder rejects so the
// flag never silently flips the install to "partial" semantics.
func TestBuildPlanWithOpts_RemoteHTTPRejectsDaemonFilter(t *testing.T) {
	m := &config.ServerManifest{
		Name:      "ctx7",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://mcp.context7.com/mcp",
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code"},
		},
	}
	_, err := BuildPlanWithOpts(m, BuildPlanOpts{
		DaemonFilter:      "default",
		IncludeAllClients: true,
	})
	if err == nil {
		t.Fatal("expected daemon-filter rejection on remote-http manifest; got nil")
	}
	if !strings.Contains(err.Error(), "remote-http") {
		t.Errorf("error must name remote-http; got %v", err)
	}
}

// TestExpectedHubURL_RemoteHTTPMatchesInstalledEntry pins bot r1
// P1 closure on PR #170: uninstall ownership detection must
// recognize remote-http entries that the install path wrote. We
// expand the manifest URL the same way install did so the result
// matches the entry's URL field.
func TestExpectedHubURL_RemoteHTTPMatchesInstalledEntry(t *testing.T) {
	m := &config.ServerManifest{
		Name:      "ctx7",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://mcp.context7.com/mcp", // no secrets — clean expansion
	}
	got := expectedHubURL(m, config.ClientBinding{Client: "claude-code"})
	if got != "https://mcp.context7.com/mcp" {
		t.Errorf("expectedHubURL for remote-http = %q; want manifest URL verbatim", got)
	}
}

// TestExpectedHubURL_RemoteHTTPHandlesMissingSecretsGracefully pins
// the residual case from bot r1 P1: if the manifest URL contains
// a ${secret:KEY} whose value isn't in the vault (e.g. at uninstall
// time after a secret rotation), expectedHubURL returns "" so
// uninstall callers treat ownership as unknown and preserve the
// entry. This is the documented acceptable trade-off vs adding
// state-file machinery.
func TestExpectedHubURL_RemoteHTTPHandlesMissingSecretsGracefully(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	m := &config.ServerManifest{
		Name:      "ctx7",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://api.example.com/${secret:NEVER_SET}/mcp",
	}
	got := expectedHubURL(m, config.ClientBinding{Client: "claude-code"})
	if got != "" {
		t.Errorf("expectedHubURL on missing-secret remote-http = %q; want empty (uninstall treats as no-match)", got)
	}
}

// TestBuildPlanWithOpts_RemoteHTTPHeadersPopulatedOnClientUpdate
// pins bot r2 P1 closure on PR #170: the plan-builder MUST
// populate Headers on every remote-http ClientUpdatePlan so the
// applier (executeInstallTo) propagates them to MCPEntry.Headers
// when writing client configs. Pre-fix, Headers were only set in
// the plan but the applier ignored them, silently dropping
// Authorization etc. and producing client configs that would 401
// at runtime.
func TestBuildPlanWithOpts_RemoteHTTPHeadersPopulatedOnClientUpdate(t *testing.T) {
	m := &config.ServerManifest{
		Name:      "ctx7",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://mcp.context7.com/mcp",
		Headers: map[string]string{
			"Authorization": "Bearer literal-token", // no ${secret:} → no vault hit
			"X-Tenant":      "acme",
		},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code"},
		},
	}
	plan, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts: %v", err)
	}
	if len(plan.ClientUpdates) != 1 {
		t.Fatalf("expected 1 client update; got %d", len(plan.ClientUpdates))
	}
	u := plan.ClientUpdates[0]
	if got := u.Headers["Authorization"]; got != "Bearer literal-token" {
		t.Errorf("Authorization header dropped from plan: got %q", got)
	}
	if got := u.Headers["X-Tenant"]; got != "acme" {
		t.Errorf("X-Tenant header dropped from plan: got %q", got)
	}
	if u.URL != "https://mcp.context7.com/mcp" {
		t.Errorf("URL = %q; want manifest URL verbatim", u.URL)
	}
}

// TestBuildPlanWithOpts_RemoteHTTPRejectsAntigravityBinding pins
// the defense-in-depth check at the plan-build layer (in case
// callers bypass Preflight).
func TestBuildPlanWithOpts_RemoteHTTPRejectsAntigravityBinding(t *testing.T) {
	m := &config.ServerManifest{
		Name:      "ctx7",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://mcp.context7.com/mcp",
		ClientBindings: []config.ClientBinding{
			{Client: "antigravity"},
		},
	}
	_, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true})
	if err == nil {
		t.Fatal("expected antigravity rejection; got nil")
	}
	if !strings.Contains(err.Error(), "antigravity") {
		t.Errorf("error must name antigravity; got %v", err)
	}
}

// TestBuildPlanWithOpts_RemoteHTTPRejectsOffMatrixClient pins codex
// cumulative G6 review P1 closure: the rejection now triggers for
// ANY adapter not on remoteHTTPCapableClients, not just antigravity.
// This prevents a future binding to an unsupported client (or a typo
// like "claude_code") from slipping through to install.
func TestBuildPlanWithOpts_RemoteHTTPRejectsOffMatrixClient(t *testing.T) {
	m := &config.ServerManifest{
		Name:      "ctx7",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://mcp.context7.com/mcp",
		ClientBindings: []config.ClientBinding{
			{Client: "claude_code"}, // underscore typo — off-matrix
		},
	}
	_, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true})
	if err == nil {
		t.Fatal("expected off-matrix client rejection; got nil")
	}
	if !strings.Contains(err.Error(), "capability matrix") {
		t.Errorf("error must reference capability matrix; got %v", err)
	}
}

// TestBuildPlanWithOpts_RemoteHTTPDisplayURLPreservesPlaceholder
// pins codex cumulative G6 review P2 closure: when a manifest URL
// embeds a `${secret:KEY}` placeholder, ClientUpdatePlan.DisplayURL
// must carry the literal pre-expansion text so plan + install
// stdout never echoes the expanded path/query.
func TestBuildPlanWithOpts_RemoteHTTPDisplayURLPreservesPlaceholder(t *testing.T) {
	t.Setenv("MCPHUB_TEST_TOKEN_FOR_G6_DISPLAY", "")
	m := &config.ServerManifest{
		Name:      "ctx7",
		Kind:      config.KindGlobal,
		Transport: config.TransportRemoteHTTP,
		URL:       "https://api.example.com/v1?token=literal-not-a-secret",
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code"},
		},
	}
	p, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true})
	if err != nil {
		t.Fatalf("BuildPlanWithOpts: %v", err)
	}
	if len(p.ClientUpdates) != 1 {
		t.Fatalf("expected 1 client update, got %d", len(p.ClientUpdates))
	}
	u := p.ClientUpdates[0]
	if u.DisplayURL != m.URL {
		t.Errorf("DisplayURL = %q, want manifest literal %q (no expansion in display)", u.DisplayURL, m.URL)
	}
	// URL field (wire form) is the expanded version — for a
	// no-placeholder manifest, expanded == literal, so URL matches.
	if u.URL != m.URL {
		t.Errorf("URL = %q, want expanded %q", u.URL, m.URL)
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/api/serena_routing"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/tray"
)

type guiQuitAdmissionContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (ctx *guiQuitAdmissionContext) Err() error {
	err := ctx.Context.Err()
	ctx.once.Do(func() { close(ctx.checked) })
	return err
}

func TestGUIQuitCanceledWhileLifecycleBlockedHasNoIntentOrStop(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	t.Setenv("LOCALAPPDATA", dir)
	lease, err := gui.AcquireSingleInstanceAt(filepath.Join(dir, "gui.pidport"), 9125)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	defer func() {
		if err := lease.OwnerLifecycle().TerminalSettle(); err != nil {
			t.Error(err)
		}
	}()
	generation := lease.OwnerLifecycle().Generation()
	held, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		holderDone <- lease.OwnerLifecycle().WithActiveGeneration(generation, func(gui.GUIOwnerRecord) error { close(held); <-release; return nil })
	}()
	<-held
	caller, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &guiQuitAdmissionContext{Context: caller, checked: make(chan struct{})}
	stops, events, daemons := 0, 0, 0
	operation := &guiQuitOperation{
		lifecycle: lease.OwnerLifecycle(), persist: api.NewAPI().RecordGUIStop,
		stop:        func() { stops++ },
		emit:        func(gui.GUIExitReason, map[string]any) error { events++; return nil },
		stopDaemons: func() error { daemons++; return nil },
	}
	type received struct {
		result gui.GUIQuitResult
		err    error
	}
	done := make(chan received, 1)
	go func() {
		result, err := operation.request(ctx, gui.GUIQuitRequest{ExpectedGeneration: generation, StopDaemons: true})
		done <- received{result, err}
	}()
	<-ctx.checked
	cancel()
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.result.Accepted || !errors.Is(got.err, context.Canceled) || stops != 0 || events != 0 || daemons != 0 {
		t.Fatalf("late accepted effects=%#v %v counts=%d/%d/%d", got.result, got.err, stops, events, daemons)
	}
	if err := api.NewAPI().WithGUIOwnerControl(func(control api.GUIOwnerControl) error {
		if control.RunDesired != api.GUIRunRunning {
			t.Fatalf("late stopped intent=%#v", control)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGUIQuitSharedOperationRetainsAcceptedErrorAndDoesNotEscalate(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	t.Setenv("LOCALAPPDATA", dir)
	path := filepath.Join(dir, "gui.pidport")
	lease, err := gui.AcquireSingleInstanceAt(path, 9125)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	defer func() {
		if err := lease.OwnerLifecycle().TerminalSettle(); err != nil {
			t.Error(err)
		}
	}()
	stopCount, daemonCount, eventCount := 0, 0, 0
	eventFailure := errors.New("controlled event failure")
	operation := &guiQuitOperation{
		lifecycle: lease.OwnerLifecycle(), persist: api.NewAPI().RecordGUIStop,
		stop:        func() { stopCount++ },
		stopDaemons: func() error { daemonCount++; return nil },
		emit:        func(gui.GUIExitReason, map[string]any) error { eventCount++; return eventFailure },
	}
	request := gui.GUIQuitRequest{ExpectedGeneration: lease.OwnerLifecycle().Generation()}
	result, err := operation.requestGUIQuit(context.Background(), request, gui.GUIExitReasonTrayQuit)
	if !result.Accepted || result.StopDaemons || !errors.Is(err, eventFailure) || stopCount != 1 || daemonCount != 0 || eventCount != 1 {
		t.Fatalf("first=%#v err=%v counts=%d/%d/%d", result, err, stopCount, daemonCount, eventCount)
	}
	request.StopDaemons = true
	repeated, err := operation.request(context.Background(), request)
	if repeated != result || !errors.Is(err, eventFailure) || stopCount != 1 || daemonCount != 0 || eventCount != 1 {
		t.Fatalf("repeat escalated: %#v %v", repeated, err)
	}
	request.ExpectedGeneration = strings.Repeat("f", 64)
	if _, err := operation.request(context.Background(), request); !errors.Is(err, gui.ErrGUIQuitGeneration) || stopCount != 1 {
		t.Fatalf("stale request=%v", err)
	}
	if !errors.Is(operation.outcomeError(), eventFailure) {
		t.Fatal("terminal caller lost accepted operation error")
	}
}

func TestGUIQuitPolicyFailureRefusesButPositiveDaemonFailureStillSettles(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	t.Setenv("LOCALAPPDATA", dir)
	path := filepath.Join(dir, "gui.pidport")
	lease, err := gui.AcquireSingleInstanceAt(path, 9125)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	defer func() {
		if err := lease.OwnerLifecycle().TerminalSettle(); err != nil {
			t.Error(err)
		}
	}()
	policyFailure, daemonFailure := errors.New("controlled policy failure"), errors.New("controlled partial daemon stop")
	stopCount, daemonCount := 0, 0
	operation := &guiQuitOperation{
		lifecycle: lease.OwnerLifecycle(), stop: func() { stopCount++ },
		persist: func(context.Context, string, string) (api.GUIOwnerControlResult, error) {
			return api.GUIOwnerControlResult{}, policyFailure
		},
		emit:        func(gui.GUIExitReason, map[string]any) error { return nil },
		stopDaemons: func() error { daemonCount++; return daemonFailure },
	}
	request := gui.GUIQuitRequest{ExpectedGeneration: lease.OwnerLifecycle().Generation(), StopDaemons: true}
	result, err := operation.request(context.Background(), request)
	if result.Accepted || !errors.Is(err, policyFailure) || stopCount != 0 || daemonCount != 0 {
		t.Fatalf("policy refusal=%#v %v", result, err)
	}
	operation.persist = api.NewAPI().RecordGUIStop
	result, err = operation.request(context.Background(), request)
	if !result.Accepted || !result.StopDaemons || !errors.Is(err, daemonFailure) || stopCount != 1 || daemonCount != 1 {
		t.Fatalf("partial daemon stop=%#v %v counts=%d/%d", result, err, stopCount, daemonCount)
	}
}

func TestGUIQuitCancellationBeforeAndAfterDurableAcceptance(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	t.Setenv("LOCALAPPDATA", dir)
	lease, err := gui.AcquireSingleInstanceAt(filepath.Join(dir, "gui.pidport"), 9125)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	defer func() {
		if err := lease.OwnerLifecycle().TerminalSettle(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	stops, persists := 0, 0
	operation := &guiQuitOperation{
		lifecycle: lease.OwnerLifecycle(), stop: func() { stops++ },
		persist: func(ctx context.Context, generation, cause string) (api.GUIOwnerControlResult, error) {
			persists++
			return api.NewAPI().RecordGUIStop(ctx, generation, cause)
		},
		emit:        func(gui.GUIExitReason, map[string]any) error { cancel(); return nil },
		stopDaemons: func() error { t.Fatal("default Quit invoked stop-all"); return nil },
	}
	request := gui.GUIQuitRequest{ExpectedGeneration: lease.OwnerLifecycle().Generation()}
	before, cancelBefore := context.WithCancel(context.Background())
	cancelBefore()
	if result, err := operation.request(before, request); result.Accepted || !errors.Is(err, context.Canceled) || persists != 0 || stops != 0 {
		t.Fatalf("preacceptance=%#v %v", result, err)
	}
	result, err := operation.request(ctx, request)
	if err != nil || !result.Accepted || persists != 1 || stops != 1 || ctx.Err() != context.Canceled {
		t.Fatalf("postacceptance=%#v %v persists=%d stops=%d", result, err, persists, stops)
	}
}

func TestQuitCommandAbsentOwnerAcceptsActualNoDeadlineContext(t *testing.T) {
	dir := apitest.HardenedTempDir(t)
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	var output bytes.Buffer
	command := newQuitCmd()
	command.SetContext(context.Background())
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "GUI already stopped") {
		t.Fatalf("missing applied result: %q", output.String())
	}
	if err := api.NewAPI().WithGUIOwnerControl(func(control api.GUIOwnerControl) error {
		if control.RunDesired != api.GUIRunStopped || control.StopCause != "cli-quit" || control.StopGeneration != "" {
			t.Fatalf("absent result=%#v", control)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestQuitCommandAbsentOwnerPositiveReplayDoesNotStopAgain(t *testing.T) {
	stopFailure := errors.New("controlled StopAll admission failure")
	for _, tc := range []struct {
		name          string
		firstPositive bool
		stopErr       error
		wantReplayErr error
	}{
		{name: "default_then_positive", wantReplayErr: gui.ErrGUIQuitBreadthConflict},
		{name: "positive_then_positive", firstPositive: true, wantReplayErr: gui.ErrGUIQuitDaemonOutcomeUnconfirmed},
		{name: "failed_positive_then_positive", firstPositive: true, stopErr: stopFailure, wantReplayErr: gui.ErrGUIQuitDaemonOutcomeUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := apitest.HardenedTempDir(t)
			t.Setenv("LOCALAPPDATA", dir)
			t.Setenv("USERPROFILE", dir)
			t.Setenv("XDG_DATA_HOME", dir)
			t.Setenv("XDG_STATE_HOME", dir)
			t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", dir)
			t.Setenv("MCPHUB_E2E_SCHEDULER", "none")
			t.Cleanup(api.SetDaemonStateRootForTest(dir))
			stopCalls := 0
			t.Cleanup(api.RegisterSupervisorControlAdmission(func(context.Context) error {
				stopCalls++
				return tc.stopErr
			}))
			run := func(positive bool) error {
				var output bytes.Buffer
				command := newQuitCmd()
				command.SetContext(context.Background())
				command.SetOut(&output)
				command.SetArgs([]string{})
				if positive {
					command.SetArgs([]string{"--stop-daemons"})
				}
				return command.Execute()
			}
			firstErr := run(tc.firstPositive)
			if (tc.stopErr == nil && firstErr != nil) || (tc.stopErr != nil && !errors.Is(firstErr, tc.stopErr)) {
				t.Fatalf("first Quit error=%v, want retained cause %v", firstErr, tc.stopErr)
			}
			wantCalls := 0
			wantCause := "cli-quit"
			if tc.firstPositive {
				wantCalls = 1
				wantCause = "cli-quit-and-stop-all"
			}
			if stopCalls != wantCalls {
				t.Fatalf("first Quit StopAll entries=%d, want %d", stopCalls, wantCalls)
			}
			replayErr := run(true)
			if err := api.NewAPI().WithGUIOwnerControl(func(control api.GUIOwnerControl) error {
				if control.RunDesired != api.GUIRunStopped || control.StopCause != wantCause || control.StopGeneration != "" {
					t.Errorf("replay changed first policy: %#v", control)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if stopCalls != wantCalls {
				t.Errorf("positive replay entered StopAll: total=%d, want %d; replay error=%v", stopCalls, wantCalls, replayErr)
			}
			if !errors.Is(replayErr, tc.wantReplayErr) {
				t.Errorf("positive replay error=%v, want %v", replayErr, tc.wantReplayErr)
			}
		})
	}
}

func TestActivateDashboardFromTrayDecisionMatrix(t *testing.T) {
	const (
		pidportPath = "test-gui.pidport"
		port        = 19123
	)
	unexpected := errors.New("tray transport failed")
	tests := []struct {
		name          string
		requestedPort int // what startGuiServer passes in (the --port FLAG value)
		activationErr error
		wantLaunches  int
		wantURL       string
		wantLog       string
	}{
		{
			name:          "no target",
			requestedPort: port,
			activationErr: &gui.IncumbentNoActivationTargetError{
				Port: port, Reason: gui.ReasonNoBrowserWindow,
			},
			wantLaunches: 1,
			wantURL:      "http://127.0.0.1:19123/",
			wantLog:      "no dashboard window to focus",
		},
		{
			// The default `--port 0` (an ephemeral bind) is the production
			// posture: the REQUESTED port stays 0 while the server binds a real
			// one. The URL must come from the handshake-verified port on the
			// typed error, or the tray opens http://127.0.0.1:0/.
			name:          "no target under default --port 0 uses the bound port",
			requestedPort: 0,
			activationErr: &gui.IncumbentNoActivationTargetError{
				Port: port, Reason: gui.ReasonNoBrowserWindow,
			},
			wantLaunches: 1,
			wantURL:      "http://127.0.0.1:19123/",
			wantLog:      "no dashboard window to focus",
		},
		{name: "nil", requestedPort: port, wantLaunches: 0},
		{name: "unexpected error", requestedPort: port, activationErr: unexpected, wantLaunches: 0, wantLog: "tray: activate-window failed: tray transport failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log bytes.Buffer
			activateCalls := 0
			launchCalls := 0
			launchedURL := ""
			activateDashboardFromTray(
				pidportPath,
				tt.requestedPort,
				&log,
				func(gotPath string, gotTimeout time.Duration) error {
					activateCalls++
					if gotPath != pidportPath {
						t.Errorf("pidport path = %q, want %q", gotPath, pidportPath)
					}
					if gotTimeout != 500*time.Millisecond {
						t.Errorf("activation timeout = %v, want 500ms", gotTimeout)
					}
					return tt.activationErr
				},
				func(url string) error {
					launchCalls++
					launchedURL = url
					return nil
				},
			)

			if activateCalls != 1 {
				t.Fatalf("activation calls = %d, want 1", activateCalls)
			}
			if launchCalls != tt.wantLaunches {
				t.Fatalf("browser launch calls = %d, want %d", launchCalls, tt.wantLaunches)
			}
			if tt.wantLaunches == 1 && launchedURL != tt.wantURL {
				t.Errorf("launched URL = %q, want %q", launchedURL, tt.wantURL)
			}
			if tt.wantLog != "" && !strings.Contains(log.String(), tt.wantLog) {
				t.Errorf("log = %q, want substring %q", log.String(), tt.wantLog)
			}
		})
	}
}

func TestActivateDashboardWindowDecisionMatrix(t *testing.T) {
	unexpected := errors.New("focus failed")
	tests := []struct {
		name         string
		focusErr     error
		headless     bool
		noBrowser    bool
		wantReason   gui.ActivationNoTargetReason
		wantLaunches int
	}{
		{name: "focus success", headless: true, noBrowser: true},
		{name: "non-no-window focus error", focusErr: unexpected, headless: true, noBrowser: true},
		{name: "headless", focusErr: gui.ErrFocusNoWindow, headless: true, wantReason: gui.ReasonHeadless},
		{name: "no browser and no window", focusErr: gui.ErrFocusNoWindow, noBrowser: true, wantReason: gui.ReasonNoBrowserWindow},
		{name: "browser fallback", focusErr: gui.ErrFocusNoWindow, wantLaunches: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log bytes.Buffer
			focusCalls := 0
			launchCalls := 0
			launchedURL := ""
			err := activateDashboardWindow(
				tt.noBrowser,
				19123,
				&log,
				func(title string) error {
					focusCalls++
					if title != "Local Dashboard" {
						t.Errorf("focus title = %q, want Local Dashboard", title)
					}
					return tt.focusErr
				},
				func(url string) error {
					launchCalls++
					launchedURL = url
					return nil
				},
				func() bool { return tt.headless },
			)

			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
			} else {
				if !errors.Is(err, gui.ErrActivationNoTarget) {
					t.Fatalf("error = %v, want ErrActivationNoTarget", err)
				}
				var noTarget *gui.ActivationNoTargetError
				if !errors.As(err, &noTarget) {
					t.Fatalf("error type = %T, want *gui.ActivationNoTargetError", err)
				}
				if noTarget.Reason != tt.wantReason {
					t.Errorf("reason = %q, want %q", noTarget.Reason, tt.wantReason)
				}
			}
			if focusCalls != 1 {
				t.Errorf("focus calls = %d, want 1", focusCalls)
			}
			if launchCalls != tt.wantLaunches {
				t.Errorf("browser launch calls = %d, want %d", launchCalls, tt.wantLaunches)
			}
			if tt.wantLaunches == 1 && launchedURL != "http://127.0.0.1:19123/" {
				t.Errorf("launched URL = %q, want http://127.0.0.1:19123/", launchedURL)
			}
		})
	}
}

func TestHandleIncumbentActivationResultGuidance(t *testing.T) {
	unexpected := errors.New("activate failed")
	tests := []struct {
		name        string
		err         error
		wantHandled bool
		wantErr     error
		wantOutput  string
	}{
		{name: "nil"},
		{
			name: "headless",
			err: &gui.IncumbentNoActivationTargetError{
				Port: 19123, Reason: gui.ReasonHeadless,
			},
			wantHandled: true,
			wantOutput:  "SSH-tunnel and visit http://127.0.0.1:19123/",
		},
		{
			name: "no browser window",
			err: &gui.IncumbentNoActivationTargetError{
				Port: 19123, Reason: gui.ReasonNoBrowserWindow,
			},
			wantHandled: true,
			wantOutput:  "no dashboard window to focus. Open http://127.0.0.1:19123/",
		},
		{name: "unexpected", err: unexpected, wantErr: unexpected},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			handled, err := handleIncumbentActivationResult(&out, tt.err)
			if handled != tt.wantHandled {
				t.Errorf("handled = %v, want %v", handled, tt.wantHandled)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantOutput != "" && !strings.Contains(out.String(), tt.wantOutput) {
				t.Errorf("output = %q, want substring %q", out.String(), tt.wantOutput)
			}
		})
	}
}

func TestGuiCmd_HelpIncludesFlags(t *testing.T) {
	cmd := newGuiCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// PR #214 closes QA-r6-Gap-4 by asserting --strict-mode appears in
	// the user-visible help. The flag was added by PR #212 r1
	// (gui_supervisor_owner.go) but the help-text test never updated;
	// a future regression that drops the cobra registration would
	// have gone unnoticed without this assertion.
	for _, want := range []string{"--port", "--no-browser", "--no-tray", "--strict-mode"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("--help missing %q; got %q", want, buf.String())
		}
	}
	// --force is intentionally hidden (Phase 3B-II placeholder); --help must NOT advertise it.
	if strings.Contains(buf.String(), "--force") {
		t.Errorf("--help unexpectedly advertises --force; should be hidden until take-over is implemented")
	}
}

func TestGuiCmd_TrayRuntimePolicyFromRoutedInvocation(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want bool
	}{
		{name: "bare invocation routes to tray-enabled GUI", argv: []string{"mcphub"}, want: true},
		{name: "explicit GUI opt-out suppresses tray", argv: []string{"mcphub", "gui", "--no-tray"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routed := RouteInvocationArgs(tt.argv)
			root := NewRootCmd()
			guiCmd, remaining, err := root.Find(routed[1:])
			if err != nil {
				t.Fatalf("find routed command for %v: %v", routed, err)
			}
			if guiCmd.Name() != "gui" {
				t.Fatalf("routed command for %v = %q, want gui", routed, guiCmd.Name())
			}
			if err := guiCmd.Flags().Parse(remaining); err != nil {
				t.Fatalf("parse routed GUI args %v: %v", remaining, err)
			}
			noTray, err := guiCmd.Flags().GetBool("no-tray")
			if err != nil {
				t.Fatalf("GetBool(no-tray): %v", err)
			}
			if got := shouldRunTray(noTray); got != tt.want {
				t.Fatalf("shouldRunTray(noTray=%t) = %t, want %t", noTray, got, tt.want)
			}
		})
	}
}

func TestStartTrayIfEnabled_NoTrayMakesZeroCalls(t *testing.T) {
	calls := 0
	done := startTrayIfEnabled(true, func() error { calls++; return nil })
	if done != nil {
		t.Fatal("--no-tray created completion custody")
	}
	if calls != 0 {
		t.Fatalf("tray launch calls = %d, want 0 for --no-tray", calls)
	}
}

func TestStartTrayRetainsOriginalCompletionAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, unsettled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	failed := errors.New("controlled original tray settlement failure")
	calls := 0
	done := startTrayIfEnabled(false, func() error {
		calls++
		close(entered)
		<-ctx.Done()
		close(unsettled)
		<-release
		return failed
	})
	<-entered
	cancel()
	<-unsettled
	select {
	case err := <-done:
		close(release)
		t.Fatalf("original Run custody escaped before settlement: %v", err)
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, failed) {
		t.Fatalf("lost original Run error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("Run calls=%d, want exactly one", calls)
	}
	if _, open := <-done; open {
		t.Fatal("completion channel remains open after original Run returned")
	}
}

func TestTrayLifecycleGUIEvent_BoundsAndRedactsDetail(t *testing.T) {
	privatePath := filepath.Join(t.TempDir(), "private", "mcphub.exe")
	if !filepath.IsAbs(privatePath) {
		t.Fatalf("diagnostic fixture path is not absolute: %q", privatePath)
	}
	ev := trayLifecycleGUIEvent(tray.LifecycleReport{
		Type:        "tray-child-unavailable",
		Phase:       "spawn",
		Attempt:     3,
		NextRetryMS: 500,
		ErrorDetail: fmt.Sprintf("open %s\nfailed", privatePath),
	})
	if ev.Type != "tray-child-unavailable" {
		t.Fatalf("event type = %q", ev.Type)
	}
	if got := ev.Body["error_detail"]; got != "operation failed" {
		t.Fatalf("error_detail = %q, want path-free fallback", got)
	}
	if got := ev.Body["error_detail"].(string); strings.Contains(got, privatePath) || strings.Contains(got, "private") {
		t.Fatalf("error_detail disclosed fixture path: %q", got)
	}
	if ev.Body["phase"] != "spawn" || ev.Body["attempt"] != 3 || ev.Body["next_retry_ms"] != int64(500) {
		t.Fatalf("event body = %#v", ev.Body)
	}

	protocol := trayLifecycleGUIEvent(tray.LifecycleReport{
		Type:    "tray-child-protocol-warning",
		Attempt: 4,
		Phase:   "event-json-invalid",
	})
	if protocol.Body["attempt"] != 4 || protocol.Body["phase"] != "event-json-invalid" {
		t.Fatalf("protocol warning body = %#v", protocol.Body)
	}
}

// Existing original completion receiving with a real required API marshal
// failure and actual closed pipe fallback. This is injected-model tray/GUI
// receiving, not native tray.Run or filesystem Sync-failure proof.
func TestQuit041RequiredDeliveryCauseSurvivesOriginalTrayCompletion(t *testing.T) {
	original := logGUIDiagnosticEventRequired
	t.Cleanup(func() { logGUIDiagnosticEventRequired = original })
	var producerErr error
	logGUIDiagnosticEventRequired = func(level, event string, fields map[string]any) error {
		producerErr = api.LogHubMcpEventRequired(level, event, map[string]any{"invalid": make(chan struct{})})
		return producerErr
	}
	reader, writer := io.Pipe()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	sink := newConsoleReleaseSink(writer)
	sink.engageDurableSink()
	done := startTrayIfEnabled(false, func() error {
		return writeTraySettlementDiagnostic(sink, "private path and arguments must not become public detail")
	})
	var result error
	select {
	case result = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("original tray completion was not received")
	}
	var unsupported *json.UnsupportedTypeError
	if producerErr == nil || !errors.Is(result, producerErr) || !errors.As(result, &unsupported) || !errors.Is(result, io.ErrClosedPipe) {
		t.Fatalf("required producer cause lost through original completion: %v", result)
	}
	var redirected bytes.Buffer
	logGUIDiagnosticEventRequired = func(string, string, map[string]any) error { return nil }
	sink = newConsoleReleaseSink(&redirected)
	sink.engageDurableSink()
	if err := writeTraySettlementDiagnostic(sink, "private path and arguments"); err != nil {
		t.Fatal(err)
	}
	if got := redirected.String(); got != "tray: settlement unconfirmed; original child remains owned: operation failed\n" {
		t.Fatalf("required public detail was not allowlisted: %q", got)
	}
}

func TestSerenaBackendLossReconcileTicker_NilTriggerWaitsForInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSerenaBackendLossReconcileTicker(ctx, &gui.Server{}, time.Hour)
	}()

	select {
	case <-done:
		t.Fatal("reconcile ticker exited before its interval fired or ctx was canceled")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile ticker did not exit after ctx cancel")
	}
}

// TestGuiCmd_ForceFlagStillParseable confirms `--force` is hidden but
// remains a valid flag (parseable, not removed). Hiding via MarkHidden
// keeps the wiring in place for Phase 3B-II without breaking any
// scripted callers that already pass --force.
func TestGuiCmd_ForceFlagStillParseable(t *testing.T) {
	cmd := newGuiCmdReal()
	if cmd.Flags().Lookup("force") == nil {
		t.Fatal("--force flag should still be defined (just hidden)")
	}
	if !cmd.Flags().Lookup("force").Hidden {
		t.Error("--force should be marked hidden")
	}
}

// TestResolveGuiPort pins bug-bash A5 (#18/#19/#20) closure: effective
// port follows --port flag when explicitly passed, otherwise reads
// `gui_server.port` settings, otherwise 0 (auto-pick). Pre-fix, the
// persisted setting was cosmetic — startup ignored it.
func TestResolveGuiPort(t *testing.T) {
	cases := []struct {
		name         string
		flagChanged  bool
		flagValue    int
		settingValue string
		want         int
	}{
		// 1. Flag explicitly set wins.
		{"explicit --port 9200 wins over setting 9125", true, 9200, "9125", 9200},
		{"explicit --port 0 wins (operator wants ephemeral)", true, 0, "9125", 0},
		// 2. Flag not set + valid setting → use setting.
		{"no flag, valid setting 9125", false, 0, "9125", 9125},
		{"no flag, setting 1024 (min)", false, 0, "1024", 1024},
		{"no flag, setting 65535 (max)", false, 0, "65535", 65535},
		{"no flag, setting with whitespace", false, 0, " 9125 ", 9125},
		// 3. Flag not set + invalid/empty setting → 0 (auto-pick).
		{"no flag, empty setting", false, 0, "", 0},
		{"no flag, non-numeric setting", false, 0, "abc", 0},
		{"no flag, below privileged-port boundary", false, 0, "80", 0},
		{"no flag, above 65535", false, 0, "70000", 0},
		{"no flag, negative setting", false, 0, "-1", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveGuiPort(tc.flagChanged, tc.flagValue, tc.settingValue)
			if got != tc.want {
				t.Errorf("resolveGuiPort(flagChanged=%v, flagValue=%d, setting=%q) = %d, want %d",
					tc.flagChanged, tc.flagValue, tc.settingValue, got, tc.want)
			}
		})
	}
}

func TestRunSessionCleanupTicker_ExpiresOldSessions(t *testing.T) {
	oldNow := time.Now().Add(-2 * time.Hour)
	sessions := serena_routing.NewSessionRouterWithClock(func() time.Time { return oldNow })
	sessions.BindSession("s1", &api.WorkspaceEntry{WorkspacePath: "alpha"})
	sessions.BindSession("s2", &api.WorkspaceEntry{WorkspacePath: "beta"})
	if got := sessions.Len(); got != 2 {
		t.Fatalf("Len before cleanup = %d, want 2", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// nil Server: this test exercises only the sticky-router sweep;
		// SweepSerenaSessions is covered by the gui-package unit test.
		runSessionCleanupTicker(ctx, nil, sessions, 10*time.Millisecond, time.Hour)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("runSessionCleanupTicker did not exit after ctx cancel")
		}
	})

	deadline := time.After(time.Second)
	for sessions.Len() != 0 {
		select {
		case <-deadline:
			t.Fatalf("Len after cleanup ticker = %d, want 0", sessions.Len())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// runSerenaIdleShutdownTicker (v0.6 #6) exits promptly on ctx cancel. A nil
// Server is the safe no-op path (the tick is skipped) — this asserts the
// goroutine lifecycle, not the sweep itself (covered by the gui-package tests).
func TestRunSerenaIdleShutdownTicker_ExitsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSerenaIdleShutdownTicker(ctx, nil, 10*time.Millisecond)
		close(done)
	}()
	// Let a couple of ticks fire against the nil server (no-op), then cancel.
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runSerenaIdleShutdownTicker did not exit after ctx cancel")
	}
}

package api

import (
	"bytes"
	"encoding/binary"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"mcp-local-hub/internal/scheduler"
)

const (
	livenessFixtureExe        = `%USERPROFILE%\.local\bin\mcphub.exe`
	livenessFixtureWorkingDir = `%USERPROFILE%\.local\bin`
	livenessPriorFixtureExe   = `%PREVIOUS_USERPROFILE%\.local\bin\mcphub.exe`
	livenessPriorFixtureDir   = `%PREVIOUS_USERPROFILE%\.local\bin`
)

// installTestCanonicalMcphubPath overrides the canonical-mcphub-path resolver
// (canonicalMcphubPathFn, liveness_task.go) for the duration of the test. These
// two helpers were migrated here from the deleted watchdog_xml_validator_test.go
// when the v0.6 redesign removed the watchdog engine — the liveness-task tests
// are now their sole consumer.
func installTestCanonicalMcphubPath(t *testing.T, path string) {
	t.Helper()
	orig := canonicalMcphubPathFn
	canonicalMcphubPathFn = func() (string, error) { return path, nil }
	t.Cleanup(func() { canonicalMcphubPathFn = orig })
}

type livenessTaskScheduler struct {
	tasks              map[string][]byte
	creates            []importXMLCall
	imports            []importXMLCall
	deletes            []string
	createErrForCall   func(int) error
	exportErrForCall   func(int) error
	exportBytesOnError []byte
	exportCalls        int
	normalizeCreate    func([]byte) []byte
}

func newLivenessTaskScheduler() *livenessTaskScheduler {
	return &livenessTaskScheduler{tasks: map[string][]byte{}}
}

func (f *livenessTaskScheduler) Create(scheduler.TaskSpec) error { return errNotImplementedForTest }
func (f *livenessTaskScheduler) Run(string) error                { return errNotImplementedForTest }
func (f *livenessTaskScheduler) Stop(string) error               { return errNotImplementedForTest }
func (f *livenessTaskScheduler) Status(string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{}, errNotImplementedForTest
}
func (f *livenessTaskScheduler) List(string) ([]scheduler.TaskStatus, error) {
	return nil, errNotImplementedForTest
}
func (f *livenessTaskScheduler) ExportXML(name string) ([]byte, error) {
	f.exportCalls++
	if f.exportErrForCall != nil {
		if err := f.exportErrForCall(f.exportCalls); err != nil {
			return bytes.Clone(f.exportBytesOnError), err
		}
	}
	xml, ok := f.tasks[name]
	if !ok {
		return nil, scheduler.ErrTaskNotFound
	}
	return append([]byte(nil), xml...), nil
}
func (f *livenessTaskScheduler) CreateXMLIfAbsent(name string, xml []byte) error {
	f.creates = append(f.creates, importXMLCall{name: name, xml: append([]byte(nil), xml...)})
	if f.createErrForCall != nil {
		if err := f.createErrForCall(len(f.creates)); err != nil {
			return err
		}
	}
	if _, exists := f.tasks[name]; exists {
		return scheduler.ErrTaskAlreadyExists
	}
	settled := append([]byte(nil), xml...)
	if f.normalizeCreate != nil {
		settled = f.normalizeCreate(settled)
	}
	f.tasks[name] = settled
	return nil
}
func (f *livenessTaskScheduler) ImportXML(name string, xml []byte) error {
	f.imports = append(f.imports, importXMLCall{name: name, xml: bytes.Clone(xml)})
	return errNotImplementedForTest
}
func (f *livenessTaskScheduler) Delete(name string) error {
	f.deletes = append(f.deletes, name)
	delete(f.tasks, name)
	return nil
}
func (f *livenessTaskScheduler) createCalls() []importXMLCall {
	return append([]importXMLCall(nil), f.creates...)
}
func (f *livenessTaskScheduler) calls() []string { return append([]string(nil), f.deletes...) }

// installTestCurrentWindowsUser overrides the current-user resolver
// (currentWindowsUserFn, liveness_task.go) for the duration of the test.
func installTestCurrentWindowsUser(t *testing.T, name string) {
	t.Helper()
	orig := currentWindowsUserFn
	currentWindowsUserFn = func() (string, error) { return name, nil }
	t.Cleanup(func() { currentWindowsUserFn = orig })
}

// ensureLivenessTaskFixtureResolved exercises the scheduler transaction with
// explicit fixture identities. Public composition resolves the current Windows
// principal before reaching this transaction, so it is intentionally covered by
// the platform-specific SID tests instead.
func ensureLivenessTaskFixtureResolved(f scheduler.Scheduler) (LivenessTaskReceipt, error) {
	return ensureLivenessTaskResolved(livenessFixtureExe, livenessFixtureWorkingDir, "S-1-5-21-test", "test", f)
}

func TestLivenessTaskDifferenceNativeDefaultsAndIdentityMatrix(t *testing.T) {
	const sid, account = "S-1-5-21-101-202-303-404", "account-canary"
	canonical := scheduler.BuildLivenessXML(livenessFixtureExe, livenessFixtureWorkingDir, sid, account)
	check := func(name, xml string, want LivenessTaskDifferenceField) {
		t.Helper()
		got := livenessTaskDifference([]byte(xml), livenessFixtureExe, livenessFixtureWorkingDir, sid, account)
		if want == "" && got != nil {
			t.Fatalf("%s difference=%v", name, got)
		}
		if want != "" && (got == nil || got.Field != want) {
			t.Fatalf("%s difference=%v want=%s", name, got, want)
		}
	}
	for _, tc := range []struct{ name, old string }{
		{"run-level", "<RunLevel>LeastPrivilege</RunLevel>"},
		{"stop", "<StopAtDurationEnd>false</StopAtDurationEnd>"},
		{"trigger-enabled", "<Enabled>true</Enabled>"},
		{"settings-enabled", "<Enabled>true</Enabled>"},
	} {
		check("omit-"+tc.name, strings.Replace(canonical, tc.old, "", 1), "")
	}
	check("calendar-enabled", strings.Replace(canonical, "<CalendarTrigger>", "<CalendarTrigger><Enabled>true</Enabled>", 1), "")
	check("calendar-enabled-one", strings.Replace(canonical, "<CalendarTrigger>", "<CalendarTrigger><Enabled>1</Enabled>", 1), "")
	check("calendar-disabled", strings.Replace(canonical, "<CalendarTrigger>", "<CalendarTrigger><Enabled>false</Enabled>", 1), livenessTaskFieldCalendarEnabled)
	check("calendar-future", strings.Replace(canonical, "2026-06-10T00:00:00", "2099-06-10T00:00:00", 1), livenessTaskFieldCalendarStartBoundary)
	check("wrong-sid", strings.Replace(canonical, sid, "S-1-5-21-wrong", 1), livenessTaskFieldPrincipalUser)
	check("wrong-account", strings.Replace(canonical, ">"+account+"<", ">wrong<", 1), livenessTaskFieldLogonTriggerUser)
	check("highest", strings.Replace(canonical, "LeastPrivilege", "HighestAvailable", 1), livenessTaskFieldPrincipalRunLevel)
	check("stop-true", strings.Replace(canonical, "<StopAtDurationEnd>false", "<StopAtDurationEnd>true", 1), livenessTaskFieldCalendarStopAtDurationEnd)
	check("trigger-disabled", strings.Replace(canonical, "<Enabled>true", "<Enabled>false", 1), livenessTaskFieldLogonTriggerEnabled)
	check("settings-disabled", strings.Replace(canonical, "<Settings>\n    <Hidden>false</Hidden>\n    <Priority>7</Priority>\n    <ExecutionTimeLimit>PT1M</ExecutionTimeLimit>\n    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\n    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n    <RunOnlyIfIdle>false</RunOnlyIfIdle>\n    <AllowStartOnDemand>true</AllowStartOnDemand>\n    <Enabled>true", "<Settings>\n    <Hidden>false</Hidden>\n    <Priority>7</Priority>\n    <ExecutionTimeLimit>PT1M</ExecutionTimeLimit>\n    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>\n    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>\n    <RunOnlyIfIdle>false</RunOnlyIfIdle>\n    <AllowStartOnDemand>true</AllowStartOnDemand>\n    <Enabled>false", 1), livenessTaskFieldSettingsEnabled)
}

func TestEnsureLivenessTaskResolved_AcceptsNativeDefaultOmissionsAndIsIdempotent(t *testing.T) {
	const sid, account = "S-1-5-21-101-202-303-404", "account-canary"
	f := newLivenessTaskScheduler()
	f.normalizeCreate = func(raw []byte) []byte {
		xml := decodeUTF16LEBOMForTest(t, raw)
		principalStart := strings.Index(xml, "    <Principal id=\"Author\">\n")
		principalEnd := strings.Index(xml, "    </Principal>\n")
		if principalStart < 0 || principalEnd < principalStart {
			t.Fatal("imported XML has no principal section")
		}
		principal := xml[principalStart : principalEnd+len("    </Principal>\n")]
		if !strings.Contains(principal, "<UserId>"+sid+"</UserId>") || strings.Contains(principal, account) {
			t.Fatalf("imported principal = %q; want resolved SID only", principal)
		}
		xml = strings.Replace(xml, "<RunLevel>LeastPrivilege</RunLevel>", "", 1)
		xml = strings.Replace(xml, "<StopAtDurationEnd>false</StopAtDurationEnd>", "", 1)
		xml = strings.Replace(xml, "<Enabled>true</Enabled>", "", 1)
		if at := strings.LastIndex(xml, "<Enabled>true</Enabled>"); at >= 0 {
			xml = xml[:at] + xml[at+len("<Enabled>true</Enabled>"):]
		} else {
			t.Fatal("imported XML has no settings enabled default to omit")
		}
		return scheduler.EncodeXMLUTF16LEBOM(xml)
	}

	receipt, err := ensureLivenessTaskResolved(livenessFixtureExe, livenessFixtureWorkingDir, sid, account, f)
	if err != nil {
		t.Fatalf("first resolved ensure: %v", err)
	}
	if receipt.Result != LivenessTaskCreated {
		t.Fatalf("first result=%q, want %q", receipt.Result, LivenessTaskCreated)
	}
	if got := len(f.createCalls()); got != 1 {
		t.Fatalf("first ensure CreateXMLIfAbsent calls=%d, want 1", got)
	}

	receipt, err = ensureLivenessTaskResolved(livenessFixtureExe, livenessFixtureWorkingDir, sid, account, f)
	if err != nil {
		t.Fatalf("second resolved ensure: %v", err)
	}
	if receipt.Result != LivenessTaskUnchanged {
		t.Fatalf("second result=%q, want %q", receipt.Result, LivenessTaskUnchanged)
	}
	if got := len(f.createCalls()); got != 1 {
		t.Fatalf("second ensure recreated normalized task: calls=%d, want 1", got)
	}
}

func TestEnsureLivenessTaskResolved_RetainsStrictOppositeReadback(t *testing.T) {
	const sid, account = "S-1-5-21-101-202-303-404", "account-canary"
	root := hardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(root))
	f := newLivenessTaskScheduler()
	f.normalizeCreate = func(raw []byte) []byte {
		return scheduler.EncodeXMLUTF16LEBOM(strings.Replace(
			decodeUTF16LEBOMForTest(t, raw),
			"<RunLevel>LeastPrivilege</RunLevel>",
			"<RunLevel>HighestAvailable</RunLevel>", 1))
	}
	_, err := ensureLivenessTaskResolved(livenessFixtureExe, livenessFixtureWorkingDir, sid, account, f)
	var failure livenessTaskPostImportFailureReporter
	if !errors.As(err, &failure) || failure.LivenessTaskPostImportStage() != string(LivenessTaskPostImportSemanticDrift) {
		t.Fatalf("post-create error is not the typed semantic drift: %v", err)
	}
	var difference livenessTaskDifferenceReporter
	if !errors.As(err, &difference) || difference.LivenessTaskDifferenceField() != string(livenessTaskFieldPrincipalRunLevel) {
		t.Fatalf("strict opposite difference=%v, want %s", err, livenessTaskFieldPrincipalRunLevel)
	}
	if !errors.Is(failure.LivenessTaskRollbackError(), ErrLivenessTaskRollbackConflict) {
		t.Fatalf("rollback error=%v, want generation refusal", failure.LivenessTaskRollbackError())
	}
	if len(f.creates) != 1 || len(f.imports) != 0 || len(f.deletes) != 0 {
		t.Fatalf("post-create failure mutated again: creates=%d imports=%d deletes=%v", len(f.creates), len(f.imports), f.deletes)
	}
	readInstallRecoverySnapshot(t, root, LivenessTaskName, f.tasks[LivenessTaskName])
}

// TestInstallLivenessTask_HappyPath asserts the supervisor-liveness install
// (v0.6 spec §15 P1-b / §5.x Phase 3a) resolves the canonical mcphub path +
// current user via the seams, then CreateXMLIfAbsent under LivenessTaskName with the
// liveness XML body (PT1M cadence + `supervise --ensure-alive` action). Reuses
// the recording liveness scheduler and explicit fixture identities.
func TestInstallLivenessTask_HappyPath(t *testing.T) {
	f := newLivenessTaskScheduler()

	if _, err := ensureLivenessTaskFixtureResolved(f); err != nil {
		t.Fatalf("InstallLivenessTask: %v", err)
	}

	imports := f.createCalls()
	if len(imports) != 1 {
		t.Fatalf("expected 1 CreateXMLIfAbsent call, got %d", len(imports))
	}
	if imports[0].name != LivenessTaskName {
		t.Errorf("CreateXMLIfAbsent target name: got %q, want %q", imports[0].name, LivenessTaskName)
	}
	body := decodeUTF16LEBOMForTest(t, imports[0].xml)
	wantFragments := []string{
		"<Interval>PT1M</Interval>",
		"<ExecutionTimeLimit>PT1M</ExecutionTimeLimit>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<Arguments>supervise --ensure-alive</Arguments>",
		"<Command>" + livenessFixtureExe + "</Command>",
		"<WorkingDirectory>" + livenessFixtureWorkingDir + "</WorkingDirectory>",
		"<UserId>test</UserId>",
	}
	for _, w := range wantFragments {
		if !strings.Contains(body, w) {
			t.Errorf("CreateXMLIfAbsent body missing %q; full body:\n%s", w, body)
		}
	}
	// The liveness install must NOT forward the watchdog action — proving it
	// is a distinct, additive task (the watchdog install is untouched).
	if strings.Contains(body, "watchdog --once") {
		t.Errorf("liveness CreateXMLIfAbsent body unexpectedly contains the watchdog action")
	}
}

func TestInstallLivenessTask_CreateXMLReceivesUTF16LEBOM(t *testing.T) {
	f := newLivenessTaskScheduler()

	if _, err := ensureLivenessTaskFixtureResolved(f); err != nil {
		t.Fatalf("InstallLivenessTask: %v", err)
	}
	imports := f.createCalls()
	if len(imports) != 1 {
		t.Fatalf("expected 1 CreateXMLIfAbsent call, got %d", len(imports))
	}
	if len(imports[0].xml) < 2 || imports[0].xml[0] != 0xFF || imports[0].xml[1] != 0xFE {
		t.Fatalf("CreateXMLIfAbsent bytes must start with UTF-16 LE BOM; first bytes=% x", imports[0].xml[:min(len(imports[0].xml), 8)])
	}
	decoded := decodeUTF16LEBOMForTest(t, imports[0].xml)
	if !strings.HasPrefix(decoded, `<?xml version="1.0" encoding="UTF-16"?>`) {
		t.Fatalf("decoded liveness XML prefix = %q", decoded[:min(len(decoded), 80)])
	}
	if !strings.Contains(decoded, "<Arguments>supervise --ensure-alive</Arguments>") {
		t.Fatalf("decoded liveness XML missing ensure-alive action:\n%s", decoded)
	}
}

// TestInstallLivenessTask_Idempotent asserts a verified second run is a no-op.
func TestInstallLivenessTask_Idempotent(t *testing.T) {
	f := newLivenessTaskScheduler()

	if _, err := ensureLivenessTaskFixtureResolved(f); err != nil {
		t.Fatalf("first InstallLivenessTask: %v", err)
	}
	if _, err := ensureLivenessTaskFixtureResolved(f); err != nil {
		t.Fatalf("second InstallLivenessTask (idempotent): %v", err)
	}
	if got := len(f.createCalls()); got != 1 {
		t.Errorf("expected one initial CreateXMLIfAbsent call, got %d", got)
	}
}

func TestEnsureLivenessTask_SemanticNormalizationIsIdempotent(t *testing.T) {
	const sid = "S-1-5-21-test"
	canonical := scheduler.BuildLivenessXML(livenessFixtureExe, livenessFixtureWorkingDir, sid, "test")
	cases := []struct {
		name      string
		normalize func(string) string
	}{
		{
			name:      "reversed equivalent trigger order",
			normalize: reverseLivenessTriggerOrderForTest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLivenessTaskScheduler()
			f.tasks[LivenessTaskName] = scheduler.EncodeXMLUTF16LEBOM(tc.normalize(canonical))

			receipt, err := ensureLivenessTaskFixtureResolved(f)
			if err != nil {
				t.Fatalf("EnsureLivenessTask: %v", err)
			}
			if receipt.Result != LivenessTaskUnchanged {
				t.Fatalf("result=%q, want %q", receipt.Result, LivenessTaskUnchanged)
			}
			if got := len(f.createCalls()); got != 0 {
				t.Fatalf("semantic match recreated task %d times", got)
			}
		})
	}
}

type livenessTaskDifferenceReporter interface {
	LivenessTaskDifferenceField() string
}

type livenessTaskPostImportFailureReporter interface {
	LivenessTaskPostImportStage() string
	LivenessTaskRollbackError() error
}

func TestInstallLivenessTask_ReadbackDifferenceReportsField(t *testing.T) {
	replace := func(old, new string) func(string) string {
		return func(taskXML string) string {
			return strings.Replace(taskXML, old, new, 1)
		}
	}
	replaceLast := func(old, new string) func(string) string {
		return func(taskXML string) string {
			at := strings.LastIndex(taskXML, old)
			if at < 0 {
				return taskXML
			}
			return taskXML[:at] + new + taskXML[at+len(old):]
		}
	}
	cases := []struct {
		name      string
		normalize func(string) string
		wantField string
	}{
		{name: "calendar disabled", normalize: replace("<CalendarTrigger>", "<CalendarTrigger><Enabled>false</Enabled>"), wantField: "trigger.calendar.enabled"},
		{name: "calendar future", normalize: replace("2026-06-10T00:00:00", "2099-06-10T00:00:00"), wantField: "trigger.calendar.start_boundary"},
		{name: "cadence", normalize: replace("<Interval>PT1M</Interval>", "<Interval>PT5M</Interval>"), wantField: "trigger.calendar.repetition.interval"},
		{name: "run level", normalize: replace("<RunLevel>LeastPrivilege</RunLevel>", "<RunLevel>HighestAvailable</RunLevel>"), wantField: "principal.run_level"},
		{name: "logon type", normalize: replace("<LogonType>InteractiveToken</LogonType>", "<LogonType>Password</LogonType>"), wantField: "principal.logon_type"},
		{name: "execution limit", normalize: replace("<ExecutionTimeLimit>PT1M</ExecutionTimeLimit>", "<ExecutionTimeLimit>PT5M</ExecutionTimeLimit>"), wantField: "settings.execution_time_limit"},
		{name: "multiple instances", normalize: replace("<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>", "<MultipleInstancesPolicy>Parallel</MultipleInstancesPolicy>"), wantField: "settings.multiple_instances_policy"},
		{name: "command", normalize: replace("<Command>"+livenessFixtureExe+"</Command>", "<Command>foreign.exe</Command>"), wantField: "action.command"},
		{name: "arguments", normalize: replace("<Arguments>supervise --ensure-alive</Arguments>", "<Arguments>supervise</Arguments>"), wantField: "action.arguments"},
		{name: "working directory", normalize: replace("<WorkingDirectory>"+livenessFixtureWorkingDir+"</WorkingDirectory>", "<WorkingDirectory>foreign</WorkingDirectory>"), wantField: "action.working_directory"},
		{name: "enabled", normalize: replaceLast("<Enabled>true</Enabled>", "<Enabled>false</Enabled>"), wantField: "settings.enabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
			f := newLivenessTaskScheduler()
			f.normalizeCreate = func(raw []byte) []byte {
				decoded := decodeUTF16LEBOMForTest(t, raw)
				return scheduler.EncodeXMLUTF16LEBOM(tc.normalize(decoded))
			}

			_, err := ensureLivenessTaskFixtureResolved(f)
			if err == nil {
				t.Fatal("normalized semantic drift was accepted")
			}
			var reporter livenessTaskDifferenceReporter
			if !errors.As(err, &reporter) {
				t.Fatalf("error does not report a typed differing field: %v", err)
			}
			if got := reporter.LivenessTaskDifferenceField(); got != tc.wantField {
				t.Fatalf("difference field=%q, want %q (err=%v)", got, tc.wantField, err)
			}
		})
	}
}

func TestEnsureLivenessTask_ObservedDriftIsRetainedAndRefused(t *testing.T) {
	canonical := scheduler.BuildLivenessXML(livenessFixtureExe, livenessFixtureWorkingDir, "S-1-5-21-test", "test")
	for _, tc := range []struct {
		name  string
		xml   string
		field LivenessTaskDifferenceField
	}{
		{"prior command", scheduler.BuildLivenessXML(livenessPriorFixtureExe, livenessPriorFixtureDir, "S-1-5-21-test", "test"), livenessTaskFieldActionCommand},
		{"disabled calendar", strings.Replace(canonical, "<CalendarTrigger>", "<CalendarTrigger><Enabled>false</Enabled>", 1), livenessTaskFieldCalendarEnabled},
		{"future calendar", strings.Replace(canonical, "2026-06-10T00:00:00", "2099-06-10T00:00:00", 1), livenessTaskFieldCalendarStartBoundary},
		{"corrupt", "<Task>corrupt", livenessTaskFieldDefinition},
		{"empty success", "", livenessTaskFieldDefinition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := hardenedTempDir(t)
			t.Cleanup(SetDaemonStateRootForTest(root))
			f := newLivenessTaskScheduler()
			prior := []byte(tc.xml)
			f.tasks[LivenessTaskName] = bytes.Clone(prior)
			receipt, err := ensureLivenessTaskFixtureResolved(f)
			var difference livenessTaskDifferenceReporter
			if !errors.Is(err, ErrSchedulerReplacementUnowned) || !errors.As(err, &difference) || difference.LivenessTaskDifferenceField() != string(tc.field) {
				t.Fatalf("observed difference=%v, want refusal and field %s", err, tc.field)
			}
			if receipt.Result != "" || len(f.creates) != 0 || len(f.imports) != 0 || len(f.deletes) != 0 || !bytes.Equal(f.tasks[LivenessTaskName], prior) {
				t.Fatalf("refusal changed occupant or reported success: receipt=%+v creates=%d imports=%d deletes=%v", receipt, len(f.creates), len(f.imports), f.deletes)
			}
			var refusal *SchedulerReplacementRefusedError
			if !errors.As(err, &refusal) {
				t.Fatalf("missing typed retention result: %v", err)
			}
			if len(prior) == 0 {
				if refusal.Cause == nil || refusal.SnapshotPath != "" {
					t.Fatalf("empty observation invented snapshot: %+v", refusal)
				}
			} else {
				if refusal.Cause != nil || refusal.SnapshotPath == "" {
					t.Fatalf("observed definition not retained: %+v", refusal)
				}
				readInstallRecoverySnapshot(t, root, LivenessTaskName, prior)
			}
		})
	}
}

func TestEnsureLivenessTask_PostCreateReadbackAndRecoveryErrorsSurvive(t *testing.T) {
	t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
	f := newLivenessTaskScheduler()
	readbackErr := errors.New("simulated post-create readback failure")
	recoveryErr := errors.New("simulated recovery observation failure")
	f.exportErrForCall = func(call int) error {
		if call == 2 {
			return readbackErr
		}
		if call == 3 {
			return recoveryErr
		}
		return nil
	}
	_, err := ensureLivenessTaskFixtureResolved(f)
	var failure livenessTaskPostImportFailureReporter
	if !errors.As(err, &failure) || failure.LivenessTaskPostImportStage() != "readback" || !errors.Is(err, readbackErr) {
		t.Fatalf("primary readback failure was lost: %v", err)
	}
	if !errors.Is(failure.LivenessTaskRollbackError(), recoveryErr) || !errors.Is(err, ErrLivenessTaskRollbackConflict) {
		t.Fatalf("recovery observation failure was lost: %v", err)
	}
	if len(f.creates) != 1 || len(f.imports) != 0 || len(f.deletes) != 0 || !bytes.Equal(f.tasks[LivenessTaskName], f.creates[0].xml) {
		t.Fatalf("failed observations mutated occupant: creates=%d imports=%d deletes=%v", len(f.creates), len(f.imports), f.deletes)
	}
}

func TestEnsureLivenessTask_ReadbackFailureRetainsCreatedTask(t *testing.T) {
	root := hardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(root))
	f := newLivenessTaskScheduler()
	readbackErr := errors.New("simulated post-create readback failure")
	f.exportErrForCall = func(call int) error {
		if call == 2 {
			return readbackErr
		}
		return nil
	}
	receipt, err := ensureLivenessTaskFixtureResolved(f)
	var failure livenessTaskPostImportFailureReporter
	if !errors.As(err, &failure) || failure.LivenessTaskPostImportStage() != "readback" || !errors.Is(err, readbackErr) {
		t.Fatalf("post-create error did not preserve readback failure: %v", err)
	}
	if receipt.Result != LivenessTaskCreated || !errors.Is(failure.LivenessTaskRollbackError(), ErrLivenessTaskRollbackConflict) {
		t.Fatalf("creation/recovery result=%+v / %v", receipt, err)
	}
	if len(f.creates) != 1 || len(f.imports) != 0 || len(f.deletes) != 0 || !bytes.Equal(f.tasks[LivenessTaskName], f.creates[0].xml) {
		t.Fatalf("readback failure mutated occupant: creates=%d imports=%d deletes=%v", len(f.creates), len(f.imports), f.deletes)
	}
	readInstallRecoverySnapshot(t, root, LivenessTaskName, f.tasks[LivenessTaskName])
}

func reverseLivenessTriggerOrderForTest(taskXML string) string {
	calendarStart := strings.Index(taskXML, "    <CalendarTrigger>")
	calendarEnd := strings.Index(taskXML, "    </CalendarTrigger>")
	logonStart := strings.Index(taskXML, "    <LogonTrigger>")
	logonEnd := strings.Index(taskXML, "    </LogonTrigger>")
	if calendarStart < 0 || calendarEnd < calendarStart || logonStart < 0 || logonEnd < logonStart {
		return taskXML
	}
	calendarEnd += len("    </CalendarTrigger>\n")
	logonEnd += len("    </LogonTrigger>\n")
	calendar := taskXML[calendarStart:calendarEnd]
	logon := taskXML[logonStart:logonEnd]
	return taskXML[:calendarStart] + logon + calendar + taskXML[logonEnd:]
}

func TestEnsureLivenessTask_CreateFailureNeverRetriesOrRemoves(t *testing.T) {
	noEffectErr := errors.New("simulated creation failure")
	unknownErr := errors.Join(scheduler.ErrUnavailable, scheduler.ErrMutationOutcomeUnknown)
	for _, tc := range []struct {
		name     string
		want     error
		effect   bool
		conflict bool
	}{
		{name: "no effect", want: noEffectErr},
		{name: "conflicting occupant", want: scheduler.ErrTaskAlreadyExists, effect: true, conflict: true},
		{name: "unknown effect with matching definition", want: unknownErr, effect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := hardenedTempDir(t)
			t.Cleanup(SetDaemonStateRootForTest(root))
			f := newLivenessTaskScheduler()
			f.createErrForCall = func(call int) error {
				if tc.effect {
					f.tasks[LivenessTaskName] = bytes.Clone(f.creates[call-1].xml)
					if tc.conflict {
						f.tasks[LivenessTaskName] = []byte("<Task>competing registration</Task>")
						return nil // CreateXMLIfAbsent itself rejects the now occupied name.
					}
				}
				return tc.want
			}
			receipt, err := ensureLivenessTaskFixtureResolved(f)
			if !errors.Is(err, tc.want) || receipt.Result != "" {
				t.Fatalf("creation failure became success: receipt=%+v err=%v want=%v", receipt, err, tc.want)
			}
			if len(f.creates) != 1 || len(f.imports) != 0 || len(f.deletes) != 0 {
				t.Fatalf("creation failure retried or compensated: creates=%d imports=%d deletes=%v", len(f.creates), len(f.imports), f.deletes)
			}
			if tc.effect {
				current := f.creates[0].xml
				if tc.conflict {
					current = []byte("<Task>competing registration</Task>")
				}
				if !bytes.Equal(f.tasks[LivenessTaskName], current) || !errors.Is(err, ErrSchedulerReplacementUnowned) {
					t.Fatalf("observed occupant was not preserved: %v", err)
				}
				readInstallRecoverySnapshot(t, root, LivenessTaskName, current)
			} else if len(f.tasks) != 0 {
				t.Fatalf("failed creation produced a task: %v", f.tasks)
			}
		})
	}
}

func TestEnsureLivenessTask_AbsenceRequiresNoObservedBytes(t *testing.T) {
	root := hardenedTempDir(t)
	t.Cleanup(SetDaemonStateRootForTest(root))
	f := newLivenessTaskScheduler()
	prior := []byte(scheduler.BuildLivenessXML(livenessFixtureExe, livenessFixtureWorkingDir, "S-1-5-21-test", "test"))
	f.tasks[LivenessTaskName] = bytes.Clone(prior)
	f.exportErrForCall = func(int) error { return scheduler.ErrTaskNotFound }
	f.exportBytesOnError = prior
	_, err := ensureLivenessTaskFixtureResolved(f)
	if !errors.Is(err, scheduler.ErrTaskNotFound) || !errors.Is(err, ErrSchedulerReplacementUnowned) || len(f.creates) != 0 || len(f.imports) != 0 || len(f.deletes) != 0 || !bytes.Equal(f.tasks[LivenessTaskName], prior) {
		t.Fatalf("contradictory absence permitted mutation: err=%v creates=%d imports=%d deletes=%v", err, len(f.creates), len(f.imports), f.deletes)
	}
	readInstallRecoverySnapshot(t, root, LivenessTaskName, prior)
}

func TestEnsureLivenessTask_RequiresXMLCreator(t *testing.T) {
	f := newLivenessTaskScheduler()
	// Hide the optional capability while keeping the ordinary scheduler methods.
	withoutXMLCreator := struct{ scheduler.Scheduler }{f}
	_, err := ensureLivenessTaskFixtureResolved(withoutXMLCreator)
	if !errors.Is(err, scheduler.ErrNotImplemented) || len(f.creates) != 0 || len(f.imports) != 0 || len(f.deletes) != 0 {
		t.Fatalf("missing XMLCreator fell back to mutation: err=%v creates=%d imports=%d deletes=%v", err, len(f.creates), len(f.imports), f.deletes)
	}
}

func TestLivenessTaskReceipt_ConfirmsAbsenceButNeverRemovesOccupant(t *testing.T) {
	for _, occupant := range []string{"created", "foreign", "absent"} {
		t.Run(occupant, func(t *testing.T) {
			root := hardenedTempDir(t)
			t.Cleanup(SetDaemonStateRootForTest(root))
			f := newLivenessTaskScheduler()
			installTestScheduler(t, f)
			receipt, err := ensureLivenessTaskFixtureResolved(f)
			if err != nil || receipt.Result != LivenessTaskCreated {
				t.Fatalf("EnsureLivenessTask: receipt=%+v err=%v", receipt, err)
			}
			switch occupant {
			case "foreign":
				f.tasks[LivenessTaskName] = []byte("<Task>foreign</Task>")
			case "absent":
				delete(f.tasks, LivenessTaskName)
			}
			current := bytes.Clone(f.tasks[LivenessTaskName])
			err = NewAPI().RestoreLivenessTask(receipt)
			if occupant == "absent" {
				if err != nil {
					t.Fatalf("confirmed absence: %v", err)
				}
			} else {
				if !errors.Is(err, ErrLivenessTaskRollbackConflict) || !errors.Is(err, ErrSchedulerReplacementUnowned) {
					t.Fatalf("RestoreLivenessTask err=%v, want ownership conflict", err)
				}
				readInstallRecoverySnapshot(t, root, LivenessTaskName, current)
			}
			if len(f.imports) != 0 || len(f.deletes) != 0 || !bytes.Equal(f.tasks[LivenessTaskName], current) {
				t.Fatalf("Restore mutated occupant: imports=%d deletes=%v", len(f.imports), f.deletes)
			}
		})
	}
}

// TestLivenessWorkingDir_OSIndependent is the bot PR #288 F5 regression: the
// liveness <WorkingDirectory> derivation must NOT use path/filepath.Dir, which
// is OS-specific. On a non-Windows host filepath.Dir of a Windows-shaped path
// finds no '/' separator and returns "." — so the rendered XML (and the
// happy-path test's <WorkingDirectory> assertion) differed by host OS and the
// test FAILED on Linux/macOS. livenessWorkingDir splits on the last separator
// of EITHER kind, so it yields the correct parent dir for both the Windows
// canonical path (backslash) and the POSIX canonical path (forward slash),
// independent of the host's filepath separator.
//
// Negative-control: replacing livenessWorkingDir(canonicalExe) with
// filepath.Dir(canonicalExe) in liveness_task.go makes the first case below
// return "." on a non-Windows `go test` host — this test then fails there
// (and TestInstallLivenessTask_HappyPath's WorkingDirectory fragment too).
// On a Windows host filepath.Dir would still pass, which is exactly why the
// pre-fix bug was invisible to a Windows-only CI run.
func TestLivenessWorkingDir_OSIndependent(t *testing.T) {
	cases := []struct {
		name string
		exe  string
		want string
	}{
		{
			name: "windows-backslash-path",
			exe:  livenessFixtureExe,
			want: livenessFixtureWorkingDir,
		},
		{
			name: "posix-forward-slash-path",
			exe:  "/home/test/.local/bin/mcphub",
			want: "/home/test/.local/bin",
		},
		{
			name: "no-separator-returns-self",
			exe:  "mcphub.exe",
			want: "mcphub.exe",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := livenessWorkingDir(tc.exe); got != tc.want {
				t.Errorf("livenessWorkingDir(%q) = %q, want %q (must be host-OS-independent)", tc.exe, got, tc.want)
			}
		})
	}
}

func decodeUTF16LEBOMForTest(t *testing.T, b []byte) string {
	t.Helper()
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE {
		t.Fatalf("missing UTF-16 LE BOM; first bytes=% x", b[:min(len(b), 8)])
	}
	if (len(b)-2)%2 != 0 {
		t.Fatalf("UTF-16 LE payload has odd byte length: %d", len(b)-2)
	}
	units := make([]uint16, 0, (len(b)-2)/2)
	for i := 2; i < len(b); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(b[i:i+2]))
	}
	return string(utf16.Decode(units))
}

// TestUninstallLivenessTask_DeletesByName asserts the symmetric teardown
// deletes the LivenessTaskName via the scheduler factory seam.
func TestUninstallLivenessTask_DeletesByName(t *testing.T) {
	a := NewAPI()
	f := newLivenessTaskScheduler()
	installTestScheduler(t, f)

	if err := a.UninstallLivenessTask(); err != nil {
		t.Fatalf("UninstallLivenessTask: %v", err)
	}
	dels := f.calls()
	if len(dels) != 1 || dels[0] != LivenessTaskName {
		t.Errorf("Delete calls = %v, want exactly [%q]", dels, LivenessTaskName)
	}
}

func TestRetainLegacyWatchdogTaskForSetup(t *testing.T) {
	name := LegacyWatchdogTaskName
	prior := scheduler.EncodeXMLUTF16LEBOM("<Task>legacy watchdog</Task>")
	for _, tc := range []struct {
		name      string
		xml       []byte
		exportErr error
		wantErr   error
		retained  bool
	}{
		{name: "absent", exportErr: scheduler.ErrTaskNotFound},
		{name: "observed", xml: prior, wantErr: ErrSchedulerReplacementUnowned, retained: true},
		{name: "conflicting_absence", xml: prior, exportErr: scheduler.ErrTaskNotFound, wantErr: scheduler.ErrTaskNotFound},
		{name: "partial_read", xml: prior, exportErr: scheduler.ErrPermissionDenied, wantErr: scheduler.ErrPermissionDenied},
		{name: "empty_success", wantErr: scheduler.ErrTaskCorrupt},
		{name: "retention_failed", xml: bytes.Repeat([]byte{'x'}, maxStateFileBytes+1), wantErr: ErrSchedulerReplacementUnowned},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := hardenedTempDir(t)
			t.Cleanup(SetDaemonStateRootForTest(root))
			f := newInstallFakeScheduler()
			if len(tc.xml) > 0 {
				f.tasks[name], f.xml[name] = true, bytes.Clone(tc.xml)
			}
			installTestScheduler(t, &installSnapshotScheduler{fakeScheduler: f, name: name, snapshot: tc.xml, exportErr: tc.exportErr})

			err := NewAPI().RetainLegacyWatchdogTaskForSetup()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("retention error = %v, want %v", err, tc.wantErr)
			}
			if f.tasks[name] != (len(tc.xml) > 0) || !bytes.Equal(f.xml[name], tc.xml) || len(f.deleteNames) != 0 || f.createCount != 0 || len(f.importNames) != 0 {
				t.Fatalf("observation mutated scheduler: deletes=%v creates=%d imports=%v", f.deleteNames, f.createCount, f.importNames)
			}
			var refusal *SchedulerReplacementRefusedError
			if tc.wantErr == ErrSchedulerReplacementUnowned {
				if !errors.As(err, &refusal) || refusal.TaskName != name || (refusal.Cause == nil) != tc.retained {
					t.Fatalf("invalid retention receipt: %v", err)
				}
				if tc.retained {
					if path := readInstallRecoverySnapshot(t, root, name, prior); refusal.SnapshotPath != path {
						t.Fatalf("snapshot path = %q, want %q", refusal.SnapshotPath, path)
					}
					return
				}
				if refusal.SnapshotPath != "" {
					t.Fatalf("failed retention reported a snapshot path: %q", refusal.SnapshotPath)
				}
			}
			if paths, err := filepath.Glob(filepath.Join(root, "scheduler-recovery-*.json")); err != nil || len(paths) != 0 {
				t.Fatalf("unexpected snapshots: %v, error %v", paths, err)
			}
		})
	}
}

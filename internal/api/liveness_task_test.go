package api

import (
	"bytes"
	"encoding/binary"
	"errors"
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
	tasks            map[string][]byte
	imports          []importXMLCall
	deletes          []string
	importErr        error
	importErrForCall func(int) error
	exportErrForCall func(int) error
	exportCalls      int
	normalizeImport  func([]byte) []byte
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
			return nil, err
		}
	}
	xml, ok := f.tasks[name]
	if !ok {
		return nil, scheduler.ErrTaskNotFound
	}
	return append([]byte(nil), xml...), nil
}
func (f *livenessTaskScheduler) ImportXML(name string, xml []byte) error {
	f.imports = append(f.imports, importXMLCall{name: name, xml: append([]byte(nil), xml...)})
	if f.importErrForCall != nil {
		if err := f.importErrForCall(len(f.imports)); err != nil {
			return err
		}
	}
	if f.importErr != nil {
		return f.importErr
	}
	settled := append([]byte(nil), xml...)
	if f.normalizeImport != nil {
		settled = f.normalizeImport(settled)
	}
	f.tasks[name] = settled
	return nil
}
func (f *livenessTaskScheduler) Delete(name string) error {
	f.deletes = append(f.deletes, name)
	delete(f.tasks, name)
	return nil
}
func (f *livenessTaskScheduler) importCalls() []importXMLCall {
	return append([]importXMLCall(nil), f.imports...)
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
	f.normalizeImport = func(raw []byte) []byte {
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
	if got := len(f.importCalls()); got != 1 {
		t.Fatalf("first ensure ImportXML calls=%d, want 1", got)
	}

	receipt, err = ensureLivenessTaskResolved(livenessFixtureExe, livenessFixtureWorkingDir, sid, account, f)
	if err != nil {
		t.Fatalf("second resolved ensure: %v", err)
	}
	if receipt.Result != LivenessTaskUnchanged {
		t.Fatalf("second result=%q, want %q", receipt.Result, LivenessTaskUnchanged)
	}
	if got := len(f.importCalls()); got != 1 {
		t.Fatalf("second ensure re-imported normalized task: calls=%d, want 1", got)
	}
}

func TestEnsureLivenessTaskResolved_StrictOppositeReadbackRestoresExactPriorXML(t *testing.T) {
	const sid, account = "S-1-5-21-101-202-303-404", "account-canary"
	f := newLivenessTaskScheduler()
	prior := scheduler.EncodeXMLUTF16LEBOM(scheduler.BuildLivenessXML(
		livenessPriorFixtureExe, livenessPriorFixtureDir, "S-1-5-21-prior", "prior-account"))
	f.tasks[LivenessTaskName] = prior
	f.normalizeImport = func(raw []byte) []byte {
		if len(f.importCalls()) != 1 {
			return raw
		}
		return scheduler.EncodeXMLUTF16LEBOM(strings.Replace(
			decodeUTF16LEBOMForTest(t, raw),
			"<RunLevel>LeastPrivilege</RunLevel>",
			"<RunLevel>HighestAvailable</RunLevel>",
			1,
		))
	}

	_, err := ensureLivenessTaskResolved(livenessFixtureExe, livenessFixtureWorkingDir, sid, account, f)
	if err == nil {
		t.Fatal("strict opposite run level was accepted after import")
	}
	var failure livenessTaskPostImportFailureReporter
	if !errors.As(err, &failure) {
		t.Fatalf("post-import error is not typed: %v", err)
	}
	if got := failure.LivenessTaskPostImportStage(); got != string(LivenessTaskPostImportSemanticDrift) {
		t.Fatalf("post-import stage=%q, want %q", got, LivenessTaskPostImportSemanticDrift)
	}
	var difference livenessTaskDifferenceReporter
	if !errors.As(err, &difference) || difference.LivenessTaskDifferenceField() != string(livenessTaskFieldPrincipalRunLevel) {
		t.Fatalf("strict opposite difference=%v, want %s", err, livenessTaskFieldPrincipalRunLevel)
	}
	if got := f.tasks[LivenessTaskName]; !bytes.Equal(got, prior) {
		t.Fatalf("strict opposite readback left XML %q, want exact prior %q", got, prior)
	}
	if got := len(f.importCalls()); got != 2 {
		t.Fatalf("strict opposite ImportXML calls=%d, want replacement plus restoration", got)
	}
}

// TestInstallLivenessTask_HappyPath asserts the supervisor-liveness install
// (v0.6 spec §15 P1-b / §5.x Phase 3a) resolves the canonical mcphub path +
// current user via the seams, then ImportXML under LivenessTaskName with the
// liveness XML body (PT1M cadence + `supervise --ensure-alive` action). Reuses
// the apiSurfacesFakeScheduler + seam helpers from api_surfaces_test.go.
func TestInstallLivenessTask_HappyPath(t *testing.T) {
	f := newLivenessTaskScheduler()

	if _, err := ensureLivenessTaskFixtureResolved(f); err != nil {
		t.Fatalf("InstallLivenessTask: %v", err)
	}

	imports := f.importCalls()
	if len(imports) != 1 {
		t.Fatalf("expected 1 ImportXML call, got %d", len(imports))
	}
	if imports[0].name != LivenessTaskName {
		t.Errorf("ImportXML target name: got %q, want %q", imports[0].name, LivenessTaskName)
	}
	body := decodeUTF16LEBOMForTest(t, imports[0].xml)
	wantFragments := []string{
		"<Interval>PT1M</Interval>",
		"<ExecutionTimeLimit>PT1M</ExecutionTimeLimit>",
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<Arguments>supervise --ensure-alive</Arguments>",
		"<Command>" + scheduler.WindowsOwnedEntrypointPath(livenessFixtureExe) + "</Command>",
		"<WorkingDirectory>" + livenessFixtureWorkingDir + "</WorkingDirectory>",
		"<UserId>test</UserId>",
	}
	for _, w := range wantFragments {
		if !strings.Contains(body, w) {
			t.Errorf("ImportXML body missing %q; full body:\n%s", w, body)
		}
	}
	// The liveness install must NOT forward the watchdog action — proving it
	// is a distinct, additive task (the watchdog install is untouched).
	if strings.Contains(body, "watchdog --once") {
		t.Errorf("liveness ImportXML body unexpectedly contains the watchdog action")
	}
}

func TestInstallLivenessTask_ImportXMLReceivesUTF16LEBOM(t *testing.T) {
	f := newLivenessTaskScheduler()

	if _, err := ensureLivenessTaskFixtureResolved(f); err != nil {
		t.Fatalf("InstallLivenessTask: %v", err)
	}
	imports := f.importCalls()
	if len(imports) != 1 {
		t.Fatalf("expected 1 ImportXML call, got %d", len(imports))
	}
	if len(imports[0].xml) < 2 || imports[0].xml[0] != 0xFF || imports[0].xml[1] != 0xFE {
		t.Fatalf("ImportXML bytes must start with UTF-16 LE BOM; first bytes=% x", imports[0].xml[:min(len(imports[0].xml), 8)])
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
	if got := len(f.importCalls()); got != 1 {
		t.Errorf("expected one initial ImportXML call, got %d", got)
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
			if got := len(f.importCalls()); got != 0 {
				t.Fatalf("semantic match re-imported task %d times", got)
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
		{name: "cadence", normalize: replace("<Interval>PT1M</Interval>", "<Interval>PT5M</Interval>"), wantField: "trigger.calendar.repetition.interval"},
		{name: "run level", normalize: replace("<RunLevel>LeastPrivilege</RunLevel>", "<RunLevel>HighestAvailable</RunLevel>"), wantField: "principal.run_level"},
		{name: "logon type", normalize: replace("<LogonType>InteractiveToken</LogonType>", "<LogonType>Password</LogonType>"), wantField: "principal.logon_type"},
		{name: "execution limit", normalize: replace("<ExecutionTimeLimit>PT1M</ExecutionTimeLimit>", "<ExecutionTimeLimit>PT5M</ExecutionTimeLimit>"), wantField: "settings.execution_time_limit"},
		{name: "multiple instances", normalize: replace("<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>", "<MultipleInstancesPolicy>Parallel</MultipleInstancesPolicy>"), wantField: "settings.multiple_instances_policy"},
		{name: "command", normalize: replace("<Command>"+scheduler.WindowsOwnedEntrypointPath(livenessFixtureExe)+"</Command>", "<Command>foreign.exe</Command>"), wantField: "action.command"},
		{name: "arguments", normalize: replace("<Arguments>supervise --ensure-alive</Arguments>", "<Arguments>supervise</Arguments>"), wantField: "action.arguments"},
		{name: "working directory", normalize: replace("<WorkingDirectory>"+livenessFixtureWorkingDir+"</WorkingDirectory>", "<WorkingDirectory>foreign</WorkingDirectory>"), wantField: "action.working_directory"},
		{name: "enabled", normalize: replaceLast("<Enabled>true</Enabled>", "<Enabled>false</Enabled>"), wantField: "settings.enabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLivenessTaskScheduler()
			f.normalizeImport = func(raw []byte) []byte {
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

func TestEnsureLivenessTask_ReadbackDriftRestoresExactPriorXML(t *testing.T) {
	f := newLivenessTaskScheduler()
	prior := scheduler.EncodeXMLUTF16LEBOM(scheduler.BuildLivenessXML(
		livenessPriorFixtureExe, livenessPriorFixtureDir, "S-1-5-21-test", "test"))
	f.tasks[LivenessTaskName] = prior
	f.normalizeImport = func(raw []byte) []byte {
		if len(f.imports) != 1 {
			return append([]byte(nil), raw...)
		}
		return scheduler.EncodeXMLUTF16LEBOM(strings.Replace(
			decodeUTF16LEBOMForTest(t, raw),
			"<Interval>PT1M</Interval>",
			"<Interval>PT5M</Interval>",
			1,
		))
	}

	_, err := ensureLivenessTaskFixtureResolved(f)
	if err == nil {
		t.Fatal("EnsureLivenessTask accepted post-import semantic drift")
	}
	var failure livenessTaskPostImportFailureReporter
	if !errors.As(err, &failure) {
		t.Fatalf("post-import error is not typed: %v", err)
	}
	if got := failure.LivenessTaskPostImportStage(); got != "semantic-drift" {
		t.Fatalf("post-import stage=%q, want semantic-drift", got)
	}
	if rollbackErr := failure.LivenessTaskRollbackError(); rollbackErr != nil {
		t.Fatalf("rollback error = %v, want nil", rollbackErr)
	}
	if got := f.tasks[LivenessTaskName]; !bytes.Equal(got, prior) {
		t.Fatalf("post-import failure left replacement task: got %q, want exact prior XML %q", got, prior)
	}
	if got := len(f.importCalls()); got != 2 {
		t.Fatalf("ImportXML calls=%d, want replacement plus restoration", got)
	}
}

func TestEnsureLivenessTask_PostImportRollbackFailureIsRecoveryRequired(t *testing.T) {
	f := newLivenessTaskScheduler()
	prior := scheduler.EncodeXMLUTF16LEBOM(scheduler.BuildLivenessXML(
		livenessPriorFixtureExe, livenessPriorFixtureDir, "S-1-5-21-test", "test"))
	f.tasks[LivenessTaskName] = prior
	restoreErr := errors.New("simulated restore failure")
	f.importErrForCall = func(call int) error {
		if call == 2 {
			return restoreErr
		}
		return nil
	}
	f.normalizeImport = func(raw []byte) []byte {
		if len(f.imports) != 1 {
			return append([]byte(nil), raw...)
		}
		return scheduler.EncodeXMLUTF16LEBOM(strings.Replace(
			decodeUTF16LEBOMForTest(t, raw),
			"<Interval>PT1M</Interval>",
			"<Interval>PT5M</Interval>",
			1,
		))
	}

	_, err := ensureLivenessTaskFixtureResolved(f)
	if err == nil {
		t.Fatal("EnsureLivenessTask accepted post-import semantic drift")
	}
	var failure livenessTaskPostImportFailureReporter
	if !errors.As(err, &failure) {
		t.Fatalf("post-import error is not typed: %v", err)
	}
	if got := failure.LivenessTaskPostImportStage(); got != "semantic-drift" {
		t.Fatalf("post-import stage=%q, want semantic-drift", got)
	}
	if !errors.Is(failure.LivenessTaskRollbackError(), restoreErr) {
		t.Fatalf("rollback error=%v, want errors.Is(..., %v)", failure.LivenessTaskRollbackError(), restoreErr)
	}
	if got := f.tasks[LivenessTaskName]; bytes.Equal(got, prior) {
		t.Fatal("restore failure unexpectedly reported the exact prior XML as restored")
	}
}

func TestEnsureLivenessTask_ReadbackFailureRestoresExactPriorXML(t *testing.T) {
	f := newLivenessTaskScheduler()
	prior := scheduler.EncodeXMLUTF16LEBOM(scheduler.BuildLivenessXML(
		livenessPriorFixtureExe, livenessPriorFixtureDir, "S-1-5-21-test", "test"))
	f.tasks[LivenessTaskName] = prior
	readbackErr := errors.New("simulated post-import readback failure")
	f.exportErrForCall = func(call int) error {
		if call == 2 {
			return readbackErr
		}
		return nil
	}

	_, err := ensureLivenessTaskFixtureResolved(f)
	if err == nil {
		t.Fatal("EnsureLivenessTask accepted a failed post-import readback")
	}
	var failure livenessTaskPostImportFailureReporter
	if !errors.As(err, &failure) {
		t.Fatalf("post-import error is not typed: %v", err)
	}
	if got := failure.LivenessTaskPostImportStage(); got != "readback" {
		t.Fatalf("post-import stage=%q, want readback", got)
	}
	if !errors.Is(err, readbackErr) {
		t.Fatalf("post-import error=%v, want errors.Is(..., %v)", err, readbackErr)
	}
	if rollbackErr := failure.LivenessTaskRollbackError(); rollbackErr != nil {
		t.Fatalf("rollback error=%v, want nil", rollbackErr)
	}
	if got := f.tasks[LivenessTaskName]; !bytes.Equal(got, prior) {
		t.Fatalf("post-import readback failure left replacement task: got %q, want exact prior XML %q", got, prior)
	}
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

// TestInstallLivenessTask_PropagatesImportXMLError asserts a scheduler failure
// is surfaced verbatim — the install path does not swallow errors.
func TestInstallLivenessTask_PropagatesImportXMLError(t *testing.T) {
	want := errors.New("simulated schtasks failure")
	f := newLivenessTaskScheduler()
	prior := scheduler.EncodeXMLUTF16LEBOM(scheduler.BuildLivenessXML(
		livenessPriorFixtureExe, livenessPriorFixtureDir, "S-1-5-21-test", "test"))
	f.tasks[LivenessTaskName] = prior
	f.importErr = want

	_, err := ensureLivenessTaskFixtureResolved(f)
	if err == nil {
		t.Fatal("InstallLivenessTask: want error, got nil")
	}
	if !errors.Is(err, want) {
		t.Errorf("InstallLivenessTask: want errors.Is(err, want); got %v", err)
	}
	if got := f.tasks[LivenessTaskName]; !bytes.Equal(got, prior) {
		t.Fatalf("pre-import failure changed prior task: got %q, want %q", got, prior)
	}
	if got := len(f.importCalls()); got != 1 {
		t.Fatalf("pre-import failure attempted rollback: ImportXML calls=%d, want 1", got)
	}
}

func TestLivenessTaskReceipt_RestoresExactPriorXMLAndPreservesForeignReplacement(t *testing.T) {
	a := NewAPI()
	f := newLivenessTaskScheduler()
	prior := scheduler.EncodeXMLUTF16LEBOM(scheduler.BuildLivenessXML(
		livenessPriorFixtureExe, livenessPriorFixtureDir, "S-1-5-21-test", "test"))
	f.tasks[LivenessTaskName] = prior
	installTestScheduler(t, f)

	receipt, err := ensureLivenessTaskFixtureResolved(f)
	if err != nil {
		t.Fatalf("EnsureLivenessTask: %v", err)
	}
	if receipt.Result != LivenessTaskReplaced {
		t.Fatalf("receipt result = %q, want %q", receipt.Result, LivenessTaskReplaced)
	}
	if err := a.RestoreLivenessTask(receipt); err != nil {
		t.Fatalf("RestoreLivenessTask: %v", err)
	}
	if got := f.tasks[LivenessTaskName]; !bytes.Equal(got, prior) {
		t.Fatalf("SchedulerRollbackExactXML: restored XML = %q, want %q", got, prior)
	}

	receipt, err = ensureLivenessTaskFixtureResolved(f)
	if err != nil {
		t.Fatalf("EnsureLivenessTask second: %v", err)
	}
	f.tasks[LivenessTaskName] = []byte("<Task>foreign</Task>")
	err = a.RestoreLivenessTask(receipt)
	if !errors.Is(err, ErrLivenessTaskRollbackConflict) {
		t.Fatalf("RestoreLivenessTask err = %v, want conflict", err)
	}
	if len(f.deletes) != 0 {
		t.Fatalf("foreign liveness task must not be deleted: %v", f.deletes)
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

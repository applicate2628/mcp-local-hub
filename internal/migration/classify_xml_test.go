package migration

import (
	"os"
	"strings"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// currentUserName mirrors what the v0.4.x Windows scheduler injects into
// <UserId>: the short username (no DOMAIN\ prefix). Tests must NEVER touch
// the real Windows user account, so we use the env var as the anchor and
// fall back to a fixed literal — the deviation classifier accepts the
// current-user parameter as an injected dependency for exactly this reason.
func currentUserName() string {
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "testuser"
}

// taskSpecMemoryDefault is the canonical "memory daemon default-binding"
// TaskSpec — the most common shape produced by `mcphub install` for a
// v0.4.x daemon task. Both LogonTrigger and RestartOnFailure are on.
func taskSpecMemoryDefault() scheduler.TaskSpec {
	return scheduler.TaskSpec{
		Name:             "mcp-local-hub-memory-default",
		Description:      "memory daemon (default)",
		Command:          `C:\install\mcphub.exe`,
		Args:             []string{"daemon", "--server", "memory", "--daemon", "default"},
		WorkingDir:       `C:\repo`,
		LogonTrigger:     true,
		RestartOnFailure: true,
	}
}

// canonicalV04xTemplateXML wraps V04xTemplateXML with the test user so the
// test bodies read closer to the plan §2393–2415 verbatim contract.
func canonicalV04xTemplateXML(spec scheduler.TaskSpec) string {
	return V04xTemplateXML(spec, currentUserName())
}

// --- plan §2393–2415 verbatim contract -------------------------------------

func TestClassifyXML_DefaultMatchSilent(t *testing.T) {
	xml := canonicalV04xTemplateXML(taskSpecMemoryDefault())
	report := ClassifyXMLDeviations(xml, taskSpecMemoryDefault(), currentUserName())
	if len(report.Deviations) != 0 {
		t.Fatalf("default-matching XML should not classify deviations: %+v", report.Deviations)
	}
	if report.HasUnsupportedAbort {
		t.Fatalf("default-matching XML must not set HasUnsupportedAbort")
	}
}

func TestClassifyXML_UnsupportedPrincipalUserID(t *testing.T) {
	xml := canonicalV04xTemplateXML(taskSpecMemoryDefault())
	xml = strings.Replace(xml,
		`<UserId>`+currentUserName()+`</UserId>`,
		`<UserId>SomeOtherUser</UserId>`,
		-1) // both LogonTrigger.UserId and Principal.UserId carry the username
	report := ClassifyXMLDeviations(xml, taskSpecMemoryDefault(), currentUserName())
	if !report.HasUnsupportedAbort {
		t.Fatalf("non-current-user UserId should classify as Unsupported abort; got %+v", report.Deviations)
	}
}

// --- extended coverage (from task spec) ------------------------------------

func TestClassifyXML_KindKnownPreserveIntent_NetworkAvailable(t *testing.T) {
	xml := canonicalV04xTemplateXML(taskSpecMemoryDefault())
	xml = strings.Replace(xml,
		"<RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>",
		"<RunOnlyIfNetworkAvailable>true</RunOnlyIfNetworkAvailable>", 1)
	report := ClassifyXMLDeviations(xml, taskSpecMemoryDefault(), currentUserName())

	if report.HasUnsupportedAbort {
		t.Fatalf("RunOnlyIfNetworkAvailable=true must not set the abort flag")
	}
	if !hasDeviation(report, "Settings.RunOnlyIfNetworkAvailable", KindKnownPreserveIntent) {
		t.Fatalf("expected KindKnownPreserveIntent at Settings.RunOnlyIfNetworkAvailable; got %+v", report.Deviations)
	}
}

func TestClassifyXML_KindKnownPreserveShim_CalendarTrigger(t *testing.T) {
	// Observed XML has CalendarTrigger instead of LogonTrigger — non-LogonTrigger
	// trigger that should ride the autostart shim per spec bucket 3.
	spec := taskSpecMemoryDefault()
	pinned := canonicalV04xTemplateXML(spec)
	// Splice in a CalendarTrigger block replacing the LogonTrigger block.
	calendar := `<CalendarTrigger>
      <StartBoundary>2026-01-04T03:00:00</StartBoundary>
      <Enabled>true</Enabled>
      <ScheduleByWeek>
        <DaysOfWeek><Sunday /></DaysOfWeek>
        <WeeksInterval>1</WeeksInterval>
      </ScheduleByWeek>
    </CalendarTrigger>`
	startTok := "<LogonTrigger>"
	endTok := "</LogonTrigger>"
	i := strings.Index(pinned, startTok)
	j := strings.Index(pinned, endTok)
	if i < 0 || j < 0 {
		t.Fatalf("pinned XML missing LogonTrigger block (got: %s)", pinned)
	}
	observed := pinned[:i] + calendar + pinned[j+len(endTok):]

	report := ClassifyXMLDeviations(observed, spec, currentUserName())
	if report.HasUnsupportedAbort {
		t.Fatalf("CalendarTrigger replacement must not set the abort flag")
	}
	if !hasDeviationKind(report, KindKnownPreserveShim) {
		t.Fatalf("expected at least one KindKnownPreserveShim deviation; got %+v", report.Deviations)
	}
}

func TestClassifyXML_KindUnsupportedAbort_WrongLogonType(t *testing.T) {
	xml := canonicalV04xTemplateXML(taskSpecMemoryDefault())
	xml = strings.Replace(xml,
		"<LogonType>InteractiveToken</LogonType>",
		"<LogonType>Password</LogonType>", 1)
	report := ClassifyXMLDeviations(xml, taskSpecMemoryDefault(), currentUserName())
	if !report.HasUnsupportedAbort {
		t.Fatalf("LogonType=Password must set abort flag; got %+v", report.Deviations)
	}
	if !hasDeviation(report, "Principals.Principal.LogonType", KindUnsupportedAbort) {
		t.Fatalf("expected KindUnsupportedAbort at Principals.Principal.LogonType; got %+v", report.Deviations)
	}
}

func TestClassifyXML_KindUnsupportedAbort_CustomActions(t *testing.T) {
	spec := taskSpecMemoryDefault()
	xml := canonicalV04xTemplateXML(spec)
	xml = strings.Replace(xml,
		"<Command>"+spec.Command+"</Command>",
		"<Command>cmd.exe</Command>", 1)
	report := ClassifyXMLDeviations(xml, spec, currentUserName())
	if !report.HasUnsupportedAbort {
		t.Fatalf("custom Actions Command must set abort flag; got %+v", report.Deviations)
	}
	if !hasDeviation(report, "Actions.Exec.Command", KindUnsupportedAbort) {
		t.Fatalf("expected KindUnsupportedAbort at Actions.Exec.Command; got %+v", report.Deviations)
	}
}

func TestClassifyXML_KindUnsupportedAbort_StopOnIdleEnd(t *testing.T) {
	xml := canonicalV04xTemplateXML(taskSpecMemoryDefault())
	xml = strings.Replace(xml,
		"<StopOnIdleEnd>false</StopOnIdleEnd>",
		"<StopOnIdleEnd>true</StopOnIdleEnd>", 1)
	report := ClassifyXMLDeviations(xml, taskSpecMemoryDefault(), currentUserName())
	if !report.HasUnsupportedAbort {
		t.Fatalf("StopOnIdleEnd=true must set abort flag; got %+v", report.Deviations)
	}
	if !hasDeviation(report, "Settings.IdleSettings.StopOnIdleEnd", KindUnsupportedAbort) {
		t.Fatalf("expected KindUnsupportedAbort at Settings.IdleSettings.StopOnIdleEnd; got %+v", report.Deviations)
	}
}

func TestClassifyXML_KindUnsupportedAbort_WakeToRun(t *testing.T) {
	xml := canonicalV04xTemplateXML(taskSpecMemoryDefault())
	xml = strings.Replace(xml,
		"<WakeToRun>false</WakeToRun>",
		"<WakeToRun>true</WakeToRun>", 1)
	report := ClassifyXMLDeviations(xml, taskSpecMemoryDefault(), currentUserName())
	if !report.HasUnsupportedAbort {
		t.Fatalf("WakeToRun=true must set abort flag; got %+v", report.Deviations)
	}
	if !hasDeviation(report, "Settings.WakeToRun", KindUnsupportedAbort) {
		t.Fatalf("expected KindUnsupportedAbort at Settings.WakeToRun; got %+v", report.Deviations)
	}
}

func TestClassifyXML_KindUnsupportedAbort_RunOnlyIfIdle(t *testing.T) {
	xml := canonicalV04xTemplateXML(taskSpecMemoryDefault())
	xml = strings.Replace(xml,
		"<RunOnlyIfIdle>false</RunOnlyIfIdle>",
		"<RunOnlyIfIdle>true</RunOnlyIfIdle>", 1)
	report := ClassifyXMLDeviations(xml, taskSpecMemoryDefault(), currentUserName())
	if !report.HasUnsupportedAbort {
		t.Fatalf("RunOnlyIfIdle=true must set abort flag; got %+v", report.Deviations)
	}
	if !hasDeviation(report, "Settings.RunOnlyIfIdle", KindUnsupportedAbort) {
		t.Fatalf("expected KindUnsupportedAbort at Settings.RunOnlyIfIdle; got %+v", report.Deviations)
	}
}

func TestClassifyXML_KindKnownDrop_Priority(t *testing.T) {
	xml := canonicalV04xTemplateXML(taskSpecMemoryDefault())
	xml = strings.Replace(xml, "<Priority>7</Priority>", "<Priority>5</Priority>", 1)
	report := ClassifyXMLDeviations(xml, taskSpecMemoryDefault(), currentUserName())
	if report.HasUnsupportedAbort {
		t.Fatalf("Priority drift is bucket 5 (drop-with-warning); abort flag must NOT be set: %+v", report.Deviations)
	}
	if !hasDeviation(report, "Settings.Priority", KindKnownDrop) {
		t.Fatalf("expected KindKnownDrop at Settings.Priority; got %+v", report.Deviations)
	}
}

func TestClassifyXML_KindUnknownDrift_Element(t *testing.T) {
	// Inject an unknown element inside <Settings>. The classifier must
	// surface it as KindUnknownDrift (NOT abort).
	xml := canonicalV04xTemplateXML(taskSpecMemoryDefault())
	xml = strings.Replace(xml,
		"<Settings>",
		"<Settings>\n    <NewSchemaField>foo</NewSchemaField>", 1)
	report := ClassifyXMLDeviations(xml, taskSpecMemoryDefault(), currentUserName())
	if report.HasUnsupportedAbort {
		t.Fatalf("unknown element under Settings must not abort (warn+preserve); got %+v", report.Deviations)
	}
	if !hasDeviationKind(report, KindUnknownDrift) {
		t.Fatalf("expected at least one KindUnknownDrift; got %+v", report.Deviations)
	}
}

func TestClassifyXML_KindUnknownDrift_ValueDrift(t *testing.T) {
	// MultipleInstancesPolicy=Queue is a valid Task Scheduler schema value
	// but not in the pinned defaults table — fall through to unknown drift.
	xml := canonicalV04xTemplateXML(taskSpecMemoryDefault())
	xml = strings.Replace(xml,
		"<MultipleInstancesPolicy>StopExisting</MultipleInstancesPolicy>",
		"<MultipleInstancesPolicy>Queue</MultipleInstancesPolicy>", 1)
	report := ClassifyXMLDeviations(xml, taskSpecMemoryDefault(), currentUserName())
	if report.HasUnsupportedAbort {
		t.Fatalf("MultipleInstancesPolicy=Queue is value-drift (warn+preserve); must not abort: %+v", report.Deviations)
	}
	if !hasDeviation(report, "Settings.MultipleInstancesPolicy", KindUnknownDrift) {
		t.Fatalf("expected KindUnknownDrift at Settings.MultipleInstancesPolicy; got %+v", report.Deviations)
	}
}

func TestClassifyXML_MultipleDeviations(t *testing.T) {
	xml := canonicalV04xTemplateXML(taskSpecMemoryDefault())
	xml = strings.Replace(xml, "<Priority>7</Priority>", "<Priority>5</Priority>", 1)
	xml = strings.Replace(xml, "<WakeToRun>false</WakeToRun>", "<WakeToRun>true</WakeToRun>", 1)
	report := ClassifyXMLDeviations(xml, taskSpecMemoryDefault(), currentUserName())
	if !report.HasUnsupportedAbort {
		t.Fatalf("ANY single KindUnsupportedAbort must set the convenience flag; got %+v", report.Deviations)
	}
	if !hasDeviation(report, "Settings.Priority", KindKnownDrop) {
		t.Fatalf("expected KindKnownDrop at Settings.Priority; got %+v", report.Deviations)
	}
	if !hasDeviation(report, "Settings.WakeToRun", KindUnsupportedAbort) {
		t.Fatalf("expected KindUnsupportedAbort at Settings.WakeToRun; got %+v", report.Deviations)
	}
}

func TestV04xTemplateXML_ContainsPinnedPriorities(t *testing.T) {
	xml := canonicalV04xTemplateXML(taskSpecMemoryDefault())
	mustContain := []string{
		"<Priority>7</Priority>",
		"<MultipleInstancesPolicy>StopExisting</MultipleInstancesPolicy>",
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		"<AllowHardTerminate>true</AllowHardTerminate>",
		"<StartWhenAvailable>false</StartWhenAvailable>",
		"<RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"<StopOnIdleEnd>false</StopOnIdleEnd>",
		"<RestartOnIdle>false</RestartOnIdle>",
		"<AllowStartOnDemand>true</AllowStartOnDemand>",
		"<Enabled>true</Enabled>",
		"<Hidden>false</Hidden>",
		"<RunOnlyIfIdle>false</RunOnlyIfIdle>",
		"<WakeToRun>false</WakeToRun>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<Interval>PT1M</Interval>",
		"<Count>3</Count>",
		"<UserId>" + currentUserName() + "</UserId>",
	}
	for _, s := range mustContain {
		if !strings.Contains(xml, s) {
			t.Errorf("pinned template missing %q\n--- full xml:\n%s", s, xml)
		}
	}
}

func TestClassifyXML_MalformedXMLDoesNotPanic(t *testing.T) {
	// Malformed XML must not crash the classifier — it must surface as a
	// single (parse error) deviation under KindUnknownDrift. Contract states
	// no error return; the report itself carries the failure signal.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("classifier panicked on malformed XML: %v", r)
		}
	}()
	report := ClassifyXMLDeviations("<not-xml<<", taskSpecMemoryDefault(), currentUserName())
	if len(report.Deviations) == 0 {
		t.Fatalf("expected at least one deviation entry for malformed XML")
	}
	if report.Deviations[0].XPath != "(parse error)" {
		t.Fatalf("expected XPath=(parse error) on malformed XML; got %+v", report.Deviations)
	}
	if !hasDeviationKind(report, KindUnknownDrift) {
		t.Fatalf("malformed XML must classify as KindUnknownDrift; got %+v", report.Deviations)
	}
}

func TestClassifyXML_NoLogonTrigger_WhenSpecHadOne(t *testing.T) {
	// spec.LogonTrigger=true but observed XML has zero triggers at all
	// (e.g. an operator deleted the trigger). Spec bucket 3 covers
	// "non-LogonTrigger triggers" through the autostart shim; the empty-
	// triggers case is also handled by the shim (no deviation at the
	// trigger level when the spec also had none, deviation when the spec
	// expected one). This test pins the latter.
	spec := taskSpecMemoryDefault()
	pinned := canonicalV04xTemplateXML(spec)
	startTok := "<Triggers>"
	endTok := "</Triggers>"
	i := strings.Index(pinned, startTok)
	j := strings.Index(pinned, endTok)
	if i < 0 || j < 0 {
		t.Fatalf("pinned XML missing Triggers block: %s", pinned)
	}
	observed := pinned[:i] + "<Triggers></Triggers>" + pinned[j+len(endTok):]
	report := ClassifyXMLDeviations(observed, spec, currentUserName())
	if report.HasUnsupportedAbort {
		t.Fatalf("missing-LogonTrigger must not abort (preserve through shim): %+v", report.Deviations)
	}
	if !hasDeviation(report, "Triggers.LogonTrigger", KindKnownPreserveShim) {
		t.Fatalf("expected KindKnownPreserveShim at Triggers.LogonTrigger; got %+v", report.Deviations)
	}
}

// --- helpers --------------------------------------------------------------

func hasDeviation(r DeviationReport, xpath string, kind DeviationKind) bool {
	for _, d := range r.Deviations {
		if d.XPath == xpath && d.Kind == kind {
			return true
		}
	}
	return false
}

func hasDeviationKind(r DeviationReport, kind DeviationKind) bool {
	for _, d := range r.Deviations {
		if d.Kind == kind {
			return true
		}
	}
	return false
}

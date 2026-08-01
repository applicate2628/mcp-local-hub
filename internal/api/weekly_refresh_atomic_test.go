package api

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/scheduler"
)

// weeklyTaskXMLForSpec supplies scheduler fakes with the same shape the
// production postcondition parser consumes.
func weeklyTaskXMLForSpec(spec scheduler.TaskSpec) []byte {
	if spec.WeeklyTrigger == nil {
		return []byte("<Task/>")
	}
	start := time.Date(2026, time.January, 4, spec.WeeklyTrigger.HourLocal, spec.WeeklyTrigger.MinuteLocal, 0, 0, time.Local)
	xmlText := func(value string) string {
		var escaped bytes.Buffer
		_ = xml.EscapeText(&escaped, []byte(value))
		return escaped.String()
	}
	days := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	day := days[spec.WeeklyTrigger.DayOfWeek]
	restart := ""
	if spec.RestartOnFailure {
		restart = "<RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure>"
	}
	return []byte(fmt.Sprintf(
		`<Task><RegistrationInfo><Description>%s</Description></RegistrationInfo><Triggers><CalendarTrigger><StartBoundary>%s</StartBoundary><Enabled>true</Enabled><ScheduleByWeek><DaysOfWeek><%s/></DaysOfWeek><WeeksInterval>1</WeeksInterval></ScheduleByWeek></CalendarTrigger></Triggers><Principals><Principal id="Author"><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals><Settings>%s<AllowHardTerminate>true</AllowHardTerminate><StartWhenAvailable>false</StartWhenAvailable><RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable><MultipleInstancesPolicy>StopExisting</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><IdleSettings><StopOnIdleEnd>false</StopOnIdleEnd><RestartOnIdle>false</RestartOnIdle></IdleSettings><AllowStartOnDemand>true</AllowStartOnDemand><Enabled>true</Enabled><Hidden>false</Hidden><RunOnlyIfIdle>false</RunOnlyIfIdle><WakeToRun>false</WakeToRun><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><Priority>7</Priority></Settings><Actions Context="Author"><Exec><Command>%s</Command><Arguments>%s</Arguments><WorkingDirectory>%s</WorkingDirectory></Exec></Actions></Task>`,
		xmlText(spec.Description),
		start.Format("2006-01-02T15:04:05"),
		day,
		restart,
		xmlText(spec.Command),
		xmlText(strings.Join(spec.Args, " ")),
		xmlText(spec.WorkingDir),
	))
}

type weeklyAtomicScheduler struct {
	mu sync.Mutex

	xml map[string][]byte

	createCalls     int
	failCreateCount int
	blockCreate     <-chan struct{}
	createEntered   chan<- struct{}
	foreignOnFail   []byte
	operationCount  int
}

func (s *weeklyAtomicScheduler) Create(spec scheduler.TaskSpec) error {
	s.mu.Lock()
	s.operationCount++
	s.createCalls++
	call := s.createCalls
	block := s.blockCreate
	entered := s.createEntered
	fail := call <= s.failCreateCount
	foreign := append([]byte(nil), s.foreignOnFail...)
	s.mu.Unlock()

	if block != nil && call == 1 {
		if entered != nil {
			entered <- struct{}{}
		}
		<-block
	}
	if fail {
		if len(foreign) > 0 {
			s.mu.Lock()
			s.xml[spec.Name] = foreign
			s.mu.Unlock()
		}
		return fmt.Errorf("injected weekly create failure #%d", call)
	}
	s.mu.Lock()
	s.xml[spec.Name] = weeklyTaskXMLForSpec(spec)
	s.mu.Unlock()
	return nil
}

func (s *weeklyAtomicScheduler) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.operationCount++
	delete(s.xml, name)
	return nil
}

func (s *weeklyAtomicScheduler) Run(string) error { return nil }

func (s *weeklyAtomicScheduler) ExportXML(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.operationCount++
	raw, ok := s.xml[name]
	if !ok {
		return nil, scheduler.ErrTaskNotFound
	}
	return append([]byte(nil), raw...), nil
}

func (s *weeklyAtomicScheduler) ImportXML(name string, raw []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.operationCount++
	s.xml[name] = append([]byte(nil), raw...)
	return nil
}

func (s *weeklyAtomicScheduler) operations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.operationCount
}

func installWeeklyAtomicHarness(t *testing.T, sch testScheduler) string {
	t.Helper()
	previousFactory := testSchedulerFactory
	previousRoot := daemonStateRootOverride
	daemonStateRootOverride = t.TempDir()
	testSchedulerFactory = func() (testScheduler, error) { return sch, nil }
	t.Cleanup(func() {
		testSchedulerFactory = previousFactory
		daemonStateRootOverride = previousRoot
	})
	stateDir, err := DaemonStateDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(stateDir, WeeklyRefreshTaskName+weeklyRefreshLockSuffix)
}

func TestEnsureWeeklyRefreshTask_SerializesFailedRestoreBeforeLaterConvergence(t *testing.T) {
	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	prior := []byte("<Task><operator-prior/></Task>")
	sch := &weeklyAtomicScheduler{
		xml:             map[string][]byte{WeeklyRefreshTaskName: prior},
		failCreateCount: 1,
		blockCreate:     block,
		createEntered:   entered,
	}
	installWeeklyAtomicHarness(t, sch)

	aDone := make(chan error, 1)
	go func() { aDone <- NewAPI().EnsureWeeklyRefreshTask() }()
	<-entered

	bStarted := make(chan struct{})
	bDone := make(chan error, 1)
	go func() {
		close(bStarted)
		bDone <- NewAPI().EnsureWeeklyRefreshTask()
	}()
	<-bStarted
	operationsWhileAHeldLock := sch.operations()
	time.Sleep(50 * time.Millisecond)
	if got := sch.operations(); got != operationsWhileAHeldLock {
		t.Fatalf("second operation reached scheduler while first held singleton lock: before=%d after=%d", operationsWhileAHeldLock, got)
	}

	close(block)
	if err := <-aDone; err == nil {
		t.Fatal("first operation unexpectedly succeeded")
	}
	if err := <-bDone; err != nil {
		t.Fatalf("later convergence: %v", err)
	}
	settled, err := sch.ExportXML(WeeklyRefreshTaskName)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := weeklyTaskXMLMatchesSpec(settled, schSpecForWeeklyTest(t))
	if err != nil || !canonical {
		t.Fatalf("final weekly task is not canonical: canonical=%t err=%v xml=%s", canonical, err, settled)
	}
}

func TestEnsureWeeklyRefreshTask_CreateFailurePreservesForeignCurrent(t *testing.T) {
	foreign := []byte("<Task><foreign-generation/></Task>")
	sch := &weeklyAtomicScheduler{
		xml:             map[string][]byte{WeeklyRefreshTaskName: []byte("<Task><prior/></Task>")},
		failCreateCount: 1,
		foreignOnFail:   foreign,
	}
	installWeeklyAtomicHarness(t, sch)

	err := NewAPI().EnsureWeeklyRefreshTask()
	if !errors.Is(err, ErrWeeklyRefreshConflict) {
		t.Fatalf("EnsureWeeklyRefreshTask error = %v, want ErrWeeklyRefreshConflict", err)
	}
	current, exportErr := sch.ExportXML(WeeklyRefreshTaskName)
	if exportErr != nil {
		t.Fatal(exportErr)
	}
	if string(current) != string(foreign) {
		t.Fatalf("foreign current task was overwritten: got=%s want=%s", current, foreign)
	}
}

func TestEnsureWeeklyRefreshTask_ReleaseFailureIsJoinedAndRecorded(t *testing.T) {
	sch := &weeklyAtomicScheduler{xml: map[string][]byte{}}
	lockPath := installWeeklyAtomicHarness(t, sch)
	unlockFailure := errors.New("injected weekly unlock failure")
	previous := flockUnlockFn
	var stranded []*flock.Flock
	flockUnlockFn = func(fl *flock.Flock) error {
		if fl.Path() == lockPath {
			stranded = append(stranded, fl)
			return unlockFailure
		}
		return previous(fl)
	}
	t.Cleanup(func() {
		flockUnlockFn = previous
		for _, fl := range stranded {
			_ = fl.Unlock()
		}
		unconfirmedLockReleasesMu.Lock()
		delete(unconfirmedLockReleases, lockPath)
		unconfirmedLockReleasesMu.Unlock()
	})

	err := NewAPI().EnsureWeeklyRefreshTask()
	if !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, unlockFailure) {
		t.Fatalf("EnsureWeeklyRefreshTask error = %v, want release class and underlying cause", err)
	}
	before := sch.operations()
	err = NewAPI().EnsureWeeklyRefreshTask()
	if !errors.Is(err, ErrLockReleaseUnconfirmed) {
		t.Fatalf("later Ensure error = %v, want fail-fast release class", err)
	}
	if after := sch.operations(); after != before {
		t.Fatalf("later Ensure reached scheduler despite ghost lock: before=%d after=%d", before, after)
	}
}

func schSpecForWeeklyTest(t *testing.T) scheduler.TaskSpec {
	t.Helper()
	canonical, err := canonicalMcphubPath()
	if err != nil {
		t.Fatal(err)
	}
	return scheduler.TaskSpec{
		Name:        WeeklyRefreshTaskName,
		Description: "mcp-local-hub: weekly refresh of workspace-scoped lazy proxies",
		Command:     canonical,
		Args:        []string{"workspace-weekly-refresh"},
		WorkingDir:  filepath.Dir(canonical),
		WeeklyTrigger: &scheduler.WeeklyTrigger{
			DayOfWeek:   0,
			HourLocal:   3,
			MinuteLocal: 0,
		},
	}
}

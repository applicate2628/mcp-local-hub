package api

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"gopkg.in/yaml.v3"

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

	createCalls           int
	failCreateCount       int
	blockCreate           <-chan struct{}
	createEntered         chan<- struct{}
	foreignOnFail         []byte
	createErrorAfterWrite bool
	operationCount        int
	operationHook         func(string)
	exportCalls           int
	failExportOnCall      map[int]error
	exportOverrideOnCall  map[int][]byte
}

func (s *weeklyAtomicScheduler) Create(spec scheduler.TaskSpec) error {
	s.recordOperation("Create")
	s.mu.Lock()
	s.createCalls++
	call := s.createCalls
	block := s.blockCreate
	entered := s.createEntered
	fail := call <= s.failCreateCount
	foreign := append([]byte(nil), s.foreignOnFail...)
	writeDesiredBeforeFailure := s.createErrorAfterWrite
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
		} else if writeDesiredBeforeFailure {
			s.mu.Lock()
			s.xml[spec.Name] = weeklyTaskXMLForSpec(spec)
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
	s.recordOperation("Delete")
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.xml, name)
	return nil
}

func (s *weeklyAtomicScheduler) Run(string) error { return nil }

func (s *weeklyAtomicScheduler) Stop(string) error { return nil }

func (s *weeklyAtomicScheduler) Status(name string) (scheduler.TaskStatus, error) {
	return scheduler.TaskStatus{Name: name}, nil
}

func (s *weeklyAtomicScheduler) List(string) ([]scheduler.TaskStatus, error) { return nil, nil }

func (s *weeklyAtomicScheduler) ExportXML(name string) ([]byte, error) {
	s.recordOperation("ExportXML")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exportCalls++
	if err := s.failExportOnCall[s.exportCalls]; err != nil {
		return nil, err
	}
	if raw := s.exportOverrideOnCall[s.exportCalls]; raw != nil {
		return append([]byte(nil), raw...), nil
	}
	raw, ok := s.xml[name]
	if !ok {
		return nil, scheduler.ErrTaskNotFound
	}
	return append([]byte(nil), raw...), nil
}

func (s *weeklyAtomicScheduler) ImportXML(name string, raw []byte) error {
	s.recordOperation("ImportXML")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.xml[name] = append([]byte(nil), raw...)
	return nil
}

func (s *weeklyAtomicScheduler) operations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.operationCount
}

func (s *weeklyAtomicScheduler) recordOperation(operation string) {
	s.mu.Lock()
	s.operationCount++
	hook := s.operationHook
	s.mu.Unlock()
	if hook != nil {
		hook(operation)
	}
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

func watchWeeklyAtomicSchedulerOperations(t *testing.T, sch *weeklyAtomicScheduler) <-chan string {
	t.Helper()
	observed := make(chan string, 1)
	sch.mu.Lock()
	sch.operationHook = func(operation string) {
		select {
		case observed <- operation:
		default:
		}
	}
	sch.mu.Unlock()
	t.Cleanup(func() {
		sch.mu.Lock()
		sch.operationHook = nil
		sch.mu.Unlock()
	})
	return observed
}

func requireNoWeeklySchedulerOperationWhileBlocked(t *testing.T, observed <-chan string) {
	t.Helper()
	select {
	case operation := <-observed:
		t.Fatalf("second writer reached scheduler operation %q before the first transaction settled", operation)
	case <-time.After(time.Second):
		// The first Create remains blocked by a test-controlled channel; this
		// timeout is only a deadlock diagnostic, not a measured race window.
	}
}

type flockUnlockFailureProbe struct {
	cause    error
	attempts int
}

func injectLedgeredFlockUnlockFailure(t *testing.T, lockPath, subject string) *flockUnlockFailureProbe {
	t.Helper()
	probe := &flockUnlockFailureProbe{cause: fmt.Errorf("injected %s unlock failure", subject)}
	previous := flockUnlockFn
	var stranded []*flock.Flock
	flockUnlockFn = func(fl *flock.Flock) error {
		if fl.Path() == lockPath {
			probe.attempts++
			stranded = append(stranded, fl)
			return probe.cause
		}
		return previous(fl)
	}
	t.Cleanup(func() {
		flockUnlockFn = previous
		for _, fl := range stranded {
			if err := fl.Unlock(); err != nil {
				t.Errorf("cleanup unlock %s: %v", lockPath, err)
			}
		}
		unconfirmedLockReleasesMu.Lock()
		delete(unconfirmedLockReleases, lockPath)
		unconfirmedLockReleasesMu.Unlock()
	})
	return probe
}

func injectWeeklyUnlockFailure(t *testing.T, lockPath string) *flockUnlockFailureProbe {
	t.Helper()
	return injectLedgeredFlockUnlockFailure(t, lockPath, "weekly")
}

func setWeeklyScheduleForAtomicTest(t *testing.T, schedule string) []byte {
	t.Helper()
	settingsRoot := t.TempDir()
	t.Setenv("LOCALAPPDATA", settingsRoot)
	t.Setenv("XDG_DATA_HOME", settingsRoot)
	if err := NewAPI().SettingsSet("daemons.weekly_schedule", schedule); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(SettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	return bytes
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
	unlockFailure := injectWeeklyUnlockFailure(t, lockPath).cause

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

func TestWeeklyRefreshWritersSerializeAtTransactionOwner(t *testing.T) {
	t.Run("settings update holds settings then weekly transaction against ensure", func(t *testing.T) {
		block := make(chan struct{})
		entered := make(chan struct{}, 1)
		sch := &weeklyAtomicScheduler{
			xml:           map[string][]byte{},
			blockCreate:   block,
			createEntered: entered,
		}
		installWeeklyAtomicHarness(t, sch)
		t.Setenv("LOCALAPPDATA", t.TempDir())
		canonical := filepath.Join(t.TempDir(), "mcphub.exe")
		previousCanonical := testCanonicalMcphubPathOverride
		testCanonicalMcphubPathOverride = canonical
		t.Cleanup(func() { testCanonicalMcphubPathOverride = previousCanonical })
		if err := NewAPI().SettingsSet("daemons.weekly_schedule", "weekly Sun 03:00"); err != nil {
			t.Fatal(err)
		}

		applyDone := make(chan error, 1)
		go func() {
			_, err := NewAPI().ApplyWeeklyRefreshSchedule(&ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 2, Hour: 14, Minute: 30})
			applyDone <- err
		}()
		<-entered
		observed := watchWeeklyAtomicSchedulerOperations(t, sch)
		ensureStarted := make(chan struct{})
		ensureDone := make(chan error, 1)
		go func() {
			close(ensureStarted)
			ensureDone <- NewAPI().EnsureWeeklyRefreshTask()
		}()
		<-ensureStarted
		requireNoWeeklySchedulerOperationWhileBlocked(t, observed)
		close(block)
		if err := <-applyDone; err != nil {
			t.Fatalf("ApplyWeeklyRefreshSchedule: %v", err)
		}
		if err := <-ensureDone; err != nil {
			t.Fatalf("EnsureWeeklyRefreshTask: %v", err)
		}
		persisted, err := NewAPI().SettingsGet("daemons.weekly_schedule")
		if err != nil || persisted != "weekly Tue 14:30" {
			t.Fatalf("final weekly setting = %q err=%v, want weekly Tue 14:30", persisted, err)
		}
		finalXML, err := sch.ExportXML(WeeklyRefreshTaskName)
		if err != nil {
			t.Fatal(err)
		}
		trigger, err := weeklyTaskTriggerFromXML(finalXML)
		if err != nil || trigger.DayOfWeek != 2 || trigger.HourLocal != 14 || trigger.MinuteLocal != 30 {
			t.Fatalf("final weekly task trigger = %+v err=%v, want Tuesday 14:30", trigger, err)
		}
	})

	t.Run("upgrade waits for schedule swap and preserves committed trigger", func(t *testing.T) {
		block := make(chan struct{})
		entered := make(chan struct{}, 1)
		canonical := filepath.Join(t.TempDir(), "mcphub.exe")
		prior := weeklyTaskXMLForSpec(weeklyRefreshTaskSpec(canonical, &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 0, Hour: 3, Minute: 0}))
		sch := &weeklyAtomicScheduler{
			xml:           map[string][]byte{WeeklyRefreshTaskName: prior},
			blockCreate:   block,
			createEntered: entered,
		}
		installWeeklyAtomicHarness(t, sch)
		previousCanonical := testCanonicalMcphubPathOverride
		testCanonicalMcphubPathOverride = canonical
		t.Cleanup(func() { testCanonicalMcphubPathOverride = previousCanonical })

		swapDone := make(chan error, 1)
		go func() {
			_, err := swapWeeklyTriggerWith(sch, &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 5, Hour: 9, Minute: 45}, prior)
			swapDone <- err
		}()
		<-entered
		observed := watchWeeklyAtomicSchedulerOperations(t, sch)
		upgradeStarted := make(chan struct{})
		upgradeDone := make(chan *SchedulerUpgradeResult, 1)
		go func() {
			close(upgradeStarted)
			upgradeDone <- upgradeWorkspaceWeeklyRefreshTask(sch, WeeklyRefreshTaskName, canonical)
		}()
		<-upgradeStarted
		requireNoWeeklySchedulerOperationWhileBlocked(t, observed)
		close(block)
		if err := <-swapDone; err != nil {
			t.Fatalf("SwapWeeklyTrigger: %v", err)
		}
		if result := <-upgradeDone; result == nil || result.Err != "" {
			t.Fatalf("upgrade result = %+v, want success", result)
		}
		finalXML, err := sch.ExportXML(WeeklyRefreshTaskName)
		if err != nil {
			t.Fatal(err)
		}
		trigger, err := weeklyTaskTriggerFromXML(finalXML)
		if err != nil || trigger.DayOfWeek != 5 || trigger.HourLocal != 9 || trigger.MinuteLocal != 45 {
			t.Fatalf("upgraded task trigger = %+v err=%v, want Friday 09:45", trigger, err)
		}
	})

	t.Run("ensure waits for upgrade transaction", func(t *testing.T) {
		block := make(chan struct{})
		entered := make(chan struct{}, 1)
		canonical := filepath.Join(t.TempDir(), "mcphub.exe")
		sch := &weeklyAtomicScheduler{
			xml:           map[string][]byte{},
			blockCreate:   block,
			createEntered: entered,
		}
		installWeeklyAtomicHarness(t, sch)

		ensureDone := make(chan error, 1)
		go func() { ensureDone <- NewAPI().EnsureWeeklyRefreshTask() }()
		<-entered
		observed := watchWeeklyAtomicSchedulerOperations(t, sch)
		upgradeStarted := make(chan struct{})
		upgradeDone := make(chan *SchedulerUpgradeResult, 1)
		go func() {
			close(upgradeStarted)
			upgradeDone <- upgradeWorkspaceWeeklyRefreshTask(sch, WeeklyRefreshTaskName, canonical)
		}()
		<-upgradeStarted
		requireNoWeeklySchedulerOperationWhileBlocked(t, observed)
		close(block)
		if err := <-ensureDone; err != nil {
			t.Fatalf("EnsureWeeklyRefreshTask: %v", err)
		}
		if result := <-upgradeDone; result == nil || result.Err != "" {
			t.Fatalf("upgrade result = %+v, want success", result)
		}
	})
}

type weeklyAPISourceSet struct {
	sourceDir string
	fileSet   *token.FileSet
	pkg       *ast.Package
	files     []*ast.File
}

func weeklyAPISourceSetForCurrentBuild(t *testing.T) weeklyAPISourceSet {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve weekly refresh test source directory")
	}
	sourceDir := filepath.Dir(file)
	context := build.Default
	context.BuildTags = append([]string(nil), build.Default.BuildTags...)
	if testEnvFallbackBuild {
		context.BuildTags = append(context.BuildTags, "test_state_path_env")
	}
	var matchErr error
	fileSet := token.NewFileSet()
	packages, err := parser.ParseDir(fileSet, sourceDir, func(info os.FileInfo) bool {
		if !strings.HasSuffix(info.Name(), ".go") || strings.HasSuffix(info.Name(), "_test.go") {
			return false
		}
		matches, err := context.MatchFile(sourceDir, info.Name())
		if err != nil {
			matchErr = err
			return false
		}
		return matches
	}, 0)
	if err != nil {
		t.Fatalf("parse non-test api source: %v", err)
	}
	if matchErr != nil {
		t.Fatalf("resolve current build constraints for non-test api source: %v", matchErr)
	}
	pkg := packages["api"]
	if pkg == nil {
		t.Fatal("non-test api package source was not parsed")
	}
	fileNames := make([]string, 0, len(pkg.Files))
	for fileName := range pkg.Files {
		fileNames = append(fileNames, fileName)
	}
	sort.Strings(fileNames)
	files := make([]*ast.File, 0, len(fileNames))
	for _, fileName := range fileNames {
		files = append(files, pkg.Files[fileName])
	}
	return weeklyAPISourceSet{sourceDir: sourceDir, fileSet: fileSet, pkg: pkg, files: files}
}

func weeklyAPISourcePackage(t *testing.T) *ast.Package {
	t.Helper()
	return weeklyAPISourceSetForCurrentBuild(t).pkg
}

func weeklyAPISourceFunctions(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	source := weeklyAPISourceSetForCurrentBuild(t)
	functions := map[string]*ast.FuncDecl{}
	for _, file := range source.files {
		for _, declaration := range file.Decls {
			if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Body != nil {
				functions[fn.Name.Name] = fn
			}
		}
	}
	return functions
}

func weeklyCallName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	default:
		return ""
	}
}

func weeklyFunctionCalls(fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && weeklyCallName(call) == name {
			found = true
		}
		return !found
	})
	return found
}

func weeklyFunctionReferences(fn *ast.FuncDecl, name string) bool {
	return weeklyNodeReferences(fn.Body, name)
}

func weeklyNodeReferences(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(candidate ast.Node) bool {
		identifier, ok := candidate.(*ast.Ident)
		if ok && identifier.Name == name {
			found = true
		}
		return !found
	})
	return found
}

func weeklyNodeCalls(node ast.Node, name string) bool {
	found := false
	ast.Inspect(node, func(candidate ast.Node) bool {
		call, ok := candidate.(*ast.CallExpr)
		if ok && weeklyCallName(call) == name {
			found = true
		}
		return !found
	})
	return found
}

func weeklySchedulerUpgradeBranch(fn *ast.FuncDecl) *ast.BlockStmt {
	var branch *ast.BlockStmt
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		statement, ok := node.(*ast.IfStmt)
		if !ok || !weeklyNodeReferences(statement.Cond, "WeeklyRefreshTaskName") {
			return true
		}
		branch = statement.Body
		return false
	})
	return branch
}

func weeklyCallbackCallsTransaction(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || weeklyCallName(call) != "withWeeklyScheduleSettings" {
			return true
		}
		for _, argument := range call.Args {
			literal, ok := argument.(*ast.FuncLit)
			if !ok {
				continue
			}
			ast.Inspect(literal.Body, func(inner ast.Node) bool {
				innerCall, ok := inner.(*ast.CallExpr)
				if ok && weeklyCallName(innerCall) == "runWeeklyRefreshTaskTransaction" {
					found = true
				}
				return !found
			})
		}
		return !found
	})
	return found
}

type weeklyGoListExportPackage struct {
	ImportPath string
	Export     string
	DepOnly    bool
}

type weeklyTypedNode struct {
	key      string
	function *types.Func
	body     *ast.BlockStmt
}

type weeklyFunctionResultOrigin struct {
	factory     *types.Func
	resultIndex int
}

type weeklyPackageCallbackOrigin struct {
	variable *types.Var
	target   *types.Func
}

type weeklyFunctionVariableDefinition struct {
	position      string
	assignment    *ast.AssignStmt
	topLevelIndex int
	leftIndex     int
	nested        bool
}

type weeklyFunctionVariableUse struct {
	position        string
	exactCallTarget bool
	nested          bool
}

type weeklyTypedInventory struct {
	fileSet    *token.FileSet
	pkg        *types.Package
	info       *types.Info
	files      []*ast.File
	nodes      map[string]*weeklyTypedNode
	byFunction map[*types.Func]*weeklyTypedNode
}

type weeklyTypedASTIndex struct {
	parent map[ast.Node]ast.Node
}

type weeklyTypedGraphOptions struct {
	schedulerInterface       types.Type
	desiredField             *types.Var
	transaction              *types.Func
	mutationType             types.Type
	approvedFunctionResults  map[weeklyFunctionResultOrigin]bool
	approvedPackageCallbacks map[*types.Var]weeklyPackageCallbackOrigin
}

type weeklyUnresolvedCall struct {
	caller     string
	position   string
	staticType string
	reason     string
	providers  []string
}

type weeklyTypedCallGraph struct {
	edges                             map[string]map[string]bool
	unresolved                        map[string][]weeklyUnresolvedCall
	approvedInterfaceDispatches       map[string][]string
	approvedFunctionResultDispatches  map[string][]weeklyFunctionResultOrigin
	approvedPackageCallbackDispatches map[string][]weeklyPackageCallbackOrigin
}

type weeklySettingsClosure struct {
	nodes map[string]bool
	next  map[string]string
}

func weeklySourceReceiverName(expression ast.Expr) string {
	switch receiver := expression.(type) {
	case *ast.Ident:
		return receiver.Name
	case *ast.StarExpr:
		return weeklySourceReceiverName(receiver.X)
	case *ast.IndexExpr:
		return weeklySourceReceiverName(receiver.X)
	case *ast.IndexListExpr:
		return weeklySourceReceiverName(receiver.X)
	default:
		return ""
	}
}

func weeklyNewTypesInfo() *types.Info {
	return &types.Info{
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Types:      map[ast.Expr]types.TypeAndValue{},
	}
}

func weeklyTypedAPIInventory(t *testing.T) weeklyTypedInventory {
	t.Helper()
	source := weeklyAPISourceSetForCurrentBuild(t)
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("resolve Go tool for weekly type inventory: %v", err)
	}
	arguments := []string{"list", "-mod=readonly", "-deps", "-export", "-json"}
	if testEnvFallbackBuild {
		arguments = append(arguments, "-tags=test_state_path_env")
	}
	arguments = append(arguments, ".")
	command := exec.Command(goTool, arguments...)
	command.Dir = source.sourceDir
	output, err := command.Output()
	if err != nil {
		t.Fatalf("load module-aware export inventory for weekly type analysis: %v", err)
	}
	exports := map[string]string{}
	decoder := json.NewDecoder(bytes.NewReader(output))
	targetImportPath := ""
	for {
		var record weeklyGoListExportPackage
		err := decoder.Decode(&record)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode module-aware export inventory for weekly type analysis: %v", err)
		}
		if record.ImportPath == "" {
			t.Fatal("module-aware export inventory contains a package without an import path")
		}
		if record.Export != "" {
			exports[record.ImportPath] = record.Export
		}
		if !record.DepOnly {
			if targetImportPath != "" && targetImportPath != record.ImportPath {
				t.Fatalf("module-aware export inventory selected multiple target packages: %s and %s", targetImportPath, record.ImportPath)
			}
			targetImportPath = record.ImportPath
		}
	}
	if targetImportPath == "" {
		t.Fatal("module-aware export inventory did not identify the API package")
	}
	lookup := func(importPath string) (io.ReadCloser, error) {
		exportPath, ok := exports[importPath]
		if !ok || exportPath == "" {
			return nil, fmt.Errorf("module-aware export inventory has no export file for %q", importPath)
		}
		reader, err := os.Open(exportPath)
		if err != nil {
			return nil, fmt.Errorf("open export data for %q: %w", importPath, err)
		}
		return reader, nil
	}
	info := weeklyNewTypesInfo()
	typeErrors := []string{}
	checker := types.Config{
		Importer: importer.ForCompiler(source.fileSet, runtime.Compiler, lookup),
		Sizes:    types.SizesFor(runtime.Compiler, runtime.GOARCH),
		Error: func(err error) {
			typeErrors = append(typeErrors, err.Error())
		},
	}
	pkg, checkErr := checker.Check(targetImportPath, source.fileSet, source.files, info)
	if checkErr != nil || len(typeErrors) != 0 {
		if len(typeErrors) == 0 {
			typeErrors = append(typeErrors, checkErr.Error())
		}
		sort.Strings(typeErrors)
		t.Fatalf("type-check build-selected non-test API source for weekly inventory: %s", strings.Join(typeErrors, "; "))
	}
	if pkg == nil {
		t.Fatal("type-check build-selected non-test API source returned no package")
	}
	return weeklyTypedInventoryFromFiles(t, source.fileSet, pkg, info, source.files)
}

func weeklyTypedInventoryFromFiles(t *testing.T, fileSet *token.FileSet, pkg *types.Package, info *types.Info, files []*ast.File) weeklyTypedInventory {
	t.Helper()
	inventory := weeklyTypedInventory{
		fileSet:    fileSet,
		pkg:        pkg,
		info:       info,
		files:      files,
		nodes:      map[string]*weeklyTypedNode{},
		byFunction: map[*types.Func]*weeklyTypedNode{},
	}
	for _, file := range files {
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			function, ok := info.Defs[fn.Name].(*types.Func)
			if !ok || function == nil {
				t.Fatalf("type inventory has no exact function object for %s", fn.Name.Name)
			}
			key := weeklyFunctionDiagnosticKey(fileSet, fn)
			if _, exists := inventory.nodes[key]; exists {
				t.Fatalf("duplicate non-test API declaration key %s", key)
			}
			node := &weeklyTypedNode{key: key, function: function, body: fn.Body}
			inventory.nodes[key] = node
			inventory.byFunction[function] = node
		}
	}
	return inventory
}

func weeklyFunctionDiagnosticKey(fileSet *token.FileSet, fn *ast.FuncDecl) string {
	key := fn.Name.Name
	position := fileSet.Position(fn.Pos())
	if fn.Name.Name == "init" {
		return fmt.Sprintf("init@%s:%d", filepath.Base(position.Filename), position.Line)
	}
	if fn.Recv != nil && len(fn.Recv.List) == 1 {
		if receiverName := weeklySourceReceiverName(fn.Recv.List[0].Type); receiverName != "" {
			key = receiverName + "." + fn.Name.Name
		}
	}
	return key
}

func weeklyRequiredPackageFunc(t *testing.T, inventory weeklyTypedInventory, name string) *types.Func {
	t.Helper()
	function, ok := inventory.pkg.Scope().Lookup(name).(*types.Func)
	if !ok || function == nil {
		t.Fatalf("type inventory is missing package function %s", name)
	}
	return function
}

func weeklyRequiredPackageVar(t *testing.T, inventory weeklyTypedInventory, name string) *types.Var {
	t.Helper()
	variable, ok := inventory.pkg.Scope().Lookup(name).(*types.Var)
	if !ok || variable == nil {
		t.Fatalf("type inventory is missing package variable %s", name)
	}
	return variable
}

func weeklyRequiredPackageType(t *testing.T, inventory weeklyTypedInventory, name string) *types.TypeName {
	t.Helper()
	typeName, ok := inventory.pkg.Scope().Lookup(name).(*types.TypeName)
	if !ok || typeName == nil {
		t.Fatalf("type inventory is missing package type %s", name)
	}
	return typeName
}

func weeklyRequiredStructField(t *testing.T, typeName *types.TypeName, fieldName string) *types.Var {
	t.Helper()
	structure, ok := typeName.Type().Underlying().(*types.Struct)
	if !ok {
		t.Fatalf("type inventory type %s is not a struct", typeName.Name())
	}
	for index := 0; index < structure.NumFields(); index++ {
		field := structure.Field(index)
		if field.Name() == fieldName {
			return field
		}
	}
	t.Fatalf("type inventory struct %s is missing field %s", typeName.Name(), fieldName)
	return nil
}

func weeklyRequiredImportedPackage(t *testing.T, inventory weeklyTypedInventory, importPath string) *types.Package {
	t.Helper()
	for _, imported := range inventory.pkg.Imports() {
		if imported.Path() == importPath {
			return imported
		}
	}
	t.Fatalf("type inventory is missing imported package %s", importPath)
	return nil
}

func weeklyProductionTypedGraphOptions(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
	t.Helper()
	mutationType := weeklyRequiredPackageType(t, inventory, "weeklyRefreshMutation")
	lockLeaf := weeklyRequiredPackageFunc(t, inventory, "lockLeafLedgered")
	if inventory.byFunction[lockLeaf] == nil {
		t.Fatal("type inventory lockLeafLedgered has no indexed local body")
	}
	lockLeafSignature, ok := lockLeaf.Type().(*types.Signature)
	if !ok || lockLeafSignature.Results().Len() == 0 {
		t.Fatal("type inventory lockLeafLedgered has no result 0")
	}
	knownFolderResolver := weeklyRequiredPackageVar(t, inventory, "knownFolderResolverFn")
	knownFolderTargetName := "stubKnownFolderUnsupported"
	if runtime.GOOS == "windows" {
		knownFolderTargetName = "realKnownFolderLocalAppData"
	}
	knownFolderTarget := weeklyRequiredPackageFunc(t, inventory, knownFolderTargetName)
	if inventory.byFunction[knownFolderTarget] == nil {
		t.Fatalf("type inventory selected %s has no indexed local body", knownFolderTargetName)
	}
	return weeklyTypedGraphOptions{
		schedulerInterface: weeklyRequiredPackageType(t, inventory, "weeklyRefreshTaskScheduler").Type(),
		desiredField:       weeklyRequiredStructField(t, mutationType, "desired"),
		transaction:        weeklyRequiredPackageFunc(t, inventory, "runWeeklyRefreshTaskTransaction"),
		mutationType:       mutationType.Type(),
		approvedFunctionResults: map[weeklyFunctionResultOrigin]bool{
			{factory: lockLeaf, resultIndex: 0}: true,
		},
		approvedPackageCallbacks: map[*types.Var]weeklyPackageCallbackOrigin{
			knownFolderResolver: {variable: knownFolderResolver, target: knownFolderTarget},
		},
	}
}

func weeklySortedFunctionKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func weeklyUnwrapCallExpression(expression ast.Expr) ast.Expr {
	for {
		switch value := expression.(type) {
		case *ast.ParenExpr:
			expression = value.X
		case *ast.IndexExpr:
			expression = value.X
		case *ast.IndexListExpr:
			expression = value.X
		default:
			return expression
		}
	}
}

func weeklyExpressionObject(inventory weeklyTypedInventory, expression ast.Expr) types.Object {
	switch value := weeklyUnwrapCallExpression(expression).(type) {
	case *ast.Ident:
		return inventory.info.Uses[value]
	case *ast.SelectorExpr:
		if selection := inventory.info.Selections[value]; selection != nil {
			return selection.Obj()
		}
		return inventory.info.Uses[value.Sel]
	default:
		return nil
	}
}

func weeklyCallTarget(inventory weeklyTypedInventory, call *ast.CallExpr) (types.Object, *types.Selection) {
	expression := weeklyUnwrapCallExpression(call.Fun)
	selector, ok := expression.(*ast.SelectorExpr)
	if ok {
		if selection := inventory.info.Selections[selector]; selection != nil {
			return selection.Obj(), selection
		}
		return inventory.info.Uses[selector.Sel], nil
	}
	identifier, ok := expression.(*ast.Ident)
	if !ok {
		return nil, nil
	}
	return inventory.info.Uses[identifier], nil
}

func weeklyNodePosition(inventory weeklyTypedInventory, position token.Pos) string {
	resolved := inventory.fileSet.Position(position)
	return fmt.Sprintf("%s:%d:%d", filepath.Base(resolved.Filename), resolved.Line, resolved.Column)
}

func weeklyCallStaticType(inventory weeklyTypedInventory, call *ast.CallExpr) string {
	typ := inventory.info.TypeOf(call.Fun)
	if typ == nil {
		return "<missing type>"
	}
	return types.TypeString(typ, func(pkg *types.Package) string { return pkg.Path() })
}

func (inventory *weeklyTypedInventory) addSyntheticNode(prefix string, literal *ast.FuncLit) (*weeklyTypedNode, error) {
	if literal == nil || literal.Body == nil {
		return nil, errors.New("synthetic function literal has no body")
	}
	position := inventory.fileSet.Position(literal.Pos())
	key := fmt.Sprintf("%s@%s:%d:%d", prefix, filepath.Base(position.Filename), position.Line, position.Column)
	if existing := inventory.nodes[key]; existing != nil {
		if existing.body != literal.Body {
			return nil, fmt.Errorf("synthetic function key collision at %s", key)
		}
		return existing, nil
	}
	node := &weeklyTypedNode{key: key, body: literal.Body}
	inventory.nodes[key] = node
	return node, nil
}

func weeklyForEachDirectCall(node *weeklyTypedNode, visit func(*ast.CallExpr)) {
	ast.Inspect(node.body, func(candidate ast.Node) bool {
		if _, nested := candidate.(*ast.FuncLit); nested {
			return false
		}
		if call, ok := candidate.(*ast.CallExpr); ok {
			visit(call)
		}
		return true
	})
}

func weeklyDesiredMutationArgumentIndex(options weeklyTypedGraphOptions) (int, error) {
	if options.transaction == nil || options.mutationType == nil {
		return 0, errors.New("desired callback resolver is missing its transaction function or mutation type")
	}
	signature, ok := options.transaction.Type().(*types.Signature)
	if !ok {
		return 0, fmt.Errorf("transaction %s does not have a function signature", options.transaction.Name())
	}
	index := -1
	for parameter := 0; parameter < signature.Params().Len(); parameter++ {
		if types.Identical(signature.Params().At(parameter).Type(), options.mutationType) {
			if index != -1 {
				return 0, fmt.Errorf("transaction %s has multiple mutation parameters", options.transaction.Name())
			}
			index = parameter
		}
	}
	if index == -1 {
		return 0, fmt.Errorf("transaction %s has no exact mutation parameter", options.transaction.Name())
	}
	return index, nil
}

func weeklyDesiredProviderForTransactionCall(inventory *weeklyTypedInventory, options weeklyTypedGraphOptions, call *ast.CallExpr) (*weeklyTypedNode, error) {
	argumentIndex, err := weeklyDesiredMutationArgumentIndex(options)
	if err != nil {
		return nil, err
	}
	if argumentIndex >= len(call.Args) {
		return nil, fmt.Errorf("transaction call at %s is missing its mutation argument", weeklyNodePosition(*inventory, call.Pos()))
	}
	argument := weeklyUnwrapCallExpression(call.Args[argumentIndex])
	if !types.Identical(inventory.info.TypeOf(argument), options.mutationType) {
		return nil, fmt.Errorf("transaction mutation at %s has type %s, want exact weekly refresh mutation", weeklyNodePosition(*inventory, call.Pos()), types.TypeString(inventory.info.TypeOf(argument), func(pkg *types.Package) string { return pkg.Path() }))
	}
	literal, ok := argument.(*ast.CompositeLit)
	if !ok {
		return nil, fmt.Errorf("transaction mutation at %s is not an exact composite literal", weeklyNodePosition(*inventory, call.Pos()))
	}
	var provider *weeklyTypedNode
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := weeklyUnwrapCallExpression(field.Key).(*ast.Ident)
		if !ok || inventory.info.Uses[key] != options.desiredField {
			continue
		}
		if provider != nil {
			return nil, fmt.Errorf("transaction mutation at %s defines desired more than once", weeklyNodePosition(*inventory, call.Pos()))
		}
		switch value := weeklyUnwrapCallExpression(field.Value).(type) {
		case *ast.FuncLit:
			node, err := inventory.addSyntheticNode("desired", value)
			if err != nil {
				return nil, err
			}
			provider = node
		default:
			function, ok := weeklyExpressionObject(*inventory, value).(*types.Func)
			if !ok {
				return nil, fmt.Errorf("transaction desired provider at %s is not a function literal or exact local function", weeklyNodePosition(*inventory, field.Value.Pos()))
			}
			node := inventory.byFunction[function]
			if node == nil {
				return nil, fmt.Errorf("transaction desired provider at %s has no indexed local function body", weeklyNodePosition(*inventory, field.Value.Pos()))
			}
			provider = node
		}
	}
	if provider == nil {
		return nil, fmt.Errorf("transaction mutation at %s has no exact desired provider", weeklyNodePosition(*inventory, call.Pos()))
	}
	return provider, nil
}

func weeklyDesiredProviders(inventory *weeklyTypedInventory, options weeklyTypedGraphOptions) ([]*weeklyTypedNode, error) {
	if options.desiredField == nil {
		return nil, nil
	}
	providers := map[string]*weeklyTypedNode{}
	problems := []string{}
	for _, file := range inventory.files {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			target, _ := weeklyCallTarget(*inventory, call)
			if target != options.transaction {
				return true
			}
			provider, err := weeklyDesiredProviderForTransactionCall(inventory, options, call)
			if err != nil {
				problems = append(problems, err.Error())
				return true
			}
			providers[provider.key] = provider
			return true
		})
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return nil, errors.New(strings.Join(problems, "; "))
	}
	if len(providers) == 0 {
		return nil, errors.New("no production weekly transaction call exposes an exact desired provider")
	}
	keys := make(map[string]bool, len(providers))
	for key := range providers {
		keys[key] = true
	}
	ordered := make([]*weeklyTypedNode, 0, len(keys))
	for _, key := range weeklySortedFunctionKeys(keys) {
		ordered = append(ordered, providers[key])
	}
	return ordered, nil
}

func weeklyRecordUnresolved(graph *weeklyTypedCallGraph, inventory weeklyTypedInventory, node *weeklyTypedNode, call *ast.CallExpr, reason string, providers ...string) {
	orderedProviders := append([]string(nil), providers...)
	sort.Strings(orderedProviders)
	graph.unresolved[node.key] = append(graph.unresolved[node.key], weeklyUnresolvedCall{
		caller:     node.key,
		position:   weeklyNodePosition(inventory, call.Pos()),
		staticType: weeklyCallStaticType(inventory, call),
		reason:     reason,
		providers:  orderedProviders,
	})
}

func weeklyApprovedSchedulerDispatch(selection *types.Selection, schedulerInterface types.Type) bool {
	return selection != nil && schedulerInterface != nil && types.Identical(selection.Recv(), schedulerInterface)
}

func weeklySortedPositions(values []string) string {
	ordered := append([]string(nil), values...)
	sort.Strings(ordered)
	return strings.Join(ordered, ",")
}

func weeklyBuildTypedASTIndex(files []*ast.File) weeklyTypedASTIndex {
	index := weeklyTypedASTIndex{parent: map[ast.Node]ast.Node{}}
	for _, file := range files {
		stack := []ast.Node{}
		ast.Inspect(file, func(node ast.Node) bool {
			if node == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			if len(stack) != 0 {
				index.parent[node] = stack[len(stack)-1]
			}
			stack = append(stack, node)
			return true
		})
	}
	return index
}

func weeklyEnclosingNamedFunction(inventory weeklyTypedInventory, index weeklyTypedASTIndex, start ast.Node) (*weeklyTypedNode, *ast.FuncDecl, bool) {
	nested := false
	for current := start; current != nil; current = index.parent[current] {
		switch value := current.(type) {
		case *ast.FuncLit:
			nested = true
		case *ast.FuncDecl:
			function, _ := inventory.info.Defs[value.Name].(*types.Func)
			return inventory.byFunction[function], value, nested
		}
	}
	return nil, nil, nested
}

func weeklyTopLevelStatementFromIndex(index weeklyTypedASTIndex, body *ast.BlockStmt, start ast.Node) (ast.Stmt, int) {
	if body == nil || start == nil {
		return nil, -1
	}
	current := start
	for current != nil && index.parent[current] != body {
		current = index.parent[current]
	}
	statement, ok := current.(ast.Stmt)
	if !ok {
		return nil, -1
	}
	for position, candidate := range body.List {
		if candidate == statement {
			return statement, position
		}
	}
	return nil, -1
}

func weeklyUnwrapParentheses(expression ast.Expr) ast.Expr {
	for {
		parenthesized, ok := expression.(*ast.ParenExpr)
		if !ok {
			return expression
		}
		expression = parenthesized.X
	}
}

func weeklyExactNilComparison(inventory weeklyTypedInventory, expression ast.Expr, variable *types.Var, operator token.Token) bool {
	binary, ok := weeklyUnwrapParentheses(expression).(*ast.BinaryExpr)
	if !ok || binary.Op != operator {
		return false
	}
	isVariable := func(candidate ast.Expr) bool {
		identifier, ok := weeklyUnwrapParentheses(candidate).(*ast.Ident)
		return ok && inventory.info.Uses[identifier] == variable
	}
	isNil := func(candidate ast.Expr) bool {
		identifier, ok := weeklyUnwrapParentheses(candidate).(*ast.Ident)
		return ok && identifier.Name == "nil" && inventory.info.Uses[identifier] == types.Universe.Lookup("nil")
	}
	return (isVariable(binary.X) && isNil(binary.Y)) || (isNil(binary.X) && isVariable(binary.Y))
}

func weeklyNamedFunctionControlFlowPositions(inventory weeklyTypedInventory, node *weeklyTypedNode) []string {
	positions := []string{}
	weeklyVisitCurrentFunctionBody(node.body, false, func(candidate ast.Node, nested bool) {
		if nested {
			return
		}
		switch value := candidate.(type) {
		case *ast.LabeledStmt:
			positions = append(positions, weeklyNodePosition(inventory, value.Pos()))
		case *ast.BranchStmt:
			if value.Tok == token.GOTO {
				positions = append(positions, weeklyNodePosition(inventory, value.Pos()))
			}
		}
	})
	sort.Strings(positions)
	return positions
}

func weeklyPackageCallbackCallHasExactGuard(inventory weeklyTypedInventory, index weeklyTypedASTIndex, node *weeklyTypedNode, call *ast.CallExpr, variable *types.Var) (bool, string) {
	if controlFlow := weeklyNamedFunctionControlFlowPositions(inventory, node); len(controlFlow) != 0 {
		return false, "named function has label/goto at " + strings.Join(controlFlow, ",")
	}
	for current := ast.Node(call); current != nil && current != node.body; current = index.parent[current] {
		body, ok := current.(*ast.BlockStmt)
		if !ok {
			continue
		}
		guard, ok := index.parent[body].(*ast.IfStmt)
		if ok && guard.Body == body && guard.Init == nil && weeklyExactNilComparison(inventory, guard.Cond, variable, token.NEQ) {
			return true, ""
		}
	}
	_, callIndex := weeklyTopLevelStatementFromIndex(index, node.body, call)
	if callIndex < 0 {
		return false, "call is not contained by a named function top-level statement"
	}
	for _, statement := range node.body.List[:callIndex] {
		guard, ok := statement.(*ast.IfStmt)
		if !ok || guard.Init != nil || guard.Else != nil || !weeklyExactNilComparison(inventory, guard.Cond, variable, token.EQL) {
			continue
		}
		if len(guard.Body.List) == 1 {
			if _, ok := guard.Body.List[0].(*ast.ReturnStmt); ok {
				return true, ""
			}
		}
	}
	return false, "call has no exact enclosing non-nil guard or earlier nil-return guard"
}

func weeklyPackageCallbackObjectDiagnostic(inventory weeklyTypedInventory, object types.Object) string {
	if object == nil {
		return "<nil>"
	}
	switch value := object.(type) {
	case *types.Var:
		if value == nil {
			return "<nil>"
		}
	case *types.Func:
		if value == nil {
			return "<nil>"
		}
	}
	position := inventory.fileSet.Position(object.Pos())
	return fmt.Sprintf("%s@%s:%d", object.Name(), filepath.Base(position.Filename), position.Line)
}

func weeklyValidateApprovedPackageCallbacks(inventory *weeklyTypedInventory, options weeklyTypedGraphOptions, index weeklyTypedASTIndex) (map[*types.Var]weeklyPackageCallbackOrigin, error) {
	type policyEntry struct {
		key    *types.Var
		origin weeklyPackageCallbackOrigin
	}
	entries := make([]policyEntry, 0, len(options.approvedPackageCallbacks))
	for key, origin := range options.approvedPackageCallbacks {
		entries = append(entries, policyEntry{key: key, origin: origin})
	}
	sort.Slice(entries, func(left, right int) bool {
		return weeklyPackageCallbackObjectDiagnostic(*inventory, entries[left].key) < weeklyPackageCallbackObjectDiagnostic(*inventory, entries[right].key)
	})

	validated := map[*types.Var]weeklyPackageCallbackOrigin{}
	problems := []string{}
	for _, entry := range entries {
		origin := entry.origin
		variable := origin.variable
		target := origin.target
		failures := []string{}
		if entry.key == nil || variable == nil || entry.key != variable {
			failures = append(failures, "policy key and exact variable object differ or are nil")
		}
		if target == nil {
			failures = append(failures, "policy exact target object is nil")
		}
		if variable == nil || target == nil {
			sort.Strings(failures)
			problems = append(problems, strings.Join(failures, "; "))
			continue
		}
		if variable.Pkg() != inventory.pkg || variable.Parent() != inventory.pkg.Scope() {
			failures = append(failures, "policy variable is not the exact package-scope object")
		}
		targetSignature, targetIsFunction := target.Type().(*types.Signature)
		if target.Pkg() != inventory.pkg || !targetIsFunction || targetSignature.Recv() != nil || inventory.byFunction[target] == nil {
			failures = append(failures, "policy target is not an exact receiver-free indexed package function")
		} else if !types.Identical(variable.Type(), target.Type()) {
			failures = append(failures, "policy variable and exact target signatures differ")
		}

		declarations := []string{}
		for identifier, object := range inventory.info.Defs {
			if object != variable {
				continue
			}
			declarations = append(declarations, weeklyNodePosition(*inventory, identifier.Pos()))
			specification, ok := index.parent[identifier].(*ast.ValueSpec)
			declaration, declarationOK := index.parent[specification].(*ast.GenDecl)
			_, topLevel := index.parent[declaration].(*ast.File)
			if !ok || !declarationOK || declaration.Tok != token.VAR || !topLevel {
				failures = append(failures, "callback declaration is not one top-level package var")
				continue
			}
			if len(specification.Values) != 0 {
				failures = append(failures, "callback package declaration has an initializer")
			}
		}
		sort.Strings(declarations)
		if len(declarations) != 1 {
			failures = append(failures, fmt.Sprintf("callback must have exactly one package declaration, got %d at %s", len(declarations), strings.Join(declarations, ",")))
		}

		writes := []*ast.AssignStmt{}
		writePositions := []string{}
		calls := []struct {
			node *weeklyTypedNode
			call *ast.CallExpr
		}{}
		unsupportedUses := []string{}
		for identifier, object := range inventory.info.Uses {
			if object != variable {
				continue
			}
			node, _, nested := weeklyEnclosingNamedFunction(*inventory, index, identifier)
			position := weeklyNodePosition(*inventory, identifier.Pos())
			if nested {
				unsupportedUses = append(unsupportedUses, position+":nested-function-literal")
				continue
			}
			if assignment, ok := index.parent[identifier].(*ast.AssignStmt); ok {
				isLeft := false
				for _, left := range assignment.Lhs {
					if left == identifier {
						isLeft = true
						break
					}
				}
				if isLeft {
					writes = append(writes, assignment)
					writePositions = append(writePositions, position)
					continue
				}
			}
			current := ast.Node(identifier)
			for {
				parenthesized, ok := index.parent[current].(*ast.ParenExpr)
				if !ok || parenthesized.X != current {
					break
				}
				current = parenthesized
			}
			if binary, ok := index.parent[current].(*ast.BinaryExpr); ok &&
				(weeklyExactNilComparison(*inventory, binary, variable, token.EQL) || weeklyExactNilComparison(*inventory, binary, variable, token.NEQ)) && node != nil {
				continue
			}
			if call, ok := index.parent[current].(*ast.CallExpr); ok && weeklyUnwrapParentheses(call.Fun) == identifier && node != nil {
				switch index.parent[call].(type) {
				case *ast.GoStmt:
					unsupportedUses = append(unsupportedUses, position+":go-dispatch")
				case *ast.DeferStmt:
					unsupportedUses = append(unsupportedUses, position+":defer-dispatch")
				default:
					calls = append(calls, struct {
						node *weeklyTypedNode
						call *ast.CallExpr
					}{node: node, call: call})
				}
				continue
			}
			unsupportedUses = append(unsupportedUses, position+":unsupported-use")
		}
		sort.Strings(writePositions)
		if len(writes) != 1 {
			failures = append(failures, fmt.Sprintf("callback must have exactly one selected non-test init write, got %d at %s", len(writes), strings.Join(writePositions, ",")))
		} else {
			assignment := writes[0]
			body, bodyOK := index.parent[assignment].(*ast.BlockStmt)
			declaration, declarationOK := index.parent[body].(*ast.FuncDecl)
			if assignment.Tok != token.ASSIGN || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 ||
				!bodyOK || !declarationOK || declaration.Name.Name != "init" || declaration.Recv != nil || declaration.Body != body {
				failures = append(failures, "callback write is not one direct top-level package init assignment")
			} else {
				left, leftOK := assignment.Lhs[0].(*ast.Ident)
				if !leftOK || inventory.info.Uses[left] != variable {
					failures = append(failures, "callback init left side is not the exact policy variable")
				}
				observedTarget, observedOK := weeklyExpressionObject(*inventory, assignment.Rhs[0]).(*types.Func)
				if !observedOK || observedTarget != target {
					observedDiagnostic := "<non-function>"
					if observedOK && observedTarget != nil {
						observedDiagnostic = weeklyPackageCallbackObjectDiagnostic(*inventory, observedTarget)
					}
					failures = append(failures, fmt.Sprintf("callback initializer target is %s, want exact %s", observedDiagnostic, weeklyPackageCallbackObjectDiagnostic(*inventory, target)))
				}
				initFunction, _ := inventory.info.Defs[declaration.Name].(*types.Func)
				if initNode := inventory.byFunction[initFunction]; initNode == nil {
					failures = append(failures, "callback init has no indexed local body")
				} else if controlFlow := weeklyNamedFunctionControlFlowPositions(*inventory, initNode); len(controlFlow) != 0 {
					failures = append(failures, "callback init contains label/goto at "+strings.Join(controlFlow, ","))
				}
			}
		}
		if len(unsupportedUses) != 0 {
			sort.Strings(unsupportedUses)
			failures = append(failures, "callback has unsupported selected non-test uses at "+strings.Join(unsupportedUses, ","))
		}
		for _, invocation := range calls {
			if ok, reason := weeklyPackageCallbackCallHasExactGuard(*inventory, index, invocation.node, invocation.call, variable); !ok {
				failures = append(failures, fmt.Sprintf("callback call at %s fails exact nil guard: %s", weeklyNodePosition(*inventory, invocation.call.Pos()), reason))
			}
		}
		if len(failures) != 0 {
			sort.Strings(failures)
			problems = append(problems, fmt.Sprintf("package callback %s: %s", weeklyPackageCallbackObjectDiagnostic(*inventory, variable), strings.Join(failures, "; ")))
			continue
		}
		validated[variable] = origin
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return validated, nil
}

func weeklyTopLevelStatementIndexContaining(body *ast.BlockStmt, target ast.Node) int {
	if body == nil || target == nil {
		return -1
	}
	for index, statement := range body.List {
		found := false
		ast.Inspect(statement, func(candidate ast.Node) bool {
			if candidate == nil {
				return true
			}
			if _, nested := candidate.(*ast.FuncLit); nested {
				return false
			}
			if candidate == target {
				found = true
				return false
			}
			return !found
		})
		if found {
			return index
		}
	}
	return -1
}

func weeklyVisitCurrentFunctionBody(body *ast.BlockStmt, nested bool, visit func(ast.Node, bool)) {
	if body == nil {
		return
	}
	ast.Inspect(body, func(candidate ast.Node) bool {
		if candidate == nil {
			return true
		}
		if literal, ok := candidate.(*ast.FuncLit); ok {
			weeklyVisitCurrentFunctionBody(literal.Body, true, visit)
			return false
		}
		visit(candidate, nested)
		return true
	})
}

func weeklyResolveApprovedFunctionResultOrigin(inventory *weeklyTypedInventory, options weeklyTypedGraphOptions, node *weeklyTypedNode, call *ast.CallExpr, variable *types.Var) (*weeklyTypedNode, weeklyFunctionResultOrigin, error) {
	if node == nil || node.body == nil || call == nil || variable == nil {
		return nil, weeklyFunctionResultOrigin{}, errors.New("function-variable origin resolver is missing its node, body, call, or variable")
	}
	targetIdentifier, ok := weeklyUnwrapCallExpression(call.Fun).(*ast.Ident)
	if !ok || inventory.info.Uses[targetIdentifier] != variable {
		return nil, weeklyFunctionResultOrigin{}, errors.New("function-variable target is not an exact local identifier use")
	}

	topLevelAssignments := map[*ast.AssignStmt]int{}
	for index, statement := range node.body.List {
		if assignment, ok := statement.(*ast.AssignStmt); ok {
			topLevelAssignments[assignment] = index
		}
	}
	assignmentLHS := map[*ast.Ident]bool{}
	weeklyVisitCurrentFunctionBody(node.body, false, func(candidate ast.Node, _ bool) {
		assignment, ok := candidate.(*ast.AssignStmt)
		if !ok {
			return
		}
		for _, left := range assignment.Lhs {
			identifier, ok := weeklyUnwrapCallExpression(left).(*ast.Ident)
			if !ok {
				continue
			}
			if inventory.info.Defs[identifier] == variable || inventory.info.Uses[identifier] == variable {
				assignmentLHS[identifier] = true
			}
		}
	})

	definitions := []weeklyFunctionVariableDefinition{}
	writes := []string{}
	uses := []weeklyFunctionVariableUse{}
	labelsOrGotos := []string{}
	weeklyVisitCurrentFunctionBody(node.body, false, func(candidate ast.Node, nested bool) {
		switch value := candidate.(type) {
		case *ast.AssignStmt:
			for leftIndex, left := range value.Lhs {
				identifier, ok := weeklyUnwrapCallExpression(left).(*ast.Ident)
				if !ok {
					continue
				}
				position := weeklyNodePosition(*inventory, identifier.Pos())
				topLevelIndex, topLevel := topLevelAssignments[value]
				if !topLevel {
					topLevelIndex = -1
				}
				switch {
				case inventory.info.Defs[identifier] == variable:
					definitions = append(definitions, weeklyFunctionVariableDefinition{
						position:      position,
						assignment:    value,
						topLevelIndex: topLevelIndex,
						leftIndex:     leftIndex,
						nested:        nested,
					})
				case inventory.info.Uses[identifier] == variable:
					writes = append(writes, position)
				}
			}
		case *ast.Ident:
			if assignmentLHS[value] {
				return
			}
			position := weeklyNodePosition(*inventory, value.Pos())
			if inventory.info.Defs[value] == variable {
				definitions = append(definitions, weeklyFunctionVariableDefinition{
					position:      position,
					topLevelIndex: -1,
					nested:        nested,
				})
			}
			if inventory.info.Uses[value] == variable {
				uses = append(uses, weeklyFunctionVariableUse{
					position:        position,
					exactCallTarget: value == targetIdentifier,
					nested:          nested,
				})
			}
		case *ast.LabeledStmt:
			if !nested {
				labelsOrGotos = append(labelsOrGotos, weeklyNodePosition(*inventory, value.Pos()))
			}
		case *ast.BranchStmt:
			if !nested && value.Tok == token.GOTO {
				labelsOrGotos = append(labelsOrGotos, weeklyNodePosition(*inventory, value.Pos()))
			}
		}
	})

	failures := []string{}
	definitionPositions := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		definitionPositions = append(definitionPositions, definition.position)
	}
	if len(definitions) != 1 {
		failures = append(failures, fmt.Sprintf("function variable must have exactly one local definition, got %d at %s", len(definitions), weeklySortedPositions(definitionPositions)))
	}
	if len(writes) != 0 {
		failures = append(failures, fmt.Sprintf("function variable has writes at %s", weeklySortedPositions(writes)))
	}
	usePositions := make([]string, 0, len(uses))
	for _, use := range uses {
		usePositions = append(usePositions, use.position)
	}
	if len(uses) != 1 || uses[0].nested || !uses[0].exactCallTarget {
		failures = append(failures, fmt.Sprintf("function variable must have one direct non-captured call use, got %d at %s", len(uses), weeklySortedPositions(usePositions)))
	}
	if len(labelsOrGotos) != 0 {
		failures = append(failures, fmt.Sprintf("function contains labels or goto at %s", weeklySortedPositions(labelsOrGotos)))
	}

	var factoryNode *weeklyTypedNode
	origin := weeklyFunctionResultOrigin{}
	if len(definitions) == 1 {
		definition := definitions[0]
		if definition.nested || definition.assignment == nil || definition.assignment.Tok != token.DEFINE || definition.topLevelIndex < 0 {
			failures = append(failures, "function variable definition is not one top-level := assignment")
		} else if len(definition.assignment.Rhs) != 1 {
			failures = append(failures, "function variable definition does not have one factory call expression")
		} else if factoryCall, ok := weeklyUnwrapCallExpression(definition.assignment.Rhs[0]).(*ast.CallExpr); !ok {
			failures = append(failures, "function variable definition right-hand side is not an exact factory call")
		} else if factory, ok := func() (*types.Func, bool) {
			target, _ := weeklyCallTarget(*inventory, factoryCall)
			function, ok := target.(*types.Func)
			return function, ok
		}(); !ok {
			failures = append(failures, "function variable definition factory is not an exact local function")
		} else if factoryNode = inventory.byFunction[factory]; factoryNode == nil {
			failures = append(failures, "function variable definition factory has no indexed local body")
		} else if signature, ok := factory.Type().(*types.Signature); !ok {
			failures = append(failures, "function variable definition factory has no function signature")
		} else if len(definition.assignment.Lhs) != signature.Results().Len() || definition.leftIndex >= signature.Results().Len() {
			failures = append(failures, "function variable definition does not match the factory result arity")
		} else if !types.Identical(signature.Results().At(definition.leftIndex).Type(), variable.Type()) {
			failures = append(failures, "function variable definition does not match the factory result type")
		} else {
			origin = weeklyFunctionResultOrigin{factory: factory, resultIndex: definition.leftIndex}
			if !options.approvedFunctionResults[origin] {
				failures = append(failures, "function variable definition factory result is not allowlisted")
			}
		}
		invocationIndex := weeklyTopLevelStatementIndexContaining(node.body, call)
		if invocationIndex == -1 || definition.topLevelIndex >= invocationIndex {
			failures = append(failures, "function variable definition does not dominate its call use")
		}
	}
	if len(failures) != 0 {
		sort.Strings(failures)
		return nil, weeklyFunctionResultOrigin{}, errors.New(strings.Join(failures, "; "))
	}
	return factoryNode, origin, nil
}

func weeklyBuildTypedCallGraph(inventory *weeklyTypedInventory, options weeklyTypedGraphOptions) (weeklyTypedCallGraph, error) {
	index := weeklyBuildTypedASTIndex(inventory.files)
	packageCallbacks, err := weeklyValidateApprovedPackageCallbacks(inventory, options, index)
	if err != nil {
		return weeklyTypedCallGraph{}, err
	}
	providers, err := weeklyDesiredProviders(inventory, options)
	if err != nil {
		return weeklyTypedCallGraph{}, err
	}
	graph := weeklyTypedCallGraph{
		edges:                             map[string]map[string]bool{},
		unresolved:                        map[string][]weeklyUnresolvedCall{},
		approvedInterfaceDispatches:       map[string][]string{},
		approvedFunctionResultDispatches:  map[string][]weeklyFunctionResultOrigin{},
		approvedPackageCallbackDispatches: map[string][]weeklyPackageCallbackOrigin{},
	}
	processed := map[string]bool{}
	for {
		processedAny := false
		allNodes := map[string]bool{}
		for key := range inventory.nodes {
			allNodes[key] = true
		}
		for _, key := range weeklySortedFunctionKeys(allNodes) {
			if processed[key] {
				continue
			}
			processed[key] = true
			processedAny = true
			node := inventory.nodes[key]
			graph.edges[key] = map[string]bool{}
			weeklyForEachDirectCall(node, func(call *ast.CallExpr) {
				if literal, ok := weeklyUnwrapCallExpression(call.Fun).(*ast.FuncLit); ok {
					child, literalErr := inventory.addSyntheticNode("literal", literal)
					if literalErr != nil {
						weeklyRecordUnresolved(&graph, *inventory, node, call, literalErr.Error())
						return
					}
					graph.edges[key][child.key] = true
					return
				}
				target, selection := weeklyCallTarget(*inventory, call)
				switch value := target.(type) {
				case *types.Func:
					if child := inventory.byFunction[value]; child != nil {
						graph.edges[key][child.key] = true
						return
					}
					if weeklyApprovedSchedulerDispatch(selection, options.schedulerInterface) {
						graph.approvedInterfaceDispatches[key] = append(graph.approvedInterfaceDispatches[key], weeklyNodePosition(*inventory, call.Pos()))
						return
					}
					if value.Pkg() == inventory.pkg {
						weeklyRecordUnresolved(&graph, *inventory, node, call, "local function or method has no indexed body")
					}
				case *types.Var:
					if value == options.desiredField {
						for _, provider := range providers {
							graph.edges[key][provider.key] = true
						}
						return
					}
					if origin, approved := packageCallbacks[value]; approved {
						target := inventory.byFunction[origin.target]
						if target == nil {
							weeklyRecordUnresolved(&graph, *inventory, node, call, "approved package callback target has no indexed local body")
							return
						}
						graph.edges[key][target.key] = true
						graph.approvedPackageCallbackDispatches[key] = append(graph.approvedPackageCallbackDispatches[key], origin)
						return
					}
					if value.Pkg() == inventory.pkg && value.Parent() == inventory.pkg.Scope() {
						weeklyRecordUnresolved(&graph, *inventory, node, call, "package function variable has no exact selected-platform origin policy")
						return
					}
					factory, origin, originErr := weeklyResolveApprovedFunctionResultOrigin(inventory, options, node, call, value)
					if originErr != nil {
						weeklyRecordUnresolved(&graph, *inventory, node, call, originErr.Error())
						return
					}
					graph.edges[key][factory.key] = true
					graph.approvedFunctionResultDispatches[key] = append(graph.approvedFunctionResultDispatches[key], origin)
				case nil:
					if inventory.info.Types[call.Fun].IsType() {
						return
					}
					weeklyRecordUnresolved(&graph, *inventory, node, call, "missing typed call target")
				}
			})
		}
		if !processedAny {
			break
		}
	}
	return graph, nil
}

func weeklyUnresolvedBoundaryError(calls []weeklyUnresolvedCall) error {
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		detail := fmt.Sprintf("unresolved typed boundary from %s at %s (%s: %s)", call.caller, call.position, call.staticType, call.reason)
		if len(call.providers) != 0 {
			detail += " providers=" + strings.Join(call.providers, ",")
		}
		parts = append(parts, detail)
	}
	sort.Strings(parts)
	return errors.New(strings.Join(parts, "; "))
}

func weeklyReachableTypedNodes(inventory weeklyTypedInventory, graph weeklyTypedCallGraph, root *types.Func) (map[string]bool, error) {
	rootNode := inventory.byFunction[root]
	if rootNode == nil {
		return nil, fmt.Errorf("weekly transaction root %s has no indexed local body", root.Name())
	}
	reachable := map[string]bool{rootNode.key: true}
	for changed := true; changed; {
		changed = false
		for _, key := range weeklySortedFunctionKeys(reachable) {
			if unresolved := graph.unresolved[key]; len(unresolved) != 0 {
				return nil, weeklyUnresolvedBoundaryError(unresolved)
			}
			for _, child := range weeklySortedFunctionKeys(graph.edges[key]) {
				if !reachable[child] {
					reachable[child] = true
					changed = true
				}
			}
		}
	}
	return reachable, nil
}

func weeklyDirectSettingsMutexAcquisition(inventory weeklyTypedInventory, node *weeklyTypedNode, settingsMu *types.Var) bool {
	found := false
	weeklyForEachDirectCall(node, func(call *ast.CallExpr) {
		selector, ok := weeklyUnwrapCallExpression(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return
		}
		target, _ := weeklyCallTarget(inventory, call)
		method, ok := target.(*types.Func)
		if !ok || (method.Name() != "Lock" && method.Name() != "TryLock") {
			return
		}
		if weeklyExpressionObject(inventory, selector.X) == settingsMu {
			found = true
		}
	})
	return found
}

func weeklyDirectFlockNewCount(inventory weeklyTypedInventory, node *weeklyTypedNode, flockNew *types.Func) int {
	count := 0
	weeklyForEachDirectCall(node, func(call *ast.CallExpr) {
		target, _ := weeklyCallTarget(inventory, call)
		if target == flockNew {
			count++
		}
	})
	return count
}

func weeklyDirectFlockLockCount(inventory weeklyTypedInventory, node *weeklyTypedNode, flockLock *types.Func) int {
	count := 0
	weeklyForEachDirectCall(node, func(call *ast.CallExpr) {
		target, _ := weeklyCallTarget(inventory, call)
		if target == flockLock {
			count++
		}
	})
	return count
}

func weeklyDirectSettingsWrite(inventory weeklyTypedInventory, node *weeklyTypedNode, write *types.Func) bool {
	found := false
	weeklyForEachDirectCall(node, func(call *ast.CallExpr) {
		target, _ := weeklyCallTarget(inventory, call)
		if target == write {
			found = true
		}
	})
	return found
}

func weeklySettingsHelperClosure(inventory weeklyTypedInventory, graph weeklyTypedCallGraph, settingsMu *types.Var) weeklySettingsClosure {
	closure := weeklySettingsClosure{nodes: map[string]bool{}, next: map[string]string{}}
	allNodes := map[string]bool{}
	for key := range inventory.nodes {
		allNodes[key] = true
	}
	for _, key := range weeklySortedFunctionKeys(allNodes) {
		if weeklyDirectSettingsMutexAcquisition(inventory, inventory.nodes[key], settingsMu) {
			closure.nodes[key] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, caller := range weeklySortedFunctionKeys(allNodes) {
			for _, callee := range weeklySortedFunctionKeys(graph.edges[caller]) {
				if closure.nodes[callee] && !closure.nodes[caller] {
					closure.nodes[caller] = true
					closure.next[caller] = callee
					changed = true
				}
			}
			for _, unresolved := range graph.unresolved[caller] {
				for _, provider := range unresolved.providers {
					if closure.nodes[provider] && !closure.nodes[caller] {
						closure.nodes[caller] = true
						closure.next[caller] = provider
						changed = true
					}
				}
			}
		}
	}
	return closure
}

func (closure weeklySettingsClosure) chain(start string) []string {
	chain := []string{start}
	seen := map[string]bool{start: true}
	for current := start; closure.next[current] != ""; {
		next := closure.next[current]
		if seen[next] {
			break
		}
		chain = append(chain, next)
		seen[next] = true
		current = next
	}
	return chain
}

func weeklyFlockLockFunction(t *testing.T, inventory weeklyTypedInventory) *types.Func {
	t.Helper()
	flockPackage := weeklyRequiredImportedPackage(t, inventory, "github.com/gofrs/flock")
	flockType, ok := flockPackage.Scope().Lookup("Flock").(*types.TypeName)
	if !ok || flockType == nil {
		t.Fatal("type inventory is missing flock.Flock")
	}
	selection := types.NewMethodSet(types.NewPointer(flockType.Type())).Lookup(flockPackage, "Lock")
	if selection == nil {
		t.Fatal("type inventory is missing (*flock.Flock).Lock")
	}
	function, ok := selection.Obj().(*types.Func)
	if !ok || function == nil {
		t.Fatal("type inventory flock Lock selection is not a function")
	}
	return function
}

func weeklyTypedFixtureInventory(t *testing.T, source string) weeklyTypedInventory {
	t.Helper()
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "weekly_typed_graph_fixture.go", source, 0)
	if err != nil {
		t.Fatalf("parse type-aware graph fixture: %v", err)
	}
	info := weeklyNewTypesInfo()
	pkg, err := (&types.Config{Sizes: types.SizesFor(runtime.Compiler, runtime.GOARCH)}).Check("fixture", fileSet, []*ast.File{file}, info)
	if err != nil {
		t.Fatalf("type-check type-aware graph fixture: %v", err)
	}
	return weeklyTypedInventoryFromFiles(t, fileSet, pkg, info, []*ast.File{file})
}

func weeklyFixtureApprovedFunctionResultOptions(t *testing.T, inventory weeklyTypedInventory, factoryName string) weeklyTypedGraphOptions {
	t.Helper()
	factory := weeklyRequiredPackageFunc(t, inventory, factoryName)
	return weeklyTypedGraphOptions{
		approvedFunctionResults: map[weeklyFunctionResultOrigin]bool{
			{factory: factory, resultIndex: 0}: true,
		},
	}
}

func weeklyFixtureApprovedPackageCallbackOptions(t *testing.T, inventory weeklyTypedInventory, variableName, targetName string) weeklyTypedGraphOptions {
	t.Helper()
	variable := weeklyRequiredPackageVar(t, inventory, variableName)
	target := weeklyRequiredPackageFunc(t, inventory, targetName)
	return weeklyTypedGraphOptions{
		approvedPackageCallbacks: map[*types.Var]weeklyPackageCallbackOrigin{
			variable: {variable: variable, target: target},
		},
	}
}

func weeklyRequireFixtureResolvedPackageCallback(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph, rootName, targetName string) {
	t.Helper()
	root := weeklyRequiredPackageFunc(t, inventory, rootName)
	target := weeklyRequiredPackageFunc(t, inventory, targetName)
	rootNode := inventory.byFunction[root]
	targetNode := inventory.byFunction[target]
	if rootNode == nil || targetNode == nil {
		t.Fatalf("fixture package callback nodes are not indexed: root=%s target=%s", rootName, targetName)
	}
	if !graph.edges[rootNode.key][targetNode.key] {
		t.Fatalf("fixture package callback edge %s -> %s is missing", rootNode.key, targetNode.key)
	}
	want := weeklyPackageCallbackOrigin{variable: weeklyRequiredPackageVar(t, inventory, "callback"), target: target}
	dispatches := graph.approvedPackageCallbackDispatches[rootNode.key]
	if len(dispatches) != 1 || dispatches[0] != want {
		t.Fatalf("fixture approved package callback dispatches = %#v, want [%#v]", dispatches, want)
	}
	if _, err := weeklyReachableTypedNodes(inventory, graph, root); err != nil {
		t.Fatalf("reach approved package callback fixture: %v", err)
	}
}

func weeklyRequireFixtureUnresolvedFunctionVariable(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph, rootName string) {
	t.Helper()
	root := weeklyRequiredPackageFunc(t, inventory, rootName)
	rootNode := inventory.byFunction[root]
	if rootNode == nil {
		t.Fatalf("fixture root %s is not indexed", rootName)
	}
	if _, err := weeklyReachableTypedNodes(inventory, graph, root); err == nil || !strings.Contains(err.Error(), "unresolved typed boundary") {
		t.Fatalf("fixture %s reachability error = %v, want unresolved typed boundary", rootName, err)
	}
	if got := len(graph.approvedFunctionResultDispatches[rootNode.key]); got != 0 {
		t.Fatalf("fixture %s approved function-result dispatches = %d, want 0", rootName, got)
	}
	if got := len(graph.approvedPackageCallbackDispatches[rootNode.key]); got != 0 {
		t.Fatalf("fixture %s approved package-callback dispatches = %d, want 0", rootName, got)
	}
}

func TestWeeklyTypedCallGraphResolution(t *testing.T) {
	tests := []struct {
		name       string
		source     string
		options    func(*testing.T, weeklyTypedInventory) weeklyTypedGraphOptions
		buildError string
		assert     func(*testing.T, weeklyTypedInventory, weeklyTypedCallGraph)
	}{
		{
			name: "interface false-positive killer",
			source: `package fixture
type fixtureMutex struct{}
func (fixtureMutex) Lock() {}
var settingsMu fixtureMutex
type scheduler interface { Create() }
type concrete struct{}
func (concrete) Create() { settingsMu.Lock() }
func root(s scheduler) { s.Create() }
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyTypedGraphOptions{schedulerInterface: weeklyRequiredPackageType(t, inventory, "scheduler").Type()}
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				root := inventory.byFunction[weeklyRequiredPackageFunc(t, inventory, "root")]
				if root == nil {
					t.Fatal("fixture root is not indexed")
				}
				if graph.edges[root.key]["concrete.Create"] {
					t.Fatal("interface scheduler dispatch was incorrectly bound to same-named concrete local method")
				}
				if got := len(graph.approvedInterfaceDispatches[root.key]); got != 1 {
					t.Fatalf("typed interface dispatch count = %d, want 1", got)
				}
				reachable, err := weeklyReachableTypedNodes(inventory, graph, root.function)
				if err != nil {
					t.Fatalf("reach interface fixture: %v", err)
				}
				closure := weeklySettingsHelperClosure(inventory, graph, weeklyRequiredPackageVar(t, inventory, "settingsMu"))
				if closure.nodes[root.key] || reachable["concrete.Create"] {
					t.Fatal("interface dispatch inherited the unrelated concrete settings owner")
				}
			},
		},
		{
			name: "local false-negative killer",
			source: `package fixture
type fixtureMutex struct{}
func (fixtureMutex) Lock() {}
var settingsMu fixtureMutex
func helper() { settingsMu.Lock() }
func root() { helper() }
`,
			options: func(*testing.T, weeklyTypedInventory) weeklyTypedGraphOptions { return weeklyTypedGraphOptions{} },
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				root := inventory.byFunction[weeklyRequiredPackageFunc(t, inventory, "root")]
				helper := inventory.byFunction[weeklyRequiredPackageFunc(t, inventory, "helper")]
				if root == nil || helper == nil {
					t.Fatal("fixture local functions are not indexed")
				}
				if !graph.edges[root.key][helper.key] {
					t.Fatal("exact local helper edge is missing")
				}
				reachable, err := weeklyReachableTypedNodes(inventory, graph, root.function)
				if err != nil {
					t.Fatalf("reach local fixture: %v", err)
				}
				closure := weeklySettingsHelperClosure(inventory, graph, weeklyRequiredPackageVar(t, inventory, "settingsMu"))
				if !reachable[helper.key] || !closure.nodes[root.key] {
					t.Fatal("local settings helper did not taint the root through exact edges")
				}
				if got := strings.Join(closure.chain(root.key), " -> "); got != root.key+" -> "+helper.key {
					t.Fatalf("settings closure chain = %q, want %q", got, root.key+" -> "+helper.key)
				}
			},
		},
		{
			name: "allowlisted ledgered function-result origin",
			source: `package fixture
func ledgered() (func() error, error) { return func() error { return nil }, nil }
func root() {
	callback, _ := ledgered()
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedFunctionResultOptions(t, inventory, "ledgered")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				root := weeklyRequiredPackageFunc(t, inventory, "root")
				factory := weeklyRequiredPackageFunc(t, inventory, "ledgered")
				rootNode := inventory.byFunction[root]
				factoryNode := inventory.byFunction[factory]
				if rootNode == nil || factoryNode == nil {
					t.Fatal("allowlisted function-result fixture nodes are not indexed")
				}
				if !graph.edges[rootNode.key][factoryNode.key] {
					t.Fatal("allowlisted function-result origin did not retain the factory edge")
				}
				want := weeklyFunctionResultOrigin{factory: factory, resultIndex: 0}
				dispatches := graph.approvedFunctionResultDispatches[rootNode.key]
				if len(dispatches) != 1 || dispatches[0] != want {
					t.Fatalf("approved function-result dispatches = %#v, want [%#v]", dispatches, want)
				}
				if _, err := weeklyReachableTypedNodes(inventory, graph, root); err != nil {
					t.Fatalf("reach allowlisted function-result fixture: %v", err)
				}
			},
		},
		{
			name: "type conversion is an external leaf",
			source: `package fixture
func root() { _ = []byte(nil) }
`,
			options: func(*testing.T, weeklyTypedInventory) weeklyTypedGraphOptions { return weeklyTypedGraphOptions{} },
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				root := weeklyRequiredPackageFunc(t, inventory, "root")
				if _, err := weeklyReachableTypedNodes(inventory, graph, root); err != nil {
					t.Fatalf("reach type-conversion fixture: %v", err)
				}
			},
		},
		{
			name: "function parameter remains unresolved",
			source: `package fixture
func root(callback func() error) { _ = callback() }
`,
			options: func(*testing.T, weeklyTypedInventory) weeklyTypedGraphOptions { return weeklyTypedGraphOptions{} },
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				weeklyRequireFixtureUnresolvedFunctionVariable(t, inventory, graph, "root")
			},
		},
		{
			name: "function literal assignment remains unresolved",
			source: `package fixture
func root() {
	callback := func() error { return nil }
	_ = callback()
}
`,
			options: func(*testing.T, weeklyTypedInventory) weeklyTypedGraphOptions { return weeklyTypedGraphOptions{} },
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				weeklyRequireFixtureUnresolvedFunctionVariable(t, inventory, graph, "root")
			},
		},
		{
			name: "named function assignment remains unresolved",
			source: `package fixture
func named() error { return nil }
func root() {
	callback := named
	_ = callback()
}
`,
			options: func(*testing.T, weeklyTypedInventory) weeklyTypedGraphOptions { return weeklyTypedGraphOptions{} },
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				weeklyRequireFixtureUnresolvedFunctionVariable(t, inventory, graph, "root")
			},
		},
		{
			name: "same-signature non-allowlisted factory remains unresolved",
			source: `package fixture
func ledgered() (func() error, error) { return func() error { return nil }, nil }
func other() (func() error, error) { return func() error { return nil }, nil }
func root() {
	callback, _ := other()
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedFunctionResultOptions(t, inventory, "ledgered")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				weeklyRequireFixtureUnresolvedFunctionVariable(t, inventory, graph, "root")
			},
		},
		{
			name: "assignment instead of short declaration remains unresolved",
			source: `package fixture
func ledgered() (func() error, error) { return func() error { return nil }, nil }
func root() {
	var callback func() error
	callback, _ = ledgered()
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedFunctionResultOptions(t, inventory, "ledgered")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				weeklyRequireFixtureUnresolvedFunctionVariable(t, inventory, graph, "root")
			},
		},
		{
			name: "conditional definition remains unresolved",
			source: `package fixture
func ledgered() (func() error, error) { return func() error { return nil }, nil }
func root() {
	if true {
		callback, _ := ledgered()
		_ = callback()
	}
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedFunctionResultOptions(t, inventory, "ledgered")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				weeklyRequireFixtureUnresolvedFunctionVariable(t, inventory, graph, "root")
			},
		},
		{
			name: "reassigned multiple-origin variable remains unresolved",
			source: `package fixture
func ledgered() (func() error, error) { return func() error { return nil }, nil }
func other() (func() error, error) { return func() error { return nil }, nil }
func root() {
	callback, _ := ledgered()
	callback, _ = other()
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedFunctionResultOptions(t, inventory, "ledgered")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				weeklyRequireFixtureUnresolvedFunctionVariable(t, inventory, graph, "root")
			},
		},
		{
			name: "escaped function variable remains unresolved",
			source: `package fixture
func ledgered() (func() error, error) { return func() error { return nil }, nil }
func consume(func() error) {}
func root() {
	callback, _ := ledgered()
	consume(callback)
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedFunctionResultOptions(t, inventory, "ledgered")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				weeklyRequireFixtureUnresolvedFunctionVariable(t, inventory, graph, "root")
			},
		},
		{
			name: "returned function variable remains unresolved",
			source: `package fixture
func ledgered() (func() error, error) { return func() error { return nil }, nil }
func root() func() error {
	callback, _ := ledgered()
	_ = callback()
	return callback
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedFunctionResultOptions(t, inventory, "ledgered")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				weeklyRequireFixtureUnresolvedFunctionVariable(t, inventory, graph, "root")
			},
		},
		{
			name: "closure-captured function variable remains unresolved",
			source: `package fixture
func ledgered() (func() error, error) { return func() error { return nil }, nil }
func root() {
	callback, _ := ledgered()
	func() { _ = callback() }()
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedFunctionResultOptions(t, inventory, "ledgered")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				weeklyRequireFixtureUnresolvedFunctionVariable(t, inventory, graph, "root")
			},
		},
		{
			name: "multiple function-variable calls remain unresolved",
			source: `package fixture
func ledgered() (func() error, error) { return func() error { return nil }, nil }
func root() {
	callback, _ := ledgered()
	_ = callback()
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedFunctionResultOptions(t, inventory, "ledgered")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				weeklyRequireFixtureUnresolvedFunctionVariable(t, inventory, graph, "root")
			},
		},
		{
			name: "allowlisted direct settings factory remains reachable",
			source: `package fixture
type fixtureMutex struct{}
func (fixtureMutex) Lock() {}
var settingsMu fixtureMutex
func ledgered() (func() error, error) {
	settingsMu.Lock()
	return func() error { return nil }, nil
}
func root() {
	callback, _ := ledgered()
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedFunctionResultOptions(t, inventory, "ledgered")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				root := weeklyRequiredPackageFunc(t, inventory, "root")
				rootNode := inventory.byFunction[root]
				factoryNode := inventory.byFunction[weeklyRequiredPackageFunc(t, inventory, "ledgered")]
				if rootNode == nil || factoryNode == nil {
					t.Fatal("direct settings fixture nodes are not indexed")
				}
				if _, err := weeklyReachableTypedNodes(inventory, graph, root); err != nil {
					t.Fatalf("reach direct settings fixture: %v", err)
				}
				closure := weeklySettingsHelperClosure(inventory, graph, weeklyRequiredPackageVar(t, inventory, "settingsMu"))
				if !closure.nodes[rootNode.key] {
					t.Fatal("allowlisted direct settings factory did not taint the root")
				}
				if got := strings.Join(closure.chain(rootNode.key), " -> "); got != rootNode.key+" -> "+factoryNode.key {
					t.Fatalf("direct settings closure chain = %q, want %q", got, rootNode.key+" -> "+factoryNode.key)
				}
			},
		},
		{
			name: "allowlisted transitive settings factory remains reachable",
			source: `package fixture
type fixtureMutex struct{}
func (fixtureMutex) Lock() {}
var settingsMu fixtureMutex
func helper() { settingsMu.Lock() }
func ledgered() (func() error, error) {
	helper()
	return func() error { return nil }, nil
}
func root() {
	callback, _ := ledgered()
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedFunctionResultOptions(t, inventory, "ledgered")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				root := weeklyRequiredPackageFunc(t, inventory, "root")
				rootNode := inventory.byFunction[root]
				factoryNode := inventory.byFunction[weeklyRequiredPackageFunc(t, inventory, "ledgered")]
				helperNode := inventory.byFunction[weeklyRequiredPackageFunc(t, inventory, "helper")]
				if rootNode == nil || factoryNode == nil || helperNode == nil {
					t.Fatal("transitive settings fixture nodes are not indexed")
				}
				if _, err := weeklyReachableTypedNodes(inventory, graph, root); err != nil {
					t.Fatalf("reach transitive settings fixture: %v", err)
				}
				closure := weeklySettingsHelperClosure(inventory, graph, weeklyRequiredPackageVar(t, inventory, "settingsMu"))
				if !closure.nodes[rootNode.key] {
					t.Fatal("allowlisted transitive settings factory did not taint the root")
				}
				want := rootNode.key + " -> " + factoryNode.key + " -> " + helperNode.key
				if got := strings.Join(closure.chain(rootNode.key), " -> "); got != want {
					t.Fatalf("transitive settings closure chain = %q, want %q", got, want)
				}
			},
		},
		{
			name: "selected Windows package callback origin",
			source: `package fixture
var callback func() error
func windowsTarget() error { return nil }
func init() { callback = windowsTarget }
func root() {
	if callback == nil { return }
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "windowsTarget")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				weeklyRequireFixtureResolvedPackageCallback(t, inventory, graph, "root", "windowsTarget")
			},
		},
		{
			name: "selected non-Windows package callback origin",
			source: `package fixture
var callback func() error
func unixTarget() error { return nil }
func init() { callback = unixTarget }
func root() {
	if callback != nil { _ = callback() }
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "unixTarget")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				weeklyRequireFixtureResolvedPackageCallback(t, inventory, graph, "root", "unixTarget")
			},
		},
		{
			name: "unconfigured package callback remains unresolved",
			source: `package fixture
var callback func() error
func target() error { return nil }
func init() { callback = target }
func root() {
	if callback == nil { return }
	_ = callback()
}
`,
			options: func(*testing.T, weeklyTypedInventory) weeklyTypedGraphOptions { return weeklyTypedGraphOptions{} },
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				weeklyRequireFixtureUnresolvedFunctionVariable(t, inventory, graph, "root")
			},
		},
		{
			name: "package callback wrong target",
			source: `package fixture
var callback func() error
func windowsTarget() error { return nil }
func alternateTarget() error { return nil }
func init() { callback = alternateTarget }
func root() {
	if callback == nil { return }
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "windowsTarget")
			},
			buildError: "callback initializer target",
		},
		{
			name: "Windows policy rejects non-Windows target",
			source: `package fixture
var callback func() error
func windowsTarget() error { return nil }
func unixTarget() error { return nil }
func init() { callback = unixTarget }
func root() {
	if callback == nil { return }
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "windowsTarget")
			},
			buildError: "callback initializer target",
		},
		{
			name: "non-Windows policy rejects Windows target",
			source: `package fixture
var callback func() error
func windowsTarget() error { return nil }
func unixTarget() error { return nil }
func init() { callback = windowsTarget }
func root() {
	if callback != nil { _ = callback() }
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "unixTarget")
			},
			buildError: "callback initializer target",
		},
		{
			name: "package callback multiple init writes",
			source: `package fixture
var callback func() error
func target() error { return nil }
func init() { callback = target }
func init() { callback = target }
func root() {
	if callback == nil { return }
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "target")
			},
			buildError: "exactly one selected non-test init write",
		},
		{
			name: "package callback missing init has no fallback",
			source: `package fixture
var callback func() error
func target() error { return nil }
func root() {
	if callback == nil { return }
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "target")
			},
			buildError: "exactly one selected non-test init write",
		},
		{
			name: "package callback nested init is mutable origin",
			source: `package fixture
var callback func() error
func target() error { return nil }
func init() {
	if true { callback = target }
}
func root() {
	if callback == nil { return }
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "target")
			},
			buildError: "not one direct top-level package init assignment",
		},
		{
			name: "test override moved into production is a second write",
			source: `package fixture
var callback func() error
func target() error { return nil }
func init() { callback = target }
func installTestOverride() { callback = target }
func root() {
	if callback == nil { return }
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "target")
			},
			buildError: "exactly one selected non-test init write",
		},
		{
			name: "package callback escape is rejected",
			source: `package fixture
var callback func() error
func target() error { return nil }
func consume(func() error) {}
func init() { callback = target }
func root() {
	if callback == nil { return }
	consume(callback)
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "target")
			},
			buildError: "unsupported selected non-test uses",
		},
		{
			name: "package callback go dispatch is rejected",
			source: `package fixture
var callback func() error
func target() error { return nil }
func init() { callback = target }
func root() {
	if callback != nil { go callback() }
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "target")
			},
			buildError: "go-dispatch",
		},
		{
			name: "package callback missing nil guard",
			source: `package fixture
var callback func() error
func target() error { return nil }
func init() { callback = target }
func root() { _ = callback() }
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "target")
			},
			buildError: "fails exact nil guard",
		},
		{
			name: "package callback direct forbidden target remains reachable",
			source: `package fixture
type fixtureMutex struct{}
func (fixtureMutex) Lock() {}
var settingsMu fixtureMutex
var callback func() error
func target() error { settingsMu.Lock(); return nil }
func init() { callback = target }
func root() {
	if callback == nil { return }
	_ = callback()
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "target")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				weeklyRequireFixtureResolvedPackageCallback(t, inventory, graph, "root", "target")
				root := inventory.byFunction[weeklyRequiredPackageFunc(t, inventory, "root")]
				target := inventory.byFunction[weeklyRequiredPackageFunc(t, inventory, "target")]
				if root == nil || target == nil {
					t.Fatal("direct forbidden package callback nodes are not indexed")
				}
				closure := weeklySettingsHelperClosure(inventory, graph, weeklyRequiredPackageVar(t, inventory, "settingsMu"))
				if !closure.nodes[root.key] || strings.Join(closure.chain(root.key), " -> ") != root.key+" -> "+target.key {
					t.Fatalf("direct forbidden package target chain = %v, want %s -> %s", closure.chain(root.key), root.key, target.key)
				}
			},
		},
		{
			name: "package callback transitive forbidden target remains reachable",
			source: `package fixture
type fixtureMutex struct{}
func (fixtureMutex) Lock() {}
var settingsMu fixtureMutex
var callback func() error
func helper() { settingsMu.Lock() }
func target() error { helper(); return nil }
func init() { callback = target }
func root() {
	if callback != nil { _ = callback() }
}
`,
			options: func(t *testing.T, inventory weeklyTypedInventory) weeklyTypedGraphOptions {
				return weeklyFixtureApprovedPackageCallbackOptions(t, inventory, "callback", "target")
			},
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				weeklyRequireFixtureResolvedPackageCallback(t, inventory, graph, "root", "target")
				root := inventory.byFunction[weeklyRequiredPackageFunc(t, inventory, "root")]
				target := inventory.byFunction[weeklyRequiredPackageFunc(t, inventory, "target")]
				helper := inventory.byFunction[weeklyRequiredPackageFunc(t, inventory, "helper")]
				if root == nil || target == nil || helper == nil {
					t.Fatal("transitive forbidden package callback nodes are not indexed")
				}
				closure := weeklySettingsHelperClosure(inventory, graph, weeklyRequiredPackageVar(t, inventory, "settingsMu"))
				want := root.key + " -> " + target.key + " -> " + helper.key
				if !closure.nodes[root.key] || strings.Join(closure.chain(root.key), " -> ") != want {
					t.Fatalf("transitive forbidden package target chain = %v, want %s", closure.chain(root.key), want)
				}
			},
		},
		{
			name: "unresolved fail-closed killer",
			source: `package fixture
type holder struct { callback func() }
func root(value holder) { value.callback() }
`,
			options: func(*testing.T, weeklyTypedInventory) weeklyTypedGraphOptions { return weeklyTypedGraphOptions{} },
			assert: func(t *testing.T, inventory weeklyTypedInventory, graph weeklyTypedCallGraph) {
				t.Helper()
				root := weeklyRequiredPackageFunc(t, inventory, "root")
				if _, err := weeklyReachableTypedNodes(inventory, graph, root); err == nil || !strings.Contains(err.Error(), "unresolved typed boundary") {
					t.Fatalf("unbounded function field reachability error = %v, want unresolved typed boundary", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inventory := weeklyTypedFixtureInventory(t, test.source)
			graph, err := weeklyBuildTypedCallGraph(&inventory, test.options(t, inventory))
			if test.buildError != "" {
				if err == nil || !strings.Contains(err.Error(), test.buildError) {
					t.Fatalf("build type-aware fixture graph error = %v, want substring %q", err, test.buildError)
				}
				return
			}
			if err != nil {
				t.Fatalf("build type-aware fixture graph: %v", err)
			}
			test.assert(t, inventory, graph)
		})
	}
}

func TestWeeklyRefreshTaskMutationOwnerInventory(t *testing.T) {
	functions := weeklyAPISourceFunctions(t)
	writers := []string{
		"EnsureWeeklyRefreshTask",
		"ApplyWeeklyRefreshSchedule",
		"swapWeeklyTriggerWith",
		"upgradeWorkspaceWeeklyRefreshTask",
	}
	for _, name := range writers {
		fn := functions[name]
		if fn == nil {
			t.Fatalf("weekly writer %s is missing from non-test api source", name)
		}
		if !weeklyFunctionCalls(fn, "runWeeklyRefreshTaskTransaction") {
			t.Fatalf("weekly writer %s does not delegate to runWeeklyRefreshTaskTransaction", name)
		}
		for _, operation := range []string{"ExportXML", "Delete", "Create", "ImportXML"} {
			if weeklyFunctionCalls(fn, operation) {
				t.Fatalf("weekly writer %s directly calls %s instead of the common transaction owner", name, operation)
			}
		}
	}
	schedulerUpgrade := functions["SchedulerUpgrade"]
	if schedulerUpgrade == nil {
		t.Fatal("SchedulerUpgrade is missing")
	}
	upgradeBranch := weeklySchedulerUpgradeBranch(schedulerUpgrade)
	if upgradeBranch == nil || !weeklyNodeCalls(upgradeBranch, "upgradeWorkspaceWeeklyRefreshTask") {
		t.Fatal("SchedulerUpgrade weekly branch no longer delegates to upgradeWorkspaceWeeklyRefreshTask")
	}
	for _, operation := range []string{"ExportXML", "Delete", "Create", "ImportXML"} {
		if weeklyNodeCalls(upgradeBranch, operation) {
			t.Fatalf("SchedulerUpgrade weekly branch directly calls %s instead of the common transaction owner", operation)
		}
	}
	for name, fn := range functions {
		if name == "runWeeklyRefreshTaskTransaction" || name == "SchedulerUpgrade" || !weeklyFunctionReferences(fn, "WeeklyRefreshTaskName") {
			continue
		}
		for _, operation := range []string{"ExportXML", "Delete", "Create", "ImportXML"} {
			if weeklyFunctionCalls(fn, operation) {
				t.Fatalf("weekly singleton operation %s remains outside runWeeklyRefreshTaskTransaction in %s", operation, name)
			}
		}
	}
	owner := functions["runWeeklyRefreshTaskTransaction"]
	if owner == nil {
		t.Fatal("weekly transaction owner is missing")
	}
	for _, operation := range []string{"ExportXML", "Delete", "Create", "ImportXML"} {
		if !weeklyFunctionCalls(owner, operation) {
			t.Fatalf("weekly transaction owner no longer contains %s", operation)
		}
	}
}

func weeklyAPITestSourceSetForCurrentBuild(t *testing.T) weeklyAPISourceSet {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve weekly refresh test source directory")
	}
	sourceDir := filepath.Dir(file)
	context := build.Default
	context.BuildTags = append([]string(nil), build.Default.BuildTags...)
	if testEnvFallbackBuild {
		context.BuildTags = append(context.BuildTags, "test_state_path_env")
	}
	var matchErr error
	fileSet := token.NewFileSet()
	packages, err := parser.ParseDir(fileSet, sourceDir, func(info os.FileInfo) bool {
		if !strings.HasSuffix(info.Name(), "_test.go") {
			return false
		}
		matches, err := context.MatchFile(sourceDir, info.Name())
		if err != nil {
			matchErr = err
			return false
		}
		return matches
	}, 0)
	if err != nil {
		t.Fatalf("parse selected api test source: %v", err)
	}
	if matchErr != nil {
		t.Fatalf("resolve selected api test build constraints: %v", matchErr)
	}
	pkg := packages["api"]
	if pkg == nil {
		t.Fatal("selected api test package source was not parsed")
	}
	fileNames := make([]string, 0, len(pkg.Files))
	for fileName := range pkg.Files {
		fileNames = append(fileNames, fileName)
	}
	sort.Strings(fileNames)
	files := make([]*ast.File, 0, len(fileNames))
	for _, fileName := range fileNames {
		files = append(files, pkg.Files[fileName])
	}
	return weeklyAPISourceSet{sourceDir: sourceDir, fileSet: fileSet, pkg: pkg, files: files}
}

func weeklyExactIdentifier(expression ast.Expr, name string) bool {
	identifier, ok := weeklyUnwrapParentheses(expression).(*ast.Ident)
	return ok && identifier.Name == name
}

func weeklyExactTestHelperCall(call *ast.CallExpr, name, argument string) bool {
	identifier, ok := weeklyUnwrapParentheses(call.Fun).(*ast.Ident)
	return ok && identifier.Name == name && len(call.Args) != 0 && weeklyExactIdentifier(call.Args[0], argument)
}

func weeklyValidateKnownFolderTestOverrideLifecycle(t *testing.T) {
	t.Helper()
	source := weeklyAPITestSourceSetForCurrentBuild(t)
	functions := map[string]*ast.FuncDecl{}
	for _, file := range source.files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Body == nil {
				continue
			}
			if function.Name.Name == "statePathsHelper" || function.Name.Name == "installKnownFolderStub" {
				if functions[function.Name.Name] != nil {
					t.Fatalf("selected test source has duplicate %s", function.Name.Name)
				}
				functions[function.Name.Name] = function
			}
		}
	}
	helper := functions["statePathsHelper"]
	setter := functions["installKnownFolderStub"]
	if helper == nil || setter == nil {
		t.Fatalf("selected test source is missing known-folder helper lifecycle: helper=%t setter=%t", helper != nil, setter != nil)
	}

	savedResolver := false
	cleanupRestore := false
	ast.Inspect(helper.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if ok && assignment.Tok == token.DEFINE && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 &&
			weeklyExactIdentifier(assignment.Lhs[0], "prevResolver") && weeklyExactIdentifier(assignment.Rhs[0], "knownFolderResolverFn") {
			savedResolver = true
		}
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		selector, ok := weeklyUnwrapParentheses(call.Fun).(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Cleanup" || !weeklyExactIdentifier(selector.X, "t") {
			return true
		}
		literal, ok := weeklyUnwrapParentheses(call.Args[0]).(*ast.FuncLit)
		if !ok {
			return true
		}
		ast.Inspect(literal.Body, func(candidate ast.Node) bool {
			restore, ok := candidate.(*ast.AssignStmt)
			if ok && restore.Tok == token.ASSIGN && len(restore.Lhs) == 1 && len(restore.Rhs) == 1 &&
				weeklyExactIdentifier(restore.Lhs[0], "knownFolderResolverFn") && weeklyExactIdentifier(restore.Rhs[0], "prevResolver") {
				cleanupRestore = true
			}
			return true
		})
		return true
	})
	if !savedResolver || !cleanupRestore {
		t.Fatalf("statePathsHelper known-folder lifecycle = save:%t cleanup-restore:%t, want both", savedResolver, cleanupRestore)
	}

	setterWrites := 0
	ast.Inspect(setter.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if ok && assignment.Tok == token.ASSIGN && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 &&
			weeklyExactIdentifier(assignment.Lhs[0], "knownFolderResolverFn") && weeklyExactIdentifier(assignment.Rhs[0], "fn") {
			setterWrites++
		}
		return true
	})
	if setterWrites != 1 {
		t.Fatalf("installKnownFolderStub exact writes = %d, want 1", setterWrites)
	}

	callSites := 0
	for _, file := range source.files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil || function == setter {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				identifier, ok := weeklyUnwrapParentheses(call.Fun).(*ast.Ident)
				if !ok || identifier.Name != "installKnownFolderStub" || len(call.Args) == 0 {
					return true
				}
				argument, ok := weeklyUnwrapParentheses(call.Args[0]).(*ast.Ident)
				if !ok {
					t.Fatalf("installKnownFolderStub call at %s has no exact testing argument", source.fileSet.Position(call.Pos()))
				}
				callSites++
				helperBefore := false
				parallel := false
				ast.Inspect(function.Body, func(candidate ast.Node) bool {
					candidateCall, ok := candidate.(*ast.CallExpr)
					if !ok {
						return true
					}
					if candidateCall.Pos() < call.Pos() && weeklyExactTestHelperCall(candidateCall, "statePathsHelper", argument.Name) {
						helperBefore = true
					}
					selector, ok := weeklyUnwrapParentheses(candidateCall.Fun).(*ast.SelectorExpr)
					if ok && selector.Sel.Name == "Parallel" && weeklyExactIdentifier(selector.X, argument.Name) {
						parallel = true
					}
					return true
				})
				if !helperBefore || parallel {
					t.Fatalf("installKnownFolderStub call at %s helper-before/parallel = %t/%t, want true/false", source.fileSet.Position(call.Pos()), helperBefore, parallel)
				}
				return true
			})
		}
	}
	wantCallSites := 3
	if testEnvFallbackBuild {
		wantCallSites = 8
	}
	if callSites != wantCallSites {
		t.Fatalf("selected installKnownFolderStub call sites = %d, want %d", callSites, wantCallSites)
	}
}

func TestWeeklyRefreshLockOrderInventory(t *testing.T) {
	weeklyValidateKnownFolderTestOverrideLifecycle(t)
	functions := weeklyAPISourceFunctions(t)
	settingsOwners := map[string]bool{}
	for name, fn := range functions {
		if weeklyFunctionCalls(fn, "withWeeklyScheduleSettings") {
			settingsOwners[name] = true
		}
	}
	for _, name := range []string{"EnsureWeeklyRefreshTask", "ApplyWeeklyRefreshSchedule"} {
		fn := functions[name]
		if !settingsOwners[name] || !weeklyCallbackCallsTransaction(fn) {
			t.Fatalf("%s must acquire settings ownership and call the weekly transaction from its settings callback", name)
		}
		delete(settingsOwners, name)
	}
	if len(settingsOwners) != 0 {
		t.Fatalf("unexpected weekly settings owners: %v", settingsOwners)
	}
	inventory := weeklyTypedAPIInventory(t)
	for _, file := range inventory.files {
		fileName := inventory.fileSet.Position(file.Pos()).Filename
		if strings.HasSuffix(fileName, "_test.go") {
			t.Fatalf("production typed inventory includes test source %s", filepath.Base(fileName))
		}
	}
	options := weeklyProductionTypedGraphOptions(t, inventory)
	if len(options.approvedPackageCallbacks) != 1 {
		t.Fatalf("production package callback policies = %d, want 1", len(options.approvedPackageCallbacks))
	}
	graph, err := weeklyBuildTypedCallGraph(&inventory, options)
	if err != nil {
		t.Fatalf("build type-aware weekly call graph: %v", err)
	}
	callbackCallerName := "resolveKnownFolderProduction"
	if testEnvFallbackBuild {
		callbackCallerName = "resolveKnownFolderWithEnvFallback"
	}
	callbackCaller := weeklyRequiredPackageFunc(t, inventory, callbackCallerName)
	callbackCallerNode := inventory.byFunction[callbackCaller]
	if callbackCallerNode == nil {
		t.Fatalf("selected known-folder callback caller %s is not indexed", callbackCallerName)
	}
	var wantCallbackOrigin weeklyPackageCallbackOrigin
	for _, origin := range options.approvedPackageCallbacks {
		wantCallbackOrigin = origin
	}
	callbackDispatches := graph.approvedPackageCallbackDispatches[callbackCallerNode.key]
	if len(callbackDispatches) != 1 || callbackDispatches[0] != wantCallbackOrigin {
		t.Fatalf("selected known-folder callback dispatches = %#v, want [%#v]", callbackDispatches, wantCallbackOrigin)
	}
	callbackTargetNode := inventory.byFunction[wantCallbackOrigin.target]
	if callbackTargetNode == nil || !graph.edges[callbackCallerNode.key][callbackTargetNode.key] {
		t.Fatalf("selected known-folder callback edge %s -> %s is missing", callbackCallerNode.key, weeklyPackageCallbackObjectDiagnostic(inventory, wantCallbackOrigin.target))
	}
	for caller, dispatches := range graph.approvedPackageCallbackDispatches {
		if caller != callbackCallerNode.key && len(dispatches) != 0 {
			t.Fatalf("unexpected package callback dispatches from %s: %#v", caller, dispatches)
		}
	}
	reachable, err := weeklyReachableTypedNodes(inventory, graph, options.transaction)
	if err != nil {
		t.Fatalf("resolve type-aware weekly reachability: %v", err)
	}
	settingsMu := weeklyRequiredPackageVar(t, inventory, "settingsMu")
	flockPackage := weeklyRequiredImportedPackage(t, inventory, "github.com/gofrs/flock")
	flockNew, ok := flockPackage.Scope().Lookup("New").(*types.Func)
	if !ok || flockNew == nil {
		t.Fatal("type inventory is missing flock.New")
	}
	flockLock := weeklyFlockLockFunction(t, inventory)
	settingsWrite := weeklyRequiredPackageFunc(t, inventory, "WriteStateFileBytesLockHeld")
	lockLeafFactory := weeklyRequiredPackageFunc(t, inventory, "lockLeafLedgered")
	lockLeafRawOwner := weeklyRequiredPackageFunc(t, inventory, "lockLeafLedgeredWithUnlock")
	transactionNode := inventory.byFunction[options.transaction]
	if transactionNode == nil {
		t.Fatal("weekly transaction root is not indexed")
	}
	wantReleaseOrigin := weeklyFunctionResultOrigin{factory: lockLeafFactory, resultIndex: 0}
	if dispatches := graph.approvedFunctionResultDispatches[transactionNode.key]; len(dispatches) != 1 || dispatches[0] != wantReleaseOrigin {
		t.Fatalf("weekly approved release dispatches = %#v, want [%#v]", dispatches, wantReleaseOrigin)
	}
	flockNewCalls := 0
	flockLockCalls := 0
	for _, key := range weeklySortedFunctionKeys(reachable) {
		source := inventory.nodes[key]
		if weeklyDirectSettingsMutexAcquisition(inventory, source, settingsMu) {
			t.Fatalf("weekly transaction path has direct settings mutex acquisition in %s", source.key)
		}
		if weeklyDirectSettingsWrite(inventory, source, settingsWrite) {
			t.Fatalf("weekly transaction path has direct settings write primitive in %s", source.key)
		}
		if count := weeklyDirectFlockNewCount(inventory, source, flockNew); count != 0 {
			if source.function != lockLeafRawOwner {
				t.Fatalf("weekly transaction path has direct flock.New acquisition in %s", source.key)
			}
			flockNewCalls += count
		}
		if count := weeklyDirectFlockLockCount(inventory, source, flockLock); count != 0 {
			if source.function != lockLeafRawOwner {
				t.Fatalf("weekly transaction path has direct flock Lock acquisition in %s", source.key)
			}
			flockLockCalls += count
		}
	}
	if flockNewCalls != 1 || flockLockCalls != 1 {
		t.Fatalf("weekly singleton flock acquisition = New:%d Lock:%d, want one call path through lockLeafLedgered", flockNewCalls, flockLockCalls)
	}
	settingsHelpers := weeklySettingsHelperClosure(inventory, graph, settingsMu)
	for _, key := range weeklySortedFunctionKeys(reachable) {
		if settingsHelpers.nodes[key] {
			t.Fatalf("weekly transaction path reaches transitive settings helper %s via %s", inventory.nodes[key].key, strings.Join(settingsHelpers.chain(key), " -> "))
		}
	}
}

func TestEnsurePreservesPersistedWeeklySchedule(t *testing.T) {
	sch := &weeklyAtomicScheduler{xml: map[string][]byte{}}
	installWeeklyAtomicHarness(t, sch)
	before := setWeeklyScheduleForAtomicTest(t, "weekly Tue 14:30")
	canonical := filepath.Join(t.TempDir(), "mcphub.exe")
	previousCanonical := testCanonicalMcphubPathOverride
	testCanonicalMcphubPathOverride = canonical
	t.Cleanup(func() { testCanonicalMcphubPathOverride = previousCanonical })

	if err := NewAPI().EnsureWeeklyRefreshTask(); err != nil {
		t.Fatalf("EnsureWeeklyRefreshTask: %v", err)
	}
	after, err := os.ReadFile(SettingsPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("EnsureWeeklyRefreshTask rewrote persisted schedule bytes:\nbefore=%q\nafter=%q", before, after)
	}
	persisted, err := NewAPI().SettingsGet("daemons.weekly_schedule")
	if err != nil || persisted != "weekly Tue 14:30" {
		t.Fatalf("persisted weekly schedule = %q err=%v, want weekly Tue 14:30", persisted, err)
	}
	current, err := sch.ExportXML(WeeklyRefreshTaskName)
	if err != nil {
		t.Fatal(err)
	}
	want := weeklyRefreshTaskSpec(canonical, &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 2, Hour: 14, Minute: 30})
	matches, err := weeklyTaskXMLMatchesSpec(current, want)
	if err != nil || !matches {
		t.Fatalf("Ensure task = %s, matches persisted Tuesday 14:30=%t err=%v", current, matches, err)
	}
}

func TestWeeklyRefreshReleaseSettlementMatrix(t *testing.T) {
	desiredSchedule := &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 2, Hour: 14, Minute: 30}

	t.Run("direct owner publishes applied-release-unconfirmed only after verified generation", func(t *testing.T) {
		for _, createErrorAfterWrite := range []bool{false, true} {
			t.Run(fmt.Sprintf("create-error-current-matches=%t", createErrorAfterWrite), func(t *testing.T) {
				canonical := filepath.Join(t.TempDir(), "mcphub.exe")
				sch := &weeklyAtomicScheduler{
					xml:                   map[string][]byte{},
					failCreateCount:       boolToInt(createErrorAfterWrite),
					createErrorAfterWrite: createErrorAfterWrite,
				}
				lockPath := installWeeklyAtomicHarness(t, sch)
				releaseCause := injectWeeklyUnlockFailure(t, lockPath).cause
				result, err := runWeeklyRefreshTaskTransaction(sch, weeklyRefreshMutation{
					desired: func([]byte, bool) (scheduler.TaskSpec, error) {
						return weeklyRefreshTaskSpec(canonical, desiredSchedule), nil
					},
				})
				if !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, releaseCause) {
					t.Fatalf("transaction error = %v, want typed release class and cause", err)
				}
				if result.state != weeklyRefreshTaskAppliedReleaseUnconfirmed || result.restoreStatus != "n/a" {
					t.Fatalf("transaction result = %+v, want applied-release-unconfirmed/n-a", result)
				}
				if matches, classifyErr := weeklyTaskXMLMatchesSpec(result.finalXML, weeklyRefreshTaskSpec(canonical, desiredSchedule)); classifyErr != nil || !matches {
					t.Fatalf("verified final XML is not requested generation: matches=%t err=%v xml=%s", matches, classifyErr, result.finalXML)
				}
				before := sch.operations()
				_, laterErr := runWeeklyRefreshTaskTransaction(sch, weeklyRefreshMutation{
					desired: func([]byte, bool) (scheduler.TaskSpec, error) {
						return weeklyRefreshTaskSpec(canonical, desiredSchedule), nil
					},
				})
				if !errors.Is(laterErr, ErrLockReleaseUnconfirmed) {
					t.Fatalf("later transaction error = %v, want retained-lock fail-fast", laterErr)
				}
				if after := sch.operations(); after != before {
					t.Fatalf("later transaction reached scheduler: before=%d after=%d", before, after)
				}
			})
		}
	})

	t.Run("Ensure preserves release failure and later fail-fast", func(t *testing.T) {
		sch := &weeklyAtomicScheduler{xml: map[string][]byte{}}
		lockPath := installWeeklyAtomicHarness(t, sch)
		releaseCause := injectWeeklyUnlockFailure(t, lockPath).cause
		setWeeklyScheduleForAtomicTest(t, "weekly Tue 14:30")
		canonical := filepath.Join(t.TempDir(), "mcphub.exe")
		previousCanonical := testCanonicalMcphubPathOverride
		testCanonicalMcphubPathOverride = canonical
		t.Cleanup(func() { testCanonicalMcphubPathOverride = previousCanonical })
		if err := NewAPI().EnsureWeeklyRefreshTask(); !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, releaseCause) {
			t.Fatalf("EnsureWeeklyRefreshTask error = %v, want typed release class and cause", err)
		}
		before := sch.operations()
		if err := NewAPI().EnsureWeeklyRefreshTask(); !errors.Is(err, ErrLockReleaseUnconfirmed) {
			t.Fatalf("later EnsureWeeklyRefreshTask error = %v, want retained-lock fail-fast", err)
		}
		if after := sch.operations(); after != before {
			t.Fatalf("later EnsureWeeklyRefreshTask reached scheduler: before=%d after=%d", before, after)
		}
	})

	t.Run("swap preserves release failure and later fail-fast", func(t *testing.T) {
		sch := &weeklyAtomicScheduler{xml: map[string][]byte{}}
		lockPath := installWeeklyAtomicHarness(t, sch)
		releaseCause := injectWeeklyUnlockFailure(t, lockPath).cause
		canonical := filepath.Join(t.TempDir(), "mcphub.exe")
		previousCanonical := testCanonicalMcphubPathOverride
		testCanonicalMcphubPathOverride = canonical
		t.Cleanup(func() { testCanonicalMcphubPathOverride = previousCanonical })
		if _, err := swapWeeklyTriggerWith(sch, desiredSchedule, nil); !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, releaseCause) {
			t.Fatalf("swapWeeklyTriggerWith error = %v, want typed release class and cause", err)
		}
		before := sch.operations()
		if _, err := swapWeeklyTriggerWith(sch, desiredSchedule, nil); !errors.Is(err, ErrLockReleaseUnconfirmed) {
			t.Fatalf("later swapWeeklyTriggerWith error = %v, want retained-lock fail-fast", err)
		}
		if after := sch.operations(); after != before {
			t.Fatalf("later swapWeeklyTriggerWith reached scheduler: before=%d after=%d", before, after)
		}
	})

	t.Run("upgrade preserves release failure and later fail-fast", func(t *testing.T) {
		sch := &weeklyAtomicScheduler{xml: map[string][]byte{}}
		lockPath := installWeeklyAtomicHarness(t, sch)
		releaseCause := injectWeeklyUnlockFailure(t, lockPath).cause
		result := upgradeWorkspaceWeeklyRefreshTask(sch, WeeklyRefreshTaskName, filepath.Join(t.TempDir(), "mcphub.exe"))
		if result == nil || !strings.Contains(result.Err, ErrLockReleaseUnconfirmed.Error()) || !strings.Contains(result.Err, releaseCause.Error()) {
			t.Fatalf("upgrade result = %+v, want release class and cause", result)
		}
		before := sch.operations()
		later := upgradeWorkspaceWeeklyRefreshTask(sch, WeeklyRefreshTaskName, filepath.Join(t.TempDir(), "mcphub.exe"))
		if later == nil || !strings.Contains(later.Err, ErrLockReleaseUnconfirmed.Error()) {
			t.Fatalf("later upgrade result = %+v, want retained-lock fail-fast", later)
		}
		if after := sch.operations(); after != before {
			t.Fatalf("later upgrade reached scheduler: before=%d after=%d", before, after)
		}
	})

	t.Run("Apply keeps matching settings and task on release failure", func(t *testing.T) {
		for _, createErrorAfterWrite := range []bool{false, true} {
			t.Run(fmt.Sprintf("create-error-current-matches=%t", createErrorAfterWrite), func(t *testing.T) {
				sch := &weeklyAtomicScheduler{
					xml:                   map[string][]byte{},
					failCreateCount:       boolToInt(createErrorAfterWrite),
					createErrorAfterWrite: createErrorAfterWrite,
				}
				lockPath := installWeeklyAtomicHarness(t, sch)
				releaseCause := injectWeeklyUnlockFailure(t, lockPath).cause
				setWeeklyScheduleForAtomicTest(t, "weekly Sun 03:00")
				canonical := filepath.Join(t.TempDir(), "mcphub.exe")
				previousCanonical := testCanonicalMcphubPathOverride
				testCanonicalMcphubPathOverride = canonical
				t.Cleanup(func() { testCanonicalMcphubPathOverride = previousCanonical })

				restoreStatus, err := NewAPI().ApplyWeeklyRefreshSchedule(desiredSchedule)
				if !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, releaseCause) {
					t.Fatalf("ApplyWeeklyRefreshSchedule error = %v, want typed release class and cause", err)
				}
				if restoreStatus != "n/a" {
					t.Fatalf("ApplyWeeklyRefreshSchedule restore status = %q, want n/a", restoreStatus)
				}
				persisted, getErr := NewAPI().SettingsGet("daemons.weekly_schedule")
				if getErr != nil || persisted != "weekly Tue 14:30" {
					t.Fatalf("persisted weekly schedule = %q err=%v, want requested schedule", persisted, getErr)
				}
				current, exportErr := sch.ExportXML(WeeklyRefreshTaskName)
				if exportErr != nil {
					t.Fatal(exportErr)
				}
				want := weeklyRefreshTaskSpec(canonical, desiredSchedule)
				matches, classifyErr := weeklyTaskXMLMatchesSpec(current, want)
				if classifyErr != nil || !matches {
					t.Fatalf("requested settings/task pair diverged: matches=%t err=%v xml=%s", matches, classifyErr, current)
				}
				before := sch.operations()
				if err := NewAPI().EnsureWeeklyRefreshTask(); !errors.Is(err, ErrLockReleaseUnconfirmed) {
					t.Fatalf("later EnsureWeeklyRefreshTask error = %v, want retained-lock fail-fast", err)
				}
				if after := sch.operations(); after != before {
					t.Fatalf("later acquire reached scheduler: before=%d after=%d", before, after)
				}
			})
		}
	})
}

func TestApplyWeeklyRefreshSchedule_RestoresPriorPairAfterPostCreateVerificationFailure(t *testing.T) {
	priorSchedule := &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 0, Hour: 3, Minute: 0}
	desiredSchedule := &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 2, Hour: 14, Minute: 30}

	for _, tt := range []struct {
		name       string
		failExport error
		override   []byte
	}{
		{name: "export-error", failExport: errors.New("injected post-create export failure")},
		{name: "unclassifiable-export", override: []byte("<Task><invalid>")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sch := &weeklyAtomicScheduler{
				xml:                  map[string][]byte{},
				failExportOnCall:     map[int]error{2: tt.failExport},
				exportOverrideOnCall: map[int][]byte{2: tt.override},
			}
			installWeeklyAtomicHarness(t, sch)
			originalSettings := setWeeklyScheduleForAtomicTest(t, "weekly Sun 03:00")
			canonical := filepath.Join(t.TempDir(), "mcphub.exe")
			previousCanonical := testCanonicalMcphubPathOverride
			testCanonicalMcphubPathOverride = canonical
			t.Cleanup(func() { testCanonicalMcphubPathOverride = previousCanonical })
			priorSpec := weeklyRefreshTaskSpec(canonical, priorSchedule)
			sch.xml[WeeklyRefreshTaskName] = weeklyTaskXMLForSpec(priorSpec)

			restoreStatus, err := NewAPI().ApplyWeeklyRefreshSchedule(desiredSchedule)
			if err == nil {
				t.Fatal("ApplyWeeklyRefreshSchedule succeeded after post-create verification failure")
			}
			if restoreStatus != "ok" {
				t.Fatalf("restore status = %q, want ok; err=%v", restoreStatus, err)
			}
			settings, readErr := os.ReadFile(SettingsPath())
			if readErr != nil {
				t.Fatalf("read restored settings: %v", readErr)
			}
			if !bytes.Equal(settings, originalSettings) {
				t.Fatalf("settings were not restored:\nbefore=%q\nafter=%q", originalSettings, settings)
			}
			current, exportErr := sch.ExportXML(WeeklyRefreshTaskName)
			if exportErr != nil {
				t.Fatalf("export restored task: %v", exportErr)
			}
			if !bytes.Equal(current, sch.xml[WeeklyRefreshTaskName]) || !bytes.Equal(current, weeklyTaskXMLForSpec(priorSpec)) {
				t.Fatalf("task was not restored to the prior generation: %s", current)
			}
		})
	}
}

func TestWeeklyScheduleSettingsReleaseSettlementMatrix(t *testing.T) {
	desiredSchedule := &ScheduleSpec{Kind: ScheduleWeekly, DayOfWeek: 2, Hour: 14, Minute: 30}

	t.Run("successful combined Apply reports settings release failure after the requested pair settles", func(t *testing.T) {
		sch := &weeklyAtomicScheduler{xml: map[string][]byte{}}
		installWeeklyAtomicHarness(t, sch)
		setWeeklyScheduleForAtomicTest(t, "weekly Sun 03:00")
		canonical := filepath.Join(t.TempDir(), "mcphub.exe")
		previousCanonical := testCanonicalMcphubPathOverride
		testCanonicalMcphubPathOverride = canonical
		t.Cleanup(func() { testCanonicalMcphubPathOverride = previousCanonical })

		settingsLockPath := SettingsPath() + ".lock"
		release := injectLedgeredFlockUnlockFailure(t, settingsLockPath, "settings")
		restoreStatus, err := NewAPI().ApplyWeeklyRefreshSchedule(desiredSchedule)
		if !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, release.cause) {
			t.Fatalf("ApplyWeeklyRefreshSchedule error = %v, want typed settings release class and cause", err)
		}
		if restoreStatus != "n/a" {
			t.Fatalf("ApplyWeeklyRefreshSchedule restore status = %q, want n/a", restoreStatus)
		}
		if release.attempts != 1 {
			t.Fatalf("settings unlock attempts = %d, want 1", release.attempts)
		}

		persistedBytes, readErr := os.ReadFile(SettingsPath())
		if readErr != nil {
			t.Fatal(readErr)
		}
		var persisted map[string]string
		if unmarshalErr := yaml.Unmarshal(persistedBytes, &persisted); unmarshalErr != nil {
			t.Fatal(unmarshalErr)
		}
		if persisted["daemons.weekly_schedule"] != "weekly Tue 14:30" {
			t.Fatalf("persisted weekly schedule = %q, want requested schedule", persisted["daemons.weekly_schedule"])
		}
		current, exportErr := sch.ExportXML(WeeklyRefreshTaskName)
		if exportErr != nil {
			t.Fatal(exportErr)
		}
		matches, classifyErr := weeklyTaskXMLMatchesSpec(current, weeklyRefreshTaskSpec(canonical, desiredSchedule))
		if classifyErr != nil || !matches {
			t.Fatalf("requested settings/task pair diverged: matches=%t err=%v xml=%s", matches, classifyErr, current)
		}

		beforeOperations := sch.operations()
		if ensureErr := NewAPI().EnsureWeeklyRefreshTask(); !errors.Is(ensureErr, ErrLockReleaseUnconfirmed) {
			t.Fatalf("later EnsureWeeklyRefreshTask error = %v, want retained settings-lock fail-fast", ensureErr)
		}
		if afterOperations := sch.operations(); afterOperations != beforeOperations {
			t.Fatalf("later EnsureWeeklyRefreshTask reached scheduler: before=%d after=%d", beforeOperations, afterOperations)
		}
		callbackEntries := 0
		if laterErr := NewAPI().withWeeklyScheduleSettings(weeklyScheduleSettingsRead, "", func(string, func() error) error {
			callbackEntries++
			return nil
		}); !errors.Is(laterErr, ErrLockReleaseUnconfirmed) {
			t.Fatalf("later settings callback error = %v, want retained settings-lock fail-fast", laterErr)
		}
		if callbackEntries != 0 {
			t.Fatalf("later settings callback entries = %d, want 0", callbackEntries)
		}
	})

	t.Run("callback failure after rollback remains primary and joins settings release failure", func(t *testing.T) {
		original := setWeeklyScheduleForAtomicTest(t, "weekly Sun 03:00")
		settingsLockPath := SettingsPath() + ".lock"
		release := injectLedgeredFlockUnlockFailure(t, settingsLockPath, "settings")
		callbackCause := errors.New("injected weekly settings callback failure")
		callbackEntries := 0
		err := NewAPI().withWeeklyScheduleSettings(weeklyScheduleSettingsUpdate, "weekly Tue 14:30", func(current string, rollback func() error) error {
			callbackEntries++
			if current != "weekly Sun 03:00" {
				t.Fatalf("callback current weekly schedule = %q, want prior schedule", current)
			}
			if rollbackErr := rollback(); rollbackErr != nil {
				t.Fatalf("rollback: %v", rollbackErr)
			}
			return callbackCause
		})
		if !errors.Is(err, callbackCause) || !errors.Is(err, ErrLockReleaseUnconfirmed) || !errors.Is(err, release.cause) {
			t.Fatalf("settings callback error = %v, want callback and typed release causes", err)
		}
		if callbackEntries != 1 || release.attempts != 1 {
			t.Fatalf("callback entries/unlock attempts = %d/%d, want 1/1", callbackEntries, release.attempts)
		}
		after, readErr := os.ReadFile(SettingsPath())
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(after, original) {
			t.Fatalf("callback rollback did not restore exact settings bytes:\nbefore=%q\nafter=%q", original, after)
		}

		replacementCallbackEntries := 0
		if laterErr := NewAPI().withWeeklyScheduleSettings(weeklyScheduleSettingsRead, "", func(string, func() error) error {
			replacementCallbackEntries++
			return nil
		}); !errors.Is(laterErr, ErrLockReleaseUnconfirmed) {
			t.Fatalf("later settings callback error = %v, want retained settings-lock fail-fast", laterErr)
		}
		if replacementCallbackEntries != 0 {
			t.Fatalf("replacement callback entries = %d, want 0", replacementCallbackEntries)
		}
	})
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
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

//go:build windows && windows_console_integration && test_state_path_env

package e2e

import (
	"bytes"
	"context"
	buildinfo "debug/buildinfo"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	runtimedebug "runtime/debug"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	hubapi "mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/clients"
	hubgui "mcp-local-hub/internal/gui"
	hubprocess "mcp-local-hub/internal/process"
)

const (
	w18StateOverrideEnv  = "MCPHUB_STATE_DIR_OVERRIDE"
	w18ReadyTimeout      = 12 * time.Second
	w18ExitTimeout       = 10 * time.Second
	w18TrayQuitSignalEnv = "MCPHUB_TEST_TRAY_QUIT_SIGNAL"
	w18TerminalHelperEnv = "MCPHUB_W18_TERMINAL_HELPER"
	w18TerminalNodeEnv   = "MCPHUB_W18_TERMINAL_NODE"
	w18TerminalShimEnv   = "MCPHUB_W18_TERMINAL_SHIM"
	w18TerminalMarkerEnv = "MCPHUB_W18_TERMINAL_MARKER"
)

var w18IsWindow = user32.NewProc("IsWindow")

type w18ProcessRow struct {
	PID    int    `json:"pid"`
	Parent int    `json:"parent"`
	Image  string `json:"image"`
	Path   string `json:"path"`
}

type w18ProcessLedger struct {
	mu   sync.Mutex
	rows map[int]w18ProcessRow
}

type w18Fixture struct {
	root           string
	stateRoot      string
	configRoot     string
	pidport        string
	quitSignal     string
	candidate      string
	node           string
	shim           string
	port           int
	env            []string
	ownedRoots     []*w18OwnedRoot
	processLedger  *w18ProcessLedger
	consoleHWND    uintptr
	consoleMembers []uint32
}

type w18OwnedRoot struct {
	pid      int
	path     string
	handle   windows.Handle
	released bool
}

type w18Evidence struct {
	Schema            int             `json:"schema"`
	Complete          bool            `json:"complete"`
	Candidate         string          `json:"candidate_basename"`
	PromptReturned    bool            `json:"prompt_returned"`
	IncumbentReused   bool            `json:"incumbent_reused"`
	CallerConsoleLive bool            `json:"caller_console_live"`
	TerminalClosed    bool            `json:"terminal_closed_with_gui_live"`
	GUI               int             `json:"gui_pid"`
	Supervisor        int             `json:"supervisor_pid"`
	Tray              int             `json:"tray_pid"`
	Rows              []w18ProcessRow `json:"rows"`
}

type w18TerminalMarker struct {
	PID         int      `json:"pid"`
	ConsoleHWND uintptr  `json:"console_hwnd"`
	ConsolePIDs []uint32 `json:"console_pids"`
}

func TestW18ExactTestStateBuildTags(t *testing.T) {
	for _, tc := range []struct {
		name    string
		info    *runtimedebug.BuildInfo
		wantErr bool
	}{
		{name: "exact", info: &runtimedebug.BuildInfo{Settings: []runtimedebug.BuildSetting{{Key: "-tags", Value: "test_state_path_env"}}}},
		{name: "missing build info", info: nil, wantErr: true},
		{name: "missing tags", info: &runtimedebug.BuildInfo{}, wantErr: true},
		{name: "empty tags", info: &runtimedebug.BuildInfo{Settings: []runtimedebug.BuildSetting{{Key: "-tags", Value: ""}}}, wantErr: true},
		{name: "production tagless", info: &runtimedebug.BuildInfo{Settings: []runtimedebug.BuildSetting{{Key: "-buildmode", Value: "exe"}}}, wantErr: true},
		{name: "inline baseline", info: &runtimedebug.BuildInfo{Settings: []runtimedebug.BuildSetting{{Key: "-tags", Value: "test_state_path_env,w18_inline_baseline"}}}, wantErr: true},
		{name: "additional custom tag", info: &runtimedebug.BuildInfo{Settings: []runtimedebug.BuildSetting{{Key: "-tags", Value: "netgo,test_state_path_env"}}}, wantErr: true},
		{name: "duplicate exact tag", info: &runtimedebug.BuildInfo{Settings: []runtimedebug.BuildSetting{{Key: "-tags", Value: "test_state_path_env,test_state_path_env"}}}, wantErr: true},
		{name: "multiple tag settings", info: &runtimedebug.BuildInfo{Settings: []runtimedebug.BuildSetting{{Key: "-tags", Value: "test_state_path_env"}, {Key: "-tags", Value: "test_state_path_env"}}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireW18ExactTestStateBuildTags(tc.info)
			if (err != nil) != tc.wantErr {
				t.Fatalf("requireW18ExactTestStateBuildTags error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}

func TestW18CandidateLocationRequiresStrictPrivateRoot(t *testing.T) {
	parent := t.TempDir()
	privateRoot := filepath.Join(parent, "o7-product-run")
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(privateRoot, "mcphub.exe")
	outside := filepath.Join(parent, "foreign.exe")
	for _, path := range []string{inside, outside} {
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, candidate string
		wantErr         bool
	}{
		{name: "strict child", candidate: inside},
		{name: "equal root", candidate: privateRoot, wantErr: true},
		{name: "sibling", candidate: outside, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := requireW18CandidateBeneathPrivateRoot(tc.candidate, privateRoot)
			if (err != nil) != tc.wantErr {
				t.Fatalf("requireW18CandidateBeneathPrivateRoot(%q) error = %v, wantErr %t", tc.candidate, err, tc.wantErr)
			}
		})
	}
}

func TestW18RepositoryFixtureSourcesResolveOutsidePackageWorkingDirectory(t *testing.T) {
	repositoryRoot, err := w18RepositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		filepath.Join("npm", "bin", "cli.js"),
		filepath.Join("npm", "lib", "platform-binary.js"),
	} {
		info, err := os.Stat(filepath.Join(repositoryRoot, relative))
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("repository fixture source %s is not a regular file: %v", relative, err)
		}
	}
}

func w18RepositoryRoot() (string, error) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok || sourceFile == "" {
		return "", errors.New("resolve W18 repository source file")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	marker, err := os.Stat(filepath.Join(repositoryRoot, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("resolve W18 repository root marker: %w", err)
	}
	if !marker.Mode().IsRegular() {
		return "", errors.New("resolve W18 repository root marker: go.mod is not a regular file")
	}
	return repositoryRoot, nil
}

func requireW18ExactTestStateBuildTags(info *runtimedebug.BuildInfo) error {
	if info == nil {
		return errors.New("W18 candidate build info is missing")
	}
	found := false
	for _, setting := range info.Settings {
		if setting.Key != "-tags" {
			continue
		}
		if found {
			return errors.New("W18 candidate has multiple embedded -tags settings")
		}
		found = true
		tags := strings.Split(setting.Value, ",")
		if len(tags) != 1 || strings.TrimSpace(tags[0]) != "test_state_path_env" {
			return fmt.Errorf("W18 candidate embedded -tags must equal exactly test_state_path_env, got %q", setting.Value)
		}
	}
	if !found {
		return errors.New("W18 candidate has no embedded -tags setting")
	}
	return nil
}

func requireW18CandidateBeneathPrivateRoot(candidate, privateRoot string) error {
	if candidate == "" || privateRoot == "" || !filepath.IsAbs(candidate) || !filepath.IsAbs(privateRoot) ||
		filepath.Clean(candidate) != candidate || filepath.Clean(privateRoot) != privateRoot {
		return errors.New("W18 candidate and private build root must be clean absolute paths")
	}
	resolvedRoot, err := filepath.EvalSymlinks(privateRoot)
	if err != nil {
		return fmt.Errorf("resolve W18 private build root: %w", err)
	}
	rootInfo, err := os.Stat(resolvedRoot)
	if err != nil {
		return fmt.Errorf("W18 private build root is not an existing directory: %w", err)
	}
	if !rootInfo.IsDir() {
		return errors.New("W18 private build root is not an existing directory")
	}
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return fmt.Errorf("resolve W18 candidate: %w", err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedCandidate)
	if err != nil || rel == "." || rel == "" || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("W18 candidate is not strictly beneath the private build root")
	}
	return nil
}

type w18IsolatedProductIdentity struct {
	artifact binaryadmission.WindowsArtifact
	settings []runtimedebug.BuildSetting
}

func readW18IsolatedProductIdentity(candidate, privateRoot string) (w18IsolatedProductIdentity, error) {
	if err := requireW18CandidateBeneathPrivateRoot(candidate, privateRoot); err != nil {
		return w18IsolatedProductIdentity{}, err
	}
	artifact, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{
		Path: candidate,
		Role: binaryadmission.WindowsArtifactRoleProduct,
	})
	if err != nil {
		return w18IsolatedProductIdentity{}, fmt.Errorf("W18 Product admission: %w", err)
	}
	info, err := buildinfo.ReadFile(candidate)
	if err != nil {
		return w18IsolatedProductIdentity{}, fmt.Errorf("read W18 candidate build info: %w", err)
	}
	if err := requireW18ExactTestStateBuildTags(info); err != nil {
		return w18IsolatedProductIdentity{}, err
	}
	return w18IsolatedProductIdentity{
		artifact: artifact,
		settings: append([]runtimedebug.BuildSetting(nil), info.Settings...),
	}, nil
}

func requireW18StagedProductIdentity(source, staged w18IsolatedProductIdentity) error {
	if source.artifact.Role != staged.artifact.Role ||
		source.artifact.Version != staged.artifact.Version ||
		source.artifact.Commit != staged.artifact.Commit ||
		source.artifact.BuildDate != staged.artifact.BuildDate ||
		source.artifact.SHA256 != staged.artifact.SHA256 ||
		source.artifact.Machine != staged.artifact.Machine ||
		source.artifact.Subsystem != staged.artifact.Subsystem {
		return errors.New("staged W18 Product identity differs from source")
	}
	if !reflect.DeepEqual(source.settings, staged.settings) {
		return errors.New("staged W18 Product embedded build settings differ from source")
	}
	return nil
}

func TestWindowsNPMBareLaunchPromptLifecycle(t *testing.T) {
	if os.Getenv(w18TerminalHelperEnv) == "1" {
		runW18TerminalHelper()
		return
	}
	fixture := newW18Fixture(t)
	defer fixture.cleanup(t)

	terminal, first, firstRows, terminalMarker := fixture.runNPMBareFromOwnedConsole(t, w18ReadyTimeout)

	guiPID, actualPort, err := waitW18GUIReady(fixture.pidport, 0, w18ReadyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	fixture.port = actualPort
	owner, err := waitW18SupervisorOwner(fixture.stateRoot, w18ReadyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	mergeW18RowsUntil(firstRows, 2*time.Second, func(rows map[int]w18ProcessRow) bool {
		for pid, row := range rows {
			if pid != owner.PID && row.Parent == guiPID && samePath(row.Path, fixture.candidate) {
				return true
			}
		}
		return false
	})
	roles, err := requireW18Topology(first.nodePID, guiPID, owner.PID, firstRows, fixture.candidate)
	if err != nil {
		t.Fatal(err)
	}
	fixture.retainRoot(t, guiPID, fixture.candidate)
	fixture.retainRoot(t, owner.PID, fixture.candidate)
	if err := terminal.terminateAndWait(5 * time.Second); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("close exact disposable initiating terminal: %v", err)
		}
	}
	if err := waitW18WindowGone(terminalMarker.ConsoleHWND, 5*time.Second); err != nil {
		t.Fatalf("initiating terminal window remained after owner exit: %v", err)
	}
	if !w18PIDLive(guiPID) || !w18PIDLive(owner.PID) {
		t.Fatalf("isolated GUI/supervisor did not survive initiating terminal close: gui=%d live=%v supervisor=%d live=%v", guiPID, w18PIDLive(guiPID), owner.PID, w18PIDLive(owner.PID))
	}
	if afterGUI, afterPort, readyErr := waitW18GUIReady(fixture.pidport, fixture.port, 2*time.Second); readyErr != nil || afterGUI != guiPID || afterPort != fixture.port {
		t.Fatalf("GUI readiness changed after initiating terminal close: pid=%d port=%d err=%v", afterGUI, afterPort, readyErr)
	}

	second, _ := fixture.runNPMBare(t, 5*time.Second)
	if second.waitErr != nil || second.exit != 0 {
		t.Fatalf("incumbent bare npm invocation did not return cleanly: exit=%d wait=%v stderr=%q", second.exit, second.waitErr, second.stderr)
	}
	secondGUI, _, err := waitW18GUIReady(fixture.pidport, fixture.port, 2*time.Second)
	if err != nil || secondGUI != guiPID {
		t.Fatalf("incumbent GUI identity changed: before=%d after=%d err=%v", guiPID, secondGUI, err)
	}
	if err := os.WriteFile(fixture.quitSignal, []byte("quit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitW18PIDsGone([]int{guiPID, roles.tray, owner.PID}, w18ExitTimeout); err != nil {
		t.Fatalf("tray Quit did not settle GUI-owned processes: %v", err)
	}
	currentMembers := currentConsolePIDs()
	if fixture.consoleHWND == 0 || fixture.consoleHWND != currentConsoleWindow() || !containsPID(currentMembers, os.Getpid()) {
		t.Fatalf("test caller console ownership changed: hwnd before=%d after=%d caller=%d members before=%v after=%v", fixture.consoleHWND, currentConsoleWindow(), os.Getpid(), fixture.consoleMembers, currentMembers)
	}

	evidence := w18Evidence{
		Schema:            1,
		Complete:          true,
		Candidate:         filepath.Base(fixture.candidate),
		PromptReturned:    true,
		IncumbentReused:   true,
		CallerConsoleLive: true,
		TerminalClosed:    true,
		GUI:               guiPID,
		Supervisor:        owner.PID,
		Tray:              roles.tray,
		Rows:              sortedW18Rows(firstRows),
	}
	writeW18Evidence(t, fixture.root, evidence)
}

func TestWindowsNPMBareLaunchFailureOwnsCleanup(t *testing.T) {
	fixture := newW18Fixture(t)
	defer fixture.cleanup(t)

	// Hold only the isolated fixture's GUI lease with this non-mcphub test
	// process. The initial activation cannot authenticate it, the spawned GUI
	// cannot acquire it, and the coordinator must visibly fail after settling
	// only the newly-created same-image GUI tree.
	blocker, err := hubgui.AcquireSingleInstanceAt(fixture.pidport, fixture.port)
	if err != nil {
		t.Fatalf("acquire isolated failure lease: %v", err)
	}
	blockerReleased := false
	defer func() {
		if !blockerReleased {
			blocker.Release()
		}
	}()

	result, rows := fixture.runNPMBare(t, 25*time.Second)
	if result.waitErr != nil {
		t.Fatalf("failed bare launch did not return to npm caller: %v", result.waitErr)
	}
	if result.exit == 0 || !strings.Contains(result.stderr, "E_WINDOWS_BARE_GUI_LAUNCH") {
		t.Fatalf("failed bare launch was not visible and typed: exit=%d stderr=%q", result.exit, result.stderr)
	}
	coordinator := findW18DirectChildImage(result.nodePID, rows, "mcphub.exe")
	if coordinator == 0 || findW18DirectChildImage(coordinator, rows, "mcphub.exe") == 0 {
		t.Fatal("forced post-admission failure did not reach the real same-image GUI child")
	}
	if err := waitW18FixtureProcessesGone(fixture, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if unheld, probeErr := hubgui.ProbeSingleInstanceLockUnheld(fixture.pidport); probeErr != nil || unheld {
		t.Fatalf("failure cleanup disturbed the independently-held isolated lease: unheld=%v err=%v", unheld, probeErr)
	}
	blocker.Release()
	blockerReleased = true
}

func (f *w18Fixture) runNPMBareFromOwnedConsole(t *testing.T, timeout time.Duration) (*ownedCommand, w18RunResult, map[int]w18ProcessRow, w18TerminalMarker) {
	t.Helper()
	helperPath, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve W18 terminal helper: %v", err)
	}
	marker := filepath.Join(f.root, "fresh-prompt-return.marker")
	_ = os.Remove(marker)
	cmd := exec.Command(helperPath, "-test.run=^TestWindowsNPMBareLaunchPromptLifecycle$")
	cmd.Dir = filepath.Dir(filepath.Dir(f.shim))
	cmd.Env = append(append([]string(nil), f.env...),
		w18TerminalHelperEnv+"=1",
		w18TerminalNodeEnv+"="+f.node,
		w18TerminalShimEnv+"="+f.shim,
		w18TerminalMarkerEnv+"="+marker,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000010}
	terminal, err := startOwnedCommand(cmd)
	if err != nil {
		t.Fatalf("start disposable initiating console: %v", err)
	}
	ledger := &w18ProcessLedger{rows: make(map[int]w18ProcessRow)}
	stop := make(chan struct{})
	done := make(chan struct{})
	go ledger.poll(stop, done)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if !terminal.live() {
			close(stop)
			<-done
			waitErr := terminal.wait(time.Second)
			t.Fatalf("disposable initiating terminal exited before prompt marker: %v", waitErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		close(stop)
		<-done
		rows := ledger.snapshot()
		nodePID := findW18DirectChildImage(cmd.Process.Pid, rows, "node.exe")
		f.retainDiscoveredRoot(t, nodePID, rows)
		if owner, ownerErr := waitW18SupervisorOwner(f.stateRoot, 2*time.Second); ownerErr == nil {
			f.retainRoot(t, owner.PID, f.candidate)
		}
		terminal.cleanup()
		t.Fatalf("fresh bare npm invocation did not return from disposable terminal within %s", timeout)
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	<-done
	rows := ledger.snapshot()
	nodePID := findW18DirectChildImage(cmd.Process.Pid, rows, "node.exe")
	if nodePID == 0 {
		terminal.cleanup()
		t.Fatal("disposable terminal process ledger did not capture its Node child")
	}
	f.retainDiscoveredRoot(t, nodePID, rows)
	if owner, ownerErr := waitW18SupervisorOwner(f.stateRoot, 2*time.Second); ownerErr == nil {
		f.retainRoot(t, owner.PID, f.candidate)
	}
	markerBytes, err := os.ReadFile(marker)
	var terminalMarker w18TerminalMarker
	if err != nil || json.Unmarshal(markerBytes, &terminalMarker) != nil {
		terminal.cleanup()
		t.Fatalf("read disposable terminal marker: %v", err)
	}
	if terminalMarker.PID != cmd.Process.Pid || terminalMarker.ConsoleHWND == 0 || !containsPID(terminalMarker.ConsolePIDs, cmd.Process.Pid) {
		terminal.cleanup()
		t.Fatalf("CREATE_NEW_CONSOLE marker does not bind the helper console: %+v helper=%d", terminalMarker, cmd.Process.Pid)
	}
	return terminal, w18RunResult{nodePID: nodePID, exit: 0}, rows, terminalMarker
}

func runW18TerminalHelper() {
	node := os.Getenv(w18TerminalNodeEnv)
	shim := os.Getenv(w18TerminalShimEnv)
	marker := os.Getenv(w18TerminalMarkerEnv)
	if node == "" || shim == "" || marker == "" {
		os.Exit(2)
	}
	cmd := exec.Command(node, shim)
	cmd.Env = os.Environ()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(3)
	}
	record, err := json.Marshal(w18TerminalMarker{PID: os.Getpid(), ConsoleHWND: currentConsoleWindow(), ConsolePIDs: currentConsolePIDs()})
	if err != nil {
		os.Exit(4)
	}
	if err := os.WriteFile(marker, append(record, '\n'), 0o600); err != nil {
		os.Exit(4)
	}
	select {}
}

type w18RunResult struct {
	nodePID int
	exit    int
	waitErr error
	stderr  string
}

func (f *w18Fixture) runNPMBare(t *testing.T, timeout time.Duration) (w18RunResult, map[int]w18ProcessRow) {
	t.Helper()
	cmd := exec.Command(f.node, f.shim)
	cmd.Dir = filepath.Dir(filepath.Dir(f.shim))
	cmd.Env = append([]string(nil), f.env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &bytes.Buffer{}
	owned, err := startOwnedCommand(cmd)
	if err != nil {
		t.Fatalf("start isolated npm shim: %v", err)
	}
	ledger := &w18ProcessLedger{rows: make(map[int]w18ProcessRow)}
	stop := make(chan struct{})
	done := make(chan struct{})
	go ledger.poll(stop, done)
	waitErr := owned.wait(timeout)
	close(stop)
	<-done
	rows := ledger.snapshot()
	f.retainDiscoveredRoot(t, cmd.Process.Pid, rows)
	if owner, ownerErr := waitW18SupervisorOwner(f.stateRoot, 2*time.Second); ownerErr == nil {
		f.retainRoot(t, owner.PID, f.candidate)
	}
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		owned.cleanup()
		return w18RunResult{nodePID: cmd.Process.Pid, exit: -1, waitErr: waitErr, stderr: stderr.String()}, rows
	}
	return w18RunResult{nodePID: cmd.Process.Pid, exit: exitCode(owned.waitError), stderr: stderr.String()}, rows
}

func newW18Fixture(t *testing.T) *w18Fixture {
	t.Helper()
	if strings.TrimSpace(*candidateFlag) == "" || strings.TrimSpace(*scratchFlag) == "" {
		t.Fatal("W18 composition test requires -candidate and -scratch")
	}
	if !filepath.IsAbs(*candidateFlag) || !filepath.IsAbs(*scratchFlag) {
		t.Fatal("W18 composition test requires absolute -candidate and -scratch paths")
	}
	candidate := filepath.Clean(*candidateFlag)
	privateRoot := filepath.Clean(*scratchFlag)
	repositoryRoot, err := w18RepositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	sourceIdentity, err := readW18IsolatedProductIdentity(candidate, privateRoot)
	if err != nil {
		t.Fatalf("test source candidate preflight: %v", err)
	}
	root := filepath.Join(privateRoot, "w18-fixture")
	if err := os.RemoveAll(root); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clear caller-owned W18 fixture scratch: %v", err)
	}
	shim, err := stageW18NPMFixture(root, repositoryRoot, candidate)
	if err != nil {
		t.Fatal(err)
	}
	stagedBin := filepath.Join(root, "npm-fixture", "node_modules", "@applicate2628", "mcp-local-hub-win32-x64", "bin")
	candidate = filepath.Join(stagedBin, "mcphub.exe")
	stagedIdentity, err := readW18IsolatedProductIdentity(candidate, privateRoot)
	if err != nil {
		t.Fatalf("test staged candidate preflight: %v", err)
	}
	if err := requireW18StagedProductIdentity(sourceIdentity, stagedIdentity); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node.exe")
	if err != nil {
		t.Fatalf("node.exe availability: %v", err)
	}
	stateRoot := filepath.Join(root, "state")
	configRoot := filepath.Join(root, "config")
	quitSignal := filepath.Join(root, "tray-quit.signal")
	for _, dir := range []string{stateRoot, configRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := clients.PrepareClientConfigSandboxDirectories(configRoot); err != nil {
		t.Fatalf("prepare caller-owned W18 config namespace (fixture retained at %s): %v", root, err)
	}
	lease, port, err := reserveLoopback()
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Close()
	settings := filepath.Join(configRoot, "AppData", "Local", "mcp-local-hub", "gui-preferences.yaml")
	pidport := filepath.Join(filepath.Dir(settings), "gui.pidport")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(fmt.Sprintf("gui_server.port: \"%d\"\n", port)), 0o600); err != nil {
		t.Fatal(err)
	}
	env := w18SandboxEnvironment(configRoot, stateRoot)
	if _, err := os.Stat(pidport); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated pidport must be absent before launch: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "supervisor.lock.owner.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("isolated supervisor owner must be absent before launch: %v", err)
	}
	return &w18Fixture{
		root: root, stateRoot: stateRoot, configRoot: configRoot, pidport: pidport, quitSignal: quitSignal,
		candidate: candidate, node: node, shim: shim,
		port: port, env: env, consoleHWND: currentConsoleWindow(),
		consoleMembers: currentConsolePIDs(),
	}
}

func stageW18NPMFixture(root, repositoryRoot, candidate string) (string, error) {
	meta := filepath.Join(root, "npm-fixture")
	shim := filepath.Join(meta, "bin", "cli.js")
	lib := filepath.Join(meta, "lib", "platform-binary.js")
	pkgBin := filepath.Join(meta, "node_modules", "@applicate2628", "mcp-local-hub-win32-x64", "bin")
	for _, dir := range []string{filepath.Dir(shim), filepath.Dir(lib), pkgBin} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
	}
	for src, dst := range map[string]string{
		filepath.Join(repositoryRoot, "npm", "bin", "cli.js"):             shim,
		filepath.Join(repositoryRoot, "npm", "lib", "platform-binary.js"): lib,
		candidate: filepath.Join(pkgBin, "mcphub.exe"),
	} {
		data, err := os.ReadFile(src)
		if err != nil {
			return "", fmt.Errorf("read fixture source %s: %w", src, err)
		}
		if err := os.WriteFile(dst, data, 0o700); err != nil {
			return "", fmt.Errorf("write fixture target %s: %w", dst, err)
		}
	}
	return shim, nil
}

func w18SandboxEnvironment(configRoot, stateRoot string) []string {
	blocked := map[string]bool{
		w18StateOverrideEnv: true, hubgui.SuppressBrowserLaunchEnv: true,
		"MCPHUB_NO_AUTO_GUI": true,
	}
	for _, entry := range clients.ClientConfigSandboxEnvironment(configRoot) {
		blocked[strings.ToUpper(entry.Key)] = true
	}
	env := make([]string, 0, len(os.Environ())+20)
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if !blocked[strings.ToUpper(key)] {
			env = append(env, item)
		}
	}
	for _, entry := range clients.ClientConfigSandboxEnvironment(configRoot) {
		if !entry.Unset {
			env = append(env, entry.Key+"="+entry.Value)
		}
	}
	return append(env,
		w18StateOverrideEnv+"="+stateRoot,
		hubgui.SuppressBrowserLaunchEnv+"=1",
		w18TrayQuitSignalEnv+"="+filepath.Join(filepath.Dir(stateRoot), "tray-quit.signal"),
	)
}

func (l *w18ProcessLedger) poll(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		rows, _ := snapshotW18Processes()
		l.mu.Lock()
		for pid, row := range rows {
			if prior, ok := l.rows[pid]; ok && prior.Path != "" && row.Path == "" {
				row.Path = prior.Path
			}
			l.rows[pid] = row
		}
		l.mu.Unlock()
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
	}
}

func (l *w18ProcessLedger) snapshot() map[int]w18ProcessRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	copyRows := make(map[int]w18ProcessRow, len(l.rows))
	for pid, row := range l.rows {
		copyRows[pid] = row
	}
	return copyRows
}

func snapshotW18Processes() (map[int]w18ProcessRow, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	rows := make(map[int]w18ProcessRow)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	for {
		image := windows.UTF16ToString(entry.ExeFile[:])
		lower := strings.ToLower(image)
		if lower == "node.exe" || lower == "mcphub.exe" {
			pid := int(entry.ProcessID)
			rows[pid] = w18ProcessRow{PID: pid, Parent: int(entry.ParentProcessID), Image: image, Path: w18ProcessPath(pid)}
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, err
		}
	}
	return rows, nil
}

func mergeW18RowsUntil(dst map[int]w18ProcessRow, timeout time.Duration, ready func(map[int]w18ProcessRow) bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rows, err := snapshotW18Processes()
		if err == nil {
			for pid, row := range rows {
				if prior, ok := dst[pid]; ok && prior.Path != "" && row.Path == "" {
					row.Path = prior.Path
				}
				dst[pid] = row
			}
			if ready(dst) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func w18ProcessPath(pid int) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:size])
}

type w18Roles struct{ tray int }

func requireW18Topology(nodePID, guiPID, supervisorPID int, rows map[int]w18ProcessRow, candidate string) (w18Roles, error) {
	gui, ok := rows[guiPID]
	if !ok || !samePath(gui.Path, candidate) {
		return w18Roles{}, fmt.Errorf("GUI PID %d missing from isolated candidate ledger", guiPID)
	}
	coordinator, ok := rows[gui.Parent]
	if !ok || !samePath(coordinator.Path, candidate) || coordinator.Parent != nodePID {
		return w18Roles{}, fmt.Errorf("GUI parent is not the npm child same-image coordinator: %+v node=%d", coordinator, nodePID)
	}
	supervisor, ok := rows[supervisorPID]
	if !ok || supervisor.Parent != guiPID || !samePath(supervisor.Path, candidate) {
		return w18Roles{}, fmt.Errorf("supervisor PID %d is not an isolated GUI child: %+v", supervisorPID, supervisor)
	}
	trayPID := 0
	for pid, row := range rows {
		if pid != supervisorPID && row.Parent == guiPID && samePath(row.Path, candidate) {
			trayPID = pid
			break
		}
	}
	if trayPID == 0 {
		return w18Roles{}, fmt.Errorf("isolated tray child of GUI PID %d not observed", guiPID)
	}
	return w18Roles{tray: trayPID}, nil
}

func waitW18GUIReady(pidport string, expectedPort int, timeout time.Duration) (int, int, error) {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 250 * time.Millisecond}
	for time.Now().Before(deadline) {
		pid, recordedPort, err := hubgui.ReadPidport(pidport)
		if err == nil && pid > 0 && recordedPort > 0 && (expectedPort == 0 || recordedPort == expectedPort) {
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/api/ping", recordedPort), nil)
			resp, requestErr := client.Do(req)
			cancel()
			if requestErr == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return pid, recordedPort, nil
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, 0, fmt.Errorf("isolated GUI did not become ready (expected port %d)", expectedPort)
}

func waitW18SupervisorOwner(stateRoot string, timeout time.Duration) (hubapi.SupervisorLockOwner, error) {
	deadline := time.Now().Add(timeout)
	path := filepath.Join(stateRoot, "supervisor.lock")
	for time.Now().Before(deadline) {
		owner, err := hubapi.ReadSupervisorLockOwner(path)
		if err == nil && owner.PID > 0 && w18PIDLive(owner.PID) {
			return owner, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return hubapi.SupervisorLockOwner{}, errors.New("isolated supervisor owner did not appear")
}

func (f *w18Fixture) retainDiscoveredRoot(t *testing.T, nodePID int, rows map[int]w18ProcessRow) {
	t.Helper()
	coordinator := 0
	for pid, row := range rows {
		if row.Parent == nodePID && samePath(row.Path, f.candidate) {
			coordinator = pid
			break
		}
	}
	if coordinator == 0 {
		// Node and the coordinator can both return between 2 ms snapshots once
		// the GUI is ready. The admitted candidate path is unique to this fresh
		// fixture, so a live process at that exact path remains an owned root.
		for pid, row := range rows {
			if samePath(row.Path, f.candidate) {
				f.retainRoot(t, pid, f.candidate)
				return
			}
		}
		return
	}
	rootPID := coordinator
	for pid, row := range rows {
		if row.Parent == coordinator && samePath(row.Path, f.candidate) {
			rootPID = pid
			break
		}
	}
	f.retainRoot(t, rootPID, f.candidate)
}

func (f *w18Fixture) retainRoot(t *testing.T, pid int, expectedPath string) {
	t.Helper()
	for _, root := range f.ownedRoots {
		if root.pid == pid {
			return
		}
	}
	if !samePath(w18ProcessPath(pid), expectedPath) {
		if !w18PIDLive(pid) {
			return
		}
		t.Fatalf("refuse to retain PID %d: image path does not match %s", pid, expectedPath)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("retain exact owned PID %d: %v", pid, err)
	}
	f.ownedRoots = append(f.ownedRoots, &w18OwnedRoot{pid: pid, path: expectedPath, handle: h})
}

func (r *w18OwnedRoot) settle() error {
	if r == nil || r.released || r.handle == 0 {
		return nil
	}
	defer func() {
		windows.CloseHandle(r.handle)
		r.handle = 0
		r.released = true
	}()
	status, err := windows.WaitForSingleObject(r.handle, 0)
	if err != nil {
		return err
	}
	if status == uint32(windows.WAIT_OBJECT_0) {
		return nil
	}
	if status != uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("unexpected wait status %d for PID %d", status, r.pid)
	}
	if !samePath(w18ProcessPath(r.pid), r.path) {
		status, waitErr := windows.WaitForSingleObject(r.handle, 100)
		if waitErr == nil && status == uint32(windows.WAIT_OBJECT_0) {
			return nil
		}
		return fmt.Errorf("refuse exact-tree cleanup for PID %d: current image identity changed", r.pid)
	}
	if err := hubprocess.TreeKillByPID(r.pid); err != nil {
		return err
	}
	status, err = windows.WaitForSingleObject(r.handle, uint32(w18ExitTimeout/time.Millisecond))
	if err != nil || status != uint32(windows.WAIT_OBJECT_0) {
		return fmt.Errorf("owned PID %d did not settle: status=%d err=%v", r.pid, status, err)
	}
	return nil
}

func (f *w18Fixture) cleanup(t *testing.T) {
	t.Helper()
	for pass := 0; pass < 2; pass++ {
		if rows, err := snapshotW18Processes(); err == nil {
			for pid, row := range rows {
				if samePath(row.Path, f.candidate) {
					f.retainRoot(t, pid, f.candidate)
				}
			}
		}
		for i := len(f.ownedRoots) - 1; i >= 0; i-- {
			if err := f.ownedRoots[i].settle(); err != nil {
				t.Errorf("W18 exact-owned cleanup: %v", err)
			}
		}
	}
	if unheld, err := hubgui.ProbeSingleInstanceLockUnheld(f.pidport); err != nil || !unheld {
		t.Errorf("isolated GUI lock not released: unheld=%v err=%v", unheld, err)
	}
	if running, pid, err := hubapi.SupervisorRunningUnderStateDir(f.stateRoot); err != nil || running {
		t.Errorf("isolated supervisor residue: running=%v pid=%d err=%v", running, pid, err)
	}
}

func findW18DirectChildImage(root int, rows map[int]w18ProcessRow, image string) int {
	for pid, row := range rows {
		if row.Parent == root && strings.EqualFold(row.Image, image) {
			return pid
		}
	}
	return 0
}

func waitW18PIDsGone(pids []int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allGone := true
		for _, pid := range pids {
			allGone = allGone && !w18PIDLive(pid)
		}
		if allGone {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("PIDs still live after %s: %v", timeout, pids)
}

func waitW18WindowGone(hwnd uintptr, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		exists, _, _ := w18IsWindow.Call(hwnd)
		if exists == 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("console HWND %d still exists", hwnd)
}

func waitW18FixtureProcessesGone(fixture *w18Fixture, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rows, err := snapshotW18Processes()
		if err == nil {
			found := false
			for _, row := range rows {
				if samePath(row.Path, fixture.candidate) {
					found = true
					break
				}
			}
			if !found {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("isolated candidate processes remained after failed bare launch cleanup")
}

func w18PIDLive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	status, err := windows.WaitForSingleObject(h, 0)
	return err == nil && status == uint32(windows.WAIT_TIMEOUT)
}

func currentConsoleWindow() uintptr {
	hwnd, _, _ := procGetConsoleWindow.Call()
	return hwnd
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	return errA == nil && errB == nil && strings.EqualFold(filepath.Clean(aa), filepath.Clean(bb))
}

func sortedW18Rows(rows map[int]w18ProcessRow) []w18ProcessRow {
	out := make([]w18ProcessRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

func writeW18Evidence(t *testing.T, root string, evidence w18Evidence) {
	t.Helper()
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "w18-functional-evidence.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

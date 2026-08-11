//go:build windows && windows_console_integration

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/gofrs/flock"
	"golang.org/x/sys/windows"

	hubapi "mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/cli"
	hubgui "mcp-local-hub/internal/gui"
	hubprocess "mcp-local-hub/internal/process"
)

var (
	candidateFlag   = flag.String("candidate", "", "path to the isolated mcphub candidate")
	scratchFlag     = flag.String("scratch", "", "writable isolated evidence directory")
	processTestFlag = flag.String("process-test", "", "path to the precompiled process companion test binary")
)

const (
	helperModeEnv       = "MCPHUB_WINDOWS_CONTRACT_HELPER"
	helperWaitEnv       = "MCPHUB_WINDOWS_CONTRACT_WAIT"
	helperCandidateEnv  = "MCPHUB_WINDOWS_CONTRACT_CANDIDATE"
	helperStateRootEnv  = "MCPHUB_WINDOWS_CONTRACT_STATE_ROOT"
	helperPortEnv       = "MCPHUB_WINDOWS_CONTRACT_PORT"
	helperPIDEnv        = "MCPHUB_WINDOWS_CONTRACT_PID"
	helperAttachPathEnv = "MCPHUB_WINDOWS_CONTRACT_ATTACH_PIDPORT"
	helperAttachHashEnv = "MCPHUB_WINDOWS_CONTRACT_ATTACH_HASH"
)

//go:embed testdata/windows-console-contract/child-matrix.json
var childMatrixJSON []byte

var (
	kernel32                   = windows.NewLazySystemDLL("kernel32.dll")
	user32                     = windows.NewLazySystemDLL("user32.dll")
	procAttachConsole          = kernel32.NewProc("AttachConsole")
	procFreeConsole            = kernel32.NewProc("FreeConsole")
	procGetConsoleProcessList  = kernel32.NewProc("GetConsoleProcessList")
	procGetConsoleWindow       = kernel32.NewProc("GetConsoleWindow")
	procGetFileType            = kernel32.NewProc("GetFileType")
	procGetStdHandle           = kernel32.NewProc("GetStdHandle")
	procEnumWindows            = user32.NewProc("EnumWindows")
	procGetWindowThreadProcess = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible        = user32.NewProc("IsWindowVisible")
	procGetClassName           = user32.NewProc("GetClassNameW")
	windowProbeMu              sync.Mutex
	windowProbeRows            []windowRow
	windowProbeCallback        = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
		visible, _, _ := procIsWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1
		}
		var pid uint32
		procGetWindowThreadProcess.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		var class [256]uint16
		n, _, _ := procGetClassName.Call(hwnd, uintptr(unsafe.Pointer(&class[0])), uintptr(len(class)))
		windowProbeRows = append(windowProbeRows, windowRow{PID: pid, HWND: hwnd, Class: windows.UTF16ToString(class[:n])})
		return 1
	})
)

type commandResult struct {
	stdout            []byte
	stderr            []byte
	exit              int
	newConsoleWindows []windowRow
}

type windowRow struct {
	PID   uint32  `json:"pid"`
	HWND  uintptr `json:"hwnd"`
	Class string  `json:"class"`
}

type childReport struct {
	PID          int      `json:"pid"`
	ConsoleCount uintptr  `json:"console_count"`
	ConsolePIDs  []uint32 `json:"console_pids"`
	ConsoleHWND  uintptr  `json:"console_hwnd"`
	StdInValid   bool     `json:"stdin_valid"`
	StdOutValid  bool     `json:"stdout_valid"`
	StdErrValid  bool     `json:"stderr_valid"`
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type ownedCommand struct {
	cmd       *exec.Cmd
	signal    windows.Handle
	done      chan error
	waited    bool
	waitError error
}

type explicitResources struct {
	stateBase         string
	runRoot           string
	evidenceRoot      string
	attachRoot        string
	allocateRoot      string
	attachPort        int
	allocatePort      int
	attachLease       net.Listener
	allocateLease     net.Listener
	attachAdmission   stateAdmissionRecord
	allocateAdmission stateAdmissionRecord
	record            boundedEvidenceRecord
	topologyEvents    []string
	topologyAdmitted  bool
	cleaned           bool
	finalized         bool
}

type ownerClass string

const (
	ownerClassTokenUser             ownerClass = "token_user"
	ownerClassLocalSystem           ownerClass = "local_system"
	ownerClassBuiltinAdministrators ownerClass = "builtin_administrators"
	ownerClassOther                 ownerClass = "other"
)

type stateAdmissionRecord struct {
	Hardened        bool       `json:"hardened"`
	OwnerClass      ownerClass `json:"owner_class"`
	TokenOwnerClass ownerClass `json:"token_owner_class"`
	TokenUserMatch  bool       `json:"token_user_match"`
	WriteGateSafe   bool       `json:"write_gate_safe"`
	StrictEnv       bool       `json:"strict_env"`
}

type ownerClassificationRecord struct {
	Schema          int        `json:"schema"`
	Complete        bool       `json:"complete"`
	OwnerClass      ownerClass `json:"owner_class"`
	TokenOwnerClass ownerClass `json:"token_owner_class"`
	TokenUserMatch  bool       `json:"token_user_match"`
	Hardened        bool       `json:"hardened"`
	WriteGate       string     `json:"write_gate"`
	CleanupRemoved  bool       `json:"cleanup_removed"`
	CleanupAbsent   bool       `json:"cleanup_absent"`
}

type explicitCaseRecord struct {
	CaseID          string               `json:"case_id"`
	Admission       stateAdmissionRecord `json:"admission"`
	PID             int                  `json:"pid"`
	Port            int                  `json:"port"`
	Membership      []uint32             `json:"membership"`
	Waited          bool                 `json:"waited"`
	Signaled        bool                 `json:"signaled"`
	CorrelatedHWNDs int                  `json:"correlated_hwnds"`
	StateHash       string               `json:"state_hash"`
}

type boundedEvidenceRecord struct {
	Schema   int                  `json:"schema"`
	Complete bool                 `json:"complete"`
	Cases    []explicitCaseRecord `json:"cases"`
}

type allocationReadyReport struct {
	IntermediaryPID   int      `json:"intermediary_pid"`
	IntermediaryPIDs  []uint32 `json:"intermediary_console_pids"`
	CandidatePID      int      `json:"candidate_pid"`
	PidportPID        int      `json:"pidport_pid"`
	PidportPort       int      `json:"pidport_port"`
	AttachStateStable bool     `json:"attach_state_stable"`
	AttachStateFree   bool     `json:"attach_state_free"`
	StdoutObserved    bool     `json:"stdout_observed"`
}

type allocationSettlementReport struct {
	CandidatePID      int  `json:"candidate_pid"`
	CandidateWaited   bool `json:"candidate_waited"`
	CandidateSignaled bool `json:"candidate_signaled"`
}

type allocationOracleReport struct {
	OraclePID    int      `json:"oracle_pid"`
	CandidatePID int      `json:"candidate_pid"`
	Before       []uint32 `json:"before"`
	Attached     []uint32 `json:"attached"`
	After        []uint32 `json:"after"`
}

func candidatePath(t *testing.T) string {
	t.Helper()
	if strings.TrimSpace(*candidateFlag) == "" {
		t.Fatal("-candidate is required")
	}
	p, err := filepath.Abs(*candidateFlag)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("candidate %q is not a regular file: %v", p, err)
	}
	return p
}

func evidenceDir(t *testing.T) string {
	t.Helper()
	if strings.TrimSpace(*scratchFlag) == "" {
		t.Fatal("-scratch is required")
	}
	p, err := filepath.Abs(*scratchFlag)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func snapshotVisibleWindows() ([]windowRow, error) {
	windowProbeMu.Lock()
	defer windowProbeMu.Unlock()
	windowProbeRows = windowProbeRows[:0]
	result, _, callErr := procEnumWindows.Call(windowProbeCallback, 0)
	if result == 0 {
		if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
			return nil, errno
		}
	}
	return append([]windowRow(nil), windowProbeRows...), nil
}

func consoleWindowClass(class string) bool {
	class = strings.ToLower(class)
	return strings.Contains(class, "consolewindowclass") || strings.Contains(class, "cascadia")
}

func windowKey(row windowRow) string { return fmt.Sprintf("%d/%d", row.PID, row.HWND) }

func runCandidateObserved(t *testing.T, args ...string) commandResult {
	t.Helper()
	before, err := snapshotVisibleWindows()
	if err != nil {
		t.Fatal(err)
	}
	baseline := make(map[string]struct{}, len(before))
	for _, row := range before {
		baseline[windowKey(row)] = struct{}{}
	}
	cmd := exec.Command(candidatePath(t), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start candidate %q: %v", args, err)
	}
	observed := make(map[string]windowRow)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var waitErr error
	deadline := time.NewTimer(750 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case waitErr = <-done:
			goto finished
		case <-deadline.C:
			_ = cmd.Process.Kill()
			waitErr = <-done
			goto finished
		default:
			rows, snapErr := snapshotVisibleWindows()
			if snapErr != nil {
				_ = cmd.Process.Kill()
				<-done
				t.Fatal(snapErr)
			}
			for _, row := range rows {
				if _, old := baseline[windowKey(row)]; !old && consoleWindowClass(row.Class) {
					observed[windowKey(row)] = row
				}
			}
			time.Sleep(time.Millisecond)
		}
	}

finished:
	windowsFound := make([]windowRow, 0, len(observed))
	for _, row := range observed {
		windowsFound = append(windowsFound, row)
	}
	return commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes(), exit: exitCode(waitErr), newConsoleWindows: windowsFound}
}

func currentConsolePIDs() []uint32 {
	var pids [128]uint32
	count, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	used := int(count)
	if used > len(pids) {
		used = len(pids)
	}
	return append([]uint32(nil), pids[:used]...)
}

func containsPID(pids []uint32, pid int) bool {
	for _, candidate := range pids {
		if int(candidate) == pid {
			return true
		}
	}
	return false
}

func startLongCandidate(t *testing.T, detached bool, args ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(candidatePath(t), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if detached {
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008, HideWindow: true}
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start candidate %q: %v", args, err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
		if err == nil {
			status, waitErr := windows.WaitForSingleObject(h, 0)
			windows.CloseHandle(h)
			if waitErr == nil && status == uint32(windows.WAIT_TIMEOUT) {
				time.Sleep(300 * time.Millisecond)
				return cmd, &stdout, &stderr
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("candidate %q did not remain live (stderr=%q)", args, stderr.String())
	return nil, nil, nil
}

func attachProbeTarget(t *testing.T, pid int) (bool, []uint32, syscall.Errno) {
	t.Helper()
	freed, _, freeErr := procFreeConsole.Call()
	if freed == 0 {
		t.Fatalf("detach probe from parent console: %v", freeErr)
	}
	attached, _, callErr := procAttachConsole.Call(uintptr(uint32(pid)))
	var errno syscall.Errno
	if value, ok := callErr.(syscall.Errno); ok {
		errno = value
	}
	var pids []uint32
	if attached != 0 {
		pids = currentConsolePIDs()
		if detached, _, detachErr := procFreeConsole.Call(); detached == 0 {
			t.Fatalf("detach probe from target console: %v", detachErr)
		}
	}
	if restored, _, restoreErr := procAttachConsole.Call(uintptr(^uint32(0))); restored == 0 {
		t.Fatalf("restore probe parent console: %v", restoreErr)
	}
	return attached != 0, pids, errno
}

func TestWindowsSnapshotVisibleWindowsCallbackReuse(t *testing.T) {
	for i := 0; i < 4096; i++ {
		if _, err := snapshotVisibleWindows(); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
	}
}

func TestWindowsConsolePrefixGrammar(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantPolicy cli.WindowsConsolePolicy
		wantArgs   []string
	}{
		{"bare", []string{"mcphub"}, cli.WindowsConsoleDisabled, []string{"mcphub"}},
		{"exact", []string{"mcphub", cli.WindowsDebugConsolePrefix}, cli.WindowsConsoleDebugExplicit, []string{"mcphub"}},
		{"exact command", []string{"mcphub", cli.WindowsDebugConsolePrefix, "status", "--help"}, cli.WindowsConsoleDebugExplicit, []string{"mcphub", "status", "--help"}},
		{"after command", []string{"mcphub", "status", cli.WindowsDebugConsolePrefix}, cli.WindowsConsoleDisabled, []string{"mcphub", "status", cli.WindowsDebugConsolePrefix}},
		{"equals", []string{"mcphub", cli.WindowsDebugConsolePrefix + "=true"}, cli.WindowsConsoleDisabled, []string{"mcphub", cli.WindowsDebugConsolePrefix + "=true"}},
		{"terminator", []string{"mcphub", "--", cli.WindowsDebugConsolePrefix}, cli.WindowsConsoleDisabled, []string{"mcphub", "--", cli.WindowsDebugConsolePrefix}},
		{"duplicate", []string{"mcphub", cli.WindowsDebugConsolePrefix, cli.WindowsDebugConsolePrefix}, cli.WindowsConsoleDebugExplicit, []string{"mcphub", cli.WindowsDebugConsolePrefix}},
		{"case", []string{"mcphub", "--DEBUG-CONSOLE"}, cli.WindowsConsoleDisabled, []string{"mcphub", "--DEBUG-CONSOLE"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]string(nil), tc.args...)
			policy, normalized := cli.ResolveWindowsConsolePolicy(tc.args)
			if policy != tc.wantPolicy || !reflect.DeepEqual(normalized, tc.wantArgs) {
				t.Fatalf("policy/argv=(%v,%q), want (%v,%q)", policy, normalized, tc.wantPolicy, tc.wantArgs)
			}
			if !reflect.DeepEqual(tc.args, before) {
				t.Fatalf("input argv mutated: %q -> %q", before, tc.args)
			}
		})
	}
	direct := runCandidateObserved(t, "status", "--help")
	explicit := runCandidateObserved(t, cli.WindowsDebugConsolePrefix, "status", "--help")
	if direct.exit != explicit.exit || !bytes.Equal(direct.stdout, explicit.stdout) || !bytes.Equal(direct.stderr, explicit.stderr) {
		t.Fatalf("explicit prefix changed command output: direct=(%d,%q,%q) explicit=(%d,%q,%q)", direct.exit, direct.stdout, direct.stderr, explicit.exit, explicit.stdout, explicit.stderr)
	}
}

func TestWindowsDefaultLaunchNoConsole(t *testing.T) {
	evidenceDir(t)
	rows := []struct {
		name string
		args []string
	}{
		{"bare", []string{"gui", "--foreground", "--no-tray", "--no-browser", "--port", "0"}},
		{"gui", []string{"gui", "--foreground", "--no-tray", "--no-browser", "--port", "0"}},
		{"foreground", []string{"gui", "--foreground", "--no-browser", "--port", "0"}},
		{"no-tray", []string{"gui", "--no-tray", "--no-browser", "--port", "0"}},
		{"supervise", []string{"supervise"}},
		{"status", []string{"status"}},
		{"version", []string{"version"}},
		{"daemon", []string{"daemon", "--help"}},
		{"route", []string{"route", "--help"}},
		{"relay", []string{"relay", "--help"}},
		{"tray", []string{"tray", "--help"}},
		{"restart", []string{"restart", "--help"}},
		{"scheduler", []string{"scheduler", "--help"}},
		{"refresh", []string{"refresh", "--help"}},
		{"hidden workers", []string{"supervise", "--help"}},
		{"promotion rejection", []string{"upgrade", "--help"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			result := runCandidateObserved(t, row.args...)
			if len(result.newConsoleWindows) != 0 {
				t.Fatalf("ordinary invocation exposed console windows: %+v", result.newConsoleWindows)
			}
		})
	}
	cmd, _, _ := startLongCandidate(t, false, "gui", "--foreground", "--no-tray", "--no-browser", "--port", "0")
	attached, pids, errno := attachProbeTarget(t, cmd.Process.Pid)
	if attached {
		t.Fatalf("ordinary GUI candidate unexpectedly owns a console: %v", pids)
	}
	if errno == 0 {
		t.Fatal("ordinary GUI console probe failed without a Win32 error")
	}
}

func TestWindowsExplicitDebugConsole(t *testing.T) {
	switch os.Getenv(helperModeEnv) {
	case "allocation-launcher":
		if err := runAllocationLauncher(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	case "allocation-oracle":
		if err := runAllocationOracle(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
	resources := reserveExplicitResources(t)
	defer resources.finalize(t)
	resources.record = boundedEvidenceRecord{Schema: 1}
	attach := runExplicitAttachTransaction(t, resources)
	resources.record.Cases = append(resources.record.Cases, attach)
	if err := resources.allocateLease.Close(); err != nil {
		t.Fatalf("release allocation port reservation: %v", err)
	}
	resources.allocateLease = nil
	allocation := runExplicitAllocationTransaction(t, resources, attach.StateHash)
	resources.record.Cases = append(resources.record.Cases, allocation)
	resources.record.Complete = true
}

func envOverlay(base []string, set map[string]string, remove ...string) []string {
	drop := make(map[string]struct{}, len(set)+len(remove))
	for key := range set {
		drop[strings.ToUpper(key)] = struct{}{}
	}
	for _, key := range remove {
		drop[strings.ToUpper(key)] = struct{}{}
	}
	result := make([]string, 0, len(base)+len(set))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if _, found := drop[strings.ToUpper(key)]; ok && found {
			continue
		}
		result = append(result, entry)
	}
	for key, value := range set {
		result = append(result, key+"="+value)
	}
	return result
}

func candidateEnvironment(stateRoot string) []string {
	return envOverlay(os.Environ(), map[string]string{
		"LOCALAPPDATA":                    stateRoot,
		"MCPHUB_REQUIRE_SINGLE_USER_HOME": "1",
	},
		helperModeEnv, helperWaitEnv, helperCandidateEnv, helperStateRootEnv,
		helperPortEnv, helperPIDEnv, helperAttachPathEnv, helperAttachHashEnv,
		"MCPHUB_GUI_TEST_PIDPORT_DIR", "MCPHUB_STATE_DIR_OVERRIDE",
		"MCPHUB_ALLOW_UNHARDENED_STATE_WRITE", "MCPHUB_ALLOW_UNHARDENED_STATE_READ")
}

func strictCandidateEnvironment(env []string, stateRoot string) bool {
	values := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			values[strings.ToUpper(key)] = value
		}
	}
	if values["LOCALAPPDATA"] != stateRoot || values["MCPHUB_REQUIRE_SINGLE_USER_HOME"] != "1" {
		return false
	}
	for _, key := range []string{
		"MCPHUB_STATE_DIR_OVERRIDE", "MCPHUB_GUI_TEST_PIDPORT_DIR",
		"MCPHUB_ALLOW_UNHARDENED_STATE_WRITE", "MCPHUB_ALLOW_UNHARDENED_STATE_READ",
	} {
		if _, found := values[key]; found {
			return false
		}
	}
	return true
}

func reserveLoopback() (net.Listener, int, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return listener, listener.Addr().(*net.TCPAddr).Port, nil
}

func reserveExplicitResources(t *testing.T) *explicitResources {
	t.Helper()
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil || strings.TrimSpace(localAppData) == "" {
		t.Fatalf("resolve guest FOLDERID_LocalAppData: path=%q err=%v", localAppData, err)
	}
	evidence := evidenceDir(t)
	if err := runOwnerClassification(localAppData, evidence); err != nil {
		t.Fatalf("sanitized state-owner classification failed: %v", err)
	}
	return reserveExplicitResourcesAt(t, localAppData, evidence)
}

func reserveExplicitResourcesAt(t *testing.T, localAppData, evidence string) *explicitResources {
	t.Helper()
	stateBase := filepath.Join(localAppData, "mcp-local-hub-phase-e")
	if err := os.MkdirAll(stateBase, 0o700); err != nil {
		t.Fatalf("create Phase-E state base: %v", err)
	}
	if err := apitest.HardenedDirForTestMain(stateBase); err != nil {
		t.Fatalf("harden Phase-E state base: %v", err)
	}
	runRoot, err := os.MkdirTemp(stateBase, "run-")
	if err != nil {
		t.Fatal(err)
	}
	attachRoot := filepath.Join(runRoot, "attach", "localappdata")
	allocateRoot := filepath.Join(runRoot, "allocation", "localappdata")
	resources := &explicitResources{
		stateBase: stateBase, runRoot: runRoot, evidenceRoot: evidence,
		attachRoot: attachRoot, allocateRoot: allocateRoot,
		record:         boundedEvidenceRecord{Schema: 1},
		topologyEvents: []string{"harden:state_base", "create:run_root"},
	}
	t.Cleanup(func() { resources.finalize(t) })
	resources.topologyEvents = append(resources.topologyEvents, "cleanup:registered")
	components := []struct {
		label string
		path  string
	}{
		{"run_root", runRoot},
		{"attach_case", filepath.Join(runRoot, "attach")},
		{"attach_localappdata", attachRoot},
		{"attach_product_parent", explicitPidportParent(attachRoot)},
		{"allocation_case", filepath.Join(runRoot, "allocation")},
		{"allocation_localappdata", allocateRoot},
		{"allocation_product_parent", explicitPidportParent(allocateRoot)},
	}
	for _, component := range components {
		if err := hardenTopologyComponent(component.path, component.label, resources.topologyAdmitted, &resources.topologyEvents); err != nil {
			t.Fatalf("harden explicit state component %s: %v", component.label, err)
		}
	}
	admissionComponents := append([]struct {
		label string
		path  string
	}{{"state_base", stateBase}}, components...)
	for _, component := range admissionComponents {
		if err := hubapi.CheckStateDirParentWriteSafe(component.path); err != nil {
			t.Fatalf("explicit state component %s write gate: %v", component.label, err)
		}
		resources.topologyEvents = append(resources.topologyEvents, "admit:"+component.label)
	}
	resources.topologyAdmitted = true
	resources.topologyEvents = append(resources.topologyEvents, "admitted")
	resources.attachAdmission, err = classifyStateAdmission(attachRoot)
	if err != nil {
		t.Fatal("classify attach state owner")
	}
	resources.allocateAdmission, err = classifyStateAdmission(allocateRoot)
	if err != nil {
		t.Fatal("classify allocation state owner")
	}
	resources.attachAdmission.Hardened, resources.attachAdmission.WriteGateSafe = true, true
	resources.allocateAdmission.Hardened, resources.allocateAdmission.WriteGateSafe = true, true
	if pathContains(evidence, attachRoot) || pathContains(evidence, allocateRoot) {
		t.Fatal("mapped evidence is equal to or an ancestor of candidate LOCALAPPDATA")
	}
	if !strictCandidateEnvironment(candidateEnvironment(attachRoot), attachRoot) || !strictCandidateEnvironment(candidateEnvironment(allocateRoot), allocateRoot) {
		t.Fatal("candidate strict environment admission failed")
	}
	resources.attachAdmission.StrictEnv, resources.allocateAdmission.StrictEnv = true, true
	attachLease, attachPort, err := reserveLoopback()
	if err != nil {
		t.Fatal(err)
	}
	allocateLease, allocatePort, err := reserveLoopback()
	if err != nil {
		_ = attachLease.Close()
		t.Fatal(err)
	}
	if attachRoot == allocateRoot || attachPort == allocatePort {
		_ = attachLease.Close()
		_ = allocateLease.Close()
		t.Fatal("explicit transactions did not receive distinct state roots and ports")
	}
	resources.attachPort, resources.allocatePort = attachPort, allocatePort
	resources.attachLease, resources.allocateLease = attachLease, allocateLease
	return resources
}

func hardenTopologyComponent(path, label string, admitted bool, events *[]string) error {
	if admitted {
		return errors.New("topology mutation attempted after admission")
	}
	parent := filepath.Dir(path)
	parentInfo, err := os.Stat(parent)
	if err != nil || !parentInfo.IsDir() {
		return fmt.Errorf("immediate parent is unavailable: %w", err)
	}
	if info, statErr := os.Stat(path); statErr == nil {
		if !info.IsDir() {
			return errors.New("topology component is not a directory")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := apitest.HardenedDirForTestMain(path); err != nil {
		return err
	}
	*events = append(*events, "harden:"+label)
	return nil
}

func (r *explicitResources) finalize(t *testing.T) {
	t.Helper()
	if r.finalized {
		return
	}
	r.cleanup(t)
	if !r.cleaned {
		r.finalized = true
		return
	}
	exportBoundedEvidence(t, r.evidenceRoot, r.record)
	r.finalized = true
}

func (r *explicitResources) cleanup(t *testing.T) {
	t.Helper()
	if r.cleaned {
		return
	}
	if r.attachLease != nil {
		_ = r.attachLease.Close()
	}
	if r.allocateLease != nil {
		_ = r.allocateLease.Close()
	}
	rel, err := filepath.Rel(r.stateBase, r.runRoot)
	if err != nil || rel == "." || rel == "" || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		t.Errorf("refuse broad explicit state cleanup: rel=%q err=%v", rel, err)
		return
	}
	if err := os.RemoveAll(r.runRoot); err != nil {
		t.Errorf("remove exact settled explicit run root: %v", err)
		return
	}
	if _, err := os.Stat(r.runRoot); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("explicit run root still exists after cleanup: %v", err)
		return
	}
	r.cleaned = true
}

func explicitPidport(root string) string {
	return filepath.Join(explicitPidportParent(root), hubgui.PidportFileLeaf)
}

func explicitPidportParent(root string) string {
	return filepath.Join(root, "mcp-local-hub")
}

func pathContains(parent, child string) bool {
	parentAbs, parentErr := filepath.Abs(parent)
	childAbs, childErr := filepath.Abs(child)
	if parentErr != nil || childErr != nil {
		return false
	}
	rel, err := filepath.Rel(parentAbs, childAbs)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)))
}

type tokenOwnerInformation struct {
	Owner *windows.SID
}

func currentTokenOwner(token windows.Token) (*windows.SID, error) {
	var size uint32
	_ = windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &size)
	if size == 0 {
		return nil, errors.New("TokenOwner size is zero")
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], size, &size); err != nil {
		return nil, err
	}
	owner := (*tokenOwnerInformation)(unsafe.Pointer(&buffer[0])).Owner
	if owner == nil {
		return nil, errors.New("TokenOwner SID is nil")
	}
	return owner.Copy()
}

func classifyOwner(sid, tokenUser *windows.SID) (ownerClass, error) {
	if sid == nil || tokenUser == nil {
		return ownerClassOther, errors.New("owner classification SID is nil")
	}
	if sid.Equals(tokenUser) {
		return ownerClassTokenUser, nil
	}
	localSystem, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return ownerClassOther, err
	}
	if sid.Equals(localSystem) {
		return ownerClassLocalSystem, nil
	}
	administrators, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return ownerClassOther, err
	}
	if sid.Equals(administrators) {
		return ownerClassBuiltinAdministrators, nil
	}
	return ownerClassOther, nil
}

func classifyStateAdmission(path string) (stateAdmissionRecord, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return stateAdmissionRecord{}, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return stateAdmissionRecord{}, err
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return stateAdmissionRecord{}, err
	}
	tokenOwner, err := currentTokenOwner(token)
	if err != nil {
		return stateAdmissionRecord{}, err
	}
	ownerKind, err := classifyOwner(owner, user.User.Sid)
	if err != nil {
		return stateAdmissionRecord{}, err
	}
	tokenOwnerKind, err := classifyOwner(tokenOwner, user.User.Sid)
	if err != nil {
		return stateAdmissionRecord{}, err
	}
	return stateAdmissionRecord{
		OwnerClass: ownerKind, TokenOwnerClass: tokenOwnerKind,
		TokenUserMatch: owner != nil && owner.Equals(user.User.Sid),
	}, nil
}

func runOwnerClassification(localAppData, evidenceRoot string) (resultErr error) {
	base := filepath.Join(localAppData, "mcp-local-hub-phase-e-classification")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return errors.New("create classification base")
	}
	runRoot, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return errors.New("create classification run root")
	}
	record := ownerClassificationRecord{Schema: 1, WriteGate: "not_run"}
	defer func() {
		if pathContains(base, runRoot) && filepath.Clean(base) != filepath.Clean(runRoot) {
			if removeErr := os.RemoveAll(runRoot); removeErr == nil {
				record.CleanupRemoved = true
				_, statErr := os.Stat(runRoot)
				record.CleanupAbsent = errors.Is(statErr, os.ErrNotExist)
			}
		}
		record.Complete = resultErr == nil && record.CleanupRemoved && record.CleanupAbsent
		if exportErr := exportOwnerClassification(evidenceRoot, record); exportErr != nil {
			resultErr = errors.Join(resultErr, errors.New("export sanitized owner classification"))
		}
	}()
	if err := apitest.HardenedDirForTestMain(runRoot); err != nil {
		return errors.New("harden classification run root")
	}
	record.Hardened = true
	admission, err := classifyStateAdmission(runRoot)
	if err != nil {
		return errors.New("classify synthetic state owner")
	}
	record.OwnerClass = admission.OwnerClass
	record.TokenOwnerClass = admission.TokenOwnerClass
	record.TokenUserMatch = admission.TokenUserMatch
	if err := hubapi.CheckStateDirParentWriteSafe(runRoot); err != nil {
		record.WriteGate = classifyWriteGateError(err)
		return errors.New("canonical state-owner gate rejected synthetic root")
	}
	record.WriteGate = "pass"
	return nil
}

func classifyWriteGateError(err error) string {
	switch {
	case err == nil:
		return "pass"
	case errors.Is(err, hubapi.ErrWrongOwner):
		return "wrong_owner"
	case errors.Is(err, hubapi.ErrDaclOutsideAllowlist), errors.Is(err, hubapi.ErrTooLoose):
		return "dacl_outside_allowlist"
	case errors.Is(err, hubapi.ErrIrregularFile):
		return "irregular_or_reparse"
	default:
		return "io_error"
	}
}

func exportOwnerClassification(evidenceRoot string, record ownerClassificationRecord) error {
	if err := os.MkdirAll(evidenceRoot, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(evidenceRoot, ".owner-classification-*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	target := filepath.Join(evidenceRoot, "owner-classification-bounded.json")
	_ = os.Remove(target)
	if err := os.Rename(tmpPath, target); err != nil {
		return err
	}
	readBack, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(readBack, data) {
		return errors.New("owner classification read-back mismatch")
	}
	return nil
}

func exportBoundedEvidence(t *testing.T, evidenceRoot string, record boundedEvidenceRecord) {
	t.Helper()
	if err := os.MkdirAll(evidenceRoot, 0o700); err != nil {
		t.Errorf("create bounded evidence root: %v", err)
		return
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Errorf("marshal bounded explicit evidence: %v", err)
		return
	}
	tmp, err := os.CreateTemp(evidenceRoot, ".explicit-console-*.json.tmp")
	if err != nil {
		t.Errorf("create bounded evidence temp: %v", err)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		t.Errorf("write bounded explicit evidence: %v", err)
		return
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		t.Errorf("sync bounded explicit evidence: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		t.Errorf("close bounded explicit evidence: %v", err)
		return
	}
	target := filepath.Join(evidenceRoot, "explicit-console-bounded.json")
	_ = os.Remove(target)
	if err := os.Rename(tmpPath, target); err != nil {
		t.Errorf("commit bounded explicit evidence: %v", err)
		return
	}
	readBack, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(readBack, data) {
		t.Errorf("bounded explicit evidence read-back mismatch: err=%v", err)
		return
	}
	sum := sha256.Sum256(readBack)
	t.Logf("bounded-evidence schema=%d complete=%v cases=%d sha256=%x", record.Schema, record.Complete, len(record.Cases), sum[:])
}

func startOwnedCommand(cmd *exec.Cmd) (*ownedCommand, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("open synchronization handle for PID %d: %w", cmd.Process.Pid, err)
	}
	owned := &ownedCommand{cmd: cmd, signal: handle, done: make(chan error, 1)}
	go func() { owned.done <- cmd.Wait() }()
	return owned, nil
}

func (p *ownedCommand) live() bool {
	if p == nil || p.signal == 0 || p.waited {
		return false
	}
	status, err := windows.WaitForSingleObject(p.signal, 0)
	return err == nil && status == uint32(windows.WAIT_TIMEOUT)
}

func (p *ownedCommand) wait(timeout time.Duration) error {
	if p.waited {
		return errors.New("owned process Wait called more than once")
	}
	select {
	case p.waitError = <-p.done:
		p.waited = true
	case <-time.After(timeout):
		return fmt.Errorf("PID %d did not exit within %s", p.cmd.Process.Pid, timeout)
	}
	status, err := windows.WaitForSingleObject(p.signal, 0)
	windows.CloseHandle(p.signal)
	p.signal = 0
	if err != nil || status != uint32(windows.WAIT_OBJECT_0) {
		return fmt.Errorf("PID %d Wait returned before synchronization handle signaled: status=%d err=%v", p.cmd.Process.Pid, status, err)
	}
	return p.waitError
}

func (p *ownedCommand) terminateAndWait(timeout time.Duration) error {
	if p == nil || p.waited {
		return nil
	}
	_ = p.cmd.Process.Kill()
	return p.wait(timeout)
}

func (p *ownedCommand) cleanup() {
	if p == nil || p.waited {
		return
	}
	_ = p.cmd.Process.Kill()
	_ = p.wait(5 * time.Second)
}

func waitCandidateReady(process *ownedCommand, stateRoot string, port int, timeout time.Duration) error {
	pidport := explicitPidport(stateRoot)
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 250 * time.Millisecond}
	var lastErr error
	for time.Now().Before(deadline) {
		if !process.live() {
			return fmt.Errorf("candidate PID %d exited before readiness", process.cmd.Process.Pid)
		}
		pid, recordedPort, err := hubgui.ReadPidport(pidport)
		if err == nil && pid == process.cmd.Process.Pid && recordedPort == port {
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/api/ping", nil)
			if reqErr == nil {
				resp, getErr := client.Do(req)
				if getErr == nil {
					var body struct {
						PID int  `json:"pid"`
						OK  bool `json:"ok"`
					}
					decodeErr := json.NewDecoder(resp.Body).Decode(&body)
					_ = resp.Body.Close()
					if resp.StatusCode == http.StatusOK && decodeErr == nil && body.OK && body.PID == pid {
						cancel()
						return nil
					}
					lastErr = fmt.Errorf("ping status=%d pid=%d decode=%v", resp.StatusCode, body.PID, decodeErr)
				} else {
					lastErr = getErr
				}
			} else {
				lastErr = reqErr
			}
			cancel()
		} else {
			lastErr = fmt.Errorf("pidport=%d:%d err=%v, want %d:%d", pid, recordedPort, err, process.cmd.Process.Pid, port)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("candidate PID %d readiness timeout: %w", process.cmd.Process.Pid, lastErr)
}

func pidportHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum[:]), nil
}

func requirePidportUnlocked(path string) error {
	lock := flock.New(path + ".lock")
	ok, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("pidport lock remains held")
	}
	return lock.Unlock()
}

func consoleWindowSet(rows []windowRow) map[string]windowRow {
	result := make(map[string]windowRow)
	for _, row := range rows {
		if consoleWindowClass(row.Class) {
			result[windowKey(row)] = row
		}
	}
	return result
}

func newConsoleWindows(baseline map[string]windowRow, rows []windowRow) map[string]windowRow {
	result := make(map[string]windowRow)
	for key, row := range consoleWindowSet(rows) {
		if _, old := baseline[key]; !old {
			result[key] = row
		}
	}
	return result
}

func waitCorrelatedWindowsAbsent(correlated map[string]windowRow, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rows, err := snapshotVisibleWindows()
		if err != nil {
			return err
		}
		current := consoleWindowSet(rows)
		remaining := make(map[string]windowRow)
		for key, row := range correlated {
			if _, found := current[key]; found {
				remaining[key] = row
			}
		}
		if len(remaining) == 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("correlated console windows remained visible: %+v", correlated)
}

func commitQuiescentConsoleBaseline(snapshot func() ([]windowRow, error), forbidden map[string]windowRow) (map[string]windowRow, error) {
	firstRows, err := snapshot()
	if err != nil {
		return nil, err
	}
	first := consoleWindowSet(firstRows)
	for key, row := range forbidden {
		if _, found := first[key]; found {
			return nil, fmt.Errorf("correlated console window still present: %+v", row)
		}
	}
	time.Sleep(100 * time.Millisecond)
	secondRows, err := snapshot()
	if err != nil {
		return nil, err
	}
	second := consoleWindowSet(secondRows)
	if !reflect.DeepEqual(first, second) {
		return nil, fmt.Errorf("console-window baseline changed across quiescence interval: first=%+v second=%+v", first, second)
	}
	return second, nil
}

func runExplicitAttachTransaction(t *testing.T, resources *explicitResources) explicitCaseRecord {
	t.Helper()
	beforeWindows, err := snapshotVisibleWindows()
	if err != nil {
		t.Fatal(err)
	}
	windowBaseline := consoleWindowSet(beforeWindows)
	parentBefore := currentConsolePIDs()
	if err := resources.attachLease.Close(); err != nil {
		t.Fatalf("release attach port reservation: %v", err)
	}
	resources.attachLease = nil
	cmd := exec.Command(candidatePath(t), cli.WindowsDebugConsolePrefix, "gui", "--foreground", "--no-tray", "--no-browser", "--port", strconv.Itoa(resources.attachPort))
	cmd.Env = candidateEnvironment(resources.attachRoot)
	var stdout, stderr lockedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	process, err := startOwnedCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer process.cleanup()
	if err := waitCandidateReady(process, resources.attachRoot, resources.attachPort, 15*time.Second); err != nil {
		t.Fatalf("attach readiness: %v stderr=%q", err, stderr.String())
	}
	parentNow := currentConsolePIDs()
	newMembers := make([]uint32, 0, len(parentNow))
	for _, pid := range parentNow {
		if !containsPID(parentBefore, int(pid)) {
			newMembers = append(newMembers, pid)
		}
	}
	if len(newMembers) != 1 || int(newMembers[0]) != process.cmd.Process.Pid {
		t.Fatalf("attach admitted members=%v, want only candidate PID %d", newMembers, process.cmd.Process.Pid)
	}
	rows, err := snapshotVisibleWindows()
	if err != nil {
		t.Fatal(err)
	}
	if added := newConsoleWindows(windowBaseline, rows); len(added) != 0 {
		t.Fatalf("attach transaction allocated console windows: %+v", added)
	}
	pid := process.cmd.Process.Pid
	if err := process.terminateAndWait(10 * time.Second); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("settle attach candidate: %v", err)
		}
	}
	if containsPID(currentConsolePIDs(), pid) {
		t.Fatalf("attach candidate PID %d remains in parent console after Wait", pid)
	}
	if err := waitCorrelatedWindowsAbsent(map[string]windowRow{}, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	pidport := explicitPidport(resources.attachRoot)
	if err := requirePidportUnlocked(pidport); err != nil {
		t.Fatalf("attach pidport settlement: %v", err)
	}
	hash, err := pidportHash(pidport)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("attach-settled pid=%d port=%d state_hash=%s", pid, resources.attachPort, hash)
	return explicitCaseRecord{
		CaseID:    "attach",
		Admission: resources.attachAdmission,
		PID:       pid, Port: resources.attachPort, Membership: newMembers,
		Waited: process.waited, Signaled: process.signal == 0, StateHash: hash,
	}
}

func decodeWithin(decoder *json.Decoder, target any, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- decoder.Decode(target) }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return errors.New("timed out waiting for helper report")
	}
}

func runAllocationLauncher() error {
	candidate := strings.TrimSpace(os.Getenv(helperCandidateEnv))
	stateRoot := strings.TrimSpace(os.Getenv(helperStateRootEnv))
	attachPath := strings.TrimSpace(os.Getenv(helperAttachPathEnv))
	attachHash := strings.TrimSpace(os.Getenv(helperAttachHashEnv))
	port, err := strconv.Atoi(strings.TrimSpace(os.Getenv(helperPortEnv)))
	if candidate == "" || stateRoot == "" || attachPath == "" || attachHash == "" || err != nil || port <= 0 {
		return errors.New("allocation launcher environment is incomplete")
	}
	preList := currentConsolePIDs()
	if len(preList) != 0 {
		return fmt.Errorf("allocation intermediary owns console membership before launch: %v", preList)
	}
	beforeHash, err := pidportHash(attachPath)
	if err != nil || beforeHash != attachHash {
		return fmt.Errorf("attach state changed before allocation: hash=%s err=%v", beforeHash, err)
	}
	if err := requirePidportUnlocked(attachPath); err != nil {
		return fmt.Errorf("attach state lock before allocation: %w", err)
	}
	nullIn, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer nullIn.Close()
	cmd := exec.Command(candidate, cli.WindowsDebugConsolePrefix, "gui", "--foreground", "--no-tray", "--no-browser", "--port", strconv.Itoa(port))
	cmd.Env = candidateEnvironment(stateRoot)
	var stdout, stderr lockedBuffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nullIn, &stdout, &stderr
	process, err := startOwnedCommand(cmd)
	if err != nil {
		return fmt.Errorf("start allocation candidate: %w", err)
	}
	defer process.cleanup()
	if err := waitCandidateReady(process, stateRoot, port, 15*time.Second); err != nil {
		return fmt.Errorf("allocation readiness: %w stderr=%q", err, stderr.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	wantBanner := "GUI listening on http://127.0.0.1:" + strconv.Itoa(port)
	for !strings.Contains(stdout.String(), wantBanner) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	pidportPID, pidportPort, err := hubgui.ReadPidport(explicitPidport(stateRoot))
	if err != nil {
		return err
	}
	afterHash, hashErr := pidportHash(attachPath)
	freeErr := requirePidportUnlocked(attachPath)
	ready := allocationReadyReport{
		IntermediaryPID: os.Getpid(), IntermediaryPIDs: preList,
		CandidatePID: process.cmd.Process.Pid, PidportPID: pidportPID, PidportPort: pidportPort,
		AttachStateStable: hashErr == nil && afterHash == attachHash,
		AttachStateFree:   freeErr == nil, StdoutObserved: strings.Contains(stdout.String(), wantBanner),
	}
	if !ready.AttachStateStable || !ready.AttachStateFree || !ready.StdoutObserved || pidportPID != process.cmd.Process.Pid || pidportPort != port {
		return fmt.Errorf("allocation readiness identity invalid: %+v hashErr=%v freeErr=%v stderr=%q", ready, hashErr, freeErr, stderr.String())
	}
	if err := json.NewEncoder(os.Stdout).Encode(ready); err != nil {
		return err
	}
	if _, err := io.CopyN(io.Discard, os.Stdin, 1); err != nil {
		return fmt.Errorf("allocation release pipe: %w", err)
	}
	waitErr := process.terminateAndWait(10 * time.Second)
	var exitErr *exec.ExitError
	if waitErr != nil && !errors.As(waitErr, &exitErr) {
		return fmt.Errorf("wait allocation candidate: %w", waitErr)
	}
	settled := allocationSettlementReport{CandidatePID: ready.CandidatePID, CandidateWaited: process.waited, CandidateSignaled: process.signal == 0}
	return json.NewEncoder(os.Stdout).Encode(settled)
}

func queryConsolePIDsExact() ([]uint32, error) {
	size := 16
	for size <= 4096 {
		pids := make([]uint32, size)
		count, _, callErr := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
		if count == 0 {
			if errno, ok := callErr.(syscall.Errno); ok && errno != 0 && errno != windows.ERROR_INVALID_HANDLE {
				return nil, errno
			}
			return nil, nil
		}
		if int(count) <= len(pids) {
			return append([]uint32(nil), pids[:count]...), nil
		}
		size = int(count)
	}
	return nil, errors.New("console participant list exceeded 4096 PIDs")
}

func runAllocationOracle() error {
	candidatePID, err := strconv.Atoi(strings.TrimSpace(os.Getenv(helperPIDEnv)))
	if err != nil || candidatePID <= 0 {
		return errors.New("allocation oracle candidate PID is invalid")
	}
	before, err := queryConsolePIDsExact()
	if err != nil || len(before) != 0 {
		return fmt.Errorf("allocation oracle pre-state=%v err=%v, want no console", before, err)
	}
	attached, _, callErr := procAttachConsole.Call(uintptr(uint32(candidatePID)))
	if attached == 0 {
		return fmt.Errorf("AttachConsole(%d): %v", candidatePID, callErr)
	}
	freed := false
	defer func() {
		if !freed {
			procFreeConsole.Call()
		}
	}()
	participants, err := queryConsolePIDsExact()
	if err != nil {
		return err
	}
	if ok, _, freeErr := procFreeConsole.Call(); ok == 0 {
		return fmt.Errorf("FreeConsole: %v", freeErr)
	}
	freed = true
	after, err := queryConsolePIDsExact()
	if err != nil || len(after) != 0 {
		return fmt.Errorf("allocation oracle post-state=%v err=%v, want no console", after, err)
	}
	return json.NewEncoder(os.Stdout).Encode(allocationOracleReport{
		OraclePID: os.Getpid(), CandidatePID: candidatePID,
		Before: before, Attached: participants, After: after,
	})
}

func validateAllocationOracle(report allocationOracleReport) error {
	if report.OraclePID <= 0 || report.CandidatePID <= 0 || len(report.Before) != 0 || len(report.After) != 0 {
		return fmt.Errorf("invalid oracle boundary report: %+v", report)
	}
	if len(report.Attached) != 2 || !containsPID(report.Attached, report.CandidatePID) || !containsPID(report.Attached, report.OraclePID) {
		return fmt.Errorf("oracle membership=%v, want candidate %d plus oracle %d only", report.Attached, report.CandidatePID, report.OraclePID)
	}
	return nil
}

func runMembershipOracleCommand(t *testing.T, candidatePID int) allocationOracleReport {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestWindowsExplicitDebugConsole$")
	cmd.Env = envOverlay(os.Environ(), map[string]string{helperModeEnv: "allocation-oracle", helperPIDEnv: strconv.Itoa(candidatePID)})
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008, HideWindow: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	process, err := startOwnedCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer process.cleanup()
	var report allocationOracleReport
	if err := decodeWithin(json.NewDecoder(stdout), &report, 5*time.Second); err != nil {
		t.Fatalf("decode allocation oracle: %v stderr=%q", err, stderr.String())
	}
	if err := process.wait(5 * time.Second); err != nil {
		t.Fatalf("wait allocation oracle: %v stderr=%q", err, stderr.String())
	}
	if report.OraclePID != process.cmd.Process.Pid || report.CandidatePID != candidatePID {
		t.Fatalf("allocation oracle process identity mismatch: report=%+v process=%d", report, process.cmd.Process.Pid)
	}
	if err := validateAllocationOracle(report); err != nil {
		t.Fatal(err)
	}
	return report
}

func runExplicitAllocationTransaction(t *testing.T, resources *explicitResources, attachStateHash string) explicitCaseRecord {
	t.Helper()
	beforeRows, err := snapshotVisibleWindows()
	if err != nil {
		t.Fatal(err)
	}
	windowBaseline := consoleWindowSet(beforeRows)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	helper := exec.Command(executable, "-test.run=^TestWindowsExplicitDebugConsole$")
	helper.Env = envOverlay(os.Environ(), map[string]string{
		helperModeEnv: "allocation-launcher", helperCandidateEnv: candidatePath(t),
		helperStateRootEnv: resources.allocateRoot, helperPortEnv: strconv.Itoa(resources.allocatePort),
		helperAttachPathEnv: explicitPidport(resources.attachRoot), helperAttachHashEnv: attachStateHash,
	})
	helper.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008, HideWindow: true}
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr lockedBuffer
	helper.Stderr = &stderr
	process, err := startOwnedCommand(helper)
	if err != nil {
		t.Fatal(err)
	}
	defer process.cleanup()
	decoder := json.NewDecoder(stdout)
	var ready allocationReadyReport
	if err := decodeWithin(decoder, &ready, 20*time.Second); err != nil {
		t.Fatalf("decode allocation readiness: %v stderr=%q", err, stderr.String())
	}
	if ready.IntermediaryPID != process.cmd.Process.Pid || len(ready.IntermediaryPIDs) != 0 || ready.PidportPID != ready.CandidatePID || ready.PidportPort != resources.allocatePort || !ready.AttachStateStable || !ready.AttachStateFree || !ready.StdoutObserved {
		t.Fatalf("allocation readiness report invalid: %+v stderr=%q", ready, stderr.String())
	}
	if !process.live() {
		t.Fatal("allocation intermediary exited before membership oracle")
	}
	pidportPID, pidportPort, err := hubgui.ReadPidport(explicitPidport(resources.allocateRoot))
	if err != nil || pidportPID != ready.CandidatePID || pidportPort != resources.allocatePort {
		t.Fatalf("allocation pidport identity=%d:%d err=%v, want %d:%d", pidportPID, pidportPort, err, ready.CandidatePID, resources.allocatePort)
	}
	correlated := newConsoleWindows(windowBaseline, mustSnapshotWindows(t))
	oracle := runMembershipOracleCommand(t, ready.CandidatePID)
	for key, row := range newConsoleWindows(windowBaseline, mustSnapshotWindows(t)) {
		correlated[key] = row
	}
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatalf("release allocation intermediary: %v", err)
	}
	_ = stdin.Close()
	var settled allocationSettlementReport
	if err := decodeWithin(decoder, &settled, 15*time.Second); err != nil {
		t.Fatalf("decode allocation settlement: %v stderr=%q", err, stderr.String())
	}
	if err := process.wait(10 * time.Second); err != nil {
		t.Fatalf("wait allocation intermediary: %v stderr=%q", err, stderr.String())
	}
	if settled.CandidatePID != ready.CandidatePID || !settled.CandidateWaited || !settled.CandidateSignaled {
		t.Fatalf("allocation candidate did not settle: %+v", settled)
	}
	for key, row := range newConsoleWindows(windowBaseline, mustSnapshotWindows(t)) {
		correlated[key] = row
	}
	if err := waitCorrelatedWindowsAbsent(correlated, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := commitQuiescentConsoleBaseline(snapshotVisibleWindows, correlated); err != nil {
		t.Fatal(err)
	}
	stateHash, err := pidportHash(explicitPidport(resources.allocateRoot))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("allocation-settled candidate=%d intermediary=%d oracle=detached port=%d correlated_hwnds=%d", ready.CandidatePID, ready.IntermediaryPID, resources.allocatePort, len(correlated))
	return explicitCaseRecord{
		CaseID:    "allocation",
		Admission: resources.allocateAdmission,
		PID:       ready.CandidatePID, Port: resources.allocatePort,
		Membership:      oracle.Attached,
		Waited:          settled.CandidateWaited && process.waited,
		Signaled:        settled.CandidateSignaled && process.signal == 0,
		CorrelatedHWNDs: len(correlated), StateHash: stateHash,
	}
}

func mustSnapshotWindows(t *testing.T) []windowRow {
	t.Helper()
	rows, err := snapshotVisibleWindows()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestWindowsExplicitHarnessSerialSettlementFalsifier(t *testing.T) {
	if os.Getenv(helperModeEnv) == "barrier-fixture" {
		if pids := currentConsolePIDs(); len(pids) > 1 || (len(pids) == 1 && int(pids[0]) != os.Getpid()) {
			fmt.Fprintf(os.Stderr, "barrier fixture console membership is not zero/self-only: %v\n", pids)
			os.Exit(2)
		}
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			PID int `json:"pid"`
		}{PID: os.Getpid()})
		if _, err := io.CopyN(io.Discard, os.Stdin, 1); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestWindowsExplicitHarnessSerialSettlementFalsifier$")
	cmd.Env = envOverlay(os.Environ(), map[string]string{helperModeEnv: "barrier-fixture"})
	hubprocess.NoConsole(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	process, err := startOwnedCommand(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer process.cleanup()
	var report struct {
		PID int `json:"pid"`
	}
	if err := decodeWithin(json.NewDecoder(stdout), &report, 5*time.Second); err != nil {
		t.Fatalf("decode barrier fixture: %v stderr=%q", err, stderr.String())
	}
	if report.PID != process.cmd.Process.Pid || !process.live() {
		t.Fatalf("barrier fixture identity/lifetime mismatch: report=%d process=%d live=%v", report.PID, process.cmd.Process.Pid, process.live())
	}
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err := process.wait(5 * time.Second); err != nil {
		t.Fatalf("barrier fixture Wait: %v stderr=%q", err, stderr.String())
	}
	if process.live() {
		t.Fatalf("barrier fixture PID %d remained live after Wait", report.PID)
	}
	if err := process.wait(time.Second); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("duplicate Wait was not rejected: %v", err)
	}
}

func TestWindowsExplicitHarnessUniqueStatePortFalsifier(t *testing.T) {
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		t.Fatal(err)
	}
	hostScratch, err := filepath.Abs(filepath.Join("..", "..", ".scratch", "windows-console-contract", "host-falsifiers-r5"))
	if err != nil {
		t.Fatal(err)
	}
	resources := reserveExplicitResourcesAt(t, localAppData, hostScratch)
	defer resources.finalize(t)
	if filepath.Clean(resources.attachRoot) == filepath.Clean(resources.allocateRoot) || resources.attachPort == resources.allocatePort {
		t.Fatalf("resources are not distinct: %+v", resources)
	}
	for _, port := range []int{resources.attachPort, resources.allocatePort} {
		listener, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			_ = listener.Close()
			t.Fatalf("reserved port %d was concurrently bindable", port)
		}
	}
}

func TestWindowsExplicitHarnessTopologyOrderingFalsifier(t *testing.T) {
	localAppData := apitest.HardenedTempDir(t)
	evidence := t.TempDir()
	resources := reserveExplicitResourcesAt(t, localAppData, evidence)
	defer resources.finalize(t)

	wantEvents := []string{
		"harden:state_base", "create:run_root", "cleanup:registered", "harden:run_root",
		"harden:attach_case", "harden:attach_localappdata", "harden:attach_product_parent",
		"harden:allocation_case", "harden:allocation_localappdata", "harden:allocation_product_parent",
		"admit:state_base", "admit:run_root", "admit:attach_case", "admit:attach_localappdata",
		"admit:attach_product_parent", "admit:allocation_case", "admit:allocation_localappdata",
		"admit:allocation_product_parent", "admitted",
	}
	if !resources.topologyAdmitted || !reflect.DeepEqual(resources.topologyEvents, wantEvents) {
		t.Fatalf("topology sequence mismatch: admitted=%v events=%q", resources.topologyAdmitted, resources.topologyEvents)
	}

	components := []string{
		resources.stateBase, resources.runRoot,
		filepath.Join(resources.runRoot, "attach"), resources.attachRoot, explicitPidportParent(resources.attachRoot),
		filepath.Join(resources.runRoot, "allocation"), resources.allocateRoot, explicitPidportParent(resources.allocateRoot),
	}
	for index, component := range components {
		probe, err := os.CreateTemp(component, ".topology-access-*")
		if err != nil {
			t.Fatalf("component %d is not writable after hardening/admission: %v", index, err)
		}
		probePath := probe.Name()
		if err := probe.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(probePath); err != nil {
			t.Fatal(err)
		}
	}

	postAdmission := filepath.Join(resources.runRoot, "post-admission-mkdir")
	beforeEvents := append([]string(nil), resources.topologyEvents...)
	if err := hardenTopologyComponent(postAdmission, "post_admission", resources.topologyAdmitted, &resources.topologyEvents); err == nil {
		t.Fatal("topology owner accepted a post-admission mkdir")
	}
	if _, err := os.Stat(postAdmission); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("post-admission topology path exists: %v", err)
	}
	if !reflect.DeepEqual(resources.topologyEvents, beforeEvents) {
		t.Fatalf("refused post-admission mutation changed event trace: before=%q after=%q", beforeEvents, resources.topologyEvents)
	}
}

func TestWindowsExplicitHarnessGuestStateExportFalsifier(t *testing.T) {
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		t.Fatal(err)
	}
	hostScratch, err := filepath.Abs(filepath.Join("..", "..", ".scratch", "windows-console-contract", "host-falsifiers-r5", "export"))
	if err != nil {
		t.Fatal(err)
	}
	resources := reserveExplicitResourcesAt(t, localAppData, hostScratch)
	record := boundedEvidenceRecord{
		Schema: 1, Complete: true,
		Cases: []explicitCaseRecord{{
			CaseID: "falsifier", Admission: resources.attachAdmission,
			PID: 1, Port: resources.attachPort, Membership: []uint32{1}, Waited: true, Signaled: true,
		}},
	}
	resources.record = record
	resources.finalize(t)
	data, err := os.ReadFile(filepath.Join(hostScratch, "explicit-console-bounded.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{resources.runRoot, resources.attachRoot, resources.allocateRoot, localAppData} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("bounded evidence leaked guest path")
		}
	}
	var decoded boundedEvidenceRecord
	if err := json.Unmarshal(data, &decoded); err != nil || !reflect.DeepEqual(decoded, record) {
		t.Fatalf("bounded evidence round trip mismatch: decoded=%+v err=%v", decoded, err)
	}
	if !resources.cleaned {
		t.Fatal("guest-local run root cleanup was not committed")
	}
}

func TestWindowsExplicitHarnessOwnerClassificationFalsifier(t *testing.T) {
	localAppData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		t.Fatal(err)
	}
	hostScratch, err := filepath.Abs(filepath.Join("..", "..", ".scratch", "windows-console-contract", "host-falsifiers-r5", "owner"))
	if err != nil {
		t.Fatal(err)
	}
	if err := runOwnerClassification(localAppData, hostScratch); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(hostScratch, "owner-classification-bounded.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record ownerClassificationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if !record.Complete || !record.Hardened || record.WriteGate != "pass" || !record.CleanupRemoved || !record.CleanupAbsent {
		t.Fatalf("classification lifecycle was incomplete: %+v", record)
	}
	allowed := map[ownerClass]bool{
		ownerClassTokenUser: true, ownerClassLocalSystem: true, ownerClassBuiltinAdministrators: true,
	}
	if !allowed[record.OwnerClass] {
		t.Fatalf("canonical gate admitted unexpected owner class %q", record.OwnerClass)
	}
	if record.TokenOwnerClass == "" {
		t.Fatal("token owner classification is empty")
	}
	for _, forbidden := range []string{localAppData, "S-1-", "owner_sid", "token_user_sid", "dacl", "error", "path"} {
		if bytes.Contains(bytes.ToLower(data), bytes.ToLower([]byte(forbidden))) {
			t.Fatalf("sanitized classification contains forbidden material %q", forbidden)
		}
	}
}

func TestWindowsExplicitHarnessMembershipProbeFalsifier(t *testing.T) {
	good := allocationOracleReport{OraclePID: 101, CandidatePID: 202, Attached: []uint32{202, 101}}
	if err := validateAllocationOracle(good); err != nil {
		t.Fatalf("exact candidate+oracle report rejected: %v", err)
	}
	for name, report := range map[string]allocationOracleReport{
		"foreign member":  {OraclePID: 101, CandidatePID: 202, Attached: []uint32{202, 101, 303}},
		"missing oracle":  {OraclePID: 101, CandidatePID: 202, Attached: []uint32{202}},
		"dirty prestate":  {OraclePID: 101, CandidatePID: 202, Before: []uint32{101}, Attached: []uint32{202, 101}},
		"dirty poststate": {OraclePID: 101, CandidatePID: 202, Attached: []uint32{202, 101}, After: []uint32{101}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateAllocationOracle(report); err == nil {
				t.Fatalf("invalid oracle report accepted: %+v", report)
			}
		})
	}
}

func TestWindowsExplicitHarnessFreshBaselineFalsifier(t *testing.T) {
	stable := []windowRow{{PID: 1, HWND: 2, Class: "ConsoleWindowClass"}}
	calls := 0
	snapshot := func() ([]windowRow, error) {
		calls++
		return append([]windowRow(nil), stable...), nil
	}
	baseline, err := commitQuiescentConsoleBaseline(snapshot, map[string]windowRow{})
	if err != nil || calls != 2 || len(baseline) != 1 {
		t.Fatalf("fresh baseline was not committed from exactly two stable snapshots: calls=%d baseline=%+v err=%v", calls, baseline, err)
	}
	forbidden := map[string]windowRow{windowKey(stable[0]): stable[0]}
	if _, err := commitQuiescentConsoleBaseline(snapshot, forbidden); err == nil {
		t.Fatal("fresh baseline accepted a correlated console window")
	}
	unstableCalls := 0
	unstable := func() ([]windowRow, error) {
		unstableCalls++
		if unstableCalls == 1 {
			return nil, nil
		}
		return append([]windowRow(nil), stable...), nil
	}
	if _, err := commitQuiescentConsoleBaseline(unstable, map[string]windowRow{}); err == nil || unstableCalls != 2 {
		t.Fatalf("changing two-snapshot baseline was not rejected: calls=%d err=%v", unstableCalls, err)
	}
}

func TestWindowsRedirectedOutput(t *testing.T) {
	cases := [][]string{{"version"}, {"__controlled_cli_error__"}, {"gui", "--reset-port", "--yes"}}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			direct := runCandidateObserved(t, args...)
			explicitArgs := append([]string{cli.WindowsDebugConsolePrefix}, args...)
			explicit := runCandidateObserved(t, explicitArgs...)
			if direct.exit != explicit.exit || !bytes.Equal(direct.stdout, explicit.stdout) || !bytes.Equal(direct.stderr, explicit.stderr) {
				t.Fatalf("redirection changed: direct=(%d,%q,%q), explicit=(%d,%q,%q)", direct.exit, direct.stdout, direct.stderr, explicit.exit, explicit.stdout, explicit.stderr)
			}
		})
	}
}

type matrixRow struct {
	Owner string `json:"owner"`
	Class string `json:"class"`
}

func stdHandle(slot uintptr) uintptr {
	h, _, _ := procGetStdHandle.Call(slot)
	return h
}

func validHandle(handle uintptr) bool {
	if handle == 0 || handle == ^uintptr(0) {
		return false
	}
	kind, _, callErr := procGetFileType.Call(handle)
	if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
		return false
	}
	return kind != 0
}

func runChildHelper() error {
	report := childReport{
		PID:         os.Getpid(),
		ConsolePIDs: currentConsolePIDs(),
		ConsoleHWND: func() uintptr { h, _, _ := procGetConsoleWindow.Call(); return h }(),
		StdInValid:  validHandle(stdHandle(^uintptr(9))),
		StdOutValid: validHandle(stdHandle(^uintptr(10))),
		StdErrValid: validHandle(stdHandle(^uintptr(11))),
	}
	report.ConsoleCount = uintptr(len(report.ConsolePIDs))
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		return err
	}
	_, err := io.CopyN(io.Discard, os.Stdin, 1)
	return err
}

func fixtureExecutable(t *testing.T, subsystem uint16) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	peOffset := int(binary.LittleEndian.Uint32(image[0x3c:0x40]))
	offset := peOffset + 24 + 68
	if offset+2 > len(image) {
		t.Fatal("test binary PE header truncated")
	}
	binary.LittleEndian.PutUint16(image[offset:offset+2], subsystem)
	target := filepath.Join(t.TempDir(), fmt.Sprintf("child-%d.exe", subsystem))
	if err := os.WriteFile(target, image, 0o700); err != nil {
		t.Fatal(err)
	}
	return target
}

func runMatrixRow(t *testing.T, row matrixRow) {
	t.Helper()
	subsystem := uint16(3)
	if row.Class == "gui" {
		subsystem = 2
	}
	cmd := exec.Command(fixtureExecutable(t, subsystem), "-test.run=^TestWindowsChildSpawnMatrix$")
	cmd.Env = append(os.Environ(), helperModeEnv+"=child", helperWaitEnv+"=stdin")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	hubprocess.NoConsole(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	var report childReport
	if err := json.NewDecoder(bufio.NewReader(stdout)).Decode(&report); err != nil {
		t.Fatalf("decode report: %v stderr=%q", err, stderr.String())
	}
	if report.PID != cmd.Process.Pid || !report.StdInValid || !report.StdOutValid || !report.StdErrValid {
		t.Fatalf("invalid PID/stdio report: %+v", report)
	}
	if subsystem == 2 {
		if report.ConsoleCount != 0 || len(report.ConsolePIDs) != 0 {
			t.Fatalf("GUI child has console membership: %+v", report)
		}
	} else if report.ConsoleCount > 1 || (report.ConsoleCount == 1 && (len(report.ConsolePIDs) != 1 || int(report.ConsolePIDs[0]) != report.PID)) {
		t.Fatalf("CUI membership is not zero/self-only: %+v", report)
	}
	if containsPID(currentConsolePIDs(), report.PID) {
		t.Fatalf("child PID %d joined parent console", report.PID)
	}
	windowsNow, err := snapshotVisibleWindows()
	if err != nil {
		t.Fatal(err)
	}
	for _, window := range windowsNow {
		if int(window.PID) == report.PID {
			t.Fatalf("child owns visible top-level window: %+v", window)
		}
	}
	if report.ConsoleHWND != 0 {
		visible, _, _ := procIsWindowVisible.Call(report.ConsoleHWND)
		if visible != 0 {
			t.Fatalf("child console HWND %#x is visible", report.ConsoleHWND)
		}
	}
	identity, err := hubprocess.LookupProcessIdentity(report.PID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if identity.CreationDateUnix <= 0 || now-identity.CreationDateUnix > 24*60*60 || identity.CreationDateUnix > now+60 {
		t.Fatalf("CreationDateUnix=%d outside recent bound now=%d", identity.CreationDateUnix, now)
	}
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child exit: %v stderr=%q", err, stderr.String())
	}
	waited = true
}

func runCreationDateCompanion(t *testing.T) {
	t.Helper()
	path := strings.TrimSpace(*processTestFlag)
	if path == "" {
		t.Fatal("-process-test is required")
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("process companion %q is not a regular file: %v", path, err)
	}
	baseline, err := commitQuiescentConsoleBaseline(snapshotVisibleWindows, map[string]windowRow{})
	if err != nil {
		t.Fatalf("commit CreationDate companion baseline: %v", err)
	}
	cmd := exec.Command(path,
		"-test.v",
		"-test.timeout=5m",
		"-test.run=^TestLookupProcessIdentity_CreationDateUnixIsRecent$",
	)
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var stdout, stderr bytes.Buffer
	cmd.Stdin = stdin
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	hubprocess.NoConsole(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start CreationDate companion: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	for {
		select {
		case waitErr := <-done:
			if waitErr != nil {
				t.Fatalf("CreationDate companion exit: %v stdout=%q stderr=%q", waitErr, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), "--- PASS: TestLookupProcessIdentity_CreationDateUnixIsRecent") || !strings.Contains(stdout.String(), "PASS") {
				t.Fatalf("CreationDate companion missing exact PASS markers: %q", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("CreationDate companion stderr=%q, want empty", stderr.String())
			}
			return
		case <-deadline.C:
			_ = cmd.Process.Kill()
			<-done
			t.Fatal("CreationDate companion exceeded 5 minute deadline")
		default:
			rows, snapErr := snapshotVisibleWindows()
			if snapErr != nil {
				_ = cmd.Process.Kill()
				<-done
				t.Fatal(snapErr)
			}
			for _, row := range rows {
				if _, existed := baseline[windowKey(row)]; !existed && (int(row.PID) == cmd.Process.Pid || consoleWindowClass(row.Class)) {
					_ = cmd.Process.Kill()
					<-done
					t.Fatalf("CreationDate companion exposed a visible window: %+v", row)
				}
			}
			time.Sleep(time.Millisecond)
		}
	}
}

func TestWindowsChildSpawnMatrix(t *testing.T) {
	if os.Getenv(helperModeEnv) == "child" {
		if err := runChildHelper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
	t.Run("CreationDateCompanion", runCreationDateCompanion)
	var rows []matrixRow
	if err := json.Unmarshal(childMatrixJSON, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("child matrix is empty")
	}
	for _, row := range rows {
		row := row
		t.Run(row.Owner, func(t *testing.T) { runMatrixRow(t, row) })
	}
}

func patchPEFixture(t *testing.T, source, name string, magic, subsystem uint16) string {
	t.Helper()
	image, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	peOffset := int(binary.LittleEndian.Uint32(image[0x3c:0x40]))
	optional := peOffset + 24
	if optional+70 > len(image) {
		t.Fatal("candidate PE header truncated")
	}
	binary.LittleEndian.PutUint16(image[optional:optional+2], magic)
	binary.LittleEndian.PutUint16(image[optional+68:optional+70], subsystem)
	target := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(target, image, 0o700); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestWindowsPESubsystemAdmission(t *testing.T) {
	candidate := candidatePath(t)
	if err := binaryadmission.AdmitWindowsGUI(candidate); err != nil {
		t.Fatalf("canonical candidate rejected: %v", err)
	}
	for _, tc := range []struct {
		name      string
		magic     uint16
		subsystem uint16
		wantErrID string
	}{
		{"PE32 GUI", 0x10b, 2, ""},
		{"PE32+ GUI", 0x20b, 2, ""},
		{"CUI", 0x20b, 3, binaryadmission.WindowsPESubsystemErrorID},
		{"unsupported magic", 0x999, 2, binaryadmission.WindowsPEFormatErrorID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := patchPEFixture(t, candidate, strings.ReplaceAll(tc.name, " ", "-")+".exe", tc.magic, tc.subsystem)
			err := binaryadmission.AdmitWindowsGUI(path)
			if tc.wantErrID == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var admissionErr *binaryadmission.Error
			if !errors.As(err, &admissionErr) || admissionErr.FailureID() != tc.wantErrID {
				t.Fatalf("error=%v, want %s", err, tc.wantErrID)
			}
		})
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"malformed", []byte("not-a-pe")},
		{"truncated", []byte{'M', 'Z'}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name+".exe")
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			var admissionErr *binaryadmission.Error
			if err := binaryadmission.AdmitWindowsGUI(path); !errors.As(err, &admissionErr) || admissionErr.FailureID() != binaryadmission.WindowsPEFormatErrorID {
				t.Fatalf("error=%v, want %s", err, binaryadmission.WindowsPEFormatErrorID)
			}
		})
	}
}

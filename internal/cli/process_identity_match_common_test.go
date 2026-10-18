package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/config"
	processowner "mcp-local-hub/internal/process"
)

func TestNPMNativeNetstatFixtureDiagnostic(t *testing.T) {
	root := apitest.HardenedTempDir(t)
	for _, assignment := range childStateEnv(root) {
		key, value, _ := strings.Cut(assignment, "=")
		t.Setenv(key, value)
	}
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	resolved, err := exec.LookPath("netstat")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := executableSHA256(resolved)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolved, "-ano")
	processowner.NoConsole(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	exit := -1
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	}
	t.Logf("resolved=%q sha256=%s argv=[-ano] exit=%d stdout-bytes=%d stderr=%q", resolved, hash, exit, stdout.Len(), stderr.String())
	if err != nil {
		t.Fatalf("fixture netstat direct probe: %v", err)
	}
}

func TestNPMNativeRuntimeImageReceiving(t *testing.T) {
	image := os.Getenv("MCPHUB_TEST_NPM_NATIVE_IMAGE")
	if image == "" || runtime.GOOS != "windows" {
		t.Skip("requires the explicitly supplied private test_state_path_env Product")
	}
	if _, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{Path: image, Role: binaryadmission.WindowsArtifactRoleProduct}); err != nil {
		t.Fatal(err)
	}
	root := apitest.HardenedTempDir(t)
	stateDir := childStateDirOverrideLeaf(root)
	if err := apitest.HardenedDirForTestMain(stateDir); err != nil {
		t.Fatal(err)
	}
	for _, assignment := range childStateEnv(root) {
		key, value, _ := strings.Cut(assignment, "=")
		t.Setenv(key, value)
	}
	home := filepath.Join(root, "home")
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	t.Cleanup(api.SetTestCanonicalMcphubPath(""))
	api.EnableSupervisorIPCTestPipeIsolation()
	if !strings.Contains(api.SupervisorIPCAddress(stateDir), "-test-") {
		t.Fatal("fixture refused a non-test pipe")
	}
	legacy, err := api.LocalInstallMcphubPath()
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	// Hold the complete chosen port set until launch, including the native
	// HTTP upstream offset, so all fixture prerequisites are known first.
	var reservations []net.Listener
	reserve := func(upstream bool) int {
		for attempt := 0; attempt < 64; attempt++ {
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := l.Addr().(*net.TCPAddr).Port
			if upstream {
				if port+config.NativeHTTPInternalPortOffset > 65535 {
					if err := l.Close(); err != nil {
						t.Fatal(err)
					}
					continue
				}
				u, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port+config.NativeHTTPInternalPortOffset))
				if err != nil {
					if err := l.Close(); err != nil {
						t.Fatal(err)
					}
					continue
				}
				reservations = append(reservations, u)
			}
			reservations = append(reservations, l)
			return port
		}
		t.Fatal("could not reserve the bounded fixture port set")
		return 0
	}
	globalPort, workspacePort, frontPort := reserve(true), reserve(false), reserve(false)
	t.Cleanup(func() {
		for _, l := range reservations {
			if err := l.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	// The strict state reader checks each owning parent, not merely the
	// fixture root. Harden the preferences/registry leaf before either write.
	if err := apitest.HardenedDirForTestMain(filepath.Join(root, "mcp-local-hub")); err != nil {
		t.Fatal(err)
	}
	if err := api.WriteStateFileAtomic(api.SettingsPath(), map[string]string{
		"mcp_front.routing_target":        string(api.MCPFrontRoutingTargetFront),
		"mcp_front.routing_generation":    "1",
		"mcp_front.routing_admitted_port": fmt.Sprint(frontPort),
		"mcp_front.port":                  fmt.Sprint(frontPort),
	}); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "upstream.cjs")
	js := `const http=require('node:http');const p=Number(process.argv[process.argv.indexOf('--port')+1]);http.createServer((q,s)=>{s.writeHead(200,{'content-type':'application/json'});s.end('{}');}).listen(p,'127.0.0.1');`
	if err := os.WriteFile(script, []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestDir, err := api.CanonicalManifestDir()
	if err != nil || manifestDir != filepath.Join(filepath.Dir(legacy), "servers") {
		t.Fatalf("storage moved: %q, %v", manifestDir, err)
	}
	manifestLeaf := filepath.Join(manifestDir, "runtime-image-fixture")
	if err := os.MkdirAll(manifestLeaf, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf("name: runtime-image-fixture\nkind: global\ntransport: native-http\ncommand: %q\nbase_args: [%q]\ndaemons:\n  - name: default\n    port: %d\nclient_bindings: []\nweekly_refresh: true\n", node, script, globalPort)
	if err := os.WriteFile(filepath.Join(manifestLeaf, "manifest.yaml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace, err = api.CanonicalWorkspacePath(workspace)
	if err != nil {
		t.Fatal(err)
	}
	ws := api.WorkspaceEntry{WorkspaceKey: api.WorkspaceKey(workspace), WorkspacePath: workspace, Language: "go", Backend: "mcp-language-server", Port: workspacePort}
	ws.TaskName = api.LSPTaskNameForWorkspaceLanguage(ws.WorkspaceKey, ws.Language)
	registryPath, err := api.DefaultRegistryPath()
	if err != nil {
		t.Fatal(err)
	}
	reg := api.NewRegistry(registryPath)
	reg.Workspaces = []api.WorkspaceEntry{ws}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	global := api.SupervisorDaemon{TaskName: `\mcp-local-hub-runtime-image-fixture-default`, Server: "runtime-image-fixture", Daemon: "default", Command: legacy, Args: []string{"daemon", "--server", "runtime-image-fixture", "--daemon", "default"}, WorkingDir: filepath.Dir(legacy), Port: globalPort, ManifestHash: api.ManifestHashContent([]byte(raw))}
	lsp := api.BuildSupervisorDaemonForLSP(ws, legacy)
	custom := api.SupervisorDaemon{TaskName: `\runtime-image-fixture-custom`, Command: "node", Args: []string{"./custom.js"}}
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent := &api.SupervisorIntentFile{Version: 1, OwnerMode: api.OwnerModeSupervise, StrictMode: true, Daemons: []api.SupervisorDaemon{global, lsp, api.BuildBuiltinRouteDaemon(legacy, frontPort), custom}, Stops: map[string]api.DaemonIntent{custom.TaskName: {Desired: "stopped", Reason: "operator"}}}
	disabled := false
	intent.MaintenanceTimers = []api.MaintenanceTimer{
		{Name: `\mcp-local-hub-runtime-image-fixture-weekly-refresh`, Kind: "server-weekly-refresh", Command: legacy, Args: []string{"restart", "--server", "runtime-image-fixture"}},
		{Name: api.WeeklyRefreshTaskName, Kind: "workspace-weekly-refresh", Command: legacy, Args: []string{"workspace-weekly-refresh"}, Enabled: &disabled},
		{Name: "foreign-maintenance", Kind: "server-weekly-refresh", Server: "foreign-fixture", Command: legacy, Args: []string{"restart", "--server", "foreign-fixture"}, Enabled: &disabled},
	}
	// Use the existing durable cadence boundary: no maintenance is due within
	// this bounded receiving window, and the disabled timers remain disabled.
	baseline := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano)
	firedAt := map[string]string{"server-weekly-refresh:runtime-image-fixture": baseline, "workspace-weekly-refresh": baseline}
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	if err := api.WriteSupervisorState(statePath, &api.SupervisorStateFile{Version: 1, Daemons: map[string]api.SupervisorDaemonState{}, MaintenanceFiredAt: firedAt}); err != nil {
		t.Fatal(err)
	}
	if err := api.WriteSupervisorIntent(intentPath, intent); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{registryPath, api.SettingsPath(), intentPath} {
		if _, err := api.ReadStateFileInodeAnchored(required); err != nil {
			t.Fatalf("strict fixture prerequisite %s: %v", filepath.Base(required), err)
		}
	}
	packageRoot := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(image))))
	t.Setenv("NODE_PATH", packageRoot)
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(repoRoot, "npm", "bin", "cli.js")
	version := runRuntimeImageFixtureCommand(t, root, "version", exec.Command(node, shim, "version"))
	if !strings.Contains(version, "running:    "+image) || strings.Contains(version, "identity:") {
		t.Fatalf("IDENTITY did not select native npm image: %s", version)
	}
	for _, l := range reservations {
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
	}
	reservations = nil
	plan := runRuntimeImageFixtureCommand(t, root, "plan", exec.Command(node, shim, "install", "--server", "runtime-image-fixture", "--dry-run"))
	if !strings.Contains(plan, image) {
		t.Fatalf("new global launch does not select npm image: %s", plan)
	}
	job, err := processowner.NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(image, "supervise", "--strict-job-protection")
	cmd.Env = os.Environ()
	input, err := os.Open(os.DevNull)
	if err != nil {
		if closeErr := job.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	outputPath := filepath.Join(root, "native-launch.log")
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if closeErr := input.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := job.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := input.Close(); err != nil {
			t.Error(err)
		}
		if err := output.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := processowner.StartWithJobFiles(job, cmd, input, output, output); err != nil {
		if closeErr := job.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait(); close(waited) }()
	t.Cleanup(func() {
		settleRuntimeImageFixtureJob(t, job, waited)
	})
	deadline := time.Now().Add(30 * time.Second)
	var proof api.SupervisorRecoveryStatusV1
	childrenReady := false
	for time.Now().Before(deadline) {
		select {
		case exitErr := <-waited:
			captured, readErr := os.ReadFile(outputPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			t.Fatalf("native exited before isolated owner: %v: %s", exitErr, captured)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		proof, err = api.DialSupervisorIPCRecoveryStatusV1(ctx, stateDir)
		cancel()
		if err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil || proof.Server.ImagePath != image {
		t.Fatalf("owned native IPC proof: image=%q error=%v", proof.Server.ImagePath, err)
	}
	selected, err := api.RuntimeMcphubExecutable()
	if err != nil || selected != image {
		t.Fatalf("ALTERNATE-OWNER selected invoking test helper instead: %q, %v", selected, err)
	}
	readyCtx, readyCancel := context.WithDeadline(context.Background(), deadline)
	defer readyCancel()
	var readinessErr error
	var firstReadinessErr error
	refuseReadiness := func(err error) bool {
		readinessErr = err
		if firstReadinessErr == nil {
			firstReadinessErr = err
		}
		return false
	}
	childrenReady = waitForCondition(time.Until(deadline), func() bool {
		if readyCtx.Err() != nil {
			return false
		}
		state, err := api.ReadSupervisorState(filepath.Join(stateDir, "supervisor-state.json"))
		if err != nil {
			return refuseReadiness(err)
		}
		owners, err := api.LoopbackPortOwnersSnapshotContext(readyCtx)
		if err != nil {
			return refuseReadiness(err)
		}
		client := http.Client{Timeout: time.Second}
		for i, task := range []string{global.TaskName, lsp.TaskName, api.BuiltinRouteTaskName} {
			port := []int{globalPort, workspacePort, frontPort}[i]
			row := state.Daemons[task]
			if row.CurrentPID <= 0 || !processowner.PIDExecutableMatches(row.CurrentPID, image) || owners[port] != row.CurrentPID {
				return refuseReadiness(fmt.Errorf("native child image/socket ownership is not yet bound for fixture %d", i))
			}
			path := "/mcp"
			if task == api.BuiltinRouteTaskName {
				path = "/lsp/go/mcp"
			}
			req, err := http.NewRequestWithContext(readyCtx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
			if err != nil {
				return refuseReadiness(err)
			}
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, err := client.Do(req)
			if err != nil {
				return refuseReadiness(err)
			}
			closeErr := resp.Body.Close()
			if closeErr != nil {
				return refuseReadiness(closeErr)
			}
			if resp.StatusCode >= 500 || resp.StatusCode == http.StatusNotFound {
				return refuseReadiness(fmt.Errorf("fixture %d route status %d", i, resp.StatusCode))
			}
			if task == api.BuiltinRouteTaskName && (resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "POST, DELETE") {
				return refuseReadiness(fmt.Errorf("builtin route did not return its producer-owned GET contract"))
			}
		}
		return true
	})
	if !childrenReady {
		t.Fatalf("same-deadline native image/socket/HTTP readiness failed: first=%v last=%v context=%v", firstReadinessErr, readinessErr, readyCtx.Err())
	}
	got, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range []string{global.TaskName, lsp.TaskName, api.BuiltinRouteTaskName} {
		if d := got.FindSupervisorDaemonByTaskName(task); d == nil || d.Command != image {
			t.Fatalf("persisted task %s did not converge", task)
		}
	}
	if !got.StrictMode || got.OwnerMode != api.OwnerModeSupervise || !reflect.DeepEqual(got.FindSupervisorDaemonByTaskName(custom.TaskName), &custom) || !reflect.DeepEqual(got.Stops, intent.Stops) {
		t.Fatal("custom descriptor, stops, strict mode, or ownership changed")
	}
	wantTimers := append([]api.MaintenanceTimer(nil), intent.MaintenanceTimers...)
	for i := 0; i < 2; i++ {
		wantTimers[i].Command = image
	}
	if !reflect.DeepEqual(got.MaintenanceTimers, wantTimers) {
		t.Fatalf("startup did not preserve/retarget generated timers: got=%#v want=%#v", got.MaintenanceTimers, wantTimers)
	}
	// Reintroduce only the generated timers' legacy commands. All daemon
	// commands remain current, so preview/apply must enter via timer detection.
	if _, err := api.MutateSupervisorIntentIfChangedReturning(intentPath, func(f *api.SupervisorIntentFile) (bool, error) {
		for i := 0; i < 2; i++ {
			f.MaintenanceTimers[i].Command = legacy
		}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	reset, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := api.PreviewGeneratedSupervisorImage(reset)
	if err != nil || !reflect.DeepEqual(preview.MaintenanceTimers, wantTimers) {
		t.Fatalf("timer-only admitted-owner preview: error=%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := api.DialSupervisorIPCReconcile(ctx, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(intentPath)
	if err != nil || string(before) != string(after) {
		t.Fatal("dry-run wrote persisted image convergence")
	}
	if _, err := api.DialSupervisorIPCReconcile(ctx, true); err != nil {
		t.Fatal(err)
	}
	converged, err := api.ReadSupervisorIntent(intentPath)
	if err != nil || converged.FindSupervisorDaemonByTaskName(global.TaskName).Command != image || !reflect.DeepEqual(converged.MaintenanceTimers, wantTimers) {
		t.Fatalf("apply receiver failed convergence: %v", err)
	}
	settled, err := os.ReadFile(intentPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := api.ConvergeGeneratedSupervisorImage(intentPath)
	if err != nil || second.Changed || !reflect.DeepEqual(second.Intent, converged) {
		t.Fatalf("second timer pass changed observed intent: changed=%v error=%v", second.Changed, err)
	}
	if after, err := os.ReadFile(intentPath); err != nil || string(settled) != string(after) {
		t.Fatalf("second timer pass wrote persisted intent: %v", err)
	}
	state, err := api.ReadSupervisorState(statePath)
	if err != nil || !reflect.DeepEqual(state.MaintenanceFiredAt, firedAt) || len(state.TransientPIDs) != 0 {
		t.Fatalf("maintenance fired or cadence/runtime state changed: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("npm image was promoted into .local: %v", err)
	}
	if path, err := api.CanonicalManifestDir(); err != nil || path != manifestDir {
		t.Fatal("native launch moved the manifest root")
	}
	t.Log("NPM-NATIVE: native npm shim, owner proof, three actual native child listeners, weekly_refresh=true persisted startup/timer-only apply convergence, read-only preview, second-pass no-op, unchanged foreign/disabled timers and cadence/runtime state; timers did not fire; owned Job cleanup registered")
}

func runRuntimeImageFixtureCommand(t *testing.T, root, leaf string, cmd *exec.Cmd) string {
	t.Helper()
	job, err := processowner.NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		settleRuntimeImageFixtureJob(t, job, nil)
	}()
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	outputPath := filepath.Join(root, leaf+".log")
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	cmd.Env = os.Environ()
	if _, err := processowner.StartWithJobFiles(job, cmd, input, output, output); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	select {
	case err := <-finished:
		if err != nil {
			bytes, _ := os.ReadFile(outputPath)
			t.Fatalf("%s failed: %v: %s", leaf, err, bytes)
		}
	case <-time.After(20 * time.Second):
		if err := job.TerminateAll(0); err != nil {
			t.Error(err)
		}
		<-finished
		t.Fatalf("%s timed out", leaf)
	}
	bytes, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(bytes)
}

// The fixture owns one retained Job for the full native/Node tree. Lifetime
// counts include exited descendants and cannot be a captured-original oracle
// for this multi-process fixture. Cleanup proves current membership separately
// from the retained root's terminal wait; no PID or process-name kill is used.
func settleRuntimeImageFixtureJob(t *testing.T, job *processowner.Job, waited <-chan error) {
	t.Helper()
	if err := job.TerminateAll(0); err != nil {
		t.Error(err)
	}
	if waited != nil {
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			t.Error("owned root terminal wait did not settle")
		}
	}
	zero := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		members, err := job.MemberPIDs()
		if err != nil {
			t.Error(err)
			break
		}
		if len(members) == 0 {
			zero = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !zero {
		t.Error("owned Job current members were not proved empty")
	}
	if err := job.Close(); err != nil {
		t.Error(err)
	}
	if zero {
		t.Log("owned Job: root wait terminal, zero current members, handle closed")
	}
}

func TestCanonicalMcphubPath(t *testing.T) {
	first := canonicalMcphubPath()
	if first == "" {
		t.Fatal("canonicalMcphubPath returned empty path")
	}
	if !filepath.IsAbs(first) {
		t.Fatalf("canonicalMcphubPath = %q, want absolute path", first)
	}
	if second := canonicalMcphubPath(); second != first {
		t.Fatalf("canonicalMcphubPath cache changed: first=%q second=%q", first, second)
	}
}

func TestPathsEqual(t *testing.T) {
	base := filepath.Join(string(os.PathSeparator), "tmp", "McphubPathTest")
	clean := filepath.Join(base, "bin", "mcphub")
	withDotDot := filepath.Join(base, "bin", "..", "bin", "mcphub")
	trailing := clean + string(os.PathSeparator)

	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{name: "case-handling", a: strings.ToUpper(clean), b: strings.ToLower(clean), want: runtime.GOOS == "windows"},
		{name: "dot-dot-cleaned", a: withDotDot, b: clean, want: true},
		{name: "trailing-separator-cleaned", a: trailing, b: clean, want: true},
		{name: "different-path", a: filepath.Join(base, "bin", "mcphub"), b: filepath.Join(base, "other", "mcphub"), want: false},
		{name: "empty-left", a: "", b: clean, want: false},
		{name: "empty-right", a: clean, b: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pathsEqual(tt.a, tt.b); got != tt.want {
				t.Fatalf("pathsEqual(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

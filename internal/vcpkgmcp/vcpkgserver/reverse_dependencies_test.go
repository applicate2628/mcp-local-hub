package vcpkgserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/reversedepgraph"
)

func TestReverseDependenciesRegistrationAndStrictSchema(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "reverse-dependencies-adapter", Version: "test"}, nil)
	if err := registerTools(&VcpkgServer{server: server}); err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "reverse-dependencies-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range listed.Tools {
		found = found || tool.Name == "vcpkg_reverse_dependencies"
	}
	if !found {
		t.Fatal("vcpkg_reverse_dependencies is not registered")
	}
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "vcpkg_reverse_dependencies", Arguments: map[string]any{"unexpected": true}})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("unknown field did not produce MCP invalid-argument result: %#v", result)
	}
}

func TestReverseDependenciesReceivingSideWire(t *testing.T) {
	root, scratch := t.TempDir(), t.TempDir()
	writeServerPortFixture(t, filepath.Join(root, "ports", "zlib"), "zlib", "")
	writeServerPortFixture(t, filepath.Join(root, "ports", "curl"), "curl", `,"dependencies":["zlib"]`)
	executableName := "vcpkg"
	if runtime.GOOS == "windows" {
		executableName += ".exe"
	}
	if err := os.WriteFile(filepath.Join(root, executableName), []byte("fake-vcpkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := reversedepgraph.RunnerFunc(func(_ context.Context, command reversedepgraph.Command) reversedepgraph.RunOutput {
		output := reversedepgraph.RunOutput{ExitCode: 0, Started: true, Reaped: true}
		capture := func(value string) reversedepgraph.CapturedStream {
			return reversedepgraph.CapturedStream{Data: []byte(value), Bytes: int64(len(value)), SHA256: "fixture"}
		}
		if len(command.Args) == 1 && command.Args[0] == "version" {
			output.Stdout = capture("vcpkg package management program version fixture\n")
			return output
		}
		if len(command.Args) == 2 && command.Args[1] == "--help" {
			output.ExitCode = 1
			output.Stderr = capture("vcpkg depend-info <port name>\n--format --triplet --host-triplet --overlay-ports --overlay-triplets --x-buildtrees-root --x-install-root --downloads-root --x-packages-root --show-depth --vcpkg-root\n")
			return output
		}
		if command.Candidate == "zlib" {
			if command.Format == "dgml" {
				output.Stdout = capture(`<DirectedGraph xmlns="http://schemas.microsoft.com/vs/2009/dgml"><Nodes><Node Id="zlib"/></Nodes><Links/></DirectedGraph>`)
			} else {
				output.Stderr = capture("(0)zlib:\n")
			}
			return output
		}
		if command.Format == "dgml" {
			output.Stdout = capture(`<DirectedGraph xmlns="http://schemas.microsoft.com/vs/2009/dgml"><Nodes><Node Id="curl"/><Node Id="zlib"/></Nodes><Links><Link Source="curl" Target="zlib"/></Links></DirectedGraph>`)
		} else {
			output.Stderr = capture("(0)curl[ssl]: zlib\n(1)zlib:\n")
		}
		return output
	})
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "reverse-dependencies-wire", Version: "test"}, nil)
	vs := &VcpkgServer{server: server, reverseDependenciesRunner: runner, trustedVcpkgRoot: root}
	if err := registerTools(vs); err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "reverse-dependencies-wire-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	wire, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "vcpkg_reverse_dependencies", Arguments: map[string]any{
		"port": "zlib", "vcpkg_root": root, "triplet": "x64-windows", "host_triplet": "x64-windows", "scratch_root": scratch, "timeout_ms": 5000,
	}})
	if err != nil || wire == nil || wire.IsError || len(wire.Content) != 1 {
		t.Fatalf("wire envelope = %#v err=%v", wire, err)
	}
	text, ok := wire.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("wire content = %T", wire.Content[0])
	}
	var body struct {
		Status   string `json:"status"`
		Coverage struct {
			Complete bool `json:"complete"`
		} `json:"coverage"`
		Direct []struct {
			Node reversedepgraph.Node `json:"node"`
		} `json:"direct_dependents"`
	}
	if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
		t.Fatalf("wire JSON: %v\n%s", err, text.Text)
	}
	if body.Status != "ok" || !body.Coverage.Complete || len(body.Direct) != 1 || body.Direct[0].Node.Name != "curl" || !strings.Contains(text.Text, `"feature_policy": "candidate-defaults"`) {
		t.Fatalf("wire body mismatch: %s", text.Text)
	}
}

func TestReverseDependenciesRejectsCallerSelectedExecutableRoot(t *testing.T) {
	trusted, attacker, scratch := t.TempDir(), t.TempDir(), t.TempDir()
	called := false
	vs := &VcpkgServer{
		trustedVcpkgRoot: trusted,
		reverseDependenciesRunner: reversedepgraph.RunnerFunc(func(context.Context, reversedepgraph.Command) reversedepgraph.RunOutput {
			called = true
			return reversedepgraph.RunOutput{}
		}),
	}
	arguments, err := json.Marshal(map[string]any{
		"port": "zlib", "vcpkg_root": attacker, "triplet": "x64-windows", "host_triplet": "x64-windows", "scratch_root": scratch,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcome := vs.reverseDependenciesTool(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: arguments}})
	if outcome.invalidArgument == nil {
		t.Fatal("caller-selected executable root was accepted")
	}
	if called {
		t.Fatal("runner was reached for a caller-selected executable root")
	}
}

func TestReverseDependenciesBindsCanonicalTrustedRootBeforeAnalysis(t *testing.T) {
	trusted, scratch := t.TempDir(), t.TempDir()
	writeServerPortFixture(t, filepath.Join(trusted, "ports", "zlib"), "zlib", "")
	writeServerPortFixture(t, filepath.Join(trusted, "ports", "curl"), "curl", `,"dependencies":["zlib"]`)
	executableName := "vcpkg"
	if runtime.GOOS == "windows" {
		executableName += ".exe"
	}
	if err := os.WriteFile(filepath.Join(trusted, executableName), []byte("fake-vcpkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	request := filepath.Join(t.TempDir(), "requested-root")
	if err := os.Symlink(trusted, request); err != nil {
		t.Skipf("directory link unavailable: %v", err)
	}
	attacker := t.TempDir()
	var received reversedepgraph.Args
	var commands []reversedepgraph.Command
	runner := reversedepgraph.RunnerFunc(func(_ context.Context, command reversedepgraph.Command) reversedepgraph.RunOutput {
		commands = append(commands, command)
		capture := func(value string) reversedepgraph.CapturedStream {
			return reversedepgraph.CapturedStream{Data: []byte(value), Bytes: int64(len(value)), SHA256: "fixture"}
		}
		output := reversedepgraph.RunOutput{ExitCode: 0, Started: true, Reaped: true}
		switch command.Stage {
		case "version":
			output.Stdout = capture("vcpkg package management program version fixture\n")
		case "help":
			output.ExitCode = 1
			output.Stderr = capture("vcpkg depend-info <port name>\n--format --triplet --host-triplet --overlay-ports --overlay-triplets --x-buildtrees-root --x-install-root --downloads-root --x-packages-root --show-depth --vcpkg-root\n")
		case "depend_info":
			if command.Format == "dgml" {
				output.Stdout = capture(`<DirectedGraph xmlns="http://schemas.microsoft.com/vs/2009/dgml"><Nodes><Node Id="curl"/><Node Id="zlib"/></Nodes><Links><Link Source="curl" Target="zlib"/></Links></DirectedGraph>`)
			} else {
				output.Stderr = capture("(0)curl: zlib\n(1)zlib:\n")
			}
		}
		return output
	})
	vs := &VcpkgServer{
		trustedVcpkgRoot:          trusted,
		reverseDependenciesRunner: runner,
		reverseDependenciesRun: func(ctx context.Context, args reversedepgraph.Args, runner reversedepgraph.Runner) reversedepgraph.Result {
			if err := os.Remove(request); err != nil {
				t.Fatalf("remove admitted request link: %v", err)
			}
			if err := os.Symlink(attacker, request); err != nil {
				t.Fatalf("retarget admitted request link: %v", err)
			}
			received = args
			return reversedepgraph.Analyze(ctx, args, runner)
		},
	}
	arguments, err := json.Marshal(reversedepgraph.Args{Port: "zlib", VcpkgRoot: request, Triplet: "x64-windows", HostTriplet: "x64-windows", ScratchRoot: scratch})
	if err != nil {
		t.Fatal(err)
	}
	outcome := vs.reverseDependenciesTool(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: arguments}})
	if outcome.invalidArgument != nil {
		t.Fatalf("admitted alias rejected: %v", outcome.invalidArgument)
	}
	want, err := process.CanonicalizePathStrict(trusted)
	if err != nil {
		t.Fatal(err)
	}
	if received.VcpkgRoot != want {
		t.Fatalf("analysis vcpkg root = %q, want canonical trusted root %q", received.VcpkgRoot, want)
	}
	wantExecutable := filepath.Join(want, executableName)
	if len(commands) == 0 {
		t.Fatal("runner received no commands")
	}
	for _, command := range commands {
		if command.Executable != wantExecutable {
			t.Fatalf("runner executable = %q, want canonical trusted executable %q", command.Executable, wantExecutable)
		}
		for _, argument := range command.Args {
			if strings.HasPrefix(argument, "--vcpkg-root=") && argument != "--vcpkg-root="+want {
				t.Fatalf("runner vcpkg root argument = %q, want canonical trusted root", argument)
			}
		}
	}
}

func TestReverseDependenciesRefusesMissingOrNonDirectoryTrustedRootsBeforeAnalysis(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("not a root"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{missing, file} {
		t.Run(filepath.Base(root), func(t *testing.T) {
			called := false
			vs := &VcpkgServer{
				trustedVcpkgRoot: root,
				reverseDependenciesRun: func(_ context.Context, _ reversedepgraph.Args, _ reversedepgraph.Runner) reversedepgraph.Result {
					called = true
					return reversedepgraph.Result{}
				},
			}
			arguments, err := json.Marshal(reversedepgraph.Args{Port: "zlib", VcpkgRoot: root, Triplet: "x64-windows", HostTriplet: "x64-windows", ScratchRoot: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			outcome := vs.reverseDependenciesTool(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: arguments}})
			if outcome.invalidArgument == nil {
				t.Fatal("unresolvable trusted root was accepted")
			}
			if called {
				t.Fatal("analysis was reached for an unresolvable trusted root")
			}
		})
	}
}

func writeServerPortFixture(t *testing.T, dir, name, dependencies string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"` + name + `","version-string":"1"` + dependencies + `}`
	if err := os.WriteFile(filepath.Join(dir, "vcpkg.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "portfile.cmake"), []byte("set(VCPKG_POLICY_EMPTY_PACKAGE enabled)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentReverseDependencyRequestLimitOne(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	root, scratch := t.TempDir(), t.TempDir()
	vs := &VcpkgServer{trustedVcpkgRoot: root, reverseDependenciesRun: func(_ context.Context, args reversedepgraph.Args, _ reversedepgraph.Runner) reversedepgraph.Result {
		close(entered)
		<-release
		result := reversedepgraph.NewResult(args)
		result.Status = evidence.StatusOK
		result.Coverage.Complete = true
		return result
	}}
	arguments, err := json.Marshal(reversedepgraph.Args{Port: "zlib", VcpkgRoot: root, Triplet: "x64-windows", HostTriplet: "x64-windows", ScratchRoot: scratch, TimeoutMS: 1000})
	if err != nil {
		t.Fatal(err)
	}
	request := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: arguments}}
	firstDone := make(chan projectableToolOutcome, 1)
	go func() { firstDone <- vs.reverseDependenciesTool(context.Background(), request) }()
	<-entered
	secondContext, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	second := vs.reverseDependenciesTool(secondContext, request)
	result, ok := second.result.(reversedepgraph.Result)
	if !ok || result.Status != evidence.StatusUnknown || result.Reason != reversedepgraph.ReasonResourceBusy {
		t.Fatalf("second request = %#v", second)
	}
	close(release)
	<-firstDone
}

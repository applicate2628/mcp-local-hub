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

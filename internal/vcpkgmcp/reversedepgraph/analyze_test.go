package reversedepgraph

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

type scriptedRunner struct {
	mu       sync.Mutex
	commands []Command
	respond  func(Command) RunOutput
}

func largeAnalyzeFixture(t *testing.T, consumers int) Args {
	t.Helper()
	root := t.TempDir()
	writePortFixture(t, filepath.Join(root, "ports", "target"), "target", "")
	for index := 0; index < consumers; index++ {
		name := fmt.Sprintf("consumer-%04d", index)
		writePortFixture(t, filepath.Join(root, "ports", name), name, `,"dependencies":["target"]`)
	}
	if err := os.WriteFile(vcpkgExecutable(root), []byte("fake-vcpkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Args{Port: "target", VcpkgRoot: root, Triplet: "x64-windows", HostTriplet: "x64-windows", ScratchRoot: t.TempDir(), TimeoutMS: 30000}
}

func batchFakeOutput(command Command) RunOutput {
	output := RunOutput{ExitCode: 0, Started: true, Reaped: true}
	if len(command.Args) == 1 && command.Args[0] == "version" {
		output.Stdout = captured("vcpkg package management program version fake-batch\n")
		return output
	}
	if len(command.Args) == 2 && command.Args[1] == "--help" {
		output.ExitCode = 1
		output.Stderr = captured("vcpkg depend-info <port name>\n--format --triplet --host-triplet --overlay-ports --overlay-triplets --x-buildtrees-root --x-install-root --downloads-root --x-packages-root --show-depth --vcpkg-root\n")
		return output
	}
	candidates := command.Candidates
	nodeSet := map[string]bool{"target": true}
	for _, candidate := range candidates {
		nodeSet[candidate] = true
	}
	names := make([]string, 0, len(nodeSet))
	for name := range nodeSet {
		names = append(names, name)
	}
	sort.Strings(names)
	if command.Format == "dgml" {
		var body strings.Builder
		body.WriteString(`<DirectedGraph xmlns="http://schemas.microsoft.com/vs/2009/dgml"><Nodes>`)
		for _, name := range names {
			fmt.Fprintf(&body, `<Node Id="%s"/>`, name)
		}
		body.WriteString(`</Nodes><Links>`)
		for _, candidate := range candidates {
			if candidate != "target" {
				fmt.Fprintf(&body, `<Link Source="%s" Target="target"/>`, candidate)
			}
		}
		body.WriteString(`</Links></DirectedGraph>`)
		output.Stdout = captured(body.String())
		return output
	}
	var body strings.Builder
	for _, name := range names {
		if name == "target" {
			body.WriteString("(1)target:\n")
		} else {
			fmt.Fprintf(&body, "(0)%s: target\n", name)
		}
	}
	output.Stderr = captured(body.String())
	return output
}

func TestMoreThan512CandidatesSettleInBoundedBatchCount(t *testing.T) {
	args := largeAnalyzeFixture(t, 520)
	runner := &scriptedRunner{respond: batchFakeOutput}
	result := Analyze(context.Background(), args, runner)
	if result.Status != evidence.StatusOK || !result.Coverage.Complete || len(result.Direct) != 520 {
		t.Fatalf("batched result = status=%s reason=%s coverage=%#v direct=%d failure=%#v", result.Status, result.Reason, result.Coverage, len(result.Direct), result.Failure)
	}
	wantBatches := (521 + MaxCandidateBatchSize - 1) / MaxCandidateBatchSize
	if result.Resources.ChildInvocations != wantBatches*2 {
		t.Fatalf("child invocations=%d want=%d", result.Resources.ChildInvocations, wantBatches*2)
	}
	if got := len(runner.snapshot()); got != 2+wantBatches*2 {
		t.Fatalf("runner calls=%d want handshake+batch=%d", got, 2+wantBatches*2)
	}
}

func TestPermutationBatchBoundaryByteEquality(t *testing.T) {
	target := targetNode("target")
	candidates := make([]Candidate, 130)
	for index := range candidates {
		candidates[index] = Candidate{Name: fmt.Sprintf("consumer-%04d", 129-index)}
	}
	encode := func(batchSize int) []byte {
		plans := []Plan{}
		for _, batch := range candidateBatches(candidates, batchSize) {
			plan := Plan{Nodes: []Node{target}}
			for _, candidate := range batch {
				node := targetNode(candidate.Name)
				plan.Nodes = append(plan.Nodes, node)
				plan.Edges = append(plan.Edges, Edge{From: node, To: target})
			}
			plans = append(plans, plan)
		}
		nodes, edges := unionPlans(plans)
		body, err := json.Marshal(ReduceGraph("target", nodes, edges))
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	left, right := encode(64), encode(37)
	if string(left) != string(right) {
		t.Fatalf("batch boundary changed bytes\n64=%s\n37=%s", left, right)
	}
}

func TestOneBatchFailureMakesNegativeUnknownAndRetainsPositives(t *testing.T) {
	args := largeAnalyzeFixture(t, 130)
	runner := &scriptedRunner{respond: func(command Command) RunOutput {
		output := batchFakeOutput(command)
		if command.BatchIndex == 1 && command.Format == "dgml" {
			output.ExitCode = 9
			output.Err = os.ErrInvalid
			output.Stderr = captured("batch failed")
		}
		return output
	}}
	result := Analyze(context.Background(), args, runner)
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonDependInfoNonzero || result.Coverage.Complete || len(result.Direct) == 0 {
		t.Fatalf("failed batch result = status=%s reason=%s complete=%v positives=%d failure=%#v", result.Status, result.Reason, result.Coverage.Complete, len(result.Direct), result.Failure)
	}
}

func TestCancellationSettlesAllActiveBatches(t *testing.T) {
	args := largeAnalyzeFixture(t, 260)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 8)
	var active sync.WaitGroup
	runner := &scriptedRunner{respond: func(command Command) RunOutput {
		if command.Stage != "depend_info" {
			return batchFakeOutput(command)
		}
		active.Add(1)
		defer active.Done()
		started <- struct{}{}
		<-ctx.Done()
		return RunOutput{ExitCode: -1, Started: true, Reaped: true, Err: ctx.Err()}
	}}
	done := make(chan Result, 1)
	go func() { done <- Analyze(ctx, args, runner) }()
	for index := 0; index < 4; index++ {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("four batch workers did not become active")
		}
	}
	cancel()
	select {
	case result := <-done:
		if result.Status != evidence.StatusUnknown || result.Reason != ReasonRequestCancelled {
			t.Fatalf("cancelled batch result=%#v", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Analyze did not settle active batches")
	}
	settled := make(chan struct{})
	go func() { active.Wait(); close(settled) }()
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("active batch runner calls survived cancellation")
	}
}

func (runner *scriptedRunner) Run(_ context.Context, command Command) RunOutput {
	runner.mu.Lock()
	runner.commands = append(runner.commands, command)
	runner.mu.Unlock()
	return runner.respond(command)
}

func (runner *scriptedRunner) snapshot() []Command {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return append([]Command(nil), runner.commands...)
}

func analyzeFixture(t *testing.T) Args {
	t.Helper()
	root := t.TempDir()
	writePortFixture(t, filepath.Join(root, "ports", "zlib"), "zlib", "")
	writePortFixture(t, filepath.Join(root, "ports", "curl"), "curl", `,"dependencies":["zlib"]`)
	if err := os.WriteFile(vcpkgExecutable(root), []byte("fake-vcpkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Args{Port: "zlib", VcpkgRoot: root, Triplet: "x64-windows", HostTriplet: "x64-windows", ScratchRoot: t.TempDir(), TimeoutMS: 5000}
}

func completeFakeOutput(command Command) RunOutput {
	output := RunOutput{ExitCode: 0, Started: true, Reaped: true}
	if len(command.Args) == 1 && command.Args[0] == "version" {
		output.Stdout = captured("vcpkg package management program version fake-1\n")
		return output
	}
	if len(command.Args) == 2 && command.Args[0] == "depend-info" && command.Args[1] == "--help" {
		output.ExitCode = 1
		output.Stderr = captured("vcpkg depend-info <port name>\n--format --triplet --host-triplet --overlay-ports --overlay-triplets --x-buildtrees-root --x-install-root --downloads-root --x-packages-root --show-depth --vcpkg-root\n")
		return output
	}
	if len(command.Candidates) > 1 {
		if command.Format == "dgml" {
			output.Stdout = captured(resolvedFixtureDGML)
		} else {
			output.Stderr = captured(resolvedFixtureList)
		}
		return output
	}
	format := command.Format
	if command.Candidate == "zlib" {
		if format == "dgml" {
			output.Stdout = captured(`<DirectedGraph xmlns="http://schemas.microsoft.com/vs/2009/dgml"><Nodes><Node Id="zlib"/></Nodes><Links/></DirectedGraph>`)
		} else {
			output.Stderr = captured("(0)zlib:\n")
		}
		return output
	}
	if format == "dgml" {
		output.Stdout = captured(resolvedFixtureDGML)
	} else {
		output.Stderr = captured(resolvedFixtureList)
	}
	return output
}

func captured(value string) CapturedStream {
	return CapturedStream{Data: []byte(value), Bytes: int64(len(value)), SHA256: "fixture"}
}

func TestAnalyzeUsesResolvedPlansAndRemovesScratch(t *testing.T) {
	args := analyzeFixture(t)
	runner := &scriptedRunner{respond: completeFakeOutput}
	result := Analyze(context.Background(), args, runner)
	if result.Status != evidence.StatusOK || !result.Coverage.Complete {
		t.Fatalf("Analyze = %#v", result)
	}
	if len(result.Direct) != 1 || result.Direct[0].Node.Name != "curl" {
		t.Fatalf("direct = %#v, want curl", result.Direct)
	}
	for _, command := range runner.snapshot() {
		if command.Stage == "depend_info" && !strings.Contains(strings.Join(command.Args, "\x00"), "--vcpkg-root="+args.VcpkgRoot) {
			t.Fatalf("command omitted explicit vcpkg root: %#v", command.Args)
		}
	}
	entries, err := os.ReadDir(args.ScratchRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch child not removed: entries=%v err=%v", entries, err)
	}
}

func TestUnsupportedVersionFailsBeforeGraph(t *testing.T) {
	args := analyzeFixture(t)
	runner := &scriptedRunner{respond: func(command Command) RunOutput {
		output := completeFakeOutput(command)
		if len(command.Args) == 2 && command.Args[1] == "--help" {
			output.Stderr = captured("vcpkg depend-info <port name> --format only\n")
		}
		return output
	}}
	result := Analyze(context.Background(), args, runner)
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonVcpkgVersionUnsupported || result.Failure.ID != FailureVersionUnsupported {
		t.Fatalf("result = %#v", result)
	}
	if got := len(runner.snapshot()); got != 2 {
		t.Fatalf("graph commands started after failed handshake: %d", got)
	}
}

func TestNonzeroPreservesRedactedStderr(t *testing.T) {
	args := analyzeFixture(t)
	redactionValue := "fixture-redaction-value"
	runner := &scriptedRunner{respond: func(command Command) RunOutput {
		output := completeFakeOutput(command)
		if containsString(command.Candidates, "curl") && command.Format == "dgml" {
			output.ExitCode = 7
			output.Err = os.ErrInvalid
			output.Stderr = captured("tok" + "en=" + redactionValue + " producer failed")
		}
		return output
	}}
	result := Analyze(context.Background(), args, runner)
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonDependInfoNonzero || result.Failure.ID != FailureNonzero {
		t.Fatalf("result = %#v", result)
	}
	body := result.Diagnostics[0].SafePrefix
	if strings.Contains(body, redactionValue) || !strings.Contains(body, "REDACTED") {
		t.Fatalf("diagnostic not redacted: %q", body)
	}
}

func TestEveryCoverageHoleMakesNegativeUnknown(t *testing.T) {
	args := analyzeFixture(t)
	runner := &scriptedRunner{respond: func(command Command) RunOutput {
		output := completeFakeOutput(command)
		if containsString(command.Candidates, "curl") && command.Format == "list" {
			output.Stderr = captured("(0)curl: openssl\n(1)openssl:\n")
		}
		return output
	}}
	result := Analyze(context.Background(), args, runner)
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonDependInfoOutputInconsistent || result.Coverage.Complete {
		t.Fatalf("coverage hole authorized a negative: %#v", result)
	}
}

func TestTripletInputDriftFailsClosed(t *testing.T) {
	args := analyzeFixture(t)
	tripletDir := filepath.Join(args.VcpkgRoot, "triplets")
	if err := os.MkdirAll(tripletDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tripletPath := filepath.Join(tripletDir, "x64-windows.cmake")
	if err := os.WriteFile(tripletPath, []byte("set(VCPKG_TARGET_ARCHITECTURE x64)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var change sync.Once
	runner := &scriptedRunner{respond: func(command Command) RunOutput {
		if command.Stage == "depend_info" {
			change.Do(func() {
				if err := os.WriteFile(tripletPath, []byte("set(VCPKG_TARGET_ARCHITECTURE arm64)\n"), 0o644); err != nil {
					t.Errorf("change triplet: %v", err)
				}
			})
		}
		return completeFakeOutput(command)
	}}
	result := Analyze(context.Background(), args, runner)
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonInputChangedDuringResolution || result.Coverage.InputUnchanged {
		t.Fatalf("triplet drift result = %#v", result)
	}
}

func TestRemoteRegistryRefusedBeforeChildStart(t *testing.T) {
	root, manifest, scratch := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(vcpkgExecutable(root), []byte("fake-vcpkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifest, "vcpkg.json"), []byte(`{"name":"app","version-string":"1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifest, "vcpkg-configuration.json"), []byte(`{"default-registry":{"kind":"git","repository":"https://example.invalid/registry"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := &scriptedRunner{respond: completeFakeOutput}
	result := Analyze(context.Background(), Args{Port: "zlib", VcpkgRoot: root, Triplet: "x64-windows", HostTriplet: "x64-windows", ManifestRoot: manifest, ScratchRoot: scratch}, runner)
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonNetworkDisabledRegistry || len(runner.snapshot()) != 0 {
		t.Fatalf("remote registry crossed child boundary: result=%#v calls=%d", result, len(runner.snapshot()))
	}
}

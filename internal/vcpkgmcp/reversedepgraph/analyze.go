package reversedepgraph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/portresolution"
)

var requiredDependInfoCapabilities = []string{
	"--format", "--triplet", "--host-triplet", "--overlay-ports",
	"--overlay-triplets", "--x-buildtrees-root", "--x-install-root",
	"--downloads-root", "--x-packages-root", "--show-depth", "--vcpkg-root",
}

func Analyze(parent context.Context, args Args, runner Runner) (result Result) {
	result = NewResult(args)
	if err := ValidateArgs(parent, args); err != nil {
		result.Status = evidence.StatusUnknown
		result.Reason = ReasonIncompletePortUniverse
		result.Failure = &Failure{ID: FailureUniverseIncomplete, Reason: ReasonIncompletePortUniverse, Stage: "admission", Detail: err.Error()}
		return result
	}
	if runner == nil {
		runner = DefaultRunner()
	}
	ctx, cancel := context.WithTimeout(parent, args.Timeout())
	defer cancel()

	executable := vcpkgExecutable(args.VcpkgRoot)
	identity, failure := inspectExecutable(executable)
	result.Executable = identity
	if failure != nil {
		result.Status = evidence.StatusUnknown
		result.Reason = failure.Reason
		result.Failure = failure
		return result
	}
	result.Evidence.AddPath(executable)
	universe := EnumerateUniverse(ctx, args)
	result.Resources.EnumeratorBytes = universe.BytesRead
	result.Resources.EnumeratorEntries = universe.Entries
	result.Coverage.UniverseComplete = universe.Complete
	result.Coverage.CandidateCount = len(universe.Candidates)
	result.Coverage.UniverseDigest = universe.Digest
	if !universe.Complete {
		return withFailure(result, universe.Failure)
	}
	potential := PotentialCandidates(universe.Candidates, args.Port)
	result.Coverage.PotentialCount = len(potential)

	if err := os.MkdirAll(args.ScratchRoot, 0o700); err != nil {
		return withFailure(result, &Failure{ID: FailureScratchIO, Reason: ReasonScratchIOFailed, Stage: "scratch_create", Detail: "scratch root create failed"})
	}
	scratch, err := os.MkdirTemp(args.ScratchRoot, "vcpkg-reverse-dependencies-")
	if err != nil {
		return withFailure(result, &Failure{ID: FailureScratchIO, Reason: ReasonScratchIOFailed, Stage: "scratch_create", Detail: "scratch child create failed"})
	}
	if pathsOverlap(scratch, args.VcpkgRoot) || (args.ManifestRoot != "" && pathsOverlap(scratch, args.ManifestRoot)) {
		_ = os.RemoveAll(scratch)
		return withFailure(result, &Failure{ID: FailureScratchIO, Reason: ReasonScratchIOFailed, Stage: "scratch_create", Detail: "resolved scratch child overlaps input"})
	}
	result.Semantics.EnvironmentKeys = environmentKeys(allowedEnvironment(scratch))
	defer func() {
		if recovered := recover(); recovered != nil {
			result = withFailure(result, &Failure{ID: FailureScratchIO, Reason: ReasonScratchIOFailed, Stage: "panic_recovery", Detail: "internal panic recovered"})
		}
		if err := os.RemoveAll(scratch); err != nil {
			result = withFailure(result, &Failure{ID: FailureScratchCleanup, Reason: ReasonScratchCleanupFailed, Stage: "scratch_cleanup", Detail: scratch})
			return
		}
		if _, err := os.Lstat(scratch); !os.IsNotExist(err) {
			result = withFailure(result, &Failure{ID: FailureScratchCleanup, Reason: ReasonScratchCleanupFailed, Stage: "scratch_cleanup", Detail: scratch})
		}
	}()

	if failure := capabilityHandshake(ctx, args, scratch, runner, &result); failure != nil {
		return withFailure(result, failure)
	}

	settled := resolvePotentialPlans(ctx, args, scratch, runner, potential)
	result.Resources.ChildInvocations += settled.invocations
	result.Resources.ReapedProcesses += settled.reaped
	result.Resources.CapturedOutputBytes += settled.captured
	result.Coverage.SettledPlanCount = settled.settledCandidates
	result.Coverage.PlansComplete = settled.failure == nil && settled.settledCandidates == len(potential)
	result.Coverage.FormatsAgree = settled.failure == nil
	result.Diagnostics = append(result.Diagnostics, settled.diagnostics...)
	for _, command := range settled.commands {
		result.Evidence.AddCommand(command)
	}

	nodes, edges := unionPlans(settled.plans)
	result.Resources.NodeHighWater = len(nodes)
	result.Resources.EdgeHighWater = len(edges)
	if len(nodes) > MaxNodes || len(edges) > MaxEdges {
		return withFailure(result, resourceFailure("graph_nodes_or_edges"))
	}
	graph := ReduceGraph(args.Port, nodes, edges)
	result.Targets, result.Direct, result.Transitive = graph.Targets, graph.Direct, graph.Transitive
	result.Edges, result.Cycles = graph.Edges, graph.Cycles
	if settled.failure != nil {
		return withFailure(result, settled.failure)
	}

	provenance, provenanceFailure := resolveProvenance(ctx, args, graph)
	result.Provenance = provenance
	result.Coverage.ProvenanceComplete = provenanceFailure == nil
	if provenanceFailure != nil {
		return withFailure(result, provenanceFailure)
	}

	after := EnumerateUniverse(ctx, args)
	if !after.Complete {
		return withFailure(result, &Failure{ID: FailureInputDrift, Reason: ReasonInputChangedDuringResolution, Stage: "input_recheck", Detail: "input universe no longer admits completely"})
	}
	result.Coverage.InputUnchanged = after.Digest == universe.Digest
	afterIdentity, execFailure := inspectExecutable(executable)
	if execFailure != nil || afterIdentity.SHA256 != result.Executable.SHA256 {
		result.Coverage.InputUnchanged = false
	}
	if !result.Coverage.InputUnchanged {
		return withFailure(result, &Failure{ID: FailureInputDrift, Reason: ReasonInputChangedDuringResolution, Stage: "input_recheck", Detail: "input digest changed"})
	}
	result.Coverage.Complete = result.Coverage.UniverseComplete && result.Coverage.PlansComplete && result.Coverage.FormatsAgree && result.Coverage.ProvenanceComplete && result.Coverage.InputUnchanged
	if !result.Coverage.Complete {
		return withFailure(result, universeFailure("coverage did not settle"))
	}
	result.Status = evidence.StatusOK
	return result
}

func inspectExecutable(path string) (ExecutableIdentity, *Failure) {
	identity := ExecutableIdentity{Path: path}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		return identity, &Failure{ID: FailureExecUnavailable, Reason: ReasonVcpkgCommandUnavailable, Stage: "executable", Detail: "validated executable is unavailable"}
	}
	file, err := os.Open(path)
	if err != nil {
		return identity, &Failure{ID: FailureExecUnavailable, Reason: ReasonVcpkgCommandUnavailable, Stage: "executable", Detail: "validated executable is unreadable"}
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return identity, &Failure{ID: FailureExecUnavailable, Reason: ReasonVcpkgCommandUnavailable, Stage: "executable", Detail: "executable hash failed"}
	}
	identity.SHA256 = hex.EncodeToString(digest.Sum(nil))
	return identity, nil
}

func capabilityHandshake(ctx context.Context, args Args, scratch string, runner Runner, result *Result) *Failure {
	commands := []Command{
		{Executable: vcpkgExecutable(args.VcpkgRoot), Args: []string{"version"}, Dir: scratch, Env: allowedEnvironment(scratch), Stage: "version"},
		{Executable: vcpkgExecutable(args.VcpkgRoot), Args: []string{"depend-info", "--help"}, Dir: scratch, Env: allowedEnvironment(scratch), Stage: "help"},
	}
	version := runner.Run(ctx, commands[0])
	result.Resources.ReapedProcesses += boolInt(version.Reaped)
	result.Resources.CapturedOutputBytes += version.Stdout.Bytes + version.Stderr.Bytes
	if failure := classifyHandshakeRun(ctx, commands[0], version, false); failure != nil {
		return failure
	}
	result.Executable.Version = firstNonemptyLine(string(version.Stdout.Data) + "\n" + string(version.Stderr.Data))
	result.Evidence.AddCommand(renderCommand(commands[0]))

	help := runner.Run(ctx, commands[1])
	result.Resources.ReapedProcesses += boolInt(help.Reaped)
	result.Resources.CapturedOutputBytes += help.Stdout.Bytes + help.Stderr.Bytes
	if failure := classifyHandshakeRun(ctx, commands[1], help, true); failure != nil {
		return failure
	}
	helpText := strings.ToLower(string(help.Stdout.Data) + "\n" + string(help.Stderr.Data))
	if !strings.Contains(helpText, "vcpkg depend-info") {
		return &Failure{ID: FailureVersionUnsupported, Reason: ReasonVcpkgVersionUnsupported, Stage: "help", Detail: "depend-info usage missing"}
	}
	for _, capability := range requiredDependInfoCapabilities {
		if !strings.Contains(helpText, capability) {
			return &Failure{ID: FailureVersionUnsupported, Reason: ReasonVcpkgVersionUnsupported, Stage: "help", Detail: "required depend-info capability missing: " + capability}
		}
	}
	result.Evidence.AddCommand(renderCommand(commands[1]))
	return nil
}

func classifyHandshakeRun(ctx context.Context, command Command, output RunOutput, expectedNonzero bool) *Failure {
	if failure := contextFailure(ctx, command.Stage); failure != nil {
		return failure
	}
	if !output.Started || !output.Reaped {
		return &Failure{ID: FailureExecUnavailable, Reason: ReasonVcpkgCommandUnavailable, Stage: command.Stage, Detail: "process did not start and reap"}
	}
	if output.Stdout.Truncated || output.Stderr.Truncated {
		return resourceFailure("handshake_output")
	}
	if expectedNonzero {
		if output.ExitCode != 0 && output.ExitCode != 1 {
			return &Failure{ID: FailureVersionUnsupported, Reason: ReasonVcpkgVersionUnsupported, Stage: command.Stage, Detail: "unexpected help exit"}
		}
		return nil
	}
	if output.ExitCode != 0 || output.Err != nil {
		return &Failure{ID: FailureVersionUnsupported, Reason: ReasonVcpkgVersionUnsupported, Stage: command.Stage, Detail: "version command failed"}
	}
	return nil
}

type settledPlans struct {
	plans             []Plan
	failure           *Failure
	diagnostics       []Diagnostic
	invocations       int
	reaped            int
	captured          int64
	settledCandidates int
	commands          []string
}

type candidatePlan struct {
	index             int
	plan              Plan
	failure           *Failure
	diagnostics       []Diagnostic
	invocations       int
	reaped            int
	captured          int64
	settledCandidates int
	commands          []string
}

func resolvePotentialPlans(ctx context.Context, args Args, scratch string, runner Runner, candidates []Candidate) settledPlans {
	batches := candidateBatches(candidates, MaxCandidateBatchSize)
	if len(batches)*2 > MaxBatchInvocations {
		return settledPlans{failure: resourceFailure("batch_invocations")}
	}
	jobs := make(chan int)
	results := make(chan candidatePlan, len(batches))
	batchContext, stopBatches := context.WithCancel(ctx)
	defer stopBatches()
	var captured atomic.Int64
	var capturedLimitExceeded atomic.Bool
	workers := 4
	if len(batches) < workers {
		workers = len(batches)
	}
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range jobs {
				results <- resolveOneBatch(batchContext, args, scratch, runner, index, batches[index], &captured, &capturedLimitExceeded, stopBatches)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index := range batches {
			select {
			case jobs <- index:
			case <-batchContext.Done():
				return
			}
		}
	}()
	go func() {
		wait.Wait()
		close(results)
	}()
	ordered := []candidatePlan{}
	for resolved := range results {
		ordered = append(ordered, resolved)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
	settled := settledPlans{}
	for _, resolved := range ordered {
		settled.invocations += resolved.invocations
		settled.reaped += resolved.reaped
		settled.captured += resolved.captured
		settled.settledCandidates += resolved.settledCandidates
		settled.diagnostics = append(settled.diagnostics, resolved.diagnostics...)
		settled.commands = append(settled.commands, resolved.commands...)
		if resolved.failure != nil {
			if settled.failure == nil {
				settled.failure = resolved.failure
			}
			continue
		}
		settled.plans = append(settled.plans, resolved.plan)
	}
	if capturedLimitExceeded.Load() {
		settled.failure = resourceFailure("captured_output")
	}
	if ctx.Err() != nil && settled.failure == nil {
		settled.failure = contextFailure(ctx, "depend_info_batches")
	}
	return settled
}

func resolveOneBatch(ctx context.Context, args Args, scratch string, runner Runner, index int, batch []Candidate, captured *atomic.Int64, capturedLimitExceeded *atomic.Bool, stopBatches context.CancelFunc) candidatePlan {
	resolved := candidatePlan{index: index}
	names := candidateNames(batch)
	candidate := strings.Join(names, ",")
	streams := map[string]RunOutput{}
	for _, format := range []string{"dgml", "list"} {
		commandScratch := filepath.Join(scratch, fmt.Sprintf("batch-%04d-%s", index, format))
		for _, directory := range []string{commandScratch, filepath.Join(commandScratch, "buildtrees"), filepath.Join(commandScratch, "installed"), filepath.Join(commandScratch, "downloads"), filepath.Join(commandScratch, "packages")} {
			if err := os.MkdirAll(directory, 0o700); err != nil {
				resolved.failure = &Failure{ID: FailureScratchIO, Reason: ReasonScratchIOFailed, Stage: "scratch_use", Candidate: candidate, Format: format, Detail: "candidate scratch create failed"}
				return resolved
			}
		}
		command := DependInfoBatchCommand(args, names, format, commandScratch, index)
		resolved.commands = append(resolved.commands, renderCommand(command))
		output := runner.Run(ctx, command)
		resolved.invocations++
		resolved.reaped += boolInt(output.Reaped)
		capturedBytes := output.Stdout.Bytes + output.Stderr.Bytes
		resolved.captured += capturedBytes
		if captured.Add(capturedBytes) > MaxCapturedBytes {
			capturedLimitExceeded.Store(true)
			stopBatches()
			resolved.failure = resourceFailure("captured_output")
			return resolved
		}
		if resolved.captured > MaxCapturedBytes {
			resolved.failure = resourceFailure("captured_output")
			return resolved
		}
		if failure := classifyDependInfoRun(ctx, command, output); failure != nil {
			resolved.failure = failure
			resolved.diagnostics = append(resolved.diagnostics, outputDiagnostic(command, output))
			return resolved
		}
		streams[format] = output
	}
	plan, failure := ParseResolvedPlan(streams["dgml"].Stdout.Data, streams["list"].Stdout.Data, streams["list"].Stderr.Data, args.Triplet, args.HostTriplet)
	if failure != nil {
		failure.Candidate = candidate
		resolved.failure = failure
		return resolved
	}
	resolved.plan = plan
	resolved.settledCandidates = len(batch)
	resolved.diagnostics = append(resolved.diagnostics, plan.Diagnostics...)
	return resolved
}

func candidateBatches(candidates []Candidate, batchSize int) [][]Candidate {
	if batchSize <= 0 {
		return nil
	}
	ordered := append([]Candidate(nil), candidates...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	batches := make([][]Candidate, 0, (len(ordered)+batchSize-1)/batchSize)
	for start := 0; start < len(ordered); start += batchSize {
		end := start + batchSize
		if end > len(ordered) {
			end = len(ordered)
		}
		batches = append(batches, append([]Candidate(nil), ordered[start:end]...))
	}
	return batches
}

func classifyDependInfoRun(ctx context.Context, command Command, output RunOutput) *Failure {
	if failure := contextFailure(ctx, command.Stage); failure != nil {
		failure.Candidate, failure.Format = command.Candidate, command.Format
		return failure
	}
	if !output.Started || !output.Reaped {
		return &Failure{ID: FailureExecUnavailable, Reason: ReasonVcpkgCommandUnavailable, Stage: command.Stage, Candidate: command.Candidate, Format: command.Format, Detail: "process did not start and reap"}
	}
	if output.Stdout.Truncated || output.Stderr.Truncated {
		failure := resourceFailure("child_stream")
		failure.Candidate, failure.Format = command.Candidate, command.Format
		return failure
	}
	if output.ExitCode != 0 || output.Err != nil {
		exit := output.ExitCode
		return &Failure{ID: FailureNonzero, Reason: ReasonDependInfoNonzero, Stage: command.Stage, Candidate: command.Candidate, Format: command.Format, Detail: "depend-info exited nonzero", ExitCode: &exit}
	}
	return nil
}

func contextFailure(ctx context.Context, stage string) *Failure {
	if ctx.Err() == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return &Failure{ID: FailureTimeout, Reason: ReasonDependInfoTimeout, Stage: stage, Detail: "whole-request deadline exceeded"}
	}
	return &Failure{ID: FailureCancelled, Reason: ReasonRequestCancelled, Stage: stage, Detail: "caller cancelled request"}
}

func outputDiagnostic(command Command, output RunOutput) Diagnostic {
	stream := output.Stderr
	streamName := "stderr"
	if len(stream.Data) == 0 {
		stream, streamName = output.Stdout, "stdout"
	}
	return Diagnostic{Stage: command.Stage, Candidate: command.Candidate, Format: command.Format, Stream: streamName, ByteCount: stream.Bytes, SHA256: stream.SHA256, Truncated: stream.Truncated, SafePrefix: redactDiagnostic(string(stream.Data))}
}

func unionPlans(plans []Plan) ([]Node, []Edge) {
	nodeSet := map[string]Node{}
	edgeSet := map[string]Edge{}
	for _, plan := range plans {
		for _, node := range plan.Nodes {
			nodeSet[node.Key()] = node
		}
		for _, edge := range plan.Edges {
			edgeSet[edge.key()] = edge
		}
	}
	nodes := make([]Node, 0, len(nodeSet))
	for _, node := range nodeSet {
		nodes = append(nodes, node)
	}
	edges := make([]Edge, 0, len(edgeSet))
	for _, edge := range edgeSet {
		edges = append(edges, edge)
	}
	sortNodes(nodes)
	sortEdges(edges)
	return nodes, edges
}

func resolveProvenance(ctx context.Context, args Args, graph ReducedGraph) ([]Provenance, *Failure) {
	names := map[string]struct{}{}
	for _, node := range graph.Targets {
		names[node.Name] = struct{}{}
	}
	for _, dependent := range graph.Transitive {
		names[dependent.Node.Name] = struct{}{}
	}
	for _, edge := range graph.Edges {
		names[edge.From.Name], names[edge.To.Name] = struct{}{}, struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	provenance := make([]Provenance, 0, len(ordered))
	for _, name := range ordered {
		resolutionArgs := portresolution.Args{Port: name, OverlayPorts: args.OverlayPorts}
		if args.ManifestRoot == "" {
			resolutionArgs.VcpkgRoot = args.VcpkgRoot
		}
		resolved := portresolution.ResolvePortContext(ctx, resolutionArgs, portresolution.DefaultDeps())
		row := Provenance{Port: name}
		if resolved.Status == evidence.StatusOK && resolved.Winner != nil {
			row.OverlayStatus = "found"
			row.Winner = resolved.Winner.Directory
			row.WinnerSource = resolved.Winner.Source
			if strings.HasPrefix(resolved.Winner.Source, "builtin") {
				row.OverlayStatus = "none"
				row.BaseSource = "builtin"
			}
			for _, shadow := range resolved.Shadows {
				row.Shadows = append(row.Shadows, shadow.Directory)
			}
		} else if args.ManifestRoot != "" && (resolved.Reason == portresolution.ReasonPortNotFound || resolved.Reason == portresolution.ReasonNoRootsSupplied) {
			row.OverlayStatus = "none"
			row.BaseSource = "registry_or_builtin"
		} else {
			return provenance, &Failure{ID: FailureOverlayUnknown, Reason: ReasonOverlayProvenanceUnknown, Stage: "provenance", Candidate: name, Detail: "port resolution could not settle overlay provenance"}
		}
		provenance = append(provenance, row)
	}
	return provenance, nil
}

func withFailure(result Result, failure *Failure) Result {
	if failure == nil {
		failure = universeFailure("unspecified failure")
	}
	result.Status = evidence.StatusUnknown
	result.Reason = failure.Reason
	result.Failure = failure
	result.Coverage.Complete = false
	return result
}

func renderCommand(command Command) string {
	return command.Executable + " " + strings.Join(command.Args, " ")
}

func firstNonemptyLine(value string) string {
	for _, line := range strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func environmentKeys(environment []string) []string {
	keys := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if found && key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return compactStrings(keys)
}

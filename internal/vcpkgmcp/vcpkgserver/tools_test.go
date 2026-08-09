package vcpkgserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/cmakegraph"
	"mcp-local-hub/internal/vcpkgmcp/cmaketrace"
	"mcp-local-hub/internal/vcpkgmcp/cmakewrap"
	"mcp-local-hub/internal/vcpkgmcp/discovery"
	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/lastfailure"
	"mcp-local-hub/internal/vcpkgmcp/patchesapply"
	"mcp-local-hub/internal/vcpkgmcp/pinstatus"
	"mcp-local-hub/internal/vcpkgmcp/portresolution"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

type registeredVcpkgToolFixture struct {
	name       string
	wantReason string
	under      func() publicresult.Projectable
	over       func() publicresult.Projectable
}

func TestPR591_RootReasonVocabularyMatchesOwnersAndCanonicalREADME(t *testing.T) {
	tools := liveRegisteredTools(t)
	discover := registeredToolByName(t, tools, "vcpkg_discover_root")
	patches := registeredToolByName(t, tools, "vcpkg_patches_apply")
	discoveryReason := string(discovery.ReasonExplicitRootRelative)
	patchReasons := []string{
		string(patchesapply.ReasonTooManyOverlayTripletRoots),
		string(patchesapply.ReasonRelativeOverlayTripletRoot),
		string(patchesapply.ReasonRelativeVcpkgRoot),
	}
	for label, value := range map[string]string{
		"discover descriptor": discover.Description,
		"discover root":       toolInputDescription(t, discover, "root"),
	} {
		if !strings.Contains(value, discoveryReason) || !strings.Contains(value, "terminal") || !strings.Contains(value, "before every") {
			t.Fatalf("%s omits owner reason %q: %q", label, discoveryReason, value)
		}
	}
	for _, reason := range patchReasons {
		if !strings.Contains(patches.Description, reason) {
			t.Fatalf("patch descriptor omits owner reason %q", reason)
		}
	}
	if got := toolInputDescription(t, patches, "vcpkg_root"); !strings.Contains(got, string(patchesapply.ReasonRelativeVcpkgRoot)) {
		t.Fatalf("vcpkg_root description omits owner reason: %q", got)
	}
	overlayDescription := toolInputDescription(t, patches, "overlay_triplets")
	for _, reason := range patchReasons[:2] {
		if !strings.Contains(overlayDescription, reason) {
			t.Fatalf("overlay_triplets description omits owner reason %q: %q", reason, overlayDescription)
		}
	}

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(testFile), "..", "..", ".."))
	readme, err := os.ReadFile(filepath.Join(repoRoot, "servers", "vcpkg", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range append([]string{discoveryReason}, patchReasons...) {
		if !bytes.Contains(readme, []byte(reason)) {
			t.Fatalf("canonical README omits owner reason %q", reason)
		}
	}
	if !bytes.Contains(readme, []byte("before any filesystem")) || !bytes.Contains(readme, []byte("All three failures occur before filesystem access")) {
		t.Fatal("canonical README omits terminal pre-I/O semantics")
	}
	argsSource, err := os.ReadFile(filepath.Join(repoRoot, "internal", "vcpkgmcp", "patchesapply", "patchesapply.go"))
	if err != nil {
		t.Fatal(err)
	}
	currentComment := "failed(" + string(patchesapply.ReasonRelativeVcpkgRoot) + ") before filesystem access"
	if !bytes.Contains(argsSource, []byte(currentComment)) {
		t.Fatalf("Args.VcpkgRoot comment omits current contract %q", currentComment)
	}
	observations := map[string]string{
		"Args.VcpkgRoot comment":    string(argsSource),
		"canonical README":          string(readme),
		"discover descriptor":       discover.Description,
		"discover root property":    toolInputDescription(t, discover, "root"),
		"patches descriptor":        patches.Description,
		"vcpkg_root property":       toolInputDescription(t, patches, "vcpkg_root"),
		"overlay_triplets property": overlayDescription,
	}
	for label, observation := range observations {
		if strings.Contains(strings.ToLower(observation), "relative value is ignored") {
			t.Fatalf("%s retains superseded ignored-relative-root assertion", label)
		}
	}
}

func TestPinStatusLiveDescriptionUsesExactTypedReasonVocabularies(t *testing.T) {
	perPortVocabulary, batchVocabulary, noPortDirs, tooManyPortDirs, relativePortDir, commitPinAbbreviated, refUnresolvable, networkDisabled := pinStatusReasonVocabularies()
	registry := pinstatus.PublicReasonRegistry()
	description := pinStatusToolDescription()
	if strings.Count(description, "Batch reasons are a closed enum: "+batchVocabulary+".") != 1 {
		t.Fatalf("live description does not render exactly the batch registry vocabulary: %q", description)
	}
	if strings.Count(description, "Per-port reasons are a closed enum: "+perPortVocabulary+".") != 1 {
		t.Fatalf("live description does not render exactly the per-port registry vocabulary: %q", description)
	}
	if !strings.Contains(description, "unknown("+noPortDirs+")") || !strings.Contains(description, "unknown("+tooManyPortDirs+")") {
		t.Fatalf("live description does not render both registry-derived batch reasons: %q", description)
	}
	if !strings.Contains(description, "omitted or empty port_dirs is unknown("+string(pinstatus.BatchReasonNoPortDirs)+")") {
		t.Fatalf("live description does not bind empty input to its typed batch reason: %q", description)
	}
	if !strings.Contains(description, "A batch over the package limit is unknown("+string(pinstatus.BatchReasonTooManyPortDirs)+")") {
		t.Fatalf("live description does not bind the over-limit batch to its typed reason: %q", description)
	}
	for _, reason := range []string{relativePortDir, commitPinAbbreviated, refUnresolvable} {
		if !strings.Contains(description, reason) {
			t.Fatalf("live description omits registry-derived named per-port reason %q: %q", reason, description)
		}
	}
	if schema := pinStatusPortDirsDescription(); !strings.Contains(schema, relativePortDir) || !strings.Contains(schema, noPortDirs) || !strings.Contains(schema, tooManyPortDirs) {
		t.Fatalf("port_dirs schema is not registry-derived: %q", schema)
	}
	if schema := pinStatusDisableNetworkDescription(); !strings.Contains(schema, networkDisabled) {
		t.Fatalf("disable_network schema is not registry-derived: %q", schema)
	}
	for _, reason := range registry.PerPort() {
		if !strings.Contains(perPortVocabulary, string(reason)) {
			t.Fatalf("typed per-port reason %q is absent from %q", reason, perPortVocabulary)
		}
		if strings.Contains(batchVocabulary, string(reason)) {
			t.Fatalf("per-port reason %q leaked into batch vocabulary %q", reason, batchVocabulary)
		}
	}
	for _, batch := range registry.Batch() {
		if !strings.Contains(batchVocabulary, string(batch)) {
			t.Fatalf("typed batch reason %q is absent from %q", batch, batchVocabulary)
		}
		if strings.Contains(perPortVocabulary, string(batch)) {
			t.Fatalf("batch reason %q leaked into per-port vocabulary %q", batch, perPortVocabulary)
		}
	}
}

func TestPinStatusLiveDescriptionsHaveNoReasonLiteralShadowOwner(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "tools.go"))
	if err != nil {
		t.Fatalf("read tools.go: %v", err)
	}
	text := string(source)
	start := strings.Index(text, "func pinStatusToolDescription")
	end := strings.Index(text, "type projectableToolOutcome")
	if start < 0 || end <= start {
		t.Fatal("could not isolate pin-status description helpers")
	}
	section := text[start:end]
	for _, reason := range pinstatus.PublicReasonRegistry().PerPort() {
		if strings.Contains(section, strconv.Quote(string(reason))) {
			t.Fatalf("per-port reason %q survives as a description literal", reason)
		}
	}
	for _, reason := range pinstatus.PublicReasonRegistry().Batch() {
		if strings.Contains(section, strconv.Quote(string(reason))) {
			t.Fatalf("batch reason %q survives as a description literal", reason)
		}
	}
}

func TestPinStatusJSONResultRedactsRemoteMetadataAtActualSerializer(t *testing.T) {
	const secret = "serializer-repo-secret"
	result, err := jsonResult(pinstatus.Result{
		Status: "ok",
		Ports: []pinstatus.PortResult{{
			Remote: pinstatus.Remote{
				Kind: pinstatus.RemoteGitHub,
				Repo: "group/token=" + secret,
				URL:  "https://github.com/group/token=" + secret + ".git",
			},
		}},
	})
	if err != nil {
		t.Fatalf("jsonResult: %v", err)
	}
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("jsonResult envelope = %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("jsonResult content = %T, want *mcp.TextContent", result.Content[0])
	}
	if strings.Contains(text.Text, secret) {
		t.Fatalf("actual vcpkg serializer leaked remote metadata secret: %s", text.Text)
	}
	if !strings.Contains(text.Text, "REDACTED") {
		t.Fatalf("actual vcpkg serializer did not visibly redact metadata: %s", text.Text)
	}
}

func TestPinStatusJSONResultRedactsRepoOnlyEvidenceAtActualSerializer(t *testing.T) {
	const carrier = "user:password@host"
	result, err := jsonResult(pinstatus.Result{Status: "unknown", Ports: []pinstatus.PortResult{{
		Remote:   pinstatus.Remote{Kind: pinstatus.RemoteGitHub, Repo: carrier, URL: "https://github.com/safe/repo.git"},
		Evidence: evidence.Evidence{Commands: []string{"git ls-remote " + carrier}},
		Failure:  &pinstatus.PublicFailure{ID: pinstatus.FailureRemoteQueryFailed, Detail: "remote query failed"},
	}}})
	if err != nil {
		t.Fatalf("jsonResult: %v", err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || strings.Contains(text.Text, carrier) {
		t.Fatalf("actual serializer leaked repo-only evidence carrier: %#v", result)
	}
}

func TestPinStatusJSONResultRedactsIndependentEvidenceCommandAtActualSerializer(t *testing.T) {
	const secret = "actual-serializer-command-secret"
	result, err := jsonResult(pinstatus.Result{Status: "unknown", Ports: []pinstatus.PortResult{{
		Remote:   pinstatus.Remote{Kind: pinstatus.RemoteGitHub, URL: "https://host/safe/repo.git"},
		Evidence: evidence.Evidence{Commands: []string{"git ls-remote https://user:" + secret + "@host/repo.git"}},
	}}})
	if err != nil {
		t.Fatalf("jsonResult: %v", err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || strings.Contains(text.Text, secret) || !strings.Contains(text.Text, "REDACTED") {
		t.Fatalf("actual serializer leaked independent evidence command: %#v", result)
	}
}

func TestPinStatusJSONResultRedactsSolCredentialCarriersAtActualSerializer(t *testing.T) {
	tests := []struct {
		name    string
		command string
		secret  string
	}{
		{"malformed encoded delimiter", "git ls-remote group/token%3Dactual-malformed-secret%ZZ", "actual-malformed-secret"},
		{"fused credential key", "git ls-remote group/apikey=actual-fused-secret", "actual-fused-secret"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := jsonResult(pinstatus.Result{Status: "unknown", Ports: []pinstatus.PortResult{{
				Remote:   pinstatus.Remote{Kind: pinstatus.RemoteGitHub, URL: "https://host/safe/repo.git"},
				Evidence: evidence.Evidence{Commands: []string{tc.command}},
			}}})
			if err != nil {
				t.Fatalf("jsonResult: %v", err)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok || strings.Contains(text.Text, tc.secret) || !strings.Contains(text.Text, "REDACTED") {
				t.Fatalf("actual serializer leaked %q: %#v", tc.secret, result)
			}
		})
	}
}

var registeredVcpkgToolFixtures = []registeredVcpkgToolFixture{
	{
		name:  "vcpkg_discover_root",
		under: func() publicresult.Projectable { return discovery.Result{} },
		over: func() publicresult.Projectable {
			return discovery.Result{Candidates: oversizedDiscoveryCandidates()}
		},
	},
	{
		name:       "vcpkg_last_failure",
		wantReason: "port_not_specified",
		under:      func() publicresult.Projectable { return lastfailure.Result{} },
		over: func() publicresult.Projectable {
			return lastfailure.Result{Diagnostics: oversizedLastFailureDiagnostics()}
		},
	},
	{
		name:       "vcpkg_port_resolution",
		wantReason: "empty_port",
		under:      func() publicresult.Projectable { return portresolution.Result{} },
		over: func() publicresult.Projectable {
			return portresolution.Result{AllCandidates: oversizedPortResolutionCandidates()}
		},
	},
	{
		name:       "vcpkg_pin_status",
		wantReason: "no_port_dirs",
		under:      func() publicresult.Projectable { return pinstatus.Result{} },
		over: func() publicresult.Projectable {
			return pinstatus.Result{Ports: oversizedPinStatusPorts()}
		},
	},
	{
		name:       "vcpkg_patches_apply",
		wantReason: "empty_port_dir",
		under:      func() publicresult.Projectable { return patchesapply.Result{} },
		over: func() publicresult.Projectable {
			return patchesapply.Result{Applied: oversizedAppliedPatches()}
		},
	},
	{
		name:       "vcpkg_cmake_trace",
		wantReason: "trace_path_not_supplied",
		under:      func() publicresult.Projectable { return cmaketrace.Result{} },
		over: func() publicresult.Projectable {
			return cmaketrace.Result{Records: oversizedCMakeTraceRecords()}
		},
	},
	{
		name:       "cmake_include_graph",
		wantReason: "args_invalid",
		under:      func() publicresult.Projectable { return cmakewrap.Result{} },
		over: func() publicresult.Projectable {
			return cmakewrap.Result{Files: oversizedStrings()}
		},
	},
}

func TestAllRegisteredToolsAreProjectableWithoutChangingCompleteWireShape(t *testing.T) {
	for _, tc := range registeredVcpkgToolFixtures {
		t.Run(tc.name, func(t *testing.T) {
			result := tc.under()
			want, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			wire, err := jsonResult(result)
			if err != nil {
				t.Fatal(err)
			}
			if wire.IsError || len(wire.Content) != 1 {
				t.Fatalf("normal result envelope=%#v", wire)
			}
			content, ok := wire.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("content=%T, want *mcp.TextContent", wire.Content[0])
			}
			got := []byte(content.Text)
			if !bytes.Equal(got, want) {
				t.Fatalf("under-budget result changed\ngot:  %s\nwant: %s", got, want)
			}
			var body map[string]any
			if err := json.Unmarshal(got, &body); err != nil {
				t.Fatal(err)
			}
			if _, projected := body["result_projection"]; projected {
				t.Fatal("under-budget result unexpectedly has result_projection")
			}
		})
	}
}

type contentBoundaryResult struct {
	Text string `json:"text"`
}

func (r contentBoundaryResult) PublicResultProjection() any {
	return struct {
		ResultProjection publicresult.Projection `json:"result_projection"`
	}{publicresult.MinimalProjection("text")}
}

func TestJSONResultExactContentBoundary(t *testing.T) {
	maxText := largestCompleteContentText(t)
	for _, tc := range []struct {
		name       string
		textLength int
		projected  bool
	}{
		{name: "N-1", textLength: maxText - 1},
		{name: "N", textLength: maxText},
		{name: "N+1", textLength: maxText + 1, projected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, err := jsonResult(contentBoundaryResult{Text: strings.Repeat("x", tc.textLength)})
			if err != nil {
				t.Fatal(err)
			}
			if wire.IsError || len(wire.Content) != 1 {
				t.Fatalf("result envelope=%#v", wire)
			}
			content, ok := wire.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("content=%T, want *mcp.TextContent", wire.Content[0])
			}
			if len(content.Text) > publicresult.MaxEncodedBytes {
				t.Fatalf("content bytes=%d, limit=%d", len(content.Text), publicresult.MaxEncodedBytes)
			}
			var body map[string]any
			if err := json.Unmarshal([]byte(content.Text), &body); err != nil {
				t.Fatalf("content is not JSON: %v", err)
			}
			_, gotProjected := body["result_projection"]
			if gotProjected != tc.projected {
				t.Fatalf("projected=%v, want %v", gotProjected, tc.projected)
			}
		})
	}
}

func TestJSONResultOversizedLastFailureUsesSharedProjection(t *testing.T) {
	diagnostics := make([]lastfailure.Diagnostic, 128)
	for i := range diagnostics {
		diagnostics[i] = lastfailure.Diagnostic{Text: strings.Repeat("x", 4096)}
	}
	result := lastfailure.Result{
		Status:      lastfailure.Status("unknown"),
		Reason:      lastfailure.ReasonPhaseLogSizeLimitExceeded,
		Diagnostics: diagnostics,
	}
	complete, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(complete) <= publicresult.MaxEncodedBytes {
		t.Fatalf("test setup encoded %d bytes, want more than %d", len(complete), publicresult.MaxEncodedBytes)
	}

	wire, err := jsonResult(result)
	if err != nil {
		t.Fatal(err)
	}
	if wire.IsError || len(wire.Content) != 1 {
		t.Fatalf("result envelope=%#v", wire)
	}
	content, ok := wire.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content=%T, want *mcp.TextContent", wire.Content[0])
	}
	if len(content.Text) > publicresult.MaxEncodedBytes {
		t.Fatalf("content bytes=%d, limit=%d", len(content.Text), publicresult.MaxEncodedBytes)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(content.Text), &body); err != nil {
		t.Fatalf("content is not JSON: %v", err)
	}
	if body["status"] != "unknown" || body["reason"] != string(lastfailure.ReasonPhaseLogSizeLimitExceeded) {
		t.Fatalf("status/reason changed: %#v", body)
	}
	if _, ok := body["result_projection"]; !ok {
		t.Fatalf("shared projection metadata absent: %#v", body)
	}
}

func TestLastFailureHasNoPrivateWholeBodyBoundary(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "lastfailure", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"MaxResponseBytes",
		"responseBytes",
		"reduceOversizeResponse",
		"json.MarshalIndent",
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, token := range forbidden {
			if bytes.Contains(source, []byte(token)) {
				t.Errorf("%s contains private whole-body boundary token %q", path, token)
			}
		}
	}
}

func largestCompleteContentText(t *testing.T) int {
	t.Helper()
	low, high := 0, publicresult.MaxEncodedBytes
	for low < high {
		middle := low + (high-low+1)/2
		body, err := json.MarshalIndent(contentBoundaryResult{Text: strings.Repeat("x", middle)}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if len(body) <= publicresult.MaxEncodedBytes {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low
}

func TestRegisterTools(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "vcpkg-mcp-test", Version: "test"}, nil)
	registerTools(&VcpkgServer{server: server})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "vcpkg-mcp-test-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer clientSession.Close()

	tools, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	// wantReason is the documented verdict each tool must return for an EMPTY
	// argument object. An empty string means the tool legitimately answers
	// from ambient discovery with no arguments at all, so no refusal is
	// expected — stated explicitly rather than left as a silently skipped row
	// (vcpkg_discover_root is the only such tool: resolving the root from the
	// environment with no arguments IS its job).
	//
	// The expected values are the ones the tools actually produce, confirmed by
	// running this test: vcpkg_last_failure refuses on the missing PORT
	// (ReasonPortNotSpecified) before it ever tries to resolve a root, which is
	// the correct precedence and not what an author would guess.
	//
	// VACUOUS-TEST FIX (2026-07-27): the `_invalid_input` subtests used to
	// assert only `result != nil && len(result.Content) != 0`. Every handler
	// ends in jsonResult(res), which produces content for EVERY possible
	// verdict, so those subtests reported only that the round-trip did not
	// blow up — nothing about refusing invalid input, which is what their name
	// claims. They would have stayed green if a tool answered `ok` for empty
	// input. The three rows without `invalidInput` were `continue`d past and
	// got no subtest at all.
	expected := make(map[string]bool, len(registeredVcpkgToolFixtures))
	for _, tc := range registeredVcpkgToolFixtures {
		expected[tc.name] = false
	}
	registered := make(map[string]struct{}, len(tools.Tools))
	for _, tool := range tools.Tools {
		if _, duplicate := registered[tool.Name]; duplicate {
			t.Errorf("registration_live_set: duplicate %q", tool.Name)
		}
		registered[tool.Name] = struct{}{}
		if _, known := expected[tool.Name]; !known {
			t.Errorf("registration_live_set: unexpected %q", tool.Name)
			continue
		}
		expected[tool.Name] = true
	}
	if len(registered) != len(expected) || len(tools.Tools) != len(expected) {
		t.Errorf("registration_live_set: count got=%d unique=%d want=%d", len(tools.Tools), len(registered), len(expected))
	}
	for name, registered := range expected {
		if !registered {
			t.Errorf("registration_live_set: missing %q", name)
		}
	}

	for _, tc := range registeredVcpkgToolFixtures {
		if tc.wantReason == "" {
			continue
		}
		t.Run(tc.name+"_invalid_input", func(t *testing.T) {
			result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
				Name:      tc.name,
				Arguments: map[string]any{},
			})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if result == nil || len(result.Content) == 0 {
				t.Fatalf("CallTool returned no well-formed result: %#v", result)
			}
			// A tool's inability to answer is expressed IN the JSON body as a
			// tri-state verdict, never as an MCP protocol error (see
			// helpers.go errResult). So the refusal has to be read out of the
			// body, which is also the only place a regression would show.
			if result.IsError {
				t.Fatalf("empty arguments produced an MCP protocol error; the contract is a normal result carrying "+
					"unknown/failed(<reason>): %#v", result)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("content[0] = %T, want *mcp.TextContent", result.Content[0])
			}
			var body struct {
				Status string `json:"status"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
				t.Fatalf("tool body is not the documented JSON result shape: %v\nbody=%s", err, text.Text)
			}
			if body.Status == "ok" {
				t.Fatalf("%s answered status=ok for EMPTY arguments — a tool that requires input must refuse it, "+
					"not report success; body=%s", tc.name, text.Text)
			}
			if body.Reason != tc.wantReason {
				t.Fatalf("%s: reason = %q, want %q — the documented refusal for empty input; body=%s",
					tc.name, body.Reason, tc.wantReason, text.Text)
			}
		})
	}

	// Receiving-side echo for PR #591: exercise the actual MCP registration,
	// JSON argument decoder and JSON response encoder, not only package calls.
	type wirePort struct {
		PortDir string `json:"port_dir"`
		Status  string `json:"status"`
		Reason  string `json:"reason"`
	}
	type wireBody struct {
		Status               string     `json:"status"`
		Reason               string     `json:"reason"`
		Ports                []wirePort `json:"ports"`
		VersionHeaderPresent bool       `json:"version_header_present"`
		Records              []any      `json:"records"`
	}
	callBody := func(t *testing.T, name string, args map[string]any) wireBody {
		t.Helper()
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("CallTool(%s): %v", name, err)
		}
		if result == nil || result.IsError || len(result.Content) != 1 {
			t.Fatalf("CallTool(%s) returned invalid normal-result envelope: %#v", name, result)
		}
		content, ok := result.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatalf("CallTool(%s) content = %T, want *mcp.TextContent", name, result.Content[0])
		}
		var body wireBody
		if err := json.Unmarshal([]byte(content.Text), &body); err != nil {
			t.Fatalf("CallTool(%s) JSON: %v\nbody=%s", name, err, content.Text)
		}
		return body
	}

	t.Run("pr591_pin_status_schema_batch_admission_is_wire_visible", func(t *testing.T) {
		portDirs := make([]string, pinstatus.MaxPortDirs+1)
		for i := range portDirs {
			portDirs[i] = "relative/port"
		}
		result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "vcpkg_pin_status", Arguments: map[string]any{"port_dirs": portDirs}})
		if err != nil {
			t.Fatal(err)
		}
		if result == nil || !result.IsError || len(result.Content) != 1 {
			t.Fatalf("result=%#v, want schema tool error", result)
		}
		text, ok := result.Content[0].(*mcp.TextContent)
		if !ok || !strings.HasPrefix(text.Text, "invalid arguments:") {
			t.Fatalf("content=%#v, want invalid arguments schema error", result.Content)
		}

		tooLong := string(filepath.Separator) + strings.Repeat("x", pinstatus.MaxPortDirBytes+1)
		result, err = clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "vcpkg_pin_status", Arguments: map[string]any{"port_dirs": []string{tooLong}}})
		if err != nil {
			t.Fatal(err)
		}
		if result == nil || !result.IsError || len(result.Content) != 1 {
			t.Fatalf("oversize item result=%#v, want schema tool error", result)
		}
		text, ok = result.Content[0].(*mcp.TextContent)
		if !ok || !strings.HasPrefix(text.Text, "invalid arguments:") {
			t.Fatalf("oversize item content=%#v, want invalid arguments schema error", result.Content)
		}
	})

	t.Run("pr591_relative_paths_are_wire_visible", func(t *testing.T) {
		pin := callBody(t, "vcpkg_pin_status", map[string]any{"port_dirs": []string{"relative/port"}})
		if pin.Status != "ok" || len(pin.Ports) != 1 || pin.Ports[0].Status != "failed" ||
			pin.Ports[0].Reason != "relative_port_dir" || pin.Ports[0].PortDir != "relative/port" {
			t.Fatalf("pin-status wire body = %+v, want ok batch with failed(relative_port_dir) entry echoed unchanged", pin)
		}

		trace := callBody(t, "vcpkg_cmake_trace", map[string]any{"trace_path": "relative/trace.json"})
		if trace.Status != "failed" || trace.Reason != "relative_trace_path" {
			t.Fatalf("cmake-trace wire body = %+v, want failed(relative_trace_path)", trace)
		}
	})

	t.Run("pr591_unsupported_trace_major_is_wire_visible", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "trace.json")
		content := []byte("{\"version\":{\"major\":2,\"minor\":0}}\n" +
			"{\"file\":\"/proj/CMakeLists.txt\",\"line\":1,\"cmd\":\"project\",\"args\":[\"p\"]}\n")
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatalf("write trace fixture: %v", err)
		}

		trace := callBody(t, "vcpkg_cmake_trace", map[string]any{"trace_path": path})
		if trace.Status != "unknown" || trace.Reason != "unsupported_trace_version" ||
			!trace.VersionHeaderPresent || len(trace.Records) != 0 {
			t.Fatalf("cmake-trace wire body = %+v, want unknown(unsupported_trace_version), header present, no records", trace)
		}
	})
}

func TestPR591_CMakeIncludeGraphDescriptorMatchesContracts(t *testing.T) {
	tool := registeredToolByName(t, liveRegisteredTools(t), "cmake_include_graph")
	if !strings.Contains(tool.Description, string(cmakegraph.CoverageSymlinkDirectorySkipped)) {
		t.Fatalf("cmake_include_graph descriptor omits typed coverage reason %q: %q", cmakegraph.CoverageSymlinkDirectorySkipped, tool.Description)
	}
	for _, property := range []string{"root", "file", "workspace_root"} {
		description := toolInputDescription(t, tool, property)
		if !strings.Contains(strings.ToLower(description), "absolute") || !strings.Contains(description, string(cmakewrap.ReasonArgsInvalid)) {
			t.Fatalf("cmake_include_graph %s descriptor = %q, want absolute and unknown(%s)", property, description, cmakewrap.ReasonArgsInvalid)
		}
	}
}

func TestPR591_PatchesApplyDescriptorNamesDeferredInvocation(t *testing.T) {
	tool := registeredToolByName(t, liveRegisteredTools(t), "vcpkg_patches_apply")
	if !strings.Contains(tool.Description, "invocation") || !strings.Contains(tool.Description, "${ARGN}") || !strings.Contains(tool.Description, string(patchesapply.ReasonPatchesDeferredCommandBody)) {
		t.Fatalf("patches descriptor does not name deferred invocation forwarding contract: %q", tool.Description)
	}
}

func TestPortResolutionSchemaMaxItemsMatchesPackageLimit(t *testing.T) {
	tool := registeredToolByName(t, liveRegisteredTools(t), "vcpkg_port_resolution")
	schema := tool.InputSchema.(map[string]any)
	properties := schema["properties"].(map[string]any)
	overlays := properties["overlay_ports"].(map[string]any)
	if got, ok := overlays["maxItems"].(float64); !ok || got != float64(portresolution.MaxOverlayRoots) {
		t.Fatalf("overlay_ports.maxItems = %#v, want package limit %d", overlays["maxItems"], portresolution.MaxOverlayRoots)
	}
}

func liveRegisteredTools(t *testing.T) []*mcp.Tool {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "vcpkg-mcp-descriptor-test", Version: "test"}, nil)
	registerTools(&VcpkgServer{server: server})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "vcpkg-mcp-descriptor-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer clientSession.Close()
	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	return listed.Tools
}

func registeredToolByName(t *testing.T, tools []*mcp.Tool, name string) *mcp.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not registered", name)
	return nil
}

func toolInputDescription(t *testing.T, tool *mcp.Tool, property string) string {
	t.Helper()
	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("%s InputSchema type = %T, want map[string]any", tool.Name, tool.InputSchema)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("%s properties = %#v", tool.Name, schema["properties"])
	}
	field, ok := properties[property].(map[string]any)
	if !ok {
		t.Fatalf("%s.%s schema = %#v", tool.Name, property, properties[property])
	}
	description, ok := field["description"].(string)
	if !ok {
		t.Fatalf("%s.%s description = %#v", tool.Name, property, field["description"])
	}
	return description
}

func TestProjectableAdapterBranches(t *testing.T) {
	sentinel := errors.New("adapter sentinel")
	for _, tc := range []struct {
		name     string
		handler  projectableToolHandler
		wantErr  error
		wantText string
	}{
		{
			name: "invalid_argument",
			handler: func(context.Context, *mcp.CallToolRequest) projectableToolOutcome {
				return projectableToolOutcome{invalidArgument: sentinel}
			},
			wantText: "invalid arguments: adapter sentinel",
		},
		{
			name: "ordinary_error",
			handler: func(context.Context, *mcp.CallToolRequest) projectableToolOutcome {
				return projectableToolOutcome{err: sentinel}
			},
			wantErr: sentinel,
		},
		{
			name: "nil_result",
			handler: func(context.Context, *mcp.CallToolRequest) projectableToolOutcome {
				return projectableToolOutcome{}
			},
			wantText: "internal invariant: nil projectable result",
		},
		{
			name: "success",
			handler: func(context.Context, *mcp.CallToolRequest) projectableToolOutcome {
				return projectableToolOutcome{result: discovery.Result{}}
			},
			wantText: "{}",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := callProjectableAdapter(t, tc.handler)
			if tc.wantErr != nil {
				outcome := tc.handler(context.Background(), &mcp.CallToolRequest{})
				if !errors.Is(outcome.err, tc.wantErr) {
					t.Fatalf("typed outcome error=%v, want errors.Is(_, %v)", outcome.err, tc.wantErr)
				}
				if err == nil || !strings.Contains(err.Error(), tc.wantErr.Error()) {
					t.Fatalf("adapter error=%v, want transport error containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || len(result.Content) != 1 {
				t.Fatalf("adapter result=%#v", result)
			}
			content, ok := result.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("adapter content=%T, want *mcp.TextContent", result.Content[0])
			}
			if !strings.Contains(content.Text, tc.wantText) {
				t.Fatalf("adapter body=%q, want text containing %q", content.Text, tc.wantText)
			}
		})
	}
}

func callProjectableAdapter(t *testing.T, handler projectableToolHandler) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "projectable-adapter-test", Version: "test"}, nil)
	registerProjectableTool(&VcpkgServer{server: server}, &mcp.Tool{
		Name:        "projectable_adapter_test",
		InputSchema: map[string]any{"type": "object"},
	}, handler)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "projectable-adapter-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer clientSession.Close()
	return clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "projectable_adapter_test", Arguments: map[string]any{}})
}

func TestRegisteredVcpkgToolsUseOnlyProjectableAdapter(t *testing.T) {
	if err := inspectRegisteredVcpkgTools(readVcpkgProductionSources(t), registeredVcpkgToolFixtures); err != nil {
		t.Fatal(err)
	}
}

func TestRegisteredVcpkgToolsOverBudgetProjectionBijection(t *testing.T) {
	for _, fixture := range registeredVcpkgToolFixtures {
		t.Run(fixture.name, func(t *testing.T) {
			if fixture.under == nil || fixture.over == nil {
				t.Fatal("registration_nil_fixture")
			}
			under := fixture.under()
			if under == nil {
				t.Fatal("registration_nil_projectable")
			}
			complete, err := json.MarshalIndent(under, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if len(complete) > publicresult.MaxEncodedBytes {
				t.Fatalf("under-budget fixture encoded %d bytes", len(complete))
			}
			wire, err := publicresult.MarshalIndent(under)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(wire, complete) {
				t.Fatal("under-budget result changed")
			}

			over := fixture.over()
			if over == nil {
				t.Fatal("registration_nil_projectable")
			}
			complete, err = json.MarshalIndent(over, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if len(complete) <= publicresult.MaxEncodedBytes {
				t.Fatalf("registration_projector_not_oversized: encoded %d bytes", len(complete))
			}
			wire, err = publicresult.MarshalIndent(over)
			if err != nil {
				t.Fatal(err)
			}
			if len(wire) > publicresult.MaxEncodedBytes {
				t.Fatalf("registration_projection_over_budget: bytes=%d limit=%d", len(wire), publicresult.MaxEncodedBytes)
			}
			var body map[string]any
			if err := json.Unmarshal(wire, &body); err != nil {
				t.Fatal(err)
			}
			projection, ok := body["result_projection"].(map[string]any)
			if !ok || projection["complete"] != false {
				t.Fatalf("registration_projection_missing: %#v", body["result_projection"])
			}
			omissions, ok := projection["omissions"].([]any)
			if !ok || len(omissions) == 0 {
				t.Fatalf("registration_projection_missing: omissions=%#v", projection["omissions"])
			}
		})
	}
}

func TestRegisteredVcpkgToolGuardRejectsDefectClasses(t *testing.T) {
	sources := readVcpkgProductionSources(t)
	toolsSource := string(vcpkgProductionSourceByName(t, sources, "tools.go").source)
	fixtures := append([]registeredVcpkgToolFixture(nil), registeredVcpkgToolFixtures...)
	for _, tc := range []struct {
		name      string
		sources   []vcpkgProductionSource
		fixtures  []registeredVcpkgToolFixture
		wantGuard string
	}{
		{
			name: "eighth_registration",
			sources: replaceVcpkgProductionSource(t, sources, "tools.go", strings.Replace(toolsSource,
				"\n}\n\nfunc (vs *VcpkgServer) discoverRootTool",
				"\n\tregisterProjectableTool(vs, &mcp.Tool{Name: \"eighth\"}, vs.discoverRootTool)\n}\n\nfunc (vs *VcpkgServer) discoverRootTool", 1)),
			fixtures: fixtures, wantGuard: "registration_count",
		},
		{
			name:     "duplicate_registration",
			sources:  replaceVcpkgProductionSource(t, sources, "tools.go", strings.Replace(toolsSource, "Name: \"vcpkg_last_failure\"", "Name: \"vcpkg_discover_root\"", 1)),
			fixtures: fixtures, wantGuard: "registration_duplicate_name",
		},
		{
			name:     "nonliteral_registration",
			sources:  replaceVcpkgProductionSource(t, sources, "tools.go", strings.Replace(toolsSource, "Name: \"vcpkg_discover_root\"", "Name: toolName", 1)),
			fixtures: fixtures, wantGuard: "registration_nonliteral_name",
		},
		{
			name:     "direct_addtool",
			sources:  replaceVcpkgProductionSource(t, sources, "tools.go", strings.Replace(toolsSource, "registerProjectableTool(vs, &mcp.Tool{", "vs.server.AddTool(&mcp.Tool{", 1)),
			fixtures: fixtures, wantGuard: "registration_direct_addtool",
		},
		{
			name:     "json_result_bypass",
			sources:  replaceVcpkgProductionSource(t, sources, "tools.go", strings.Replace(toolsSource, "return jsonResult(outcome.result)", "return otherJSONResult(outcome.result)", 1)),
			fixtures: fixtures, wantGuard: "registration_json_result_bypass",
		},
		{
			name: "handler_wire_escape",
			sources: replaceVcpkgProductionSource(t, sources, "tools.go", strings.Replace(toolsSource,
				"func (vs *VcpkgServer) discoverRootTool(ctx context.Context, req *mcp.CallToolRequest) projectableToolOutcome",
				"func (vs *VcpkgServer) discoverRootTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error)", 1)),
			fixtures: fixtures, wantGuard: "registration_handler_wire_escape",
		},
		{
			name: "cross_file_helper_addtool",
			sources: appendVcpkgProductionSource(
				replaceVcpkgProductionSource(t, sources, "tools.go", strings.Replace(toolsSource,
					"\n}\n\nfunc (vs *VcpkgServer) discoverRootTool",
					"\n\tregisterExtraVcpkgTool(vs)\n}\n\nfunc (vs *VcpkgServer) discoverRootTool", 1)),
				vcpkgProductionSource{name: "zz_cross_file_addtool.go", source: []byte("package vcpkgserver\n\nfunc registerExtraVcpkgTool(vs *VcpkgServer) {\n\tvs.server.AddTool(nil, nil)\n}\n")},
			),
			fixtures: fixtures, wantGuard: "registration_direct_addtool",
		},
		{
			name: "cross_file_json_result",
			sources: appendVcpkgProductionSource(sources,
				vcpkgProductionSource{name: "zz_cross_file_json.go", source: []byte("package vcpkgserver\n\nfunc alternateSerializationBoundary(v any) {\n\tjsonResult(v)\n}\n")},
			),
			fixtures: fixtures, wantGuard: "registration_json_result_bypass",
		},
		{
			name:    "missing_fixture",
			sources: sources, fixtures: fixtures[:len(fixtures)-1], wantGuard: "registration_fixture_bijection",
		},
		{
			name:    "nil_fixture",
			sources: sources, fixtures: append(fixtures, registeredVcpkgToolFixture{name: fixtures[0].name}), wantGuard: "registration_nil_fixture",
		},
		{
			name:    "nil_projectable",
			sources: sources, fixtures: replaceFixture(fixtures, 0, func(f *registeredVcpkgToolFixture) {
				f.under = func() publicresult.Projectable { return nil }
			}), wantGuard: "registration_nil_projectable",
		},
		{
			name:    "not_oversized",
			sources: sources, fixtures: replaceFixture(fixtures, 0, func(f *registeredVcpkgToolFixture) {
				f.over = f.under
			}), wantGuard: "registration_projector_not_oversized",
		},
		{
			name:    "missing_projection",
			sources: sources, fixtures: replaceFixture(fixtures, 0, func(f *registeredVcpkgToolFixture) {
				f.over = func() publicresult.Projectable { return oversizedMissingProjection{} }
			}), wantGuard: "registration_projection_missing",
		},
		{
			name:    "projection_over_budget",
			sources: sources, fixtures: replaceFixture(fixtures, 0, func(f *registeredVcpkgToolFixture) {
				f.over = func() publicresult.Projectable { return oversizedProjection{} }
			}), wantGuard: "registration_projection_over_budget",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := inspectRegisteredVcpkgTools(tc.sources, tc.fixtures)
			if err == nil || !strings.Contains(err.Error(), tc.wantGuard) {
				t.Fatalf("guard error=%v, want %q", err, tc.wantGuard)
			}
		})
	}
}

func replaceFixture(fixtures []registeredVcpkgToolFixture, index int, change func(*registeredVcpkgToolFixture)) []registeredVcpkgToolFixture {
	copy := append([]registeredVcpkgToolFixture(nil), fixtures...)
	change(&copy[index])
	return copy
}

type vcpkgProductionSource struct {
	name   string
	source []byte
}

func readVcpkgProductionSources(t *testing.T) []vcpkgProductionSource {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	filenames := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			filenames = append(filenames, name)
		}
	}
	sort.Strings(filenames)
	sources := make([]vcpkgProductionSource, 0, len(filenames))
	for _, name := range filenames {
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("registration_source_read: %s: %v", name, err)
		}
		sources = append(sources, vcpkgProductionSource{name: name, source: source})
	}
	return sources
}

func vcpkgProductionSourceByName(t *testing.T, sources []vcpkgProductionSource, name string) vcpkgProductionSource {
	t.Helper()
	for _, source := range sources {
		if source.name == name {
			return source
		}
	}
	t.Fatalf("registration_source_read: %s missing", name)
	return vcpkgProductionSource{}
}

func replaceVcpkgProductionSource(t *testing.T, sources []vcpkgProductionSource, name, replacement string) []vcpkgProductionSource {
	t.Helper()
	updated := append([]vcpkgProductionSource(nil), sources...)
	for index := range updated {
		if updated[index].name == name {
			updated[index].source = []byte(replacement)
			return updated
		}
	}
	t.Fatalf("registration_source_read: %s missing", name)
	return nil
}

func appendVcpkgProductionSource(sources []vcpkgProductionSource, source vcpkgProductionSource) []vcpkgProductionSource {
	updated := append([]vcpkgProductionSource(nil), sources...)
	updated = append(updated, source)
	sort.Slice(updated, func(left, right int) bool { return updated[left].name < updated[right].name })
	return updated
}

type vcpkgRegistrationInventory struct {
	functions        []*ast.FuncDecl
	packageFunctions map[string][]*ast.FuncDecl
	methods          map[string][]*ast.FuncDecl
}

func inspectRegisteredVcpkgTools(sources []vcpkgProductionSource, fixtures []registeredVcpkgToolFixture) error {
	inventory, err := inspectVcpkgProductionSources(sources)
	if err != nil {
		return err
	}
	registerTools, err := inventory.exactPackageFunction("registerTools")
	if err != nil {
		return err
	}
	registerProjectableTool, err := inventory.exactPackageFunction("registerProjectableTool")
	if err != nil {
		return err
	}
	if err := inspectSingleMCPBoundary(inventory.functions, registerProjectableTool); err != nil {
		return err
	}
	registrations, err := registrationsFromRegisterTools(inventory.functions, registerTools)
	if err != nil {
		return err
	}
	if len(registrations) != 7 {
		return fmt.Errorf("registration_count: got %d want 7", len(registrations))
	}
	registrationNames := map[string]struct{}{}
	for _, registration := range registrations {
		if _, duplicate := registrationNames[registration.name]; duplicate {
			return fmt.Errorf("registration_duplicate_name: %q", registration.name)
		}
		registrationNames[registration.name] = struct{}{}
		handler, err := inventory.exactVcpkgServerMethod(registration.handler)
		if err != nil || !hasProjectableOutcomeSignature(handler) || handlerContainsWireEscape(handler) {
			return fmt.Errorf("registration_handler_wire_escape: %s", registration.handler)
		}
	}
	return inspectFixtureBijection(registrationNames, fixtures)
}

func inspectVcpkgProductionSources(sources []vcpkgProductionSource) (vcpkgRegistrationInventory, error) {
	inventory := vcpkgRegistrationInventory{
		packageFunctions: map[string][]*ast.FuncDecl{},
		methods:          map[string][]*ast.FuncDecl{},
	}
	fileSet := token.NewFileSet()
	for _, source := range sources {
		parsed, err := parser.ParseFile(fileSet, source.name, source.source, parser.AllErrors)
		if err != nil {
			return vcpkgRegistrationInventory{}, fmt.Errorf("registration_parse: %s: %w", source.name, err)
		}
		if parsed.Name == nil || parsed.Name.Name != "vcpkgserver" {
			packageName := ""
			if parsed.Name != nil {
				packageName = parsed.Name.Name
			}
			return vcpkgRegistrationInventory{}, fmt.Errorf("registration_parse: %s: package %q", source.name, packageName)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			inventory.functions = append(inventory.functions, function)
			if function.Recv == nil {
				inventory.packageFunctions[function.Name.Name] = append(inventory.packageFunctions[function.Name.Name], function)
				continue
			}
			inventory.methods[methodKey(receiverTypeName(function), function.Name.Name)] = append(inventory.methods[methodKey(receiverTypeName(function), function.Name.Name)], function)
		}
	}
	return inventory, nil
}

func (inventory vcpkgRegistrationInventory) exactPackageFunction(name string) (*ast.FuncDecl, error) {
	functions := inventory.packageFunctions[name]
	if len(functions) != 1 {
		return nil, fmt.Errorf("registration_count: %s count=%d", name, len(functions))
	}
	return functions[0], nil
}

func (inventory vcpkgRegistrationInventory) exactVcpkgServerMethod(name string) (*ast.FuncDecl, error) {
	methods := inventory.methods[methodKey("VcpkgServer", name)]
	if len(methods) != 1 {
		return nil, fmt.Errorf("registration_handler_wire_escape: %s count=%d", name, len(methods))
	}
	return methods[0], nil
}

func receiverTypeName(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return ""
	}
	switch receiver := function.Recv.List[0].Type.(type) {
	case *ast.Ident:
		return receiver.Name
	case *ast.StarExpr:
		return fieldName(receiver.X)
	}
	return ""
}

func methodKey(receiver, name string) string {
	return receiver + "\x00" + name
}

type parsedRegistration struct {
	name    string
	handler string
}

func registrationsFromRegisterTools(functions []*ast.FuncDecl, registerTools *ast.FuncDecl) ([]parsedRegistration, error) {
	registrations := []parsedRegistration{}
	var guardErr error
	for _, function := range functions {
		if function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if guardErr != nil || node == nil {
				return guardErr == nil
			}
			call, ok := node.(*ast.CallExpr)
			if !ok || callName(call) != "registerProjectableTool" {
				return true
			}
			if function != registerTools {
				guardErr = fmt.Errorf("registration_count: registerProjectableTool owner %s", function.Name.Name)
				return false
			}
			if len(call.Args) != 3 {
				guardErr = errors.New("registration_count: malformed registration")
				return false
			}
			tool, ok := call.Args[1].(*ast.UnaryExpr)
			if !ok || tool.Op != token.AND {
				guardErr = errors.New("registration_nonliteral_name: tool is not an addressable literal")
				return false
			}
			literal, ok := tool.X.(*ast.CompositeLit)
			if !ok {
				guardErr = errors.New("registration_nonliteral_name: tool is not a literal")
				return false
			}
			name, ok := literalToolName(literal)
			if !ok {
				guardErr = errors.New("registration_nonliteral_name: name is not a string literal")
				return false
			}
			handler, ok := call.Args[2].(*ast.SelectorExpr)
			if !ok {
				guardErr = errors.New("registration_handler_wire_escape: handler is not a direct method")
				return false
			}
			handlerReceiver, directVcpkgServerMethod := handler.X.(*ast.Ident)
			if !directVcpkgServerMethod || handlerReceiver.Name != "vs" {
				guardErr = errors.New("registration_handler_wire_escape: handler is not a direct method")
				return false
			}
			registrations = append(registrations, parsedRegistration{name: name, handler: handler.Sel.Name})
			return true
		})
	}
	return registrations, guardErr
}

func literalToolName(literal *ast.CompositeLit) (string, bool) {
	for _, element := range literal.Elts {
		field, ok := element.(*ast.KeyValueExpr)
		if !ok || fieldName(field.Key) != "Name" {
			continue
		}
		value, ok := field.Value.(*ast.BasicLit)
		if !ok || value.Kind != token.STRING {
			return "", false
		}
		name, err := strconv.Unquote(value.Value)
		return name, err == nil && name != ""
	}
	return "", false
}

func inspectSingleMCPBoundary(functions []*ast.FuncDecl, registerProjectableTool *ast.FuncDecl) error {
	addTools, jsonResults := 0, 0
	badAddTool, badJSONResult := false, false
	for _, function := range functions {
		if function.Body == nil {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch callName(call) {
			case "AddTool":
				addTools++
				badAddTool = badAddTool || function != registerProjectableTool
			case "jsonResult":
				jsonResults++
				badJSONResult = badJSONResult || function != registerProjectableTool
			}
			return true
		})
	}
	if addTools != 1 || badAddTool {
		return fmt.Errorf("registration_direct_addtool: count=%d", addTools)
	}
	if jsonResults != 1 || badJSONResult {
		return fmt.Errorf("registration_json_result_bypass: count=%d", jsonResults)
	}
	return nil
}

func hasProjectableOutcomeSignature(function *ast.FuncDecl) bool {
	return function.Type.Results != nil && len(function.Type.Results.List) == 1 &&
		len(function.Type.Results.List[0].Names) == 0 && fieldName(function.Type.Results.List[0].Type) == "projectableToolOutcome"
}

func handlerContainsWireEscape(function *ast.FuncDecl) bool {
	escaped := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && (callName(call) == "AddTool" || callName(call) == "jsonResult") {
			escaped = true
			return false
		}
		return true
	})
	return escaped
}

func inspectFixtureBijection(registrationNames map[string]struct{}, fixtures []registeredVcpkgToolFixture) error {
	fixtureNames := map[string]struct{}{}
	for _, fixture := range fixtures {
		if fixture.name == "" || fixture.under == nil || fixture.over == nil {
			return errors.New("registration_nil_fixture")
		}
		if fixture.under() == nil || fixture.over() == nil {
			return errors.New("registration_nil_projectable")
		}
		fixtureNames[fixture.name] = struct{}{}
	}
	if len(fixtureNames) != len(fixtures) || len(fixtureNames) != len(registrationNames) {
		return errors.New("registration_fixture_bijection")
	}
	for name := range registrationNames {
		if _, ok := fixtureNames[name]; !ok {
			return fmt.Errorf("registration_fixture_bijection: missing %s", name)
		}
	}
	for name := range fixtureNames {
		if _, ok := registrationNames[name]; !ok {
			return fmt.Errorf("registration_fixture_bijection: stale %s", name)
		}
	}
	return inspectFixtureProjections(fixtures)
}

func inspectFixtureProjections(fixtures []registeredVcpkgToolFixture) error {
	for _, fixture := range fixtures {
		complete, err := json.MarshalIndent(fixture.over(), "", "  ")
		if err != nil {
			return fmt.Errorf("registration_projection_missing: %w", err)
		}
		if len(complete) <= publicresult.MaxEncodedBytes {
			return fmt.Errorf("registration_projector_not_oversized: %s", fixture.name)
		}
		projected, err := publicresult.MarshalIndent(fixture.over())
		if err != nil {
			if errors.Is(err, publicresult.ErrBudgetInvariant) {
				return fmt.Errorf("registration_projection_over_budget: %s", fixture.name)
			}
			return fmt.Errorf("registration_projection_missing: %w", err)
		}
		if len(projected) > publicresult.MaxEncodedBytes {
			return fmt.Errorf("registration_projection_over_budget: %s", fixture.name)
		}
		var body map[string]any
		if err := json.Unmarshal(projected, &body); err != nil {
			return fmt.Errorf("registration_projection_missing: %w", err)
		}
		projection, ok := body["result_projection"].(map[string]any)
		if !ok || projection["complete"] != false {
			return fmt.Errorf("registration_projection_missing: %s", fixture.name)
		}
		omissions, ok := projection["omissions"].([]any)
		if !ok || len(omissions) == 0 {
			return fmt.Errorf("registration_projection_missing: %s", fixture.name)
		}
	}
	return nil
}

func callName(call *ast.CallExpr) string {
	return fieldName(call.Fun)
}

func fieldName(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.Ident:
		return expression.Name
	case *ast.SelectorExpr:
		return expression.Sel.Name
	}
	return ""
}

func oversizedStrings() []string {
	values := make([]string, 128)
	for index := range values {
		values[index] = strings.Repeat("x", 4096)
	}
	return values
}

func oversizedDiscoveryCandidates() []discovery.Candidate {
	values := make([]discovery.Candidate, 128)
	for index := range values {
		values[index].Path = strings.Repeat("x", 4096)
	}
	return values
}

func oversizedLastFailureDiagnostics() []lastfailure.Diagnostic {
	values := make([]lastfailure.Diagnostic, 128)
	for index := range values {
		values[index].Text = strings.Repeat("x", 4096)
	}
	return values
}

func oversizedPortResolutionCandidates() []portresolution.CandidateLocation {
	values := make([]portresolution.CandidateLocation, 128)
	for index := range values {
		values[index].Directory = strings.Repeat("x", 4096)
	}
	return values
}

func oversizedPinStatusPorts() []pinstatus.PortResult {
	values := make([]pinstatus.PortResult, 128)
	for index := range values {
		values[index].PortDir = strings.Repeat("x", 4096)
	}
	return values
}

func oversizedAppliedPatches() []patchesapply.AppliedPatch {
	values := make([]patchesapply.AppliedPatch, 128)
	for index := range values {
		values[index].Filename = strings.Repeat("x", 4096)
	}
	return values
}

func oversizedCMakeTraceRecords() []cmaketrace.Record {
	values := make([]cmaketrace.Record, 128)
	for index := range values {
		values[index].Args = []string{strings.Repeat("x", 4096)}
	}
	return values
}

type oversizedMissingProjection struct{}

func (oversizedMissingProjection) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Text string `json:"text"`
	}{Text: strings.Repeat("x", publicresult.MaxEncodedBytes)})
}

func (oversizedMissingProjection) PublicResultProjection() any { return struct{}{} }

type oversizedProjection struct{}

func (oversizedProjection) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Text string `json:"text"`
	}{Text: strings.Repeat("x", publicresult.MaxEncodedBytes)})
}

func (oversizedProjection) PublicResultProjection() any { return oversizedProjection{} }

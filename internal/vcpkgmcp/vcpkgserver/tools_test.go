package vcpkgserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

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
	testCases := []struct {
		name       string
		wantReason string
	}{
		{name: "vcpkg_discover_root"},
		{name: "vcpkg_last_failure", wantReason: "port_not_specified"},
		{name: "cmake_include_graph", wantReason: "args_invalid"},
		{name: "vcpkg_port_resolution", wantReason: "empty_port"},
		{name: "vcpkg_pin_status", wantReason: "no_port_dirs"},
		{name: "vcpkg_patches_apply", wantReason: "empty_port_dir"},
		{name: "vcpkg_cmake_trace", wantReason: "trace_path_not_supplied"},
	}
	expected := make(map[string]bool, len(testCases))
	for _, tc := range testCases {
		expected[tc.name] = false
	}
	registered := make(map[string]struct{}, len(tools.Tools))
	for _, tool := range tools.Tools {
		if _, duplicate := registered[tool.Name]; duplicate {
			t.Errorf("duplicate tool registration: %q", tool.Name)
		}
		registered[tool.Name] = struct{}{}
		if _, known := expected[tool.Name]; known {
			expected[tool.Name] = true
		}
	}
	for name, registered := range expected {
		if !registered {
			t.Errorf("tool %q was not registered", name)
		}
	}

	for _, tc := range testCases {
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
}

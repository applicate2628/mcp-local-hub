package vcpkgserver

import (
	"context"
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

	testCases := []struct {
		name         string
		invalidInput bool
	}{
		{name: "vcpkg_discover_root"},
		{name: "vcpkg_last_failure"},
		{name: "cmake_include_graph"},
		{name: "vcpkg_port_resolution", invalidInput: true},
		{name: "vcpkg_pin_status", invalidInput: true},
		{name: "vcpkg_patches_apply", invalidInput: true},
		{name: "vcpkg_cmake_trace", invalidInput: true},
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
		if !tc.invalidInput {
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
		})
	}
}

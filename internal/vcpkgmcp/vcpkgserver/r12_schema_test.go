package vcpkgserver

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/cmakegraph"
	"mcp-local-hub/internal/vcpkgmcp/patchesapply"
)

func TestAllRegisteredToolsRejectUnknownArgumentsBeforeHandler(t *testing.T) {
	for _, advertised := range liveRegisteredTools(t) {
		advertised := advertised
		t.Run(advertised.Name, func(t *testing.T) {
			server := mcp.NewServer(&mcp.Implementation{Name: "strict-test", Version: "test"}, nil)
			var calls atomic.Int32
			tool := &mcp.Tool{Name: advertised.Name, Description: advertised.Description, InputSchema: advertised.InputSchema}
			if err := registerProjectableTool(&VcpkgServer{server: server}, tool, func(context.Context, *mcp.CallToolRequest) projectableToolOutcome {
				calls.Add(1)
				return projectableToolOutcome{}
			}); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			st, ct := mcp.NewInMemoryTransports()
			ss, err := server.Connect(ctx, st, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			client := mcp.NewClient(&mcp.Implementation{Name: "strict-client", Version: "test"}, nil)
			cs, err := client.Connect(ctx, ct, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			unknown := "unknown_member"
			if advertised.Name == "vcpkg_pin_status" {
				unknown = "disable_netwrok"
			}
			result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: advertised.Name, Arguments: map[string]any{unknown: true}})
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || !result.IsError || len(result.Content) != 1 {
				t.Fatalf("result=%#v", result)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok || !strings.HasPrefix(text.Text, "invalid arguments:") {
				t.Fatalf("content=%#v", result.Content)
			}
			if calls.Load() != 0 {
				t.Fatalf("handler calls=%d, want 0", calls.Load())
			}
		})
	}
}

func TestAllAdvertisedInputSchemasAreResolvedStrictObjects(t *testing.T) {
	for _, advertised := range liveRegisteredTools(t) {
		t.Run(advertised.Name, func(t *testing.T) {
			tool := &mcp.Tool{Name: advertised.Name, InputSchema: advertised.InputSchema}
			resolved, err := strictResolvedSchema(tool)
			if err != nil {
				t.Fatal(err)
			}
			if err := resolved.Validate(&map[string]any{"unknown_member": true}); err == nil {
				t.Fatal("unknown member validated")
			}
			if advertised.Name == "vcpkg_patches_apply" {
				if err := resolved.Validate(&map[string]any{"var_overrides": map[string]any{"CUSTOM_KEY": "value"}}); err != nil {
					t.Fatalf("intentional map rejected: %v", err)
				}
			}
			var raw map[string]any
			encoded, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &raw); err != nil {
				t.Fatal(err)
			}
			properties, _ := raw["properties"].(map[string]any)
			if advertised.Name == "vcpkg_patches_apply" {
				overlay, _ := properties["overlay_triplets"].(map[string]any)
				if got := int(overlay["maxItems"].(float64)); got != patchesapply.MaxOverlayTripletRoots {
					t.Fatalf("overlay maxItems=%d, want package constant %d", got, patchesapply.MaxOverlayTripletRoots)
				}
			}
			if advertised.Name == "cmake_include_graph" {
				entry, _ := properties["entry_names"].(map[string]any)
				if got := int(entry["maxItems"].(float64)); got != cmakegraph.MaxEntryFilters {
					t.Fatalf("entry_names maxItems=%d, want package constant %d", got, cmakegraph.MaxEntryFilters)
				}
				for name, want := range map[string]int64{
					"max_depth": int64(cmakegraph.MaxDepthLimit), "max_nodes": int64(cmakegraph.MaxNodesLimit),
					"max_file_bytes": cmakegraph.MaxFileBytesLimit, "max_roots": int64(cmakegraph.MaxRootsLimit),
				} {
					property, _ := properties[name].(map[string]any)
					if got := int64(property["maximum"].(float64)); got != want {
						t.Fatalf("%s maximum=%d, want %d", name, got, want)
					}
				}
			}
		})
	}
}

func TestRegistrationFailureReturnsFromRunOwner(t *testing.T) {
	register := func(vs *VcpkgServer) error {
		return registerProjectableTool(vs, &mcp.Tool{Name: "invalid", InputSchema: map[string]any{"type": "string"}}, func(context.Context, *mcp.CallToolRequest) projectableToolOutcome { return projectableToolOutcome{} })
	}
	server, err := newRegisteredServer(register)
	if err == nil || server != nil {
		t.Fatalf("server=%v err=%v, want no server and registration error", server, err)
	}
}

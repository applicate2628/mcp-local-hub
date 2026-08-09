package vcpkgserver

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestR33TraceSchemaRejectsNegativeMaxRecords(t *testing.T) {
	tool := registeredToolByName(t, liveRegisteredTools(t), "vcpkg_cmake_trace")
	schema := tool.InputSchema.(map[string]any)
	property := schema["properties"].(map[string]any)["max_records"].(map[string]any)
	if minimum, ok := property["minimum"].(float64); !ok || minimum != 0 {
		t.Fatalf("max_records.minimum=%v, want 0", property["minimum"])
	}
	resolved, err := strictResolvedSchema(&mcp.Tool{Name: tool.Name, InputSchema: tool.InputSchema})
	if err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(&map[string]any{"max_records": -1}); err == nil {
		t.Fatal("max_records=-1 unexpectedly validated")
	}
}

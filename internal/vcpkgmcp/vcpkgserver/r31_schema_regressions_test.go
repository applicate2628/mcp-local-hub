package vcpkgserver

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestR31GraphLimitSchemasRejectNegativeValues(t *testing.T) {
	for _, advertised := range liveRegisteredTools(t) {
		if advertised.Name != "cmake_include_graph" {
			continue
		}
		var schema map[string]any
		encoded, err := json.Marshal(advertised.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &schema); err != nil {
			t.Fatal(err)
		}
		properties := schema["properties"].(map[string]any)
		for _, name := range []string{"max_depth", "max_nodes", "max_file_bytes", "max_roots"} {
			property := properties[name].(map[string]any)
			if minimum, ok := property["minimum"].(float64); !ok || minimum != 0 {
				t.Fatalf("%s minimum=%v, want 0", name, property["minimum"])
			}
			resolved, err := strictResolvedSchema(&mcp.Tool{Name: advertised.Name, InputSchema: advertised.InputSchema})
			if err != nil {
				t.Fatal(err)
			}
			if err := resolved.Validate(&map[string]any{name: -1}); err == nil {
				t.Fatalf("%s=-1 unexpectedly validated", name)
			}
		}
		return
	}
	t.Fatal("cmake_include_graph tool not registered")
}

package reversedepgraph

import (
	"encoding/json"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

func TestProjectionNMinusOneNPlusOne(t *testing.T) {
	result := Result{Status: evidence.StatusOK, Query: Query{Port: "target", Triplet: "x64-windows", HostTriplet: "x64-windows"}}
	for i := 0; i < 5000; i++ {
		result.Transitive = append(result.Transitive, Dependent{Node: Node{Name: "port-" + strings.Repeat("x", 80), Role: RoleTarget, Triplet: "x64-windows"}, Distance: i + 1})
	}
	ordinary, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if len(ordinary) <= publicresult.MaxEncodedBytes {
		t.Fatalf("fixture not oversized: %d", len(ordinary))
	}
	wire, err := publicresult.MarshalIndent(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) > publicresult.MaxEncodedBytes {
		t.Fatalf("projection bytes=%d", len(wire))
	}
	var body struct {
		Projection publicresult.Projection `json:"result_projection"`
		Coverage   Coverage                `json:"coverage"`
		Query      Query                   `json:"query"`
	}
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatal(err)
	}
	if body.Projection.Complete || body.Query.Port != "target" {
		t.Fatalf("projection lost causal core: %s", wire)
	}
}

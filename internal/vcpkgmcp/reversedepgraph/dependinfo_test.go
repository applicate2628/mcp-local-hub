package reversedepgraph

import (
	"testing"
)

const resolvedFixtureDGML = `<?xml version="1.0" encoding="utf-8"?>
<DirectedGraph xmlns="http://schemas.microsoft.com/vs/2009/dgml">
  <Nodes>
    <Node Id="curl" />
    <Node Id="zlib" />
  </Nodes>
  <Links>
    <Link Source="curl" Target="zlib" />
  </Links>
</DirectedGraph>`

const resolvedFixtureList = `(0)curl[ssl, sspi]: zlib
(1)zlib:
`

func TestResolvedDGMLAndListContract(t *testing.T) {
	plan, failure := ParseResolvedPlan(
		[]byte(resolvedFixtureDGML), nil, []byte(resolvedFixtureList),
		"x64-windows", "x64-windows",
	)
	if failure != nil {
		t.Fatalf("ParseResolvedPlan failed: %#v", failure)
	}
	if len(plan.Nodes) != 2 || len(plan.Edges) != 1 {
		t.Fatalf("plan sizes = nodes %d edges %d, want 2/1: %#v", len(plan.Nodes), len(plan.Edges), plan)
	}
	wantCurl := Node{Name: "curl", Role: RoleTarget, Triplet: "x64-windows", Features: []string{"ssl", "sspi"}}
	if !plan.Nodes[0].Equal(wantCurl) {
		t.Fatalf("curl node = %#v, want %#v", plan.Nodes[0], wantCurl)
	}
	if plan.Edges[0].From.Name != "curl" || plan.Edges[0].To.Name != "zlib" {
		t.Fatalf("edge = %#v, want curl -> zlib", plan.Edges[0])
	}
}

func TestDGMLListMismatchFailsClosed(t *testing.T) {
	list := `(0)curl[ssl]: openssl
(1)openssl:
`
	_, failure := ParseResolvedPlan([]byte(resolvedFixtureDGML), nil, []byte(list), "x64-windows", "x64-windows")
	if failure == nil || failure.Reason != ReasonDependInfoOutputInconsistent || failure.ID != FailureOutputMismatch {
		t.Fatalf("failure = %#v, want %s/%s", failure, ReasonDependInfoOutputInconsistent, FailureOutputMismatch)
	}
}

func TestTargetHostOtherTripletIdentity(t *testing.T) {
	dgml := `<DirectedGraph xmlns="http://schemas.microsoft.com/vs/2009/dgml"><Nodes>` +
		`<Node Id="app"/><Node Id="tool:host"/><Node Id="lib:arm64-windows"/>` +
		`</Nodes><Links><Link Source="app" Target="tool:host"/><Link Source="app" Target="lib:arm64-windows"/></Links></DirectedGraph>`
	list := `(0)app: tool:host, lib:arm64-windows
(1)tool:host[core]:
(1)lib:arm64-windows[simd]:
`
	plan, failure := ParseResolvedPlan([]byte(dgml), []byte(list), nil, "x64-windows", "x64-windows-static")
	if failure != nil {
		t.Fatal(failure)
	}
	got := map[string]Node{}
	for _, node := range plan.Nodes {
		got[node.Name+"/"+string(node.Role)] = node
	}
	if got["tool/host"].Triplet != "x64-windows-static" || got["lib/other"].Triplet != "arm64-windows" {
		t.Fatalf("identity collapsed: %#v", got)
	}
}

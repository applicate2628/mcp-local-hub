package reversedepgraph

import "testing"

func targetNode(name string, features ...string) Node {
	return Node{Name: name, Role: RoleTarget, Triplet: "x64-windows", Features: features}
}

func TestReverseDiamondMinDistance(t *testing.T) {
	target := targetNode("target")
	a := targetNode("a")
	b := targetNode("b")
	c := targetNode("c")
	nodes := []Node{target, a, b, c}
	edges := []Edge{
		{From: a, To: target},
		{From: b, To: target},
		{From: c, To: a},
		{From: c, To: b},
	}
	graph := ReduceGraph("target", nodes, edges)
	if len(graph.Direct) != 2 || graph.Direct[0].Node.Name != "a" || graph.Direct[1].Node.Name != "b" {
		t.Fatalf("direct = %#v", graph.Direct)
	}
	if len(graph.Transitive) != 3 || graph.Transitive[2].Node.Name != "c" || graph.Transitive[2].Distance != 2 {
		t.Fatalf("transitive = %#v", graph.Transitive)
	}
	if got := graph.Transitive[2].Path; len(got) != 3 || got[0].Name != "c" || got[2].Name != "target" {
		t.Fatalf("canonical shortest path = %#v", got)
	}
}

func TestCycleSCCRetainsClosingEdges(t *testing.T) {
	a := targetNode("a")
	b := targetNode("b")
	c := targetNode("c")
	graph := ReduceGraph("a", []Node{a, b, c}, []Edge{{From: a, To: b}, {From: b, To: c}, {From: c, To: a}})
	if len(graph.Cycles) != 1 {
		t.Fatalf("cycles = %#v", graph.Cycles)
	}
	if len(graph.Cycles[0].Nodes) != 3 || len(graph.Cycles[0].Edges) != 3 {
		t.Fatalf("cycle lost nodes/closing edge: %#v", graph.Cycles[0])
	}
	for _, dependent := range graph.Transitive {
		if dependent.Node.Name == "a" {
			t.Fatalf("selected port leaked into dependent list: %#v", graph.Transitive)
		}
	}
}

func TestFeatureQualifiedNodesDoNotCollapse(t *testing.T) {
	target := targetNode("target")
	plain := targetNode("consumer")
	tls := targetNode("consumer", "tls")
	graph := ReduceGraph("target", []Node{target, plain, tls}, []Edge{{From: plain, To: target}, {From: tls, To: target}})
	if len(graph.Direct) != 2 || graph.Direct[0].Node.Key() == graph.Direct[1].Node.Key() {
		t.Fatalf("feature-qualified nodes collapsed: %#v", graph.Direct)
	}
}

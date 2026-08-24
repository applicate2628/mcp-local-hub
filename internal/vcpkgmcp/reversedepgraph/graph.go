package reversedepgraph

import "sort"

func ReduceGraph(selected string, rawNodes []Node, rawEdges []Edge) ReducedGraph {
	nodes := map[string]Node{}
	for _, node := range rawNodes {
		node = node.normalized()
		nodes[node.Key()] = node
	}
	edges := map[string]Edge{}
	for _, edge := range rawEdges {
		edge.From = edge.From.normalized()
		edge.To = edge.To.normalized()
		nodes[edge.From.Key()] = edge.From
		nodes[edge.To.Key()] = edge.To
		edges[edge.key()] = edge
	}

	reverse := map[string][]string{}
	for _, edge := range edges {
		reverse[edge.To.Key()] = append(reverse[edge.To.Key()], edge.From.Key())
	}
	for key := range reverse {
		sort.Slice(reverse[key], func(i, j int) bool { return nodeLess(nodes[reverse[key][i]], nodes[reverse[key][j]]) })
	}

	graph := ReducedGraph{}
	distance := map[string]int{}
	paths := map[string][]Node{}
	queue := []string{}
	for key, node := range nodes {
		if node.Name == selected {
			graph.Targets = append(graph.Targets, node)
			distance[key] = 0
			paths[key] = []Node{node}
			queue = append(queue, key)
		}
	}
	sortNodes(graph.Targets)
	sort.Slice(queue, func(i, j int) bool { return nodeLess(nodes[queue[i]], nodes[queue[j]]) })
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, dependent := range reverse[current] {
			candidateDistance := distance[current] + 1
			candidatePath := append([]Node{nodes[dependent]}, paths[current]...)
			existing, seen := distance[dependent]
			if seen && existing < candidateDistance {
				continue
			}
			if seen && existing == candidateDistance && !pathLess(candidatePath, paths[dependent]) {
				continue
			}
			distance[dependent] = candidateDistance
			paths[dependent] = candidatePath
			if !seen {
				queue = append(queue, dependent)
			}
		}
	}
	for key, dist := range distance {
		if dist == 0 || nodes[key].Name == selected {
			continue
		}
		dependent := Dependent{Node: nodes[key], Distance: dist, Path: paths[key]}
		graph.Transitive = append(graph.Transitive, dependent)
		if dist == 1 {
			graph.Direct = append(graph.Direct, dependent)
		}
	}
	sortDependents(graph.Direct)
	sortDependents(graph.Transitive)
	for _, edge := range edges {
		if _, fromRelevant := distance[edge.From.Key()]; fromRelevant {
			graph.Edges = append(graph.Edges, edge)
			continue
		}
		if _, toRelevant := distance[edge.To.Key()]; toRelevant {
			graph.Edges = append(graph.Edges, edge)
		}
	}
	sortEdges(graph.Edges)
	graph.Cycles = graphCycles(nodes, edges, distance)
	return graph
}

func pathLess(left, right []Node) bool {
	for index := 0; index < len(left) && index < len(right); index++ {
		if left[index].Key() == right[index].Key() {
			continue
		}
		return nodeLess(left[index], right[index])
	}
	return len(left) < len(right)
}

func sortDependents(values []Dependent) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].Distance != values[j].Distance {
			return values[i].Distance < values[j].Distance
		}
		return nodeLess(values[i].Node, values[j].Node)
	})
}

func graphCycles(nodes map[string]Node, edges map[string]Edge, relevant map[string]int) []Cycle {
	adjacency := map[string][]string{}
	selfEdge := map[string]bool{}
	for _, edge := range edges {
		adjacency[edge.From.Key()] = append(adjacency[edge.From.Key()], edge.To.Key())
		if edge.From.Key() == edge.To.Key() {
			selfEdge[edge.From.Key()] = true
		}
	}
	for key := range adjacency {
		sort.Strings(adjacency[key])
	}
	index := 0
	indices, low := map[string]int{}, map[string]int{}
	stack := []string{}
	onStack := map[string]bool{}
	components := [][]string{}
	var visit func(string)
	visit = func(key string) {
		indices[key], low[key] = index, index
		index++
		stack = append(stack, key)
		onStack[key] = true
		for _, next := range adjacency[key] {
			if _, seen := indices[next]; !seen {
				visit(next)
				if low[next] < low[key] {
					low[key] = low[next]
				}
			} else if onStack[next] && indices[next] < low[key] {
				low[key] = indices[next]
			}
		}
		if low[key] != indices[key] {
			return
		}
		component := []string{}
		for {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[last] = false
			component = append(component, last)
			if last == key {
				break
			}
		}
		components = append(components, component)
	}
	keys := make([]string, 0, len(nodes))
	for key := range nodes {
		if _, ok := relevant[key]; ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, seen := indices[key]; !seen {
			visit(key)
		}
	}
	cycles := []Cycle{}
	for _, component := range components {
		if len(component) == 1 && !selfEdge[component[0]] {
			continue
		}
		member := map[string]bool{}
		cycle := Cycle{}
		for _, key := range component {
			member[key] = true
			cycle.Nodes = append(cycle.Nodes, nodes[key])
		}
		sortNodes(cycle.Nodes)
		for _, edge := range edges {
			if member[edge.From.Key()] && member[edge.To.Key()] {
				cycle.Edges = append(cycle.Edges, edge)
			}
		}
		sortEdges(cycle.Edges)
		cycles = append(cycles, cycle)
	}
	sort.Slice(cycles, func(i, j int) bool { return nodeLess(cycles[i].Nodes[0], cycles[j].Nodes[0]) })
	return cycles
}

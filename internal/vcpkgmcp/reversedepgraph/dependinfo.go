package reversedepgraph

import (
	"bytes"
	"encoding/xml"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"mcp-local-hub/internal/vcpkgmcp/portname"
)

type dgmlDocument struct {
	XMLName xml.Name   `xml:"DirectedGraph"`
	Nodes   []dgmlNode `xml:"Nodes>Node"`
	Links   []dgmlLink `xml:"Links>Link"`
}

type dgmlNode struct {
	ID string `xml:"Id,attr"`
}
type dgmlLink struct {
	Source string `xml:"Source,attr"`
	Target string `xml:"Target,attr"`
}

func ParseResolvedPlan(dgml, listStdout, listStderr []byte, triplet, hostTriplet string) (Plan, *Failure) {
	if len(dgml) == 0 || len(dgml) > MaxStreamBytes || !utf8.Valid(dgml) {
		return Plan{}, &Failure{ID: FailureUnparseable, Reason: ReasonDependInfoOutputUnparseable, Stage: "parse_dgml", Detail: "invalid or over-limit UTF-8 DGML"}
	}
	list := append(append([]byte(nil), listStdout...), listStderr...)
	if len(list) == 0 || len(list) > 2*MaxStreamBytes || !utf8.Valid(list) {
		return Plan{}, &Failure{ID: FailureUnparseable, Reason: ReasonDependInfoOutputUnparseable, Stage: "parse_list", Detail: "invalid or over-limit UTF-8 list output"}
	}

	var document dgmlDocument
	decoder := xml.NewDecoder(bytes.NewReader(dgml))
	decoder.Strict = true
	if err := decoder.Decode(&document); err != nil || document.XMLName.Local != "DirectedGraph" {
		return Plan{}, &Failure{ID: FailureUnparseable, Reason: ReasonDependInfoOutputUnparseable, Stage: "parse_dgml", Detail: "DGML syntax"}
	}

	listNodes := map[string]Node{}
	listEdges := map[string][2]string{}
	diagnostics := []Diagnostic{}
	for _, rawLine := range strings.Split(strings.ReplaceAll(string(list), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		identity, dependencies, ok := parseListLine(line, triplet, hostTriplet)
		if !ok {
			diagnostics = append(diagnostics, Diagnostic{Stage: "parse_list", Stream: "combined", ByteCount: int64(len(rawLine)), SafePrefix: redactDiagnostic(rawLine)})
			continue
		}
		base := identity.baseKey()
		if previous, exists := listNodes[base]; exists && !previous.Equal(identity) {
			if len(previous.Features) != 0 && len(identity.Features) != 0 {
				return Plan{}, &Failure{ID: FailureOutputMismatch, Reason: ReasonDependInfoOutputInconsistent, Stage: "parse_list", Detail: "conflicting duplicate feature row"}
			}
		}
		listNodes[base] = identity
		for _, dependency := range dependencies {
			listEdges[base+"\x01"+dependency.baseKey()] = [2]string{base, dependency.baseKey()}
			if previous, exists := listNodes[dependency.baseKey()]; !exists {
				listNodes[dependency.baseKey()] = dependency
			} else if len(previous.Features) != 0 && len(dependency.Features) != 0 && !previous.Equal(dependency) {
				return Plan{}, &Failure{ID: FailureOutputMismatch, Reason: ReasonDependInfoOutputInconsistent, Stage: "parse_list", Detail: "conflicting dependency feature row"}
			}
		}
	}
	if len(listNodes) == 0 {
		return Plan{}, &Failure{ID: FailureUnparseable, Reason: ReasonDependInfoOutputUnparseable, Stage: "parse_list", Detail: "no list grammar rows"}
	}

	dgmlNodes := map[string]Node{}
	for _, rawNode := range document.Nodes {
		node, ok := parsePackageIdentity(strings.TrimSpace(rawNode.ID), triplet, hostTriplet)
		if !ok {
			return Plan{}, &Failure{ID: FailureUnparseable, Reason: ReasonDependInfoOutputUnparseable, Stage: "parse_dgml", Detail: "invalid node identity"}
		}
		dgmlNodes[node.baseKey()] = node
	}
	dgmlEdges := map[string][2]string{}
	for _, rawLink := range document.Links {
		from, fromOK := parsePackageIdentity(strings.TrimSpace(rawLink.Source), triplet, hostTriplet)
		to, toOK := parsePackageIdentity(strings.TrimSpace(rawLink.Target), triplet, hostTriplet)
		if !fromOK || !toOK {
			return Plan{}, &Failure{ID: FailureUnparseable, Reason: ReasonDependInfoOutputUnparseable, Stage: "parse_dgml", Detail: "invalid edge identity"}
		}
		dgmlEdges[from.baseKey()+"\x01"+to.baseKey()] = [2]string{from.baseKey(), to.baseKey()}
	}
	if !sameKeySet(dgmlNodes, listNodes) || !sameEdgeSet(dgmlEdges, listEdges) {
		return Plan{}, &Failure{ID: FailureOutputMismatch, Reason: ReasonDependInfoOutputInconsistent, Stage: "cross_check", Detail: "DGML/list node or edge set differs"}
	}

	plan := Plan{Diagnostics: diagnostics}
	for _, node := range listNodes {
		plan.Nodes = append(plan.Nodes, node.normalized())
	}
	sortNodes(plan.Nodes)
	for _, endpoints := range listEdges {
		plan.Edges = append(plan.Edges, Edge{From: listNodes[endpoints[0]].normalized(), To: listNodes[endpoints[1]].normalized()})
	}
	sortEdges(plan.Edges)
	return plan, nil
}

func parseListLine(line, triplet, hostTriplet string) (Node, []Node, bool) {
	if !strings.HasPrefix(line, "(") {
		return Node{}, nil, false
	}
	closeDepth := strings.IndexByte(line, ')')
	if closeDepth < 2 {
		return Node{}, nil, false
	}
	if _, err := strconv.Atoi(line[1:closeDepth]); err != nil {
		return Node{}, nil, false
	}
	rest := strings.TrimSpace(line[closeDepth+1:])
	delimiter := strings.Index(rest, ": ")
	identityText, dependencyText := "", ""
	if delimiter >= 0 {
		identityText = strings.TrimSpace(rest[:delimiter])
		dependencyText = strings.TrimSpace(rest[delimiter+2:])
	} else if strings.HasSuffix(rest, ":") {
		identityText = strings.TrimSpace(strings.TrimSuffix(rest, ":"))
	} else {
		return Node{}, nil, false
	}
	identity, ok := parsePackageIdentity(identityText, triplet, hostTriplet)
	if !ok {
		return Node{}, nil, false
	}
	dependencies := []Node{}
	for _, token := range splitOutsideBrackets(dependencyText) {
		if token == "" {
			continue
		}
		dependency, valid := parsePackageIdentity(token, triplet, hostTriplet)
		if !valid {
			return Node{}, nil, false
		}
		dependencies = append(dependencies, dependency)
	}
	return identity, dependencies, true
}

func parsePackageIdentity(raw, triplet, hostTriplet string) (Node, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Node{}, false
	}
	features := []string{}
	if open := strings.IndexByte(raw, '['); open >= 0 {
		close := strings.IndexByte(raw[open:], ']')
		if close < 0 {
			return Node{}, false
		}
		close += open
		for _, feature := range strings.Split(raw[open+1:close], ",") {
			feature = strings.TrimSpace(feature)
			if feature != "" {
				features = append(features, feature)
			}
		}
		raw = raw[:open] + raw[close+1:]
	}
	name, suffix, decorated := strings.Cut(raw, ":")
	if strings.Contains(suffix, ":") {
		return Node{}, false
	}
	if _, err := portname.Parse(name); err != nil {
		return Node{}, false
	}
	node := Node{Name: name, Role: RoleTarget, Triplet: triplet, Features: features}
	if decorated {
		switch suffix {
		case "host":
			node.Role = RoleHost
			node.Triplet = hostTriplet
		case "", triplet:
			return Node{}, false
		default:
			if !validTriplet(suffix) {
				return Node{}, false
			}
			node.Role = RoleOther
			node.Triplet = suffix
		}
	}
	return node.normalized(), true
}

func splitOutsideBrackets(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := []string{}
	start, depth := 0, 0
	for index, char := range value {
		switch char {
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(value[start:index]))
				start = index + 1
			}
		}
	}
	parts = append(parts, strings.TrimSpace(value[start:]))
	return parts
}

func sameKeySet(left map[string]Node, right map[string]Node) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if _, ok := right[key]; !ok {
			return false
		}
	}
	return true
}

func sameEdgeSet(left, right map[string][2]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key := range left {
		if _, ok := right[key]; !ok {
			return false
		}
	}
	return true
}

func sortNodes(nodes []Node) {
	sort.Slice(nodes, func(i, j int) bool { return nodeLess(nodes[i], nodes[j]) })
}

func nodeLess(left, right Node) bool {
	if left.Name != right.Name {
		return left.Name < right.Name
	}
	rank := func(role Role) int {
		switch role {
		case RoleTarget:
			return 0
		case RoleHost:
			return 1
		default:
			return 2
		}
	}
	if rank(left.Role) != rank(right.Role) {
		return rank(left.Role) < rank(right.Role)
	}
	if left.Triplet != right.Triplet {
		return left.Triplet < right.Triplet
	}
	return strings.Join(left.Features, "\x00") < strings.Join(right.Features, "\x00")
}

func sortEdges(edges []Edge) {
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From.Key() != edges[j].From.Key() {
			return nodeLess(edges[i].From, edges[j].From)
		}
		return nodeLess(edges[i].To, edges[j].To)
	})
}

func redactDiagnostic(value string) string {
	value = strings.TrimSpace(value)
	value = diagnosticUserinfo.ReplaceAllString(value, `${1}REDACTED@`)
	value = diagnosticQueryValue.ReplaceAllString(value, `${1}=REDACTED`)
	for _, marker := range []string{"token=", "password=", "apikey=", "api_key=", "secret="} {
		if index := strings.Index(strings.ToLower(value), marker); index >= 0 {
			value = value[:index+len(marker)] + "REDACTED"
		}
	}
	if len(value) > 512 {
		value = value[:512]
	}
	return value
}

var (
	diagnosticUserinfo   = regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`)
	diagnosticQueryValue = regexp.MustCompile(`([?&][^=&\s]+)=([^&\s]+)`)
)

package archguard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type repositoryContractLane struct {
	WP       string `yaml:"wp"`
	Decision string `yaml:"decision"`
	Lane     string `yaml:"lane"`
}

type repositoryContractWP struct {
	StartAfter        []string                 `yaml:"start_after"`
	DecisionGates     []string                 `yaml:"decision_gates"`
	LaneDecisionGates []repositoryContractLane `yaml:"lane_decision_gates"`
	DeltaGates        []string                 `yaml:"delta_gates"`
	PRGates           []string                 `yaml:"pr_gates"`
}

type repositoryContractDecision struct {
	BeforeWP    []string                 `yaml:"before_wp"`
	BeforeLanes []repositoryContractLane `yaml:"before_lanes"`
}

type repositoryContractDelta struct {
	WP            []string `yaml:"wp"`
	DecisionGates []string `yaml:"decision_gates"`
	Revalidation  []string `yaml:"revalidation"`
}

type repositoryContractPR struct {
	WP      string   `yaml:"wp"`
	Deps    []string `yaml:"deps"`
	WPGates []string `yaml:"wp_gates"`
}

// This is a projection of the gate-related fields, not a second full schema.
type repositoryContractTraceability struct {
	SchemaVersion int `yaml:"schema_version"`
	Metadata      struct {
		AuditRegister struct {
			Path   string `yaml:"path"`
			SHA256 string `yaml:"sha256"`
		} `yaml:"audit_register"`
	} `yaml:"metadata"`
	WorkPackages    map[string]repositoryContractWP       `yaml:"work_packages"`
	Decisions       map[string]repositoryContractDecision `yaml:"decisions"`
	Deltas          map[string]repositoryContractDelta    `yaml:"deltas"`
	ArchitecturePRs map[string]repositoryContractPR       `yaml:"architecture_prs"`
}

func TestRepositoryArchitectureContracts(t *testing.T) {
	root := repositoryContractRoot(t)
	docs := filepath.Join(root, "docs", "modernization")
	t.Run("validators", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			mutate func(*repositoryContractTraceability)
		}{
			{"valid", nil},
			{"wp-cycle", func(v *repositoryContractTraceability) {
				v.WorkPackages["A"] = repositoryContractWP{StartAfter: []string{"B"}}
				v.WorkPackages["B"] = repositoryContractWP{StartAfter: []string{"A"}}
			}},
			{"mixed-cycle", func(v *repositoryContractTraceability) {
				v.WorkPackages["A"] = repositoryContractWP{PRGates: []string{"PB"}}
				v.ArchitecturePRs["PB"] = repositoryContractPR{WP: "B", WPGates: []string{"A"}}
			}},
			{"self-package-gate", func(v *repositoryContractTraceability) {
				v.ArchitecturePRs["PA"] = repositoryContractPR{WP: "A", WPGates: []string{"A"}}
			}},
			{"pr-cycle", func(v *repositoryContractTraceability) {
				v.ArchitecturePRs["PA"] = repositoryContractPR{WP: "A", Deps: []string{"PB"}}
				v.ArchitecturePRs["PB"] = repositoryContractPR{WP: "B", Deps: []string{"PA"}}
			}},
			{"unknown-pr", func(v *repositoryContractTraceability) {
				v.ArchitecturePRs["PA"] = repositoryContractPR{WP: "A", Deps: []string{"missing"}}
			}},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := repositoryContractTraceability{
					WorkPackages:    map[string]repositoryContractWP{"A": {}, "B": {}},
					ArchitecturePRs: map[string]repositoryContractPR{"PA": {WP: "A"}, "PB": {WP: "B"}},
				}
				if test.mutate != nil {
					test.mutate(&fixture)
				}
				graph, err := repositoryContractDependencyGraph(fixture)
				if err != nil {
					t.Fatal(err)
				}
				if err := repositoryContractAcyclic(graph); (err != nil) != (test.mutate != nil) {
					t.Fatalf("unexpected dependency validation result: %v", err)
				}
			})
		}
		for _, invalid := range []string{"", "1234567", strings.Repeat("g", 40), strings.Repeat("A", 40)} {
			if repositoryContractFullCommitID(invalid) {
				t.Errorf("invalid commit ID accepted: %q", invalid)
			}
		}
		if !repositoryContractFullCommitID(strings.Repeat("a", 40)) {
			t.Error("valid commit ID rejected")
		}
		for _, invalid := range []string{"", ".", "../audit.md", "/audit.md", "C:/audit.md", `a\b.md`} {
			if repositoryContractAuditRelativePath(invalid) {
				t.Errorf("invalid audit path accepted: %q", invalid)
			}
		}
		if !repositoryContractAuditRelativePath("audits/register.md") {
			t.Error("valid audit path rejected")
		}
	})

	// Missing baseline must not hide independent documentation/policy failures.
	t.Run("traceability", func(t *testing.T) {
		var trace repositoryContractTraceability
		repositoryContractReadYAML(t, filepath.Join(docs, "traceability.yaml"), &trace)
		if trace.SchemaVersion != 5 {
			t.Fatalf("unsupported traceability schema_version: %d", trace.SchemaVersion)
		}
		if len(trace.WorkPackages) == 0 || len(trace.Decisions) == 0 ||
			len(trace.Deltas) == 0 || len(trace.ArchitecturePRs) == 0 {
			t.Fatal("traceability registries must not be empty")
		}
		repositoryContractCheckGates(t, trace)
		repositoryContractCheckPRGraph(t, trace)
		repositoryContractCheckAuditHash(t, docs,
			trace.Metadata.AuditRegister.Path, trace.Metadata.AuditRegister.SHA256)
	})

	t.Run("policy", func(t *testing.T) {
		policy, err := LoadPolicy(filepath.Join(root, "architecture", "policy.yaml"))
		if err != nil {
			t.Fatalf("load architecture policy: %v", err)
		}
		if err := validatePolicyModule(root, policy.Module); err != nil {
			t.Fatal(err)
		}
		if len(policy.AllowedGlobalNamePatterns) != 0 {
			t.Fatalf("allowed_global_name_patterns must stay empty; baseline exact fingerprints instead, got %v",
				policy.AllowedGlobalNamePatterns)
		}
		for _, required := range []string{"cmd", "internal", "tools"} {
			if !repositoryContractContains(policy.SourceRoots, required) {
				t.Fatalf("policy must scan source root %s", required)
			}
		}
		if _, err := LoadOwners(filepath.Join(root, "architecture", "owners.yaml")); err != nil {
			t.Fatalf("load architecture owners: %v", err)
		}
		if _, err := LoadWorkers(filepath.Join(root, "architecture", "workers.yaml")); err != nil {
			t.Fatalf("load architecture workers: %v", err)
		}
	})

	t.Run("baseline", func(t *testing.T) {
		baseline, err := LoadBaseline(filepath.Join(root, "architecture", "baseline.yaml"))
		if err != nil {
			t.Fatalf("load architecture baseline: %v", err)
		}
		// Shape only: provenance is checked in the documented A2 generation gate.
		// Comparing today's source to generated_from here would break the ratchet.
		if !repositoryContractFullCommitID(baseline.GeneratedFrom) {
			t.Fatal("architecture baseline generated_from must be a full lowercase 40-digit commit ID")
		}
		// Load our own dependency: this subtest also runs correctly in isolation.
		var trace repositoryContractTraceability
		repositoryContractReadYAML(t, filepath.Join(docs, "traceability.yaml"), &trace)
		for _, entry := range baseline.Entries {
			owner := strings.TrimSpace(entry.Owner)
			if owner == "architecture-triage" || owner == "triage-owner" {
				t.Fatalf("baseline entry %s still uses temporary owner %q", entry.Fingerprint, owner)
			}
			if baselineEntryUnowned(entry) {
				t.Fatalf("baseline entry %s has incomplete ownership", entry.Fingerprint)
			}
			if _, ok := ParseViolationKind(string(entry.Kind)); !ok {
				t.Fatalf("baseline entry %s has unknown kind %q", entry.Fingerprint, entry.Kind)
			}
			repositoryContractRequireKeys(t, "baseline entry "+entry.Fingerprint,
				[]string{entry.WorkPackage}, trace.WorkPackages)
		}
		workers, err := LoadWorkers(filepath.Join(root, "architecture", "workers.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, worker := range workers.Entries {
			repositoryContractRequireKeys(t, "worker "+worker.Fingerprint,
				[]string{worker.WorkPackage}, trace.WorkPackages)
		}
	})
}

func repositoryContractFullCommitID(value string) bool {
	if len(value) != 40 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func repositoryContractCheckGates(t *testing.T, trace repositoryContractTraceability) {
	t.Helper()

	for _, wpID := range repositoryContractSortedKeys(trace.WorkPackages) {
		wp := trace.WorkPackages[wpID]
		repositoryContractRequireKeys(t, "work package "+wpID, wp.StartAfter, trace.WorkPackages)
		repositoryContractRequireKeys(t, "work package "+wpID, wp.PRGates, trace.ArchitecturePRs)

		for _, decisionID := range wp.DecisionGates {
			decision, ok := trace.Decisions[decisionID]
			if !ok || !repositoryContractContains(decision.BeforeWP, wpID) {
				t.Fatalf("asymmetric decision gate %s -> %s", wpID, decisionID)
			}
		}
		for _, gate := range wp.LaneDecisionGates {
			if strings.TrimSpace(gate.Lane) == "" {
				t.Fatalf("empty lane in work package %s", wpID)
			}
			decision, ok := trace.Decisions[gate.Decision]
			if !ok || !repositoryContractContainsLane(decision.BeforeLanes, wpID, gate.Lane) {
				t.Fatalf("asymmetric lane gate %s/%s -> %s", wpID, gate.Lane, gate.Decision)
			}
		}
		for _, deltaID := range wp.DeltaGates {
			delta, ok := trace.Deltas[deltaID]
			if !ok || !repositoryContractContains(delta.WP, wpID) {
				t.Fatalf("asymmetric delta gate %s -> %s", wpID, deltaID)
			}
		}
	}

	for _, decisionID := range repositoryContractSortedKeys(trace.Decisions) {
		decision := trace.Decisions[decisionID]
		for _, wpID := range decision.BeforeWP {
			wp, ok := trace.WorkPackages[wpID]
			if !ok || !repositoryContractContains(wp.DecisionGates, decisionID) {
				t.Fatalf("asymmetric decision gate %s -> %s", decisionID, wpID)
			}
		}
		for _, lane := range decision.BeforeLanes {
			if strings.TrimSpace(lane.Lane) == "" {
				t.Fatalf("empty lane in decision %s", decisionID)
			}
			wp, ok := trace.WorkPackages[lane.WP]
			if !ok || !repositoryContractContainsLaneGate(wp.LaneDecisionGates, decisionID, lane.Lane) {
				t.Fatalf("asymmetric lane gate %s -> %s/%s", decisionID, lane.WP, lane.Lane)
			}
		}
	}

	for _, deltaID := range repositoryContractSortedKeys(trace.Deltas) {
		delta := trace.Deltas[deltaID]
		repositoryContractRequireKeys(t, "delta "+deltaID, delta.DecisionGates, trace.Decisions)
		repositoryContractRequireKeys(t, "delta "+deltaID, delta.Revalidation, trace.ArchitecturePRs)
		for _, wpID := range delta.WP {
			wp, ok := trace.WorkPackages[wpID]
			if !ok || !repositoryContractContains(wp.DeltaGates, deltaID) {
				t.Fatalf("asymmetric delta gate %s -> %s", deltaID, wpID)
			}
		}
	}
}

func repositoryContractCheckPRGraph(t *testing.T, trace repositoryContractTraceability) {
	t.Helper()
	graph, err := repositoryContractDependencyGraph(trace)
	if err != nil {
		t.Fatal(err)
	}
	if err := repositoryContractAcyclic(graph); err != nil {
		t.Fatal(err)
	}
}

func repositoryContractDependencyGraph(trace repositoryContractTraceability) (map[string][]string, error) {
	// Keep start and completion distinct. A work package completes after its
	// registered PRs; a PR may require other completed packages. This exposes
	// mixed WP/PR cycles without copying GitHub's mutable completion status.
	graph := make(map[string][]string)
	for _, wpID := range repositoryContractSortedKeys(trace.WorkPackages) {
		wp := trace.WorkPackages[wpID]
		start, done := "wp-start/"+wpID, "wp-done/"+wpID
		graph[start] = nil
		graph[done] = []string{start}
		for _, dependency := range wp.StartAfter {
			graph[start] = append(graph[start], "wp-done/"+dependency)
		}
		for _, dependency := range wp.PRGates {
			graph[start] = append(graph[start], "pr-done/"+dependency)
		}
	}
	for _, prID := range repositoryContractSortedKeys(trace.ArchitecturePRs) {
		pr := trace.ArchitecturePRs[prID]
		if _, ok := trace.WorkPackages[pr.WP]; !ok {
			return nil, fmt.Errorf("PR %s references unknown work package %s", prID, pr.WP)
		}
		done := "pr-done/" + prID
		graph[done] = []string{"wp-start/" + pr.WP}
		graph["wp-done/"+pr.WP] = append(graph["wp-done/"+pr.WP], done)
		for _, dependency := range pr.Deps {
			graph[done] = append(graph[done], "pr-done/"+dependency)
		}
		for _, dependency := range pr.WPGates {
			graph[done] = append(graph[done], "wp-done/"+dependency)
		}
	}
	return graph, nil
}

func repositoryContractAcyclic(graph map[string][]string) error {
	state := make(map[string]uint8, len(graph))
	var stack []string
	var visit func(string) error
	visit = func(node string) error {
		if _, ok := graph[node]; !ok {
			return fmt.Errorf("architecture graph references unknown node %s", node)
		}
		switch state[node] {
		case 1:
			for i, prior := range stack {
				if prior == node {
					return fmt.Errorf("architecture dependency cycle: %s -> %s", strings.Join(stack[i:], " -> "), node)
				}
			}
		case 2:
			return nil
		}
		state[node] = 1
		stack = append(stack, node)
		dependencies := append([]string(nil), graph[node]...)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[node] = 2
		return nil
	}
	for _, node := range repositoryContractSortedKeys(graph) {
		if err := visit(node); err != nil {
			return err
		}
	}
	return nil
}

func repositoryContractSortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func repositoryContractAuditRelativePath(value string) bool {
	return value != "." && fs.ValidPath(value) && !strings.ContainsAny(value, `\:`)
}

func repositoryContractCheckAuditHash(t *testing.T, docs, relativePath, declared string) {
	t.Helper()
	if !repositoryContractAuditRelativePath(relativePath) {
		t.Fatalf("audit register path must be a file relative to docs/modernization: %q", relativePath)
	}

	data, err := os.ReadFile(filepath.Join(docs, filepath.FromSlash(relativePath)))
	if err != nil {
		t.Fatalf("read audit register: %v", err)
	}
	sum := sha256.Sum256(data)
	if actual := hex.EncodeToString(sum[:]); actual != declared {
		t.Fatalf("audit-register SHA-256 mismatch: declared=%s actual=%s", declared, actual)
	}
}

func repositoryContractRequireKeys[T any](t *testing.T, source string, ids []string, known map[string]T) {
	t.Helper()
	for _, id := range ids {
		if _, ok := known[id]; !ok {
			t.Fatalf("%s references unknown id %s", source, id)
		}
	}
}

func repositoryContractContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func repositoryContractContainsLane(values []repositoryContractLane, wp, lane string) bool {
	for _, value := range values {
		if value.WP == wp && value.Lane == lane {
			return true
		}
	}
	return false
}

func repositoryContractContainsLaneGate(values []repositoryContractLane, decision, lane string) bool {
	for _, value := range values {
		if value.Decision == decision && value.Lane == lane {
			return true
		}
	}
	return false
}

func repositoryContractRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root with go.mod was not found")
		}
		dir = parent
	}
}

func repositoryContractReadYAML(t *testing.T, path string, dst any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// Other schema-v5 fields are intentionally outside this gate projection.
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(dst); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("%s must contain exactly one YAML document: %v", path, err)
	}
}

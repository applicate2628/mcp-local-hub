package archguard

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
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

type repositoryContractTraceability struct {
	Metadata struct {
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
	for name, load := range map[string]func(string) error{
		"policy": func(path string) error {
			_, err := LoadPolicy(path)
			return err
		},
		"owners": func(path string) error {
			_, err := LoadOwners(path)
			return err
		},
		"workers": func(path string) error {
			_, err := LoadWorkers(path)
			return err
		},
	} {
		if err := load(filepath.Join(root, "architecture", name+".yaml")); err != nil {
			t.Fatalf("load architecture %s: %v", name, err)
		}
	}

	docs := filepath.Join(root, "docs", "modernization")
	var trace repositoryContractTraceability
	repositoryContractReadYAML(t, filepath.Join(docs, "traceability.yaml"), &trace)

	if len(trace.WorkPackages) == 0 || len(trace.Decisions) == 0 ||
		len(trace.Deltas) == 0 || len(trace.ArchitecturePRs) == 0 {
		t.Fatal("traceability registries must not be empty")
	}

	repositoryContractCheckGates(t, trace)
	repositoryContractCheckPRGraph(t, trace)
	repositoryContractCheckAuditHash(
		t,
		docs,
		trace.Metadata.AuditRegister.Path,
		trace.Metadata.AuditRegister.SHA256,
	)
}

func repositoryContractCheckGates(t *testing.T, trace repositoryContractTraceability) {
	t.Helper()

	for wpID, wp := range trace.WorkPackages {
		repositoryContractRequireKeys(t, "work package "+wpID, wp.StartAfter, trace.WorkPackages)
		repositoryContractRequireKeys(t, "work package "+wpID, wp.PRGates, trace.ArchitecturePRs)

		for _, decisionID := range wp.DecisionGates {
			decision, ok := trace.Decisions[decisionID]
			if !ok || !repositoryContractContains(decision.BeforeWP, wpID) {
				t.Fatalf("asymmetric decision gate %s -> %s", wpID, decisionID)
			}
		}
		for _, gate := range wp.LaneDecisionGates {
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

	for decisionID, decision := range trace.Decisions {
		for _, wpID := range decision.BeforeWP {
			wp, ok := trace.WorkPackages[wpID]
			if !ok || !repositoryContractContains(wp.DecisionGates, decisionID) {
				t.Fatalf("asymmetric decision gate %s -> %s", decisionID, wpID)
			}
		}
		for _, lane := range decision.BeforeLanes {
			wp, ok := trace.WorkPackages[lane.WP]
			if !ok || !repositoryContractContainsLaneGate(wp.LaneDecisionGates, decisionID, lane.Lane) {
				t.Fatalf("asymmetric lane gate %s -> %s/%s", decisionID, lane.WP, lane.Lane)
			}
		}
	}

	for deltaID, delta := range trace.Deltas {
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

	for prID, pr := range trace.ArchitecturePRs {
		if _, ok := trace.WorkPackages[pr.WP]; !ok {
			t.Fatalf("PR %s references unknown work package %s", prID, pr.WP)
		}
		repositoryContractRequireKeys(t, "PR "+prID, pr.Deps, trace.ArchitecturePRs)
		repositoryContractRequireKeys(t, "PR "+prID, pr.WPGates, trace.WorkPackages)
	}

	state := make(map[string]uint8, len(trace.ArchitecturePRs))
	var visit func(string)
	visit = func(prID string) {
		switch state[prID] {
		case 1:
			t.Fatalf("architecture PR graph contains a cycle at %s", prID)
		case 2:
			return
		}
		state[prID] = 1
		for _, dep := range trace.ArchitecturePRs[prID].Deps {
			visit(dep)
		}
		state[prID] = 2
	}
	for prID := range trace.ArchitecturePRs {
		visit(prID)
	}
}

func repositoryContractCheckAuditHash(t *testing.T, docs, relativePath, declared string) {
	t.Helper()

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
	if err := yaml.Unmarshal(data, dst); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}

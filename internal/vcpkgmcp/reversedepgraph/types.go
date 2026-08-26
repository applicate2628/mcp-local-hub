// Package reversedepgraph computes a bounded selected-port reverse dependency
// graph exclusively from resolved vcpkg depend-info plans.
package reversedepgraph

import (
	"context"
	"sort"
	"strings"
	"time"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

const (
	MaxOverlayRoots       = 64
	MaxLocalRegistries    = 16
	MaxEntriesPerRoot     = 8192
	MaxDistinctPorts      = 4096
	MaxMetadataBytes      = 1 << 20
	MaxEnumeratorBytes    = 64 << 20
	MaxCandidateBatchSize = 64
	MaxBatchInvocations   = 2 * ((MaxDistinctPorts + MaxCandidateBatchSize - 1) / MaxCandidateBatchSize)
	MaxStreamBytes        = 4 << 20
	MaxCapturedBytes      = 64 << 20
	MaxNodes              = 65536
	MaxEdges              = 262144
	DefaultTimeoutMS      = 60000
	MinTimeoutMS          = 1000
	MaxTimeoutMS          = 300000
)

type Reason string

const (
	ReasonVcpkgCommandUnavailable      Reason = "vcpkg_command_unavailable"
	ReasonVcpkgVersionUnsupported      Reason = "vcpkg_version_unsupported"
	ReasonRequestCancelled             Reason = "request_cancelled"
	ReasonDependInfoTimeout            Reason = "depend_info_timeout"
	ReasonDependInfoNonzero            Reason = "depend_info_nonzero"
	ReasonDependInfoOutputUnparseable  Reason = "depend_info_output_unparseable"
	ReasonDependInfoOutputInconsistent Reason = "depend_info_output_inconsistent"
	ReasonIncompletePortUniverse       Reason = "incomplete_port_universe"
	ReasonNetworkDisabledRegistry      Reason = "network_disabled_registry"
	ReasonOverlayProvenanceUnknown     Reason = "overlay_provenance_unknown"
	ReasonInputChangedDuringResolution Reason = "input_changed_during_resolution"
	ReasonResourceLimitExceeded        Reason = "resource_limit_exceeded"
	ReasonResourceBusy                 Reason = "resource_busy"
	ReasonScratchIOFailed              Reason = "scratch_io_failed"
	ReasonScratchCleanupFailed         Reason = "scratch_cleanup_failed"
)

type FailureID string

const (
	FailureExecUnavailable        FailureID = "VRDG_EXEC_UNAVAILABLE"
	FailureVersionUnsupported     FailureID = "VRDG_VERSION_UNSUPPORTED"
	FailureCancelled              FailureID = "VRDG_CANCELLED"
	FailureTimeout                FailureID = "VRDG_TIMEOUT"
	FailureNonzero                FailureID = "VRDG_NONZERO"
	FailureUnparseable            FailureID = "VRDG_UNPARSEABLE"
	FailureOutputMismatch         FailureID = "VRDG_OUTPUT_MISMATCH"
	FailureUniverseIncomplete     FailureID = "VRDG_UNIVERSE_INCOMPLETE"
	FailureNetworkRegistryRefused FailureID = "VRDG_NETWORK_REGISTRY_REFUSED"
	FailureOverlayUnknown         FailureID = "VRDG_OVERLAY_UNKNOWN"
	FailureInputDrift             FailureID = "VRDG_INPUT_DRIFT"
	FailureResourceLimit          FailureID = "VRDG_RESOURCE_LIMIT"
	FailureResourceBusy           FailureID = "VRDG_RESOURCE_BUSY"
	FailureScratchIO              FailureID = "VRDG_SCRATCH_IO"
	FailureScratchCleanup         FailureID = "VRDG_SCRATCH_CLEANUP"
)

type Failure struct {
	ID        FailureID `json:"id"`
	Reason    Reason    `json:"reason"`
	Stage     string    `json:"stage,omitempty"`
	Candidate string    `json:"candidate,omitempty"`
	Format    string    `json:"format,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	ExitCode  *int      `json:"exit_code,omitempty"`
	cause     error
}

func (failure *Failure) Error() string {
	if failure == nil {
		return ""
	}
	parts := []string{string(failure.ID)}
	if failure.Stage != "" {
		parts = append(parts, failure.Stage)
	}
	if failure.Detail != "" {
		parts = append(parts, failure.Detail)
	}
	return strings.Join(parts, ": ")
}

func (failure *Failure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

type Args struct {
	Port            string   `json:"port"`
	VcpkgRoot       string   `json:"vcpkg_root"`
	Triplet         string   `json:"triplet"`
	HostTriplet     string   `json:"host_triplet"`
	OverlayPorts    []string `json:"overlay_ports,omitempty"`
	OverlayTriplets []string `json:"overlay_triplets,omitempty"`
	ManifestRoot    string   `json:"manifest_root,omitempty"`
	ScratchRoot     string   `json:"scratch_root"`
	TimeoutMS       int      `json:"timeout_ms,omitempty"`
}

func (args Args) Timeout() time.Duration {
	if args.TimeoutMS == 0 {
		return DefaultTimeoutMS * time.Millisecond
	}
	return time.Duration(args.TimeoutMS) * time.Millisecond
}

type Role string

const (
	RoleTarget Role = "target"
	RoleHost   Role = "host"
	RoleOther  Role = "other"
)

type Node struct {
	Name     string   `json:"name"`
	Role     Role     `json:"role"`
	Triplet  string   `json:"triplet"`
	Features []string `json:"features,omitempty"`
}

func (node Node) normalized() Node {
	copyNode := node
	copyNode.Features = append([]string(nil), node.Features...)
	sort.Strings(copyNode.Features)
	copyNode.Features = compactStrings(copyNode.Features)
	return copyNode
}

func (node Node) Key() string {
	node = node.normalized()
	return node.Name + "\x00" + string(node.Role) + "\x00" + node.Triplet + "\x00" + strings.Join(node.Features, "\x00")
}

func (node Node) baseKey() string {
	return node.Name + "\x00" + string(node.Role) + "\x00" + node.Triplet
}

func (node Node) Equal(other Node) bool { return node.Key() == other.Key() }

type Edge struct {
	From Node `json:"from"`
	To   Node `json:"to"`
}

func (edge Edge) key() string { return edge.From.Key() + "\x01" + edge.To.Key() }

type Plan struct {
	Nodes       []Node       `json:"nodes"`
	Edges       []Edge       `json:"edges"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

type Dependent struct {
	Node     Node   `json:"node"`
	Distance int    `json:"distance"`
	Path     []Node `json:"path,omitempty"`
}

type Cycle struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

type ReducedGraph struct {
	Targets    []Node      `json:"targets"`
	Direct     []Dependent `json:"direct"`
	Transitive []Dependent `json:"transitive"`
	Edges      []Edge      `json:"edges"`
	Cycles     []Cycle     `json:"cycles,omitempty"`
}

type Candidate struct {
	Name                 string   `json:"name"`
	WinnerDirectory      string   `json:"winner_directory,omitempty"`
	WinnerSource         string   `json:"winner_source,omitempty"`
	Shadows              []string `json:"shadows,omitempty"`
	DeclaredDependencies []string `json:"declared_dependencies,omitempty"`
	Inspectable          bool     `json:"inspectable"`
	DefinitionHash       string   `json:"definition_sha256,omitempty"`
}

type UniverseOutcome struct {
	Complete   bool        `json:"complete"`
	Reason     Reason      `json:"reason,omitempty"`
	Failure    *Failure    `json:"failure,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
	Digest     string      `json:"digest,omitempty"`
	BytesRead  int64       `json:"bytes_read"`
	Entries    int         `json:"entries"`
}

type Query struct {
	Port            string   `json:"port"`
	VcpkgRoot       string   `json:"vcpkg_root"`
	Triplet         string   `json:"triplet"`
	HostTriplet     string   `json:"host_triplet"`
	OverlayPorts    []string `json:"overlay_ports,omitempty"`
	OverlayTriplets []string `json:"overlay_triplets,omitempty"`
	ManifestRoot    string   `json:"manifest_root,omitempty"`
	ScratchRoot     string   `json:"scratch_root"`
	ManifestMode    bool     `json:"manifest_mode"`
}

type ExecutableIdentity struct {
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

type Semantics struct {
	FeaturePolicy   string   `json:"feature_policy"`
	NetworkPolicy   string   `json:"network_policy"`
	BuildPolicy     string   `json:"build_policy"`
	Producer        string   `json:"producer"`
	EnvironmentKeys []string `json:"environment_keys,omitempty"`
}

type Coverage struct {
	Complete           bool   `json:"complete"`
	UniverseComplete   bool   `json:"universe_complete"`
	PlansComplete      bool   `json:"plans_complete"`
	FormatsAgree       bool   `json:"formats_agree"`
	ProvenanceComplete bool   `json:"provenance_complete"`
	InputUnchanged     bool   `json:"input_unchanged"`
	CandidateCount     int    `json:"candidate_count"`
	PotentialCount     int    `json:"potential_candidate_count"`
	SettledPlanCount   int    `json:"settled_plan_count"`
	UniverseDigest     string `json:"universe_digest,omitempty"`
}

type Resources struct {
	ChildInvocations    int   `json:"child_invocations"`
	ReapedProcesses     int   `json:"reaped_processes"`
	CapturedOutputBytes int64 `json:"captured_output_bytes"`
	EnumeratorBytes     int64 `json:"enumerator_bytes"`
	EnumeratorEntries   int   `json:"enumerator_entries"`
	NodeHighWater       int   `json:"node_high_water"`
	EdgeHighWater       int   `json:"edge_high_water"`
}

type Diagnostic struct {
	Stage      string `json:"stage"`
	Candidate  string `json:"candidate,omitempty"`
	Format     string `json:"format,omitempty"`
	Stream     string `json:"stream,omitempty"`
	ByteCount  int64  `json:"byte_count,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	SafePrefix string `json:"safe_prefix,omitempty"`
}

type Provenance struct {
	Port          string   `json:"port"`
	OverlayStatus string   `json:"overlay_status"`
	BaseSource    string   `json:"base_source,omitempty"`
	Winner        string   `json:"winner,omitempty"`
	WinnerSource  string   `json:"winner_source,omitempty"`
	Shadows       []string `json:"shadows,omitempty"`
}

type Result struct {
	Status      evidence.Status          `json:"status"`
	Reason      Reason                   `json:"reason,omitempty"`
	Failure     *Failure                 `json:"failure,omitempty"`
	Query       Query                    `json:"query"`
	Executable  ExecutableIdentity       `json:"executable"`
	Semantics   Semantics                `json:"semantics"`
	Coverage    Coverage                 `json:"coverage"`
	Resources   Resources                `json:"resources"`
	Targets     []Node                   `json:"target_instances,omitempty"`
	Direct      []Dependent              `json:"direct_dependents,omitempty"`
	Transitive  []Dependent              `json:"transitive_dependents,omitempty"`
	Edges       []Edge                   `json:"edges,omitempty"`
	Cycles      []Cycle                  `json:"cycles,omitempty"`
	Provenance  []Provenance             `json:"provenance,omitempty"`
	Diagnostics []Diagnostic             `json:"diagnostics,omitempty"`
	Evidence    evidence.Evidence        `json:"evidence"`
	Projection  *publicresult.Projection `json:"result_projection,omitempty"`
}

func NewResult(args Args) Result {
	return Result{
		Query: Query{
			Port: args.Port, VcpkgRoot: args.VcpkgRoot, Triplet: args.Triplet, HostTriplet: args.HostTriplet,
			OverlayPorts:    append([]string(nil), args.OverlayPorts...),
			OverlayTriplets: append([]string(nil), args.OverlayTriplets...),
			ManifestRoot:    args.ManifestRoot, ScratchRoot: args.ScratchRoot,
			ManifestMode: args.ManifestRoot != "",
		},
		Semantics: Semantics{
			FeaturePolicy: "candidate-defaults",
			NetworkPolicy: "configuration_disabled_not_kernel_isolated",
			BuildPolicy:   "no_install_no_build",
			Producer:      "vcpkg_depend_info_dgml_list_cross_check",
		},
	}
}

func UnknownResult(args Args, reason Reason, failure *Failure) Result {
	result := NewResult(args)
	result.Status = evidence.StatusUnknown
	result.Reason = reason
	result.Failure = failure
	return result
}

type Command struct {
	Executable string
	Args       []string
	Dir        string
	Env        []string
	Stage      string
	Candidate  string
	Candidates []string
	BatchIndex int
	Format     string
}

type CapturedStream struct {
	Data      []byte
	Bytes     int64
	SHA256    string
	Truncated bool
}

type RunOutput struct {
	Stdout   CapturedStream
	Stderr   CapturedStream
	ExitCode int
	Started  bool
	Reaped   bool
	Err      error
}

type Runner interface {
	Run(context.Context, Command) RunOutput
}

type RunnerFunc func(context.Context, Command) RunOutput

func (fn RunnerFunc) Run(ctx context.Context, command Command) RunOutput { return fn(ctx, command) }

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	write := 1
	for read := 1; read < len(values); read++ {
		if values[read] == values[write-1] {
			continue
		}
		values[write] = values[read]
		write++
	}
	return values[:write]
}

func candidateNames(candidates []Candidate) []string {
	names := make([]string, len(candidates))
	for index, candidate := range candidates {
		names[index] = candidate.Name
	}
	return names
}

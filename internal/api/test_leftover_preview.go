package api

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"
	"unicode"

	processpkg "mcp-local-hub/internal/process"
)

const (
	DefaultTestLeftoverMinAgeSec = int64(60)
	testLeftoverApplyFloorSec    = int64(600)

	TestLeftoverSnapshotComplete        = "snapshot-complete"
	TestLeftoverSnapshotDegraded        = "snapshot-degraded"
	TestLeftoverApplyDeferred           = "apply-deferred-v1"
	TestLeftoverNotEvaluated            = "not-evaluated-v1"
	TestLeftoverEnvironmentNotCollected = "not-collected-v1"

	testLeftoverManualOnlyNote = "manual-reap-only: verify identity out-of-band before killing"
)

type testLeftoverSnapshotFunc func() (string, error)
type testLeftoverBuildInfoReadFunc func(string) (*debug.BuildInfo, error)
type testLeftoverParentStateFunc func(int) (processpkg.PIDState, error)

// TestLeftoverPreviewOpts is the complete V1 diagnostic scope. Neither field
// authorizes an action: MinAgeSec changes age labels only, and TempRoot adds a
// strict-canonical display classification for reliability-family images.
type TestLeftoverPreviewOpts struct {
	MinAgeSec int64
	TempRoot  string
}

// TestLeftoverPreview is the structured result rendered by the CLI. A false
// Exhaustive value always accompanies snapshot-degraded.
type TestLeftoverPreview struct {
	SnapshotVerdict        string                  `json:"snapshot_verdict"`
	SnapshotDiagnostic     string                  `json:"snapshot_diagnostic,omitempty"`
	Exhaustive             bool                    `json:"exhaustive"`
	RequestedMinAgeSec     int64                   `json:"requested_min_age_sec"`
	TempRootVerdict        string                  `json:"temp_root_verdict"`
	ProtectedScopeVerdicts map[string]string       `json:"protected_scope_verdicts"`
	Candidates             []TestLeftoverCandidate `json:"candidates"`
}

// TestLeftoverCandidate contains independent read-only evidence for one census
// row. ExecutablePath and CommandLine are local-renderer inputs only and are
// intentionally excluded from JSON; the structured form exposes a basename and
// normalized argv shape instead.
type TestLeftoverCandidate struct {
	PID                  int      `json:"pid"`
	ParentPID            int      `json:"parent_pid"`
	StartedAt            string   `json:"started_at,omitempty"`
	IdentityVerdict      string   `json:"identity_verdict"`
	ExecutablePath       string   `json:"-"`
	ExecutableDisplay    string   `json:"executable_path"`
	ExecutablePathPolicy string   `json:"executable_path_policy"`
	CommandLine          string   `json:"-"`
	ArgvShape            string   `json:"argv_shape"`
	ImageFamily          string   `json:"image_family"`
	PatternClass         string   `json:"pattern_class"`
	PathVerdict          string   `json:"path_verdict"`
	PathRelations        []string `json:"path_relations"`
	AgeSec               int64    `json:"age_sec"`
	AgeVsRequestedMin    string   `json:"age_vs_requested_min"`
	AgeVsApplyFloor      string   `json:"age_vs_deferred_apply_floor"`
	ParentLiveness       string   `json:"parent_liveness"`
	BuildInfoTag         string   `json:"buildinfo_tag"`
	EnvironmentOverride  string   `json:"environment_override"`
	ApplyLifecycle       string   `json:"apply_lifecycle"`
	WouldRefuse          string   `json:"would_refuse"`
	OperatorNote         string   `json:"operator_note,omitempty"`
}

type testLeftoverPathScope struct {
	osTemp                   string
	operatorTemp             string
	operatorRootErr          bool
	productionState          string
	installPath              string
	repoRoot                 string
	protectedScopeVerdicts   map[string]string
	protectedScopeUnverified bool
}

// PreviewTestLeftovers enumerates and classifies test-leftover evidence. It is
// deliberately data-only: the method owns no action option, process handle, or
// mutation dependency, and returns after evidence collection.
func (a *API) PreviewTestLeftovers(opts TestLeftoverPreviewOpts) (TestLeftoverPreview, error) {
	if opts.MinAgeSec < 0 {
		return TestLeftoverPreview{}, fmt.Errorf("test-leftover preview: min-age-sec must be non-negative")
	}

	snapshotFn := testLeftoverSnapshotFunc(runProcessSnapshot)
	nowFn := time.Now
	buildInfoReadFn := testLeftoverBuildInfoReadFunc(buildinfo.ReadFile)
	parentStateFn := testLeftoverParentStateFunc(func(pid int) (processpkg.PIDState, error) {
		return orphanParentStateFn(pid)
	})
	if a != nil {
		if a.testLeftoverSnapshotFn != nil {
			snapshotFn = a.testLeftoverSnapshotFn
		}
		if a.testLeftoverNowFn != nil {
			nowFn = a.testLeftoverNowFn
		}
		if a.testLeftoverBuildInfoReadFn != nil {
			buildInfoReadFn = a.testLeftoverBuildInfoReadFn
		}
		if a.testLeftoverParentStateFn != nil {
			parentStateFn = a.testLeftoverParentStateFn
		}
	}

	raw, err := snapshotFn()
	if err != nil {
		return TestLeftoverPreview{}, fmt.Errorf("test-leftover preview: acquire process snapshot: %w", err)
	}
	rows, byPID, snapshotErr := parseProcessRows(strings.NewReader(raw))
	if len(rows) == 0 {
		return TestLeftoverPreview{}, fmt.Errorf("test-leftover preview: process snapshot contained no usable process rows")
	}
	scope := buildTestLeftoverPathScope(opts.TempRoot)
	result := TestLeftoverPreview{
		SnapshotVerdict:        TestLeftoverSnapshotComplete,
		Exhaustive:             true,
		RequestedMinAgeSec:     opts.MinAgeSec,
		TempRootVerdict:        "not-supplied",
		ProtectedScopeVerdicts: scope.protectedScopeVerdicts,
		Candidates:             make([]TestLeftoverCandidate, 0),
	}
	if strings.TrimSpace(opts.TempRoot) != "" {
		if scope.operatorRootErr {
			result.TempRootVerdict = "path-canonicalization-error"
		} else {
			result.TempRootVerdict = "path-canonical"
		}
	}
	if snapshotErr != nil {
		result.SnapshotVerdict = TestLeftoverSnapshotDegraded
		result.SnapshotDiagnostic = "process snapshot ended early; candidates are not exhaustive"
		result.Exhaustive = false
	}

	now := nowFn().UTC()
	for _, row := range rows {
		candidate, include := collectTestLeftoverCandidate(row, byPID, scope, opts, now, buildInfoReadFn, parentStateFn)
		if include {
			result.Candidates = append(result.Candidates, candidate)
		}
	}
	if snapshotErr != nil {
		for index := range result.Candidates {
			result.Candidates[index].WouldRefuse = TestLeftoverSnapshotDegraded
		}
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		return result.Candidates[i].PID < result.Candidates[j].PID
	})
	return result, nil
}

func buildTestLeftoverPathScope(operatorRoot string) testLeftoverPathScope {
	scope := testLeftoverPathScope{
		protectedScopeVerdicts: map[string]string{
			"production-state": "protected-scope-unverified",
			"install-path":     "protected-scope-unverified",
			"repo-path":        "protected-scope-unverified",
		},
	}
	scope.osTemp, _ = processpkg.CanonicalizePathStrict(os.TempDir())
	if strings.TrimSpace(operatorRoot) != "" {
		var err error
		scope.operatorTemp, err = processpkg.CanonicalizePathStrict(operatorRoot)
		scope.operatorRootErr = err != nil
	}
	if productionState, err := daemonStateDirReadOnly(); err == nil {
		scope.productionState, scope.protectedScopeVerdicts["production-state"] = canonicalizeTestLeftoverProtectedScope(productionState)
	}
	if installPath, err := canonicalMcphubPath(); err == nil {
		scope.installPath, scope.protectedScopeVerdicts["install-path"] = canonicalizeTestLeftoverProtectedScope(installPath)
	}
	if repoRoot, err := findTestLeftoverRepoRoot(); err == nil {
		scope.repoRoot, scope.protectedScopeVerdicts["repo-path"] = canonicalizeTestLeftoverProtectedScope(repoRoot)
	}
	for _, verdict := range scope.protectedScopeVerdicts {
		if verdict == "protected-scope-unverified" {
			scope.protectedScopeUnverified = true
			break
		}
	}
	return scope
}

func canonicalizeTestLeftoverProtectedScope(path string) (string, string) {
	canonical, err := processpkg.CanonicalizePathStrict(path)
	if err != nil {
		return "", "protected-scope-unverified"
	}
	return canonical, "path-canonical"
}

func findTestLeftoverRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("test-leftover preview: resolve repo working directory: %w", err)
	}
	for {
		if info, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil && !info.IsDir() {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("test-leftover preview: no repository root above %q", dir)
		}
		dir = parent
	}
}

func collectTestLeftoverCandidate(
	row procRow,
	byPID map[int]procRow,
	scope testLeftoverPathScope,
	opts TestLeftoverPreviewOpts,
	now time.Time,
	buildInfoReadFn testLeftoverBuildInfoReadFunc,
	parentStateFn testLeftoverParentStateFunc,
) (TestLeftoverCandidate, bool) {
	base := strings.ToLower(testLeftoverPathBasename(row.exePath))
	reliabilityImage := isTestLeftoverReliabilityImage(base)
	guiImageLexical := isTestLeftoverGUIE2EPath(row.exePath, base)
	goBuildLexical := isTestLeftoverGoBuildPath(row.exePath, base)
	if !reliabilityImage && !guiImageLexical && !goBuildLexical {
		return TestLeftoverCandidate{}, false
	}

	candidate := TestLeftoverCandidate{
		PID:                  row.pid,
		ParentPID:            row.ppid,
		StartedAt:            orphanStartedAt(row.created),
		IdentityVerdict:      "identity-available",
		ExecutablePath:       row.exePath,
		ExecutableDisplay:    testLeftoverPathBasename(row.exePath),
		ExecutablePathPolicy: "basename-only",
		CommandLine:          row.cmdline,
		PathRelations:        make([]string, 0),
		EnvironmentOverride:  TestLeftoverEnvironmentNotCollected,
		ApplyLifecycle:       TestLeftoverApplyDeferred,
		WouldRefuse:          TestLeftoverNotEvaluated,
	}
	if candidate.PID <= 0 || candidate.StartedAt == "" || strings.TrimSpace(candidate.ExecutablePath) == "" {
		candidate.IdentityVerdict = "identity-unavailable"
	}

	canonicalPath, pathErr := processpkg.CanonicalizePathStrict(row.exePath)
	if pathErr != nil {
		candidate.PathVerdict = "path-canonicalization-error"
	} else {
		candidate.PathVerdict = "path-canonical"
		candidate.PathRelations = testLeftoverPathRelations(canonicalPath, scope)
	}

	guiPath := guiImageLexical
	goBuildPath := goBuildLexical
	operatorPath := false
	osTempPath := false
	if pathErr == nil {
		guiPath = isTestLeftoverGUIE2EPath(canonicalPath, base)
		goBuildPath = isTestLeftoverGoBuildPath(canonicalPath, base)
		operatorPath = reliabilityImage && rootContains(scope.operatorTemp, canonicalPath)
		osTempPath = reliabilityImage && rootContains(scope.osTemp, canonicalPath)
	}

	matchCount := 0
	for _, matched := range []bool{guiPath, goBuildPath, operatorPath, osTempPath && !operatorPath} {
		if matched {
			matchCount++
		}
	}
	switch {
	case matchCount > 1:
		candidate.PatternClass = "ambiguous-multi-match"
		candidate.ImageFamily = "ambiguous-multi-match"
	case guiPath:
		candidate.PatternClass = "gui-e2e"
		candidate.ImageFamily = "gui-e2e"
	case goBuildPath:
		candidate.PatternClass = "go-build-cache"
		candidate.ImageFamily = "go-build-cache"
	case operatorPath:
		candidate.PatternClass = "operator-temp-root"
		candidate.ImageFamily = "reliability-temp"
	case osTempPath:
		candidate.PatternClass = "reliability-temp"
		candidate.ImageFamily = "reliability-temp"
	default:
		candidate.PatternClass = "unclassified"
		if reliabilityImage {
			candidate.ImageFamily = "reliability-temp"
		}
	}

	candidate.ParentLiveness = testLeftoverParentLiveness(row, byPID, parentStateFn)
	candidate.ArgvShape = classifyTestLeftoverArgv(row.cmdline, candidate.ImageFamily)
	if candidate.ArgvShape == "supervise" {
		if candidate.ParentLiveness == "parent-alive" {
			candidate.PatternClass = "live-supervise"
		} else {
			candidate.PatternClass = "standalone-supervise"
			candidate.OperatorNote = testLeftoverManualOnlyNote
		}
	}
	candidate.AgeSec, candidate.AgeVsRequestedMin, candidate.AgeVsApplyFloor = testLeftoverAgeEvidence(now, row.created, opts.MinAgeSec)
	candidate.BuildInfoTag = testLeftoverBuildInfoFinding(row.exePath, buildInfoReadFn)
	candidate.WouldRefuse = testLeftoverWouldRefuse(candidate, scope.operatorRootErr, scope.protectedScopeUnverified)
	return candidate, true
}

func testLeftoverPathRelations(canonicalPath string, scope testLeftoverPathScope) []string {
	relations := make([]string, 0, 6)
	for _, relation := range []struct {
		name string
		root string
	}{
		{name: "operator-temp-root", root: scope.operatorTemp},
		{name: "os-temp-root", root: scope.osTemp},
		{name: "production-state", root: scope.productionState},
		{name: "repo-path", root: scope.repoRoot},
	} {
		if rootContains(relation.root, canonicalPath) {
			relations = append(relations, relation.name)
		}
	}
	if scope.installPath != "" && rootContains(scope.installPath, canonicalPath) && rootContains(canonicalPath, scope.installPath) {
		relations = append(relations, "install-path")
	}
	return relations
}

func testLeftoverParentLiveness(row procRow, byPID map[int]procRow, query testLeftoverParentStateFunc) string {
	if row.ppid <= 0 || query == nil {
		return "parent-unproven"
	}
	if _, present := byPID[row.ppid]; present {
		return "parent-alive"
	}
	state, err := query(row.ppid)
	if err != nil {
		return "parent-unproven"
	}
	switch state {
	case processpkg.PIDStateDead:
		return "parent-proven-dead"
	case processpkg.PIDStateAlive:
		return "parent-alive"
	default:
		return "parent-unproven"
	}
}

func testLeftoverBuildInfoFinding(path string, readFn testLeftoverBuildInfoReadFunc) string {
	if strings.TrimSpace(path) == "" || readFn == nil {
		return "not-collected"
	}
	info, err := readFn(path)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return "unreadable"
		}
		return "unparsable"
	}
	if info == nil {
		return "unparsable"
	}
	for _, setting := range info.Settings {
		if setting.Key != "-tags" {
			continue
		}
		for _, tag := range strings.FieldsFunc(setting.Value, func(r rune) bool {
			return r == ',' || unicode.IsSpace(r)
		}) {
			if tag == "test_state_path_env" {
				return "test-tag-present"
			}
		}
	}
	return "test-tag-absent"
}

func testLeftoverAgeEvidence(now, created time.Time, requestedMin int64) (int64, string, string) {
	if created.IsZero() {
		return -1, "age-unavailable", "age-unavailable"
	}
	age := int64(now.Sub(created.UTC()) / time.Second)
	if age < 0 {
		age = 0
	}
	requestedVerdict := "at-or-above-requested-min-age"
	if age < requestedMin {
		requestedVerdict = "younger-than-requested-min-age"
	}
	applyVerdict := "at-or-above-apply-floor"
	if age < testLeftoverApplyFloorSec {
		applyVerdict = "younger-than-apply-floor"
	}
	return age, requestedVerdict, applyVerdict
}

func testLeftoverWouldRefuse(candidate TestLeftoverCandidate, operatorRootErr, protectedScopeUnverified bool) string {
	if candidate.PatternClass == "standalone-supervise" {
		return "supervise-not-tree-reachable"
	}
	if candidate.IdentityVerdict == "identity-unavailable" {
		return "identity-unavailable"
	}
	if candidate.PathVerdict == "path-canonicalization-error" || operatorRootErr {
		return "path-canonicalization-error"
	}
	for _, relation := range candidate.PathRelations {
		switch relation {
		case "production-state", "install-path":
			return relation
		case "repo-path":
			if candidate.PatternClass != "gui-e2e" {
				return relation
			}
		}
	}
	if candidate.ArgvShape == "unrecognized" {
		return "argv-not-in-branch"
	}
	if candidate.PatternClass == "unclassified" && candidate.ImageFamily == "reliability-temp" {
		return "requires-explicit-temp-root"
	}
	switch candidate.BuildInfoTag {
	case "test-tag-absent":
		return "not-test-tagged"
	case "unreadable", "unparsable", "not-collected":
		return "guard-evaluation-error"
	}
	if candidate.ParentLiveness != "parent-proven-dead" {
		return "parent-alive-or-unproven"
	}
	if candidate.AgeVsApplyFloor == "younger-than-apply-floor" {
		return "min-age-below-apply-floor"
	}
	if protectedScopeUnverified {
		return "protected-scope-unverified"
	}
	return TestLeftoverNotEvaluated
}

func testLeftoverPathBasename(path string) string {
	path = strings.TrimSpace(path)
	if index := strings.LastIndexAny(path, `/\\`); index >= 0 {
		path = path[index+1:]
	}
	if path == "" {
		return "<unknown>"
	}
	return path
}

func isTestLeftoverGUIE2EPath(path, base string) bool {
	if base != "mcphub" && base != "mcphub.exe" {
		return false
	}
	parts := testLeftoverPathParts(path)
	return containsTestLeftoverPartSequence(parts, []string{"internal", "gui", "e2e", "bin"})
}

func isTestLeftoverReliabilityImage(base string) bool {
	const prefix = "mcphub-reliability-"
	if !strings.HasPrefix(base, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(base, prefix)
	suffix = strings.TrimSuffix(suffix, ".exe")
	return suffix != "" && !strings.ContainsAny(suffix, `.\\/`)
}

func isTestLeftoverGoBuildPath(path, base string) bool {
	if base != "mcphub" && base != "mcphub.exe" {
		return false
	}
	parts := testLeftoverPathParts(path)
	for index, part := range parts {
		if !isTestLeftoverGoBuildComponent(part) {
			continue
		}
		for _, descendant := range parts[index+1:] {
			if descendant == "exe" {
				return true
			}
		}
	}
	return false
}

func isTestLeftoverGoBuildComponent(part string) bool {
	const prefix = "go-build"
	if !strings.HasPrefix(part, prefix) || len(part) == len(prefix) {
		return false
	}
	for _, digit := range part[len(prefix):] {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func testLeftoverPathParts(path string) []string {
	normalized := strings.ReplaceAll(strings.ToLower(path), `\`, "/")
	raw := strings.Split(normalized, "/")
	parts := raw[:0]
	for _, part := range raw {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	return parts
}

func containsTestLeftoverPartSequence(parts, sequence []string) bool {
	if len(sequence) == 0 || len(parts) < len(sequence) {
		return false
	}
	for start := 0; start <= len(parts)-len(sequence); start++ {
		matched := true
		for offset := range sequence {
			if parts[start+offset] != sequence[offset] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func classifyTestLeftoverArgv(commandLine, imageFamily string) string {
	argv := splitTestLeftoverCommandLine(commandLine)
	if len(argv) < 2 {
		return "unrecognized"
	}
	switch strings.ToLower(strings.TrimSpace(argv[1])) {
	case "supervise":
		return "supervise"
	case "gui":
		if imageFamily == "gui-e2e" {
			return "gui-e2e"
		}
		return "gui"
	case "daemon":
		if imageFamily == "reliability-temp" {
			return "reliability-daemon"
		}
	}
	return "unrecognized"
}

func splitTestLeftoverCommandLine(commandLine string) []string {
	var args []string
	var current strings.Builder
	inQuote := false
	flush := func() {
		if current.Len() == 0 {
			return
		}
		args = append(args, current.String())
		current.Reset()
	}
	for _, r := range strings.TrimSpace(commandLine) {
		switch {
		case r == '"':
			inQuote = !inQuote
		case unicode.IsSpace(r) && !inQuote:
			flush()
		default:
			current.WriteRune(r)
		}
	}
	flush()
	return args
}

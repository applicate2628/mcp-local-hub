//go:build windows

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
	hubprocess "mcp-local-hub/internal/process"
)

const cstProbeTestPythonEnv = "MCPHUB_CST_PROBE_TEST_PYTHON"

const cstGenerationInputLimit = 128 << 10

const (
	cstBindingRequestSchema = "mcphub.cst.saved_field.selector_binding.request.v1"
	cstBindingReceiptSchema = "mcphub.cst.saved_field.selector_binding.receipt.v1"
)

func cstFingerprintBudgetFromBaseline(baseline cstProbeManifest, growth int64) (int64, int64, error) {
	if baseline.Schema != cstProbeManifestSchema || len(baseline.Rows) == 0 || growth < 0 {
		return 0, 0, &cstFingerprintError{Kind: "invalid_input"}
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	var sum, largest int64
	for _, row := range baseline.Rows {
		if row.Size < 0 || row.Size > maxInt64-sum {
			return 0, 0, &cstFingerprintError{Kind: "size_limit"}
		}
		sum += row.Size
		if row.Size > largest {
			largest = row.Size
		}
	}
	if largest == 0 {
		largest = 1
	}
	if sum == 0 {
		sum = 1
	}
	if largest > maxInt64-growth || sum > maxInt64-growth {
		return 0, 0, &cstFingerprintError{Kind: "size_limit"}
	}
	return largest + growth, sum + growth, nil
}

func TestCSTSavedFieldFingerprintBaselineBudget(t *testing.T) {
	baseline := cstProbeManifest{Schema: cstProbeManifestSchema, Rows: []cstProbeManifestRow{{Size: 7}, {Size: 11}}}
	row, total, err := cstFingerprintBudgetFromBaseline(baseline, 5)
	if err != nil || row != 16 || total != 23 {
		t.Fatalf("baseline+growth budget row=%d total=%d err=%v, want 16/23", row, total, err)
	}
}

func TestCSTSavedFieldFingerprintZeroSizeRowAtExactAggregateBudget(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "probe.cst")
	cstProbeWriteFile(t, project, "project")
	cstProbeWriteFile(t, filepath.Join(root, "probe", "Model", "empty.bin"), "")
	manifest, err := cstBindingInventoryManifestWithBudget(project, filepath.Join(root, "probe"), true, 7, 7)
	if err != nil || len(manifest.Rows) != 2 {
		t.Fatalf("zero-size row after exact budget was rejected: rows=%d err=%v", len(manifest.Rows), err)
	}
}

var cstBindingAllowedEvents = []string{
	"environment-census", "environment-new", "project-open", "result-tree-first-child",
	"result-tree-next-item", "result-tree-file", "select-tree-item", "field-frequency",
	"project-close", "environment-close",
}

var cstBindingForbiddenEvents = []string{
	"solve", "remesh", "history", "export", "project-save", "3DSave",
	"GetFieldVector", "GetResultFromTreeItem", "Result3D", "registration",
}

const (
	cstProbeRequestSchema  = "mcphub.cst.saved_field.result_tree_probe.request.v1"
	cstProbeReceiptSchema  = "mcphub.cst.saved_field.result_tree_probe.receipt.v1"
	cstProbeManifestSchema = "sha256-canonical-file-list-v2"
	cstProbeRunEnv         = "MCPHUB_CST_RESULT_TREE_RUN"
	cstProbePythonEnv      = "MCPHUB_CST_RESULT_TREE_PYTHON"
	cstProbeCSTRootEnv     = "MCPHUB_CST_RESULT_TREE_CST_ROOT"
	cstProbeFixtureEnv     = "MCPHUB_CST_RESULT_TREE_FIXTURE"
	cstProbeScratchEnv     = "MCPHUB_CST_RESULT_TREE_SCRATCH"
	cstProbeFixtureSHA256  = "b29a4cde6a234c052d7d5e75a6a529e6648f7f817f083b25858e6b49ab633a7e"
)

type cstProbeManifestRow struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Stream     string `json:"stream"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	ModifiedNS int64  `json:"modified_ns"`
}

type cstProbeManifest struct {
	Schema          string                `json:"schema"`
	Rows            []cstProbeManifestRow `json:"rows"`
	AggregateSHA256 string                `json:"aggregate_sha256"`
}

type cstProbeRequest struct {
	Schema             string           `json:"schema"`
	SourceProject      string           `json:"source_project"`
	WorkspaceRoot      string           `json:"workspace_root"`
	ExpectedProjectSHA string           `json:"expected_project_sha256"`
	ExpectedHelperSHA  string           `json:"expected_helper_sha256"`
	ExpectedPythonSHA  string           `json:"expected_python_sha256"`
	SourceManifest     cstProbeManifest `json:"source_manifest"`
	TestStallAfterOpen int              `json:"test_stall_after_open_ms,omitempty"`
	TestMutation       string           `json:"test_preservation_mutation,omitempty"`
}

type cstProbeRowState struct {
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	ModifiedNS int64  `json:"modified_ns"`
}

type cstProbeRowDifference struct {
	Side       string            `json:"side"`
	Label      string            `json:"label"`
	Membership string            `json:"membership"`
	Before     *cstProbeRowState `json:"before"`
	After      *cstProbeRowState `json:"after"`
}

type cstProbeForeignDifference struct {
	Added   []string `json:"added"`
	Missing []string `json:"missing"`
}

type cstProbePreservationEvidence struct {
	Rows                 []cstProbeRowDifference   `json:"rows"`
	ForeignIdentityDelta cstProbeForeignDifference `json:"foreign_identity_delta"`
}

type cstProbeAPIEvent struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
}

type cstProbeAPITrace struct {
	Events []cstProbeAPIEvent `json:"events"`
	Counts map[string]int     `json:"counts"`
}

type cstProbeReceipt struct {
	Schema                    string                        `json:"schema"`
	Status                    string                        `json:"status"`
	FailureID                 string                        `json:"failure_id"`
	SourceManifestSHA256      string                        `json:"source_manifest_sha256"`
	DestinationManifestSHA256 string                        `json:"destination_manifest_sha256"`
	SessionIdentitySHA256     string                        `json:"session_identity_sha256"`
	Root                      string                        `json:"root"`
	Selectors                 []string                      `json:"selectors"`
	TraversalCount            int                           `json:"traversal_count"`
	MaxDepth                  int                           `json:"max_depth"`
	MaxNodes                  int                           `json:"max_nodes"`
	MaxSelectorBytes          int                           `json:"max_selector_bytes"`
	WorkspaceSettled          bool                          `json:"workspace_settled"`
	SessionSettled            bool                          `json:"session_settled"`
	SourceUnchanged           bool                          `json:"source_unchanged"`
	CacheCleared              bool                          `json:"cache_cleared"`
	ClosedWithoutSave         bool                          `json:"closed_without_save"`
	ForeignProcessesPreserved bool                          `json:"foreign_processes_preserved"`
	Preservation              *cstProbePreservationEvidence `json:"preservation"`
	APITrace                  *cstProbeAPITrace             `json:"api_trace"`
}

type cstProbeFailureReceipt struct {
	Schema    string `json:"schema"`
	Status    string `json:"status"`
	FailureID string `json:"failure_id"`
}

type cstBindingPair struct {
	Field     string `json:"field"`
	Port      int    `json:"port"`
	M3DPath   string `json:"m3d_path"`
	M3DSHA256 string `json:"m3d_sha256"`
	RexPath   string `json:"rex_path"`
	RexSHA256 string `json:"rex_sha256"`
}

type cstBindingRequest struct {
	Schema              string           `json:"schema"`
	SourceProject       string           `json:"source_project"`
	WorkspaceRoot       string           `json:"workspace_root"`
	ExpectedProjectSHA  string           `json:"expected_project_sha256"`
	ExpectedHelperSHA   string           `json:"expected_helper_sha256"`
	ExpectedPythonSHA   string           `json:"expected_python_sha256"`
	FullManifest        cstProbeManifest `json:"full_manifest"`
	ResultManifest      cstProbeManifest `json:"result_manifest"`
	Pairs               []cstBindingPair `json:"pairs"`
	ExpectedDirectories []string         `json:"-"` // Go-only pre-run directory identity; not a receipt/request field.
	TestForbiddenCall   string           `json:"test_forbidden_call,omitempty"`
	TestStallAfterOpen  int              `json:"test_stall_after_open_ms,omitempty"`
}

type cstBindingRow struct {
	Field        string  `json:"field"`
	Port         int     `json:"port"`
	Selector     string  `json:"selector"`
	RunID        string  `json:"run_id"`
	ReturnedFile string  `json:"returned_file"`
	M3DPath      string  `json:"m3d_path"`
	M3DSHA256    string  `json:"m3d_sha256"`
	RexPath      string  `json:"rex_path"`
	RexSHA256    string  `json:"rex_sha256"`
	RawFrequency float64 `json:"raw_frequency"`
}

type cstBindingReceipt struct {
	Schema               string                        `json:"schema"`
	Status               string                        `json:"status"`
	FailureID            string                        `json:"failure_id"`
	Rows                 []cstBindingRow               `json:"rows"`
	FullManifestSHA256   string                        `json:"full_manifest_sha256"`
	ResultManifestSHA256 string                        `json:"result_manifest_sha256"`
	CopyManifestSHA256   string                        `json:"copy_manifest_sha256"`
	CopyResultSHA256     string                        `json:"copy_result_sha256"`
	SourceUnchanged      bool                          `json:"source_unchanged"`
	CopyUnchanged        bool                          `json:"copy_unchanged"`
	ForeignPreserved     bool                          `json:"foreign_processes_preserved"`
	SessionSettled       bool                          `json:"session_settled"`
	ClosedWithoutSave    bool                          `json:"closed_without_save"`
	APITrace             *cstProbeAPITrace             `json:"api_trace"`
	SelectorDiagnostic   *cstBindingSelectorDiagnostic `json:"selector_diagnostic,omitempty"`
}

type cstBindingSelectorLabel struct {
	Parent string `json:"parent"`
	Leaf   string `json:"leaf"`
}

type cstBindingSelectorDiagnostic struct {
	Branch         string                    `json:"branch"`
	TotalCount     int                       `json:"total_count"`
	CandidateCount *int                      `json:"candidate_count"`
	Labels         []cstBindingSelectorLabel `json:"labels"`
	Truncated      bool                      `json:"truncated"`
}

type cstBindingCopyDelta struct {
	Path   string           `json:"path"`
	Before cstProbeRowState `json:"before"`
	After  cstProbeRowState `json:"after"`
}

type cstBindingSummary struct {
	Status               string                        `json:"status"`
	FailureID            string                        `json:"failure_id"`
	RowCount             int                           `json:"row_count"`
	Rows                 []cstBindingRow               `json:"rows,omitempty"`
	RowsSHA256           string                        `json:"rows_sha256,omitempty"`
	TraceCount           int                           `json:"trace_event_count"`
	TraceEvents          []cstProbeAPIEvent            `json:"trace_events,omitempty"`
	TraceCounts          map[string]int                `json:"trace_counts,omitempty"`
	SelectorDiagnostic   *cstBindingSelectorDiagnostic `json:"selector_diagnostic,omitempty"`
	ClosedWithoutSave    bool                          `json:"closed_without_save"`
	SessionSettled       bool                          `json:"session_settled"`
	SourceUnchanged      bool                          `json:"source_unchanged"`
	CopyUnchanged        bool                          `json:"copy_unchanged"`
	ForeignPreserved     bool                          `json:"foreign_preserved"`
	SourceFullSHA        string                        `json:"source_full_sha"`
	SourceResultSHA      string                        `json:"source_result_sha"`
	CopyFullSHA          string                        `json:"copy_full_sha"`
	CopyResultSHA        string                        `json:"copy_result_sha"`
	ReceiptCopyFullSHA   string                        `json:"receipt_copy_full_sha"`
	ReceiptCopyResultSHA string                        `json:"receipt_copy_result_sha"`
	CopyDeltas           []cstBindingCopyDelta         `json:"copy_deltas,omitempty"`
	CopyDeltaCount       int                           `json:"copy_delta_count"`
	CopyAdded            []cstProbeManifestRow         `json:"copy_added,omitempty"`
	CopyAddedCount       int                           `json:"copy_added_count"`
	ReasonCodes          []string                      `json:"reason_codes,omitempty"`
	Disposition          string                        `json:"disposition"`
}

func cstProbeRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve probe test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}

func cstProbeHelperPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(
		cstProbeRepositoryRoot(t),
		"servers", "electromagnetics-mcp", "tests", "_helpers",
		"cst_saved_field_result_tree_probe.py",
	)
}

func cstGenerationHelperPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(
		cstProbeRepositoryRoot(t),
		"servers", "electromagnetics-mcp", "tests", "_helpers",
		"cst_saved_field_fixture_generation.py",
	)
}

type cstGenerationRequest struct {
	Schema              string            `json:"schema"`
	SourceProject       string            `json:"source_project"`
	OutputRoot          string            `json:"output_root"`
	ExpectedProjectSHA  string            `json:"expected_project_sha256"`
	ExpectedHelperSHA   string            `json:"expected_helper_sha256"`
	ExpectedPythonSHA   string            `json:"expected_python_sha256"`
	SourceManifest      cstProbeManifest  `json:"source_manifest"`
	TestStallMS         int               `json:"test_stall_before_solver_ms,omitempty"`
	TestInjectStage     string            `json:"test_inject_stage,omitempty"`
	TestSetupOnly       bool              `json:"test_setup_only,omitempty"`
	TestPort2Completion bool              `json:"test_port2_completion,omitempty"`
	DiagnosticCopyMode  string            `json:"diagnostic_copy_mode,omitempty"`
	FullSourceManifest  *cstProbeManifest `json:"full_source_manifest,omitempty"`
}

type cstGenerationResultRow struct {
	Field string `json:"field"`
	Port  int    `json:"port"`
	Mode  int    `json:"mode"`
	Path  string `json:"path"`
}

type cstGenerationBundleProbe struct {
	CopyMode  string `json:"copy_mode"`
	PortCount int    `json:"port_count"`
}

type cstGenerationReceipt struct {
	Schema                 string                    `json:"schema"`
	Status                 string                    `json:"status"`
	FailureID              string                    `json:"failure_id"`
	Stage                  string                    `json:"stage"`
	ExceptionType          string                    `json:"exception_type"`
	CleanupStage           string                    `json:"cleanup_stage"`
	CleanupExceptionType   string                    `json:"cleanup_exception_type"`
	CompilerDiagnostic     string                    `json:"compiler_diagnostic"`
	SourceManifestSHA256   string                    `json:"source_manifest_sha256"`
	RetainedManifestSHA256 string                    `json:"retained_manifest_sha256"`
	SessionIdentitySHA256  string                    `json:"session_identity_sha256"`
	Results                []cstGenerationResultRow  `json:"results"`
	APITrace               *cstProbeAPITrace         `json:"api_trace"`
	SourceUnchanged        bool                      `json:"source_unchanged"`
	ForeignPreserved       bool                      `json:"foreign_processes_preserved"`
	SessionSettled         bool                      `json:"session_settled"`
	RetainedBundle         string                    `json:"retained_bundle"`
	FullSourceManifestSHA  string                    `json:"full_source_manifest_sha256"`
	FullSourceRowCount     int                       `json:"full_source_row_count"`
	BundleProbe            *cstGenerationBundleProbe `json:"bundle_probe"`
	ObservedSelectors      []string                  `json:"observed_selectors"`
}

func cstProbeTestPython(t *testing.T) string {
	t.Helper()
	python := os.Getenv(cstProbeTestPythonEnv)
	if python == "" {
		t.Skipf("set %s to an explicit Python executable", cstProbeTestPythonEnv)
	}
	absolute, err := filepath.Abs(python)
	if err != nil {
		t.Fatalf("resolve explicit Python: %v", err)
	}
	info, err := os.Stat(absolute)
	if err != nil || info.IsDir() {
		t.Fatalf("explicit Python unavailable: %v", err)
	}
	return absolute
}

func cstProbeMinimalEnvironment(python string) []string {
	root := os.Getenv("SystemRoot")
	environment := []string{
		"SystemRoot=" + root,
		"WINDIR=" + root,
		"PATH=" + filepath.Join(root, "System32") + ";" + root,
		"PYTHONHOME=" + filepath.Dir(python),
		"PYTHONDONTWRITEBYTECODE=1",
		"PYTHONNOUSERSITE=1",
		"PYTHONUTF8=1",
	}
	for _, key := range []string{
		"TEMP", "TMP", "COMSPEC", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA",
	} {
		if value := os.Getenv(key); value != "" {
			environment = append(environment, key+"="+value)
		}
	}
	return environment
}

func cstProbeFileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func cstProbeManifestForProject(t *testing.T, project string) cstProbeManifest {
	t.Helper()
	manifest, err := cstProbeManifestForProjectChecked(project)
	if err != nil {
		t.Fatalf("five-row source inventory: %v", err)
	}
	return manifest
}

func cstProbeManifestForProjectChecked(project string) (cstProbeManifest, error) {
	return cstProbeManifestForProjectCheckedWithBudget(project, cstFingerprintInitialAggregateBudget)
}

func cstProbeManifestForProjectCheckedWithBudget(project string, aggregateBudget int64) (cstProbeManifest, error) {
	if aggregateBudget <= 0 {
		return cstProbeManifest{}, &cstFingerprintError{Kind: "invalid_input"}
	}
	stem := strings.TrimSuffix(filepath.Base(project), filepath.Ext(project))
	relatives := []string{
		filepath.Base(project),
		filepath.ToSlash(filepath.Join(stem, "Result", "db.parmap")),
		filepath.ToSlash(filepath.Join(stem, "Result", "DS", "db.parmap")),
		filepath.ToSlash(filepath.Join(stem, "Result", "Model.res")),
		filepath.ToSlash(filepath.Join(stem, "Result", "Storage.sdb")),
	}
	sort.Strings(relatives)
	rows := make([]cstProbeManifestRow, 0, len(relatives))
	aggregate := sha256.New()
	remaining := aggregateBudget
	for _, relative := range relatives {
		if remaining < 0 {
			return cstProbeManifest{}, &cstFingerprintError{Kind: "size_limit", Relative: relative}
		}
		row, err := cstFingerprintFile(context.Background(), filepath.Dir(project), relative, min(cstFingerprintInitialRowBudget, remaining), nil)
		if err != nil {
			return cstProbeManifest{}, err
		}
		remaining -= row.Size
		rows = append(rows, row)
		fmt.Fprintf(
			aggregate,
			"%s\x00%s\x00%s\x00%d\x00%s\x00%d\n",
			row.Path, row.Type, row.Stream, row.Size, row.SHA256, row.ModifiedNS,
		)
	}
	return cstProbeManifest{
		Schema:          cstProbeManifestSchema,
		Rows:            rows,
		AggregateSHA256: fmt.Sprintf("%x", aggregate.Sum(nil)),
	}, nil
}

func TestCSTSavedFieldFiveRowExactBudgetAllowsFinalZeroOnly(t *testing.T) {
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	stem := strings.TrimSuffix(filepath.Base(project), filepath.Ext(project))
	lastRelative := filepath.ToSlash(filepath.Join(stem, "Result", "db.parmap"))
	lastPath := filepath.Join(filepath.Dir(project), filepath.FromSlash(lastRelative))
	if err := os.WriteFile(lastPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	previous := []string{filepath.Base(project), filepath.ToSlash(filepath.Join(stem, "Result", "DS", "db.parmap")), filepath.ToSlash(filepath.Join(stem, "Result", "Model.res")), filepath.ToSlash(filepath.Join(stem, "Result", "Storage.sdb"))}
	var capBytes int64
	for _, relative := range previous {
		info, err := os.Stat(filepath.Join(filepath.Dir(project), filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		capBytes += info.Size()
	}
	if capBytes <= 0 {
		t.Fatal("test aggregate cap must be positive")
	}
	zero, err := cstProbeManifestForProjectCheckedWithBudget(project, capBytes)
	if err != nil || len(zero.Rows) != 5 || zero.Rows[4].Size != 0 {
		t.Fatalf("final zero row rejected: rows=%d err=%v", len(zero.Rows), err)
	}
	ordinary, err := cstProbeManifestForProjectChecked(project)
	if err != nil || !reflect.DeepEqual(zero, ordinary) {
		t.Fatalf("five-row wire changed at exact cap: %v", err)
	}
	if err := os.WriteFile(lastPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	partial, err := cstProbeManifestForProjectCheckedWithBudget(project, capBytes)
	var typed *cstFingerprintError
	if !errors.As(err, &typed) || typed.Kind != "size_limit" || typed.DataReadEntered || len(partial.Rows) != 0 {
		t.Fatalf("final positive row admitted: rows=%d err=%v", len(partial.Rows), err)
	}
	entered := 0
	_, err = cstFingerprintFile(context.Background(), filepath.Dir(project), lastRelative, 0, &cstFingerprintHooks{BeforeRead: func() error { entered++; return nil }})
	if !errors.As(err, &typed) || typed.Kind != "size_limit" || typed.DataReadEntered || entered != 0 {
		t.Fatalf("zero budget entered data read: entered=%d err=%v", entered, err)
	}
}

func TestCSTSavedFieldFiveRowManifestRejectsOutsideHardlink(t *testing.T) {
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	stem := strings.TrimSuffix(filepath.Base(project), filepath.Ext(project))
	relative := filepath.ToSlash(filepath.Join(stem, "Result", "Storage.sdb"))
	target := filepath.Join(filepath.Dir(project), filepath.FromSlash(relative))
	outside := filepath.Join(root, "synthetic-outside.bin")
	content := []byte("test-owned five-row sentinel")
	if err := os.WriteFile(outside, content, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, target); err != nil {
		t.Fatal(err)
	}
	manifest, err := cstProbeManifestForProjectChecked(project)
	var refused *cstFingerprintError
	if !errors.As(err, &refused) || refused.Kind != "multi_link" || refused.Relative != relative || refused.DataReadEntered || len(manifest.Rows) != 0 {
		t.Fatalf("five-row hardlink admitted or lacked pre-read witness: rows=%d err=%v", len(manifest.Rows), err)
	}
	after, err := os.Stat(outside)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("outside identity changed: %v", err)
	}
	got, err := os.ReadFile(outside)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("outside synthetic bytes changed: %v", err)
	}
}

func cstProbeWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Base(filepath.Dir(path)), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}

func cstProbeSyntheticBundle(t *testing.T, root string) string {
	t.Helper()
	project := filepath.Join(root, "line10.cst")
	rows := map[string]string{
		project: "project",
		filepath.Join(root, "line10", "Result", "db.parmap"):       "root-map",
		filepath.Join(root, "line10", "Result", "DS", "db.parmap"): "ds-map",
		filepath.Join(root, "line10", "Result", "Model.res"):       "model",
		filepath.Join(root, "line10", "Result", "Storage.sdb"):     "storage-SECRET_PRIVATE_SENTINEL",
	}
	for path, content := range rows {
		cstProbeWriteFile(t, path, content)
	}
	return project
}

func cstProbeSyntheticInterface(t *testing.T, root string) (string, string) {
	t.Helper()
	trace := filepath.Join(root, "trace.jsonl")
	moduleRoot := filepath.Join(root, "fake-python")
	cstProbeWriteFile(t, filepath.Join(moduleRoot, "cst", "__init__.py"), "")
	cstProbeWriteFile(t, filepath.Join(moduleRoot, "cst", "interface", "__init__.py"), `
import json
import os

_running = {111, 222}
_trace_path = os.environ["MCPHUB_CST_PROBE_TRACE"]

def _trace(name, value=None):
    with open(_trace_path, "a", encoding="utf-8") as stream:
        stream.write(json.dumps({"name": name, "value": value}, sort_keys=True) + "\n")

def running_design_environments():
    _trace("running_design_environments", sorted(_running))
    return sorted(_running)

class _Tree:
    def GetFirstChildName(self, parent):
        _trace("GetFirstChildName", parent)
        return {
            "2D/3D Results": r"2D/3D Results\E-Field\e1",
        }.get(parent, "")

    def GetNextItemName(self, current):
        _trace("GetNextItemName", current)
        return {
            r"2D/3D Results\E-Field\e1": r"2D/3D Results\H-Field\h1",
        }.get(current, "")

class _Model3D:
    ResultTree = _Tree()

class _Project:
    model3d = _Model3D()
    def close(self):
        _trace("project.close")

class DesignEnvironment:
    @staticmethod
    def new():
        _trace("DesignEnvironment.new")
        _running.add(333)
        return DesignEnvironment()

    def open_project(self, path):
        _trace("open_project", os.path.basename(path))
        return _Project()

    def close(self):
        _trace("DesignEnvironment.close")
        _running.discard(333)
        if os.environ.get("MCPHUB_CST_PROBE_FAKE_FOREIGN_DELTA") == "1":
            _running.discard(222)
            _running.add(444)
`)
	return moduleRoot, trace
}

func cstGenerationSyntheticInterface(t *testing.T, root string) (string, string) {
	t.Helper()
	trace := filepath.Join(root, "generation-trace.jsonl")
	moduleRoot := filepath.Join(root, "generation-fake-python")
	cstProbeWriteFile(t, filepath.Join(moduleRoot, "cst", "__init__.py"), "")
	cstProbeWriteFile(t, filepath.Join(moduleRoot, "cst", "interface", "__init__.py"), `
import json
import os
from pathlib import Path

_running = {101, 202}
_trace_path = os.environ["MCPHUB_CST_GENERATION_TRACE"]

def _trace(name, value=None):
    with open(_trace_path, "a", encoding="utf-8") as stream:
        stream.write(json.dumps({"name": name, "value": value}, sort_keys=True) + "\n")

def running_design_environments():
    _trace("running_design_environments", sorted(_running))
    return sorted(_running)

class _Tree:
    _items = [
        r"2D/3D Results\E-Field (f=3GHz)\Port 1 Mode 1",
        r"2D/3D Results\E-Field (f=3GHz)\Port 2 Mode 1",
        r"2D/3D Results\H-Field (f=3GHz)\Port 1 Mode 1",
        r"2D/3D Results\H-Field (f=3GHz)\Port 2 Mode 1",
    ]
    def GetFirstChildName(self, parent):
        _trace("GetFirstChildName", parent)
        if os.environ.get("MCPHUB_CST_GENERATION_BAD_TREE") == "1":
            return r"2D/3D Results\\E-Field (f=3GHz)\\Port 1 Mode 1" if parent == "2D/3D Results" else ""
        return self._items[0] if parent == "2D/3D Results" else ""
    def GetNextItemName(self, current):
        _trace("GetNextItemName", current)
        try:
            index = self._items.index(current) + 1
        except ValueError:
            return ""
        return self._items[index] if index < len(self._items) else ""

class _Model3D:
    ResultTree = _Tree()
    def __init__(self, project_path):
        self._project_path = Path(project_path)
        self._global_data = {}
        self._port2_completion_requested = False
        self._transient_port2_ready = False
    def add_to_history(self, name, command):
        _trace("add_to_history", {"name": name, "command": command})
        if os.environ.get("MCPHUB_CST_GENERATION_SETUP_ERROR"):
            raise RuntimeError(os.environ["MCPHUB_CST_GENERATION_SETUP_ERROR"])
    def run_solver(self):
        _trace("run_solver")
        if self._port2_completion_requested and not self._transient_port2_ready:
            raise RuntimeError("synthetic transient port 2 stimulation required")
        result = self._project_path.with_suffix("") / "Result" / "generated"
        result.mkdir(parents=True, exist_ok=True)
        for name in ("e-p1.bin", "e-p2.bin", "h-p1.bin", "h-p2.bin"):
            (result / name).write_bytes((name + "-payload").encode())
        return True
    def _execute_vba_code(self, code):
        _trace("execute_vba_code", code)
        model_state = self._project_path.with_suffix("") / "Model" / "Model.prj"
        self._global_data["MCPHUB_BUNDLE_PROBE_V1"] = os.environ.get("MCPHUB_CST_GENERATION_PORT_COUNT", "2" if model_state.is_file() else "0")
        if "MCPHUB_PORT2" in code:
            self._port2_completion_requested = True
            if not code.startswith("Sub Main\n") or not code.endswith("\nEnd Sub"):
                raise RuntimeError("synthetic VBA requires Sub Main module wrapper")
            self._transient_port2_ready = all(value in code for value in (
                "With Solver",
                ".StimulationPort 2",
                ".StimulationMode 1",
                ".ConsiderTwoPortReciprocity False",
                "Solver.GetStimulationPort",
                "Solver.GetStimulationMode",
            ))
            self._global_data["MCPHUB_PORT2"] = os.environ.get("MCPHUB_CST_GENERATION_PORT2_GUARD", "2:1")
    def GetGlobalData(self, name):
        _trace("GetGlobalData", name)
        return self._global_data.get(name, "")

class _Project:
    def __init__(self, path):
        self.model3d = _Model3D(path)
    def save(self, path="", include_results=True, allow_overwrite=False):
        _trace("project.save", {"path": path, "include_results": include_results, "allow_overwrite": allow_overwrite})
        if isinstance(path, bool):
            raise TypeError("path must be os.PathLike")
        if os.environ.get("MCPHUB_CST_GENERATION_SAVE_ERROR"):
            raise RuntimeError("synthetic save failure")
    def close(self):
        _trace("project.close")

class DesignEnvironment:
    @staticmethod
    def new():
        _trace("DesignEnvironment.new")
        _running.add(303)
        return DesignEnvironment()
    def open_project(self, path):
        _trace("open_project", os.path.basename(path))
        return _Project(path)
    def close(self):
        _trace("DesignEnvironment.close")
        _running.discard(303)
`)
	return moduleRoot, trace
}

func cstProbeEnvironment(python string, pythonPaths []string, extra map[string]string) []string {
	environment := cstProbeMinimalEnvironment(python)
	environment = append(environment, "PYTHONPATH="+strings.Join(pythonPaths, string(os.PathListSeparator)))
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		environment = append(environment, key+"="+extra[key])
	}
	return environment
}

type cstLegacyChildError struct {
	Stage string
	Cause error
}

func (e *cstLegacyChildError) Error() string { return e.Stage + ": " + e.Cause.Error() }
func (e *cstLegacyChildError) Unwrap() error { return e.Cause }

func cstDecodeLegacyJSON(stage string, result hubprocess.StrictRunResult, target any) error {
	if result.Stdout.Truncated || len(result.Stdout.Prefix) == 0 {
		return &cstLegacyChildError{Stage: stage, Cause: fmt.Errorf("missing or truncated child JSON")}
	}
	if err := json.Unmarshal(result.Stdout.Prefix, target); err != nil {
		return &cstLegacyChildError{Stage: stage, Cause: err}
	}
	return nil
}

func cstDecodeProbeReceipt(result hubprocess.StrictRunResult, runErr error) (cstProbeReceipt, error) {
	var receipt cstProbeReceipt
	if err := cstDecodeLegacyJSON("receipt_decode_probe", result, &receipt); err != nil {
		return cstProbeReceipt{}, errors.Join(runErr, err)
	}
	if receipt.Schema != cstProbeReceiptSchema || receipt.Status == "" {
		return cstProbeReceipt{}, errors.Join(runErr, &cstLegacyChildError{Stage: "receipt_decode_probe", Cause: fmt.Errorf("receipt schema or status invalid")})
	}
	return receipt, runErr
}

func cstDecodeGenerationReceipt(result hubprocess.StrictRunResult, runErr error) (cstGenerationReceipt, error) {
	var receipt cstGenerationReceipt
	if err := cstDecodeLegacyJSON("receipt_decode_generation", result, &receipt); err != nil {
		return cstGenerationReceipt{}, errors.Join(runErr, err)
	}
	if receipt.Schema != "mcphub.cst.saved_field.fixture_generation.receipt.v1" || receipt.Status == "" {
		return cstGenerationReceipt{}, errors.Join(runErr, &cstLegacyChildError{Stage: "receipt_decode_generation", Cause: fmt.Errorf("receipt schema or status invalid")})
	}
	return receipt, runErr
}

type cstGenerationMetadataResponse struct {
	Schema         string           `json:"schema"`
	SourceManifest cstProbeManifest `json:"source_manifest"`
}

func cstDecodeGenerationMetadata(result hubprocess.StrictRunResult, runErr error) (cstProbeManifest, error) {
	var response cstGenerationMetadataResponse
	if err := cstDecodeLegacyJSON("metadata_invalid", result, &response); err != nil {
		return cstProbeManifest{}, errors.Join(runErr, err)
	}
	if response.Schema != "mcphub.cst.saved_field.fixture_generation.metadata.v1" ||
		response.SourceManifest.Schema != cstProbeManifestSchema || len(response.SourceManifest.Rows) == 0 {
		return cstProbeManifest{}, errors.Join(runErr, &cstLegacyChildError{Stage: "metadata_invalid", Cause: fmt.Errorf("metadata schema or source manifest invalid")})
	}
	return response.SourceManifest, runErr
}

func cstProbeRun(
	t *testing.T,
	ctx context.Context,
	python string,
	request cstProbeRequest,
	pythonPaths []string,
	extra map[string]string,
) (cstProbeReceipt, hubprocess.StrictRunResult, error) {
	t.Helper()
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return cstProbeReceipt{}, hubprocess.StrictRunResult{}, &cstLegacyChildError{Stage: "request_encode_probe", Cause: err}
	}
	cmd := exec.Command(python, cstProbeHelperPath(t))
	cmd.Env = cstProbeEnvironment(python, pythonPaths, extra)
	cmd.Dir = request.WorkspaceRoot
	result, runErr := hubprocess.RunStrictlyContained(ctx, hubprocess.StrictRunInvocation{
		Command:     cmd,
		Input:       requestBytes,
		InputLimit:  64 << 10,
		StdoutLimit: 1 << 20,
		StderrLimit: 64 << 10,
	})
	receipt, err := cstDecodeProbeReceipt(result, runErr)
	return receipt, result, err
}

func TestCSTSavedFieldLegacyChildReceiptDecodeAllReturns(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		wantError     bool
	}{
		{"empty", "", true},
		{"malformed", "{", true},
		{"partial", `{"schema":"mcphub.cst.saved_field.result_tree_probe.receipt.v1"`, true},
		{"missing_status", `{"schema":"mcphub.cst.saved_field.result_tree_probe.receipt.v1"}`, true},
		{"blocked", `{"schema":"mcphub.cst.saved_field.result_tree_probe.receipt.v1","status":"BLOCKED:fixture"}`, false},
		{"pass", `{"schema":"mcphub.cst.saved_field.result_tree_probe.receipt.v1","status":"PASS:owned_result_tree_listed"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt, err := cstDecodeProbeReceipt(hubprocess.StrictRunResult{Stdout: hubprocess.StrictRunCapture{Prefix: []byte(tc.payload)}}, nil)
			if (err != nil) != tc.wantError {
				t.Fatalf("receipt=%+v err=%v wantError=%v", receipt, err, tc.wantError)
			}
			if tc.wantError && (receipt.Schema != "" || receipt.Status != "") {
				t.Fatalf("invalid receipt admitted: %+v", receipt)
			}
		})
	}
	runnerCause := errors.New("synthetic runner close fault")
	_, joined := cstDecodeProbeReceipt(hubprocess.StrictRunResult{Stdout: hubprocess.StrictRunCapture{Prefix: []byte("{")}}, runnerCause)
	var staged *cstLegacyChildError
	if !errors.Is(joined, runnerCause) || !errors.As(joined, &staged) || staged.Stage != "receipt_decode_probe" {
		t.Fatalf("lost runner/decode causes: %v", joined)
	}
	for _, tc := range []struct {
		name, payload string
		valid         bool
	}{
		{"generation_empty", "", false},
		{"generation_partial", `{"schema":"mcphub.cst.saved_field.fixture_generation.receipt.v1"`, false},
		{"generation_blocked", `{"schema":"mcphub.cst.saved_field.fixture_generation.receipt.v1","status":"BLOCKED:fixture"}`, true},
		{"generation_pass", `{"schema":"mcphub.cst.saved_field.fixture_generation.receipt.v1","status":"PASS:setup_only"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt, err := cstDecodeGenerationReceipt(hubprocess.StrictRunResult{Stdout: hubprocess.StrictRunCapture{Prefix: []byte(tc.payload)}}, nil)
			if (err == nil) != tc.valid || (!tc.valid && receipt.Status != "") {
				t.Fatalf("receipt=%+v err=%v valid=%v", receipt, err, tc.valid)
			}
		})
	}
	for _, payload := range []string{"", "{", `{"schema":"mcphub.cst.saved_field.fixture_generation.metadata.v1"}`} {
		manifest, err := cstDecodeGenerationMetadata(hubprocess.StrictRunResult{Stdout: hubprocess.StrictRunCapture{Prefix: []byte(payload)}}, nil)
		if err == nil || len(manifest.Rows) != 0 {
			t.Fatalf("invalid metadata admitted: %+v err=%v", manifest, err)
		}
	}
}

func TestCSTSavedFieldLegacyFakeChildRun(t *testing.T) {
	if os.Getenv("MCPHUB_CST_R1_FAKE_CHILD") == "1" {
		_, _ = os.Stdout.WriteString(os.Getenv("MCPHUB_CST_R1_PAYLOAD"))
		os.Exit(0)
	}
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	full := cstGenerationFullManifest(t, project)
	resultBaseline, err := cstBindingResultInventory(project)
	if err != nil {
		t.Fatal(err)
	}
	projectRow, err := cstNativeManifestExactRow(full, filepath.Base(project))
	if err != nil {
		t.Fatal(err)
	}
	foreignRoot := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreignRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	foreignBefore := map[int]cstProbeProcessRow{}
	for _, tc := range []struct {
		name, payload string
		valid         bool
	}{
		{"empty", "", false},
		{"malformed", "{", false},
		{"partial", `{"schema":"mcphub.cst.saved_field.fixture_generation.receipt.v1"`, false},
		{"blocked", `{"schema":"mcphub.cst.saved_field.fixture_generation.receipt.v1","status":"BLOCKED:fixture"}`, true},
		{"pass", `{"schema":"mcphub.cst.saved_field.fixture_generation.receipt.v1","status":"PASS:setup_only"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCSTSavedFieldLegacyFakeChildRun$")
			cmd.Env = append(os.Environ(), "MCPHUB_CST_R1_FAKE_CHILD=1", "MCPHUB_CST_R1_PAYLOAD="+tc.payload)
			cmd.Dir = t.TempDir()
			result, runErr := hubprocess.RunStrictlyContained(ctx, hubprocess.StrictRunInvocation{Command: cmd, InputLimit: 1, StdoutLimit: 1 << 20, StderrLimit: 64 << 10})
			receipt, err := cstDecodeGenerationReceipt(result, runErr)
			reasons := cstGenerationPostflight(project, full, resultBaseline, projectRow.SHA256, foreignRoot, foreignBefore)
			if len(reasons) != 0 {
				t.Fatalf("fake child postflight=%v", reasons)
			}
			if runErr != nil || (err == nil) != tc.valid || (!tc.valid && receipt.Status != "") {
				t.Fatalf("runner=%v receipt=%+v decode=%v valid=%v", runErr, receipt, err, tc.valid)
			}
		})
	}
}

func cstGenerationRun(
	t *testing.T,
	ctx context.Context,
	python string,
	request cstGenerationRequest,
	pythonPaths []string,
	extra map[string]string,
) (cstGenerationReceipt, hubprocess.StrictRunResult, error) {
	t.Helper()
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return cstGenerationReceipt{}, hubprocess.StrictRunResult{}, &cstLegacyChildError{Stage: "request_encode_generation", Cause: err}
	}
	cmd := exec.Command(python, cstGenerationHelperPath(t))
	cmd.Env = cstProbeEnvironment(python, pythonPaths, extra)
	cmd.Dir = request.OutputRoot
	result, runErr := hubprocess.RunStrictlyContained(ctx, hubprocess.StrictRunInvocation{
		Command: cmd, Input: requestBytes, InputLimit: cstGenerationInputLimit, StdoutLimit: 1 << 20, StderrLimit: 64 << 10,
	})
	receipt, err := cstDecodeGenerationReceipt(result, runErr)
	return receipt, result, err
}

func cstGenerationPaddedRequest(t *testing.T, project, outputRoot string, manifest cstProbeManifest, length int) cstGenerationRequest {
	t.Helper()
	request := cstGenerationRequest{
		Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", SourceProject: project, OutputRoot: outputRoot,
		ExpectedHelperSHA: strings.Repeat("0", 64), ExpectedPythonSHA: strings.Repeat("0", 64), SourceManifest: manifest,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > length {
		t.Fatalf("base generation request length=%d exceeds target=%d", len(encoded), length)
	}
	request.ExpectedProjectSHA = strings.Repeat("0", length-len(encoded))
	encoded, err = json.Marshal(request)
	if err != nil || len(encoded) != length {
		t.Fatalf("generation request length=%d want=%d err=%v", len(encoded), length, err)
	}
	return request
}

func TestCSTSavedFieldGenerationFullManifestWire(t *testing.T) {
	full := cstProbeManifest{
		Schema:          cstProbeManifestSchema,
		Rows:            []cstProbeManifestRow{{Path: "line10.cst", Type: "regular", Stream: "::$DATA", Size: 7, SHA256: strings.Repeat("0", 64), ModifiedNS: 1}},
		AggregateSHA256: strings.Repeat("1", 64),
	}
	for _, test := range []struct {
		name    string
		request cstGenerationRequest
		present bool
	}{
		{name: "absent", request: cstGenerationRequest{}, present: false},
		{name: "present", request: cstGenerationRequest{FullSourceManifest: &full}, present: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.request)
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			raw, present := wire["full_source_manifest"]
			if present != test.present {
				t.Fatalf("wire=%s present=%t want=%t", encoded, present, test.present)
			}
			if !present {
				return
			}
			var decoded cstProbeManifest
			if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Schema != full.Schema || decoded.AggregateSHA256 != full.AggregateSHA256 || len(decoded.Rows) != len(full.Rows) {
				t.Fatalf("full manifest wire=%s decoded=%+v err=%v", raw, decoded, err)
			}
		})
	}
}

func TestCSTSavedFieldGenerationInputBudget(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	outputRoot := filepath.Join(root, "output")
	if err := os.Mkdir(outputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := cstProbeManifestForProject(t, project)
	repositoryRoot := cstProbeRepositoryRoot(t)
	for _, test := range []struct {
		name   string
		length int
		launch bool
	}{
		{name: "representative-87327", length: 87327, launch: true},
		{name: "generation-128kib", length: 128 << 10, launch: true},
		{name: "generation-128kib-plus-one", length: (128 << 10) + 1, launch: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := cstGenerationPaddedRequest(t, project, outputRoot, manifest, test.length)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			receipt, result, err := cstGenerationRun(t, ctx, python, request,
				[]string{filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
				map[string]string{"MCPHUB_CST_GENERATION_SYNTHETIC": "1"})
			var strictErr *hubprocess.StrictRunError
			if test.launch {
				if err == nil || (errors.As(err, &strictErr) && strictErr.Kind == hubprocess.StrictRunInvalidInvocation) || receipt.Status != "BLOCKED:field_generation_configuration_invalid" || len(result.Stdout.Prefix) == 0 {
					t.Fatalf("generation admission length=%d receipt=%+v result=%+v err=%v", test.length, receipt, result, err)
				}
				return
			}
			if !errors.As(err, &strictErr) || strictErr.Kind != hubprocess.StrictRunInvalidInvocation || len(result.Stdout.Prefix) != 0 {
				t.Fatalf("generation refusal length=%d receipt=%+v result=%+v err=%v", test.length, receipt, result, err)
			}
		})
	}

	metadata := map[string]string{
		"schema": "mcphub.cst.saved_field.fixture_generation.metadata.v1", "source_project": project,
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		length int
		pass   bool
	}{
		{name: "metadata-64kib", length: 64 << 10, pass: true},
		{name: "metadata-64kib-plus-one", length: (64 << 10) + 1, pass: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := append(append([]byte(nil), encoded...), []byte(strings.Repeat(" ", test.length-len(encoded)))...)
			cmd := exec.Command(python, cstGenerationHelperPath(t), "--metadata")
			cmd.Env = cstProbeEnvironment(python, []string{filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")}, nil)
			cmd.Dir = outputRoot
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := hubprocess.RunStrictlyContained(ctx, hubprocess.StrictRunInvocation{
				Command: cmd, Input: payload, InputLimit: 128 << 10, StdoutLimit: 1 << 20, StderrLimit: 64 << 10,
			})
			var response map[string]any
			if decodeErr := json.Unmarshal(result.Stdout.Prefix, &response); decodeErr != nil {
				t.Fatalf("decode metadata length=%d stdout=%q err=%v", test.length, result.Stdout.Prefix, decodeErr)
			}
			if test.pass {
				if err != nil || response["schema"] != "mcphub.cst.saved_field.fixture_generation.metadata.v1" {
					t.Fatalf("metadata admission length=%d response=%+v err=%v", test.length, response, err)
				}
				return
			}
			if err == nil || response["status"] != "BLOCKED:metadata_invalid" {
				t.Fatalf("metadata refusal length=%d response=%+v err=%v", test.length, response, err)
			}
		})
	}
}

func cstGenerationSourceManifest(t *testing.T, ctx context.Context, python, project string, pythonPaths []string, extra map[string]string) (cstProbeManifest, hubprocess.StrictRunResult, error) {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"schema": "mcphub.cst.saved_field.fixture_generation.metadata.v1", "source_project": project})
	if err != nil {
		return cstProbeManifest{}, hubprocess.StrictRunResult{}, &cstLegacyChildError{Stage: "metadata_request_encode", Cause: err}
	}
	cmd := exec.Command(python, cstGenerationHelperPath(t), "--metadata")
	cmd.Env = cstProbeEnvironment(python, pythonPaths, extra)
	cmd.Dir = filepath.Dir(project)
	result, runErr := hubprocess.RunStrictlyContained(ctx, hubprocess.StrictRunInvocation{Command: cmd, Input: payload, InputLimit: 64 << 10, StdoutLimit: 1 << 20, StderrLimit: 64 << 10})
	manifest, decodeErr := cstDecodeGenerationMetadata(result, runErr)
	return manifest, result, decodeErr
}

func TestCSTSavedFieldGenerationManifestAdmissionBeforeCSTImport(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	for index := 0; index < 6; index++ {
		cstProbeWriteFile(t, filepath.Join(strings.TrimSuffix(project, filepath.Ext(project)), "Result", "extra", fmt.Sprintf("%d.bin", index)), "extra")
	}
	repositoryRoot := cstProbeRepositoryRoot(t)
	sentinelRoot := filepath.Join(root, "sentinel")
	marker := filepath.Join(root, "cst-imported")
	cstProbeWriteFile(t, filepath.Join(sentinelRoot, "cst", "__init__.py"), "")
	cstProbeWriteFile(t, filepath.Join(sentinelRoot, "cst", "interface", "__init__.py"), "from pathlib import Path\nimport os\nPath(os.environ['CST_SENTINEL']).write_text('imported')\nraise RuntimeError('cst import sentinel')\n")
	paths := []string{sentinelRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	manifest5, _, metadataErr := cstGenerationSourceManifest(t, ctx, python, cstProbeSyntheticBundle(t, filepath.Join(root, "five")), paths, map[string]string{"CST_SENTINEL": marker})
	if metadataErr != nil {
		t.Fatal(metadataErr)
	}
	manifest11, _, metadataErr := cstGenerationSourceManifest(t, ctx, python, project, paths, map[string]string{"CST_SENTINEL": marker})
	if metadataErr != nil {
		t.Fatal(metadataErr)
	}
	if len(manifest5.Rows) != 5 || len(manifest11.Rows) != 11 || manifest11.AggregateSHA256 != cstGenerationFullManifest(t, project).AggregateSHA256 {
		t.Fatalf("metadata manifests five=%d eleven=%d", len(manifest5.Rows), len(manifest11.Rows))
	}
	for _, mutate := range []func(cstProbeManifest) cstProbeManifest{
		func(m cstProbeManifest) cstProbeManifest { m.Rows = m.Rows[:len(m.Rows)-1]; return m },
		func(m cstProbeManifest) cstProbeManifest { m.Rows = append(m.Rows, m.Rows[0]); return m },
		func(m cstProbeManifest) cstProbeManifest { m.Rows[0].SHA256 = strings.Repeat("0", 64); return m },
	} {
		output := filepath.Join(root, "output-"+fmt.Sprint(len(marker)))
		if err := os.MkdirAll(output, 0o700); err != nil {
			t.Fatal(err)
		}
		bad := mutate(manifest11)
		request := cstGenerationRequest{Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", SourceProject: project, OutputRoot: output,
			ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, cstGenerationHelperPath(t)), ExpectedPythonSHA: cstProbeFileSHA256(t, python), SourceManifest: bad, FullSourceManifest: &manifest11}
		receipt, _, err := cstGenerationRun(t, ctx, python, request, paths, map[string]string{"CST_SENTINEL": marker})
		if err == nil || receipt.Status != "BLOCKED:field_generation_configuration_invalid" || receipt.Stage != "manifest" {
			t.Fatalf("bad manifest receipt=%+v err=%v", receipt, err)
		}
		if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("cst imported before manifest rejection: %v", statErr)
		}
	}
}

func TestCSTSavedFieldGenerationMetadataPostcheckSupportsVariableRows(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	for index := 0; index < 6; index++ {
		cstProbeWriteFile(t, filepath.Join(strings.TrimSuffix(project, filepath.Ext(project)), "Result", "extra", fmt.Sprintf("%d.bin", index)), "extra")
	}
	repositoryRoot := cstProbeRepositoryRoot(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	before, _, metadataErr := cstGenerationSourceManifest(t, ctx, python, project,
		[]string{filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")}, nil)
	if metadataErr != nil {
		t.Fatal(metadataErr)
	}
	if len(before.Rows) != 11 {
		t.Fatalf("metadata source rows=%d want=11", len(before.Rows))
	}
	unchanged, _, metadataErr := cstGenerationSourceManifest(t, ctx, python, project,
		[]string{filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")}, nil)
	if metadataErr != nil {
		t.Fatal(metadataErr)
	}
	if unchanged.AggregateSHA256 != before.AggregateSHA256 {
		t.Fatalf("unchanged metadata aggregate before=%s after=%s", before.AggregateSHA256, unchanged.AggregateSHA256)
	}
	cstProbeWriteFile(t, filepath.Join(strings.TrimSuffix(project, filepath.Ext(project)), "Result", "extra", "0.bin"), "changed")
	changed, _, metadataErr := cstGenerationSourceManifest(t, ctx, python, project,
		[]string{filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")}, nil)
	if metadataErr != nil {
		t.Fatal(metadataErr)
	}
	if changed.AggregateSHA256 == before.AggregateSHA256 {
		t.Fatalf("metadata owner missed source mutation aggregate=%s", changed.AggregateSHA256)
	}
}

func cstProbeRequireNormalAPITrace(t *testing.T, trace *cstProbeAPITrace) {
	t.Helper()
	if trace == nil {
		t.Fatal("missing API trace")
	}
	wantEvents := []string{
		"environment-census",
		"environment-new",
		"project-open",
		"environment-census",
		"environment-census",
		"result-tree-first-child",
		"result-tree-first-child",
		"result-tree-next-item",
		"result-tree-first-child",
		"result-tree-next-item",
		"project-close",
		"environment-close",
		"environment-census",
	}
	if len(trace.Events) != len(wantEvents) {
		t.Fatalf("API events = %+v, want %v", trace.Events, wantEvents)
	}
	for index, want := range wantEvents {
		if trace.Events[index].Name != want || trace.Events[index].Outcome != "finish" {
			t.Fatalf("API event %d = %+v, want %s/finish", index, trace.Events[index], want)
		}
	}
	wantCounts := map[string]int{
		"environment-census":      4,
		"environment-new":         1,
		"project-open":            1,
		"result-tree-first-child": 3,
		"result-tree-next-item":   2,
		"project-close":           1,
		"environment-close":       1,
		"solve":                   0,
		"remesh":                  0,
		"history":                 0,
		"export":                  0,
		"project-save":            0,
		"3DSave":                  0,
	}
	if len(trace.Counts) != len(wantCounts) {
		t.Fatalf("API counters = %v, want %v", trace.Counts, wantCounts)
	}
	for name, want := range wantCounts {
		if trace.Counts[name] != want {
			t.Fatalf("API counter %s = %d, want %d; all=%v", name, trace.Counts[name], want, trace.Counts)
		}
	}
}

func cstProbeRequireAllowedAPITrace(t *testing.T, trace *cstProbeAPITrace) {
	t.Helper()
	if trace == nil {
		t.Fatal("missing API trace")
	}
	allowed := map[string]bool{
		"environment-census": true, "environment-new": true, "project-open": true,
		"result-tree-first-child": true, "result-tree-next-item": true,
		"project-close": true, "environment-close": true,
	}
	observed := make(map[string]int)
	for _, event := range trace.Events {
		if !allowed[event.Name] || event.Outcome != "finish" {
			t.Fatalf("unexpected API event: %+v", event)
		}
		observed[event.Name]++
	}
	for name, count := range observed {
		if trace.Counts[name] != count {
			t.Fatalf("API counter %s=%d, events=%d", name, trace.Counts[name], count)
		}
	}
	for _, required := range []string{"environment-census", "environment-new", "project-open", "result-tree-first-child", "project-close", "environment-close"} {
		if trace.Counts[required] == 0 {
			t.Fatalf("missing required API call %s: %v", required, trace.Counts)
		}
	}
	for _, forbidden := range []string{"solve", "remesh", "history", "export", "project-save", "3DSave"} {
		if trace.Counts[forbidden] != 0 {
			t.Fatalf("forbidden API counter %s=%d", forbidden, trace.Counts[forbidden])
		}
	}
}

func TestOwnedResultTreeProbeRejectsOpenRequest(t *testing.T) {
	python := cstProbeTestPython(t)
	cmd := exec.Command(python, cstProbeHelperPath(t))
	cmd.Env = cstProbeMinimalEnvironment(python)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := hubprocess.RunStrictlyContained(ctx, hubprocess.StrictRunInvocation{
		Command:     cmd,
		Input:       []byte("{}"),
		InputLimit:  64 << 10,
		StdoutLimit: 1 << 20,
		StderrLimit: 64 << 10,
	})
	if err != nil {
		t.Fatalf("closed-request helper execution: %v; stderr=%q", err, result.Stderr.Prefix)
	}
	var receipt cstProbeFailureReceipt
	if err := json.Unmarshal(result.Stdout.Prefix, &receipt); err != nil {
		t.Fatalf("decode closed-request receipt: %v; stdout=%q", err, result.Stdout.Prefix)
	}
	if receipt.Schema != "mcphub.cst.saved_field.result_tree_probe.receipt.v1" ||
		receipt.Status != "BLOCKED:probe_configuration_invalid" ||
		receipt.FailureID != "probe_configuration_invalid" {
		t.Fatalf("closed-request receipt = %+v", receipt)
	}
}

func TestOwnedResultTreeProbeSyntheticLifecycle(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	workspaceRoot := filepath.Join(root, "owned")
	if err := os.Mkdir(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeRoot, tracePath := cstProbeSyntheticInterface(t, root)
	repositoryRoot := cstProbeRepositoryRoot(t)
	manifest := cstProbeManifestForProject(t, project)
	request := cstProbeRequest{
		Schema:             cstProbeRequestSchema,
		SourceProject:      project,
		WorkspaceRoot:      workspaceRoot,
		ExpectedProjectSHA: cstProbeFileSHA256(t, project),
		ExpectedHelperSHA:  cstProbeFileSHA256(t, cstProbeHelperPath(t)),
		ExpectedPythonSHA:  cstProbeFileSHA256(t, python),
		SourceManifest:     manifest,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, result, err := cstProbeRun(
		t,
		ctx,
		python,
		request,
		[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
		map[string]string{
			"MCPHUB_CST_PROBE_SYNTHETIC": "1",
			"MCPHUB_CST_PROBE_TRACE":     tracePath,
		},
	)
	if err != nil {
		t.Fatalf("synthetic contained run: %v; stderr=%q", err, result.Stderr.Prefix)
	}
	wantSelectors := []string{
		`2D/3D Results\E-Field\e1`,
		`2D/3D Results\H-Field\h1`,
	}
	if receipt.Schema != cstProbeReceiptSchema || receipt.Status != "PASS:owned_result_tree_listed" ||
		receipt.FailureID != "" || receipt.SourceManifestSHA256 != manifest.AggregateSHA256 ||
		receipt.DestinationManifestSHA256 != manifest.AggregateSHA256 || !cstProbeIsDigest(receipt.SessionIdentitySHA256) ||
		receipt.Root != "2D/3D Results" || fmt.Sprint(receipt.Selectors) != fmt.Sprint(wantSelectors) ||
		receipt.TraversalCount != len(wantSelectors) || receipt.MaxDepth != 64 || receipt.MaxNodes != 4096 ||
		receipt.MaxSelectorBytes != 1<<20 || !receipt.WorkspaceSettled || !receipt.SessionSettled ||
		!receipt.SourceUnchanged || !receipt.CacheCleared || !receipt.ClosedWithoutSave ||
		!receipt.ForeignProcessesPreserved {
		t.Fatalf("synthetic receipt = %+v", receipt)
	}
	if receipt.Preservation == nil || len(receipt.Preservation.Rows) != 0 || len(receipt.Preservation.ForeignIdentityDelta.Added) != 0 || len(receipt.Preservation.ForeignIdentityDelta.Missing) != 0 {
		t.Fatalf("normal preservation audit = %+v", receipt.Preservation)
	}
	cstProbeRequireNormalAPITrace(t, receipt.APITrace)
	if strings.Contains(string(result.Stdout.Prefix), `"owned_process_identity"`) {
		t.Fatalf("raw owned identity leaked: %s", result.Stdout.Prefix)
	}
	if got := cstProbeManifestForProject(t, project); got.AggregateSHA256 != manifest.AggregateSHA256 {
		t.Fatalf("source manifest changed: before=%s after=%s", manifest.AggregateSHA256, got.AggregateSHA256)
	}
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("workspace not settled: entries=%v err=%v", entries, err)
	}
	traceBytes, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	trace := string(traceBytes)
	for _, required := range []string{
		`"name": "DesignEnvironment.new"`,
		`"name": "open_project"`,
		`"name": "GetFirstChildName"`,
		`"name": "GetNextItemName"`,
		`"name": "project.close"`,
		`"name": "DesignEnvironment.close"`,
	} {
		if !strings.Contains(trace, required) {
			t.Fatalf("missing trace event %s in %s", required, trace)
		}
	}
	for _, forbidden := range []string{
		"SelectTreeItem", "GetTreeResults", "GetFieldVector", "save", "solver", "remesh",
	} {
		if strings.Contains(trace, forbidden) {
			t.Fatalf("forbidden trace event %q in %s", forbidden, trace)
		}
	}
}

func TestCSTSavedFieldOwnedResultTreeProbeTimeout(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	workspaceRoot := filepath.Join(root, "owned")
	if err := os.Mkdir(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeRoot, tracePath := cstProbeSyntheticInterface(t, root)
	repositoryRoot := cstProbeRepositoryRoot(t)
	manifest := cstProbeManifestForProject(t, project)
	request := cstProbeRequest{
		Schema:             cstProbeRequestSchema,
		SourceProject:      project,
		WorkspaceRoot:      workspaceRoot,
		ExpectedProjectSHA: cstProbeFileSHA256(t, project),
		ExpectedHelperSHA:  cstProbeFileSHA256(t, cstProbeHelperPath(t)),
		ExpectedPythonSHA:  cstProbeFileSHA256(t, python),
		SourceManifest:     manifest,
		TestStallAfterOpen: 5_000,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, result, err := cstProbeRun(
		t,
		ctx,
		python,
		request,
		[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
		map[string]string{
			"MCPHUB_CST_PROBE_SYNTHETIC": "1",
			"MCPHUB_CST_PROBE_TRACE":     tracePath,
		},
	)
	var strictErr *hubprocess.StrictRunError
	if !errors.As(err, &strictErr) || strictErr.Kind != hubprocess.StrictRunTimeout {
		t.Fatalf("timeout run error=%v receipt=%q stderr=%q", err, result.Stdout.Prefix, result.Stderr.Prefix)
	}
	if got := cstProbeManifestForProject(t, project); got.AggregateSHA256 != manifest.AggregateSHA256 {
		t.Fatalf("timeout changed source: before=%s after=%s", manifest.AggregateSHA256, got.AggregateSHA256)
	}
	if err := os.RemoveAll(workspaceRoot); err != nil {
		t.Fatalf("remove exact owned workspace after timeout: %v", err)
	}
	if _, err := os.Stat(workspaceRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned workspace remains after timeout cleanup: %v", err)
	}
}

func cstProbeIsDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func TestOwnedResultTreeProbePreservationEvidence(t *testing.T) {
	python := cstProbeTestPython(t)
	cases := []struct {
		name           string
		mutation       string
		foreignDelta   bool
		blocked        bool
		wantSide       string
		wantLabel      string
		wantMembership string
	}{
		{name: "source-change", mutation: "source-change", blocked: true, wantSide: "source", wantLabel: "storage-sdb", wantMembership: "changed"},
		{name: "copy-change", mutation: "copy-change", wantSide: "copy", wantLabel: "storage-sdb", wantMembership: "changed"},
		{name: "copy-mtime-only", mutation: "copy-mtime-only", wantSide: "copy", wantLabel: "storage-sdb", wantMembership: "changed"},
		{name: "copy-missing", mutation: "copy-missing", wantSide: "copy", wantLabel: "storage-sdb", wantMembership: "missing"},
		{name: "copy-extra", mutation: "copy-extra", wantSide: "copy", wantLabel: "extra:", wantMembership: "extra"},
		{name: "foreign-census", foreignDelta: true, blocked: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root := t.TempDir()
			project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
			workspaceRoot := filepath.Join(root, "owned")
			if err := os.Mkdir(workspaceRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			fakeRoot, tracePath := cstProbeSyntheticInterface(t, root)
			repositoryRoot := cstProbeRepositoryRoot(t)
			manifest := cstProbeManifestForProject(t, project)
			request := cstProbeRequest{
				Schema:             cstProbeRequestSchema,
				SourceProject:      project,
				WorkspaceRoot:      workspaceRoot,
				ExpectedProjectSHA: cstProbeFileSHA256(t, project),
				ExpectedHelperSHA:  cstProbeFileSHA256(t, cstProbeHelperPath(t)),
				ExpectedPythonSHA:  cstProbeFileSHA256(t, python),
				SourceManifest:     manifest,
				TestMutation:       testCase.mutation,
			}
			extra := map[string]string{
				"MCPHUB_CST_PROBE_SYNTHETIC": "1",
				"MCPHUB_CST_PROBE_TRACE":     tracePath,
			}
			if testCase.foreignDelta {
				extra["MCPHUB_CST_PROBE_FAKE_FOREIGN_DELTA"] = "1"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			receipt, result, err := cstProbeRun(
				t,
				ctx,
				python,
				request,
				[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
				extra,
			)
			if err != nil {
				t.Fatalf("preservation run: %v; stderr=%q", err, result.Stderr.Prefix)
			}
			wantStatus := "PASS:owned_result_tree_listed"
			wantFailure := ""
			if testCase.blocked {
				wantStatus = "BLOCKED:probe_preservation_failed"
				wantFailure = "probe_preservation_failed"
			}
			if receipt.Status != wantStatus || receipt.FailureID != wantFailure || receipt.Preservation == nil || !cstProbeIsDigest(receipt.SessionIdentitySHA256) {
				t.Fatalf("preservation receipt = %+v", receipt)
			}
			cstProbeRequireNormalAPITrace(t, receipt.APITrace)
			raw := string(result.Stdout.Prefix)
			for _, forbidden := range []string{root, project, "SECRET_PRIVATE_SENTINEL", `"owned_process_identity"`} {
				if strings.Contains(raw, forbidden) {
					t.Fatalf("unsafe preservation receipt contains %q: %s", forbidden, raw)
				}
			}
			if testCase.blocked && strings.Contains(raw, `"selectors"`) {
				t.Fatalf("blocked receipt exposed selectors: %s", raw)
			}
			entries, readErr := os.ReadDir(workspaceRoot)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("preservation workspace not cleaned: entries=%v err=%v", entries, readErr)
			}
			if testCase.foreignDelta {
				delta := receipt.Preservation.ForeignIdentityDelta
				if len(delta.Added) != 1 || len(delta.Missing) != 1 || !cstProbeIsDigest(delta.Added[0]) || !cstProbeIsDigest(delta.Missing[0]) {
					t.Fatalf("foreign identity delta = %+v", delta)
				}
				return
			}
			var found *cstProbeRowDifference
			for index := range receipt.Preservation.Rows {
				candidate := &receipt.Preservation.Rows[index]
				labelMatches := candidate.Label == testCase.wantLabel || strings.HasPrefix(candidate.Label, testCase.wantLabel)
				if candidate.Side == testCase.wantSide && labelMatches && candidate.Membership == testCase.wantMembership {
					found = candidate
					break
				}
			}
			if found == nil {
				t.Fatalf("missing expected row evidence in %+v", receipt.Preservation.Rows)
			}
			switch testCase.wantMembership {
			case "missing":
				if found.Before == nil || found.After != nil {
					t.Fatalf("missing evidence = %+v", found)
				}
			case "extra":
				if found.Before != nil || found.After == nil {
					t.Fatalf("extra evidence = %+v", found)
				}
			case "changed":
				if found.Before == nil || found.After == nil {
					t.Fatalf("changed evidence = %+v", found)
				}
				if testCase.mutation == "copy-mtime-only" {
					if found.Before.SHA256 != found.After.SHA256 || found.Before.Size != found.After.Size || found.Before.ModifiedNS == found.After.ModifiedNS {
						t.Fatalf("mtime-only evidence = %+v", found)
					}
				} else if found.Before.SHA256 == found.After.SHA256 {
					t.Fatalf("content-change evidence = %+v", found)
				}
			}
		})
	}
}

func TestOwnedResultTreeProbeForbiddenCallFailsClosed(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	workspaceRoot := filepath.Join(root, "owned")
	if err := os.Mkdir(workspaceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeRoot, tracePath := cstProbeSyntheticInterface(t, root)
	repositoryRoot := cstProbeRepositoryRoot(t)
	manifest := cstProbeManifestForProject(t, project)
	request := cstProbeRequest{
		Schema:             cstProbeRequestSchema,
		SourceProject:      project,
		WorkspaceRoot:      workspaceRoot,
		ExpectedProjectSHA: cstProbeFileSHA256(t, project),
		ExpectedHelperSHA:  cstProbeFileSHA256(t, cstProbeHelperPath(t)),
		ExpectedPythonSHA:  cstProbeFileSHA256(t, python),
		SourceManifest:     manifest,
		TestMutation:       "forbidden-call",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, result, err := cstProbeRun(
		t,
		ctx,
		python,
		request,
		[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
		map[string]string{"MCPHUB_CST_PROBE_SYNTHETIC": "1", "MCPHUB_CST_PROBE_TRACE": tracePath},
	)
	if err != nil {
		t.Fatalf("forbidden-call run: %v; stderr=%q", err, result.Stderr.Prefix)
	}
	if receipt.Status != "BLOCKED:unexpected_api_call" || receipt.FailureID != "unexpected_api_call" ||
		receipt.APITrace == nil || receipt.Preservation == nil || !cstProbeIsDigest(receipt.SessionIdentitySHA256) ||
		!receipt.WorkspaceSettled || !receipt.SessionSettled || !receipt.ClosedWithoutSave {
		t.Fatalf("forbidden-call receipt = %+v", receipt)
	}
	events := receipt.APITrace.Events
	forbiddenIndex, closeIndex := -1, -1
	for index, event := range events {
		if event == (cstProbeAPIEvent{Name: "project-save", Outcome: "error"}) {
			forbiddenIndex = index
		}
		if event.Name == "project-close" {
			closeIndex = index
		}
	}
	if forbiddenIndex < 0 || closeIndex <= forbiddenIndex || receipt.APITrace.Counts["project-save"] != 1 {
		t.Fatalf("forbidden-call trace = %+v", receipt.APITrace)
	}
	for _, name := range []string{"solve", "remesh", "history", "export", "3DSave"} {
		if receipt.APITrace.Counts[name] != 0 {
			t.Fatalf("forbidden counter %s = %d", name, receipt.APITrace.Counts[name])
		}
	}
	raw := string(result.Stdout.Prefix)
	for _, forbidden := range []string{root, project, "SECRET_PRIVATE_SENTINEL", `"selectors"`, `"owned_process_identity"`} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("unsafe forbidden-call receipt contains %q: %s", forbidden, raw)
		}
	}
	entries, readErr := os.ReadDir(workspaceRoot)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("forbidden-call workspace not cleaned: entries=%v err=%v", entries, readErr)
	}
}

func TestCSTSavedFieldGenerateLine10FixtureCommandAllowlist(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	outputRoot := filepath.Join(root, "output")
	if err := os.Mkdir(outputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeRoot, tracePath := cstGenerationSyntheticInterface(t, root)
	repositoryRoot := cstProbeRepositoryRoot(t)
	manifest := cstProbeManifestForProject(t, project)
	request := cstGenerationRequest{
		Schema:        "mcphub.cst.saved_field.fixture_generation.request.v1",
		SourceProject: project, OutputRoot: outputRoot,
		ExpectedProjectSHA: cstProbeFileSHA256(t, project),
		ExpectedHelperSHA:  cstProbeFileSHA256(t, cstGenerationHelperPath(t)),
		ExpectedPythonSHA:  cstProbeFileSHA256(t, python), SourceManifest: manifest,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, result, err := cstGenerationRun(t, ctx, python, request,
		[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
		map[string]string{"MCPHUB_CST_GENERATION_SYNTHETIC": "1", "MCPHUB_CST_GENERATION_TRACE": tracePath})
	if err != nil {
		t.Fatalf("generation synthetic run: %v; stderr=%q", err, result.Stderr.Prefix)
	}
	if receipt.Status != "PASS:copied_line10_fields_generated" || len(receipt.Results) != 4 ||
		!receipt.SourceUnchanged || !receipt.ForeignPreserved || !receipt.SessionSettled ||
		!cstProbeIsDigest(receipt.SessionIdentitySHA256) || !cstProbeIsDigest(receipt.RetainedManifestSHA256) {
		t.Fatalf("generation receipt = %+v", receipt)
	}
	if receipt.APITrace == nil || receipt.APITrace.Counts["run-solver"] != 1 || receipt.APITrace.Counts["project-save"] != 1 ||
		receipt.APITrace.Counts["add-history"] != 1 {
		t.Fatalf("generation trace = %+v", receipt.APITrace)
	}
	for _, forbidden := range []string{"remote-worker", "export", "remesh", "second-start", "source-save"} {
		if receipt.APITrace.Counts[forbidden] != 0 {
			t.Fatalf("forbidden generation counter %s=%d", forbidden, receipt.APITrace.Counts[forbidden])
		}
	}
	if got := cstProbeManifestForProject(t, project); got.AggregateSHA256 != manifest.AggregateSHA256 {
		t.Fatalf("generation changed source: before=%s after=%s", manifest.AggregateSHA256, got.AggregateSHA256)
	}
	if _, err := os.Stat(filepath.Join(outputRoot, receipt.RetainedBundle)); err != nil {
		t.Fatalf("retained bundle absent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputRoot, "staging")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging remains: %v", err)
	}
	trace, err := os.ReadFile(tracePath)
	if err != nil || !strings.Contains(string(trace), "Solver.FrequencyRange 1.0, 20.0") || !strings.Contains(string(trace), "AddSampleInterval 3.0, 3.0, 1") {
		t.Fatalf("history wire missing fixture range or single 3 GHz representation: err=%v trace=%q", err, trace)
	}
}

func cstGenerationFullManifest(t *testing.T, project string) cstProbeManifest {
	t.Helper()
	manifest, err := cstBindingFullManifest(project)
	if err != nil {
		t.Fatalf("inventory full bundle: %v", err)
	}
	return manifest
}

func cstBindingFullManifest(project string) (cstProbeManifest, error) {
	return cstBindingInventoryManifest(project, strings.TrimSuffix(project, filepath.Ext(project)), true)
}

func cstBindingFullManifestWithBudget(project string, rowBudget, aggregateBudget int64) (cstProbeManifest, error) {
	return cstBindingInventoryManifestWithBudget(project, strings.TrimSuffix(project, filepath.Ext(project)), true, rowBudget, aggregateBudget)
}

func TestCSTSavedFieldFingerprintInventoryHardlinkBeforeFallback(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "synthetic-outside.bin")
	if err := os.WriteFile(outside, []byte("synthetic sentinel only"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(root, "copy")
	project := filepath.Join(parent, "probe.cst")
	cstProbeWriteFile(t, project, "project")
	linked := filepath.Join(parent, "probe", "Model", "linked.bin")
	if err := os.MkdirAll(filepath.Dir(linked), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, linked); err != nil {
		t.Fatal(err)
	}
	_, err := cstBindingFullManifest(project)
	var unsafeRow *cstFingerprintError
	if !errors.As(err, &unsafeRow) || unsafeRow.Kind != "multi_link" || unsafeRow.Relative != "probe/Model/linked.bin" || unsafeRow.DataReadEntered {
		t.Fatalf("unsafe row lacked typed pre-read witness: %v", err)
	}
}

type cstFingerprintError struct {
	Kind, Relative  string
	DataReadEntered bool
	Cause           error
}

func (e *cstFingerprintError) Error() string { return e.Kind + ": " + fmt.Sprint(e.Cause) }
func (e *cstFingerprintError) Unwrap() error { return e.Cause }

type cstFingerprintHooks struct {
	BeforeLeafOpen func() error
	BeforeRead     func() error
	CaptureLimit   int64
	Capture        *[]byte
}

func cstFingerprintOpenRoot(root string) (windows.Handle, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return 0, errors.New("root must be exact absolute path")
	}
	native := `\??\` + root
	if strings.HasPrefix(root, `\\`) {
		native = `\??\UNC\` + strings.TrimPrefix(root, `\\`)
	}
	name, err := windows.NewNTUnicodeString(native)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	var iosb windows.IO_STATUS_BLOCK
	var allocation int64
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, windows.FILE_TRAVERSE|windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		oa, &iosb, &allocation, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	return handle, err
}

func cstFingerprintOpenRelative(parent windows.Handle, component string, directory bool) (windows.Handle, error) {
	name, err := windows.NewNTUnicodeString(component)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	access := uint32(windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	options := uint32(windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_NON_DIRECTORY_FILE)
	if directory {
		access = windows.FILE_TRAVERSE | windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
		options = windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_DIRECTORY_FILE
	}
	var iosb windows.IO_STATUS_BLOCK
	var allocation int64
	var handle windows.Handle
	err = windows.NtCreateFile(&handle, access, oa, &iosb, &allocation, windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, options, 0, 0)
	return handle, err
}

func cstFingerprintHandleInfo(handle windows.Handle, directory bool) (windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return info, &cstFingerprintError{Kind: "metadata", Cause: err}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return info, &cstFingerprintError{Kind: "reparse"}
	}
	if (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return info, &cstFingerprintError{Kind: "invalid_kind"}
	}
	if !directory && info.NumberOfLinks != 1 {
		return info, &cstFingerprintError{Kind: "multi_link"}
	}
	return info, nil
}

func cstFingerprintFile(ctx context.Context, root, relative string, rowLimit int64, hooks *cstFingerprintHooks) (row cstProbeManifestRow, retErr error) {
	bad := func(kind string, cause error) (cstProbeManifestRow, error) {
		return cstProbeManifestRow{}, &cstFingerprintError{Kind: kind, Cause: cause}
	}
	if ctx == nil || rowLimit < 0 || !filepath.IsAbs(root) || relative == "" || filepath.IsAbs(filepath.FromSlash(relative)) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative))) != relative {
		return bad("invalid_input", nil)
	}
	parts := strings.Split(relative, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `\/:`) || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return bad("invalid_input", nil)
		}
	}
	if err := ctx.Err(); err != nil {
		return bad("canceled", err)
	}
	rootHandle, err := cstFingerprintOpenRoot(root)
	if err != nil {
		return bad("unsafe_object", err)
	}
	handles := []windows.Handle{rootHandle}
	defer func() {
		for i := len(handles) - 1; i >= 0; i-- {
			if closeErr := windows.CloseHandle(handles[i]); closeErr != nil {
				if retErr == nil {
					row = cstProbeManifestRow{}
					retErr = &cstFingerprintError{Kind: "close_failed", Cause: closeErr}
				} else {
					retErr = errors.Join(retErr, closeErr)
				}
			}
		}
	}()
	if _, err := cstFingerprintHandleInfo(rootHandle, true); err != nil {
		return bad("unsafe_object", err)
	}
	parent := rootHandle
	for _, component := range parts[:len(parts)-1] {
		if err := ctx.Err(); err != nil {
			return bad("canceled", err)
		}
		next, err := cstFingerprintOpenRelative(parent, component, true)
		if err != nil {
			return bad("unsafe_object", err)
		}
		handles = append(handles, next)
		if _, err := cstFingerprintHandleInfo(next, true); err != nil {
			return bad("unsafe_object", err)
		}
		parent = next
	}
	return cstFingerprintLeafOnParent(ctx, parent, parts[len(parts)-1], relative, rowLimit, hooks)
}

func cstFingerprintCaptureFile(ctx context.Context, root, relative string, rowLimit, captureLimit int64, hooks *cstFingerprintHooks) (cstProbeManifestRow, []byte, error) {
	if captureLimit <= 0 || captureLimit > cstFingerprintSelectedRexCaptureLimit {
		return cstProbeManifestRow{}, nil, &cstFingerprintError{Kind: "size_limit", Relative: relative}
	}
	var captured []byte
	ownedHooks := &cstFingerprintHooks{CaptureLimit: captureLimit, Capture: &captured}
	if hooks != nil {
		ownedHooks.BeforeLeafOpen, ownedHooks.BeforeRead = hooks.BeforeLeafOpen, hooks.BeforeRead
	}
	row, err := cstFingerprintFile(ctx, root, relative, rowLimit, ownedHooks)
	if err != nil {
		return cstProbeManifestRow{}, nil, err
	}
	return row, captured, nil
}

func TestCSTSavedFieldFingerprintSelectedRexCapture(t *testing.T) {
	root := t.TempDir()
	relative := "probe/Result/frame.rex"
	path := filepath.Join(root, filepath.FromSlash(relative))
	cstProbeWriteFile(t, path, "exact safe rex")
	row, raw, err := cstFingerprintCaptureFile(context.Background(), root, relative, 16<<20, 16<<20, nil)
	if err != nil || string(raw) != "exact safe rex" || row.SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw)) {
		t.Fatalf("same-handle positive capture: row=%+v bytes=%d err=%v", row, len(raw), err)
	}
	t.Run("cap_and_cap_plus_one", func(t *testing.T) {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(f.Truncate(16<<20), f.Close()); err != nil {
			t.Fatal(err)
		}
		_, exact, err := cstFingerprintCaptureFile(context.Background(), root, relative, 16<<20, 16<<20, nil)
		if err != nil || len(exact) != 16<<20 {
			t.Fatalf("exact cap rejected: bytes=%d err=%v", len(exact), err)
		}
		if err := os.Truncate(path, (16<<20)+1); err != nil {
			t.Fatal(err)
		}
		entered := 0
		_, oversize, err := cstFingerprintCaptureFile(context.Background(), root, relative, (16<<20)+1, 16<<20, &cstFingerprintHooks{BeforeRead: func() error { entered++; return nil }})
		var typed *cstFingerprintError
		if !errors.As(err, &typed) || typed.Kind != "size_limit" || typed.DataReadEntered || len(oversize) != 0 || entered != 0 {
			t.Fatalf("cap+1 admitted: bytes=%d entered=%d err=%v", len(oversize), entered, err)
		}
	})
	t.Run("growth_and_swap", func(t *testing.T) {
		cstProbeWriteFile(t, path, "stable")
		_, grown, err := cstFingerprintCaptureFile(context.Background(), root, relative, 64, 64, &cstFingerprintHooks{BeforeRead: func() error {
			f, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if openErr != nil {
				return openErr
			}
			_, writeErr := f.Write([]byte("growth"))
			return errors.Join(writeErr, f.Close())
		}})
		var typed *cstFingerprintError
		if !errors.As(err, &typed) || typed.Kind != "changed_during_read" || len(grown) != 0 {
			t.Fatalf("growth returned bytes: %d %v", len(grown), err)
		}
		outside := filepath.Join(root, "synthetic-outside.bin")
		if err := os.WriteFile(outside, []byte("outside synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
		reads := 0
		_, swapped, err := cstFingerprintCaptureFile(context.Background(), root, relative, 64, 64, &cstFingerprintHooks{
			BeforeLeafOpen: func() error {
				if err := os.Rename(path, path+".moved"); err != nil {
					return err
				}
				return os.Link(outside, path)
			},
			BeforeRead: func() error { reads++; return nil },
		})
		if !errors.As(err, &typed) || typed.Kind != "multi_link" || len(swapped) != 0 || reads != 0 {
			t.Fatalf("swap read outside: bytes=%d reads=%d err=%v", len(swapped), reads, err)
		}
	})
}

func cstFingerprintLeafOnParent(ctx context.Context, parent windows.Handle, name, relative string, rowLimit int64, hooks *cstFingerprintHooks) (row cstProbeManifestRow, retErr error) {
	bad := func(kind string, readEntered bool, cause error) (cstProbeManifestRow, error) {
		return cstProbeManifestRow{}, &cstFingerprintError{Kind: kind, Relative: relative, DataReadEntered: readEntered, Cause: cause}
	}
	if hooks != nil && hooks.BeforeLeafOpen != nil {
		if err := hooks.BeforeLeafOpen(); err != nil {
			return bad("step", false, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return bad("canceled", false, err)
	}
	leaf, err := cstFingerprintOpenRelative(parent, name, false)
	if err != nil {
		return bad("leaf_open", false, err)
	}
	defer func() {
		if closeErr := windows.CloseHandle(leaf); closeErr != nil {
			if retErr == nil {
				row = cstProbeManifestRow{}
				retErr = &cstFingerprintError{Kind: "close", Relative: relative, DataReadEntered: true, Cause: closeErr}
			} else {
				retErr = errors.Join(retErr, &cstFingerprintError{Kind: "close", Relative: relative, DataReadEntered: false, Cause: closeErr})
			}
		}
	}()
	before, err := cstFingerprintHandleInfo(leaf, false)
	if err != nil {
		var typed *cstFingerprintError
		if errors.As(err, &typed) {
			return bad(typed.Kind, false, typed.Cause)
		}
		return bad("metadata", false, err)
	}
	size := int64(before.FileSizeHigh)<<32 | int64(before.FileSizeLow)
	if size < 0 || size > rowLimit {
		return bad("size_limit", false, nil)
	}
	if hooks != nil && hooks.Capture != nil {
		if hooks.CaptureLimit <= 0 || size > hooks.CaptureLimit {
			return bad("size_limit", false, nil)
		}
		*hooks.Capture = make([]byte, 0, int(size))
	}
	if hooks != nil && hooks.BeforeRead != nil {
		if err := hooks.BeforeRead(); err != nil {
			return bad("step", false, err)
		}
	}
	hash := sha256.New()
	chunk := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return bad("canceled", true, err)
		}
		var read uint32
		readErr := windows.ReadFile(leaf, chunk, &read, nil)
		if read > 0 {
			total += int64(read)
			if total > size {
				return bad("changed_during_read", true, nil)
			}
			if total > rowLimit {
				return bad("size_limit", true, nil)
			}
			_, _ = hash.Write(chunk[:read])
			if hooks != nil && hooks.Capture != nil {
				*hooks.Capture = append(*hooks.Capture, chunk[:read]...)
			}
		}
		if readErr == windows.ERROR_HANDLE_EOF || (readErr == nil && read == 0) {
			break
		}
		if readErr != nil {
			return bad("read", true, readErr)
		}
	}
	after, err := cstFingerprintHandleInfo(leaf, false)
	if err != nil {
		return bad("metadata", true, err)
	}
	if total != size || before.VolumeSerialNumber != after.VolumeSerialNumber || before.FileIndexHigh != after.FileIndexHigh || before.FileIndexLow != after.FileIndexLow || before.FileSizeHigh != after.FileSizeHigh || before.FileSizeLow != after.FileSizeLow || before.LastWriteTime != after.LastWriteTime {
		return bad("changed_during_read", true, nil)
	}
	return cstProbeManifestRow{Path: relative, Type: "regular", Stream: "::$DATA", Size: total, SHA256: fmt.Sprintf("%x", hash.Sum(nil)), ModifiedNS: before.LastWriteTime.Nanoseconds()}, nil
}

func TestCSTSavedFieldFingerprintBudgetGrowthAndSwap(t *testing.T) {
	root := t.TempDir()
	relative := "probe/Model/data.bin"
	path := filepath.Join(root, filepath.FromSlash(relative))
	cstProbeWriteFile(t, path, strings.Repeat("a", 128))
	checkKind := func(t *testing.T, err error, want string) {
		t.Helper()
		var typed *cstFingerprintError
		if !errors.As(err, &typed) || typed.Kind != want {
			t.Fatalf("fingerprint error=%v, want %s", err, want)
		}
	}
	t.Run("oversized_row", func(t *testing.T) {
		_, err := cstFingerprintFile(context.Background(), root, relative, 64, nil)
		checkKind(t, err, "size_limit")
	})
	t.Run("growing_row", func(t *testing.T) {
		readCalls := 0
		_, err := cstFingerprintFile(context.Background(), root, relative, 256, &cstFingerprintHooks{BeforeRead: func() error {
			readCalls++
			f, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if openErr != nil {
				return openErr
			}
			_, writeErr := f.Write([]byte("growth"))
			return errors.Join(writeErr, f.Close())
		}})
		if readCalls != 1 {
			t.Fatalf("growth hook calls=%d", readCalls)
		}
		checkKind(t, err, "changed_during_read")
	})
	t.Run("path_swap_window", func(t *testing.T) {
		outside := filepath.Join(root, "synthetic-outside.bin")
		if err := os.WriteFile(outside, []byte("outside synthetic only"), 0o600); err != nil {
			t.Fatal(err)
		}
		original := filepath.Join(root, "probe", "Model", "original.bin")
		cstProbeWriteFile(t, original, "bound original")
		byteReads := 0
		_, err := cstFingerprintFile(context.Background(), root, "probe/Model/original.bin", 256, &cstFingerprintHooks{
			BeforeLeafOpen: func() error {
				if err := os.Rename(original, original+".moved"); err != nil {
					return err
				}
				return os.Link(outside, original)
			},
			BeforeRead: func() error { byteReads++; return nil },
		})
		checkKind(t, err, "multi_link")
		if byteReads != 0 {
			t.Fatalf("rejected outside identity was read %d times", byteReads)
		}
	})
}

func cstBindingResultInventory(project string) (cstProbeManifest, error) {
	return cstBindingInventoryManifest(project, filepath.Join(strings.TrimSuffix(project, filepath.Ext(project)), "Result"), true)
}

func cstBindingResultInventoryWithBudget(project string, rowBudget, aggregateBudget int64) (cstProbeManifest, error) {
	return cstBindingInventoryManifestWithBudget(project, filepath.Join(strings.TrimSuffix(project, filepath.Ext(project)), "Result"), true, rowBudget, aggregateBudget)
}

const (
	cstFingerprintInitialRowBudget        = int64(2 << 30)
	cstFingerprintInitialAggregateBudget  = int64(8 << 30)
	cstFingerprintCopyGrowthBudget        = int64(2 << 30)
	cstFingerprintSelectedRexCaptureLimit = int64(16 << 20)
	cstFingerprintMaximumRows             = 16384
)

type cstFingerprintScan struct {
	rowBudget, remaining int64
	rows                 []cstProbeManifestRow
	entries              int
}

func (scan *cstFingerprintScan) add(ctx context.Context, parent windows.Handle, name, relative string) error {
	if len(scan.rows) >= cstFingerprintMaximumRows || scan.remaining < 0 {
		return &cstFingerprintError{Kind: "size_limit"}
	}
	limit := min(scan.rowBudget, scan.remaining)
	row, err := cstFingerprintLeafOnParent(ctx, parent, name, relative, limit, nil)
	if err != nil {
		return err
	}
	scan.remaining -= row.Size
	scan.rows = append(scan.rows, row)
	return nil
}

// The directory File enumerates the same retained no-follow handle used as
// RootDirectory for child opens. No discovered pathname is reopened for data.
func cstFingerprintWalk(ctx context.Context, directory windows.Handle, relative string, scan *cstFingerprintScan) (retErr error) {
	file := os.NewFile(uintptr(directory), relative)
	defer func() {
		if err := file.Close(); err != nil {
			retErr = errors.Join(retErr, &cstFingerprintError{Kind: "close_failed", Cause: err})
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return &cstFingerprintError{Kind: "canceled", Cause: err}
		}
		entries, err := file.ReadDir(256)
		if err != nil && err != io.EOF {
			return &cstFingerprintError{Kind: "io_error", Cause: err}
		}
		for _, entry := range entries {
			scan.entries++
			if scan.entries > cstFingerprintMaximumRows*2 {
				return &cstFingerprintError{Kind: "size_limit", Cause: errors.New("too many entries")}
			}
			name := entry.Name()
			if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `\/:`) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
				return &cstFingerprintError{Kind: "unsafe_object", Cause: errors.New("invalid entry name")}
			}
			childRelative := relative + "/" + name
			if entry.IsDir() {
				child, openErr := cstFingerprintOpenRelative(directory, name, true)
				if openErr != nil {
					return &cstFingerprintError{Kind: "unsafe_object", Cause: openErr}
				}
				if _, infoErr := cstFingerprintHandleInfo(child, true); infoErr != nil {
					return &cstFingerprintError{Kind: "unsafe_object", Cause: errors.Join(infoErr, windows.CloseHandle(child))}
				}
				if walkErr := cstFingerprintWalk(ctx, child, childRelative, scan); walkErr != nil {
					return walkErr
				}
			} else if addErr := scan.add(ctx, directory, name, childRelative); addErr != nil {
				return addErr
			}
		}
		if err == io.EOF {
			break
		}
	}
	return nil
}

func cstBindingInventoryManifestWithBudget(project, walkRoot string, includeProject bool, rowBudget, aggregateBudget int64) (manifest cstProbeManifest, retErr error) {
	if rowBudget <= 0 || aggregateBudget <= 0 {
		return manifest, &cstFingerprintError{Kind: "invalid_input"}
	}
	parent := filepath.Dir(project)
	root, err := cstFingerprintOpenRoot(parent)
	if err != nil {
		return manifest, &cstFingerprintError{Kind: "unsafe_object", Cause: err}
	}
	handles := []windows.Handle{root}
	defer func() {
		for i := len(handles) - 1; i >= 0; i-- {
			if closeErr := windows.CloseHandle(handles[i]); closeErr != nil {
				retErr = errors.Join(retErr, &cstFingerprintError{Kind: "close_failed", Cause: closeErr})
				manifest = cstProbeManifest{}
			}
		}
	}()
	if _, err := cstFingerprintHandleInfo(root, true); err != nil {
		return manifest, &cstFingerprintError{Kind: "unsafe_object", Cause: err}
	}
	ctx := context.Background()
	scan := &cstFingerprintScan{rowBudget: rowBudget, remaining: aggregateBudget}
	if includeProject {
		if err := scan.add(ctx, root, filepath.Base(project), filepath.Base(project)); err != nil {
			return manifest, err
		}
	}
	walkRelative, err := filepath.Rel(parent, walkRoot)
	if err != nil || walkRelative == "." || strings.HasPrefix(walkRelative, "..") || filepath.IsAbs(walkRelative) {
		return manifest, &cstFingerprintError{Kind: "invalid_input", Cause: err}
	}
	components := strings.Split(filepath.ToSlash(walkRelative), "/")
	current := root
	for i, component := range components {
		if component == "" || component == "." || component == ".." || strings.ContainsAny(component, `\/:`) {
			return manifest, &cstFingerprintError{Kind: "invalid_input"}
		}
		child, openErr := cstFingerprintOpenRelative(current, component, true)
		if openErr != nil {
			return manifest, &cstFingerprintError{Kind: "unsafe_object", Cause: openErr}
		}
		if _, infoErr := cstFingerprintHandleInfo(child, true); infoErr != nil {
			return manifest, &cstFingerprintError{Kind: "unsafe_object", Cause: errors.Join(infoErr, windows.CloseHandle(child))}
		}
		if i != len(components)-1 {
			handles = append(handles, child)
			current = child
		} else {
			if walkErr := cstFingerprintWalk(ctx, child, filepath.ToSlash(walkRelative), scan); walkErr != nil {
				return manifest, walkErr
			}
		}
	}
	if len(scan.rows) == 0 {
		return manifest, errors.New("empty bundle inventory")
	}
	sort.Slice(scan.rows, func(i, j int) bool { return scan.rows[i].Path < scan.rows[j].Path })
	aggregate := sha256.New()
	for _, row := range scan.rows {
		fmt.Fprintf(aggregate, "%s\x00%s\x00%s\x00%d\x00%s\x00%d\n", row.Path, row.Type, row.Stream, row.Size, row.SHA256, row.ModifiedNS)
	}
	return cstProbeManifest{Schema: cstProbeManifestSchema, Rows: scan.rows, AggregateSHA256: fmt.Sprintf("%x", aggregate.Sum(nil))}, nil
}

func cstBindingInventoryManifest(project, walkRoot string, includeProject bool) (cstProbeManifest, error) {
	return cstBindingInventoryManifestWithBudget(project, walkRoot, includeProject, cstFingerprintInitialRowBudget, cstFingerprintInitialAggregateBudget)
}

func cstBindingDirectoryInventory(project string) ([]string, error) {
	parent := filepath.Dir(project)
	root := strings.TrimSuffix(project, filepath.Ext(project))
	var directories []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("linked bundle entry %q", filepath.Base(path))
		}
		if info.IsDir() {
			relative, err := filepath.Rel(parent, path)
			if err != nil {
				return err
			}
			directories = append(directories, filepath.ToSlash(relative))
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("nonregular bundle entry %q", filepath.Base(path))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(directories)
	return directories, nil
}

func cstBindingAddedFileLinkCount(path string) (uint32, error) {
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, err := windows.CreateFile(pathW, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return 0, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks == 0 {
		return 0, errors.New("new copy entry is linked, a directory, or has no stable link count")
	}
	return info.NumberOfLinks, nil
}

func cstBindingFrozenDirectories(t *testing.T, project string) []string {
	t.Helper()
	directories, err := cstBindingDirectoryInventory(project)
	if err != nil || len(directories) == 0 {
		t.Fatalf("freeze binding directory set: directories=%v err=%v", directories, err)
	}
	return directories
}

func TestCSTSavedFieldBundleProbeCopyModesNoSolve(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	cstProbeWriteFile(t, filepath.Join(strings.TrimSuffix(project, filepath.Ext(project)), "Model", "Model.prj"), "full-project-state")
	fakeRoot, tracePath := cstGenerationSyntheticInterface(t, root)
	repositoryRoot := cstProbeRepositoryRoot(t)
	manifest := cstProbeManifestForProject(t, project)
	fullManifest := cstGenerationFullManifest(t, project)
	for _, mode := range []string{"five-row", "full-bundle"} {
		t.Run(mode, func(t *testing.T) {
			outputRoot := filepath.Join(root, "output-"+mode)
			if err := os.Mkdir(outputRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			request := cstGenerationRequest{
				Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", SourceProject: project, OutputRoot: outputRoot,
				ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, cstGenerationHelperPath(t)),
				ExpectedPythonSHA: cstProbeFileSHA256(t, python), SourceManifest: manifest,
				DiagnosticCopyMode: mode, FullSourceManifest: &fullManifest,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			receipt, result, err := cstGenerationRun(t, ctx, python, request,
				[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
				map[string]string{"MCPHUB_CST_GENERATION_SYNTHETIC": "1", "MCPHUB_CST_GENERATION_TRACE": tracePath})
			if err != nil {
				t.Fatalf("bundle probe: %v; stdout=%q stderr=%q", err, result.Stdout.Prefix, result.Stderr.Prefix)
			}
			if receipt.Status != "PASS:bundle_port_state_inspected" || receipt.BundleProbe == nil || receipt.BundleProbe.CopyMode != mode ||
				!receipt.SourceUnchanged || !receipt.ForeignPreserved || !receipt.SessionSettled || receipt.FullSourceManifestSHA != fullManifest.AggregateSHA256 || receipt.FullSourceRowCount != len(fullManifest.Rows) {
				t.Fatalf("bundle probe receipt=%+v", receipt)
			}
			wantPorts := 0
			if mode == "full-bundle" {
				wantPorts = 2
			}
			if receipt.BundleProbe.PortCount != wantPorts {
				t.Fatalf("bundle state=%+v wantPorts=%d", receipt.BundleProbe, wantPorts)
			}
			if receipt.APITrace == nil || receipt.APITrace.Counts["port-state-query"] != 1 || receipt.APITrace.Counts["port-state-read"] != 1 || receipt.APITrace.Counts["add-history"] != 0 || receipt.APITrace.Counts["run-solver"] != 0 || receipt.APITrace.Counts["project-save"] != 0 {
				t.Fatalf("bundle trace=%+v", receipt.APITrace)
			}
			if entries, readErr := os.ReadDir(outputRoot); readErr != nil || len(entries) != 0 {
				t.Fatalf("bundle workspace entries=%v err=%v", entries, readErr)
			}
		})
	}
}

func TestCSTSavedFieldFullBundleGenerationRequiresTwoPortsBeforeSolver(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	cstProbeWriteFile(t, filepath.Join(strings.TrimSuffix(project, filepath.Ext(project)), "Model", "Model.prj"), "full-project-state")
	fakeRoot, tracePath := cstGenerationSyntheticInterface(t, root)
	repositoryRoot := cstProbeRepositoryRoot(t)
	manifest := cstProbeManifestForProject(t, project)
	fullManifest := cstGenerationFullManifest(t, project)
	for _, portCount := range []string{"2", "0"} {
		t.Run("ports-"+portCount, func(t *testing.T) {
			outputRoot := filepath.Join(root, "output-"+portCount)
			if err := os.Mkdir(outputRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			request := cstGenerationRequest{
				Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", SourceProject: project, OutputRoot: outputRoot,
				ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, cstGenerationHelperPath(t)),
				ExpectedPythonSHA: cstProbeFileSHA256(t, python), SourceManifest: manifest, FullSourceManifest: &fullManifest,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			receipt, _, err := cstGenerationRun(t, ctx, python, request,
				[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
				map[string]string{"MCPHUB_CST_GENERATION_PORT_COUNT": portCount, "MCPHUB_CST_GENERATION_TRACE": tracePath})
			if portCount == "2" {
				if err != nil || receipt.Status != "PASS:copied_line10_fields_generated" || receipt.BundleProbe == nil || receipt.BundleProbe.CopyMode != "full-bundle" || receipt.BundleProbe.PortCount != 2 || receipt.APITrace == nil || receipt.APITrace.Counts["run-solver"] != 1 || receipt.FullSourceManifestSHA != fullManifest.AggregateSHA256 || receipt.FullSourceRowCount != len(fullManifest.Rows) {
					t.Fatalf("full generation receipt=%+v err=%v", receipt, err)
				}
				if err := os.RemoveAll(filepath.Join(outputRoot, receipt.RetainedBundle)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil || receipt.Status != "BLOCKED:field_generation_port_count_invalid" || receipt.BundleProbe == nil || receipt.BundleProbe.PortCount != 0 || receipt.APITrace == nil || receipt.APITrace.Counts["run-solver"] != 0 || !receipt.SourceUnchanged {
					t.Fatalf("zero-port receipt=%+v err=%v", receipt, err)
				}
			}
			if entries, readErr := os.ReadDir(outputRoot); readErr != nil || len(entries) != 0 {
				t.Fatalf("output residue=%v err=%v", entries, readErr)
			}
			if got := cstGenerationFullManifest(t, project); got.AggregateSHA256 != fullManifest.AggregateSHA256 {
				t.Fatal("full source changed")
			}
		})
	}
}

func TestCSTSavedFieldFullBundleRetainsUnverifiedPostSolverTree(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	cstProbeWriteFile(t, filepath.Join(strings.TrimSuffix(project, filepath.Ext(project)), "Model", "Model.prj"), "full-project-state")
	outputRoot := filepath.Join(root, "output")
	if err := os.Mkdir(outputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeRoot, tracePath := cstGenerationSyntheticInterface(t, root)
	repositoryRoot := cstProbeRepositoryRoot(t)
	manifest := cstProbeManifestForProject(t, project)
	fullManifest := cstGenerationFullManifest(t, project)
	request := cstGenerationRequest{Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", SourceProject: project, OutputRoot: outputRoot,
		ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, cstGenerationHelperPath(t)),
		ExpectedPythonSHA: cstProbeFileSHA256(t, python), SourceManifest: manifest, FullSourceManifest: &fullManifest}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, _, err := cstGenerationRun(t, ctx, python, request,
		[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
		map[string]string{"MCPHUB_CST_GENERATION_PORT_COUNT": "2", "MCPHUB_CST_GENERATION_BAD_TREE": "1", "MCPHUB_CST_GENERATION_TRACE": tracePath})
	if err == nil || receipt.Status != "UNVERIFIED:postsolve_result_tree" || receipt.APITrace == nil || receipt.APITrace.Counts["run-solver"] != 1 || receipt.APITrace.Counts["project-save"] != 1 || !receipt.SourceUnchanged || !receipt.ForeignPreserved || !receipt.SessionSettled || len(receipt.ObservedSelectors) != 1 {
		t.Fatalf("unverified receipt=%+v err=%v", receipt, err)
	}
	retained := filepath.Join(outputRoot, receipt.RetainedBundle)
	if _, statErr := os.Stat(retained); statErr != nil {
		t.Fatalf("unverified retained bundle=%v", statErr)
	}
	if err := os.RemoveAll(retained); err != nil {
		t.Fatal(err)
	}
	if got := cstGenerationFullManifest(t, project); got.AggregateSHA256 != fullManifest.AggregateSHA256 {
		t.Fatal("full source changed")
	}
}

func TestCSTSavedFieldBundleCompletenessLine10Diagnostic(t *testing.T) {
	if os.Getenv("MCPHUB_CST_GENERATION_RUN") != "1" {
		t.Skip("set MCPHUB_CST_GENERATION_RUN=1 for authorized native bundle diagnostic")
	}
	python := cstProbeExplicitFile(t, cstProbePythonEnv)
	cstRoot := cstProbeExplicitDirectory(t, cstProbeCSTRootEnv)
	fixture := cstProbeExplicitFile(t, cstProbeFixtureEnv)
	outputRoot := cstProbeExplicitDirectory(t, "MCPHUB_CST_GENERATION_OUTPUT_ROOT")
	fullManifest := cstGenerationFullManifest(t, fixture)
	projectRow, err := cstNativeManifestExactRow(fullManifest, filepath.Base(fixture))
	if err != nil || projectRow.SHA256 != cstProbeFixtureSHA256 {
		t.Fatalf("diagnostic fixture identity drift: %v", err)
	}
	resultBaseline, err := cstBindingResultInventory(fixture)
	if err != nil {
		t.Fatalf("diagnostic Result baseline unavailable: %v", err)
	}
	repositoryRoot := cstProbeRepositoryRoot(t)
	systemRoot := os.Getenv("SystemRoot")
	pythonPaths := []string{filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src"), filepath.Join(cstRoot, "AMD64"), filepath.Join(cstRoot, "AMD64", "python_cst_libraries"), filepath.Join(cstRoot, "Library", "Python", "lib")}
	extra := map[string]string{"PATH": strings.Join([]string{filepath.Join(cstRoot, "AMD64"), filepath.Join(cstRoot, "Python"), filepath.Join(systemRoot, "System32"), systemRoot}, string(os.PathListSeparator))}
	foreignBefore := cstProbeProcessSnapshot(t, cstRoot)
	for _, name := range []string{"staging", "retained-line10-3ghz"} {
		if _, err := os.Stat(filepath.Join(outputRoot, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("diagnostic path exists before metadata: %s", name)
		}
	}
	metadataCtx, metadataCancel := context.WithTimeout(context.Background(), 60*time.Second)
	manifest, _, metadataErr := cstGenerationSourceManifest(t, metadataCtx, python, fixture, pythonPaths, extra)
	metadataCancel()
	if reasons := cstGenerationPostflight(fixture, fullManifest, resultBaseline, projectRow.SHA256, cstRoot, foreignBefore); len(reasons) != 0 || metadataErr != nil {
		t.Fatalf("diagnostic metadata postflight=%v metadata=%v", reasons, metadataErr)
	}
	counts := make(map[string]int, 2)
	for _, mode := range []string{"five-row", "full-bundle"} {
		for _, name := range []string{"staging", "retained-line10-3ghz"} {
			if _, err := os.Stat(filepath.Join(outputRoot, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s diagnostic path exists: %s", mode, name)
			}
		}
		request := cstGenerationRequest{
			Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", SourceProject: fixture, OutputRoot: outputRoot,
			ExpectedProjectSHA: cstProbeFixtureSHA256, ExpectedHelperSHA: cstProbeFileSHA256(t, cstGenerationHelperPath(t)),
			ExpectedPythonSHA: cstProbeFileSHA256(t, python), SourceManifest: manifest,
			DiagnosticCopyMode: mode, FullSourceManifest: &fullManifest,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		receipt, result, runErr := cstGenerationRun(t, ctx, python, request, pythonPaths, extra)
		cancel()
		postReasons := cstGenerationPostflight(fixture, fullManifest, resultBaseline, projectRow.SHA256, cstRoot, foreignBefore)
		if len(postReasons) != 0 {
			t.Fatalf("diagnostic source/foreign postflight: %v", postReasons)
		}
		t.Logf("CST_BUNDLE_DIAGNOSTIC_RECEIPT=%s", strings.TrimSpace(string(result.Stdout.Prefix)))
		if runErr != nil || result.SettledExit == nil || result.SettledExit.TimedOut || result.SettledExit.ExitCode != 0 || receipt.Status != "PASS:bundle_port_state_inspected" || receipt.BundleProbe == nil || receipt.BundleProbe.CopyMode != mode ||
			!receipt.SourceUnchanged || !receipt.ForeignPreserved || !receipt.SessionSettled || receipt.FullSourceManifestSHA != fullManifest.AggregateSHA256 || receipt.FullSourceRowCount != len(fullManifest.Rows) {
			t.Fatalf("%s bundle diagnostic receipt=%+v runErr=%v stderr=%q", mode, receipt, runErr, result.Stderr.Prefix)
		}
		if receipt.APITrace == nil || receipt.APITrace.Counts["port-state-query"] != 1 || receipt.APITrace.Counts["port-state-read"] != 1 || receipt.APITrace.Counts["add-history"] != 0 || receipt.APITrace.Counts["run-solver"] != 0 || receipt.APITrace.Counts["project-save"] != 0 {
			t.Fatalf("%s bundle diagnostic trace=%+v", mode, receipt.APITrace)
		}
		postCtx, postCancel := context.WithTimeout(context.Background(), 60*time.Second)
		got, _, metadataErr := cstGenerationSourceManifest(t, postCtx, python, fixture, pythonPaths, extra)
		postCancel()
		if reasons := cstGenerationPostflight(fixture, fullManifest, resultBaseline, projectRow.SHA256, cstRoot, foreignBefore); len(reasons) != 0 || metadataErr != nil {
			t.Fatalf("%s metadata postflight=%v metadata=%v", mode, reasons, metadataErr)
		}
		if got.AggregateSHA256 != manifest.AggregateSHA256 {
			t.Fatalf("%s changed source", mode)
		}
		if got := cstGenerationFullManifest(t, fixture); got.AggregateSHA256 != fullManifest.AggregateSHA256 {
			t.Fatalf("%s changed full source", mode)
		}
		if foreignAfter := cstProbeProcessSnapshot(t, cstRoot); fmt.Sprint(foreignAfter) != fmt.Sprint(foreignBefore) {
			t.Fatalf("%s changed foreign CST", mode)
		}
		for _, name := range []string{"staging", "retained-line10-3ghz"} {
			if _, err := os.Stat(filepath.Join(outputRoot, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s diagnostic output remains: %s", mode, name)
			}
		}
		counts[mode] = receipt.BundleProbe.PortCount
	}
	if counts["full-bundle"] != 2 {
		t.Fatalf("BLOCKED:canonical_project_port_state_invalid counts=%v", counts)
	}
	if counts["five-row"] == 2 {
		t.Fatalf("BLOCKED:excitation_failure_unexplained counts=%v", counts)
	}
	t.Logf("PASS:five_row_copy_insufficient_for_solve counts=%v", counts)
}

func TestCSTSavedFieldPort2CompletionRetainsObservedOutput(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	cstProbeWriteFile(t, filepath.Join(strings.TrimSuffix(project, filepath.Ext(project)), "Model", "Model.prj"), "full-project-state")
	outputRoot := filepath.Join(root, "output")
	if err := os.Mkdir(outputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeRoot, tracePath := cstGenerationSyntheticInterface(t, root)
	repositoryRoot := cstProbeRepositoryRoot(t)
	manifest := cstProbeManifestForProject(t, project)
	fullManifest := cstGenerationFullManifest(t, project)
	request := cstGenerationRequest{Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", SourceProject: project, OutputRoot: outputRoot,
		ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, cstGenerationHelperPath(t)),
		ExpectedPythonSHA: cstProbeFileSHA256(t, python), SourceManifest: manifest, FullSourceManifest: &fullManifest, TestPort2Completion: true}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, _, err := cstGenerationRun(t, ctx, python, request,
		[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
		map[string]string{"MCPHUB_CST_GENERATION_PORT_COUNT": "2", "MCPHUB_CST_GENERATION_PORT2_GUARD": "2:1", "MCPHUB_CST_GENERATION_TRACE": tracePath})
	if err == nil || receipt.Status != "UNVERIFIED:postsolve_result_tree" || receipt.APITrace == nil || receipt.APITrace.Counts["port2-vba"] != 1 || receipt.APITrace.Counts["port2-readback"] != 1 || receipt.APITrace.Counts["run-solver"] != 1 || receipt.APITrace.Counts["project-save"] != 1 || len(receipt.Results) != 0 || len(receipt.ObservedSelectors) == 0 || !receipt.SourceUnchanged || !receipt.ForeignPreserved || !receipt.SessionSettled {
		t.Fatalf("port2 completion receipt=%+v err=%v", receipt, err)
	}
	if err := os.RemoveAll(filepath.Join(outputRoot, receipt.RetainedBundle)); err != nil {
		t.Fatal(err)
	}
}

func TestCSTSavedFieldGenerateVendorFailuresRetainRedactedReceipt(t *testing.T) {
	python := cstProbeTestPython(t)
	for _, stage := range []string{
		"environment-census", "environment-new", "project-open", "add-history", "run-solver", "project-save",
		"result-tree-first-child", "result-tree-next-item", "project-close", "environment-close",
	} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
			outputRoot := filepath.Join(root, "output")
			if err := os.Mkdir(outputRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			fakeRoot, tracePath := cstGenerationSyntheticInterface(t, root)
			repositoryRoot := cstProbeRepositoryRoot(t)
			manifest := cstProbeManifestForProject(t, project)
			fullManifest := cstGenerationFullManifest(t, project)
			request := cstGenerationRequest{
				Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", SourceProject: project, OutputRoot: outputRoot,
				ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, cstGenerationHelperPath(t)),
				ExpectedPythonSHA: cstProbeFileSHA256(t, python), SourceManifest: manifest, FullSourceManifest: &fullManifest, TestInjectStage: stage,
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			receipt, result, err := cstGenerationRun(t, ctx, python, request,
				[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
				map[string]string{"MCPHUB_CST_GENERATION_SYNTHETIC": "1", "MCPHUB_CST_GENERATION_TRACE": tracePath, "MCPHUB_CST_GENERATION_INJECT_STAGE": stage})
			if err == nil {
				t.Fatal("injected vendor failure exited successfully")
			}
			if receipt.Stage != stage || receipt.ExceptionType != "RuntimeError" || receipt.APITrace == nil || receipt.APITrace.Counts[stage] < 1 || !receipt.SourceUnchanged {
				t.Fatalf("redacted failure receipt = %+v", receipt)
			}
			if stage == "project-save" {
				if receipt.Status != "UNVERIFIED:postsolve_result_tree" || receipt.RetainedBundle == "" || !receipt.ForeignPreserved || !receipt.SessionSettled {
					t.Fatalf("post-solver save retention = %+v", receipt)
				}
				if err := os.RemoveAll(filepath.Join(outputRoot, receipt.RetainedBundle)); err != nil {
					t.Fatal(err)
				}
			} else if receipt.Status != "BLOCKED:field_generation_vendor_exception" || receipt.RetainedBundle != "" {
				t.Fatalf("non-retained failure receipt = %+v", receipt)
			}
			if stage == "project-close" || stage == "environment-close" {
				if receipt.SessionSettled || receipt.CleanupStage != stage {
					t.Fatalf("cleanup failure settlement = %+v", receipt)
				}
			} else if !receipt.SessionSettled {
				t.Fatalf("successful cleanup did not settle session: %+v", receipt)
			}
			wantSolverCount := 0
			switch stage {
			case "run-solver", "project-save", "result-tree-first-child", "result-tree-next-item", "project-close", "environment-close":
				wantSolverCount = 1
			}
			if got := receipt.APITrace.Counts["run-solver"]; got != wantSolverCount {
				t.Fatalf("solver count after %s failure = %d, want %d", stage, got, wantSolverCount)
			}
			for _, forbidden := range []string{root, project, "SECRET_PRIVATE_SENTINEL", string(result.Stderr.Prefix)} {
				if forbidden != "" && strings.Contains(string(result.Stdout.Prefix), forbidden) {
					t.Fatalf("receipt retained unsafe value %q", forbidden)
				}
			}
			for _, name := range []string{"staging", "retained-line10-3ghz"} {
				if _, statErr := os.Stat(filepath.Join(outputRoot, name)); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("failed output remains at %s: %v", name, statErr)
				}
			}
		})
	}
}

func TestCSTSavedFieldGenerateSetupOnlyDiagnosticRedactsVendorMessage(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	outputRoot := filepath.Join(root, "output")
	if err := os.Mkdir(outputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeRoot, tracePath := cstGenerationSyntheticInterface(t, root)
	repositoryRoot := cstProbeRepositoryRoot(t)
	manifest := cstProbeManifestForProject(t, project)
	canary := "SECRET_SETUP_CANARY=do-not-retain"
	request := cstGenerationRequest{
		Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", SourceProject: project, OutputRoot: outputRoot,
		ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, cstGenerationHelperPath(t)),
		ExpectedPythonSHA: cstProbeFileSHA256(t, python), SourceManifest: manifest, TestSetupOnly: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, result, err := cstGenerationRun(t, ctx, python, request,
		[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
		map[string]string{"MCPHUB_CST_GENERATION_SYNTHETIC": "1", "MCPHUB_CST_GENERATION_TRACE": tracePath, "MCPHUB_CST_GENERATION_SETUP_ERROR": project + " " + canary})
	if err == nil {
		t.Fatal("setup-only diagnostic exited successfully")
	}
	if receipt.Status != "BLOCKED:field_generation_vendor_exception" || receipt.Stage != "add-history" || receipt.ExceptionType != "RuntimeError" || receipt.APITrace == nil || receipt.APITrace.Counts["add-history"] != 1 || receipt.APITrace.Counts["run-solver"] != 0 || !receipt.SourceUnchanged || receipt.CompilerDiagnostic == "" || len(receipt.CompilerDiagnostic) > 2048 {
		t.Fatalf("setup-only receipt = %+v", receipt)
	}
	for _, forbidden := range []string{project, outputRoot, canary, "do-not-retain", string(result.Stderr.Prefix)} {
		if forbidden != "" && strings.Contains(string(result.Stdout.Prefix), forbidden) {
			t.Fatalf("diagnostic leaked %q", forbidden)
		}
	}
	for _, name := range []string{"staging", "retained-line10-3ghz"} {
		if _, statErr := os.Stat(filepath.Join(outputRoot, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("setup-only output remains at %s: %v", name, statErr)
		}
	}
}

func TestCSTSavedFieldGenerateSetupOnlyStopsBeforeSolver(t *testing.T) {
	python := cstProbeTestPython(t)
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	outputRoot := filepath.Join(root, "output")
	if err := os.Mkdir(outputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeRoot, tracePath := cstGenerationSyntheticInterface(t, root)
	repositoryRoot := cstProbeRepositoryRoot(t)
	manifest := cstProbeManifestForProject(t, project)
	request := cstGenerationRequest{
		Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", SourceProject: project, OutputRoot: outputRoot,
		ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, cstGenerationHelperPath(t)),
		ExpectedPythonSHA: cstProbeFileSHA256(t, python), SourceManifest: manifest, TestSetupOnly: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	receipt, _, err := cstGenerationRun(t, ctx, python, request,
		[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
		map[string]string{"MCPHUB_CST_GENERATION_SYNTHETIC": "1", "MCPHUB_CST_GENERATION_TRACE": tracePath})
	if err != nil || receipt.Status != "PASS:setup_only" || receipt.APITrace == nil || receipt.APITrace.Counts["add-history"] != 1 || receipt.APITrace.Counts["run-solver"] != 0 || receipt.APITrace.Counts["project-save"] != 0 || !receipt.SourceUnchanged || !receipt.ForeignPreserved || !receipt.SessionSettled {
		t.Fatalf("setup-only control receipt=%+v err=%v", receipt, err)
	}
	for _, name := range []string{"staging", "retained-line10-3ghz"} {
		if _, statErr := os.Stat(filepath.Join(outputRoot, name)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("setup-only output remains at %s: %v", name, statErr)
		}
	}
}

func TestCSTSavedFieldGenerateLine10FixtureRetention(t *testing.T) {
	TestCSTSavedFieldGenerateLine10FixtureCommandAllowlist(t)
}

func TestCSTSavedFieldGenerateLine10FixtureLimits(t *testing.T) {
	if os.Getenv("MCPHUB_CST_TIMEOUT_FIXTURE_WORKER") != "1" {
		t.Skip("exercised by the manual-lifetime fixture parent")
	}
	python := cstProbeTestPython(t)
	root := os.Getenv("MCPHUB_CST_TIMEOUT_FIXTURE_ROOT")
	if !filepath.IsAbs(root) || !strings.HasPrefix(filepath.Base(root), "cst-generation-timeout-") {
		t.Fatal("fixture worker root is not exact test-owned root")
	}
	owned := &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: root, prefix: "cst-generation-timeout-"}}
	defer t.Logf("TEST_WORKER_RETAINED:exact_owned_root=%q", root)
	if err := owned.captureChecked(root); err != nil {
		t.Fatalf("timeout root identity unavailable: %v", err)
	}
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	outputRoot := filepath.Join(root, "output")
	if err := os.Mkdir(outputRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeRoot, tracePath := cstGenerationSyntheticInterface(t, root)
	repositoryRoot := cstProbeRepositoryRoot(t)
	full := cstGenerationFullManifest(t, project)
	resultBaseline, err := cstBindingResultInventory(project)
	if err != nil {
		t.Fatal(err)
	}
	projectRow, err := cstNativeManifestExactRow(full, filepath.Base(project))
	if err != nil {
		t.Fatal(err)
	}
	foreignBefore := cstProbeProcessSnapshot(t, root)
	request := cstGenerationRequest{
		Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", SourceProject: project, OutputRoot: outputRoot,
		ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, cstGenerationHelperPath(t)),
		ExpectedPythonSHA: cstProbeFileSHA256(t, python), SourceManifest: cstProbeManifestForProject(t, project), TestStallMS: 5000,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, result, runErr := cstGenerationRun(t, ctx, python, request,
		[]string{fakeRoot, filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src")},
		map[string]string{"MCPHUB_CST_GENERATION_SYNTHETIC": "1", "MCPHUB_CST_GENERATION_TRACE": tracePath})
	postReasons := cstGenerationPostflight(project, full, resultBaseline, projectRow.SHA256, root, foreignBefore)
	var strict *hubprocess.StrictRunError
	if !errors.As(runErr, &strict) || strict.Kind != hubprocess.StrictRunTimeout || len(postReasons) != 0 {
		t.Fatalf("fixture timeout/source/foreign unverified: run=%v post=%v", runErr, postReasons)
	}
	marker := "timeout-negative.marker"
	if result.SettledExit != nil {
		if err := cstGenerationTimeoutDisposalProof(result, runErr, postReasons, owned); err != nil {
			t.Fatalf("positive timeout proof invalid: %v", err)
		}
		marker = "timeout-positive.marker"
	} else {
		var proof *hubprocess.JobTerminationProofError
		if !errors.As(runErr, &proof) || proof.Stage != "member_wait" || !errors.Is(runErr, hubprocess.ErrCleanupTimeout) {
			t.Fatalf("negative timeout lacks typed proof-window expiry: %v", runErr)
		}
	}
	if err := os.WriteFile(filepath.Join(root, marker), []byte("typed timeout observed after source and foreign postflight"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCSTSavedFieldGenerateLine10FixtureLimitsPostWorkerRoot(t *testing.T) {
	_ = cstProbeTestPython(t)
	root, err := os.MkdirTemp("", "cst-generation-timeout-")
	if err != nil {
		t.Fatal(err)
	}
	owned := &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: root, prefix: "cst-generation-timeout-"}}
	cleanable, cleaned := false, false
	t.Cleanup(func() {
		if cleaned {
			return
		}
		if !cleanable {
			t.Errorf("TEST_CLEANUP_BLOCKED exact_owned_root=%q", root)
			return
		}
		if err := owned.checkIdentity(); err != nil {
			t.Errorf("TEST_CLEANUP_BLOCKED identity=%v exact_root=%q", err, root)
			return
		}
		if err := owned.inventory(root); err != nil {
			t.Errorf("TEST_CLEANUP_BLOCKED inventory=%v exact_root=%q", err, root)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("TEST_CLEANUP_BLOCKED remove=%v exact_root=%q", err, root)
		}
	})
	if err := owned.captureChecked(root); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "source-sentinel.bin")
	if err := os.WriteFile(sentinel, []byte("synthetic source unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreignBefore, err := cstBindingProcessSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCSTSavedFieldGenerateLine10FixtureLimits$")
	cmd.Env = append(os.Environ(), "MCPHUB_CST_TIMEOUT_FIXTURE_WORKER=1", "MCPHUB_CST_TIMEOUT_FIXTURE_ROOT="+root)
	cmd.Dir = root
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, runErr := hubprocess.RunStrictlyContained(ctx, hubprocess.StrictRunInvocation{Command: cmd, InputLimit: 1, StdoutLimit: 1 << 20, StderrLimit: 64 << 10})
	if runErr != nil || result.SettledExit == nil || result.SettledExit.ExitCode != 0 || result.SettledExit.TimedOut || cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("fixture worker not settled: run=%v result=%+v", runErr, result)
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("timeout root did not outlive worker: %v", err)
	}
	content, err := os.ReadFile(sentinel)
	if err != nil || string(content) != "synthetic source unchanged" {
		t.Fatalf("source sentinel changed: %v %q", err, content)
	}
	foreignAfter, err := cstBindingProcessSnapshot(root)
	if err != nil || !reflect.DeepEqual(foreignBefore, foreignAfter) {
		t.Fatalf("foreign process delta: %v", err)
	}
	if err := owned.checkIdentity(); err != nil {
		t.Fatal(err)
	}
	if err := owned.inventory(root); err != nil {
		t.Fatal(err)
	}
	cleanable = true
	positive := filepath.Join(root, "timeout-positive.marker")
	negative := filepath.Join(root, "timeout-negative.marker")
	_, positiveErr := os.Lstat(positive)
	_, negativeErr := os.Lstat(negative)
	if errors.Is(positiveErr, os.ErrNotExist) == errors.Is(negativeErr, os.ErrNotExist) {
		t.Fatalf("worker did not record exactly one typed timeout result: positive=%v negative=%v", positiveErr, negativeErr)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact timeout root remains: %v", err)
	}
	cleaned = true
}

func TestCSTSavedFieldGenerationTimeoutDisposalRequiresPositiveProof(t *testing.T) {
	if os.Getenv("MCPHUB_CST_TIMEOUT_PROOF_WORKER") != "1" {
		t.Skip("exercised by the manual-lifetime parent worker test")
	}
	root := os.Getenv("MCPHUB_CST_TIMEOUT_PROOF_ROOT")
	if !filepath.IsAbs(root) || !strings.HasPrefix(filepath.Base(root), "cst-generation-timeout-proof-") {
		t.Fatal("worker root is not the exact test-owned root")
	}
	owned := &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: root, prefix: "cst-generation-timeout-proof-"}}
	if err := owned.captureChecked(root); err != nil {
		t.Fatal(err)
	}
	timeoutErr := &hubprocess.StrictRunError{Kind: hubprocess.StrictRunTimeout, Cause: context.DeadlineExceeded}
	settled := hubprocess.StrictRunResult{SettledExit: &hubprocess.StrictRunSettledExit{ExitCode: 1, TimedOut: true}}
	if err := cstGenerationTimeoutDisposalProof(settled, timeoutErr, nil, owned); err != nil {
		t.Fatalf("clean settled timeout vetoed: %v", err)
	}
	for _, tc := range []struct {
		name    string
		result  hubprocess.StrictRunResult
		runErr  error
		reasons []string
		owner   *cstSelectorSyntheticRoot
	}{
		{name: "missing_fact", runErr: timeoutErr, owner: owned},
		{name: "close_fault", runErr: errors.Join(timeoutErr, errors.New("close fault")), owner: owned},
		{name: "source_foreign_drift", result: settled, runErr: timeoutErr, reasons: []string{"source_drift", "foreign_delta"}, owner: owned},
		{name: "wrong_kind", result: settled, runErr: errors.New("not a timeout"), owner: owned},
		{name: "wrong_owner", result: settled, runErr: timeoutErr, owner: &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: root, prefix: "wrong-"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := cstGenerationTimeoutDisposalProof(tc.result, tc.runErr, tc.reasons, tc.owner); err == nil {
				t.Fatal("uncertain timeout admitted exact-root deletion")
			}
			if _, err := os.Lstat(root); err != nil {
				t.Fatalf("veto lost exact owned root: %v", err)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(root, "worker-veto.marker"), []byte("uncertain root retained"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCSTSavedFieldGenerationTimeoutProofRootOutlivesWorker(t *testing.T) {
	root, err := os.MkdirTemp("", "cst-generation-timeout-proof-")
	if err != nil {
		t.Fatal(err)
	}
	owned := &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: root, prefix: "cst-generation-timeout-proof-"}}
	cleanable := false
	cleaned := false
	t.Cleanup(func() {
		if cleaned {
			return
		}
		if !cleanable {
			t.Errorf("TEST_CLEANUP_BLOCKED exact_owned_root=%q", root)
			return
		}
		if err := owned.checkIdentity(); err != nil {
			t.Errorf("TEST_CLEANUP_BLOCKED exact_owned_root=%q identity=%v", root, err)
			return
		}
		if err := owned.inventory(root); err != nil {
			t.Errorf("TEST_CLEANUP_BLOCKED exact_owned_root=%q inventory=%v", root, err)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("TEST_CLEANUP_BLOCKED exact_owned_root=%q remove=%v", root, err)
		}
	})
	if err := owned.captureChecked(root); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "source-sentinel.bin")
	if err := os.WriteFile(sentinel, []byte("synthetic source unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	foreignBefore, err := cstBindingProcessSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCSTSavedFieldGenerationTimeoutDisposalRequiresPositiveProof$")
	cmd.Env = append(os.Environ(), "MCPHUB_CST_TIMEOUT_PROOF_WORKER=1", "MCPHUB_CST_TIMEOUT_PROOF_ROOT="+root)
	cmd.Dir = root
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, runErr := hubprocess.RunStrictlyContained(ctx, hubprocess.StrictRunInvocation{Command: cmd, InputLimit: 1, StdoutLimit: 1 << 20, StderrLimit: 64 << 10})
	if runErr != nil || result.SettledExit == nil || result.SettledExit.ExitCode != 0 || result.SettledExit.TimedOut || cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("worker not settled: run=%v result=%+v", runErr, result)
	}
	if _, err := os.Lstat(root); err != nil {
		t.Fatalf("uncertain root did not outlive worker: %v", err)
	}
	content, err := os.ReadFile(sentinel)
	if err != nil || string(content) != "synthetic source unchanged" {
		t.Fatalf("source sentinel changed: %v %q", err, content)
	}
	foreignAfter, err := cstBindingProcessSnapshot(root)
	if err != nil || !reflect.DeepEqual(foreignBefore, foreignAfter) {
		t.Fatalf("foreign process delta: %v", err)
	}
	if err := owned.checkIdentity(); err != nil {
		t.Fatal(err)
	}
	if err := owned.inventory(root); err != nil {
		t.Fatal(err)
	}
	cleanable = true
	marker := filepath.Join(root, "worker-veto.marker")
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("worker did not preserve uncertainty marker after exit: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact test root remains: %v", err)
	}
	cleaned = true
}

func cstGenerationTimeoutDisposalProof(result hubprocess.StrictRunResult, runErr error, postReasons []string, owned *cstSelectorSyntheticRoot) error {
	var strict *hubprocess.StrictRunError
	if result.SettledExit == nil || !result.SettledExit.TimedOut || !errors.As(runErr, &strict) || strict.Kind != hubprocess.StrictRunTimeout {
		return fmt.Errorf("owned_settlement_uncertain: %w", runErr)
	}
	if len(postReasons) != 0 {
		return fmt.Errorf("preservation_unverified: %s", strings.Join(postReasons, "; "))
	}
	if owned == nil {
		return errors.New("root_identity_unproved: missing owner")
	}
	if err := owned.checkIdentity(); err != nil {
		return fmt.Errorf("root_identity_unproved: %w", err)
	}
	if err := owned.inventory(owned.root); err != nil {
		return fmt.Errorf("root_inventory_unproved: %w", err)
	}
	return nil
}

func cstGenerationTreeBytes(root string) (int64, error) {
	return cstGenerationTreeBytesContext(context.Background(), root)
}

func cstGenerationTreeBytesContext(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in generation staging")
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	return total, err
}

func cstStartGenerationWatch(ctx context.Context, cancel context.CancelFunc, root string, baseline int64) (func(), <-chan struct{}, <-chan error) {
	return cstStartGenerationWatchWithWalk(ctx, cancel, root, baseline, cstGenerationTreeBytesContext)
}

type cstGenerationWatchError struct {
	Kind  string
	Cause error
}

func (e *cstGenerationWatchError) Error() string { return e.Kind + ": " + e.Cause.Error() }
func (e *cstGenerationWatchError) Unwrap() error { return e.Cause }

func cstStartGenerationWatchWithWalk(ctx context.Context, cancel context.CancelFunc, root string, baseline int64, walk func(context.Context, string) (int64, error)) (func(), <-chan struct{}, <-chan error) {
	walkCtx, cancelWalk := context.WithCancel(ctx)
	stopCh := make(chan struct{})
	done := make(chan struct{})
	limitHit := make(chan error, 1)
	var once sync.Once
	stop := func() {
		once.Do(func() { cancelWalk(); close(stopCh) })
		<-done
	}
	go func() {
		defer close(done)
		defer cancelWalk()
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-walkCtx.Done():
				return
			case <-stopCh:
				return
			case <-ticker.C:
				size, err := walk(walkCtx, root)
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				if walkCtx.Err() != nil {
					return
				}
				if err == nil && size <= baseline+(2<<30) {
					continue
				}
				if err == nil {
					err = &cstGenerationWatchError{Kind: "growth", Cause: fmt.Errorf("workspace growth exceeded 2 GiB")}
				} else {
					err = &cstGenerationWatchError{Kind: "read", Cause: err}
				}
				limitHit <- err
				cancel()
				return
			}
		}
	}()
	return stop, done, limitHit
}

func TestCSTSavedFieldGenerationWatcherStopJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	stop, done, _ := cstStartGenerationWatch(ctx, cancel, t.TempDir(), 1)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher ignored context cancellation")
	}
	stop()
	stop()
}

func TestCSTSavedFieldGenerationWatcherStopCancelsActiveWalk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	entered := make(chan struct{})
	stop, done, _ := cstStartGenerationWatchWithWalk(ctx, cancel, t.TempDir(), 1, func(walkCtx context.Context, _ string) (int64, error) {
		close(entered)
		<-walkCtx.Done()
		return 0, walkCtx.Err()
	})
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("watcher never entered bounded walk")
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("watcher stop waited on active walk without cancellation")
	}
	select {
	case <-done:
	default:
		t.Fatal("watcher stop returned before join")
	}
	if ctx.Err() != nil {
		t.Fatal("watcher ordinary stop canceled child deadline")
	}
}

func TestCSTSavedFieldGenerationWatcherFaultKindsCancelChild(t *testing.T) {
	for _, tc := range []struct {
		name, wantKind string
		size           int64
		readErr        error
	}{
		{name: "growth", wantKind: "growth", size: (2 << 30) + 1},
		{name: "read", wantKind: "read", readErr: errors.New("synthetic staging read failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stop, done, fault := cstStartGenerationWatchWithWalk(ctx, cancel, t.TempDir(), 0, func(context.Context, string) (int64, error) { return tc.size, tc.readErr })
			defer stop()
			select {
			case err := <-fault:
				var typed *cstGenerationWatchError
				if !errors.As(err, &typed) || typed.Kind != tc.wantKind {
					t.Fatalf("watcher fault=%v, want=%s", err, tc.wantKind)
				}
			case <-time.After(time.Second):
				t.Fatal("watcher fault was not delivered")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("faulted watcher not joined")
			}
			if ctx.Err() != context.Canceled {
				t.Fatalf("child cancellation lost: %v", ctx.Err())
			}
		})
	}
}

func TestCSTSavedFieldGenerationWatcherGoexitJoinsActiveWalk(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	root := t.TempDir()
	entered := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		stop, _, _ := cstStartGenerationWatchWithWalk(ctx, cancel, root, 1, func(walkCtx context.Context, _ string) (int64, error) {
			close(entered)
			<-walkCtx.Done()
			return 0, walkCtx.Err()
		})
		defer func() { stop(); close(joined) }()
		<-entered
		runtime.Goexit()
	}()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("Goexit defer did not cancel and join active watcher")
	}
	if ctx.Err() != nil {
		t.Fatalf("Goexit watcher stop canceled child deadline: %v", ctx.Err())
	}
}

func cstGenerationPostflight(project string, full, result cstProbeManifest, projectSHA string, cstRoot string, foreignBefore map[int]cstProbeProcessRow) []string {
	reasons := cstNativeObserveRetained(project, full, result, projectSHA, nil)
	foreignAfter, err := cstBindingProcessSnapshot(cstRoot)
	if err != nil || !reflect.DeepEqual(foreignAfter, foreignBefore) {
		reasons = append(reasons, fmt.Sprintf("foreign_delta=%v", err))
	}
	return reasons
}

type cstGenerationOutcomeError struct {
	Kind  string
	Cause error
}

func (e *cstGenerationOutcomeError) Error() string {
	if e.Cause == nil {
		return e.Kind
	}
	return e.Kind + ": " + e.Cause.Error()
}
func (e *cstGenerationOutcomeError) Unwrap() error { return e.Cause }

func cstGenerationClassifyOutcome(setupOnly, port2 bool, receipt cstGenerationReceipt, result hubprocess.StrictRunResult, runErr, watchErr error, postReasons []string, full cstProbeManifest) error {
	fail := func(kind string, causes ...error) error {
		return &cstGenerationOutcomeError{Kind: kind, Cause: errors.Join(causes...)}
	}
	if len(postReasons) != 0 {
		return fail("preservation_unverified", errors.New(strings.Join(postReasons, "; ")), runErr, watchErr)
	}
	if watchErr != nil {
		return fail("watcher_unsettled", watchErr, runErr)
	}
	settled := result.SettledExit
	if settled == nil || settled.TimedOut || settled.ExitCode < 0 {
		return fail("owned_settlement_uncertain", runErr)
	}
	var decodeErr *cstLegacyChildError
	if errors.As(runErr, &decodeErr) {
		return fail("receipt_unverified", runErr)
	}
	cleanZero := settled.ExitCode == 0 && runErr == nil
	cleanNonzero := func() bool {
		if settled.ExitCode <= 0 || runErr == nil {
			return false
		}
		var strict *hubprocess.StrictRunError
		var childExit *exec.ExitError
		return errors.As(runErr, &strict) && strict.Kind == hubprocess.StrictRunExecutionFailed &&
			errors.As(strict.Cause, &childExit) && childExit.ExitCode() == settled.ExitCode
	}
	if !receipt.SourceUnchanged || !receipt.ForeignPreserved || !receipt.SessionSettled || receipt.APITrace == nil {
		return fail("receipt_preservation_invalid", runErr)
	}
	if setupOnly {
		if receipt.APITrace.Counts["run-solver"] != 0 || receipt.APITrace.Counts["project-save"] != 0 {
			return fail("receipt_trace_invalid", runErr)
		}
		switch receipt.Status {
		case "PASS:setup_only":
			if cleanZero {
				return nil
			}
		case "BLOCKED:field_generation_vendor_exception":
			if cleanNonzero() && receipt.Stage == "add-history" && receipt.CompilerDiagnostic != "" && receipt.APITrace.Counts["add-history"] == 1 {
				return nil
			}
		}
		return fail("owned_outcome_invalid", runErr)
	}
	if port2 {
		if cleanNonzero() && receipt.Status == "UNVERIFIED:postsolve_result_tree" &&
			receipt.APITrace.Counts["port2-vba"] == 1 && receipt.APITrace.Counts["port2-readback"] == 1 &&
			receipt.APITrace.Counts["run-solver"] == 1 && receipt.APITrace.Counts["project-save"] == 1 &&
			len(receipt.Results) == 0 && len(receipt.ObservedSelectors) != 0 &&
			receipt.BundleProbe != nil && receipt.BundleProbe.CopyMode == "full-bundle" && receipt.BundleProbe.PortCount == 2 &&
			receipt.FullSourceManifestSHA == full.AggregateSHA256 && receipt.FullSourceRowCount == len(full.Rows) {
			return nil
		}
		return fail("owned_outcome_invalid", runErr)
	}
	if cleanZero && receipt.Status == "PASS:copied_line10_fields_generated" && len(receipt.Results) == 4 &&
		receipt.BundleProbe != nil && receipt.BundleProbe.CopyMode == "full-bundle" && receipt.BundleProbe.PortCount == 2 &&
		receipt.FullSourceManifestSHA == full.AggregateSHA256 && receipt.FullSourceRowCount == len(full.Rows) {
		return nil
	}
	return fail("owned_outcome_invalid", runErr)
}

func TestCSTSavedFieldLegacyChildPostflightObservesSourceAndForeignIndependently(t *testing.T) {
	root := t.TempDir()
	project := cstProbeSyntheticBundle(t, filepath.Join(root, "source"))
	full := cstGenerationFullManifest(t, project)
	result, err := cstBindingResultInventory(project)
	if err != nil {
		t.Fatal(err)
	}
	projectRow, err := cstNativeManifestExactRow(full, filepath.Base(project))
	if err != nil {
		t.Fatal(err)
	}
	foreignRoot := filepath.Join(root, "foreign")
	if err := os.Mkdir(foreignRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if reasons := cstGenerationPostflight(project, full, result, projectRow.SHA256, foreignRoot, map[int]cstProbeProcessRow{}); len(reasons) != 0 {
		t.Fatalf("untouched baseline: %v", reasons)
	}
	cstProbeWriteFile(t, project, "source drift")
	reasons := cstGenerationPostflight(project, full, result, projectRow.SHA256, foreignRoot, map[int]cstProbeProcessRow{-1: {PID: -1}})
	joined := strings.Join(reasons, "|")
	if !strings.Contains(joined, "source_drift:project") || !strings.Contains(joined, "foreign_delta") {
		t.Fatalf("independent postflight observations lost: %v", reasons)
	}
}

func TestCSTSavedFieldGenerationSettledOutcomeFake(t *testing.T) {
	if os.Getenv("MCPHUB_CST_SETTLEMENT_CHILD") == "1" {
		_, _ = os.Stdout.WriteString(os.Getenv("MCPHUB_CST_SETTLEMENT_PAYLOAD"))
		exitCode, _ := strconv.Atoi(os.Getenv("MCPHUB_CST_SETTLEMENT_EXIT"))
		os.Exit(exitCode)
	}
	full := cstProbeManifest{Schema: cstProbeManifestSchema, AggregateSHA256: strings.Repeat("a", 64), Rows: make([]cstProbeManifestRow, 2)}
	base := cstGenerationReceipt{Schema: "mcphub.cst.saved_field.fixture_generation.receipt.v1", SourceUnchanged: true, ForeignPreserved: true, SessionSettled: true, FullSourceManifestSHA: full.AggregateSHA256, FullSourceRowCount: 2}
	setup := base
	setup.Status = "PASS:setup_only"
	setup.APITrace = &cstProbeAPITrace{Counts: map[string]int{"run-solver": 0, "project-save": 0}}
	blocked := setup
	blocked.Status = "BLOCKED:field_generation_vendor_exception"
	blocked.Stage = "add-history"
	blocked.CompilerDiagnostic = "synthetic compiler diagnostic"
	blocked.APITrace = &cstProbeAPITrace{Counts: map[string]int{"add-history": 1, "run-solver": 0, "project-save": 0}}
	blockedMissingTrace := blocked
	blockedMissingTrace.APITrace = &cstProbeAPITrace{Counts: map[string]int{"run-solver": 0, "project-save": 0}}
	port2 := base
	port2.Status = "UNVERIFIED:postsolve_result_tree"
	port2.APITrace = &cstProbeAPITrace{Counts: map[string]int{"port2-vba": 1, "port2-readback": 1, "run-solver": 1, "project-save": 1}}
	port2.ObservedSelectors = []string{"synthetic selector"}
	port2.BundleProbe = &cstGenerationBundleProbe{CopyMode: "full-bundle", PortCount: 2}
	run := func(t *testing.T, desired cstGenerationReceipt, exitCode int, closeFault error) (cstGenerationReceipt, hubprocess.StrictRunResult, error) {
		t.Helper()
		payload, err := json.Marshal(desired)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestCSTSavedFieldGenerationSettledOutcomeFake$")
		cmd.Env = append(os.Environ(), "MCPHUB_CST_SETTLEMENT_CHILD=1", "MCPHUB_CST_SETTLEMENT_PAYLOAD="+string(payload), fmt.Sprintf("MCPHUB_CST_SETTLEMENT_EXIT=%d", exitCode))
		cmd.Dir = t.TempDir()
		invocation := hubprocess.StrictRunInvocation{Command: cmd, InputLimit: 1, StdoutLimit: 1 << 20, StderrLimit: 64 << 10}
		var result hubprocess.StrictRunResult
		var runErr error
		if closeFault != nil {
			result, runErr = hubprocess.RunStrictlyContainedWithJobCloseFaultForTest(context.Background(), invocation, closeFault)
		} else {
			result, runErr = hubprocess.RunStrictlyContained(context.Background(), invocation)
		}
		receipt, decodeErr := cstDecodeGenerationReceipt(result, runErr)
		return receipt, result, decodeErr
	}
	for _, tc := range []struct {
		name                 string
		receipt              cstGenerationReceipt
		exitCode             int
		setup, port2         bool
		closeFault, watchErr error
		reasons              []string
		wantKind             string
	}{
		{name: "setup_pass", receipt: setup, setup: true},
		{name: "setup_blocked_exit1", receipt: blocked, exitCode: 1, setup: true},
		{name: "setup_blocked_missing_history_trace", receipt: blockedMissingTrace, exitCode: 1, setup: true, wantKind: "owned_outcome_invalid"},
		{name: "port2_exit1", receipt: port2, exitCode: 1, port2: true},
		{name: "setup_job_close_fault", receipt: setup, setup: true, closeFault: errors.New("inside runner close fault"), wantKind: "owned_settlement_uncertain"},
		{name: "port2_job_close_fault", receipt: port2, exitCode: 1, port2: true, closeFault: errors.New("inside runner close fault"), wantKind: "owned_settlement_uncertain"},
		{name: "watcher_growth", receipt: setup, setup: true, watchErr: errors.New("synthetic growth"), wantKind: "watcher_unsettled"},
		{name: "source_foreign_drift", receipt: setup, setup: true, reasons: []string{"source_drift:project", "foreign_delta"}, wantKind: "preservation_unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt, result, runErr := run(t, tc.receipt, tc.exitCode, tc.closeFault)
			err := cstGenerationClassifyOutcome(tc.setup, tc.port2, receipt, result, runErr, tc.watchErr, tc.reasons, full)
			if tc.wantKind == "" && err != nil {
				t.Fatalf("valid outcome rejected: %v", err)
			}
			if tc.wantKind != "" && (err == nil || !strings.Contains(err.Error(), tc.wantKind)) {
				t.Fatalf("fault not rejected as %s: %v", tc.wantKind, err)
			}
		})
	}
}

func cstGenerationDeadline(setupOnly bool) time.Duration {
	if setupOnly {
		return 60 * time.Second
	}
	return 15 * time.Minute
}

func TestCSTSavedFieldGenerateSetupOnlyRequestUsesShortDeadline(t *testing.T) {
	request := cstGenerationRequest{Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", TestSetupOnly: true}
	if !request.TestSetupOnly || cstGenerationDeadline(request.TestSetupOnly) != 60*time.Second || cstGenerationDeadline(false) != 15*time.Minute {
		t.Fatalf("setup request=%+v deadlines setup=%v full=%v", request, cstGenerationDeadline(true), cstGenerationDeadline(false))
	}
}

func TestCSTSavedFieldGeneratePort2RequestMode(t *testing.T) {
	request := cstGenerationRequest{Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", TestPort2Completion: true}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if request.TestSetupOnly || !request.TestPort2Completion || !strings.Contains(string(encoded), `"test_port2_completion":true`) {
		t.Fatalf("port2 request=%s", encoded)
	}
}

func runCSTSavedFieldGenerateLine10(t *testing.T, setupOnly, port2Completion bool) {
	if os.Getenv("MCPHUB_CST_GENERATION_RUN") != "1" {
		t.Skip("set MCPHUB_CST_GENERATION_RUN=1 for authorized native generation path")
	}
	python := cstProbeExplicitFile(t, cstProbePythonEnv)
	cstRoot := cstProbeExplicitDirectory(t, cstProbeCSTRootEnv)
	fixture := cstProbeExplicitFile(t, cstProbeFixtureEnv)
	outputRoot := cstProbeExplicitDirectory(t, "MCPHUB_CST_GENERATION_OUTPUT_ROOT")
	ctx, cancel := context.WithTimeout(context.Background(), cstGenerationDeadline(setupOnly))
	defer cancel()
	fullManifest := cstGenerationFullManifest(t, fixture)
	projectRow, err := cstNativeManifestExactRow(fullManifest, filepath.Base(fixture))
	if err != nil {
		t.Fatalf("generation project row unavailable: %v", err)
	}
	fixtureSHA := projectRow.SHA256
	resultBaseline, err := cstBindingResultInventory(fixture)
	if err != nil {
		t.Fatalf("generation Result baseline unavailable: %v", err)
	}
	repositoryRoot := cstProbeRepositoryRoot(t)
	systemRoot := os.Getenv("SystemRoot")
	pythonPaths := []string{filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src"), filepath.Join(cstRoot, "AMD64"), filepath.Join(cstRoot, "AMD64", "python_cst_libraries"), filepath.Join(cstRoot, "Library", "Python", "lib")}
	extra := map[string]string{"PATH": strings.Join([]string{filepath.Join(cstRoot, "AMD64"), filepath.Join(cstRoot, "Python"), filepath.Join(systemRoot, "System32"), systemRoot}, string(os.PathListSeparator))}
	foreignBefore := cstProbeProcessSnapshot(t, cstRoot)
	for _, name := range []string{"staging", "retained-line10-3ghz"} {
		if _, err := os.Stat(filepath.Join(outputRoot, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("generation path exists: %s", name)
		}
	}
	manifest, _, metadataErr := cstGenerationSourceManifest(t, ctx, python, fixture, pythonPaths, extra)
	if reasons := cstGenerationPostflight(fixture, fullManifest, resultBaseline, fixtureSHA, cstRoot, foreignBefore); len(reasons) != 0 || metadataErr != nil {
		t.Fatalf("generation metadata postflight=%v metadata=%v", reasons, metadataErr)
	}
	request := cstGenerationRequest{
		Schema: "mcphub.cst.saved_field.fixture_generation.request.v1", SourceProject: fixture, OutputRoot: outputRoot,
		ExpectedProjectSHA: fixtureSHA, ExpectedHelperSHA: cstProbeFileSHA256(t, cstGenerationHelperPath(t)),
		ExpectedPythonSHA: cstProbeFileSHA256(t, python), SourceManifest: manifest, FullSourceManifest: &fullManifest,
		TestSetupOnly: setupOnly, TestPort2Completion: port2Completion,
	}
	baseline := int64(0)
	for _, row := range manifest.Rows {
		baseline += row.Size
	}
	stopWatch, _, limitHit := cstStartGenerationWatch(ctx, cancel, filepath.Join(outputRoot, "staging"), baseline)
	defer stopWatch()
	receipt, result, runErr := cstGenerationRun(t, ctx, python, request,
		pythonPaths, extra)
	stopWatch()
	var watchErr error
	select {
	case err := <-limitHit:
		watchErr = err
	default:
	}
	sourceReasons := cstGenerationPostflight(fixture, fullManifest, resultBaseline, fixtureSHA, cstRoot, foreignBefore)
	if outcomeErr := cstGenerationClassifyOutcome(setupOnly, port2Completion, receipt, result, runErr, watchErr, sourceReasons, fullManifest); outcomeErr != nil {
		t.Fatalf("generation unverified: %v; exact owned output preserved at %q", outcomeErr, outputRoot)
	}
	t.Logf("CST_GENERATION_RECEIPT=%s", strings.TrimSpace(string(result.Stdout.Prefix)))
	if port2Completion {
		got, _, metadataErr := cstGenerationSourceManifest(t, ctx, python, fixture, pythonPaths, extra)
		if reasons := cstGenerationPostflight(fixture, fullManifest, resultBaseline, fixtureSHA, cstRoot, foreignBefore); len(reasons) != 0 || metadataErr != nil {
			t.Fatalf("port2 metadata postflight=%v metadata=%v", reasons, metadataErr)
		}
		if got.AggregateSHA256 != manifest.AggregateSHA256 {
			t.Fatal("source changed")
		}
		foreignAfter := cstProbeProcessSnapshot(t, cstRoot)
		if fmt.Sprint(foreignAfter) != fmt.Sprint(foreignBefore) {
			t.Fatalf("foreign CST changed: before=%v after=%v", foreignBefore, foreignAfter)
		}
		if _, err := os.Stat(filepath.Join(outputRoot, receipt.RetainedBundle)); err != nil {
			t.Fatalf("retained bundle missing: %v", err)
		}
		return
	}
	if setupOnly {
		for _, name := range []string{"staging", "retained-line10-3ghz"} {
			if _, err := os.Stat(filepath.Join(outputRoot, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("setup-only output remains at %s: %v", name, err)
			}
		}
		return
	}
	got, _, metadataErr := cstGenerationSourceManifest(t, ctx, python, fixture, pythonPaths, extra)
	if reasons := cstGenerationPostflight(fixture, fullManifest, resultBaseline, fixtureSHA, cstRoot, foreignBefore); len(reasons) != 0 || metadataErr != nil {
		t.Fatalf("generation late metadata postflight=%v metadata=%v", reasons, metadataErr)
	}
	if got.AggregateSHA256 != manifest.AggregateSHA256 {
		t.Fatal("source changed")
	}
	foreignAfter := cstProbeProcessSnapshot(t, cstRoot)
	if fmt.Sprint(foreignAfter) != fmt.Sprint(foreignBefore) {
		t.Fatalf("foreign CST changed: before=%v after=%v", foreignBefore, foreignAfter)
	}
	if _, err := os.Stat(filepath.Join(outputRoot, receipt.RetainedBundle)); err != nil {
		t.Fatalf("retained bundle missing: %v", err)
	}
}

func TestCSTSavedFieldGenerateLine10Fixture(t *testing.T) {
	runCSTSavedFieldGenerateLine10(t, false, false)
}

func TestCSTSavedFieldGenerateLine10SetupOnlyDiagnostic(t *testing.T) {
	runCSTSavedFieldGenerateLine10(t, true, false)
}

func TestCSTSavedFieldGenerateLine10Port2Completion(t *testing.T) {
	runCSTSavedFieldGenerateLine10(t, false, true)
}

type cstProbeProcessRow struct {
	PID          int
	Parent       int
	Image        string
	Path         string
	CreationTime uint64
}

func cstProbeProcessPathAndCreation(pid uint32) (string, uint64) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return "", 0
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return "", 0
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return "", 0
	}
	return windows.UTF16ToString(buffer[:size]), uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
}

func cstProbeProcessSnapshot(t *testing.T, cstRoot string) map[int]cstProbeProcessRow {
	t.Helper()
	rows, err := cstBindingProcessSnapshot(cstRoot)
	if err != nil {
		t.Fatalf("snapshot CST processes: %v", err)
	}
	return rows
}

func cstBindingProcessSnapshot(cstRoot string) (map[int]cstProbeProcessRow, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	rows := make(map[int]cstProbeProcessRow)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return nil, err
	}
	rootPrefix := strings.ToLower(filepath.Clean(cstRoot)) + string(os.PathSeparator)
	for {
		path, creation := cstProbeProcessPathAndCreation(entry.ProcessID)
		if path != "" && strings.HasPrefix(strings.ToLower(filepath.Clean(path)), rootPrefix) {
			pid := int(entry.ProcessID)
			rows[pid] = cstProbeProcessRow{
				PID:          pid,
				Parent:       int(entry.ParentProcessID),
				Image:        windows.UTF16ToString(entry.ExeFile[:]),
				Path:         path,
				CreationTime: creation,
			}
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, err
		}
	}
	return rows, nil
}

func cstProbeExplicitFile(t *testing.T, envName string) string {
	t.Helper()
	raw := os.Getenv(envName)
	if raw == "" || !filepath.IsAbs(raw) {
		t.Fatalf("%s must name an explicit absolute file", envName)
	}
	path := filepath.Clean(raw)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("%s unavailable: %v", envName, err)
	}
	return path
}

func cstProbeExplicitDirectory(t *testing.T, envName string) string {
	t.Helper()
	raw := os.Getenv(envName)
	if raw == "" || !filepath.IsAbs(raw) {
		t.Fatalf("%s must name an explicit absolute directory", envName)
	}
	path := filepath.Clean(raw)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("%s unavailable: %v", envName, err)
	}
	return path
}

func cstProbeRemoveOwnedRoot(t *testing.T, root, parent string) {
	t.Helper()
	root = filepath.Clean(root)
	parent = filepath.Clean(parent)
	if filepath.Dir(root) != parent || !strings.HasPrefix(filepath.Base(root), "cst-result-tree-probe-") {
		t.Fatalf("refuse cleanup outside exact owned root: %q", filepath.Base(root))
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove exact owned root: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned root remains after cleanup: %v", err)
	}
}

func TestCSTSavedFieldOwnedResultTreeProbe(t *testing.T) {
	if os.Getenv(cstProbeRunEnv) != "1" {
		t.Skipf("set %s=1 for the single authorized native inspection", cstProbeRunEnv)
	}
	python := cstProbeExplicitFile(t, cstProbePythonEnv)
	cstRoot := cstProbeExplicitDirectory(t, cstProbeCSTRootEnv)
	fixture := cstProbeExplicitFile(t, cstProbeFixtureEnv)
	scratch := cstProbeExplicitDirectory(t, cstProbeScratchEnv)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	manifest, err := cstProbeManifestForProjectChecked(fixture)
	if err != nil {
		t.Fatalf("safe five-row fixture unavailable: %v", err)
	}
	projectRow, err := cstNativeManifestExactRow(manifest, filepath.Base(fixture))
	if err != nil || projectRow.SHA256 != cstProbeFixtureSHA256 {
		t.Fatalf("fixture identity mismatch: %v", err)
	}
	if len(manifest.Rows) != 5 {
		t.Fatalf("fixture row count = %d, want 5", len(manifest.Rows))
	}
	foreignBefore := cstProbeProcessSnapshot(t, cstRoot)
	ownedRoot, err := os.MkdirTemp(scratch, "cst-result-tree-probe-")
	if err != nil {
		t.Fatalf("create owned probe root: %v", err)
	}
	cleaned := false
	defer func() {
		if !cleaned {
			t.Logf("CST_RESULT_TREE_QUARANTINE exact_owned_root=%q", ownedRoot)
		}
	}()
	request := cstProbeRequest{
		Schema:             cstProbeRequestSchema,
		SourceProject:      fixture,
		WorkspaceRoot:      ownedRoot,
		ExpectedProjectSHA: projectRow.SHA256,
		ExpectedHelperSHA:  cstProbeFileSHA256(t, cstProbeHelperPath(t)),
		ExpectedPythonSHA:  cstProbeFileSHA256(t, python),
		SourceManifest:     manifest,
	}
	root := os.Getenv("SystemRoot")
	repositoryRoot := cstProbeRepositoryRoot(t)
	receipt, result, runErr := cstProbeRun(
		t,
		ctx,
		python,
		request,
		[]string{
			filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src"),
			filepath.Join(cstRoot, "AMD64"),
			filepath.Join(cstRoot, "AMD64", "python_cst_libraries"),
			filepath.Join(cstRoot, "Library", "Python", "lib"),
		},
		map[string]string{
			"PATH": strings.Join([]string{
				filepath.Join(cstRoot, "AMD64"),
				filepath.Join(cstRoot, "Python"),
				filepath.Join(root, "System32"),
				root,
			}, string(os.PathListSeparator)),
		},
	)
	t.Logf("CST_RESULT_TREE_PROBE_RECEIPT=%s", strings.TrimSpace(string(result.Stdout.Prefix)))
	postManifest, postErr := cstProbeManifestForProjectChecked(fixture)
	foreignAfter, foreignErr := cstBindingProcessSnapshot(cstRoot)
	var postReasons []string
	if runErr != nil {
		postReasons = append(postReasons, "owned_run="+runErr.Error())
	}
	if result.SettledExit == nil || result.SettledExit.TimedOut || result.SettledExit.ExitCode != 0 {
		postReasons = append(postReasons, "owned_settlement_uncertain")
	}
	if result.Stdout.Truncated || result.Stderr.Truncated {
		postReasons = append(postReasons, "stream_truncated")
	}
	if postErr != nil || !reflect.DeepEqual(postManifest, manifest) {
		postReasons = append(postReasons, fmt.Sprintf("source_drift:five_row=%v", postErr))
	}
	if foreignErr != nil || !reflect.DeepEqual(foreignAfter, foreignBefore) {
		postReasons = append(postReasons, fmt.Sprintf("foreign_delta=%v", foreignErr))
	}
	if receipt.Status != "PASS:owned_result_tree_listed" && receipt.Status != "BLOCKED:field_reference_absent" {
		postReasons = append(postReasons, "receipt_status_invalid")
	}
	if receipt.Schema != cstProbeReceiptSchema || receipt.SourceManifestSHA256 != manifest.AggregateSHA256 ||
		!cstProbeIsDigest(receipt.DestinationManifestSHA256) || !cstProbeIsDigest(receipt.SessionIdentitySHA256) ||
		receipt.Root != "2D/3D Results" || receipt.TraversalCount != len(receipt.Selectors) ||
		receipt.MaxDepth != 64 || receipt.MaxNodes != 4096 || receipt.MaxSelectorBytes != 1<<20 ||
		!receipt.WorkspaceSettled || !receipt.SessionSettled || !receipt.SourceUnchanged ||
		!receipt.CacheCleared || !receipt.ClosedWithoutSave || !receipt.ForeignProcessesPreserved {
		postReasons = append(postReasons, "receipt_preservation_invalid")
	}
	if len(postReasons) != 0 {
		t.Fatalf("native probe unverified: reasons=%v exact_owned_root=%q", postReasons, ownedRoot)
	}
	cstProbeRequireAllowedAPITrace(t, receipt.APITrace)
	cstProbeRemoveOwnedRoot(t, ownedRoot, scratch)
	cleaned = true
}

func cstBindingHelperPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "tests", "_helpers", "cst_saved_field_selector_binding.py")
}

func cstBindingResultManifest(t *testing.T, full cstProbeManifest, project string) cstProbeManifest {
	t.Helper()
	result, err := cstBindingResultManifestChecked(full, project)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func cstBindingResultManifestChecked(full cstProbeManifest, project string) (cstProbeManifest, error) {
	projectName := filepath.Base(project)
	prefix := strings.TrimSuffix(filepath.Base(project), filepath.Ext(project)) + "/Result/"
	rows := make([]cstProbeManifestRow, 0)
	aggregate := sha256.New()
	for _, row := range full.Rows {
		if row.Path != projectName && !strings.HasPrefix(row.Path, prefix) {
			continue
		}
		rows = append(rows, row)
		fmt.Fprintf(aggregate, "%s\x00%s\x00%s\x00%d\x00%s\x00%d\n", row.Path, row.Type, row.Stream, row.Size, row.SHA256, row.ModifiedNS)
	}
	if len(rows) == 0 {
		return cstProbeManifest{}, errors.New("full bundle has no Result rows")
	}
	return cstProbeManifest{Schema: cstProbeManifestSchema, Rows: rows, AggregateSHA256: fmt.Sprintf("%x", aggregate.Sum(nil))}, nil
}

func cstNativeSourceAdmission(project, expectedProjectSHA string, pairs []cstBindingPair) (cstProbeManifest, cstProbeManifest, error) {
	full, err := cstBindingFullManifest(project)
	if err != nil {
		return cstProbeManifest{}, cstProbeManifest{}, err
	}
	result, err := cstBindingResultInventory(project)
	if err != nil {
		return cstProbeManifest{}, cstProbeManifest{}, err
	}
	projected, err := cstBindingResultManifestChecked(full, project)
	if err != nil || !reflect.DeepEqual(projected, result) {
		return cstProbeManifest{}, cstProbeManifest{}, &cstFingerprintError{Kind: "result_projection", Cause: err}
	}
	projectRow, err := cstNativeManifestExactRow(full, filepath.Base(project))
	if err != nil {
		return cstProbeManifest{}, cstProbeManifest{}, err
	}
	if projectRow.SHA256 != expectedProjectSHA {
		return cstProbeManifest{}, cstProbeManifest{}, &cstFingerprintError{Kind: "identity_mismatch", Relative: projectRow.Path}
	}
	for _, pair := range pairs {
		for _, wanted := range []struct{ path, digest string }{{pair.M3DPath, pair.M3DSHA256}, {pair.RexPath, pair.RexSHA256}} {
			row, err := cstNativeManifestExactRow(full, wanted.path)
			if err != nil {
				return cstProbeManifest{}, cstProbeManifest{}, err
			}
			if row.SHA256 != wanted.digest {
				return cstProbeManifest{}, cstProbeManifest{}, &cstFingerprintError{Kind: "identity_mismatch", Relative: row.Path}
			}
		}
	}
	return full, result, nil
}

func cstNativeManifestExactRow(manifest cstProbeManifest, relative string) (cstProbeManifestRow, error) {
	if manifest.Schema != cstProbeManifestSchema || relative == "" || filepath.IsAbs(filepath.FromSlash(relative)) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative))) != relative {
		return cstProbeManifestRow{}, &cstFingerprintError{Kind: "invalid_input", Relative: relative}
	}
	index := sort.Search(len(manifest.Rows), func(i int) bool { return manifest.Rows[i].Path >= relative })
	if index == len(manifest.Rows) || manifest.Rows[index].Path != relative {
		return cstProbeManifestRow{}, &cstFingerprintError{Kind: "missing_row", Relative: relative}
	}
	row := manifest.Rows[index]
	if row.Type != "regular" || row.Stream != "::$DATA" || row.Size <= 0 || !cstProbeIsDigest(row.SHA256) {
		return cstProbeManifestRow{}, &cstFingerprintError{Kind: "invalid_row", Relative: relative}
	}
	return row, nil
}

func cstNativeObserveRetained(project string, baselineFull, baselineResult cstProbeManifest, expectedProjectSHA string, pairs []cstBindingPair) []string {
	var reasons []string
	fullRowBudget, fullAggregateBudget, fullBudgetErr := cstFingerprintBudgetFromBaseline(baselineFull, 0)
	var full cstProbeManifest
	fullErr := fullBudgetErr
	if fullErr == nil {
		full, fullErr = cstBindingFullManifestWithBudget(project, fullRowBudget, fullAggregateBudget)
	}
	if fullErr != nil || !reflect.DeepEqual(full, baselineFull) {
		reasons = append(reasons, fmt.Sprintf("source_drift:full=%v", fullErr))
	}
	resultRowBudget, resultAggregateBudget, resultBudgetErr := cstFingerprintBudgetFromBaseline(baselineResult, 0)
	var result cstProbeManifest
	resultErr := resultBudgetErr
	if resultErr == nil {
		result, resultErr = cstBindingResultInventoryWithBudget(project, resultRowBudget, resultAggregateBudget)
	}
	if resultErr != nil || !reflect.DeepEqual(result, baselineResult) {
		reasons = append(reasons, fmt.Sprintf("source_drift:result=%v", resultErr))
	}
	if fullErr == nil && resultErr == nil {
		projected, projectionErr := cstBindingResultManifestChecked(full, project)
		if projectionErr != nil || !reflect.DeepEqual(projected, result) {
			reasons = append(reasons, fmt.Sprintf("source_drift:result_projection=%v", projectionErr))
		}
	}
	check := func(relative, expectedSHA, reason string) {
		baseline, err := cstNativeManifestExactRow(baselineFull, relative)
		if err != nil {
			reasons = append(reasons, reason+" baseline="+err.Error())
			return
		}
		if baseline.SHA256 != expectedSHA {
			reasons = append(reasons, reason+" baseline_digest")
			return
		}
		var observed cstProbeManifestRow
		if fullErr == nil {
			observed, err = cstNativeManifestExactRow(full, relative)
		} else {
			observed, err = cstFingerprintFile(context.Background(), filepath.Dir(project), relative, max(baseline.Size, 1), nil)
		}
		if err != nil || !reflect.DeepEqual(observed, baseline) {
			reasons = append(reasons, fmt.Sprintf("%s observed=%v", reason, err))
		}
	}
	check(filepath.Base(project), expectedProjectSHA, "source_drift:project")
	for _, pair := range pairs {
		check(pair.M3DPath, pair.M3DSHA256, "source_drift:pair="+pair.M3DPath)
		check(pair.RexPath, pair.RexSHA256, "source_drift:pair="+pair.RexPath)
	}
	return reasons
}

func TestCSTSavedFieldNativePostflightHardlinksKeepIndependentEvidence(t *testing.T) {
	for _, mode := range []string{"project", "rex"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
			full := cstGenerationFullManifest(t, project)
			result, err := cstBindingResultInventory(project)
			if err != nil {
				t.Fatal(err)
			}
			projectRow, err := cstNativeManifestExactRow(full, filepath.Base(project))
			if err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(root, "synthetic-outside.bin")
			if err := os.WriteFile(outside, []byte("outside test data"), 0o600); err != nil {
				t.Fatal(err)
			}
			target := project
			if mode == "rex" {
				target = filepath.Join(filepath.Dir(project), filepath.FromSlash(pairs[0].RexPath))
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(outside, target); err != nil {
				t.Fatal(err)
			}
			reasons := cstNativeObserveRetained(project, full, result, projectRow.SHA256, pairs)
			want := "source_drift:project"
			if mode == "rex" {
				want = "source_drift:pair=" + pairs[0].RexPath
			}
			if !slicesContainsPrefix(reasons, "source_drift:full") || !slicesContainsPrefix(reasons, "source_drift:result") || !slicesContainsPrefix(reasons, want) {
				t.Fatalf("postflight %s stopped before independent reasons: %v", mode, reasons)
			}
		})
	}
}

func TestCSTSavedFieldNativeSourceAdmissionRejectsRetainedHardlinks(t *testing.T) {
	t.Run("safe_exact_full_and_result", func(t *testing.T) {
		root := t.TempDir()
		project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
		full := cstGenerationFullManifest(t, project)
		projectSHA := ""
		for _, row := range full.Rows {
			if row.Path == filepath.Base(project) {
				projectSHA = row.SHA256
			}
		}
		gotFull, gotResult, err := cstNativeSourceAdmission(project, projectSHA, pairs)
		if err != nil || !reflect.DeepEqual(gotFull, full) || len(gotResult.Rows) == 0 {
			t.Fatalf("safe native source not admitted: full=%d Result=%d err=%v", len(gotFull.Rows), len(gotResult.Rows), err)
		}
	})
	for _, mode := range []string{"project", "m3d", "rex"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
			full, err := cstBindingFullManifest(project)
			if err != nil {
				t.Fatal(err)
			}
			projectSHA := ""
			for _, row := range full.Rows {
				if row.Path == filepath.Base(project) {
					projectSHA = row.SHA256
				}
			}
			if projectSHA == "" {
				t.Fatal("test project row absent")
			}
			outside := filepath.Join(root, "synthetic-outside.bin")
			content := []byte("test-owned outside sentinel")
			if err := os.WriteFile(outside, content, 0o600); err != nil {
				t.Fatal(err)
			}
			outsideBefore, err := os.Stat(outside)
			if err != nil {
				t.Fatal(err)
			}
			target := project
			if mode == "m3d" {
				target = filepath.Join(filepath.Dir(project), filepath.FromSlash(pairs[0].M3DPath))
			}
			if mode == "rex" {
				target = filepath.Join(filepath.Dir(project), filepath.FromSlash(pairs[0].RexPath))
			}
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(outside, target); err != nil {
				t.Fatal(err)
			}
			gotFull, gotResult, err := cstNativeSourceAdmission(project, projectSHA, pairs)
			var refused *cstFingerprintError
			if !errors.As(err, &refused) || refused.Kind != "multi_link" || refused.DataReadEntered || len(gotFull.Rows) != 0 || len(gotResult.Rows) != 0 {
				t.Fatalf("native %s hardlink admitted or lost typed veto: full=%d Result=%d err=%v", mode, len(gotFull.Rows), len(gotResult.Rows), err)
			}
			outsideAfter, err := os.Stat(outside)
			if err != nil || !os.SameFile(outsideBefore, outsideAfter) {
				t.Fatalf("outside identity changed: %v", err)
			}
			outsideBytes, err := os.ReadFile(outside)
			if err != nil || !bytes.Equal(outsideBytes, content) {
				t.Fatalf("outside synthetic bytes changed: %v", err)
			}
		})
	}
}

func cstBindingSyntheticBundle(t *testing.T, root string) (string, []cstBindingPair) {
	t.Helper()
	project := filepath.Join(root, "line10.cst")
	cstProbeWriteFile(t, project, "synthetic full project")
	pairs := make([]cstBindingPair, 0, 4)
	for _, field := range []string{"E", "H"} {
		for port := 1; port <= 2; port++ {
			runID := map[string]map[int]int{"E": {1: 8, 2: 3}, "H": {1: 6, 2: 1}}[field][port]
			base := fmt.Sprintf("mcp_line10_%s_3GHz_%d,1", field, port)
			m3d := filepath.ToSlash(filepath.Join("line10", "Result", base+".m3d"))
			rex := filepath.ToSlash(filepath.Join("line10", "Result", base+"_m3d.rex"))
			cstProbeWriteFile(t, filepath.Join(root, filepath.FromSlash(m3d)), fmt.Sprintf("%s port %d unique volume payload", field, port))
			unit, kind := "&amp;U:V^1.:m^-1", "E-Field"
			if field == "H" {
				unit, kind = "&amp;U:A^1.:m^-1", "H-Field"
			}
			rexText := fmt.Sprintf(`<MetaResultFile creator="Solver HFTD - Field 3DFD Monitor"><SimulationProperties fieldname="mcp_line10_%s_3GHz [%d]" frequency="3" encoded_unit="%s" fieldtype="%s" fieldscaling="PEAK"/><ResultDataType vector="1" complex="1"/><ResultGroups><Frame><FieldResultFile filename="%s.m3d" type="m3d"/></Frame></ResultGroups></MetaResultFile>`, field, runID, unit, kind, base)
			cstProbeWriteFile(t, filepath.Join(root, filepath.FromSlash(rex)), rexText)
			pairs = append(pairs, cstBindingPair{Field: field, Port: port, M3DPath: m3d, M3DSHA256: cstProbeFileSHA256(t, filepath.Join(root, filepath.FromSlash(m3d))), RexPath: rex, RexSHA256: cstProbeFileSHA256(t, filepath.Join(root, filepath.FromSlash(rex)))})
		}
	}
	return project, pairs
}

func cstBindingSyntheticInterface(t *testing.T, root string) string {
	t.Helper()
	moduleRoot := filepath.Join(root, "binding-fake-python")
	cstProbeWriteFile(t, filepath.Join(moduleRoot, "cst", "__init__.py"), "")
	cstProbeWriteFile(t, filepath.Join(moduleRoot, "cst", "interface", "__init__.py"), `
import os
from pathlib import Path

_running = {101, 202}
_items = [
    r"2D/3D Results\E-Field\mcp_line10_E_3GHz [8]",
    r"2D/3D Results\E-Field\mcp_line10_E_3GHz [3]",
    r"2D/3D Results\H-Field\mcp_line10_H_3GHz [6]",
    r"2D/3D Results\H-Field\mcp_line10_H_3GHz [1]",
]
if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "surface-current-siblings":
    _items.extend([
        r"2D/3D Results\Surface Current\mcp_line10_H_3GHz [6]",
        r"2D/3D Results\Surface Current\mcp_line10_H_3GHz [1]",
    ])
if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "all-wrong-parents":
    _items = [item.replace("E-Field", "Other-E").replace("H-Field", "Other-H") for item in _items]
if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "five-eligible":
    _items.append(r"2D/3D Results\E-Field\mcp_line10_E_3GHz [42]")
if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "ancestor-only-parent":
    _items[0] = r"2D/3D Results\E-Field\Other\mcp_line10_E_3GHz [8]"
if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "wrong-parent":
    _items[0] = r"2D/3D Results\Wrong-Field\mcp_line10_E_3GHz [8]"
if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "late-wrong-parent":
    _items = [rf"2D/3D Results\Other\other_{index}" for index in range(36)] + [
        r"2D/3D Results\Wrong-Field\mcp_line10_E_3GHz [8]",
        r"2D/3D Results\E-Field\mcp_line10_E_3GHz [3]",
        r"2D/3D Results\H-Field\mcp_line10_H_3GHz [6]",
        r"2D/3D Results\H-Field\mcp_line10_H_3GHz [1]",
    ]
if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "many-labels":
    _items = [rf"2D/3D Results\E-Field\mcp_line10_E_3GHz [{index}]" for index in range(40)]
if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "unicode-labels":
    _items = [rf"2D/3D Results\Other\{index:02d}-" + "é" * 48 for index in range(40)]
if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "overlong-label":
    _items.append(r"2D/3D Results\E-Field\mcp_line10_E_3GHz [" + "9" * 200 + "]")

def running_design_environments():
    return sorted(_running)

class _Tree:
    def __init__(self, model):
        self.model = model
    def GetFirstChildName(self, parent):
        if parent == "2D/3D Results" and os.environ.get("MCPHUB_CST_BINDING_FAULT") == "unsafe-traversal":
            return r"Z:\synthetic-private\project.cst"
        if parent == "2D/3D Results" and os.environ.get("MCPHUB_CST_BINDING_FAULT") == "embedded-drive":
            return r"2D/3D Results\E-Field\Z:\SyntheticVaultDir\ExampleSecretLeaf"
        if parent == "2D/3D Results" and os.environ.get("MCPHUB_CST_BINDING_FAULT") == "embedded-unc":
            return r"2D/3D Results\E-Field\\SyntheticServer\SyntheticVaultDir\ExampleSecretLeaf"
        if parent == "2D/3D Results" and os.environ.get("MCPHUB_CST_BINDING_FAULT") == "invalid-return":
            return 7
        return _items[0] if parent == "2D/3D Results" else ""
    def GetNextItemName(self, current):
        if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "duplicate-selector" and current == _items[-1]:
            return _items[0]
        items = _items[:-1] if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "missing" else _items
        try:
            index = items.index(current) + 1
        except ValueError:
            return ""
        return items[index] if index < len(items) else ""
    def GetFileFromTreeItem(self, selector):
        index = _items.index(selector)
        fault = os.environ.get("MCPHUB_CST_BINDING_FAULT")
        if fault == "duplicate-filename" and index == 1:
            index = 0
        if fault == "swap" and index in (0, 1):
            index = 1 - index
        if fault == "wrong-filename" and index == 0:
            return "missing-field.rex"
        if fault == "outside-copy" and index == 0:
            return os.environ["MCPHUB_CST_BINDING_OUTSIDE"]
        field = "E" if index < 2 else "H"
        port = index % 2 + 1
        return f"mcp_line10_{field}_3GHz_{port},1_m3d.rex"

class _Model3D:
    def __init__(self):
        self.ResultTree = _Tree(self)
        self.selected = None
    def SelectTreeItem(self, selector):
        self.selected = selector
        return os.environ.get("MCPHUB_CST_BINDING_FAULT") != "false-selection" or selector != _items[1]
    def GetFieldFrequency(self):
        if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "bad-frequency" and self.selected == _items[1]:
            return 2.0
        if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "zero-frequency" and self.selected == _items[1]:
            return 0.0
        if os.environ.get("MCPHUB_CST_BINDING_FAULT") == "nan-frequency" and self.selected == _items[1]:
            return float("nan")
        return 3.0

class _Project:
    def __init__(self, path):
        self.model3d = _Model3D()
        self.path = Path(path)
    def close(self):
        fault = os.environ.get("MCPHUB_CST_BINDING_FAULT")
        if fault in ("copy-additive-one", "copy-additive-alternate"):
            result = self.path.with_suffix("") / "Result"
            with (result / "mcp_line10_E_3GHz_1,1_m3d.rex").open("ab") as stream:
                stream.write(b"synthetic copied REX side effect")
            name = "synthetic-one.bin" if fault == "copy-additive-one" else "other-format.dat"
            (result / name).write_bytes(b"new ordinary copied result")

class DesignEnvironment:
    @staticmethod
    def new():
        marker = os.environ.get("MCPHUB_CST_BINDING_PID_FILE")
        if marker:
            with open(marker, "w", encoding="ascii") as stream:
                stream.write(str(os.getpid()))
        _running.add(303)
        return DesignEnvironment()
    def open_project(self, path):
        return _Project(path)
    def close(self):
        _running.discard(303)
`)
	return moduleRoot
}

func cstBindingRun(ctx context.Context, python, helperPath string, request cstBindingRequest, pythonPaths []string, extra map[string]string) (cstBindingReceipt, hubprocess.StrictRunResult, error) {
	input, err := json.Marshal(request)
	if err != nil {
		return cstBindingReceipt{}, hubprocess.StrictRunResult{}, err
	}
	if len(input) > cstGenerationInputLimit {
		return cstBindingReceipt{}, hubprocess.StrictRunResult{}, fmt.Errorf("binding request exceeds 128 KiB: %d", len(input))
	}
	cmd := exec.Command(python, helperPath)
	cmd.Env = cstProbeEnvironment(python, pythonPaths, extra)
	cmd.Dir = request.WorkspaceRoot
	result, runErr := hubprocess.RunStrictlyContained(ctx, hubprocess.StrictRunInvocation{Command: cmd, Input: input, InputLimit: cstGenerationInputLimit, StdoutLimit: 1 << 20, StderrLimit: 64 << 10})
	var receipt cstBindingReceipt
	if len(result.Stdout.Prefix) != 0 {
		if err := json.Unmarshal(result.Stdout.Prefix, &receipt); err != nil {
			return receipt, result, errors.Join(runErr, fmt.Errorf("decode binding receipt: %w", err))
		}
	}
	return receipt, result, runErr
}

func cstBindingRootExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func cstBindingExpectedSelector(sourceProject string, baseline cstProbeManifest, pair cstBindingPair) (string, string, error) {
	return cstBindingExpectedSelectorWithHooks(sourceProject, baseline, pair, nil)
}

func cstBindingParseSelectorMetadata(pair cstBindingPair, data []byte) (string, string, error) {
	var metadata struct {
		XMLName    xml.Name `xml:"MetaResultFile"`
		Simulation []struct {
			FieldName string `xml:"fieldname,attr"`
		} `xml:"SimulationProperties"`
	}
	if err := xml.Unmarshal(data, &metadata); err != nil || len(metadata.Simulation) != 1 {
		return "", "", fmt.Errorf("saved rex fieldname unavailable: %v", err)
	}
	leaf := metadata.Simulation[0].FieldName
	prefix := fmt.Sprintf("mcp_line10_%s_3GHz [", pair.Field)
	if !strings.HasPrefix(leaf, prefix) || !strings.HasSuffix(leaf, "]") {
		return "", "", errors.New("saved rex fieldname shape mismatch")
	}
	runID := strings.TrimSuffix(strings.TrimPrefix(leaf, prefix), "]")
	if runID == "" {
		return "", "", errors.New("saved rex RunID absent")
	}
	for _, digit := range runID {
		if digit < '0' || digit > '9' {
			return "", "", errors.New("saved rex RunID nonnumeric")
		}
	}
	return fmt.Sprintf("2D/3D Results\\%s-Field\\%s", pair.Field, leaf), runID, nil
}

func cstBindingExpectedSelectorWithHooks(sourceProject string, baseline cstProbeManifest, pair cstBindingPair, hooks *cstFingerprintHooks) (string, string, error) {
	baselineRow, err := cstNativeManifestExactRow(baseline, pair.RexPath)
	if err != nil {
		return "", "", err
	}
	if baselineRow.Size > cstFingerprintSelectedRexCaptureLimit {
		return "", "", &cstFingerprintError{Kind: "size_limit", Relative: pair.RexPath}
	}
	if baselineRow.SHA256 != pair.RexSHA256 {
		return "", "", &cstFingerprintError{Kind: "identity_mismatch", Relative: pair.RexPath}
	}
	row, raw, err := cstFingerprintCaptureFile(context.Background(), filepath.Dir(sourceProject), pair.RexPath, baselineRow.Size, cstFingerprintSelectedRexCaptureLimit, hooks)
	if err != nil {
		return "", "", err
	}
	if !reflect.DeepEqual(row, baselineRow) || row.SHA256 != pair.RexSHA256 {
		return "", "", &cstFingerprintError{Kind: "identity_mismatch", Relative: pair.RexPath}
	}
	return cstBindingParseSelectorMetadata(pair, raw)
}

func TestCSTSavedFieldSelectorRexSafeCapture(t *testing.T) {
	root := t.TempDir()
	project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
	baseline := cstGenerationFullManifest(t, project)
	selector, runID, err := cstBindingExpectedSelectorWithHooks(project, baseline, pairs[0], nil)
	if err != nil || selector != `2D/3D Results\E-Field\mcp_line10_E_3GHz [8]` || runID != "8" {
		t.Fatalf("safe E selector rejected: selector=%q RunID=%q err=%v", selector, runID, err)
	}
	t.Run("growth", func(t *testing.T) {
		path := filepath.Join(filepath.Dir(project), filepath.FromSlash(pairs[0].RexPath))
		_, _, err := cstBindingExpectedSelectorWithHooks(project, baseline, pairs[0], &cstFingerprintHooks{BeforeRead: func() error {
			f, openErr := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if openErr != nil {
				return openErr
			}
			_, writeErr := f.Write([]byte("growth"))
			return errors.Join(writeErr, f.Close())
		}})
		var typed *cstFingerprintError
		if !errors.As(err, &typed) || typed.Kind != "changed_during_read" {
			t.Fatalf("changed rex accepted: %v", err)
		}
	})
	t.Run("oversize", func(t *testing.T) {
		otherRoot := t.TempDir()
		other, otherPairs := cstBindingSyntheticBundle(t, filepath.Join(otherRoot, "source"))
		path := filepath.Join(filepath.Dir(other), filepath.FromSlash(otherPairs[0].RexPath))
		if err := os.Truncate(path, cstFingerprintSelectedRexCaptureLimit+1); err != nil {
			t.Fatal(err)
		}
		otherFull := cstGenerationFullManifest(t, other)
		row, rowErr := cstNativeManifestExactRow(otherFull, otherPairs[0].RexPath)
		if rowErr != nil {
			t.Fatal(rowErr)
		}
		otherPairs[0].RexSHA256 = row.SHA256
		_, _, err := cstBindingExpectedSelectorWithHooks(other, otherFull, otherPairs[0], &cstFingerprintHooks{BeforeRead: func() error { t.Fatal("oversized rex entered data read"); return nil }})
		var typed *cstFingerprintError
		if !errors.As(err, &typed) || typed.Kind != "size_limit" || typed.DataReadEntered {
			t.Fatalf("oversized rex accepted: %v", err)
		}
	})
	t.Run("swap", func(t *testing.T) {
		otherRoot := t.TempDir()
		other, otherPairs := cstBindingSyntheticBundle(t, filepath.Join(otherRoot, "source"))
		otherFull := cstGenerationFullManifest(t, other)
		path := filepath.Join(filepath.Dir(other), filepath.FromSlash(otherPairs[0].RexPath))
		outside := filepath.Join(otherRoot, "synthetic-outside.bin")
		if err := os.WriteFile(outside, []byte("test-owned outside"), 0o600); err != nil {
			t.Fatal(err)
		}
		reads := 0
		_, _, err := cstBindingExpectedSelectorWithHooks(other, otherFull, otherPairs[0], &cstFingerprintHooks{
			BeforeLeafOpen: func() error {
				if err := os.Rename(path, path+".moved"); err != nil {
					return err
				}
				return os.Link(outside, path)
			},
			BeforeRead: func() error { reads++; return nil },
		})
		var typed *cstFingerprintError
		if !errors.As(err, &typed) || typed.Kind != "multi_link" || reads != 0 {
			t.Fatalf("swapped rex read outside: reads=%d err=%v", reads, err)
		}
	})
}

func cstBindingTraceError(trace *cstProbeAPITrace, completeBinding bool) error {
	if trace == nil || len(trace.Events) == 0 || len(trace.Counts) != len(cstBindingAllowedEvents)+len(cstBindingForbiddenEvents) {
		return errors.New("missing or incomplete API trace")
	}
	allowed := make(map[string]bool, len(cstBindingAllowedEvents))
	for _, name := range cstBindingAllowedEvents {
		allowed[name] = true
	}
	positions := make(map[string][]int, len(cstBindingAllowedEvents))
	observed := make(map[string]int, len(cstBindingAllowedEvents))
	for index, event := range trace.Events {
		if !allowed[event.Name] {
			return fmt.Errorf("unknown or forbidden API event at index %d", index)
		}
		if event.Outcome != "finish" && event.Outcome != "error" {
			return fmt.Errorf("invalid API outcome at index %d", index)
		}
		if completeBinding && event.Outcome != "finish" {
			return fmt.Errorf("failed API event in selector PASS at index %d", index)
		}
		observed[event.Name]++
		positions[event.Name] = append(positions[event.Name], index)
	}
	for _, name := range cstBindingAllowedEvents {
		count, present := trace.Counts[name]
		if !present || count != observed[name] {
			return errors.New("API event/count mismatch")
		}
	}
	for _, name := range cstBindingForbiddenEvents {
		count, present := trace.Counts[name]
		if !present || count != 0 {
			return errors.New("forbidden API counter")
		}
	}
	if !completeBinding {
		return nil
	}
	for _, name := range []string{"environment-new", "project-open", "project-close", "environment-close"} {
		if len(positions[name]) != 1 {
			return errors.New("incomplete owned-session event sequence")
		}
	}
	files := positions["result-tree-file"]
	selects := positions["select-tree-item"]
	frequencies := positions["field-frequency"]
	if len(files) != 4 || len(selects) != 4 || len(frequencies) != 4 || len(positions["result-tree-first-child"]) == 0 {
		return errors.New("incomplete four-binding event sequence")
	}
	created, opened := positions["environment-new"][0], positions["project-open"][0]
	projectClosed, environmentClosed := positions["project-close"][0], positions["environment-close"][0]
	if !(created < opened && opened < files[0] && files[3] < selects[0] && frequencies[3] < projectClosed && projectClosed+1 == environmentClosed) {
		return errors.New("owned-session event order mismatch")
	}
	for index := 0; index < 4; index++ {
		if files[index] != files[0]+index || selects[index] != files[0]+4+2*index || frequencies[index] != selects[index]+1 {
			return errors.New("file/select/frequency event order mismatch")
		}
	}
	beforeOpen, afterClose := false, false
	for _, position := range positions["environment-census"] {
		beforeOpen = beforeOpen || position < created
		afterClose = afterClose || position > environmentClosed
		if !(position < created || (position > opened && position < files[0]) || position > environmentClosed) {
			return errors.New("environment census event order mismatch")
		}
	}
	if !beforeOpen || !afterClose {
		return errors.New("owned-session census incomplete")
	}
	for _, name := range []string{"result-tree-first-child", "result-tree-next-item"} {
		for _, position := range positions[name] {
			if position <= opened || position >= files[0] {
				return errors.New("result-tree traversal event order mismatch")
			}
		}
	}
	return nil
}

func cstBindingSafeDiagnosticComponent(value string, private map[string]bool) bool {
	if value == "<redacted>" {
		return true
	}
	if value == "" || value == "." || value == ".." || len(value) > 96 || !utf8.ValidString(value) || private[strings.ToLower(value)] || strings.ContainsAny(value, "/\\:") {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
			return false
		}
	}
	return true
}

func cstBindingExactDiagnosticObject(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, errors.New("diagnostic must be an object")
	}
	allowed := make(map[string]bool, len(names))
	for _, name := range names {
		allowed[name] = true
	}
	fields := make(map[string]json.RawMessage, len(names))
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok || !allowed[key] || fields[key] != nil {
			return nil, errors.New("duplicate or unknown diagnostic field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, errors.New("invalid diagnostic field value")
		}
		fields[key] = value
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(fields) != len(names) {
		return nil, errors.New("incomplete diagnostic object")
	}
	return fields, nil
}

func cstBindingSelectorDiagnosticError(request cstBindingRequest, receipt cstBindingReceipt, output hubprocess.StrictRunResult, copyPresent bool) (*cstBindingSelectorDiagnostic, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(output.Stdout.Prefix, &envelope); err != nil {
		return nil, nil // The existing receipt decoder already vetoes malformed output.
	}
	raw, present := envelope["selector_diagnostic"]
	trace := receipt.APITrace
	preFile := copyPresent && receipt.Status == "BLOCKED:selector_payload_ambiguous" && trace != nil &&
		trace.Counts["result-tree-file"] == 0 && trace.Counts["select-tree-item"] == 0 && trace.Counts["field-frequency"] == 0
	if trace != nil {
		for _, name := range cstBindingForbiddenEvents {
			preFile = preFile && trace.Counts[name] == 0
		}
	}
	if !preFile {
		if present || receipt.SelectorDiagnostic != nil {
			return nil, errors.New("diagnostic on unrelated selector outcome")
		}
		return nil, nil
	}
	if !present || receipt.SelectorDiagnostic == nil || len(raw) > 8<<10 || len(raw) == 0 || raw[0] != '{' {
		return nil, errors.New("missing or oversized diagnostic object")
	}
	fields, err := cstBindingExactDiagnosticObject(raw, "branch", "total_count", "candidate_count", "labels", "truncated")
	if err != nil {
		return nil, err
	}
	for _, name := range []string{"branch", "total_count", "candidate_count", "labels", "truncated"} {
		if _, ok := fields[name]; !ok {
			return nil, errors.New("missing diagnostic field")
		}
		if name != "candidate_count" && string(fields[name]) == "null" {
			return nil, errors.New("null diagnostic field")
		}
	}
	if len(fields["labels"]) == 0 || fields["labels"][0] != '[' {
		return nil, errors.New("invalid diagnostic labels")
	}
	var rawLabels []json.RawMessage
	if err := json.Unmarshal(fields["labels"], &rawLabels); err != nil {
		return nil, errors.New("invalid diagnostic label list")
	}
	for _, rawLabel := range rawLabels {
		labelFields, err := cstBindingExactDiagnosticObject(rawLabel, "parent", "leaf")
		if err != nil || len(labelFields["parent"]) == 0 || len(labelFields["leaf"]) == 0 || string(labelFields["parent"]) == "null" || string(labelFields["leaf"]) == "null" {
			return nil, errors.New("invalid diagnostic label fields")
		}
	}
	var diagnostic cstBindingSelectorDiagnostic
	if err := json.Unmarshal(raw, &diagnostic); err != nil || !reflect.DeepEqual(&diagnostic, receipt.SelectorDiagnostic) {
		return nil, errors.New("diagnostic decode mismatch")
	}
	if diagnostic.TotalCount < 0 || diagnostic.TotalCount > 4097 || len(diagnostic.Labels) > 32 || len(diagnostic.Labels) > diagnostic.TotalCount || (diagnostic.TotalCount > 0 && len(diagnostic.Labels) == 0) || (diagnostic.TotalCount > 32 && !diagnostic.Truncated) || (!diagnostic.Truncated && len(diagnostic.Labels) != diagnostic.TotalCount) {
		return nil, errors.New("diagnostic count or truncation invalid")
	}
	if diagnostic.CandidateCount != nil && (*diagnostic.CandidateCount < 0 || *diagnostic.CandidateCount > 4096 || *diagnostic.CandidateCount > diagnostic.TotalCount) {
		return nil, errors.New("diagnostic candidate count invalid")
	}
	switch diagnostic.Branch {
	case "traversal_invalid":
		if diagnostic.CandidateCount != nil {
			return nil, errors.New("traversal branch requires null candidate count")
		}
	case "candidate_count_mismatch":
		if diagnostic.CandidateCount == nil || *diagnostic.CandidateCount == 4 {
			return nil, errors.New("candidate branch count contradiction")
		}
	case "parent_segment_mismatch":
		if diagnostic.CandidateCount == nil || *diagnostic.CandidateCount != 4 {
			return nil, errors.New("parent branch count contradiction")
		}
	default:
		return nil, errors.New("unknown diagnostic branch")
	}
	if trace != nil && diagnostic.TotalCount > trace.Counts["result-tree-first-child"]+trace.Counts["result-tree-next-item"] {
		return nil, errors.New("diagnostic count exceeds traversal calls")
	}
	private := make(map[string]bool)
	for _, path := range []string{request.SourceProject, request.WorkspaceRoot, os.Getenv("USERPROFILE")} {
		for _, component := range strings.FieldsFunc(path, func(character rune) bool { return character == '/' || character == '\\' || character == ':' }) {
			if len(component) > 1 {
				private[strings.ToLower(component)] = true
			}
		}
	}
	private[strings.ToLower(strings.TrimSuffix(filepath.Base(request.SourceProject), filepath.Ext(request.SourceProject)))] = true
	for _, label := range diagnostic.Labels {
		if !cstBindingSafeDiagnosticComponent(label.Parent, private) || !cstBindingSafeDiagnosticComponent(label.Leaf, private) {
			return nil, errors.New("unsafe diagnostic label")
		}
	}
	return &diagnostic, nil
}

// cstBindingCopyDeltaProjection is the one Go policy for a freshly copied
// project after opening. Selector and ASCII hosts consume the same catalog;
// neither treats copy-only nonprotected changes as source preservation loss.
func cstBindingCopyLinkedRowReason(baseline map[string]cstProbeManifestRow, relative string) string {
	if _, present := baseline[relative]; present {
		return "copy_inventory_invalid:linked_row=" + relative
	}
	return "copy_inventory_invalid:linked_added_row=" + relative
}

func cstBindingCopyFailedRowReason(baseline cstProbeManifest, scanErr error) string {
	var failed *cstFingerprintError
	if !errors.As(scanErr, &failed) || failed.Relative == "" || failed.DataReadEntered || filepath.IsAbs(filepath.FromSlash(failed.Relative)) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(failed.Relative))) != failed.Relative {
		return ""
	}
	switch failed.Kind {
	case "multi_link", "reparse", "leaf_open", "invalid_kind", "metadata":
	default:
		return ""
	}
	rows := make(map[string]cstProbeManifestRow, len(baseline.Rows))
	for _, row := range baseline.Rows {
		rows[row.Path] = row
	}
	return cstBindingCopyLinkedRowReason(rows, failed.Relative)
}

func cstBindingCopyDeltaProjection(project string, before, after cstProbeManifest, directories []string, pairs []cstBindingPair) ([]cstBindingCopyDelta, []cstProbeManifestRow, []string) {
	var deltas []cstBindingCopyDelta
	var added []cstProbeManifestRow
	var reasons []string
	baseline := make(map[string]cstProbeManifestRow, len(before.Rows))
	for _, row := range before.Rows {
		baseline[row.Path] = row
	}
	protected := map[string]bool{filepath.Base(project): true}
	for _, pair := range pairs {
		protected[pair.M3DPath] = true
	}
	knownDirectories := make(map[string]bool, len(directories))
	for _, path := range directories {
		knownDirectories[path] = true
	}
	actualDirectories, err := cstBindingDirectoryInventory(project)
	if err != nil || !reflect.DeepEqual(actualDirectories, directories) {
		reasons = append(reasons, fmt.Sprintf("copy_inventory_invalid:directories=%v", err))
	}
	for _, relative := range actualDirectories {
		path := filepath.Join(filepath.Dir(project), filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || cstSelectorCheckEntry(path, info) != nil {
			reasons = append(reasons, "copy_inventory_invalid:directory_kind="+relative)
		}
	}
	seen := make(map[string]bool, len(after.Rows))
	for _, row := range after.Rows {
		relative := filepath.FromSlash(row.Path)
		prior, found := baseline[row.Path]
		if row.Type != "regular" || row.Stream != "::$DATA" || filepath.IsAbs(relative) || filepath.ToSlash(filepath.Clean(relative)) != row.Path || strings.HasPrefix(row.Path, "../") || seen[row.Path] {
			reasons = append(reasons, "copy_inventory_invalid:row="+row.Path)
			continue
		}
		seen[row.Path] = true
		path := filepath.Join(filepath.Dir(project), relative)
		info, statErr := os.Lstat(path)
		links, linkErr := cstBindingAddedFileLinkCount(path)
		if statErr != nil || !info.Mode().IsRegular() || cstSelectorCheckEntry(path, info) != nil || linkErr != nil || links != 1 {
			reasons = append(reasons, fmt.Sprintf("%s links=%d stat=%v count=%v", cstBindingCopyLinkedRowReason(baseline, row.Path), links, statErr, linkErr))
			continue
		}
		if !found {
			if !knownDirectories[filepath.ToSlash(filepath.Dir(relative))] {
				reasons = append(reasons, "copy_inventory_invalid:added_row="+row.Path)
			} else {
				added = append(added, row)
			}
			continue
		}
		if prior.Type != row.Type || prior.Stream != row.Stream {
			reasons = append(reasons, "copy_inventory_invalid:row_kind="+row.Path)
		} else if !reflect.DeepEqual(prior, row) {
			if protected[row.Path] {
				reasons = append(reasons, "copy_protected_drift:row="+row.Path)
			} else {
				deltas = append(deltas, cstBindingCopyDelta{Path: row.Path,
					Before: cstProbeRowState{SHA256: prior.SHA256, Size: prior.Size, ModifiedNS: prior.ModifiedNS},
					After:  cstProbeRowState{SHA256: row.SHA256, Size: row.Size, ModifiedNS: row.ModifiedNS}})
			}
		}
	}
	for path := range baseline {
		if !seen[path] {
			reasons = append(reasons, "copy_inventory_invalid:missing="+path)
		}
	}
	if len(deltas)+len(added) > 512 {
		reasons = append(reasons, "copy_inventory_invalid:delta_bound")
	}
	return deltas, added, reasons
}

type cstBindingCopyEvidence struct {
	Full, Result     cstProbeManifest
	FullOK, ResultOK bool
	Deltas           []cstBindingCopyDelta
	Added            []cstProbeManifestRow
	Reasons          []string
}

// Inventory and protected-file reads are independent observations. A broken
// full walk cannot suppress the Result census or exact protected paths.
func cstBindingObserveCopy(project string, baseline cstProbeManifest, directories []string, pairs []cstBindingPair) cstBindingCopyEvidence {
	return cstBindingObserveCopyWithHooks(project, baseline, directories, pairs, nil)
}

func cstBindingObserveCopyWithHooks(project string, baseline cstProbeManifest, directories []string, pairs []cstBindingPair, hooks func(string) *cstFingerprintHooks) cstBindingCopyEvidence {
	var evidence cstBindingCopyEvidence
	fullRowBudget, fullAggregateBudget, fullErr := cstFingerprintBudgetFromBaseline(baseline, cstFingerprintCopyGrowthBudget)
	if fullErr == nil {
		evidence.Full, fullErr = cstBindingFullManifestWithBudget(project, fullRowBudget, fullAggregateBudget)
	}
	evidence.FullOK = fullErr == nil
	if !evidence.FullOK {
		evidence.Reasons = append(evidence.Reasons, fmt.Sprintf("copy_inventory_invalid:full=%v", fullErr))
		if rowReason := cstBindingCopyFailedRowReason(baseline, fullErr); rowReason != "" {
			evidence.Reasons = append(evidence.Reasons, rowReason)
		}
	}
	resultBaseline, resultErr := cstBindingResultManifestChecked(baseline, project)
	if resultErr == nil {
		rowBudget, aggregateBudget, budgetErr := cstFingerprintBudgetFromBaseline(resultBaseline, cstFingerprintCopyGrowthBudget)
		resultErr = budgetErr
		if resultErr == nil {
			evidence.Result, resultErr = cstBindingResultInventoryWithBudget(project, rowBudget, aggregateBudget)
		}
	}
	evidence.ResultOK = resultErr == nil
	if !evidence.ResultOK {
		evidence.Reasons = append(evidence.Reasons, fmt.Sprintf("copy_inventory_invalid:result=%v", resultErr))
	}
	if evidence.FullOK {
		var projectionReasons []string
		evidence.Deltas, evidence.Added, projectionReasons = cstBindingCopyDeltaProjection(project, baseline, evidence.Full, directories, pairs)
		evidence.Reasons = append(evidence.Reasons, projectionReasons...)
	} else {
		if evidence.ResultOK {
			evidence.Reasons = append(evidence.Reasons, "copy_inventory_invalid:result_observed="+evidence.Result.AggregateSHA256)
		}
		if dirs, err := cstBindingDirectoryInventory(project); err != nil || !reflect.DeepEqual(dirs, directories) {
			evidence.Reasons = append(evidence.Reasons, fmt.Sprintf("copy_inventory_invalid:directories=%v", err))
		}
	}
	baselineRows := make(map[string]cstProbeManifestRow, len(baseline.Rows))
	for _, row := range baseline.Rows {
		baselineRows[row.Path] = row
	}
	protected := []string{filepath.Base(project)}
	for _, pair := range pairs {
		protected = append(protected, pair.M3DPath)
	}
	for _, relative := range protected {
		want, found := baselineRows[relative]
		if !found {
			evidence.Reasons = append(evidence.Reasons, "copy_protected_drift:row="+relative+" missing_baseline")
			continue
		}
		var rowHooks *cstFingerprintHooks
		if hooks != nil {
			rowHooks = hooks(relative)
		}
		observed, readErr := cstFingerprintFile(context.Background(), filepath.Dir(project), relative, max(want.Size, 1), rowHooks)
		if readErr != nil || !reflect.DeepEqual(observed, want) {
			reason := "copy_protected_drift:row=" + relative
			seen := false
			for _, prior := range evidence.Reasons {
				if prior == reason {
					seen = true
					break
				}
			}
			if !seen {
				evidence.Reasons = append(evidence.Reasons, reason)
			}
		}
	}
	return evidence
}

func TestCSTSavedFieldProtectedHardlinkAfterDegradedFull(t *testing.T) {
	root := t.TempDir()
	project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "copy"))
	model := filepath.Join(filepath.Dir(project), "line10", "Model", "extra.bin")
	cstProbeWriteFile(t, model, "ordinary model row")
	baseline, err := cstBindingFullManifest(project)
	if err != nil {
		t.Fatal(err)
	}
	directories, err := cstBindingDirectoryInventory(project)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside-synthetic.bin")
	if err := os.WriteFile(outside, []byte("test-owned outside sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(model); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, model); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(filepath.Dir(project), filepath.FromSlash(pairs[0].M3DPath))
	if err := os.Remove(protected); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, protected); err != nil {
		t.Fatal(err)
	}
	safeReads, outsideReads := 0, 0
	evidence := cstBindingObserveCopyWithHooks(project, baseline, directories, pairs, func(relative string) *cstFingerprintHooks {
		return &cstFingerprintHooks{BeforeRead: func() error {
			if relative == pairs[0].M3DPath {
				outsideReads++
			} else {
				safeReads++
			}
			return nil
		}}
	})
	if evidence.FullOK || evidence.ResultOK {
		t.Fatalf("linked full/Result were accepted: %+v", evidence)
	}
	if outsideReads != 0 || safeReads != 4 {
		t.Fatalf("protected byte handles: outside=%d safe=%d", outsideReads, safeReads)
	}
	if !slicesContainsPrefix(evidence.Reasons, "copy_inventory_invalid:full=") || !slicesContainsPrefix(evidence.Reasons, "copy_inventory_invalid:result=") || !slicesContainsPrefix(evidence.Reasons, "copy_protected_drift:row="+pairs[0].M3DPath) {
		t.Fatalf("degraded evidence lost: %v", evidence.Reasons)
	}
}

// cstBindingSettle is the only post-run owner of this invocation's disposable copy.
// It records every independent observation before deciding whether that copy is disposable.
func cstBindingSettle(t *testing.T, request cstBindingRequest, receipt cstBindingReceipt, output hubprocess.StrictRunResult, runErr, growthErr error, expectedStatus, pidFile, cstRoot, expectedParent string, foreignBefore map[int]cstProbeProcessRow, priorReasons []string, observation *cstBindingSummary) []string {
	t.Helper()
	reasons := append([]string(nil), priorReasons...)
	add := func(reason string) { reasons = append(reasons, reason) }
	if runErr != nil {
		var strictErr *hubprocess.StrictRunError
		if errors.As(runErr, &strictErr) && strictErr.Kind == hubprocess.StrictRunTimeout {
			if errors.Is(runErr, context.Canceled) {
				add("cancelled")
			} else {
				add("timeout")
			} // StrictRunResult has no typed Job-close proof.
		} else {
			add("receipt_invalid:run=" + runErr.Error())
		}
	}
	if growthErr != nil {
		add("growth_bound:" + growthErr.Error())
	}
	if output.Stdout.Truncated || output.Stderr.Truncated {
		add("receipt_invalid:stream_truncated")
	}
	if pidFile != "" {
		pidBytes, err := os.ReadFile(pidFile)
		if err != nil {
			add("owned_unsettled:pid_marker=" + err.Error())
		} else {
			var pid uint32
			if _, err := fmt.Sscan(string(pidBytes), &pid); err != nil || pid == 0 {
				add("owned_unsettled:invalid_pid_marker")
			} else if path, _ := cstProbeProcessPathAndCreation(pid); path != "" {
				add(fmt.Sprintf("owned_unsettled:pid=%d", pid))
			}
		}
	}
	if cstRoot != "" {
		foreignAfter, err := cstBindingProcessSnapshot(cstRoot)
		if err != nil || !reflect.DeepEqual(foreignBefore, foreignAfter) {
			add(fmt.Sprintf("foreign_delta:snapshot=%v before=%v after=%v", err, foreignBefore, foreignAfter))
		}
	}
	baselineRows := make(map[string]cstProbeManifestRow, len(request.FullManifest.Rows))
	for _, row := range request.FullManifest.Rows {
		baselineRows[row.Path] = row
	}
	var copyFull, copyResult cstProbeManifest
	var copyFullOK, copyResultOK bool
	var copyDeltas []cstBindingCopyDelta
	var copyAdded []cstProbeManifestRow
	checkBundle := func(label, project string) {
		if label == "copy" {
			evidence := cstBindingObserveCopy(project, request.FullManifest, request.ExpectedDirectories, request.Pairs)
			copyFull, copyResult = evidence.Full, evidence.Result
			copyFullOK, copyResultOK = evidence.FullOK, evidence.ResultOK
			copyDeltas, copyAdded = evidence.Deltas, evidence.Added
			for _, reason := range evidence.Reasons {
				add(reason)
			}
			return
		}
		rowBudget, aggregateBudget, err := cstFingerprintBudgetFromBaseline(request.FullManifest, 0)
		var full cstProbeManifest
		if err == nil {
			full, err = cstBindingFullManifestWithBudget(project, rowBudget, aggregateBudget)
		}
		if err != nil {
			add("source_drift:full=" + err.Error())
		} else {
			if !reflect.DeepEqual(full, request.FullManifest) {
				add("source_drift:full_manifest")
			}
		}
		rowBudget, aggregateBudget, err = cstFingerprintBudgetFromBaseline(request.ResultManifest, 0)
		var result cstProbeManifest
		if err == nil {
			result, err = cstBindingResultInventoryWithBudget(project, rowBudget, aggregateBudget)
		}
		if err != nil {
			add("source_drift:result=" + err.Error())
		} else {
			if !reflect.DeepEqual(result, request.ResultManifest) {
				add("source_drift:result_manifest")
			}
		}
		checkProtected := func(relative string) {
			baseline, found := baselineRows[relative]
			if !found {
				add("source_drift:row=" + relative + " missing_baseline")
				return
			}
			observed, readErr := cstFingerprintFile(context.Background(), filepath.Dir(project), relative, max(baseline.Size, 1), nil)
			if readErr != nil || !reflect.DeepEqual(observed, baseline) {
				add(fmt.Sprintf("source_drift:row=%s read=%v", relative, readErr))
			}
		}
		checkProtected(filepath.Base(project))
		for _, pair := range request.Pairs {
			checkProtected(pair.M3DPath)
			checkProtected(pair.RexPath)
		}
	}
	projectBaseline, projectFound := baselineRows[filepath.Base(request.SourceProject)]
	var projectRow cstProbeManifestRow
	var projectErr error
	if projectFound {
		projectRow, projectErr = cstFingerprintFile(context.Background(), filepath.Dir(request.SourceProject), filepath.Base(request.SourceProject), max(projectBaseline.Size, 1), nil)
	}
	if !projectFound || projectErr != nil || projectRow.SHA256 != request.ExpectedProjectSHA {
		add(fmt.Sprintf("source_drift:project=%v", projectErr))
	}
	checkBundle("source", request.SourceProject)
	if len(request.ExpectedDirectories) == 0 {
		add("copy_inventory_invalid:directory_baseline_absent")
	} else if directories, err := cstBindingDirectoryInventory(request.SourceProject); err != nil || !reflect.DeepEqual(directories, request.ExpectedDirectories) {
		add(fmt.Sprintf("source_drift:directories=%v", err))
	}
	copied := filepath.Join(request.WorkspaceRoot, "bundle", filepath.Base(request.SourceProject))
	copyInfo, copyErr := os.Lstat(copied)
	copyPresent := copyErr == nil && copyInfo.Mode().IsRegular()
	bundleInfo, bundleErr := os.Lstat(filepath.Dir(copied))
	if bundleErr == nil && bundleInfo.Mode()&os.ModeSymlink != 0 {
		add("copy_inventory_invalid:linked_bundle_root")
	} else if copyPresent || (bundleErr == nil && bundleInfo.IsDir()) {
		checkBundle("copy", copied)
	}
	if copyPresent {
		workspaceEntries, err := os.ReadDir(request.WorkspaceRoot)
		if err != nil || len(workspaceEntries) != 1 || workspaceEntries[0].Name() != "bundle" || !workspaceEntries[0].IsDir() {
			add(fmt.Sprintf("copy_inventory_invalid:workspace_entries=%v", err))
		}
		bundleEntries, err := os.ReadDir(filepath.Dir(copied))
		projectName := filepath.Base(copied)
		dataName := strings.TrimSuffix(projectName, filepath.Ext(projectName))
		if err != nil || len(bundleEntries) != 2 {
			add(fmt.Sprintf("copy_inventory_invalid:bundle_entries=%v", err))
		} else {
			seenProject, seenData := false, false
			for _, entry := range bundleEntries {
				if entry.Name() == projectName && entry.Type().IsRegular() {
					seenProject = true
				} else if entry.Name() == dataName && entry.IsDir() {
					seenData = true
				}
			}
			if !seenProject || !seenData {
				add("copy_inventory_invalid:bundle_entry_kind")
			}
		}
	}
	if !copyPresent {
		entries, err := os.ReadDir(request.WorkspaceRoot)
		if err != nil || len(entries) != 0 {
			add(fmt.Sprintf("copy_inventory_invalid:absent_or_partial stat=%v entries=%v read=%v", copyErr, entries, err))
		}
	}
	if copyFullOK && copyResultOK {
		projected, err := cstBindingResultManifestChecked(copyFull, copied)
		if err != nil || !reflect.DeepEqual(projected, copyResult) {
			add("copy_inventory_invalid:result_projection")
		}
	}
	if receipt.Schema != cstBindingReceiptSchema || receipt.Status != expectedStatus || expectedStatus == "" {
		add("receipt_invalid:schema_or_status")
	}
	preadmission := !copyPresent && strings.HasPrefix(expectedStatus, "BLOCKED:")
	if preadmission {
		if receipt.FailureID != strings.TrimPrefix(expectedStatus, "BLOCKED:") || len(receipt.Rows) != 0 || receipt.APITrace != nil || receipt.CopyManifestSHA256 != "" || receipt.CopyResultSHA256 != "" {
			add("receipt_invalid:preadmission")
		}
	} else {
		copyUnchanged := copyFullOK && copyResultOK && reflect.DeepEqual(copyFull, request.FullManifest) && reflect.DeepEqual(copyResult, request.ResultManifest)
		if !copyPresent || !copyFullOK || !copyResultOK || receipt.FullManifestSHA256 != request.FullManifest.AggregateSHA256 || receipt.ResultManifestSHA256 != request.ResultManifest.AggregateSHA256 || receipt.CopyManifestSHA256 != copyFull.AggregateSHA256 || receipt.CopyResultSHA256 != copyResult.AggregateSHA256 || !receipt.SourceUnchanged || receipt.CopyUnchanged != copyUnchanged || !receipt.ForeignPreserved || !receipt.SessionSettled || !receipt.ClosedWithoutSave || receipt.APITrace == nil {
			add("receipt_invalid:preservation")
		}
		if expectedStatus == "PASS:selector_payload_bound" {
			if receipt.FailureID != "" || len(receipt.Rows) != 4 {
				add("receipt_invalid:pass_rows")
			}
			seen := make(map[string]bool)
			seenSelectors := make(map[string]bool)
			for _, row := range receipt.Rows {
				key := fmt.Sprintf("%s:%d", row.Field, row.Port)
				var matched *cstBindingPair
				for index := range request.Pairs {
					pair := &request.Pairs[index]
					if pair.Field == row.Field && pair.Port == row.Port {
						matched = pair
						break
					}
				}
				if seen[key] || matched == nil || row.M3DPath != matched.M3DPath || row.M3DSHA256 != matched.M3DSHA256 || row.RexPath != matched.RexPath || row.RexSHA256 != matched.RexSHA256 || row.RawFrequency != 3 || row.RunID == "" || !strings.HasPrefix(row.Selector, "2D/3D Results\\") || (row.ReturnedFile != matched.M3DPath && row.ReturnedFile != matched.RexPath) {
					add("receipt_invalid:row=" + key)
				}
				if matched != nil {
					selector, runID, selectorErr := cstBindingExpectedSelector(request.SourceProject, request.FullManifest, *matched)
					if selectorErr != nil || row.Selector != selector || row.RunID != runID || seenSelectors[row.Selector] {
						add(fmt.Sprintf("receipt_invalid:selector=%s source=%v", key, selectorErr))
					}
				}
				seen[key] = true
				seenSelectors[row.Selector] = true
			}
			if len(seen) != 4 {
				add("receipt_invalid:pair_inventory")
			}
			if receipt.APITrace == nil || receipt.APITrace.Counts["result-tree-file"] != 4 || receipt.APITrace.Counts["select-tree-item"] != 4 || receipt.APITrace.Counts["field-frequency"] != 4 {
				add("receipt_invalid:binding_trace")
			}
		} else if len(receipt.Rows) != 0 || receipt.FailureID != strings.TrimPrefix(expectedStatus, "BLOCKED:") {
			add("receipt_invalid:blocked_rows")
		}
		if strings.HasPrefix(expectedStatus, "BLOCKED:") && receipt.Status == expectedStatus {
			add("selector_blocked:" + receipt.FailureID)
		}
		if receipt.APITrace != nil {
			for _, name := range cstBindingForbiddenEvents {
				want := 0
				if name == request.TestForbiddenCall && strings.HasPrefix(expectedStatus, "BLOCKED:") {
					want = 1
				}
				if receipt.APITrace.Counts[name] != want {
					add("receipt_invalid:forbidden_trace=" + name)
				}
			}
			if err := cstBindingTraceError(receipt.APITrace, expectedStatus == "PASS:selector_payload_bound"); err != nil {
				add("receipt_invalid:api_events=" + err.Error())
			}
		}
	}
	validatedDiagnostic, diagnosticErr := cstBindingSelectorDiagnosticError(request, receipt, output, copyPresent)
	if diagnosticErr != nil {
		add("receipt_invalid:selector_diagnostic=" + diagnosticErr.Error())
	}
	root := filepath.Clean(request.WorkspaceRoot)
	parent := filepath.Clean(filepath.Dir(root))
	if !filepath.IsAbs(root) || !strings.EqualFold(parent, filepath.Clean(expectedParent)) || !strings.HasPrefix(filepath.Base(root), "cst-selector-binding-") {
		add("owned_unsettled:root_identity")
	} else {
		for _, path := range []string{parent, root} {
			info, err := os.Lstat(path)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				add(fmt.Sprintf("owned_unsettled:root_guard=%s err=%v", path, err))
			}
		}
	}
	if len(copyDeltas)+len(copyAdded) > 512 {
		add("copy_inventory_invalid:delta_bound")
	}
	summary := cstBindingSummary{
		Status: "invalid", FailureID: "invalid", RowCount: len(receipt.Rows),
		ClosedWithoutSave: receipt.ClosedWithoutSave, SessionSettled: receipt.SessionSettled,
		SourceUnchanged: receipt.SourceUnchanged, CopyUnchanged: receipt.CopyUnchanged,
		ForeignPreserved: receipt.ForeignPreserved, CopyDeltas: copyDeltas, CopyDeltaCount: len(copyDeltas),
		CopyAdded: copyAdded, CopyAddedCount: len(copyAdded),
		SelectorDiagnostic: validatedDiagnostic,
	}
	if receipt.Status == expectedStatus && expectedStatus != "" {
		summary.Status = receipt.Status
		if receipt.FailureID == "" || expectedStatus == "BLOCKED:"+receipt.FailureID {
			summary.FailureID = receipt.FailureID
		}
	}
	for _, entry := range []struct {
		value  string
		target *string
	}{
		{receipt.FullManifestSHA256, &summary.SourceFullSHA}, {receipt.ResultManifestSHA256, &summary.SourceResultSHA},
		{receipt.CopyManifestSHA256, &summary.ReceiptCopyFullSHA}, {receipt.CopyResultSHA256, &summary.ReceiptCopyResultSHA},
	} {
		if cstProbeIsDigest(entry.value) {
			*entry.target = entry.value
		}
	}
	if copyFullOK {
		summary.CopyFullSHA = copyFull.AggregateSHA256
	}
	if copyResultOK {
		summary.CopyResultSHA = copyResult.AggregateSHA256
	}
	receiptInvalid := false
	reasonCodes := make(map[string]bool)
	for _, reason := range reasons {
		code := strings.SplitN(reason, ":", 2)[0]
		reasonCodes[code] = true
		if code == "receipt_invalid" {
			receiptInvalid = true
		}
	}
	for code := range reasonCodes {
		summary.ReasonCodes = append(summary.ReasonCodes, code)
	}
	sort.Strings(summary.ReasonCodes)
	if !receiptInvalid && len(receipt.Rows) <= 4 {
		summary.Rows = receipt.Rows
	} else if len(receipt.Rows) != 0 {
		encoded, _ := json.Marshal(receipt.Rows)
		summary.RowsSHA256 = fmt.Sprintf("%x", sha256.Sum256(encoded))
	}
	if receipt.APITrace != nil {
		known := map[string]bool{}
		for _, name := range append(append([]string(nil), cstBindingAllowedEvents...), cstBindingForbiddenEvents...) {
			known[name] = true
		}
		summary.TraceCount = len(receipt.APITrace.Events)
		summary.TraceCounts = make(map[string]int)
		for name, count := range receipt.APITrace.Counts {
			if known[name] {
				summary.TraceCounts[name] = count
			}
		}
		for index, event := range receipt.APITrace.Events {
			if index == 64 {
				break
			}
			if !known[event.Name] || (event.Outcome != "finish" && event.Outcome != "error") {
				event = cstProbeAPIEvent{Name: "redacted", Outcome: "error"}
			}
			summary.TraceEvents = append(summary.TraceEvents, event)
		}
	}
	if len(reasons) == 0 {
		summary.Disposition = "delete_eligible"
	} else {
		summary.Disposition = "quarantine"
	}
	encodedSummary, _ := json.Marshal(summary)
	if len(encodedSummary) > 128<<10 {
		add("receipt_invalid:summary_bound")
		summary.Rows = nil
		summary.TraceEvents = nil
		summary.CopyDeltas = nil
		summary.CopyAdded = nil
		summary.ReasonCodes = append(summary.ReasonCodes, "receipt_invalid")
		summary.Disposition = "quarantine"
		encodedSummary, _ = json.Marshal(summary)
	}
	if observation != nil {
		*observation = summary
	}
	t.Logf("CST_BINDING_SUMMARY=%s", encodedSummary)
	if len(reasons) != 0 {
		return reasons
	}
	if err := os.RemoveAll(root); err != nil {
		add("delete_failed:" + err.Error())
	} else if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		add(fmt.Sprintf("delete_failed:root_remains=%v", err))
	} else {
		t.Log("CST_BINDING_DISPOSITION=deleted")
	}
	return reasons
}

type cstSelectorSyntheticAncestor struct {
	path string
	info os.FileInfo
}

type cstSelectorSyntheticLink struct {
	path, target, kind, sha string
	info                    os.FileInfo
}

type cstSelectorRootIdentity struct {
	root, prefix string
	rootInfo     os.FileInfo
	ancestors    []cstSelectorSyntheticAncestor
	captureErr   error
}

type cstSelectorSyntheticRoot struct {
	cstSelectorRootIdentity
	pidFile       string
	link          *cstSelectorSyntheticLink
	runReturned   bool
	settled       bool
	productExists bool
	cleaned       bool
	reasons       []string
}

// The creating subtest owns only this MkdirTemp result. Product quarantine is
// asserted before this callback can consider deleting its synthetic copy.
func cstSelectorOwnSyntheticRoot(t *testing.T, root string) *cstSelectorSyntheticRoot {
	t.Helper()
	owned := &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: filepath.Clean(root), prefix: "cst-selector-binding-synthetic-"}}
	t.Cleanup(func() { owned.cleanup(t) })
	owned.capture(t, root)
	t.Logf("TEST_OWNED_SELECTOR_ROOT=%q", owned.root)
	return owned
}

func (identity *cstSelectorRootIdentity) capture(t *testing.T, original string) {
	t.Helper()
	if err := identity.captureChecked(original); err != nil {
		t.Fatal(err)
	}
}

func (identity *cstSelectorRootIdentity) captureChecked(original string) error {
	if !filepath.IsAbs(original) || identity.root != original || !strings.HasPrefix(filepath.Base(original), identity.prefix) {
		identity.captureErr = fmt.Errorf("not an exact absolute owned root: %q", original)
		return identity.captureErr
	}
	identity.rootInfo, identity.captureErr = os.Lstat(identity.root)
	if identity.captureErr == nil {
		identity.captureErr = cstSelectorCheckEntry(identity.root, identity.rootInfo)
	}
	if identity.captureErr == nil && !identity.rootInfo.IsDir() {
		identity.captureErr = errors.New("owned root is not a directory")
	}
	for path := filepath.Dir(identity.root); identity.captureErr == nil; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err == nil {
			err = cstSelectorCheckEntry(path, info)
		}
		if err == nil && !info.IsDir() {
			err = errors.New("ancestor is not a directory")
		}
		if err != nil {
			identity.captureErr = fmt.Errorf("capture ancestor %q: %w", path, err)
			break
		}
		identity.ancestors = append(identity.ancestors, cstSelectorSyntheticAncestor{path: path, info: info})
		if filepath.Dir(path) == path {
			break
		}
	}
	return identity.captureErr
}

func cstSelectorCheckEntry(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("link at %q", path)
	}
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(pathW)
	if err != nil {
		return err
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("reparse point at %q", path)
	}
	return nil
}

func (owned *cstSelectorSyntheticRoot) observe(reasons []string) {
	owned.settled = true
	owned.reasons = append([]string(nil), reasons...)
	owned.productExists = cstBindingRootExists(owned.root)
}

func (owned *cstSelectorSyntheticRoot) allowLink(t *testing.T, path, target, kind string) {
	t.Helper()
	wantPath := filepath.Join(owned.root, "bundle", "line10", "Model", "model.bin")
	if kind == "hardlink" {
		wantPath = filepath.Join(owned.root, "bundle", "line10", "Result", "synthetic-linked.dat")
	} else if kind == "protected-symlink" {
		wantPath = filepath.Join(owned.root, "bundle", "line10", "Result", "mcp_line10_E_3GHz_1,1.m3d")
	}
	if owned.link != nil || (kind != "symlink" && kind != "hardlink" && kind != "protected-symlink") || path != wantPath || !filepath.IsAbs(target) {
		t.Fatal("only one known injected synthetic link is allowed")
	}
	if relative, err := filepath.Rel(owned.root, target); err != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))) {
		t.Fatalf("injected link target must be outside synthetic root: target=%q relative=%q err=%v", target, relative, err)
	}
	info, err := os.Stat(target)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("capture external synthetic sentinel %q: %v", target, err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("hash external synthetic sentinel %q: %v", target, err)
	}
	owned.link = &cstSelectorSyntheticLink{path: path, target: target, kind: kind, info: info, sha: fmt.Sprintf("%x", sha256.Sum256(content))}
}

func (identity *cstSelectorRootIdentity) checkIdentity() error {
	if identity.captureErr != nil || identity.rootInfo == nil || len(identity.ancestors) == 0 || !filepath.IsAbs(identity.root) || !strings.HasPrefix(filepath.Base(identity.root), identity.prefix) || filepath.Dir(identity.root) != identity.ancestors[0].path {
		return fmt.Errorf("initial identity unavailable: %v", identity.captureErr)
	}
	for _, ancestor := range identity.ancestors {
		info, err := os.Lstat(ancestor.path)
		if err != nil {
			return fmt.Errorf("ancestor %q: %w", ancestor.path, err)
		}
		if err := cstSelectorCheckEntry(ancestor.path, info); err != nil || !info.IsDir() || !os.SameFile(ancestor.info, info) {
			return fmt.Errorf("ancestor identity changed at %q: %v", ancestor.path, err)
		}
	}
	info, err := os.Lstat(identity.root)
	if err != nil {
		return fmt.Errorf("root %q: %w", identity.root, err)
	}
	if err := cstSelectorCheckEntry(identity.root, info); err != nil || !info.IsDir() || !os.SameFile(identity.rootInfo, info) {
		return fmt.Errorf("root identity changed: %v", err)
	}
	return nil
}

func (owned *cstSelectorSyntheticRoot) checkTarget(wantLinks uint32) error {
	if owned.link == nil {
		return nil
	}
	info, err := os.Lstat(owned.link.target)
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(owned.link.info, info) {
		return fmt.Errorf("external sentinel identity changed: %v", err)
	}
	if err := cstSelectorCheckEntry(owned.link.target, info); err != nil {
		return err
	}
	content, err := os.ReadFile(owned.link.target)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(content)) != owned.link.sha {
		return fmt.Errorf("external sentinel content changed: %v", err)
	}
	links, err := cstBindingAddedFileLinkCount(owned.link.target)
	if err != nil || links != wantLinks {
		return fmt.Errorf("external sentinel link count=%d want=%d: %v", links, wantLinks, err)
	}
	return nil
}

func (owned *cstSelectorSyntheticRoot) inventory(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if owned.link != nil && path == owned.link.path {
			if owned.link.kind == "symlink" || owned.link.kind == "protected-symlink" {
				resolved, readErr := os.Readlink(path)
				if info.Mode()&os.ModeSymlink == 0 || readErr != nil || resolved != owned.link.target {
					return fmt.Errorf("injected symlink changed: %v", readErr)
				}
			} else {
				targetInfo, targetErr := os.Lstat(owned.link.target)
				links, linkErr := cstBindingAddedFileLinkCount(path)
				if !info.Mode().IsRegular() || targetErr != nil || !os.SameFile(info, targetInfo) || linkErr != nil || links != 2 {
					return fmt.Errorf("injected hardlink changed: target=%v links=%d count=%v", targetErr, links, linkErr)
				}
			}
			continue
		}
		if err := cstSelectorCheckEntry(path, info); err != nil {
			return err
		}
		if info.IsDir() {
			if err := owned.inventory(path); err != nil {
				return err
			}
		} else if info.Mode().IsRegular() {
			links, err := cstBindingAddedFileLinkCount(path)
			if err != nil || links != 1 {
				return fmt.Errorf("unknown hardlink at %q count=%d: %v", path, links, err)
			}
		} else {
			return fmt.Errorf("unknown entry type at %q: %v", path, info.Mode())
		}
	}
	return nil
}

func (owned *cstSelectorSyntheticRoot) cleanup(t *testing.T) {
	t.Helper()
	block := func(kind string, err error) {
		t.Errorf("TEST_CLEANUP_BLOCKED:%s exact_root=%q: %v", kind, owned.root, err)
	}
	if !owned.settled || !owned.runReturned || slicesContainsPrefix(owned.reasons, "owned_unsettled:") || slicesContainsPrefix(owned.reasons, "delete_failed:") {
		block("unsettled", fmt.Errorf("product_observed=%v run_returned=%v reasons=%v", owned.settled, owned.runReturned, owned.reasons))
		return
	}
	if owned.pidFile != "" {
		pidBytes, err := os.ReadFile(owned.pidFile)
		var pid uint32
		count, parseErr := fmt.Sscan(string(pidBytes), &pid)
		if err != nil || parseErr != nil || count != 1 || pid == 0 {
			block("unsettled", fmt.Errorf("child marker unavailable: read=%v parse=%v", err, parseErr))
			return
		}
		if path, _ := cstProbeProcessPathAndCreation(pid); path != "" {
			block("unsettled", fmt.Errorf("child PID %d still present at %q", pid, path))
			return
		}
	}
	if len(owned.reasons) == 0 {
		if owned.productExists || cstBindingRootExists(owned.root) {
			block("unsettled", errors.New("Product did not delete an eligible copy"))
		}
		return
	}
	if !owned.productExists {
		block("unsettled", errors.New("Product quarantine was not observed before test cleanup"))
		return
	}
	if err := owned.checkIdentity(); err != nil {
		block("identity_or_link", err)
		return
	}
	if err := owned.inventory(owned.root); err != nil {
		block("identity_or_link", err)
		return
	}
	wantLinks := uint32(1)
	if owned.link != nil && owned.link.kind == "hardlink" {
		wantLinks = 2
	}
	if err := owned.checkTarget(wantLinks); err != nil {
		block("delete_or_target", err)
		return
	}
	if owned.link != nil {
		if err := os.Remove(owned.link.path); err != nil {
			block("delete_or_target", err)
			return
		}
		if _, err := os.Lstat(owned.link.path); !errors.Is(err, os.ErrNotExist) {
			block("delete_or_target", fmt.Errorf("injected link remains: %v", err))
			return
		}
		if err := owned.checkTarget(1); err != nil {
			block("delete_or_target", err)
			return
		}
	}
	if err := owned.checkIdentity(); err != nil {
		block("identity_or_link", err)
		return
	}
	if err := owned.inventory(owned.root); err != nil {
		block("identity_or_link", err)
		return
	}
	if err := os.RemoveAll(owned.root); err != nil {
		block("delete_or_target", err)
		return
	}
	if _, err := os.Lstat(owned.root); !errors.Is(err, os.ErrNotExist) {
		block("delete_or_target", fmt.Errorf("exact root remains: %v", err))
		return
	}
	if err := owned.checkTarget(1); err != nil {
		block("delete_or_target", err)
		return
	}
	owned.cleaned = true
	t.Logf("TEST_CLEANUP_REMOVED exact_root=%q product_reasons=%v", owned.root, owned.reasons)
}

type cstSelectorHardlinkOutside struct {
	cstSelectorRootIdentity
	sentinel             string
	sentinelInfo         os.FileInfo
	sentinelSHA          string
	workspaceCreatedPath string
	workspace            *cstSelectorSyntheticRoot
	observedTwo          bool
	observationErr       error
}

func cstSelectorOwnHardlinkOutside(t *testing.T, root string) *cstSelectorHardlinkOutside {
	t.Helper()
	owned := &cstSelectorHardlinkOutside{cstSelectorRootIdentity: cstSelectorRootIdentity{root: filepath.Clean(root), prefix: "cst-selector-hardlink-outside-"}}
	t.Cleanup(func() { owned.cleanup(t) })
	owned.capture(t, root)
	t.Logf("TEST_OWNED_OUTSIDE_ROOT=%q", owned.root)
	return owned
}

func (owned *cstSelectorHardlinkOutside) captureSentinel(t *testing.T, content []byte) {
	t.Helper()
	path := filepath.Join(owned.root, "synthetic-sentinel.bin")
	info, err := os.Lstat(path)
	if err == nil {
		err = cstSelectorCheckEntry(path, info)
	}
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("capture exact synthetic sentinel %q: %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, content) {
		t.Fatalf("synthetic sentinel content differs at creation %q: %v", path, err)
	}
	links, err := cstBindingAddedFileLinkCount(path)
	if err != nil || links != 1 {
		t.Fatalf("synthetic sentinel initial link count=%d want=1: %v", links, err)
	}
	owned.sentinel, owned.sentinelInfo = path, info
	owned.sentinelSHA = fmt.Sprintf("%x", sha256.Sum256(data))
}

func (owned *cstSelectorHardlinkOutside) checkSentinel(wantLinks uint32) error {
	if owned.sentinel == "" || owned.sentinelInfo == nil {
		return errors.New("sentinel identity unavailable")
	}
	info, err := os.Lstat(owned.sentinel)
	if err != nil {
		return err
	}
	if err := cstSelectorCheckEntry(owned.sentinel, info); err != nil || !info.Mode().IsRegular() || !os.SameFile(owned.sentinelInfo, info) {
		return fmt.Errorf("sentinel identity changed: %v", err)
	}
	data, err := os.ReadFile(owned.sentinel)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != owned.sentinelSHA {
		return fmt.Errorf("sentinel content changed: %v", err)
	}
	links, err := cstBindingAddedFileLinkCount(owned.sentinel)
	if err != nil || links != wantLinks {
		return fmt.Errorf("sentinel link count=%d want=%d: %v", links, wantLinks, err)
	}
	return nil
}

func (owned *cstSelectorHardlinkOutside) observeProduct() {
	if owned.workspace == nil || !owned.workspace.settled || !owned.workspace.productExists || len(owned.workspace.reasons) == 0 || owned.workspace.link == nil || owned.workspace.link.target != owned.sentinel {
		owned.observationErr = errors.New("workspace quarantine and exact hardlink were not observed")
		return
	}
	owned.observationErr = owned.checkSentinel(2)
	owned.observedTwo = owned.observationErr == nil
}

func (owned *cstSelectorHardlinkOutside) cleanup(t *testing.T) {
	t.Helper()
	block := func(kind string, err error) {
		t.Errorf("TEST_CLEANUP_BLOCKED:%s exact_root=%q: %v", kind, owned.root, err)
	}
	if owned.workspaceCreatedPath != "" {
		if owned.workspace == nil || owned.workspace.root != owned.workspaceCreatedPath || !owned.workspace.cleaned {
			block("outside_dependency", fmt.Errorf("workspace root=%q bound=%v cleaned=%v", owned.workspaceCreatedPath, owned.workspace != nil && owned.workspace.root == owned.workspaceCreatedPath, owned.workspace != nil && owned.workspace.cleaned))
			return
		}
		if _, err := os.Lstat(owned.workspaceCreatedPath); !errors.Is(err, os.ErrNotExist) {
			block("outside_dependency", fmt.Errorf("workspace root=%q not proved absent: %v", owned.workspaceCreatedPath, err))
			return
		}
		if owned.sentinelInfo != nil && !owned.observedTwo {
			block("outside_dependency", fmt.Errorf("two-link Product observation missing: %v", owned.observationErr))
			return
		}
	}
	if err := owned.checkIdentity(); err != nil {
		block("identity", err)
		return
	}
	entries, err := os.ReadDir(owned.root)
	if err != nil || len(entries) != 0 && (len(entries) != 1 || owned.sentinel == "" || entries[0].Name() != filepath.Base(owned.sentinel)) || len(entries) == 0 && owned.sentinel != "" {
		block("identity", fmt.Errorf("outside root not sentinel-only or proved empty: entries=%v err=%v", entries, err))
		return
	}
	if owned.sentinel != "" {
		if err := owned.checkSentinel(1); err != nil {
			block("target", err)
			return
		}
	}
	if err := owned.checkIdentity(); err != nil {
		block("identity", err)
		return
	}
	if owned.sentinel != "" {
		if err := os.Remove(owned.sentinel); err != nil {
			block("delete", err)
			return
		}
		if _, err := os.Lstat(owned.sentinel); !errors.Is(err, os.ErrNotExist) {
			block("delete", fmt.Errorf("sentinel remains: %v", err))
			return
		}
	}
	if err := os.Remove(owned.root); err != nil {
		block("delete", err)
		return
	}
	if _, err := os.Lstat(owned.root); !errors.Is(err, os.ErrNotExist) {
		block("delete", fmt.Errorf("outside root remains: %v", err))
		return
	}
	t.Logf("TEST_CLEANUP_REMOVED_OUTSIDE exact_root=%q", owned.root)
}

func slicesContainsPrefix(values []string, prefix string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// A capture failure must not let the outside owner remove its sentinel while
// the just-created workspace is still retained by its cleanup callback.
func TestCSTSelectorHardlinkCaptureFailurePreservesOutside(t *testing.T) {
	cstProbeTestPython(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	probeRoot, err := os.MkdirTemp("", "cst-selector-hardlink-capture-probe-")
	if err != nil {
		t.Fatal(err)
	}
	probeInfo, err := os.Lstat(probeRoot)
	if err != nil || !probeInfo.IsDir() || cstSelectorCheckEntry(probeRoot, probeInfo) != nil {
		t.Fatalf("TEST_CLEANUP_BLOCKED:identity exact_root=%q: %v", probeRoot, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestCSTSavedFieldSelectorBindingSynthetic/copied-added-hardlink-outside-quarantined$", "-test.v=true")
	cmd.Env = append(os.Environ(), "TMP="+probeRoot, "TEMP="+probeRoot, "MCPHUB_CST_SELECTOR_CAPTURE_FAULT=1")
	output, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("TEST_CLEANUP_BLOCKED:unsettled exact_root=%q child=%v output=%s", probeRoot, ctx.Err(), output)
	}
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("capture-fault child exit=%v output=%s", runErr, output)
	}
	entries, err := os.ReadDir(probeRoot)
	if err != nil {
		t.Fatalf("TEST_CLEANUP_BLOCKED:identity exact_root=%q: %v", probeRoot, err)
	}
	var workspace, outsideRoot string
	for _, entry := range entries {
		path := filepath.Join(probeRoot, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.IsDir() || cstSelectorCheckEntry(path, info) != nil {
			t.Fatalf("TEST_CLEANUP_BLOCKED:identity exact_root=%q: %v", path, statErr)
		}
		switch {
		case strings.HasPrefix(entry.Name(), "cst-selector-binding-synthetic-") && workspace == "":
			workspace = path
		case strings.HasPrefix(entry.Name(), "cst-selector-hardlink-outside-") && outsideRoot == "":
			outsideRoot = path
		default:
			t.Fatalf("TEST_CLEANUP_BLOCKED:identity unexpected root=%q", path)
		}
	}
	workspaceRetained, outsideRetained := workspace != "", outsideRoot != ""
	sentinelIntact := false
	if workspaceRetained {
		children, readErr := os.ReadDir(workspace)
		if readErr != nil || len(children) != 0 {
			t.Fatalf("TEST_CLEANUP_BLOCKED:identity exact_root=%q children=%v err=%v", workspace, children, readErr)
		}
		if err := os.Remove(workspace); err != nil {
			t.Fatalf("TEST_CLEANUP_BLOCKED:delete exact_root=%q: %v", workspace, err)
		}
	}
	if outsideRetained {
		children, readErr := os.ReadDir(outsideRoot)
		sentinel := filepath.Join(outsideRoot, "synthetic-sentinel.bin")
		info, statErr := os.Lstat(sentinel)
		data, dataErr := os.ReadFile(sentinel)
		links, linkErr := cstBindingAddedFileLinkCount(sentinel)
		if readErr != nil || len(children) != 1 || children[0].Name() != "synthetic-sentinel.bin" || statErr != nil || !info.Mode().IsRegular() || cstSelectorCheckEntry(sentinel, info) != nil || dataErr != nil || !bytes.Equal(data, []byte("synthetic external hardlink sentinel")) || linkErr != nil || links != 1 {
			t.Fatalf("TEST_CLEANUP_BLOCKED:target exact_root=%q entries=%v stat=%v read=%v links=%d link_err=%v", outsideRoot, children, statErr, dataErr, links, linkErr)
		}
		sentinelIntact = true
		if err := os.Remove(sentinel); err != nil {
			t.Fatalf("TEST_CLEANUP_BLOCKED:delete exact_root=%q: %v", sentinel, err)
		}
		if err := os.Remove(outsideRoot); err != nil {
			t.Fatalf("TEST_CLEANUP_BLOCKED:delete exact_root=%q: %v", outsideRoot, err)
		}
	}
	if current, statErr := os.Lstat(probeRoot); statErr != nil || !os.SameFile(probeInfo, current) {
		t.Fatalf("TEST_CLEANUP_BLOCKED:identity exact_root=%q: %v", probeRoot, statErr)
	}
	if err := os.Remove(probeRoot); err != nil {
		t.Fatalf("TEST_CLEANUP_BLOCKED:delete exact_root=%q: %v", probeRoot, err)
	}
	if _, statErr := os.Lstat(probeRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("TEST_CLEANUP_BLOCKED:delete exact_root=%q remains: %v", probeRoot, statErr)
	}
	t.Logf("capture-fault exact workspace=%q outside=%q sentinel_intact=%v child_output=%s", workspace, outsideRoot, sentinelIntact, output)
	if !workspaceRetained || !outsideRetained || !sentinelIntact || !bytes.Contains(output, []byte(fmt.Sprintf("TEST_CLEANUP_BLOCKED:unsettled exact_root=%q", workspace))) || !bytes.Contains(output, []byte(fmt.Sprintf("TEST_CLEANUP_BLOCKED:outside_dependency exact_root=%q", outsideRoot))) || bytes.Contains(output, []byte("TEST_CLEANUP_REMOVED_OUTSIDE")) {
		t.Errorf("capture-failure dependency not preserved: workspace=%v outside=%v sentinel=%v child_output=%s", workspaceRetained, outsideRetained, sentinelIntact, output)
	}
}

func TestCSTSavedFieldSelectorBindingSynthetic(t *testing.T) {
	python := cstProbeTestPython(t)
	cases := []struct {
		name, fault, wantFailure, traceFault string
		wantQuarantine, largeBundle          bool
		metadataDelta, blockedCopyDelta      bool
		copyAdditive                         bool
	}{
		{name: "four-exact-pairs"},
		{name: "copy-additive-regular", fault: "copy-additive-one", copyAdditive: true},
		{name: "copy-additive-alternate-name", fault: "copy-additive-alternate", copyAdditive: true},
		{name: "field-parents-with-surface-current-siblings", fault: "surface-current-siblings"},
		{name: "receipt-trace-missing-event", traceFault: "drop-select", wantQuarantine: true},
		{name: "receipt-trace-forbidden-name", traceFault: "rename-frequency", wantQuarantine: true},
		{name: "receipt-trace-failed-outcome", traceFault: "fail-frequency", wantQuarantine: true},
		{name: "receipt-trace-reordered-binding", traceFault: "reorder-binding", wantQuarantine: true},
		{name: "copy-metadata-delta-binding-pass", fault: "copy-metadata-delta", metadataDelta: true},
		{name: "selector-blocked-with-copy-metadata-delta", fault: "missing", wantFailure: "selector_payload_ambiguous", metadataDelta: true, blockedCopyDelta: true, wantQuarantine: true},
		{name: "full-251-project-plus-result-143", largeBundle: true},
		{name: "receipt-duplicate-selector", fault: "receipt-duplicate-selector", wantQuarantine: true},
		{name: "receipt-wrong-runid", fault: "receipt-wrong-runid", wantQuarantine: true},
		{name: "missing-selector", fault: "missing", wantFailure: "selector_payload_ambiguous"},
		{name: "duplicate-selector", fault: "duplicate-selector", wantFailure: "selector_payload_ambiguous"},
		{name: "wrong-filename", fault: "wrong-filename", wantFailure: "selector_payload_ambiguous"},
		{name: "duplicate-filename", fault: "duplicate-filename", wantFailure: "selector_payload_ambiguous"},
		{name: "swapped-filenames", fault: "swap", wantFailure: "selector_payload_ambiguous", wantQuarantine: true},
		{name: "outside-copy", fault: "outside-copy", wantFailure: "selector_payload_ambiguous"},
		{name: "wrong-rex-link", fault: "wrong-rex-link", wantFailure: "selector_payload_ambiguous"},
		{name: "wrong-rex-hash", fault: "wrong-rex-hash", wantFailure: "selector_payload_ambiguous"},
		{name: "wrong-m3d-hash", fault: "wrong-m3d-hash", wantFailure: "selector_payload_ambiguous"},
		{name: "false-select", fault: "false-selection", wantFailure: "selector_binding_api_unavailable"},
		{name: "wrong-frequency", fault: "bad-frequency", wantFailure: "selector_frequency_mismatch"},
		{name: "zero-frequency", fault: "zero-frequency", wantFailure: "selector_frequency_mismatch"},
		{name: "nonfinite-frequency", fault: "nan-frequency", wantFailure: "selector_frequency_mismatch"},
		{name: "forbidden-call", fault: "forbidden-call", wantFailure: "selector_payload_ambiguous"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
			if tc.metadataDelta {
				cstProbeWriteFile(t, filepath.Join(root, "source", "line10", "Result", "Storage.sdb"), "synthetic copy metadata before")
			}
			if tc.largeBundle {
				for index := 0; index < 134; index++ {
					cstProbeWriteFile(t, filepath.Join(root, "source", "line10", "Result", fmt.Sprintf("extra-%03d.bin", index)), "synthetic Result row")
				}
				for index := 0; index < 108; index++ {
					cstProbeWriteFile(t, filepath.Join(root, "source", "line10", "Model", fmt.Sprintf("extra-%03d.bin", index)), "synthetic Model row")
				}
			}
			if tc.fault == "wrong-rex-link" {
				rex := filepath.Join(root, "source", filepath.FromSlash(pairs[1].RexPath))
				content, err := os.ReadFile(rex)
				if err != nil {
					t.Fatal(err)
				}
				wrong := strings.Replace(string(content), "E_3GHz_2,1.m3d", "E_3GHz_1,1.m3d", 1)
				cstProbeWriteFile(t, rex, wrong)
				pairs[1].RexSHA256 = cstProbeFileSHA256(t, rex)
			}
			fakeRoot := cstBindingSyntheticInterface(t, root)
			full := cstGenerationFullManifest(t, project)
			resultManifest := cstBindingResultManifest(t, full, project)
			if tc.largeBundle && (len(full.Rows) != 251 || len(resultManifest.Rows) != 143 || resultManifest.Rows[0].Path != filepath.Base(project)) {
				t.Fatalf("synthetic project-plus-Result manifest: full=%d Result=%d first=%+v", len(full.Rows), len(resultManifest.Rows), resultManifest.Rows[0])
			}
			helperPath := cstBindingHelperPath(t)
			pythonSource := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "src")
			request := cstBindingRequest{Schema: cstBindingRequestSchema, SourceProject: project, ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, helperPath), ExpectedPythonSHA: cstProbeFileSHA256(t, python), FullManifest: full, ResultManifest: resultManifest, Pairs: pairs, ExpectedDirectories: cstBindingFrozenDirectories(t, project)}
			if tc.fault == "wrong-rex-hash" {
				request.Pairs[0].RexSHA256 = strings.Repeat("0", 64)
			}
			if tc.fault == "wrong-m3d-hash" {
				request.Pairs[0].M3DSHA256 = strings.Repeat("0", 64)
			}
			if tc.fault == "forbidden-call" {
				request.TestForbiddenCall = "GetFieldVector"
			}
			extra := map[string]string{"MCPHUB_CST_BINDING_SYNTHETIC": "1", "MCPHUB_CST_BINDING_FAULT": tc.fault}
			if tc.blockedCopyDelta {
				extra["MCPHUB_CST_BINDING_COPY_DELTA"] = "1"
			}
			extra["MCPHUB_CST_BINDING_OUTSIDE"] = filepath.Join(root, "source", filepath.FromSlash(pairs[0].RexPath))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			workspace, err := os.MkdirTemp("", "cst-selector-binding-synthetic-")
			if err != nil {
				t.Fatal(err)
			}
			owned := cstSelectorOwnSyntheticRoot(t, workspace)
			request.WorkspaceRoot = workspace
			receipt, output, err := cstBindingRun(ctx, python, helperPath, request, []string{fakeRoot, pythonSource}, extra)
			owned.runReturned = true
			traceMutated := false
			if tc.traceFault != "" && receipt.APITrace != nil {
				for index, event := range receipt.APITrace.Events {
					if tc.traceFault == "reorder-binding" && event.Name == "select-tree-item" && index+1 < len(receipt.APITrace.Events) && receipt.APITrace.Events[index+1].Name == "field-frequency" {
						receipt.APITrace.Events[index], receipt.APITrace.Events[index+1] = receipt.APITrace.Events[index+1], receipt.APITrace.Events[index]
						traceMutated = true
						break
					}
					if tc.traceFault == "drop-select" && event.Name == "select-tree-item" {
						receipt.APITrace.Events = append(append([]cstProbeAPIEvent(nil), receipt.APITrace.Events[:index]...), receipt.APITrace.Events[index+1:]...)
						traceMutated = true
						break
					}
					if event.Name == "field-frequency" && tc.traceFault == "rename-frequency" {
						receipt.APITrace.Events[index].Name = "export"
						traceMutated = true
						break
					}
					if event.Name == "field-frequency" && tc.traceFault == "fail-frequency" {
						receipt.APITrace.Events[index].Outcome = "error"
						traceMutated = true
						break
					}
				}
			}
			wantStatus := "PASS:selector_payload_bound"
			if tc.wantFailure != "" {
				wantStatus = "BLOCKED:" + tc.wantFailure
			}
			var summary cstBindingSummary
			reasons := cstBindingSettle(t, request, receipt, output, err, nil, wantStatus, "", "", os.TempDir(), nil, nil, &summary)
			owned.observe(reasons)
			if tc.copyAdditive {
				addedName := "synthetic-one.bin"
				if tc.fault == "copy-additive-alternate" {
					addedName = "other-format.dat"
				}
				wantAddedPath := filepath.ToSlash(filepath.Join("line10", "Result", addedName))
				if err != nil || receipt.Status != "PASS:selector_payload_bound" || len(receipt.Rows) != 4 || receipt.APITrace == nil || receipt.APITrace.Counts["result-tree-file"] != 4 || receipt.APITrace.Counts["select-tree-item"] != 4 || receipt.APITrace.Counts["field-frequency"] != 4 || !receipt.ClosedWithoutSave || !receipt.SessionSettled || receipt.CopyUnchanged || receipt.CopyManifestSHA256 == full.AggregateSHA256 || summary.CopyFullSHA != receipt.CopyManifestSHA256 || summary.CopyResultSHA != receipt.CopyResultSHA256 || len(summary.CopyDeltas) != 1 || summary.CopyDeltas[0].Path != pairs[0].RexPath || summary.CopyDeltas[0].Before.SHA256 == summary.CopyDeltas[0].After.SHA256 || len(summary.CopyAdded) != 1 || summary.CopyAdded[0].Path != wantAddedPath || summary.CopyAdded[0].Type != "regular" || summary.CopyAdded[0].Size == 0 || summary.Disposition != "delete_eligible" || len(reasons) != 0 || cstBindingRootExists(workspace) {
					t.Fatalf("additive copied side effect not safely settled: fault=%s root=%q exists=%v receipt=%+v summary=%+v reasons=%v", tc.fault, workspace, cstBindingRootExists(workspace), receipt, summary, reasons)
				}
				return
			}
			if tc.metadataDelta && !tc.blockedCopyDelta {
				if receipt.Status != "PASS:selector_payload_bound" || len(receipt.Rows) != 4 || receipt.APITrace == nil || !receipt.ClosedWithoutSave || !receipt.SessionSettled || receipt.CopyUnchanged || receipt.CopyManifestSHA256 == full.AggregateSHA256 || len(summary.CopyDeltas) != 1 || summary.CopyDeltas[0].Path != "line10/Result/Storage.sdb" || summary.CopyDeltas[0].Before.SHA256 == summary.CopyDeltas[0].After.SHA256 || summary.CopyFullSHA != receipt.CopyManifestSHA256 || summary.Disposition != "delete_eligible" || len(reasons) != 0 || cstBindingRootExists(workspace) {
					t.Fatalf("nonprotected copy delta lost binding or cleanup proof: receipt=%+v summary=%+v owned_root=%q exists=%v reasons=%v", receipt, summary, workspace, cstBindingRootExists(workspace), reasons)
				}
				return
			}
			preadmission := tc.fault == "wrong-rex-hash" || tc.fault == "wrong-m3d-hash"
			if tc.wantQuarantine || (tc.wantFailure != "" && !preadmission) {
				if tc.traceFault != "" && (!traceMutated || err != nil || receipt.Status != "PASS:selector_payload_bound" || len(receipt.Rows) != 4 || receipt.APITrace.Counts["result-tree-file"] != 4 || receipt.APITrace.Counts["select-tree-item"] != 4 || receipt.APITrace.Counts["field-frequency"] != 4) {
					t.Fatalf("synthetic trace falsifier setup failed; root=%q mutated=%v run=%v receipt=%+v", workspace, traceMutated, err, receipt)
				}
				wantReason := "receipt_invalid"
				if tc.wantFailure != "" {
					wantReason = "selector_blocked:" + tc.wantFailure
				}
				if tc.blockedCopyDelta && (receipt.Status != "BLOCKED:selector_payload_ambiguous" || len(receipt.Rows) != 0 || receipt.APITrace == nil || !receipt.ClosedWithoutSave || !receipt.SessionSettled || receipt.CopyUnchanged || receipt.CopyManifestSHA256 == full.AggregateSHA256 || len(summary.CopyDeltas) != 1 || summary.CopyDeltas[0].Path != "line10/Result/Storage.sdb" || summary.Disposition != "quarantine") {
					t.Fatalf("copy delta replaced selector failure: receipt=%+v owned_root=%q reasons=%v", receipt, workspace, reasons)
				}
				if !strings.Contains(strings.Join(reasons, ","), wantReason) || !cstBindingRootExists(workspace) {
					t.Fatalf("semantic receipt was allowed to delete exact owned root=%q exists=%v reasons=%v", workspace, cstBindingRootExists(workspace), reasons)
				}
				t.Logf("quarantined owned synthetic root=%q exists=true reasons=%v", workspace, reasons)
				return
			}
			if len(reasons) != 0 {
				t.Fatalf("binding settlement quarantined owned root=%q exists=%v reasons=%v", workspace, cstBindingRootExists(workspace), reasons)
			}
			if err != nil || output.Stdout.Truncated || output.Stderr.Truncated {
				t.Fatalf("binding run=%v stdout=%q stderr=%q", err, output.Stdout.Prefix, output.Stderr.Prefix)
			}
			if receipt.Schema != cstBindingReceiptSchema {
				t.Fatalf("wrong binding schema=%+v", receipt)
			}
			if !preadmission && (receipt.FullManifestSHA256 != full.AggregateSHA256 || receipt.ResultManifestSHA256 != resultManifest.AggregateSHA256 || receipt.CopyManifestSHA256 != full.AggregateSHA256 || receipt.CopyResultSHA256 != resultManifest.AggregateSHA256 || !receipt.SourceUnchanged || !receipt.CopyUnchanged || !receipt.ForeignPreserved || !receipt.SessionSettled || !receipt.ClosedWithoutSave) {
				t.Fatalf("incomplete binding receipt=%+v stderr=%q", receipt, output.Stderr.Prefix)
			}
			_ = preadmission // pre-admission emptiness and trace are checked before disposition.
			if tc.wantFailure == "" {
				if receipt.Status != "PASS:selector_payload_bound" || receipt.FailureID != "" || len(receipt.Rows) != 4 {
					t.Fatalf("four-pair receipt=%+v", receipt)
				}
				rowsByPair := make(map[string]cstBindingRow, len(receipt.Rows))
				for _, row := range receipt.Rows {
					key := fmt.Sprintf("%s:%d", row.Field, row.Port)
					if _, duplicate := rowsByPair[key]; duplicate {
						t.Fatalf("duplicate binding row for %s", key)
					}
					rowsByPair[key] = row
				}
				for _, pair := range pairs {
					key := fmt.Sprintf("%s:%d", pair.Field, pair.Port)
					row := rowsByPair[key]
					wantRunID := map[string]string{"E:1": "8", "E:2": "3", "H:1": "6", "H:2": "1"}[fmt.Sprintf("%s:%d", pair.Field, pair.Port)]
					wantSelector := fmt.Sprintf("2D/3D Results\\%s-Field\\mcp_line10_%s_3GHz [%s]", pair.Field, pair.Field, wantRunID)
					if row.Field != pair.Field || row.Port != pair.Port || row.Selector != wantSelector || row.RunID != wantRunID || row.ReturnedFile != pair.RexPath || row.M3DPath != pair.M3DPath || row.M3DSHA256 != pair.M3DSHA256 || row.RexPath != pair.RexPath || row.RexSHA256 != pair.RexSHA256 || row.RawFrequency != 3 {
						t.Fatalf("row[%s]=%+v want pair=%+v selector=%q", key, row, pair, wantSelector)
					}
				}
				if receipt.APITrace == nil || receipt.APITrace.Counts["result-tree-file"] != 4 || receipt.APITrace.Counts["select-tree-item"] != 4 || receipt.APITrace.Counts["field-frequency"] != 4 {
					t.Fatalf("trace=%+v", receipt.APITrace)
				}
			} else if receipt.Status != "BLOCKED:"+tc.wantFailure || receipt.FailureID != tc.wantFailure || len(receipt.Rows) != 0 {
				t.Fatalf("fault=%s receipt=%+v", tc.fault, receipt)
			}
			if receipt.APITrace != nil {
				for _, name := range []string{"solve", "remesh", "history", "export", "project-save", "3DSave", "GetResultFromTreeItem", "Result3D", "registration"} {
					if receipt.APITrace.Counts[name] != 0 {
						t.Fatalf("forbidden %s trace=%+v", name, receipt.APITrace)
					}
				}
				if tc.fault == "forbidden-call" && receipt.APITrace.Counts["GetFieldVector"] != 1 {
					t.Fatalf("forbidden-call guard not exercised: %+v", receipt.APITrace)
				}
			}
			if cstBindingRootExists(workspace) {
				t.Fatalf("settled synthetic copy remains: %q", workspace)
			}
		})
	}
	t.Run("copied-added-hardlink-outside-quarantined", func(t *testing.T) {
		root := t.TempDir()
		project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
		fakeRoot := cstBindingSyntheticInterface(t, root)
		full := cstGenerationFullManifest(t, project)
		helperPath := cstBindingHelperPath(t)
		pythonSource := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "src")
		request := cstBindingRequest{Schema: cstBindingRequestSchema, SourceProject: project, ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, helperPath), ExpectedPythonSHA: cstProbeFileSHA256(t, python), FullManifest: full, ResultManifest: cstBindingResultManifest(t, full, project), Pairs: pairs, ExpectedDirectories: cstBindingFrozenDirectories(t, project)}
		outsideRoot, err := os.MkdirTemp("", "cst-selector-hardlink-outside-")
		if err != nil {
			t.Fatal(err)
		}
		outsideOwned := cstSelectorOwnHardlinkOutside(t, outsideRoot)
		outside := filepath.Join(outsideRoot, "synthetic-sentinel.bin")
		sentinelContent := []byte("synthetic external hardlink sentinel")
		if err := os.WriteFile(outside, sentinelContent, 0o600); err != nil {
			t.Fatalf("create exact external sentinel root=%q: %v", outsideRoot, err)
		}
		outsideOwned.captureSentinel(t, sentinelContent)
		outsideBefore, err := os.Stat(outside)
		if err != nil {
			t.Fatal(err)
		}
		outsideSHA := cstProbeFileSHA256(t, outside)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		workspace, err := os.MkdirTemp("", "cst-selector-binding-synthetic-")
		if err != nil {
			t.Fatal(err)
		}
		outsideOwned.workspaceCreatedPath = workspace
		if os.Getenv("MCPHUB_CST_SELECTOR_CAPTURE_FAULT") == "1" {
			cstSelectorOwnSyntheticRoot(t, workspace+string(os.PathSeparator))
		}
		owned := cstSelectorOwnSyntheticRoot(t, workspace)
		outsideOwned.workspace = owned
		request.WorkspaceRoot = workspace
		volumeSame := strings.EqualFold(filepath.VolumeName(outside), filepath.VolumeName(workspace))
		receipt, output, runErr := cstBindingRun(ctx, python, helperPath, request, []string{fakeRoot, pythonSource}, map[string]string{"MCPHUB_CST_BINDING_SYNTHETIC": "1"})
		owned.runReturned = true
		copied := filepath.Join(workspace, "bundle", filepath.Base(project))
		copyFull, fullErr := cstBindingFullManifest(copied)
		copyResult, resultErr := cstBindingResultInventory(copied)
		var setupReasons []string
		if fullErr != nil || resultErr != nil {
			setupReasons = append(setupReasons, fmt.Sprintf("copy_inventory_invalid:synthetic_prelink_manifest full=%v Result=%v", fullErr, resultErr))
		} else {
			receipt.CopyManifestSHA256 = copyFull.AggregateSHA256
			receipt.CopyResultSHA256 = copyResult.AggregateSHA256
			encoded, marshalErr := json.Marshal(receipt)
			if marshalErr != nil {
				setupReasons = append(setupReasons, "receipt_invalid:synthetic_rebind="+marshalErr.Error())
			} else {
				output.Stdout.Prefix = encoded
				output.Stdout.Bytes = len(encoded)
			}
		}
		linked := filepath.Join(workspace, "bundle", "line10", "Result", "synthetic-linked.dat")
		if err := os.Link(outside, linked); err != nil {
			setupReasons = append(setupReasons, "copy_inventory_invalid:synthetic_link="+err.Error())
		} else {
			owned.allowLink(t, linked, outside, "hardlink")
		}
		linkInfo, linkErr := os.Lstat(linked)
		var summary cstBindingSummary
		reasons := cstBindingSettle(t, request, receipt, output, runErr, nil, "PASS:selector_payload_bound", "", "", os.TempDir(), nil, setupReasons, &summary)
		owned.observe(reasons)
		outsideAfter, outsideErr := os.Stat(outside)
		outsideBytes, readErr := os.ReadFile(outside)
		outsideLinks, linkCountErr := cstBindingAddedFileLinkCount(outside)
		outsideSame := outsideErr == nil && os.SameFile(outsideBefore, outsideAfter)
		outsideHashSame := readErr == nil && fmt.Sprintf("%x", sha256.Sum256(outsideBytes)) == outsideSHA
		outsideOwned.observeProduct()
		if !volumeSame || runErr != nil || len(setupReasons) != 0 || receipt.Status != "PASS:selector_payload_bound" || len(receipt.Rows) != 4 || receipt.APITrace == nil || receipt.APITrace.Counts["result-tree-file"] != 4 || receipt.APITrace.Counts["select-tree-item"] != 4 || receipt.APITrace.Counts["field-frequency"] != 4 || !receipt.ClosedWithoutSave || !receipt.SessionSettled || receipt.CopyManifestSHA256 != copyFull.AggregateSHA256 || receipt.CopyResultSHA256 != copyResult.AggregateSHA256 || linkErr != nil || !linkInfo.Mode().IsRegular() || !os.SameFile(linkInfo, outsideBefore) || !outsideSame || !outsideHashSame || linkCountErr != nil || outsideLinks != 2 || !strings.Contains(strings.Join(reasons, ","), "copy_inventory_invalid:linked_added_row") || !cstBindingRootExists(workspace) {
			t.Fatalf("added hardlink not quarantined: root=%q exists=%v outside=%q same_volume=%v run=%v setup=%v linked=%v same_external=%v hash_same=%v outside_links=%d link_count_err=%v reasons=%v", workspace, cstBindingRootExists(workspace), outsideRoot, volumeSame, runErr, setupReasons, linkErr, outsideSame, outsideHashSame, outsideLinks, linkCountErr, reasons)
		}
		t.Logf("quarantined owned hardlink copy=%q outside_root=%q identity_preserved=true", workspace, outsideRoot)
	})
	for _, tc := range []struct {
		name, fault, branch                       string
		total, candidate                          int // candidate -1 means JSON null.
		truncated, redacted, reserveLast, byteCap bool
	}{
		{name: "diagnostic-traversal-invalid", fault: "duplicate-selector", branch: "traversal_invalid", total: 5, candidate: -1},
		{name: "diagnostic-candidate-count", fault: "missing", branch: "candidate_count_mismatch", total: 3, candidate: 3},
		{name: "diagnostic-parent-segment", fault: "wrong-parent", branch: "parent_segment_mismatch", total: 4, candidate: 4},
		{name: "diagnostic-all-wrong-parents", fault: "all-wrong-parents", branch: "parent_segment_mismatch", total: 4, candidate: 4},
		{name: "diagnostic-ancestor-only-parent", fault: "ancestor-only-parent", branch: "parent_segment_mismatch", total: 4, candidate: 4},
		{name: "diagnostic-five-eligible", fault: "five-eligible", branch: "candidate_count_mismatch", total: 5, candidate: 5},
		{name: "diagnostic-late-parent", fault: "late-wrong-parent", branch: "parent_segment_mismatch", total: 40, candidate: 4, truncated: true, reserveLast: true},
		{name: "diagnostic-invalid-return", fault: "invalid-return", branch: "traversal_invalid", total: 1, candidate: -1, redacted: true},
		{name: "diagnostic-unsafe-absolute", fault: "unsafe-traversal", branch: "traversal_invalid", total: 1, candidate: -1, redacted: true},
		{name: "diagnostic-embedded-drive", fault: "embedded-drive", branch: "candidate_count_mismatch", total: 1, candidate: 0, redacted: true},
		{name: "diagnostic-embedded-unc", fault: "embedded-unc", branch: "candidate_count_mismatch", total: 1, candidate: 0, redacted: true},
		{name: "diagnostic-many-labels", fault: "many-labels", branch: "candidate_count_mismatch", total: 40, candidate: 40, truncated: true},
		{name: "diagnostic-json-byte-cap", fault: "unicode-labels", branch: "candidate_count_mismatch", total: 40, candidate: 0, truncated: true, byteCap: true},
		{name: "diagnostic-overlong-label", fault: "overlong-label", branch: "candidate_count_mismatch", total: 5, candidate: 5, truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
			fakeRoot := cstBindingSyntheticInterface(t, root)
			full := cstGenerationFullManifest(t, project)
			helperPath := cstBindingHelperPath(t)
			pythonSource := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "src")
			request := cstBindingRequest{Schema: cstBindingRequestSchema, SourceProject: project, ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, helperPath), ExpectedPythonSHA: cstProbeFileSHA256(t, python), FullManifest: full, ResultManifest: cstBindingResultManifest(t, full, project), Pairs: pairs, ExpectedDirectories: cstBindingFrozenDirectories(t, project)}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			workspace, err := os.MkdirTemp("", "cst-selector-binding-synthetic-")
			if err != nil {
				t.Fatal(err)
			}
			owned := cstSelectorOwnSyntheticRoot(t, workspace)
			request.WorkspaceRoot = workspace
			receipt, output, runErr := cstBindingRun(ctx, python, helperPath, request, []string{fakeRoot, pythonSource}, map[string]string{"MCPHUB_CST_BINDING_SYNTHETIC": "1", "MCPHUB_CST_BINDING_FAULT": tc.fault})
			owned.runReturned = true
			var summary cstBindingSummary
			reasons := cstBindingSettle(t, request, receipt, output, runErr, nil, "BLOCKED:selector_payload_ambiguous", "", "", os.TempDir(), nil, nil, &summary)
			owned.observe(reasons)
			var wire map[string]json.RawMessage
			decodeErr := json.Unmarshal(output.Stdout.Prefix, &wire)
			rawDiagnostic := wire["selector_diagnostic"]
			var diagnostic struct {
				Branch         string `json:"branch"`
				TotalCount     int    `json:"total_count"`
				CandidateCount *int   `json:"candidate_count"`
				Labels         []struct {
					Parent string `json:"parent"`
					Leaf   string `json:"leaf"`
				} `json:"labels"`
				Truncated bool `json:"truncated"`
			}
			diagnosticErr := json.Unmarshal(rawDiagnostic, &diagnostic)
			if runErr != nil || decodeErr != nil || len(rawDiagnostic) == 0 || diagnosticErr != nil || len(rawDiagnostic) > 8<<10 || receipt.Status != "BLOCKED:selector_payload_ambiguous" || receipt.FailureID != "selector_payload_ambiguous" || len(receipt.Rows) != 0 || receipt.APITrace == nil || receipt.APITrace.Counts["result-tree-file"] != 0 || receipt.APITrace.Counts["select-tree-item"] != 0 || receipt.APITrace.Counts["field-frequency"] != 0 || !receipt.ClosedWithoutSave || !receipt.SessionSettled || diagnostic.Branch != tc.branch || diagnostic.TotalCount != tc.total || (tc.candidate == -1 && diagnostic.CandidateCount != nil) || (tc.candidate >= 0 && (diagnostic.CandidateCount == nil || *diagnostic.CandidateCount != tc.candidate)) || diagnostic.Truncated != tc.truncated || len(diagnostic.Labels) > 32 || summary.SelectorDiagnostic == nil || summary.SelectorDiagnostic.Branch != tc.branch || summary.SelectorDiagnostic.TotalCount != tc.total || !strings.Contains(strings.Join(reasons, ","), "selector_blocked:selector_payload_ambiguous") || !cstBindingRootExists(workspace) {
				t.Fatalf("pre-file diagnostic contract: case=%s diagnostic=%+v raw=%q root=%q exists=%v run=%v decode=%v diagnosticErr=%v reasons=%v", tc.name, diagnostic, rawDiagnostic, workspace, cstBindingRootExists(workspace), runErr, decodeErr, diagnosticErr, reasons)
			}
			if tc.redacted && (len(diagnostic.Labels) == 0 || diagnostic.Labels[0].Parent != "<redacted>" || diagnostic.Labels[0].Leaf != "<redacted>" || summary.SelectorDiagnostic == nil || len(summary.SelectorDiagnostic.Labels) == 0 || summary.SelectorDiagnostic.Labels[0].Parent != "<redacted>" || summary.SelectorDiagnostic.Labels[0].Leaf != "<redacted>") {
				t.Fatalf("unsafe selector label escaped redaction: %+v", diagnostic.Labels)
			}
			if tc.total > 32 && ((!tc.byteCap && len(diagnostic.Labels) != 32) || (tc.byteCap && len(diagnostic.Labels) >= 32)) {
				t.Fatalf("sample bound=%d byteCap=%v", len(diagnostic.Labels), tc.byteCap)
			}
			if tc.reserveLast && (len(diagnostic.Labels) != 32 || diagnostic.Labels[31].Parent != "Wrong-Field" || diagnostic.Labels[31].Leaf != "mcp_line10_E_3GHz [8]") {
				t.Fatalf("offending candidate not retained in final sample slot: %+v", diagnostic.Labels)
			}
			for _, label := range diagnostic.Labels {
				if len([]byte(label.Parent)) > 96 || len([]byte(label.Leaf)) > 96 {
					t.Fatalf("unbounded safe label: %+v", label)
				}
			}
			t.Logf("quarantined owned synthetic root=%q exists=true branch=%s count=%d", workspace, diagnostic.Branch, diagnostic.TotalCount)
		})
	}
	for _, tamper := range []string{"missing", "unsafe", "oversized", "too-many", "forged", "duplicate-field"} {
		t.Run("diagnostic-forged-"+tamper, func(t *testing.T) {
			root := t.TempDir()
			project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
			fakeRoot := cstBindingSyntheticInterface(t, root)
			full := cstGenerationFullManifest(t, project)
			helperPath := cstBindingHelperPath(t)
			pythonSource := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "src")
			request := cstBindingRequest{Schema: cstBindingRequestSchema, SourceProject: project, ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, helperPath), ExpectedPythonSHA: cstProbeFileSHA256(t, python), FullManifest: full, ResultManifest: cstBindingResultManifest(t, full, project), Pairs: pairs, ExpectedDirectories: cstBindingFrozenDirectories(t, project)}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			workspace, err := os.MkdirTemp("", "cst-selector-binding-synthetic-")
			if err != nil {
				t.Fatal(err)
			}
			owned := cstSelectorOwnSyntheticRoot(t, workspace)
			request.WorkspaceRoot = workspace
			receipt, output, runErr := cstBindingRun(ctx, python, helperPath, request, []string{fakeRoot, pythonSource}, map[string]string{"MCPHUB_CST_BINDING_SYNTHETIC": "1", "MCPHUB_CST_BINDING_FAULT": "missing"})
			owned.runReturned = true
			var envelope map[string]json.RawMessage
			tamperErr := json.Unmarshal(output.Stdout.Prefix, &envelope)
			if tamperErr == nil {
				if tamper == "missing" {
					delete(envelope, "selector_diagnostic")
				} else {
					labels := []map[string]string{{"parent": "E-Field", "leaf": "mcp_line10_E_3GHz [8]"}, {"parent": "E-Field", "leaf": "mcp_line10_E_3GHz [3]"}, {"parent": "H-Field", "leaf": "mcp_line10_H_3GHz [6]"}}
					branch, total, candidate, truncated := "candidate_count_mismatch", 3, 3, false
					switch tamper {
					case "unsafe":
						labels[0] = map[string]string{"parent": `Z:\synthetic-private`, "leaf": `..\secret`}
					case "oversized":
						labels[0]["leaf"] = strings.Repeat("A", 9000)
					case "too-many":
						branch, total, candidate, truncated = "candidate_count_mismatch", 40, 40, true
						labels = make([]map[string]string, 33)
						for index := range labels {
							labels[index] = map[string]string{"parent": "E-Field", "leaf": fmt.Sprintf("safe-%d", index)}
						}
					case "forged":
						branch = "parent_segment_mismatch" // This branch requires exactly four candidates.
					}
					var raw []byte
					raw, tamperErr = json.Marshal(map[string]any{"branch": branch, "total_count": total, "candidate_count": candidate, "labels": labels, "truncated": truncated})
					if tamper == "duplicate-field" && tamperErr == nil {
						raw = append([]byte(`{"branch":"candidate_count_mismatch",`), raw[1:]...)
					}
					if tamperErr == nil {
						envelope["selector_diagnostic"] = raw
					}
				}
			}
			if tamperErr == nil {
				output.Stdout.Prefix, tamperErr = json.Marshal(envelope)
				output.Stdout.Bytes = len(output.Stdout.Prefix)
			}
			if tamperErr == nil {
				tamperErr = json.Unmarshal(output.Stdout.Prefix, &receipt)
			}
			var priorReasons []string
			if tamperErr != nil {
				priorReasons = append(priorReasons, "receipt_invalid:synthetic_tamper="+tamperErr.Error())
			}
			var summary cstBindingSummary
			reasons := cstBindingSettle(t, request, receipt, output, runErr, nil, "BLOCKED:selector_payload_ambiguous", "", "", os.TempDir(), nil, priorReasons, &summary)
			owned.observe(reasons)
			if runErr != nil || tamperErr != nil || summary.SelectorDiagnostic != nil || !strings.Contains(strings.Join(reasons, ","), "receipt_invalid:selector_diagnostic") || !cstBindingRootExists(workspace) {
				t.Fatalf("forged diagnostic accepted: fault=%s root=%q exists=%v run=%v tamper=%v reasons=%v", tamper, workspace, cstBindingRootExists(workspace), runErr, tamperErr, reasons)
			}
			t.Logf("quarantined owned synthetic root=%q exists=true diagnostic_fault=%s", workspace, tamper)
		})
	}
	t.Run("full-inventory-error-still-observes-result-and-protected-payloads", func(t *testing.T) {
		root := t.TempDir()
		project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
		modelRelative := filepath.Join("line10", "Model", "model.bin")
		cstProbeWriteFile(t, filepath.Join(root, "source", modelRelative), "synthetic model row")
		resultRelative := filepath.Join("line10", "Result", "extra.bin")
		cstProbeWriteFile(t, filepath.Join(root, "source", resultRelative), "synthetic Result row")
		fakeRoot := cstBindingSyntheticInterface(t, root)
		full := cstGenerationFullManifest(t, project)
		helperPath := cstBindingHelperPath(t)
		pythonSource := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "src")
		request := cstBindingRequest{Schema: cstBindingRequestSchema, SourceProject: project, ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, helperPath), ExpectedPythonSHA: cstProbeFileSHA256(t, python), FullManifest: full, ResultManifest: cstBindingResultManifest(t, full, project), Pairs: pairs, ExpectedDirectories: cstBindingFrozenDirectories(t, project)}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		workspace, err := os.MkdirTemp("", "cst-selector-binding-synthetic-")
		if err != nil {
			t.Fatal(err)
		}
		owned := cstSelectorOwnSyntheticRoot(t, workspace)
		request.WorkspaceRoot = workspace
		receipt, output, runErr := cstBindingRun(ctx, python, helperPath, request, []string{fakeRoot, pythonSource}, map[string]string{"MCPHUB_CST_BINDING_SYNTHETIC": "1"})
		owned.runReturned = true
		copiedBase := filepath.Join(workspace, "bundle")
		linkedRow := filepath.Join(copiedBase, modelRelative)
		var injectionErrors []string
		if err := os.Remove(linkedRow); err != nil {
			injectionErrors = append(injectionErrors, "remove non-pair row: "+err.Error())
		} else if err := os.Symlink(filepath.Join(root, "source", modelRelative), linkedRow); err != nil {
			injectionErrors = append(injectionErrors, "link non-pair row: "+err.Error())
		} else {
			owned.allowLink(t, linkedRow, filepath.Join(root, "source", modelRelative), "symlink")
		}
		if err := os.WriteFile(filepath.Join(copiedBase, resultRelative), []byte("changed Result row"), 0o600); err != nil {
			injectionErrors = append(injectionErrors, "change Result row: "+err.Error())
		}
		for _, pair := range pairs {
			relative := pair.M3DPath
			path := filepath.Join(copiedBase, filepath.FromSlash(relative))
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				injectionErrors = append(injectionErrors, "open protected payload "+relative+": "+err.Error())
				continue
			}
			_, writeErr := file.Write([]byte("synthetic-drift"))
			if err := errors.Join(writeErr, file.Close()); err != nil {
				injectionErrors = append(injectionErrors, "change protected payload "+relative+": "+err.Error())
			}
		}
		reasons := cstBindingSettle(t, request, receipt, output, runErr, nil, "PASS:selector_payload_bound", "", "", os.TempDir(), nil, injectionErrors, nil)
		owned.observe(reasons)
		if len(injectionErrors) != 0 {
			t.Fatalf("synthetic fault setup failed; owned root=%q exists=%v errors=%v reasons=%v", workspace, cstBindingRootExists(workspace), injectionErrors, reasons)
		}
		reasonText := strings.Join(reasons, ",")
		if runErr != nil || !strings.Contains(reasonText, "copy_inventory_invalid:full=") || !strings.Contains(reasonText, "copy_inventory_invalid:result_observed=") || !cstBindingRootExists(workspace) {
			t.Fatalf("degraded inventory skipped independent evidence; owned root=%q exists=%v run=%v reasons=%v", workspace, cstBindingRootExists(workspace), runErr, reasons)
		}
		for _, pair := range pairs {
			if !strings.Contains(reasonText, "copy_protected_drift:row="+pair.M3DPath) {
				t.Fatalf("degraded inventory skipped protected payload=%q; owned root=%q reasons=%v", pair.M3DPath, workspace, reasons)
			}
		}
		t.Logf("quarantined owned synthetic root=%q exists=true reasons=%v", workspace, reasons)
	})
	t.Run("protected-reparse-after-degraded-full-refused-before-read", func(t *testing.T) {
		root := t.TempDir()
		project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
		fakeRoot := cstBindingSyntheticInterface(t, root)
		full := cstGenerationFullManifest(t, project)
		helperPath := cstBindingHelperPath(t)
		pythonSource := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "src")
		request := cstBindingRequest{Schema: cstBindingRequestSchema, SourceProject: project, ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, helperPath), ExpectedPythonSHA: cstProbeFileSHA256(t, python), FullManifest: full, ResultManifest: cstBindingResultManifest(t, full, project), Pairs: pairs, ExpectedDirectories: cstBindingFrozenDirectories(t, project)}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		workspace, err := os.MkdirTemp("", "cst-selector-binding-synthetic-")
		if err != nil {
			t.Fatal(err)
		}
		owned := cstSelectorOwnSyntheticRoot(t, workspace)
		request.WorkspaceRoot = workspace
		receipt, output, runErr := cstBindingRun(ctx, python, helperPath, request, []string{fakeRoot, pythonSource}, map[string]string{"MCPHUB_CST_BINDING_SYNTHETIC": "1"})
		owned.runReturned = true
		settled := false
		defer func() {
			if !settled {
				owned.observe(cstBindingSettle(t, request, receipt, output, runErr, nil, "PASS:selector_payload_bound", "", "", os.TempDir(), nil, nil, nil))
			}
		}()
		copied := filepath.Join(workspace, "bundle", filepath.Base(project))
		protectedRelative := pairs[0].M3DPath
		protected := filepath.Join(filepath.Dir(copied), filepath.FromSlash(protectedRelative))
		controlReads := 0
		if _, err := cstFingerprintFile(ctx, filepath.Dir(copied), protectedRelative, 1<<20, &cstFingerprintHooks{BeforeRead: func() error { controlReads++; return nil }}); err != nil || controlReads != 1 {
			t.Fatalf("regular protected row did not exercise pre-read witness: reads=%d err=%v", controlReads, err)
		}
		outside := filepath.Join(root, "outside-synthetic.bin")
		if err := os.WriteFile(outside, []byte("test-owned outside sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(protected); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, protected); err != nil {
			t.Fatal(err)
		}
		owned.allowLink(t, protected, outside, "protected-symlink")
		reparseReads := 0
		_, err = cstFingerprintFile(ctx, filepath.Dir(copied), protectedRelative, 1<<20, &cstFingerprintHooks{BeforeRead: func() error { reparseReads++; return nil }})
		var unsafeRow *cstFingerprintError
		if !errors.As(err, &unsafeRow) || (unsafeRow.Kind != "reparse" && unsafeRow.Kind != "leaf_open") || unsafeRow.DataReadEntered || reparseReads != 0 {
			t.Fatalf("protected reparse lacked typed pre-read refusal: reads=%d err=%v", reparseReads, err)
		}
		safeReads, outsideReads := 0, 0
		evidence := cstBindingObserveCopyWithHooks(copied, full, request.ExpectedDirectories, pairs, func(relative string) *cstFingerprintHooks {
			return &cstFingerprintHooks{BeforeRead: func() error {
				if relative == protectedRelative {
					outsideReads++
				} else {
					safeReads++
				}
				return nil
			}}
		})
		if evidence.FullOK || evidence.ResultOK || outsideReads != 0 || safeReads != 4 {
			t.Fatalf("protected reparse entered data read or inventories passed: full=%v result=%v outside=%d safe=%d reasons=%v", evidence.FullOK, evidence.ResultOK, outsideReads, safeReads, evidence.Reasons)
		}
		if !slicesContainsPrefix(evidence.Reasons, "copy_inventory_invalid:full=") || !slicesContainsPrefix(evidence.Reasons, "copy_inventory_invalid:result=") || !slicesContainsPrefix(evidence.Reasons, "copy_protected_drift:row="+protectedRelative) {
			t.Fatalf("protected reparse evidence missing: %v", evidence.Reasons)
		}
		var summary cstBindingSummary
		reasons := cstBindingSettle(t, request, receipt, output, runErr, nil, "PASS:selector_payload_bound", "", "", os.TempDir(), nil, nil, &summary)
		owned.observe(reasons)
		settled = true
		if runErr != nil || summary.Disposition != "quarantine" || !cstBindingRootExists(workspace) || !slicesContainsPrefix(reasons, "copy_protected_drift:row="+protectedRelative) {
			t.Fatalf("protected reparse was not quarantined: run=%v disposition=%s exists=%v reasons=%v", runErr, summary.Disposition, cstBindingRootExists(workspace), reasons)
		}
	})
	for _, tc := range []struct {
		name, wantReason      string
		nonprotectedSourceRow bool
	}{
		{name: "copied-project-protected-drift", wantReason: "copy_protected_drift"},
		{name: "copied-pair-protected-drift", wantReason: "copy_protected_drift"},
		{name: "copied-new-row", wantReason: "copy_inventory_invalid"},
		{name: "copied-bundle-sibling-file", wantReason: "copy_inventory_invalid"},
		{name: "source-rex-drift", wantReason: "source_drift"},
		{name: "receipt-copy-aggregate-mismatch", wantReason: "receipt_invalid"},
		{name: "copied-empty-directory", wantReason: "copy_inventory_invalid"},
		{name: "copied-missing-row", wantReason: "copy_inventory_invalid", nonprotectedSourceRow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
			extraRelative := filepath.Join("line10", "Model", "extra.bin")
			if tc.nonprotectedSourceRow {
				cstProbeWriteFile(t, filepath.Join(root, "source", extraRelative), "synthetic nonprotected row")
			}
			fakeRoot := cstBindingSyntheticInterface(t, root)
			full := cstGenerationFullManifest(t, project)
			helperPath := cstBindingHelperPath(t)
			pythonSource := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "src")
			request := cstBindingRequest{Schema: cstBindingRequestSchema, SourceProject: project, ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, helperPath), ExpectedPythonSHA: cstProbeFileSHA256(t, python), FullManifest: full, ResultManifest: cstBindingResultManifest(t, full, project), Pairs: pairs, ExpectedDirectories: cstBindingFrozenDirectories(t, project)}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			workspace, err := os.MkdirTemp("", "cst-selector-binding-synthetic-")
			if err != nil {
				t.Fatal(err)
			}
			owned := cstSelectorOwnSyntheticRoot(t, workspace)
			request.WorkspaceRoot = workspace
			receipt, output, runErr := cstBindingRun(ctx, python, helperPath, request, []string{fakeRoot, pythonSource}, map[string]string{"MCPHUB_CST_BINDING_SYNTHETIC": "1"})
			owned.runReturned = true
			copyBase := filepath.Join(workspace, "bundle")
			var mutationErr error
			switch tc.name {
			case "copied-project-protected-drift":
				mutationErr = os.WriteFile(filepath.Join(copyBase, filepath.Base(project)), []byte("changed copied project"), 0o600)
			case "copied-pair-protected-drift":
				mutationErr = os.WriteFile(filepath.Join(copyBase, filepath.FromSlash(pairs[0].M3DPath)), []byte("changed copied field"), 0o600)
			case "copied-new-row":
				if err := os.MkdirAll(filepath.Dir(filepath.Join(copyBase, extraRelative)), 0o700); err != nil {
					mutationErr = err
				} else {
					mutationErr = os.WriteFile(filepath.Join(copyBase, extraRelative), []byte("new copied row"), 0o600)
				}
			case "copied-missing-row":
				mutationErr = os.Remove(filepath.Join(copyBase, extraRelative))
			case "copied-empty-directory":
				mutationErr = os.Mkdir(filepath.Join(copyBase, "line10", "Unexpected"), 0o700)
			case "copied-bundle-sibling-file":
				mutationErr = os.WriteFile(filepath.Join(copyBase, "unclassified.bin"), []byte("outside project tree"), 0o600)
			case "source-rex-drift":
				mutationErr = os.WriteFile(filepath.Join(root, "source", filepath.FromSlash(pairs[0].RexPath)), []byte("changed retained source metadata"), 0o600)
			case "receipt-copy-aggregate-mismatch":
				receipt.CopyManifestSHA256 = strings.Repeat("0", 64)
			}
			var priorReasons []string
			if mutationErr != nil {
				priorReasons = append(priorReasons, "copy_inventory_invalid:synthetic_mutation="+mutationErr.Error())
			}
			reasons := cstBindingSettle(t, request, receipt, output, runErr, nil, "PASS:selector_payload_bound", "", "", os.TempDir(), nil, priorReasons, nil)
			owned.observe(reasons)
			if mutationErr != nil || runErr != nil || !strings.Contains(strings.Join(reasons, ","), tc.wantReason) || (tc.name != "source-rex-drift" && strings.Contains(strings.Join(reasons, ","), "source_drift")) || !cstBindingRootExists(workspace) {
				t.Fatalf("copy guard failed: case=%s root=%q exists=%v run=%v mutate=%v reasons=%v", tc.name, workspace, cstBindingRootExists(workspace), runErr, mutationErr, reasons)
			}
			t.Logf("quarantined owned synthetic root=%q exists=true reasons=%v", workspace, reasons)
		})
	}
	for _, tc := range []struct {
		name, fault   string
		wantCopyDrift bool
	}{{"forced-timeout-settles-job", "copy-drift-before-timeout", true}, {"forced-timeout-clean-copy-quarantined", "", false}} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
			fakeRoot := cstBindingSyntheticInterface(t, root)
			full := cstGenerationFullManifest(t, project)
			helperPath := cstBindingHelperPath(t)
			pythonSource := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "src")
			request := cstBindingRequest{Schema: cstBindingRequestSchema, SourceProject: project, ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, helperPath), ExpectedPythonSHA: cstProbeFileSHA256(t, python), FullManifest: full, ResultManifest: cstBindingResultManifest(t, full, project), Pairs: pairs, ExpectedDirectories: cstBindingFrozenDirectories(t, project), TestStallAfterOpen: 5_000}
			pidFile := filepath.Join(root, "owned-child.pid")
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			workspace, err := os.MkdirTemp("", "cst-selector-binding-synthetic-")
			if err != nil {
				t.Fatal(err)
			}
			owned := cstSelectorOwnSyntheticRoot(t, workspace)
			owned.pidFile = pidFile
			request.WorkspaceRoot = workspace
			_, output, err := cstBindingRun(ctx, python, helperPath, request, []string{fakeRoot, pythonSource}, map[string]string{"MCPHUB_CST_BINDING_SYNTHETIC": "1", "MCPHUB_CST_BINDING_PID_FILE": pidFile, "MCPHUB_CST_BINDING_FAULT": tc.fault})
			owned.runReturned = true
			reasons := cstBindingSettle(t, request, cstBindingReceipt{}, output, err, nil, "", pidFile, "", os.TempDir(), nil, nil, nil)
			owned.observe(reasons)
			var strictErr *hubprocess.StrictRunError
			if !errors.As(err, &strictErr) || strictErr.Kind != hubprocess.StrictRunTimeout {
				t.Fatalf("forced timeout=%v stdout=%q stderr=%q", err, output.Stdout.Prefix, output.Stderr.Prefix)
			}
			pidBytes, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatalf("owned child did not enter forced window: %v", err)
			}
			var pid uint32
			if _, err := fmt.Sscan(string(pidBytes), &pid); err != nil || pid == 0 {
				t.Fatalf("invalid owned child marker %q: %v", pidBytes, err)
			}
			if path, _ := cstProbeProcessPathAndCreation(pid); path != "" {
				t.Fatalf("owned child remains after Job timeout: pid=%d path=%q", pid, path)
			}
			if after := cstGenerationFullManifest(t, project); after.AggregateSHA256 != full.AggregateSHA256 {
				t.Fatalf("timeout changed source: %s", after.AggregateSHA256)
			}
			reasonText := strings.Join(reasons, ",")
			if strings.Contains(reasonText, "copy_protected_drift") != tc.wantCopyDrift || !strings.Contains(reasonText, "timeout") || strings.Contains(reasonText, "owned_unsettled") || !cstBindingRootExists(workspace) {
				t.Fatalf("timeout copy disposition wrong; exact owned root=%q exists=%v reasons=%v", workspace, cstBindingRootExists(workspace), reasons)
			}
			t.Logf("quarantined owned synthetic root=%q exists=true reasons=%v", workspace, reasons)
		})
	}
	t.Run("forced-cancellation-quarantines-copy", func(t *testing.T) {
		root := t.TempDir()
		project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
		fakeRoot := cstBindingSyntheticInterface(t, root)
		full := cstGenerationFullManifest(t, project)
		helperPath := cstBindingHelperPath(t)
		pythonSource := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "src")
		request := cstBindingRequest{Schema: cstBindingRequestSchema, SourceProject: project, ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, helperPath), ExpectedPythonSHA: cstProbeFileSHA256(t, python), FullManifest: full, ResultManifest: cstBindingResultManifest(t, full, project), Pairs: pairs, ExpectedDirectories: cstBindingFrozenDirectories(t, project), TestStallAfterOpen: 5_000}
		pidFile := filepath.Join(root, "owned-child.pid")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		workspace, err := os.MkdirTemp("", "cst-selector-binding-synthetic-")
		if err != nil {
			t.Fatal(err)
		}
		owned := cstSelectorOwnSyntheticRoot(t, workspace)
		owned.pidFile = pidFile
		request.WorkspaceRoot = workspace
		cancelDone := make(chan struct{})
		go func() {
			defer close(cancelDone)
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				if _, err := os.Stat(pidFile); err == nil {
					cancel()
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
		_, output, runErr := cstBindingRun(ctx, python, helperPath, request, []string{fakeRoot, pythonSource}, map[string]string{"MCPHUB_CST_BINDING_SYNTHETIC": "1", "MCPHUB_CST_BINDING_PID_FILE": pidFile})
		owned.runReturned = true
		cancel()
		<-cancelDone
		reasons := cstBindingSettle(t, request, cstBindingReceipt{}, output, runErr, nil, "", pidFile, "", os.TempDir(), nil, nil, nil)
		owned.observe(reasons)
		if !errors.Is(runErr, context.Canceled) || !strings.Contains(strings.Join(reasons, ","), "cancelled") || strings.Contains(strings.Join(reasons, ","), "owned_unsettled") || !cstBindingRootExists(workspace) {
			t.Fatalf("cancellation copy disposition wrong; exact owned root=%q exists=%v run=%v reasons=%v", workspace, cstBindingRootExists(workspace), runErr, reasons)
		}
		t.Logf("quarantined owned synthetic root=%q exists=true reasons=%v", workspace, reasons)
	})
	t.Run("malformed-receipt-after-copy", func(t *testing.T) {
		root := t.TempDir()
		project, pairs := cstBindingSyntheticBundle(t, filepath.Join(root, "source"))
		fakeRoot := cstBindingSyntheticInterface(t, root)
		full := cstGenerationFullManifest(t, project)
		helperPath := cstBindingHelperPath(t)
		pythonSource := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "src")
		request := cstBindingRequest{Schema: cstBindingRequestSchema, SourceProject: project, ExpectedProjectSHA: cstProbeFileSHA256(t, project), ExpectedHelperSHA: cstProbeFileSHA256(t, helperPath), ExpectedPythonSHA: cstProbeFileSHA256(t, python), FullManifest: full, ResultManifest: cstBindingResultManifest(t, full, project), Pairs: pairs, ExpectedDirectories: cstBindingFrozenDirectories(t, project)}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pidFile := filepath.Join(root, "owned-child.pid")
		workspace, err := os.MkdirTemp("", "cst-selector-binding-synthetic-")
		if err != nil {
			t.Fatal(err)
		}
		owned := cstSelectorOwnSyntheticRoot(t, workspace)
		owned.pidFile = pidFile
		request.WorkspaceRoot = workspace
		receipt, output, err := cstBindingRun(ctx, python, helperPath, request, []string{fakeRoot, pythonSource}, map[string]string{"MCPHUB_CST_BINDING_SYNTHETIC": "1", "MCPHUB_CST_BINDING_PID_FILE": pidFile, "MCPHUB_CST_BINDING_FAULT": "malformed-after-copy"})
		owned.runReturned = true
		reasons := cstBindingSettle(t, request, receipt, output, err, nil, "PASS:selector_payload_bound", pidFile, "", os.TempDir(), nil, nil, nil)
		owned.observe(reasons)
		if err == nil {
			t.Fatalf("malformed child receipt was accepted; exact owned root=%q", workspace)
		}
		if !strings.Contains(strings.Join(reasons, ","), "receipt_invalid") || strings.Contains(strings.Join(reasons, ","), "owned_unsettled") || !cstBindingRootExists(workspace) {
			t.Fatalf("malformed receipt not quarantined; exact owned root=%q exists=%v reasons=%v", workspace, cstBindingRootExists(workspace), reasons)
		}
		t.Logf("quarantined owned synthetic root=%q exists=true reasons=%v", workspace, reasons)
	})
	t.Run("cleanup-wrong-root-refused", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "cst-selector-binding-synthetic-owned")
		other := filepath.Join(parent, "cst-selector-binding-synthetic-other")
		for _, path := range []string{root, other} {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		rootInfo, err := os.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		parentInfo, err := os.Lstat(parent)
		if err != nil {
			t.Fatal(err)
		}
		wrong := &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: other, prefix: "cst-selector-binding-synthetic-", rootInfo: rootInfo, ancestors: []cstSelectorSyntheticAncestor{{path: parent, info: parentInfo}}}}
		if err := wrong.checkIdentity(); err == nil || !cstBindingRootExists(root) || !cstBindingRootExists(other) {
			t.Fatalf("wrong-root cleanup identity was not refused: %v", err)
		}
	})
	t.Run("cleanup-unknown-links-refused", func(t *testing.T) {
		parent := t.TempDir()
		root := filepath.Join(parent, "cst-selector-binding-synthetic-owned")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(parent, "external-sentinel.bin")
		content := []byte("external sentinel must not be changed")
		if err := os.WriteFile(sentinel, content, 0o600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(sentinel)
		if err != nil {
			t.Fatal(err)
		}
		owned := &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: root}}
		for _, tc := range []struct {
			name string
			link func(string, string) error
		}{{"symlink", os.Symlink}, {"hardlink", os.Link}} {
			path := filepath.Join(root, tc.name)
			if err := tc.link(sentinel, path); err != nil {
				t.Fatal(err)
			}
			if err := owned.inventory(root); err == nil || !cstBindingRootExists(root) {
				t.Fatalf("unknown %s did not refuse cleanup: %v", tc.name, err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		after, err := os.Lstat(sentinel)
		if err != nil || !os.SameFile(before, after) {
			t.Fatalf("unknown-link refusal changed sentinel identity: %v", err)
		}
		got, err := os.ReadFile(sentinel)
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("unknown-link refusal changed sentinel content: %v", err)
		}
	})
}

// The host config is consumed only by this Windows end-to-end test. FakeScript
// replaces the external CST module only in synthetic tests; an opt-in native
// entry must leave it empty.
type cstASCIIHostConfig struct {
	WorkItem, Executable, Helper, CSTRoot, Scratch string
	Mesh, Case, Help                               string
	ExpectedExecutableSHA                          string
	Source                                         cstASCIIBundleGuard
	FramePorts                                     [2]int // E then H; each is a source pair's port identity.
	Retained                                       []cstASCIIBundleGuard
	OtherRetained                                  []cstASCIIRetainedGuard
	PythonPaths                                    []string
	Extra                                          map[string]string
	FakeScript                                     string
	FakeMode                                       string
	FakeDeadline                                   time.Duration
	FakeCancelAfter                                time.Duration
	FakePostRunFault                               string
	FakeCaptureError                               error
	FakeJobCloseFault                              error
	FakeRunnerObservation                          *cstASCIIRunnerObservation
}

func cstASCIISelectFrames(source cstASCIIBundleGuard, ports [2]int) ([]cstASCIIFrame, error) {
	frames := make([]cstASCIIFrame, 0, 2)
	for i, field := range []string{"E", "H"} {
		if ports[i] <= 0 {
			return nil, fmt.Errorf("missing %s frame port", field)
		}
		var selected *cstBindingPair
		for j := range source.Pairs {
			pair := &source.Pairs[j]
			if pair.Field == field && pair.Port == ports[i] {
				if selected != nil {
					return nil, fmt.Errorf("ambiguous %s frame port", field)
				}
				selected = pair
			}
		}
		if selected == nil {
			return nil, fmt.Errorf("missing %s source pair", field)
		}
		selector, _, err := cstBindingExpectedSelector(source.Project, source.Full, *selected)
		if err != nil {
			return nil, fmt.Errorf("%s source selector: %w", field, err)
		}
		for _, prior := range frames {
			if prior.Selector == selector {
				return nil, fmt.Errorf("ambiguous selected tree item")
			}
		}
		frames = append(frames, cstASCIIFrame{selected.Field, selector, selected.M3DPath, selected.M3DSHA256, selected.RexPath, selected.RexSHA256, 3})
	}
	return frames, nil
}

type cstASCIIRunnerObservation struct {
	PID        int
	MainExited bool
}

type cstASCIIRetainedGuard struct {
	Project, ProjectSHA string
	Full, Result        cstProbeManifest
	Directories         []string
}

func cstASCIIRetainedUnchanged(guard cstASCIIRetainedGuard) error {
	var failures []error
	for _, reason := range cstNativeObserveRetained(guard.Project, guard.Full, guard.Result, guard.ProjectSHA, nil) {
		failures = append(failures, errors.New(reason))
	}
	dirs, err := cstBindingDirectoryInventory(guard.Project)
	if err != nil || !reflect.DeepEqual(dirs, guard.Directories) {
		failures = append(failures, fmt.Errorf("retained directory drift: %v", err))
	}
	return errors.Join(failures...)
}

func cstASCIIHostRun(t *testing.T, cfg cstASCIIHostConfig) (status string, resultErr error) {
	t.Helper()
	blocked := func(status, reason string) (string, error) { return status, &cstASCIIError{status, reason} }
	if cfg.WorkItem == "" || cfg.Executable == "" || cfg.Helper == "" || cfg.Scratch == "" || cfg.CSTRoot == "" || len(cfg.Retained) == 0 {
		return blocked("BLOCKED:selector_evidence_missing_or_stale", "host_config")
	}
	if _, err := cstASCIISection(cfg.WorkItem); err != nil {
		return blocked("BLOCKED:selector_evidence_missing_or_stale", "section")
	}
	for _, guard := range cfg.Retained {
		if err := cstASCIIBundleUnchanged(guard); err != nil {
			return blocked("BLOCKED:probe_preservation_failed", "retained_preflight")
		}
	}
	for _, guard := range cfg.OtherRetained {
		if err := cstASCIIRetainedUnchanged(guard); err != nil {
			return blocked("BLOCKED:probe_preservation_failed", "other_retained_preflight")
		}
	}
	if err := cstASCIIBundleUnchanged(cfg.Source); err != nil {
		return blocked("BLOCKED:probe_preservation_failed", "source_preflight")
	}
	frames, err := cstASCIISelectFrames(cfg.Source, cfg.FramePorts)
	if err != nil {
		return blocked("BLOCKED:selector_evidence_missing_or_stale", "source_frame_selection")
	}
	for _, directory := range cfg.Source.Directories {
		path := filepath.Join(filepath.Dir(cfg.Source.Project), filepath.FromSlash(directory))
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			return blocked("BLOCKED:probe_preservation_failed", "source_directory_kind")
		}
		if err := cstSelectorCheckEntry(path, info); err != nil {
			return blocked("BLOCKED:probe_preservation_failed", "source_directory_reparse")
		}
	}
	// There is one executable: the exact Python image passed to exec.Command.
	// A child receipt may correlate its SHA, but cannot establish this identity.
	pythonBytes, err := cstASCIIBoundedFile(cfg.Executable, 128<<20)
	if err != nil {
		return blocked("BLOCKED:selector_evidence_missing_or_stale", "python_unavailable")
	}
	pythonSHA := fmt.Sprintf("%x", sha256.Sum256(pythonBytes))
	if cfg.ExpectedExecutableSHA != "" && pythonSHA != cfg.ExpectedExecutableSHA {
		return blocked("BLOCKED:selector_evidence_missing_or_stale", "launched_executable_pin_mismatch")
	}
	if cfg.FakeScript == "" {
		if !cstProbeIsDigest(cfg.ExpectedExecutableSHA) || pythonSHA != cfg.ExpectedExecutableSHA ||
			!strings.EqualFold(filepath.Clean(cfg.Executable), filepath.Join(cfg.CSTRoot, "Python", "python.exe")) {
			return blocked("BLOCKED:selector_evidence_missing_or_stale", "native_executable_pin")
		}
		for _, checked := range []struct{ path, sha string }{{cfg.Mesh, "27e10d39875077ae858f099d986a0b51945696edd0540a4dd13aac2dc1d6866a"}, {cfg.Case, "debdb9ca1294c0c0c2bd060edea5ac93b634d2dc4e9cc70ac3cc388ca7b3f858"}, {cfg.Help, cstASCIIHelpSHA}} {
			if checked.path == "" {
				return blocked("BLOCKED:selector_evidence_missing_or_stale", "native_provenance_path")
			}
			data, err := cstASCIIBoundedFile(checked.path, 128<<20)
			if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != checked.sha {
				return blocked("BLOCKED:selector_evidence_missing_or_stale", "native_provenance_hash")
			}
		}
	}
	helperBytes, err := cstASCIIBoundedFile(cfg.Helper, 1<<20)
	if err != nil {
		return blocked("BLOCKED:selector_evidence_missing_or_stale", "helper_unavailable")
	}
	helperSHA := fmt.Sprintf("%x", sha256.Sum256(helperBytes))
	if cfg.FakeScript == "" && helperSHA != "fb38ab5fea3c9987235f4eeae47375dc4c012eec918c96cdde5c9d67be1ed47a" {
		return blocked("BLOCKED:selector_evidence_missing_or_stale", "native_helper_pin")
	}
	foreignBefore, err := cstBindingProcessSnapshot(cfg.CSTRoot)
	if err != nil {
		return blocked("BLOCKED:probe_preservation_failed", "foreign_preflight")
	}
	portsBefore, err := cstASCIIPortSnapshot(foreignBefore)
	if err != nil {
		return blocked("BLOCKED:probe_preservation_failed", "foreign_ports_preflight")
	}
	beforeRoots, err := cstASCIIOwnedRootNames(cfg.Scratch)
	if err != nil {
		return blocked("BLOCKED:probe_preservation_failed", "root_preflight")
	}
	root, err := os.MkdirTemp(cfg.Scratch, "cst-result-tree-probe-ascii-host-")
	if err != nil {
		return blocked("BLOCKED:probe_preservation_failed", "root_create")
	}
	owner := &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: root, prefix: "cst-result-tree-probe-ascii-host-"}}
	status = "BLOCKED:probe_preservation_failed"
	var failure error
	var copyGuard cstASCIIBundleGuard
	var cmd *exec.Cmd
	var runReturned bool
	var ownedRunErr error
	var retainNative bool
	defer func() {
		// The defer does not infer preservation from a child echo or a receipt.
		// It independently rechecks every retained source and the whole copy.
		for _, guard := range cfg.Retained {
			if err := cstASCIIBundleUnchanged(guard); err != nil {
				failure = errors.Join(failure, fmt.Errorf("retained postcheck: %w", err))
			}
		}
		for _, guard := range cfg.OtherRetained {
			if err := cstASCIIRetainedUnchanged(guard); err != nil {
				failure = errors.Join(failure, fmt.Errorf("other retained postcheck: %w", err))
			}
		}
		if err := cstASCIIBundleUnchanged(cfg.Source); err != nil {
			failure = errors.Join(failure, fmt.Errorf("source postcheck: %w", err))
		}
		if copyGuard.Project != "" {
			evidence := cstBindingObserveCopy(copyGuard.Project, copyGuard.Full, copyGuard.Directories, copyGuard.Pairs)
			for _, reason := range evidence.Reasons {
				failure = errors.Join(failure, errors.New(reason))
			}
			if evidence.FullOK && evidence.ResultOK {
				projected, projectionErr := cstBindingResultManifestChecked(evidence.Full, copyGuard.Project)
				if projectionErr != nil || !reflect.DeepEqual(projected, evidence.Result) {
					failure = errors.Join(failure, fmt.Errorf("copy_inventory_invalid:result_projection=%v", projectionErr))
				}
			}
			t.Logf("ASCII_COPY_DELTA changed=%d added=%d", len(evidence.Deltas), len(evidence.Added))
		} else if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
			failure = errors.Join(failure, fmt.Errorf("copy_inventory_invalid:absent_or_partial=%v entries=%d", err, len(entries)))
		}
		if after, err := cstBindingProcessSnapshot(cfg.CSTRoot); err != nil {
			failure = errors.Join(failure, fmt.Errorf("foreign PID census failed: %w", err))
		} else {
			if cfg.FakeScript != "" && cfg.FakePostRunFault == "foreign_pid" {
				after[-1] = cstProbeProcessRow{PID: -1}
			}
			if !reflect.DeepEqual(after, foreignBefore) {
				failure = errors.Join(failure, errors.New("foreign PID identity changed"))
			}
		}
		if after, err := cstASCIIPortSnapshot(foreignBefore); err != nil {
			failure = errors.Join(failure, fmt.Errorf("foreign port census failed: %w", err))
		} else {
			if cfg.FakeScript != "" && cfg.FakePostRunFault == "foreign_port" {
				after[-1] = []string{"synthetic-port-drift"}
			}
			if !reflect.DeepEqual(after, portsBefore) {
				failure = errors.Join(failure, errors.New("foreign port identity changed"))
			}
		}
		if cmd != nil && (!runReturned || cmd.ProcessState == nil || !cmd.ProcessState.Exited()) {
			failure = errors.Join(failure, errors.New("owned child not joined"))
		}
		if cmd != nil && cmd.Process != nil {
			if path, _ := cstProbeProcessPathAndCreation(uint32(cmd.Process.Pid)); path != "" {
				failure = errors.Join(failure, errors.New("owned child PID still present"))
			}
		}
		if ownedRunErr != nil {
			failure = errors.Join(failure, fmt.Errorf("owned_settlement_uncertain: %w", ownedRunErr))
		}
		if cfg.FakeScript != "" && cfg.FakePostRunFault == "settlement" {
			failure = errors.Join(failure, errors.New("synthetic settlement uncertainty"))
		}
		if err := owner.checkIdentity(); err != nil {
			failure = errors.Join(failure, fmt.Errorf("root_identity_unproved: %w", err))
		}
		if err := owner.inventory(root); err != nil {
			failure = errors.Join(failure, fmt.Errorf("root inventory: %w", err))
		}
		if failure == nil && !retainNative {
			if err := os.RemoveAll(root); err != nil {
				failure = errors.Join(failure, fmt.Errorf("owned-root removal: %w", err))
			}
			if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
				failure = errors.Join(failure, fmt.Errorf("owned root remains: %v", err))
			}
		}
		afterRoots, namesErr := cstASCIIOwnedRootNames(cfg.Scratch)
		want := beforeRoots
		if cstBindingRootExists(root) {
			want = append(append([]string(nil), beforeRoots...), filepath.Base(root))
			sort.Strings(want)
		}
		if namesErr != nil || !reflect.DeepEqual(afterRoots, want) {
			failure = errors.Join(failure, fmt.Errorf("root-name delta: %v", namesErr))
		}
		if failure != nil {
			status = "BLOCKED:probe_preservation_failed"
			resultErr = errors.Join(&cstASCIIError{status, "host_settlement"}, resultErr, failure)
			t.Logf("ASCII_HOST_QUARANTINE exact_owned_root=%q: %v", root, failure)
		} else if retainNative {
			t.Logf("ASCII_HOST_QUARANTINE exact_owned_root=%q reason=native_raw_evidence_pending_review", root)
		}
	}()
	if cfg.FakeScript != "" && cfg.FakeCaptureError != nil {
		owner.captureErr = cfg.FakeCaptureError
		failure = errors.Join(failure, fmt.Errorf("root_identity_unproved: %w", cfg.FakeCaptureError))
		return blocked("BLOCKED:probe_preservation_failed", "root_identity_unproved")
	}
	if err := owner.captureChecked(root); err != nil {
		failure = errors.Join(failure, fmt.Errorf("root_identity_unproved: %w", err))
		return blocked("BLOCKED:probe_preservation_failed", "root_identity_unproved")
	}
	if err := cstASCIICopyBundle(cfg.Source, filepath.Join(root, "bundle")); err != nil {
		return blocked("BLOCKED:probe_preservation_failed", "copy_failed")
	}
	copyProject := filepath.Join(root, "bundle", filepath.Base(cfg.Source.Project))
	copyFull, err := cstBindingFullManifest(copyProject)
	if err != nil {
		return blocked("BLOCKED:probe_preservation_failed", "copy_full")
	}
	copyResult, err := cstBindingResultInventory(copyProject)
	if err != nil {
		return blocked("BLOCKED:probe_preservation_failed", "copy_result")
	}
	copyDirs, err := cstBindingDirectoryInventory(copyProject)
	if err != nil {
		return blocked("BLOCKED:probe_preservation_failed", "copy_directories")
	}
	copyGuard = cstASCIIBundleGuard{copyProject, copyFull, copyResult, copyDirs, cfg.Source.Pairs}
	if err := cstASCIIBundleUnchanged(copyGuard); err != nil {
		return blocked("BLOCKED:probe_preservation_failed", "copy_baseline")
	}
	if !reflect.DeepEqual(copyFull, cfg.Source.Full) || !reflect.DeepEqual(copyResult, cfg.Source.Result) || !reflect.DeepEqual(copyDirs, cfg.Source.Directories) {
		return blocked("BLOCKED:probe_preservation_failed", "copy_projection")
	}
	request := cstASCIIRequest{Version: "ASCIIPointCalibrationRequestV1", CopyRoot: filepath.Join(root, "bundle"), ProjectSHA: cfg.Source.Full.Rows[0].SHA256,
		FullSHA: cfg.Source.Full.AggregateSHA256, ResultSHA: cfg.Source.Result.AggregateSHA256, SectionID: cstASCIISectionID, SectionSHA: cstASCIISectionSHA,
		MeshSHA: "27e10d39875077ae858f099d986a0b51945696edd0540a4dd13aac2dc1d6866a", CaseSHA: "debdb9ca1294c0c0c2bd060edea5ac93b634d2dc4e9cc70ac3cc388ca7b3f858",
		ExecutableSHA: pythonSHA, HelperSHA: helperSHA, PointBase64: base64.StdEncoding.EncodeToString([]byte(cstASCIIPoints)),
		Points: []cstASCIIPoint{{"p1:t19:f3:q0", [3]string{"0", "7.5184627019783594", "6.1002692254485309"}}, {"p2:t11613:f2:q0", [3]string{"10", "6.3645824908535733", "0.20448276178909414"}}}}
	request.Frames = frames
	input, err := json.Marshal(request)
	if err != nil {
		return blocked("BLOCKED:probe_preservation_failed", "request_encode")
	}
	if err := cstASCIIPreflight(cfg.WorkItem, request, input, cfg.Source, copyGuard, cfg.FramePorts, cfg.Helper, cfg.Executable); err != nil {
		return "BLOCKED:selector_evidence_missing_or_stale", err
	}
	if data, err := cstASCIIBoundedFile(cfg.Executable, 128<<20); err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != pythonSHA {
		return blocked("BLOCKED:selector_evidence_missing_or_stale", "launched_executable_drift")
	}
	if cfg.FakeScript == "" {
		cmd = exec.Command(cfg.Executable, cfg.Helper)
	} else {
		cmd = exec.Command(cfg.Executable, "-B", "-c", cfg.FakeScript, cfg.Helper, cfg.FakeMode)
	}
	cmd.Dir = request.CopyRoot
	cmd.Env = cstProbeEnvironment(cfg.Executable, cfg.PythonPaths, cfg.Extra)
	deadline := 60 * time.Second
	if cfg.FakeScript != "" && cfg.FakeDeadline > 0 {
		deadline = cfg.FakeDeadline
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	if cfg.FakeScript != "" && cfg.FakeCancelAfter > 0 {
		cancelTimer := time.AfterFunc(cfg.FakeCancelAfter, cancel)
		defer cancelTimer.Stop()
	}
	baseSize := int64(0)
	for _, row := range cfg.Source.Full.Rows {
		baseSize += row.Size
	}
	growth := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				size, err := cstGenerationTreeBytes(root)
				if err != nil || size > baseSize+(2<<30) {
					growth <- fmt.Errorf("growth bound: size=%d err=%v", size, err)
					cancel()
					return
				}
			}
		}
	}()
	invocation := hubprocess.StrictRunInvocation{Command: cmd, Input: input, InputLimit: 128 << 10, StdoutLimit: 1 << 20, StderrLimit: 64 << 10}
	var output hubprocess.StrictRunResult
	var runErr error
	if cfg.FakeScript != "" && cfg.FakeJobCloseFault != nil {
		output, runErr = hubprocess.RunStrictlyContainedWithJobCloseFaultForTest(ctx, invocation, cfg.FakeJobCloseFault)
	} else {
		output, runErr = hubprocess.RunStrictlyContained(ctx, invocation)
	}
	runReturned = true
	if cfg.FakeScript != "" && cfg.FakeRunnerObservation != nil && cmd.Process != nil {
		cfg.FakeRunnerObservation.PID = cmd.Process.Pid
		cfg.FakeRunnerObservation.MainExited = cmd.ProcessState != nil && cmd.ProcessState.Exited()
	}
	ownedRunErr = runErr
	cancel()
	<-done
	if cfg.FakeScript != "" && cfg.FakePostRunFault == "copy" {
		path := filepath.Join(request.CopyRoot, filepath.FromSlash(cfg.Source.Pairs[3].M3DPath))
		if err := os.WriteFile(path, []byte("synthetic copied fourth-pair drift"), 0o600); err != nil {
			failure = errors.Join(failure, err)
		}
	}
	if cfg.FakeScript == "" && len(output.Stdout.Prefix) > 0 {
		if err := os.WriteFile(filepath.Join(root, "ascii-raw-receipt.json"), output.Stdout.Prefix, 0o600); err != nil {
			failure = errors.Join(failure, fmt.Errorf("retain bounded native receipt: %w", err))
		}
		retainNative = true
	}
	select {
	case err := <-growth:
		failure = errors.Join(failure, err)
	default:
	}
	if runErr != nil || output.Stdout.Truncated || output.Stderr.Truncated || output.Stderr.Bytes != 0 {
		if output.Stdout.Truncated || output.Stderr.Truncated || output.Stderr.Bytes != 0 {
			failure = errors.Join(failure, errors.New("receipt_invalid:stream"))
		}
		return blocked("BLOCKED:probe_preservation_failed", "bounded_child_or_stream")
	}
	var files [4][]byte
	var fileErr error
	for i, field := range []string{"E", "E", "H", "H"} {
		files[i], err = cstASCIIBoundedFile(filepath.Join(request.CopyRoot, fmt.Sprintf("ascii-calibration-%s-%d.txt", field, i%2+1)), 64<<10)
		if err != nil {
			fileErr = errors.Join(fileErr, err)
		}
	}
	raw, err := cstASCIIConsume(request, input, output.Stdout.Prefix, files)
	if err != nil {
		var typed *cstASCIIError
		if errors.As(err, &typed) {
			if typed.Reason != "producer_failure" {
				failure = errors.Join(failure, fmt.Errorf("receipt_invalid: %w", err))
			}
			return typed.Status, err
		}
		failure = errors.Join(failure, fmt.Errorf("receipt_invalid: %w", err))
		return blocked("BLOCKED:probe_preservation_failed", "receipt")
	}
	if fileErr != nil {
		failure = errors.Join(failure, fmt.Errorf("receipt_invalid:output_file=%w", fileErr))
		return blocked("BLOCKED:probe_preservation_failed", "output_file")
	}
	_, err = cstASCIIParse(raw)
	if err != nil {
		var typed *cstASCIIError
		if errors.As(err, &typed) {
			return typed.Status, err
		}
		return blocked("BLOCKED:probe_preservation_failed", "raw_parse")
	}
	return "BLOCKED:comparison_prerequisite", nil
}

func cstASCIIOwnedRootNames(parent string) ([]string, error) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "cst-result-tree-probe-ascii-host-") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Port identities complement the existing exact PID/image/creation census.
// Only listeners/connections owned by those preexisting CST PIDs are compared.
func cstASCIIPortSnapshot(foreign map[int]cstProbeProcessRow) (map[int][]string, error) {
	ports := make(map[int][]string, len(foreign))
	for pid := range foreign {
		ports[pid] = []string{}
	}
	if len(foreign) == 0 {
		return ports, nil
	}
	tool := filepath.Join(os.Getenv("SystemRoot"), "System32", "netstat.exe")
	if data, err := cstASCIIBoundedFile(tool, 8<<20); err != nil || len(data) == 0 {
		return nil, errors.New("Windows TCP inventory tool unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := hubprocess.RunStrictlyContained(ctx, hubprocess.StrictRunInvocation{Command: exec.Command(tool, "-ano", "-p", "tcp"), StdoutLimit: 4 << 20, StderrLimit: 64 << 10})
	if err != nil || result.Stdout.Truncated || result.Stderr.Truncated || result.Stderr.Bytes != 0 {
		return nil, fmt.Errorf("bounded TCP inventory failed: %v", err)
	}
	for _, line := range strings.Split(string(result.Stdout.Prefix), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "TCP" {
			continue
		}
		pid, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			continue
		}
		if _, wanted := foreign[pid]; wanted {
			ports[pid] = append(ports[pid], fields[1]+"|"+fields[2]+"|"+fields[3])
		}
	}
	for pid := range ports {
		sort.Strings(ports[pid])
	}
	return ports, nil
}

func cstASCIICopyBundle(source cstASCIIBundleGuard, destination string) error {
	if err := os.Mkdir(destination, 0o700); err != nil {
		return err
	}
	for _, relative := range source.Directories {
		if err := os.MkdirAll(filepath.Join(destination, filepath.FromSlash(relative)), 0o700); err != nil {
			return err
		}
	}
	for _, row := range source.Full.Rows {
		if row.Type != "regular" || row.Stream != "::$DATA" || filepath.IsAbs(row.Path) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(row.Path))) != row.Path || strings.HasPrefix(row.Path, "../") {
			return errors.New("invalid copy row")
		}
		from := filepath.Join(filepath.Dir(source.Project), filepath.FromSlash(row.Path))
		to := filepath.Join(destination, filepath.FromSlash(row.Path))
		info, err := os.Lstat(from)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("source row changed: %v", err)
		}
		if err := cstSelectorCheckEntry(from, info); err != nil {
			return err
		}
		links, err := cstBindingAddedFileLinkCount(from)
		if err != nil || links != 1 {
			return fmt.Errorf("source hardlink count: %d %v", links, err)
		}
		in, err := os.Open(from)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeOut, closeIn := out.Close(), in.Close()
		if err := errors.Join(copyErr, closeOut, closeIn); err != nil {
			return err
		}
		stamp := time.Unix(0, row.ModifiedNS)
		if err := os.Chtimes(to, stamp, stamp); err != nil {
			return err
		}
	}
	return nil
}

func TestCSTSavedFieldASCIIPointCalibrationHostFake(t *testing.T) {
	python := cstProbeTestPython(t)
	item := os.Getenv("MCPHUB_CST_ASCII_WORK_ITEM")
	if item == "" {
		t.Skip("explicit receiving-section work item required")
	}
	root := t.TempDir()
	project := filepath.Join(root, "probe.cst")
	cstProbeWriteFile(t, project, "copied project")
	cstProbeWriteFile(t, filepath.Join(root, "probe", "Model", "state.bin"), "frozen model state")
	var pairs []cstBindingPair
	for _, field := range []string{"E", "H"} {
		for port := 1; port <= 2; port++ {
			base := fmt.Sprintf("probe/Result/mcp_line10_%s_3GHz_%d,1", field, port)
			m3d, rex := base+".m3d", base+"_m3d.rex"
			cstProbeWriteFile(t, filepath.Join(root, filepath.FromSlash(m3d)), fmt.Sprintf("%s payload %d", field, port))
			cstProbeWriteFile(t, filepath.Join(root, filepath.FromSlash(rex)), fmt.Sprintf(`<MetaResultFile><SimulationProperties fieldname="mcp_line10_%s_3GHz [%d]"/></MetaResultFile>`, field, port))
			pairs = append(pairs, cstBindingPair{Field: field, Port: port, M3DPath: m3d, RexPath: rex,
				M3DSHA256: cstProbeFileSHA256(t, filepath.Join(root, filepath.FromSlash(m3d))),
				RexSHA256: cstProbeFileSHA256(t, filepath.Join(root, filepath.FromSlash(rex)))})
		}
	}
	full, err := cstBindingFullManifest(project)
	if err != nil {
		t.Fatal(err)
	}
	result, err := cstBindingResultInventory(project)
	if err != nil {
		t.Fatal(err)
	}
	source := cstASCIIBundleGuard{project, full, result, cstBindingFrozenDirectories(t, project), pairs}
	helper := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "tests", "_helpers", "cst_saved_field_ascii_point_calibration.py")
	// This fake is the producer's own fake interface, never cst.interface.
	script := `import hashlib,importlib.util,json,sys,time
from pathlib import Path
spec=importlib.util.spec_from_file_location("ascii_fixture",sys.argv[1]);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
raw=sys.stdin.buffer.read(131073);request=json.loads(raw)
mode=sys.argv[2]
if mode=="alternate_h2":
    frame=request["frames"][1]
    assert frame["selector"]=="2D/3D Results\\H-Field\\mcp_line10_H_3GHz [2]"
    assert frame["rexPath"]=="probe/Result/mcp_line10_H_3GHz_2,1_m3d.rex"
    assert frame["rexSha256"]==hashlib.sha256((Path(request["copyRoot"])/frame["rexPath"]).read_bytes()).hexdigest()
if mode in ("malformed","partial"):
    sys.stdout.write('{"version":');sys.stdout.flush();raise SystemExit
if mode=="oversized":
    sys.stdout.write("x"*1048577);sys.stdout.flush();raise SystemExit
if mode=="timeout":
    sys.stdout.write('{"version":');sys.stdout.flush();time.sleep(30)
class Fake(m._FakeCST):
    def GetFileFromTreeItem(self,selector):
        self.calls.append("GetFileFromTreeItem")
        return {frame["selector"]: frame["m3dPath"] for frame in request["frames"]}[selector]
    def open_project(self,path):
        if mode=="copy_on_open_allowed":
            (Path(request["copyRoot"])/"probe"/"Model"/"state.bin").write_bytes(b"copy-only open state")
            (Path(request["copyRoot"])/"probe"/"Result"/"copy-added.coe").write_bytes(b"copy-only coefficient")
        return super().open_project(path)
fake=Fake(Path(request["copyRoot"]))
receipt=m._execute(request,raw,fake)
sys.stdout.write(json.dumps(receipt,separators=(",",":"),ensure_ascii=False)+"\n")
`
	cfg := cstASCIIHostConfig{WorkItem: item, Executable: python, Helper: helper, CSTRoot: filepath.Join(root, "foreign"), Scratch: filepath.Join(root, "scratch"), Source: source, FramePorts: [2]int{1, 1}, Retained: []cstASCIIBundleGuard{source}, FakeScript: script}
	cfg.ExpectedExecutableSHA = cstProbeFileSHA256(t, python)
	for _, path := range []string{cfg.CSTRoot, cfg.Scratch} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	status, err := cstASCIIHostRun(t, cfg)
	var blocked *cstASCIIError
	if !errors.As(err, &blocked) || status != "BLOCKED:ascii_header_unverified" || blocked.Status != status {
		t.Fatalf("fake export should reach raw header blocker, status=%q err=%v", status, err)
	}
	entries, err := os.ReadDir(cfg.Scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("owned roots remain after settled fake export: %v %v", entries, err)
	}
	t.Run("alternate_h2", func(t *testing.T) {
		cfg.FramePorts, cfg.FakeMode = [2]int{1, 2}, "alternate_h2"
		got, runErr := cstASCIIHostRun(t, cfg)
		cfg.FramePorts, cfg.FakeMode = [2]int{1, 1}, ""
		var typed *cstASCIIError
		if got != "BLOCKED:ascii_header_unverified" || !errors.As(runErr, &typed) || typed.Status != got {
			t.Fatalf("explicit H port two request failed: status=%s err=%v", got, runErr)
		}
		entries, readErr := os.ReadDir(cfg.Scratch)
		if readErr != nil || len(entries) != 0 {
			t.Fatalf("alternate frame left an owned root: %v %v", entries, readErr)
		}
	})
	for _, ports := range [][2]int{{0, 1}, {1, 3}} {
		t.Run(fmt.Sprintf("invalid_frame_ports_%d_%d", ports[0], ports[1]), func(t *testing.T) {
			cfg.FramePorts = ports
			got, runErr := cstASCIIHostRun(t, cfg)
			cfg.FramePorts = [2]int{1, 1}
			var typed *cstASCIIError
			if got != "BLOCKED:selector_evidence_missing_or_stale" || !errors.As(runErr, &typed) || typed.Status != got {
				t.Fatalf("invalid frame identity was admitted: status=%s err=%v", got, runErr)
			}
			entries, readErr := os.ReadDir(cfg.Scratch)
			if readErr != nil || len(entries) != 0 {
				t.Fatalf("invalid frame identity created an owned root: %v %v", entries, readErr)
			}
		})
	}
	t.Run("copy_on_open_allowed", func(t *testing.T) {
		cfg.FakeMode = "copy_on_open_allowed"
		status, runErr := cstASCIIHostRun(t, cfg)
		entries, readErr := os.ReadDir(cfg.Scratch)
		// An old-policy quarantine is still only this test's root. Settle it
		// safely before reporting the expected RED assertion.
		if readErr == nil && len(entries) == 1 {
			path := filepath.Join(cfg.Scratch, entries[0].Name())
			owned := &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: path, prefix: "cst-result-tree-probe-ascii-host-"}}
			owned.capture(t, path)
			if err := errors.Join(owned.checkIdentity(), owned.inventory(path)); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
		}
		cfg.FakeMode = ""
		var typed *cstASCIIError
		if status != "BLOCKED:ascii_header_unverified" || !errors.As(runErr, &typed) || typed.Status != status || readErr != nil || len(entries) != 0 {
			t.Fatalf("admissible copy delta vetoed: status=%s err=%v roots=%v read=%v", status, runErr, entries, readErr)
		}
	})
	for _, fault := range []string{"capture_after_mkdirtemp", "job_close_after_main_exit"} {
		t.Run(fault, func(t *testing.T) {
			cfg.FakeMode, cfg.FakeDeadline, cfg.FakeCancelAfter, cfg.FakePostRunFault = "", 0, 0, ""
			observation := &cstASCIIRunnerObservation{}
			cfg.FakeRunnerObservation = observation
			if fault == "capture_after_mkdirtemp" {
				cfg.FakeCaptureError = errors.New("synthetic capture failure")
				cfg.FakePostRunFault = "foreign_pid" // proves postflight did not short-circuit.
			} else {
				cfg.FakeJobCloseFault = errors.New("synthetic Job.Close failure")
			}
			status, runErr := cstASCIIHostRun(t, cfg)
			entries, readErr := os.ReadDir(cfg.Scratch)
			if readErr == nil && len(entries) == 1 {
				path := filepath.Join(cfg.Scratch, entries[0].Name())
				owned := &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: path, prefix: "cst-result-tree-probe-ascii-host-"}}
				owned.capture(t, path)
				if err := errors.Join(owned.checkIdentity(), owned.inventory(path)); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
			}
			cfg.FakeCaptureError, cfg.FakeJobCloseFault, cfg.FakePostRunFault, cfg.FakeRunnerObservation = nil, nil, "", nil
			wantReason := "owned_settlement_uncertain"
			if fault == "capture_after_mkdirtemp" {
				wantReason = "root_identity_unproved"
			}
			var typed *cstASCIIError
			if status != "BLOCKED:probe_preservation_failed" || !errors.As(runErr, &typed) || typed.Status != status || !strings.Contains(fmt.Sprint(runErr), wantReason) || readErr != nil || len(entries) != 1 {
				t.Fatalf("post-root owner missed %s: status=%s err=%v roots=%v read=%v", fault, status, runErr, entries, readErr)
			}
			if fault == "capture_after_mkdirtemp" && !strings.Contains(fmt.Sprint(runErr), "foreign PID identity changed") {
				t.Fatalf("capture failure skipped independent foreign postflight: %v", runErr)
			}
			if fault == "job_close_after_main_exit" {
				var strict *hubprocess.StrictRunError
				if !errors.As(runErr, &strict) || strict.Kind != hubprocess.StrictRunExecutionFailed || !strings.Contains(fmt.Sprint(runErr), "synthetic Job.Close failure") || observation.PID <= 0 || !observation.MainExited {
					t.Fatalf("runner close fault not bound to exited child: err=%v observed=%+v", runErr, observation)
				}
				if path, _ := cstProbeProcessPathAndCreation(uint32(observation.PID)); path != "" {
					t.Fatalf("runner close fault left child PID %d", observation.PID)
				}
			}
		})
	}
	t.Run("wrong_launched_image_pin", func(t *testing.T) {
		cfg.ExpectedExecutableSHA = strings.Repeat("0", 64)
		status, err := cstASCIIHostRun(t, cfg)
		var typed *cstASCIIError
		if status != "BLOCKED:selector_evidence_missing_or_stale" || !errors.As(err, &typed) || typed.Status != status {
			t.Fatalf("launched-image mismatch not blocked: %s %v", status, err)
		}
		cfg.ExpectedExecutableSHA = cstProbeFileSHA256(t, python)
		entries, err := os.ReadDir(cfg.Scratch)
		if err != nil || len(entries) != 0 {
			t.Fatalf("pre-child mismatch created root: %v %v", entries, err)
		}
	})
	for _, tc := range []struct {
		mode                  string
		deadline, cancelAfter time.Duration
	}{
		{"malformed", 0, 0}, {"partial", 0, 0}, {"oversized", 0, 0},
		{"timeout", 200 * time.Millisecond, 0}, {"timeout", 0, 200 * time.Millisecond},
	} {
		t.Run(fmt.Sprintf("%s_%d", tc.mode, tc.cancelAfter), func(t *testing.T) {
			cfg.FakeMode, cfg.FakeDeadline, cfg.FakeCancelAfter = tc.mode, tc.deadline, tc.cancelAfter
			got, err := cstASCIIHostRun(t, cfg)
			var typed *cstASCIIError
			if got != "BLOCKED:probe_preservation_failed" || !errors.As(err, &typed) || typed.Status != got {
				t.Fatalf("fault was not typed: mode=%s status=%q err=%v", tc.mode, got, err)
			}
			entries, err := os.ReadDir(cfg.Scratch)
			if err != nil || len(entries) != 1 {
				t.Fatalf("unproved runner or receipt was not quarantined: %v %v", entries, err)
			}
			quarantined := filepath.Join(cfg.Scratch, entries[0].Name())
			owned := &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: quarantined, prefix: "cst-result-tree-probe-ascii-host-"}}
			owned.capture(t, quarantined)
			if err := errors.Join(owned.checkIdentity(), owned.inventory(quarantined)); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(quarantined); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, fault := range []string{"copy", "foreign_pid", "foreign_port", "settlement"} {
		t.Run(fault, func(t *testing.T) {
			cfg.FakeMode, cfg.FakeDeadline, cfg.FakeCancelAfter, cfg.FakePostRunFault = "", 0, 0, fault
			got, err := cstASCIIHostRun(t, cfg)
			var typed *cstASCIIError
			if got != "BLOCKED:probe_preservation_failed" || !errors.As(err, &typed) || typed.Status != got {
				t.Fatalf("post-run fault not vetoed: %s %s %v", fault, got, err)
			}
			entries, err := os.ReadDir(cfg.Scratch)
			if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "cst-result-tree-probe-ascii-host-") {
				t.Fatalf("uncertain exact root not quarantined: %v %v", entries, err)
			}
			quarantined := filepath.Join(cfg.Scratch, entries[0].Name())
			// All fault bytes and the only child are synthetic/test-owned. Verify
			// identity and links before explicit test-only disposal of this root.
			owned := &cstSelectorSyntheticRoot{cstSelectorRootIdentity: cstSelectorRootIdentity{root: quarantined, prefix: "cst-result-tree-probe-ascii-host-"}}
			owned.capture(t, quarantined)
			if err := owned.checkIdentity(); err != nil {
				t.Fatal(err)
			}
			if err := owned.inventory(quarantined); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(quarantined); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(quarantined); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("synthetic quarantine remains: %v", err)
			}
			if err := cstASCIIBundleUnchanged(source); err != nil {
				t.Fatalf("source changed: %v", err)
			}
		})
	}
}

// This is deliberately inert in ordinary suites. Even after explicit opt-in,
// it is an export-only receiving probe, never a solve or numerical PASS.
func TestCSTSavedFieldASCIIPointCalibrationNative(t *testing.T) {
	if os.Getenv("MCPHUB_CST_ASCII_NATIVE_RUN") != "1" {
		t.Skip("native ASCII export needs separate one-run admission")
	}
	item := cstProbeExplicitDirectory(t, "MCPHUB_CST_ASCII_WORK_ITEM")
	cstRoot := cstProbeExplicitDirectory(t, cstProbeCSTRootEnv)
	scratch := cstProbeExplicitDirectory(t, cstProbeScratchEnv)
	python := cstProbeExplicitFile(t, cstProbePythonEnv)
	fixture := cstProbeExplicitFile(t, "MCPHUB_CST_SELECTOR_BINDING_FIXTURE")
	original := cstProbeExplicitFile(t, "MCPHUB_CST_SELECTOR_BINDING_ORIGINAL")
	failed := cstProbeExplicitFile(t, "MCPHUB_CST_SELECTOR_BINDING_FAILED")
	if fixture == original || fixture == failed || original == failed {
		t.Fatal("retained bundle paths must be distinct")
	}
	retained := []struct{ project, projectSHA, fullSHA string }{
		{fixture, "2a1cf725ab680ea41ff73604024684226f87359a13482a43549d6f3184513388", "51aeeb415ed932ef502c739eb066b8ee54b67a69f634ac78c4becde8f0fc4861"},
		{original, "7f65676dd04ba45c9b21fb1d134e05c58d90f08fe0c71912062d5cf6003ed545", "22fafe9a54f2e256cc959883806b3ae0029d6794abfbbfeb8e15538b8786a2be"},
		{failed, "c2d26cdcf250c5afbdc0119550f9c1234f70554e9f05b311f832e8599cc3c101", "6c0685f244b272e77c5c4924dd6677ffa6853bf45df8e1066b34feb4c3792472"},
	}
	var others []cstASCIIRetainedGuard
	var source cstASCIIBundleGuard
	for i, r := range retained {
		full := cstGenerationFullManifest(t, r.project)
		projectRow, err := cstNativeManifestExactRow(full, filepath.Base(r.project))
		if err != nil || projectRow.SHA256 != r.projectSHA {
			t.Fatalf("retained project %d changed: %v", i, err)
		}
		if full.AggregateSHA256 != r.fullSHA {
			t.Fatalf("retained full manifest %d changed", i)
		}
		result, err := cstBindingResultInventory(r.project)
		if err != nil {
			t.Fatal(err)
		}
		projected, err := cstBindingResultManifestChecked(full, r.project)
		if err != nil || !reflect.DeepEqual(projected, result) {
			t.Fatalf("retained Result %d changed: %v", i, err)
		}
		dirs := cstBindingFrozenDirectories(t, r.project)
		if i > 0 {
			others = append(others, cstASCIIRetainedGuard{r.project, r.projectSHA, full, result, dirs})
			continue
		}
		if len(full.Rows) != 251 || len(result.Rows) != 143 || result.AggregateSHA256 != "45d84dc7bd9de5f6d0bf8d2be25548932904d0a2998754091d0c288da2d7a2dc" {
			t.Fatal("target manifest identity changed")
		}
		stem := strings.TrimSuffix(filepath.Base(r.project), filepath.Ext(r.project))
		pairs := []cstBindingPair{
			{Field: "E", Port: 1, M3DSHA256: "3cc3f770707547ac219dbbe06baabe49f2c4683a96bf39a8021384b083a042de", RexSHA256: "c2477609e4a5c43a92bf9cf6e590c1be39b9884f09d66bb1be7935e47a65d1e4"},
			{Field: "E", Port: 2, M3DSHA256: "42669dfdf9ac06fbccf6439cb17c9a536e3b5c8ea2875d29228a29260ce4cc34", RexSHA256: "0b7405ad994545854df6f46009651adae6ca60380d16221a2ff1f767de35f469"},
			{Field: "H", Port: 1, M3DSHA256: "da638fb66302cde611c557ac21b97993119ff02abfb13f471cda81315430ca05", RexSHA256: "dccb33219789c06d49710c980c661d454ccef8c6517fca2e8a6b8af3fea740f9"},
			{Field: "H", Port: 2, M3DSHA256: "915e6c612164e9f1b71d00433df79025009a72b8483fd59cce46fbefb1fadf40", RexSHA256: "d84c8710925f45890f6fd586db1ae0342c9f13efcb18cac8fc54d786229d9a5e"},
		}
		for j := range pairs {
			base := fmt.Sprintf("%s/Result/mcp_line10_%s_3GHz_%d,1", stem, pairs[j].Field, pairs[j].Port)
			pairs[j].M3DPath, pairs[j].RexPath = base+".m3d", base+"_m3d.rex"
			for _, wanted := range []struct{ path, digest string }{{pairs[j].M3DPath, pairs[j].M3DSHA256}, {pairs[j].RexPath, pairs[j].RexSHA256}} {
				row, rowErr := cstNativeManifestExactRow(full, wanted.path)
				if rowErr != nil || row.SHA256 != wanted.digest {
					t.Fatalf("retained pair %d changed: %v", j, rowErr)
				}
			}
		}
		source = cstASCIIBundleGuard{r.project, full, result, dirs, pairs}
	}
	helper := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "tests", "_helpers", "cst_saved_field_ascii_point_calibration.py")
	root := os.Getenv("SystemRoot")
	cfg := cstASCIIHostConfig{WorkItem: item, Executable: python, Helper: helper, CSTRoot: cstRoot, Scratch: scratch, Source: source, FramePorts: [2]int{1, 2},
		Retained: []cstASCIIBundleGuard{source}, OtherRetained: others, ExpectedExecutableSHA: os.Getenv("MCPHUB_CST_ASCII_PYTHON_SHA256"),
		Mesh: cstProbeExplicitFile(t, "MCPHUB_CST_ASCII_MESH"), Case: cstProbeExplicitFile(t, "MCPHUB_CST_ASCII_CASE_MANIFEST"), Help: cstProbeExplicitFile(t, "MCPHUB_CST_ASCII_HELP"),
		PythonPaths: []string{filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "src"), filepath.Join(cstRoot, "AMD64"), filepath.Join(cstRoot, "AMD64", "python_cst_libraries"), filepath.Join(cstRoot, "Library", "Python", "lib")},
		Extra:       map[string]string{"PATH": strings.Join([]string{filepath.Join(cstRoot, "AMD64"), filepath.Join(cstRoot, "Python"), filepath.Join(root, "System32"), root}, string(os.PathListSeparator))}}
	status, err := cstASCIIHostRun(t, cfg)
	if err != nil {
		t.Fatalf("native ASCII export unverified: status=%s err=%v", status, err)
	}
	if status != "BLOCKED:comparison_prerequisite" {
		t.Fatalf("native export did not stay comparison-blocked: %s", status)
	}
}

func TestCSTSavedFieldSelectorBindingNative(t *testing.T) {
	if os.Getenv("MCPHUB_CST_SELECTOR_BINDING_RUN") != "1" {
		t.Skip("native copied-project selector binding requires explicit one-run opt-in")
	}
	python := cstProbeExplicitFile(t, cstProbePythonEnv)
	cstRoot := cstProbeExplicitDirectory(t, cstProbeCSTRootEnv)
	scratch := cstProbeExplicitDirectory(t, cstProbeScratchEnv)
	fixture := cstProbeExplicitFile(t, "MCPHUB_CST_SELECTOR_BINDING_FIXTURE")
	original := cstProbeExplicitFile(t, "MCPHUB_CST_SELECTOR_BINDING_ORIGINAL")
	failed := cstProbeExplicitFile(t, "MCPHUB_CST_SELECTOR_BINDING_FAILED")
	if fixture == original || fixture == failed || original == failed {
		t.Fatal("three retained bundle project paths must be distinct")
	}
	retained := []struct {
		project, projectSHA, fullSHA string
	}{
		{fixture, "2a1cf725ab680ea41ff73604024684226f87359a13482a43549d6f3184513388", "51aeeb415ed932ef502c739eb066b8ee54b67a69f634ac78c4becde8f0fc4861"},
		{original, "7f65676dd04ba45c9b21fb1d134e05c58d90f08fe0c71912062d5cf6003ed545", "22fafe9a54f2e256cc959883806b3ae0029d6794abfbbfeb8e15538b8786a2be"},
		{failed, "c2d26cdcf250c5afbdc0119550f9c1234f70554e9f05b311f832e8599cc3c101", "6c0685f244b272e77c5c4924dd6677ffa6853bf45df8e1066b34feb4c3792472"},
	}
	var full cstProbeManifest
	retainedBaseline := make([]cstProbeManifest, len(retained))
	retainedResultBaseline := make([]cstProbeManifest, len(retained))
	for index, bundle := range retained {
		manifest := cstGenerationFullManifest(t, bundle.project)
		projectRow, err := cstNativeManifestExactRow(manifest, filepath.Base(bundle.project))
		if err != nil || projectRow.SHA256 != bundle.projectSHA {
			t.Fatalf("retained project[%d] identity drift: %v", index, err)
		}
		retainedBaseline[index] = manifest
		result, err := cstBindingResultInventory(bundle.project)
		if err != nil {
			t.Fatalf("retained Result[%d] unavailable: %v", index, err)
		}
		projected, err := cstBindingResultManifestChecked(manifest, bundle.project)
		if err != nil || !reflect.DeepEqual(projected, result) {
			t.Fatalf("retained Result[%d] projection drift: %v", index, err)
		}
		retainedResultBaseline[index] = result
		if manifest.AggregateSHA256 != bundle.fullSHA {
			t.Fatalf("retained bundle[%d] identity drift: %s", index, manifest.AggregateSHA256)
		}
		if index == 0 {
			full = manifest
		}
	}
	resultManifest := cstBindingResultManifest(t, full, fixture)
	if resultManifest.AggregateSHA256 != "45d84dc7bd9de5f6d0bf8d2be25548932904d0a2998754091d0c288da2d7a2dc" || len(full.Rows) != 251 || len(resultManifest.Rows) != 143 {
		t.Fatalf("target manifest mismatch: full=%d/%s Result=%d/%s", len(full.Rows), full.AggregateSHA256, len(resultManifest.Rows), resultManifest.AggregateSHA256)
	}
	stem := strings.TrimSuffix(filepath.Base(fixture), filepath.Ext(fixture))
	pairs := []cstBindingPair{
		{Field: "E", Port: 1, M3DSHA256: "3cc3f770707547ac219dbbe06baabe49f2c4683a96bf39a8021384b083a042de", RexSHA256: "c2477609e4a5c43a92bf9cf6e590c1be39b9884f09d66bb1be7935e47a65d1e4"},
		{Field: "E", Port: 2, M3DSHA256: "42669dfdf9ac06fbccf6439cb17c9a536e3b5c8ea2875d29228a29260ce4cc34", RexSHA256: "0b7405ad994545854df6f46009651adae6ca60380d16221a2ff1f767de35f469"},
		{Field: "H", Port: 1, M3DSHA256: "da638fb66302cde611c557ac21b97993119ff02abfb13f471cda81315430ca05", RexSHA256: "dccb33219789c06d49710c980c661d454ccef8c6517fca2e8a6b8af3fea740f9"},
		{Field: "H", Port: 2, M3DSHA256: "915e6c612164e9f1b71d00433df79025009a72b8483fd59cce46fbefb1fadf40", RexSHA256: "d84c8710925f45890f6fd586db1ae0342c9f13efcb18cac8fc54d786229d9a5e"},
	}
	for index := range pairs {
		base := fmt.Sprintf("%s/Result/mcp_line10_%s_3GHz_%d,1", stem, pairs[index].Field, pairs[index].Port)
		pairs[index].M3DPath = base + ".m3d"
		pairs[index].RexPath = base + "_m3d.rex"
		for _, wanted := range []struct{ path, digest string }{{pairs[index].M3DPath, pairs[index].M3DSHA256}, {pairs[index].RexPath, pairs[index].RexSHA256}} {
			row, rowErr := cstNativeManifestExactRow(full, wanted.path)
			if rowErr != nil || row.SHA256 != wanted.digest {
				t.Fatalf("qualified pair[%d] drift: %v", index, rowErr)
			}
		}
	}
	foreignBefore := cstProbeProcessSnapshot(t, cstRoot)
	helperPath := cstBindingHelperPath(t)
	pythonSHA := cstProbeFileSHA256(t, python)
	helperSHA := cstProbeFileSHA256(t, helperPath)
	repositoryRoot := cstProbeRepositoryRoot(t)
	root := os.Getenv("SystemRoot")
	sourceDirs := cstBindingFrozenDirectories(t, fixture)
	ownedRoot, err := os.MkdirTemp(scratch, "cst-selector-binding-")
	if err != nil {
		t.Fatal(err)
	}
	request := cstBindingRequest{Schema: cstBindingRequestSchema, SourceProject: fixture, WorkspaceRoot: ownedRoot, ExpectedProjectSHA: retained[0].projectSHA, ExpectedHelperSHA: helperSHA, ExpectedPythonSHA: pythonSHA, FullManifest: full, ResultManifest: resultManifest, Pairs: pairs, ExpectedDirectories: sourceDirs}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var copyBaseBytes int64
	for _, row := range full.Rows {
		copyBaseBytes += row.Size
	}
	growthFailure := make(chan error, 1)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				bytes, walkErr := cstGenerationTreeBytes(ownedRoot)
				if walkErr != nil || bytes > copyBaseBytes+(2<<30) {
					growthFailure <- fmt.Errorf("owned copy growth bound: bytes=%d err=%v", bytes, walkErr)
					cancel()
					return
				}
			}
		}
	}()
	receipt, output, runErr := cstBindingRun(ctx, python, helperPath, request,
		[]string{filepath.Join(repositoryRoot, "servers", "electromagnetics-mcp", "src"), filepath.Join(cstRoot, "AMD64"), filepath.Join(cstRoot, "AMD64", "python_cst_libraries"), filepath.Join(cstRoot, "Library", "Python", "lib")},
		map[string]string{"PATH": strings.Join([]string{filepath.Join(cstRoot, "AMD64"), filepath.Join(cstRoot, "Python"), filepath.Join(root, "System32"), root}, string(os.PathListSeparator))})
	cancel()
	<-watchDone
	var growthErr error
	select {
	case growthErr = <-growthFailure:
	default:
	}
	var priorReasons []string
	for index, bundle := range retained {
		var selectedPairs []cstBindingPair
		if index == 0 {
			selectedPairs = pairs
		}
		for _, reason := range cstNativeObserveRetained(bundle.project, retainedBaseline[index], retainedResultBaseline[index], bundle.projectSHA, selectedPairs) {
			priorReasons = append(priorReasons, fmt.Sprintf("retained[%d]:%s", index, reason))
		}
	}
	wantStatus := "PASS:selector_payload_bound"
	if receipt.Status == "BLOCKED:selector_payload_ambiguous" || receipt.Status == "BLOCKED:selector_binding_api_unavailable" || receipt.Status == "BLOCKED:selector_frequency_mismatch" {
		wantStatus = receipt.Status
	}
	reasons := cstBindingSettle(t, request, receipt, output, runErr, growthErr, wantStatus, "", cstRoot, scratch, foreignBefore, priorReasons, nil)
	if len(reasons) != 0 || receipt.Status != "PASS:selector_payload_bound" {
		t.Fatalf("native binding not proven: status=%q run=%v growth=%v owned_root=%q exists=%v reasons=%v stderr=%q", receipt.Status, runErr, growthErr, ownedRoot, cstBindingRootExists(ownedRoot), reasons, output.Stderr.Prefix)
	}
}

// ASCII calibration is a test-only codec oracle. A synthetic match is not
// evidence of CST output grammar, a native query, or Line10 numerical acceptance.
type cstASCIIMismatch struct {
	Row, Column, Slot    int
	RawToken             string
	SignificantDigits    int
	ASCII17, Query17     string
	ASCIIBits, QueryBits uint64
}

const (
	cstASCIISectionID  = "2026-09-23 — one hardlink-guarded native D1 selector receiving run"
	cstASCIISectionSHA = "263ed9e3af251450097c6913c65c8154bdf5097a7315ff042bd00e2a25d58947"
	cstASCIIHelpSHA    = "3c8447e6dc54b2f434f6e12353290f95148c75a5792d5840ea624b8a6e0aa850"
	cstASCIIPoints     = "0 7.5184627019783594 6.1002692254485309\r\n10 6.3645824908535733 0.20448276178909414\r\n"
)

type cstASCIIFrame struct {
	Field        string  `json:"field"`
	Selector     string  `json:"selector"`
	M3DPath      string  `json:"m3dPath"`
	M3DSHA       string  `json:"m3dSha256"`
	RexPath      string  `json:"rexPath"`
	RexSHA       string  `json:"rexSha256"`
	RawFrequency float64 `json:"rawFrequency"`
}

type cstASCIIPoint struct {
	ID  string    `json:"id"`
	XYZ [3]string `json:"xyzMm"`
}

type cstASCIIRequest struct {
	Version       string          `json:"version"`
	CopyRoot      string          `json:"copyRoot"`
	ProjectSHA    string          `json:"projectSha256"`
	FullSHA       string          `json:"fullAggregateSha256"`
	ResultSHA     string          `json:"resultAggregateSha256"`
	SectionID     string          `json:"selectorReceivingSectionId"`
	SectionSHA    string          `json:"selectorReceivingSectionSha256"`
	MeshSHA       string          `json:"meshSha256"`
	CaseSHA       string          `json:"caseManifestSha256"`
	ExecutableSHA string          `json:"cstExecutableSha256"`
	HelperSHA     string          `json:"helperSha256"`
	Frames        []cstASCIIFrame `json:"frames"`
	Points        []cstASCIIPoint `json:"points"`
	PointBase64   string          `json:"pointFileBase64"`
}

type cstASCIIBoundFrame struct {
	Field        string  `json:"field"`
	Selector     string  `json:"selector"`
	SelectedItem string  `json:"selectedTreeItem"`
	ReturnedPath string  `json:"returnedM3dPath"`
	Selected     bool    `json:"selectionResult"`
	Frequency    float64 `json:"rawFrequency"`
	M3DSHA       string  `json:"m3dSha256"`
	RexSHA       string  `json:"rexSha256"`
}

type cstASCIIHashFrame struct {
	Field  string `json:"field"`
	M3DSHA string `json:"m3dSha256"`
	RexSHA string `json:"rexSha256"`
}

type cstASCIIHashes struct {
	ProjectSHA string              `json:"projectSha256"`
	Frames     []cstASCIIHashFrame `json:"frames"`
}

type cstASCIIReceipt struct {
	Version    string               `json:"version"`
	RequestSHA string               `json:"requestSha256"`
	Status     string               `json:"status"`
	SectionID  string               `json:"selectorReceivingSectionId"`
	SectionSHA string               `json:"selectorReceivingSectionSha256"`
	Trace      []cstProbeAPIEvent   `json:"apiTrace"`
	Frames     []cstASCIIBoundFrame `json:"boundFrames"`
	Hashes     struct {
		Pre  cstASCIIHashes `json:"pre"`
		Post cstASCIIHashes `json:"post"`
	} `json:"prePostHashes"`
	ExecutableSHA string `json:"cstExecutableSha256"`
	HelpSHA       string `json:"helpSha256"`
	PointBase64   string `json:"pointFileBase64"`
	PointSHA      string `json:"pointFileSha256"`
	Outputs       []struct {
		Field     string `json:"field"`
		Repeat    int    `json:"repeat"`
		RawBase64 string `json:"rawBase64"`
		SHA       string `json:"sha256"`
	} `json:"outputs"`
	Error      string `json:"error"`
	Settlement struct {
		Closed  bool `json:"closedWithoutSave"`
		Session bool `json:"sessionSettled"`
		Process bool `json:"ownedProcessSettled"`
		Foreign bool `json:"foreignPreserved"`
	} `json:"settlement"`
}

type cstASCIIError struct{ Status, Reason string }

func (e *cstASCIIError) Error() string { return e.Status + ":" + e.Reason }

func cstASCIISection(root string) ([]byte, error) {
	bad := func(reason string) ([]byte, error) {
		return nil, &cstASCIIError{"BLOCKED:selector_evidence_missing_or_stale", reason}
	}
	if !filepath.IsAbs(root) {
		return bad("work_item_root_not_absolute")
	}
	raw, err := cstASCIIBoundedFile(filepath.Join(root, "implementation-s5h-native-producer-d1-discovery.md"), 4<<20)
	if err != nil {
		return bad("section_file:" + err.Error())
	}
	heading := []byte("## " + cstASCIISectionID)
	start, end, count := -1, len(raw), 0
	offset := 0
	for _, line := range bytes.SplitAfter(raw, []byte("\n")) {
		text := bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
		if bytes.Equal(text, heading) {
			count++
			if start < 0 {
				start = offset
			}
		}
		if start >= 0 && offset > start && end == len(raw) && bytes.HasPrefix(text, []byte("## ")) {
			end = offset
		}
		offset += len(line)
	}
	if count != 1 {
		return bad("heading_missing_or_duplicate")
	}
	// Only terminal blank separator lines/newlines may be removed; spaces on
	// a nonblank final line are part of the pinned section.
	section := bytes.TrimRight(raw[start:end], "\r\n")
	if len(section) != 9236 || fmt.Sprintf("%x", sha256.Sum256(section)) != cstASCIISectionSHA {
		return bad("section_digest")
	}
	return section, nil
}

func cstASCIIBoundedFile(path string, limit int64) ([]byte, error) {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			return nil, errors.New("linked_or_unavailable_path")
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	links, err := cstBindingAddedFileLinkCount(path)
	if err != nil || links != 1 {
		return nil, errors.New("nonregular_or_multilink_file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file_size_limit")
	}
	return data, nil
}

// Reuse the selector gate's closed-object/duplicate-key checker at every
// nested object. Unmarshal alone accepts duplicate keys and absent fields.
func cstASCIIJSONShape(raw []byte, kind reflect.Type) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return errors.New("null_wire_value")
	}
	switch kind.Kind() {
	case reflect.Struct:
		names := make([]string, kind.NumField())
		for i := range names {
			names[i] = kind.Field(i).Tag.Get("json")
		}
		fields, err := cstBindingExactDiagnosticObject(raw, names...)
		if err != nil {
			return err
		}
		for i, name := range names {
			if err := cstASCIIJSONShape(fields[name], kind.Field(i).Type); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var rows []json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil {
			return err
		}
		if kind.Kind() == reflect.Array && len(rows) != kind.Len() {
			return errors.New("fixed_array_length")
		}
		for _, row := range rows {
			if err := cstASCIIJSONShape(row, kind.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

// This consumes a raw producer receipt only. It cannot establish native
// provenance or numerical agreement; the caller still owns host settlement.
func cstASCIIConsume(request cstASCIIRequest, input, raw []byte, files [4][]byte) ([4][]byte, error) {
	bad := func(status, reason string) ([4][]byte, error) { return [4][]byte{}, &cstASCIIError{status, reason} }
	if len(raw) == 0 || len(raw) > 1<<20 || len(input) == 0 || len(input) > 128<<10 {
		return bad("BLOCKED:probe_preservation_failed", "wire_size")
	}
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return bad("BLOCKED:probe_preservation_failed", "malformed_receipt")
	}
	var receipt cstASCIIReceipt
	if _, err := cstBindingExactDiagnosticObject(raw, "version", "requestSha256", "status", "selectorReceivingSectionId", "selectorReceivingSectionSha256", "apiTrace", "boundFrames", "prePostHashes", "cstExecutableSha256", "helpSha256", "pointFileBase64", "pointFileSha256", "outputs", "error", "settlement"); err != nil {
		return bad("BLOCKED:probe_preservation_failed", "closed_receipt_shape")
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return bad("BLOCKED:probe_preservation_failed", "receipt_types")
	}
	if receipt.Version != "ASCIIPointCalibrationReceiptV1" {
		return bad("BLOCKED:probe_preservation_failed", "receipt_version")
	}
	if request.SectionID != cstASCIISectionID || request.SectionSHA != cstASCIISectionSHA || receipt.SectionID != request.SectionID || receipt.SectionSHA != request.SectionSHA || receipt.RequestSHA != fmt.Sprintf("%x", sha256.Sum256(input)) {
		return bad("BLOCKED:selector_evidence_missing_or_stale", "receipt_binding")
	}
	if receipt.ExecutableSHA != request.ExecutableSHA || receipt.HelpSHA != cstASCIIHelpSHA {
		return bad("BLOCKED:selector_evidence_missing_or_stale", "executable_or_help")
	}
	if receipt.Status != "PASS:ascii_point_calibration_raw" {
		// Failure receipts can legitimately have partial hashes/frames. They
		// never authorize cleanup or success. Preserve only closed typed causes,
		// never interpolate untrusted vendor strings into a retained diagnostic.
		switch receipt.Status {
		case "BLOCKED:selector_evidence_missing_or_stale", "BLOCKED:selector_binding_stale", "BLOCKED:point_file_grammar_unverified", "BLOCKED:probe_preservation_failed":
			return bad(receipt.Status, "producer_failure")
		default:
			return bad("BLOCKED:probe_preservation_failed", "unknown_producer_status")
		}
	}
	if cstASCIIJSONShape(raw, reflect.TypeOf(receipt)) != nil || receipt.Error != "" {
		return bad("BLOCKED:probe_preservation_failed", "closed_success_receipt")
	}
	if !receipt.Settlement.Closed || !receipt.Settlement.Session || !receipt.Settlement.Process || !receipt.Settlement.Foreign {
		return bad("BLOCKED:probe_preservation_failed", "producer_unsettled")
	}
	if len(request.Frames) != 2 || len(receipt.Frames) != 2 {
		return bad("BLOCKED:selector_binding_stale", "frame_count")
	}
	wantHashes := cstASCIIHashes{ProjectSHA: request.ProjectSHA}
	for i, field := range []string{"E", "H"} {
		want, got := request.Frames[i], receipt.Frames[i]
		if want.Field != field || want.RawFrequency != 3 || got.Field != field || got.Selector != want.Selector || got.SelectedItem != want.Selector || got.ReturnedPath != want.M3DPath || !got.Selected || got.Frequency != 3 || got.M3DSHA != want.M3DSHA || got.RexSHA != want.RexSHA {
			return bad("BLOCKED:selector_binding_stale", "bound_frame_"+field)
		}
		wantHashes.Frames = append(wantHashes.Frames, cstASCIIHashFrame{field, want.M3DSHA, want.RexSHA})
	}
	if !reflect.DeepEqual(receipt.Hashes.Pre, wantHashes) || !reflect.DeepEqual(receipt.Hashes.Post, wantHashes) {
		return bad("BLOCKED:probe_preservation_failed", "pre_post_hashes")
	}
	names := []string{"running_design_environments", "environment-new", "project-open", "running_design_environments", "GetFileFromTreeItem", "GetFileFromTreeItem"}
	for range 2 {
		names = append(names, "SelectTreeItem", "GetSelectedTreeItem", "GetFileFromTreeItem", "GetFieldFrequency")
		for range 2 {
			names = append(names, "Reset", "FileName", "SetPointFile", "Execute")
		}
	}
	names = append(names, "project-close", "environment-close", "running_design_environments")
	if len(receipt.Trace) != len(names) {
		return bad("BLOCKED:probe_preservation_failed", "trace_length")
	}
	for i, name := range names {
		if receipt.Trace[i].Name != name || receipt.Trace[i].Outcome != "finish" {
			return bad("BLOCKED:probe_preservation_failed", fmt.Sprintf("trace_event_%d", i))
		}
	}
	point, err := base64.StdEncoding.Strict().DecodeString(receipt.PointBase64)
	if err != nil || string(point) != cstASCIIPoints || receipt.PointBase64 != request.PointBase64 || receipt.PointSHA != fmt.Sprintf("%x", sha256.Sum256(point)) {
		return bad("BLOCKED:ascii_shape_or_order", "point_bytes")
	}
	if len(receipt.Outputs) != 4 {
		return bad("BLOCKED:ascii_shape_or_order", "output_count")
	}
	var decoded [4][]byte
	for i, output := range receipt.Outputs {
		if output.Field != []string{"E", "E", "H", "H"}[i] || output.Repeat != i%2+1 {
			return bad("BLOCKED:ascii_shape_or_order", "output_order")
		}
		if len(output.RawBase64) > base64.StdEncoding.EncodedLen(64<<10) {
			return bad("BLOCKED:ascii_shape_or_order", "output_size")
		}
		data, err := base64.StdEncoding.Strict().DecodeString(output.RawBase64)
		if err != nil || len(data) == 0 || len(data) > 64<<10 || fmt.Sprintf("%x", sha256.Sum256(data)) != output.SHA {
			return bad("BLOCKED:ascii_shape_or_order", "output_bytes_or_hash")
		}
		if !bytes.Equal(data, files[i]) {
			return bad("BLOCKED:probe_preservation_failed", "output_file_receipt_mismatch")
		}
		decoded[i] = data
	}
	if !bytes.Equal(decoded[0], decoded[1]) || !bytes.Equal(decoded[2], decoded[3]) {
		return bad("BLOCKED:repeat_nondeterministic", "raw_repeat_bytes")
	}
	return decoded, nil
}

func TestCSTSavedFieldASCIIPointCalibrationReceipt(t *testing.T) {
	// Exact producer wire, independently written as JSON rather than marshaling
	// the receiving type; unknown/duplicate keys must not be silently accepted.
	request := cstASCIIRequest{Version: "ASCIIPointCalibrationRequestV1", SectionID: cstASCIISectionID, SectionSHA: cstASCIISectionSHA, ProjectSHA: strings.Repeat("a", 64), ExecutableSHA: strings.Repeat("b", 64), PointBase64: base64.StdEncoding.EncodeToString([]byte(cstASCIIPoints))}
	request.Frames = []cstASCIIFrame{{Field: "E", Selector: `2D/3D Results\E-Field\mcp_line10_E_3GHz [1]`, M3DPath: "probe/Result/mcp_line10_E_3GHz_1,1.m3d", M3DSHA: strings.Repeat("c", 64), RexSHA: strings.Repeat("d", 64), RawFrequency: 3}, {Field: "H", Selector: `2D/3D Results\H-Field\mcp_line10_H_3GHz [1]`, M3DPath: "probe/Result/mcp_line10_H_3GHz_1,1.m3d", M3DSHA: strings.Repeat("e", 64), RexSHA: strings.Repeat("f", 64), RawFrequency: 3}}
	input, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	files := [4][]byte{[]byte("E synthetic raw\r\n"), []byte("E synthetic raw\r\n"), []byte("H synthetic raw\r\n"), []byte("H synthetic raw\r\n")}
	trace := []map[string]string{}
	names := []string{"running_design_environments", "environment-new", "project-open", "running_design_environments", "GetFileFromTreeItem", "GetFileFromTreeItem"}
	for range 2 {
		names = append(names, "SelectTreeItem", "GetSelectedTreeItem", "GetFileFromTreeItem", "GetFieldFrequency")
		for range 2 {
			names = append(names, "Reset", "FileName", "SetPointFile", "Execute")
		}
	}
	names = append(names, "project-close", "environment-close", "running_design_environments")
	for _, name := range names {
		trace = append(trace, map[string]string{"name": name, "outcome": "finish"})
	}
	frames := []map[string]any{}
	hashes := []map[string]any{}
	for _, f := range request.Frames {
		frames = append(frames, map[string]any{"field": f.Field, "selector": f.Selector, "selectedTreeItem": f.Selector, "returnedM3dPath": f.M3DPath, "selectionResult": true, "rawFrequency": 3, "m3dSha256": f.M3DSHA, "rexSha256": f.RexSHA})
		hashes = append(hashes, map[string]any{"field": f.Field, "m3dSha256": f.M3DSHA, "rexSha256": f.RexSHA})
	}
	outputs := []map[string]any{}
	for i, data := range files {
		outputs = append(outputs, map[string]any{"field": []string{"E", "E", "H", "H"}[i], "repeat": i%2 + 1, "rawBase64": base64.StdEncoding.EncodeToString(data), "sha256": fmt.Sprintf("%x", sha256.Sum256(data))})
	}
	wire := map[string]any{"version": "ASCIIPointCalibrationReceiptV1", "requestSha256": fmt.Sprintf("%x", sha256.Sum256(input)), "status": "PASS:ascii_point_calibration_raw", "selectorReceivingSectionId": cstASCIISectionID, "selectorReceivingSectionSha256": cstASCIISectionSHA, "apiTrace": trace, "boundFrames": frames, "prePostHashes": map[string]any{"pre": map[string]any{"projectSha256": request.ProjectSHA, "frames": hashes}, "post": map[string]any{"projectSha256": request.ProjectSHA, "frames": hashes}}, "cstExecutableSha256": request.ExecutableSHA, "helpSha256": cstASCIIHelpSHA, "pointFileBase64": request.PointBase64, "pointFileSha256": "73553c9a992359395cb656d1215d9c91c91532ae3c5a5c52ef895060af2433ba", "outputs": outputs, "error": "", "settlement": map[string]any{"closedWithoutSave": true, "sessionSettled": true, "ownedProcessSettled": true, "foreignPreserved": true}}
	good, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("synthetic_raw_wire_only", func(t *testing.T) {
		got, err := cstASCIIConsume(request, input, good, files)
		if err != nil || !reflect.DeepEqual(got, files) {
			t.Fatalf("raw receipt rejected: %v", err)
		}
	})
	for _, tc := range []struct {
		name   string
		edit   func(map[string]any)
		status string
	}{
		{"stale_request", func(w map[string]any) { w["requestSha256"] = strings.Repeat("0", 64) }, "BLOCKED:selector_evidence_missing_or_stale"},
		{"wrong_section_id", func(w map[string]any) { w["selectorReceivingSectionId"] = "## " + cstASCIISectionID }, "BLOCKED:selector_evidence_missing_or_stale"},
		{"foreign_frame", func(w map[string]any) { w["boundFrames"].([]any)[0].(map[string]any)["selector"] = "foreign" }, "BLOCKED:selector_binding_stale"},
		{"wrong_selected_readback", func(w map[string]any) { w["boundFrames"].([]any)[1].(map[string]any)["selectedTreeItem"] = "foreign" }, "BLOCKED:selector_binding_stale"},
		{"false_selection", func(w map[string]any) { w["boundFrames"].([]any)[1].(map[string]any)["selectionResult"] = false }, "BLOCKED:selector_binding_stale"},
		{"unsettled", func(w map[string]any) { w["settlement"].(map[string]any)["ownedProcessSettled"] = false }, "BLOCKED:probe_preservation_failed"},
		{"forbidden_call", func(w map[string]any) { w["apiTrace"].([]any)[4].(map[string]any)["name"] = "GetFieldVector" }, "BLOCKED:probe_preservation_failed"},
		{"changed_hash", func(w map[string]any) {
			w["prePostHashes"].(map[string]any)["post"].(map[string]any)["projectSha256"] = strings.Repeat("0", 64)
		}, "BLOCKED:probe_preservation_failed"},
		{"missing_field", func(w map[string]any) { delete(w, "helpSha256") }, "BLOCKED:probe_preservation_failed"},
		{"unknown_field", func(w map[string]any) { w["unexpected"] = true }, "BLOCKED:probe_preservation_failed"},
		{"wrong_repeat", func(w map[string]any) { w["outputs"].([]any)[1].(map[string]any)["repeat"] = 1 }, "BLOCKED:ascii_shape_or_order"},
		{"producer_grammar_failure", func(w map[string]any) {
			w["status"] = "BLOCKED:point_file_grammar_unverified"
			w["error"] = "E"
			w["prePostHashes"] = map[string]any{}
			w["outputs"] = []any{}
		}, "BLOCKED:point_file_grammar_unverified"},
		{"output_oversized", func(w map[string]any) {
			w["outputs"].([]any)[0].(map[string]any)["rawBase64"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("0"), (64<<10)+1))
		}, "BLOCKED:ascii_shape_or_order"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w map[string]any
			if err := json.Unmarshal(good, &w); err != nil {
				t.Fatal(err)
			}
			tc.edit(w)
			raw, err := json.Marshal(w)
			if err != nil {
				t.Fatal(err)
			}
			_, err = cstASCIIConsume(request, input, raw, files)
			var failure *cstASCIIError
			if !errors.As(err, &failure) || failure.Status != tc.status {
				t.Fatalf("want %s got %v", tc.status, err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"partial", good[:len(good)/2]}, {"oversized", bytes.Repeat([]byte(" "), (1<<20)+1)}, {"trailing_json", append(append([]byte(nil), good...), []byte("{}")...)}, {"duplicate_key", bytes.Replace(good, []byte(`"version":`), []byte(`"status":"PASS:ascii_point_calibration_raw","version":`), 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cstASCIIConsume(request, input, tc.raw, files); err == nil {
				t.Fatal("invalid receipt accepted")
			}
		})
	}
	t.Run("unknown_status_not_echoed", func(t *testing.T) {
		var w map[string]any
		if err := json.Unmarshal(good, &w); err != nil {
			t.Fatal(err)
		}
		w["status"] = "untrusted-private-value"
		raw, err := json.Marshal(w)
		if err != nil {
			t.Fatal(err)
		}
		_, err = cstASCIIConsume(request, input, raw, files)
		if err == nil || strings.Contains(err.Error(), "untrusted-private-value") {
			t.Fatalf("unsafe status diagnostic: %v", err)
		}
	})
}

func TestCSTSavedFieldASCIIPointCalibrationSection(t *testing.T) {
	root := os.Getenv("MCPHUB_CST_ASCII_WORK_ITEM")
	if root == "" {
		t.Skip("explicit work-item root required for immutable native section fixture")
	}
	section, err := cstASCIISection(root)
	if err != nil || len(section) != 9236 {
		t.Fatalf("pinned section: bytes=%d error=%v", len(section), err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		ok   bool
	}{
		{"exact", append(append([]byte(nil), section...), []byte("\n\n## later unrelated section\nanything\n")...), true},
		{"missing", []byte("## unrelated\n"), false},
		{"changed", append(append([]byte(nil), section...), byte('x')), false},
		{"duplicate", append(append(append([]byte(nil), section...), []byte("\n\n")...), section...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cstProbeWriteFile(t, filepath.Join(dir, "implementation-s5h-native-producer-d1-discovery.md"), string(tc.data))
			_, err := cstASCIISection(dir)
			if (err == nil) != tc.ok {
				t.Fatalf("section admission want %v: %v", tc.ok, err)
			}
		})
	}
}

type cstASCIIBundleGuard struct {
	Project      string
	Full, Result cstProbeManifest
	Directories  []string
	Pairs        []cstBindingPair
}

func cstASCIIBundleUnchanged(guard cstASCIIBundleGuard) error {
	bad := func(reason string) error { return &cstASCIIError{"BLOCKED:probe_preservation_failed", reason} }
	if len(guard.Full.Rows) == 0 || len(guard.Result.Rows) == 0 || len(guard.Pairs) != 4 || len(guard.Directories) == 0 {
		return bad("missing_bundle_baseline")
	}
	projectRow, err := cstNativeManifestExactRow(guard.Full, filepath.Base(guard.Project))
	if err != nil {
		return bad("project_baseline_invalid")
	}
	reasons := cstNativeObserveRetained(guard.Project, guard.Full, guard.Result, projectRow.SHA256, guard.Pairs)
	directories, err := cstBindingDirectoryInventory(guard.Project)
	if err != nil || !reflect.DeepEqual(directories, guard.Directories) {
		reasons = append(reasons, fmt.Sprintf("directory_drift:%v", err))
	}
	for i, pair := range guard.Pairs {
		if pair.Field != []string{"E", "E", "H", "H"}[i] || pair.Port != i%2+1 {
			reasons = append(reasons, "four_pair_order")
		}
		for _, target := range []struct{ path, digest string }{{pair.M3DPath, pair.M3DSHA256}, {pair.RexPath, pair.RexSHA256}} {
			if filepath.IsAbs(target.path) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(target.path))) != target.path || strings.HasPrefix(target.path, "../") {
				reasons = append(reasons, "pair_path_escape")
			}
		}
	}
	if len(reasons) != 0 {
		return bad(strings.Join(reasons, ";"))
	}
	return nil
}

// Synthetic harness admission only: source/copy baselines are supplied by its
// fixture. This is not the frozen 251-row native admission or an executable,
// help, mesh/case-file, retained-bundle, or foreign-session native census.
func cstASCIIPreflight(workItem string, request cstASCIIRequest, input []byte, source, copy cstASCIIBundleGuard, ports [2]int, helper, executable string) error {
	bad := func(reason string) error { return &cstASCIIError{"BLOCKED:selector_evidence_missing_or_stale", reason} }
	if _, err := cstASCIISection(workItem); err != nil {
		return err
	}
	if len(input) == 0 || len(input) > 128<<10 || request.Version != "ASCIIPointCalibrationRequestV1" || request.SectionID != cstASCIISectionID || request.SectionSHA != cstASCIISectionSHA {
		return bad("request_identity")
	}
	var decoded cstASCIIRequest
	if !json.Valid(input) || cstASCIIJSONShape(input, reflect.TypeOf(decoded)) != nil || json.Unmarshal(input, &decoded) != nil || !reflect.DeepEqual(decoded, request) {
		return bad("request_wire_binding")
	}
	if request.CopyRoot != filepath.Dir(copy.Project) || source.Project == copy.Project || !filepath.IsAbs(request.CopyRoot) {
		return bad("source_copy_ownership")
	}
	if err := cstASCIIBundleUnchanged(source); err != nil {
		return bad("source:" + err.Error())
	}
	if err := cstASCIIBundleUnchanged(copy); err != nil {
		return bad("copy:" + err.Error())
	}
	if request.FullSHA != source.Full.AggregateSHA256 || request.ResultSHA != source.Result.AggregateSHA256 || !reflect.DeepEqual(source.Pairs, copy.Pairs) || len(source.Full.Rows) != len(copy.Full.Rows) {
		return bad("source_copy_projection")
	}
	// Copy filesystem timestamps may differ in the synthetic fixture; content,
	// membership, stream kind and independent per-copy timestamp baselines may not.
	for i, row := range source.Full.Rows {
		other := copy.Full.Rows[i]
		if row.Path != other.Path || row.SHA256 != other.SHA256 || row.Size != other.Size || row.Type != other.Type || row.Stream != other.Stream {
			return bad("source_copy_row")
		}
	}
	for _, guard := range []cstASCIIBundleGuard{source, copy} {
		row, err := cstNativeManifestExactRow(guard.Full, filepath.Base(guard.Project))
		if err != nil || row.SHA256 != request.ProjectSHA {
			return bad("source_or_copy_project_hash")
		}
	}
	for _, target := range []struct{ path, digest string }{{helper, request.HelperSHA}, {executable, request.ExecutableSHA}} {
		data, err := cstASCIIBoundedFile(target.path, 128<<20)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(data)) != target.digest {
			return bad("source_or_tool_hash")
		}
	}
	if request.MeshSHA != "27e10d39875077ae858f099d986a0b51945696edd0540a4dd13aac2dc1d6866a" || request.CaseSHA != "debdb9ca1294c0c0c2bd060edea5ac93b634d2dc4e9cc70ac3cc388ca7b3f858" {
		return bad("coordinate_provenance")
	}
	if len(request.Frames) != 2 {
		return bad("frame_count")
	}
	wantFrames, err := cstASCIISelectFrames(source, ports)
	if err != nil {
		return bad("source_selector_metadata")
	}
	if !reflect.DeepEqual(request.Frames, wantFrames) {
		return bad("frame_projection")
	}
	wantPoints := []cstASCIIPoint{{"p1:t19:f3:q0", [3]string{"0", "7.5184627019783594", "6.1002692254485309"}}, {"p2:t11613:f2:q0", [3]string{"10", "6.3645824908535733", "0.20448276178909414"}}}
	if !reflect.DeepEqual(request.Points, wantPoints) || request.PointBase64 != base64.StdEncoding.EncodeToString([]byte(cstASCIIPoints)) {
		return bad("point_provenance")
	}
	return nil
}

func TestCSTSavedFieldASCIIPointCalibrationContainedFake(t *testing.T) {
	python := cstProbeTestPython(t)
	item := os.Getenv("MCPHUB_CST_ASCII_WORK_ITEM")
	if item == "" {
		t.Skip("explicit section fixture required")
	}
	helper := filepath.Join(cstProbeRepositoryRoot(t), "servers", "electromagnetics-mcp", "tests", "_helpers", "cst_saved_field_ascii_point_calibration.py")
	// Only this fixed fake interface is called. This script cannot load CST's
	// native interface, and its fake sessions have no external process lifetime.
	script := `import importlib.util,json,sys,time
from pathlib import Path
spec=importlib.util.spec_from_file_location("ascii_fixture",sys.argv[1])
m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
raw=sys.stdin.buffer.read(131073);request=json.loads(raw)
class Fake(m._FakeCST):
    def GetFileFromTreeItem(self,selector):
        self.calls.append("GetFileFromTreeItem")
        return {frame["selector"]: frame["m3dPath"] for frame in request["frames"]}[selector]
    def Execute(self):
        if sys.argv[2]=="timeout":
            sys.stdout.write('{"version":');sys.stdout.flush();time.sleep(30)
        if sys.argv[2]=="partial":
            sys.stdout.write('{"version":');sys.stdout.flush();raise SystemExit
        if sys.argv[2]=="wire":
            return super().Execute()
        prefix="0 7.5184627019783594 6.1002692254485309 "
        second="10 6.3645824908535733 0.20448276178909414 "
        values=("4 1 6 2 5 3","14 11 16 12 15 13") if "E-Field" in self.current_selector else ("24 21 26 22 25 23","34 31 36 32 35 33")
        Path(self.output).write_bytes((prefix+values[0]+"\r\n"+second+values[1]+"\r\n").encode("ascii"))
fake=Fake(Path(request["copyRoot"]))
receipt=m._execute(request,raw,fake)
if sys.argv[2]!="partial":
    sys.stdout.write(json.dumps(receipt,separators=(",",":"),ensure_ascii=False)+"\n")
`
	for _, mode := range []string{"success", "alternate_h2", "wire", "partial", "timeout", "stale_source", "stale_pair", "wrong_id", "extra_coordinate"} {
		t.Run(mode, func(t *testing.T) {
			// MkdirTemp, not t.TempDir: no unconditional deferred deletion is
			// permitted if host settlement or bundle identity becomes uncertain.
			root, err := os.MkdirTemp("", "cst-result-tree-probe-ascii-")
			if err != nil {
				t.Fatal(err)
			}
			settled := false
			defer func() {
				if settled {
					cstProbeRemoveOwnedRoot(t, root, filepath.Dir(root))
				} else {
					t.Errorf("synthetic root quarantined (settlement unproven): %s", root)
				}
			}()
			makeBundle := func(parent string) cstASCIIBundleGuard {
				project := filepath.Join(parent, "probe.cst")
				cstProbeWriteFile(t, project, "copied project")
				var pairs []cstBindingPair
				for _, field := range []string{"E", "H"} {
					for port := 1; port <= 2; port++ {
						base := fmt.Sprintf("probe/Result/mcp_line10_%s_3GHz_%d,1", field, port)
						m3d, rex := base+".m3d", base+"_m3d.rex"
						cstProbeWriteFile(t, filepath.Join(parent, filepath.FromSlash(m3d)), fmt.Sprintf("%s payload %d", field, port))
						runID := port
						if mode == "alternate_h2" && field == "H" && port == 2 {
							runID = 7
						}
						cstProbeWriteFile(t, filepath.Join(parent, filepath.FromSlash(rex)), fmt.Sprintf(`<MetaResultFile><SimulationProperties fieldname="mcp_line10_%s_3GHz [%d]"/></MetaResultFile>`, field, runID))
						pairs = append(pairs, cstBindingPair{Field: field, Port: port, M3DPath: m3d, RexPath: rex, M3DSHA256: cstProbeFileSHA256(t, filepath.Join(parent, filepath.FromSlash(m3d))), RexSHA256: cstProbeFileSHA256(t, filepath.Join(parent, filepath.FromSlash(rex)))})
					}
				}
				full, err := cstBindingFullManifest(project)
				if err != nil {
					t.Fatal(err)
				}
				result, err := cstBindingResultInventory(project)
				if err != nil {
					t.Fatal(err)
				}
				return cstASCIIBundleGuard{project, full, result, cstBindingFrozenDirectories(t, project), pairs}
			}
			source := makeBundle(filepath.Join(root, "source"))
			copyGuard := makeBundle(filepath.Join(root, "copy"))
			request := cstASCIIRequest{Version: "ASCIIPointCalibrationRequestV1", CopyRoot: filepath.Dir(copyGuard.Project), ProjectSHA: cstProbeFileSHA256(t, source.Project), FullSHA: source.Full.AggregateSHA256, ResultSHA: source.Result.AggregateSHA256, SectionID: cstASCIISectionID, SectionSHA: cstASCIISectionSHA, MeshSHA: "27e10d39875077ae858f099d986a0b51945696edd0540a4dd13aac2dc1d6866a", CaseSHA: "debdb9ca1294c0c0c2bd060edea5ac93b634d2dc4e9cc70ac3cc388ca7b3f858", ExecutableSHA: cstProbeFileSHA256(t, python), HelperSHA: cstProbeFileSHA256(t, helper), PointBase64: base64.StdEncoding.EncodeToString([]byte(cstASCIIPoints)), Points: []cstASCIIPoint{{"p1:t19:f3:q0", [3]string{"0", "7.5184627019783594", "6.1002692254485309"}}, {"p2:t11613:f2:q0", [3]string{"10", "6.3645824908535733", "0.20448276178909414"}}}}
			indexes := []int{0, 2}
			if mode == "alternate_h2" {
				indexes = []int{0, 3}
			}
			for _, index := range indexes {
				p := source.Pairs[index]
				selector, _, err := cstBindingExpectedSelector(source.Project, source.Full, p)
				if err != nil {
					t.Fatal(err)
				}
				request.Frames = append(request.Frames, cstASCIIFrame{p.Field, selector, p.M3DPath, p.M3DSHA256, p.RexPath, p.RexSHA256, 3})
			}
			if mode == "alternate_h2" && (request.Frames[1].Selector != `2D/3D Results\H-Field\mcp_line10_H_3GHz [7]` || request.Frames[1].RexSHA != source.Pairs[3].RexSHA256) {
				t.Fatal("alternate H frame is not bound to source H port two metadata")
			}
			if mode == "wrong_id" {
				request.SectionID = "## " + request.SectionID
			}
			var changedPath string
			var original []byte
			var originalTime time.Time
			if mode == "stale_source" || mode == "stale_pair" {
				changedPath = source.Project
				if mode == "stale_pair" {
					changedPath = filepath.Join(filepath.Dir(source.Project), filepath.FromSlash(source.Pairs[3].M3DPath))
				}
				original, err = os.ReadFile(changedPath)
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(changedPath)
				if err != nil {
					t.Fatal(err)
				}
				originalTime = info.ModTime()
				cstProbeWriteFile(t, changedPath, "intentional synthetic drift")
			}
			input, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "extra_coordinate" {
				input = bytes.Replace(input, []byte(`"6.1002692254485309"]`), []byte(`"6.1002692254485309","ignored"]`), 1)
			}
			ports := [2]int{1, 1}
			if mode == "alternate_h2" {
				ports = [2]int{1, 2}
			}
			admission := cstASCIIPreflight(item, request, input, source, copyGuard, ports, helper, python)
			if changedPath != "" {
				cstProbeWriteFile(t, changedPath, string(original))
				if err := os.Chtimes(changedPath, originalTime, originalTime); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "stale_source" || mode == "stale_pair" || mode == "wrong_id" || mode == "extra_coordinate" {
				settled = cstASCIIBundleUnchanged(source) == nil && cstASCIIBundleUnchanged(copyGuard) == nil
				if admission == nil {
					t.Fatal("invalid preflight would launch child")
				}
				return
			}
			if admission != nil {
				settled = cstASCIIBundleUnchanged(source) == nil && cstASCIIBundleUnchanged(copyGuard) == nil
				t.Fatal(admission)
			}
			cmd := exec.Command(python, "-B", "-c", script, helper, mode)
			cmd.Dir = request.CopyRoot
			cmd.Env = cstProbeMinimalEnvironment(python)
			deadline := 10 * time.Second
			if mode == "timeout" {
				deadline = time.Second
			}
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			output, runErr := hubprocess.RunStrictlyContained(ctx, hubprocess.StrictRunInvocation{Command: cmd, Input: input, InputLimit: 128 << 10, StdoutLimit: 1 << 20, StderrLimit: 64 << 10})
			cancel()
			// The fixed fake creates no descendants/native sessions. A joined
			// Python process plus both independent bundle checks proves this
			// synthetic root disposable, even when its receipt is partial.
			settled = cmd.ProcessState != nil && cmd.ProcessState.Exited() && cstASCIIBundleUnchanged(source) == nil && cstASCIIBundleUnchanged(copyGuard) == nil
			if !settled {
				t.Fatal("host settlement or bundle preservation unproven")
			}
			if mode == "timeout" {
				var strict *hubprocess.StrictRunError
				if !errors.As(runErr, &strict) || strict.Kind != hubprocess.StrictRunTimeout || len(output.Stdout.Prefix) == 0 {
					t.Fatalf("timeout/partial receipt guard: %v bytes=%d", runErr, len(output.Stdout.Prefix))
				}
				return
			}
			if runErr != nil || output.Stdout.Truncated || output.Stderr.Truncated || output.Stderr.Bytes != 0 {
				t.Fatalf("contained fake: %v stdout=%d stderr=%d", runErr, output.Stdout.Bytes, output.Stderr.Bytes)
			}
			var files [4][]byte
			if mode != "partial" {
				for i, field := range []string{"E", "E", "H", "H"} {
					files[i], err = cstASCIIBoundedFile(filepath.Join(request.CopyRoot, fmt.Sprintf("ascii-calibration-%s-%d.txt", field, i%2+1)), 64<<10)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			raw, err := cstASCIIConsume(request, input, output.Stdout.Prefix, files)
			if mode == "partial" {
				if err == nil {
					t.Fatal("partial receipt accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := cstASCIIParse(raw)
			if mode == "wire" {
				var blocked *cstASCIIError
				if !errors.As(err, &blocked) || blocked.Status != "BLOCKED:ascii_header_unverified" {
					t.Fatalf("producer fake header: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			query := [][6]float64{{1, 2, 3, 4, 5, 6}, {11, 12, 13, 14, 15, 16}, {21, 22, 23, 24, 25, 26}, {31, 32, 33, 34, 35, 36}}
			if got := cstASCIIBijection(parsed.Values, query, parsed.Tokens); got.Status != "PASS:ascii_component_bijection_synthetic" {
				t.Fatalf("synthetic comparator: %+v", got)
			}
		})
	}
}

type cstASCIICalibrationResult struct {
	Status, Reason                                   string
	Permutation                                      [6]int // ASCII column -> canonical ReX,ReY,ReZ,ImX,ImY,ImZ slot.
	ExactMaps, OneCellMaps, MinMismatch, Comparisons int
	Mismatch                                         *cstASCIIMismatch
}

type cstASCIIParsed struct {
	Raw            [4][]byte
	Values         [][6]float64
	Tokens         [][6]string
	CoordinateUnit string
	InBandUnit     string // Empty is absent, never inferred from the field kind.
}

func cstASCIIParse(raw [4][]byte) (cstASCIIParsed, error) {
	parsed := cstASCIIParsed{Raw: raw, Values: make([][6]float64, 4), Tokens: make([][6]string, 4)}
	bad := func(status, reason string) (cstASCIIParsed, error) { return parsed, &cstASCIIError{status, reason} }
	if !bytes.Equal(raw[0], raw[1]) || !bytes.Equal(raw[2], raw[3]) {
		return bad("BLOCKED:repeat_nondeterministic", "raw_repeat_bytes")
	}
	decimal := regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)
	mm := [2][3]float64{{0, 7.5184627019783594, 6.1002692254485309}, {10, 6.3645824908535733, 0.20448276178909414}}
	matches := [2]bool{true, true}
	for f, data := range raw {
		if len(data) == 0 || len(data) > 64<<10 {
			return bad("BLOCKED:ascii_shape_or_order", "file_size")
		}
		for i, b := range data {
			if b >= 128 || (b < 32 && b != '\t' && b != '\r' && b != '\n') || b == 127 {
				return bad("BLOCKED:ascii_header_unverified", "non_ascii_or_control")
			}
			if b == '\r' && (i+1 == len(data) || data[i+1] != '\n') {
				return bad("BLOCKED:ascii_shape_or_order", "bare_cr")
			}
		}
		// Slicing a parsing view does not rewrite the separately retained raw.
		body := string(data)
		body = strings.TrimSuffix(body, "\n")
		body = strings.TrimSuffix(body, "\r")
		lines := strings.Split(body, "\n")
		rows := make([][]string, len(lines))
		for r, line := range lines {
			line = strings.TrimSuffix(line, "\r")
			rows[r] = strings.FieldsFunc(line, func(c rune) bool { return c == ' ' || c == '\t' })
			for _, token := range rows[r] {
				if !decimal.MatchString(token) {
					return bad("BLOCKED:ascii_header_unverified", "unadmitted_token")
				}
			}
		}
		if len(rows) != 2 {
			return bad("BLOCKED:ascii_shape_or_order", "row_count")
		}
		for p, tokens := range rows {
			if len(tokens) != 9 {
				return bad("BLOCKED:ascii_shape_or_order", "column_count")
			}
			for j, token := range tokens {
				value, err := strconv.ParseFloat(token, 64)
				if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
					return bad("BLOCKED:ascii_shape_or_order", "nonfinite_or_unparseable_decimal")
				}
				if j < 3 {
					matches[0] = matches[0] && math.Float64bits(value) == math.Float64bits(mm[p][j])
					matches[1] = matches[1] && math.Float64bits(value) == math.Float64bits(cstASCIIMillimetresToMetres(mm[p][j]))
				} else if f%2 == 0 {
					parsed.Values[f+p][j-3] = value
					parsed.Tokens[f+p][j-3] = token
				}
			}
		}
	}
	if matches[0] == matches[1] {
		return bad("BLOCKED:ascii_shape_or_order", "coordinate_unit_or_order")
	}
	parsed.CoordinateUnit = "mm"
	if matches[1] {
		parsed.CoordinateUnit = "m"
	}
	return parsed, nil
}

func TestCSTSavedFieldASCIIPointCalibrationParser(t *testing.T) {
	e := "0 7.5184627019783594 6.1002692254485309 4 1 6 2 5 3\r\n10 6.3645824908535733 0.20448276178909414 14 11 16 12 15 13\r\n"
	h := "0 7.5184627019783594 6.1002692254485309 24 21 26 22 25 23\r\n10 6.3645824908535733 0.20448276178909414 34 31 36 32 35 33\r\n"
	good := [4][]byte{[]byte(e), []byte(e), []byte(h), []byte(h)}
	t.Run("headerless_synthetic_pipeline", func(t *testing.T) {
		parsed, err := cstASCIIParse(good)
		if err != nil {
			t.Fatal(err)
		}
		q := [][6]float64{{1, 2, 3, 4, 5, 6}, {11, 12, 13, 14, 15, 16}, {21, 22, 23, 24, 25, 26}, {31, 32, 33, 34, 35, 36}}
		got := cstASCIIBijection(parsed.Values, q, parsed.Tokens)
		if got.Status != "PASS:ascii_component_bijection_synthetic" || parsed.CoordinateUnit != "mm" || parsed.InBandUnit != "" || !reflect.DeepEqual(parsed.Raw, good) {
			t.Fatalf("synthetic pipeline: %+v unit=%s", got, parsed.CoordinateUnit)
		}
	})
	for _, tc := range []struct{ name, body, status string }{
		{"producer_fake", "# fake\r\n0 1 2 3 4 5 6 7 8\r\n", "BLOCKED:ascii_header_unverified"},
		{"plausible_header", "E V/m PEAK 3 GHz\n" + e, "BLOCKED:ascii_header_unverified"},
		{"bom", "\xef\xbb\xbf" + e, "BLOCKED:ascii_header_unverified"},
		{"non_ascii", "µ" + e, "BLOCKED:ascii_header_unverified"},
		{"nan", strings.Replace(e, "4 1 6", "NaN 1 6", 1), "BLOCKED:ascii_header_unverified"},
		{"overflow", strings.Replace(e, "4 1 6", "1e999 1 6", 1), "BLOCKED:ascii_shape_or_order"},
		{"hex", strings.Replace(e, "4 1 6", "0x1p2 1 6", 1), "BLOCKED:ascii_header_unverified"},
		{"bare_cr", strings.ReplaceAll(e, "\r\n", "\r"), "BLOCKED:ascii_shape_or_order"},
		{"blank_line", e + "\r\n", "BLOCKED:ascii_shape_or_order"},
		{"eight_columns", strings.Replace(e, "4 1 6", "4 1", 1), "BLOCKED:ascii_shape_or_order"},
		{"duplicate_point", strings.Split(e, "\r\n")[0] + "\r\n" + strings.Split(e, "\r\n")[0] + "\r\n", "BLOCKED:ascii_shape_or_order"},
		{"swapped_points", strings.Split(e, "\r\n")[1] + "\r\n" + strings.Split(e, "\r\n")[0] + "\r\n", "BLOCKED:ascii_shape_or_order"},
		{"rounded_coordinate", strings.Replace(e, "0.20448276178909414", "0.2044827617890941", 1), "BLOCKED:ascii_shape_or_order"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := good
			raw[0], raw[1] = []byte(tc.body), []byte(tc.body)
			parsed, err := cstASCIIParse(raw)
			var failure *cstASCIIError
			if !errors.As(err, &failure) || failure.Status != tc.status || !reflect.DeepEqual(parsed.Raw, raw) {
				t.Fatalf("want %s retained raw: %v", tc.status, err)
			}
		})
	}
	t.Run("repeat_drift", func(t *testing.T) {
		raw := good
		raw[1] = []byte(strings.ReplaceAll(e, "\r\n", "\n"))
		_, err := cstASCIIParse(raw)
		var failure *cstASCIIError
		if !errors.As(err, &failure) || failure.Status != "BLOCKED:repeat_nondeterministic" {
			t.Fatal(err)
		}
	})
	t.Run("metre_exact_rational_independent_oracle", func(t *testing.T) {
		// big.Rat independently divides the exact binary64 input ratio by the
		// integer 1000 and rounds its rational directly to nearest binary64.
		var coordinates [2][3]string
		for p, row := range [2][3]float64{{0, 7.5184627019783594, 6.1002692254485309}, {10, 6.3645824908535733, 0.20448276178909414}} {
			for j, v := range row {
				r := new(big.Rat).SetFloat64(v)
				r.Quo(r, big.NewRat(1000, 1))
				want, _ := r.Float64()
				got := cstASCIIMillimetresToMetres(v)
				if math.Float64bits(got) != math.Float64bits(want) {
					t.Fatalf("rational mismatch %d/%d", p, j)
				}
				coordinates[p][j] = strconv.FormatFloat(want, 'g', 17, 64)
			}
		}
		converted := good
		for i, data := range good {
			lines := strings.Split(strings.TrimSuffix(string(data), "\r\n"), "\r\n")
			for p := range lines {
				fields := strings.Fields(lines[p])
				copy(fields[:3], coordinates[p][:])
				lines[p] = strings.Join(fields, " ")
			}
			converted[i] = []byte(strings.Join(lines, "\n"))
		}
		parsed, err := cstASCIIParse(converted)
		if err != nil || parsed.CoordinateUnit != "m" {
			t.Fatalf("metre echo: %v", err)
		}
		wrongRound := converted
		for i, data := range converted {
			wrongRound[i] = bytes.Replace(data, []byte(coordinates[1][2]), []byte(strconv.FormatFloat(math.Float64frombits(0x3f2acd4d8c88a1bd), 'g', 17, 64)), 1)
		}
		if _, err := cstASCIIParse(wrongRound); err == nil {
			t.Fatal("rounded-multiplier metre echo accepted")
		}
		converted[2], converted[3] = good[2], good[3]
		if _, err := cstASCIIParse(converted); err == nil {
			t.Fatal("mixed units accepted")
		}
	})
}

func cstASCIIBijection(ascii, query [][6]float64, tokens [][6]string) cstASCIICalibrationResult {
	result := cstASCIICalibrationResult{MinMismatch: 24}
	if len(query) != 4 {
		result.Status, result.Reason = "BLOCKED:comparison_prerequisite", "four_query_rows_required"
		return result
	}
	if len(ascii) != 4 || len(tokens) != 4 {
		result.Status, result.Reason = "BLOCKED:ascii_shape_or_order", "four_ascii_rows_required"
		return result
	}
	for r := range ascii {
		for j, value := range ascii[r] {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				result.Status, result.Reason = "BLOCKED:ascii_shape_or_order", "nonfinite_ascii"
				return result
			}
			if math.IsNaN(query[r][j]) || math.IsInf(query[r][j], 0) {
				result.Status, result.Reason = "BLOCKED:comparison_prerequisite", "nonfinite_query"
				return result
			}
			parsed, err := strconv.ParseFloat(tokens[r][j], 64)
			if err != nil || math.Float64bits(parsed) != math.Float64bits(value) {
				result.Status, result.Reason = "BLOCKED:ascii_shape_or_order", "raw_token_mismatch"
				return result
			}
		}
	}
	permutation := [6]int{0, 1, 2, 3, 4, 5}
	var oneCell cstASCIIMismatch
	for {
		mismatches := 0
		var cell cstASCIIMismatch
		for r := range ascii {
			for j, k := range permutation {
				result.Comparisons++
				left, right := math.Float64bits(ascii[r][j]), math.Float64bits(query[r][k])
				if left != right {
					mismatches++
					cell = cstASCIIMismatch{Row: r, Column: j, Slot: k, RawToken: tokens[r][j], ASCII17: strconv.FormatFloat(ascii[r][j], 'g', 17, 64), Query17: strconv.FormatFloat(query[r][k], 'g', 17, 64), ASCIIBits: left, QueryBits: right}
				}
			}
		}
		if mismatches < result.MinMismatch {
			result.MinMismatch = mismatches
		}
		if mismatches == 0 {
			result.ExactMaps++
			result.Permutation = permutation
		}
		if mismatches == 1 {
			result.OneCellMaps++
			oneCell = cell
		}
		// Lexicographic next permutation, so every one of 6! maps is checked.
		i := 4
		for i >= 0 && permutation[i] >= permutation[i+1] {
			i--
		}
		if i < 0 {
			break
		}
		j := 5
		for permutation[j] <= permutation[i] {
			j--
		}
		permutation[i], permutation[j] = permutation[j], permutation[i]
		for l, r := i+1, 5; l < r; l, r = l+1, r-1 {
			permutation[l], permutation[r] = permutation[r], permutation[l]
		}
	}
	result.Status = "BLOCKED:component_permutation_ambiguous"
	if result.ExactMaps > 1 {
		result.Reason = "multiple_exact_maps"
		return result
	}
	if result.ExactMaps == 0 {
		result.Reason = "zero_exact_maps"
		if result.OneCellMaps == 1 {
			result.Status, result.Reason = "BLOCKED:exact_round_trip_failed", "unique_one_cell_mismatch"
			mantissa := strings.TrimLeft(strings.FieldsFunc(oneCell.RawToken, func(r rune) bool { return r == 'e' || r == 'E' })[0], "+-")
			digits := strings.TrimLeft(strings.ReplaceAll(mantissa, ".", ""), "0")
			oneCell.SignificantDigits = len(digits)
			if oneCell.SignificantDigits == 0 {
				oneCell.SignificantDigits = 1
			}
			result.Mismatch = &oneCell
		}
		return result
	}
	for f, field := range []string{"E", "H"} {
		witness := false
		for r := 2 * f; r < 2*f+2; r++ {
			for k := 3; k < 6; k++ {
				v := query[r][k]
				witness = witness || (v != 0 && math.Float64bits(v) != math.Float64bits(-v))
			}
		}
		if !witness {
			result.Status, result.Reason = "BLOCKED:component_witness_insufficient", field
			return result
		}
	}
	result.Status = "PASS:ascii_component_bijection_synthetic"
	return result
}

func cstASCIIMillimetresToMetres(value float64) float64 {
	// 1000 is exact in binary64. Division rounds the exact rational quotient
	// once; multiplying by the rounded binary64 0.001 does not.
	return value / 1000
}

func TestCSTSavedFieldASCIIPointCalibration(t *testing.T) {
	// Independent column signatures: slot k has (k+1,k+11,k+21,k+31).
	// None of the expected permutations are computed by the oracle under test.
	baseline := [][6]float64{{1, 2, 3, 4, 5, 6}, {11, 12, 13, 14, 15, 16}, {21, 22, 23, 24, 25, 26}, {31, 32, 33, 34, 35, 36}}
	permuted := [][6]float64{{4, 1, 6, 2, 5, 3}, {14, 11, 16, 12, 15, 13}, {24, 21, 26, 22, 25, 23}, {34, 31, 36, 32, 35, 33}}
	tokensFor := func(values [][6]float64) [][6]string {
		tokens := make([][6]string, len(values))
		for r := range values {
			for j, v := range values[r] {
				tokens[r][j] = strconv.FormatFloat(v, 'g', 17, 64)
			}
		}
		return tokens
	}
	t.Run("synthetic_unique_common_map", func(t *testing.T) {
		got := cstASCIIBijection(permuted, baseline, tokensFor(permuted))
		if got.Status != "PASS:ascii_component_bijection_synthetic" || got.Permutation != [6]int{3, 0, 5, 1, 4, 2} || got.ExactMaps != 1 || got.Comparisons != 17280 {
			t.Fatalf("unique synthetic map: %+v", got)
		}
	})
	for _, tc := range []struct {
		name           string
		mutate         func([][6]float64, [][6]float64)
		status, reason string
	}{
		{"one_bit", func(a, q [][6]float64) { a[0][0] = math.Nextafter(a[0][0], math.Inf(1)) }, "BLOCKED:exact_round_trip_failed", "unique_one_cell_mismatch"},
		{"two_cells", func(a, q [][6]float64) { a[0][0] = 7; a[1][0] = 17 }, "BLOCKED:component_permutation_ambiguous", "zero_exact_maps"},
		{"multiple_maps", func(a, q [][6]float64) {
			for r := range a {
				a[r][1] = a[r][0]
				q[r][1] = q[r][0]
			}
		}, "BLOCKED:component_permutation_ambiguous", "multiple_exact_maps"},
		{"E_signed_zero_witness", func(a, q [][6]float64) {
			for r := 0; r < 2; r++ {
				for j := 3; j < 6; j++ {
					v := math.Copysign(0, -1)
					if (r+j)%2 == 0 {
						v = 0
					}
					a[r][j] = v
					q[r][j] = v
				}
			}
		}, "BLOCKED:component_witness_insufficient", "E"},
		{"H_signed_zero_witness", func(a, q [][6]float64) {
			for r := 2; r < 4; r++ {
				for j := 3; j < 6; j++ {
					a[r][j] = 0
					q[r][j] = 0
				}
			}
		}, "BLOCKED:component_witness_insufficient", "H"},
		{"signed_zero_bits", func(a, q [][6]float64) { a[0][0] = 0; q[0][0] = math.Copysign(0, -1) }, "BLOCKED:exact_round_trip_failed", "unique_one_cell_mismatch"},
		{"nan", func(a, q [][6]float64) { a[0][0] = math.NaN() }, "BLOCKED:ascii_shape_or_order", "nonfinite_ascii"},
		{"infinite_query", func(a, q [][6]float64) { q[0][0] = math.Inf(1) }, "BLOCKED:comparison_prerequisite", "nonfinite_query"},
		{"frame_specific_permutation", func(a, q [][6]float64) {
			for r := 2; r < 4; r++ {
				a[r][0], a[r][1] = a[r][1], a[r][0]
			}
		}, "BLOCKED:component_permutation_ambiguous", "zero_exact_maps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, q := append([][6]float64(nil), baseline...), append([][6]float64(nil), baseline...)
			tc.mutate(a, q)
			got := cstASCIIBijection(a, q, tokensFor(a))
			if got.Status != tc.status || got.Reason != tc.reason {
				t.Fatalf("want %s/%s, got %+v", tc.status, tc.reason, got)
			}
			if tc.name == "one_bit" && (got.Mismatch == nil || got.Mismatch.ASCIIBits != 0x3ff0000000000001 || got.Mismatch.QueryBits != 0x3ff0000000000000 || got.Mismatch.RawToken != "1.0000000000000002" || got.Mismatch.SignificantDigits != 17) {
				t.Fatalf("one-cell witness: %+v", got.Mismatch)
			}
		})
	}
	t.Run("missing_query", func(t *testing.T) {
		if got := cstASCIIBijection(baseline, nil, tokensFor(baseline)); got.Status != "BLOCKED:comparison_prerequisite" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("singleton_ascii", func(t *testing.T) {
		if got := cstASCIIBijection(baseline[:1], baseline, tokensFor(baseline[:1])); got.Status != "BLOCKED:ascii_shape_or_order" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("duplicate_in_one_row_still_unique", func(t *testing.T) {
		a := append([][6]float64(nil), baseline...)
		a[0][1] = a[0][0]
		if got := cstASCIIBijection(a, a, tokensFor(a)); got.Status != "PASS:ascii_component_bijection_synthetic" || got.ExactMaps != 1 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("metre_one_round", func(t *testing.T) {
		// These are frozen independently derived nearest-even reference bits,
		// not values computed with the conversion routine under test.
		for _, tc := range []struct {
			mm   float64
			bits uint64
		}{
			{0, 0}, {10, 0x3f847ae147ae147b}, {0.20448276178909414, 0x3f2acd4d8c88a1bc}, {math.Copysign(0, -1), 0x8000000000000000},
		} {
			if got := math.Float64bits(cstASCIIMillimetresToMetres(tc.mm)); got != tc.bits {
				t.Errorf("mm %.17g: got %016x want %016x", tc.mm, got, tc.bits)
			}
		}
	})
}

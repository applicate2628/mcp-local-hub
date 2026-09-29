package sonnet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRealAttRes16ThroughMCP is intentionally opt-in. The caller supplies
// fixture locations and expected identities, keeping product/sample facts out
// of production code and confining all copied work and raw receipts to task
// scratch. It never executes the source tree or an arbitrary command.
func TestRealAttRes16ThroughMCP(t *testing.T) {
	if os.Getenv("SONNET_REAL_ATT_RUN") != "1" {
		t.Skip("set SONNET_REAL_ATT_RUN=1 with controlled fixture metadata to authorize one real sample run")
	}
	sourceRoot := requireRealEnv(t, "SONNET_REAL_ATT_SOURCE_ROOT")
	runRoot := requireRealEnv(t, "SONNET_REAL_ATT_RUN_ROOT")
	wantEM := requireRealEnv(t, "SONNET_REAL_ATT_EM_SHA256")
	wantSample := requireRealEnv(t, "SONNET_REAL_ATT_SAMPLE_SHA256")
	wantLines, err := strconv.Atoi(requireRealEnv(t, "SONNET_REAL_ATT_LINE_COUNT"))
	if err != nil || wantLines <= 0 {
		t.Fatalf("invalid SONNET_REAL_ATT_LINE_COUNT")
	}
	markers := strings.Split(requireRealEnv(t, "SONNET_REAL_ATT_METADATA_MARKERS"), "|")
	if !strings.Contains(filepath.ToSlash(filepath.Clean(runRoot)), "/.scratch/work-items/2026-09-10-sonnet-basic-mcp/") {
		t.Fatalf("run root is outside the admitted task scratch boundary: %q", runRoot)
	}
	sample := filepath.Join(sourceRoot, "att_res16.son")
	if got := sha256File(t, sample); !strings.EqualFold(got, wantSample) {
		t.Fatalf("sample identity mismatch: got %s", got)
	}
	before, err := snapshotOutput(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SONNET_REAL_ATT_COLD_ONE_FILE") == "1" {
		if len(before) != 1 || before["att_res16.son"].SHA256 == "" {
			t.Fatal("cold fixture must contain only att_res16.son before copy")
		}
	}
	body, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if got := len(lines); got != wantLines {
		t.Fatalf("sample line count=%d, want %d", got, wantLines)
	}
	for _, marker := range markers {
		if marker == "" {
			t.Fatal("empty sample metadata marker")
		}
		if !strings.Contains(string(body), marker) {
			t.Fatalf("sample metadata marker absent: %q", marker)
		}
	}

	s := NewServer(Config{RunRoot: runRoot, Driver: strictDriver{}})
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.Close(shutdownCtx); err != nil {
			t.Errorf("owned run settlement: %v", err)
		}
	}()
	product, err := s.ProductInfo()
	if err != nil || !product.Available || !strings.EqualFold(product.SHA256, wantEM) {
		t.Fatalf("validated product unavailable or identity mismatch: available=%t source=%q err=%v", product.Available, product.DiscoverySource, err)
	}

	client := wireClient(t, s)
	started, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_run_ready_project", Arguments: map[string]any{
		"project_root":    sourceRoot,
		"project_file":    "att_res16.son",
		"timeout_seconds": 30,
	}})
	if err != nil || started.IsError {
		t.Fatalf("sample start rejected: is_error=%t err=%v", started != nil && started.IsError, err)
	}
	var start StartResponse
	if err := json.Unmarshal([]byte(resultText(t, started)), &start); err != nil {
		t.Fatal(err)
	}
	record := s.runs[start.RunID]
	if record == nil || !sameManifest(before, record.Baseline) || record.SourceManifestSHA256 != canonicalManifestHash(before) {
		t.Fatal("owned copied baseline does not match the canonical source manifest")
	}
	deadline := time.Now().Add(35 * time.Second)
	for {
		statusResult, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_run_status", Arguments: map[string]any{"run_id": start.RunID}})
		if err != nil || statusResult.IsError {
			t.Fatalf("sample status failed: is_error=%t err=%v", statusResult != nil && statusResult.IsError, err)
		}
		var status StatusResponse
		if err := json.Unmarshal([]byte(resultText(t, statusResult)), &status); err != nil {
			t.Fatal(err)
		}
		if terminal(status.State) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sample owner did not settle within the bounded observation window")
		}
		time.Sleep(100 * time.Millisecond)
	}
	assertPathAdmissionSnapshots(t, record.workspace, record.PathAdmission, record.SourceManifestSHA256, record.CopiedProjectSHA256)
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_run_result", Arguments: map[string]any{"run_id": start.RunID}})
	if err != nil || result.IsError {
		t.Fatalf("sample did not produce a reportable result: is_error=%t err=%v", result != nil && result.IsError, err)
	}
	manifest, ok := resultJSON(t, result)["output_manifest"].(map[string]any)
	if !ok {
		t.Fatal("sample result omitted output manifest")
	}
	for _, raw := range manifest["outputs"].([]any) {
		if raw.(map[string]any)["path"] == "baseline.s2p" {
			t.Fatal("baseline input appeared as a new public output")
		}
	}
	after, err := snapshotOutput(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("source tree changed during owned-copy run")
	}
	writeRealEvidence(t, runRoot, sourceRoot, before, record.Baseline, after, record)
}

type realSampleEvidenceV1 struct {
	SchemaVersion        string                `json:"schema_version"`
	SourceRoot           string                `json:"source_root"`
	SourceBefore         map[string]OutputFile `json:"source_before"`
	CopiedBefore         map[string]OutputFile `json:"copied_before"`
	SourceAfter          map[string]OutputFile `json:"source_after"`
	SourceManifestSHA256 string                `json:"source_manifest_sha256"`
	CopiedProjectSHA256  string                `json:"copied_project_sha256"`
	PathAdmission        SonnetPathAdmissionV1 `json:"path_admission"`
}

func assertPathAdmissionSnapshots(t *testing.T, workspace string, want SonnetPathAdmissionV1, sourceHash, copiedHash string) {
	t.Helper()
	for generation := 1; generation <= 3; generation++ {
		body, err := os.ReadFile(filepath.Join(workspace, "control", snapshotName(generation)))
		if err != nil {
			t.Fatal(err)
		}
		var snapshot struct {
			SourceManifestSHA256 string                `json:"SourceManifestSHA256"`
			CopiedProjectSHA256  string                `json:"CopiedProjectSHA256"`
			PathAdmission        SonnetPathAdmissionV1 `json:"path_admission"`
		}
		if err := json.Unmarshal(body, &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.SourceManifestSHA256 != sourceHash || snapshot.CopiedProjectSHA256 != copiedHash || !reflect.DeepEqual(snapshot.PathAdmission, want) {
			t.Fatalf("generation %d did not bind source/copy/path admission", generation)
		}
	}
}

func writeRealEvidence(t *testing.T, runRoot, sourceRoot string, before, copied, after map[string]OutputFile, record *record) {
	t.Helper()
	evidence := realSampleEvidenceV1{SchemaVersion: "sonnet-real-source-evidence-v1", SourceRoot: sourceRoot, SourceBefore: before, CopiedBefore: copied, SourceAfter: after, SourceManifestSHA256: record.SourceManifestSHA256, CopiedProjectSHA256: record.CopiedProjectSHA256, PathAdmission: record.PathAdmission}
	body, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(runRoot, "evidence")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "source-manifest-v1.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func requireRealEnv(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("missing required real-sample metadata %s", name)
	}
	return value
}

func sha256File(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("result content=%v", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content type=%T", result.Content[0])
	}
	return text.Text
}

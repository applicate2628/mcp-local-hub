package gui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

type guiProjectAdoptFixture struct {
	entry         string
	globalConfig  string
	projectRoot   string
	projectConfig string
	manifestRoot  string
	stateRoot     string
}

func newGUIProjectAdoptFixture(t *testing.T, entry string) guiProjectAdoptFixture {
	t.Helper()
	globalConfig, manifestRoot, stateRoot := setupGUIAdoptTestEnv(t, entry, fmt.Sprintf(`[mcp_servers.%s]
command = "global-tool"
args = ["--global"]
`, entry))
	projectRoot := filepath.Join(t.TempDir(), "selected-project")
	configDir := filepath.Join(projectRoot, ".codex")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	projectConfig := filepath.Join(configDir, "config.toml")
	if err := os.WriteFile(projectConfig, []byte(guiProjectAdoptConfig(entry, "project-tool")), 0o600); err != nil {
		t.Fatal(err)
	}
	return guiProjectAdoptFixture{
		entry:         entry,
		globalConfig:  globalConfig,
		projectRoot:   projectRoot,
		projectConfig: projectConfig,
		manifestRoot:  manifestRoot,
		stateRoot:     stateRoot,
	}
}

func guiProjectAdoptConfig(entry, command string) string {
	return fmt.Sprintf(`[profile.default]
private_canary = "raw-project-config-canary"

[mcp_servers.%s]
command = %q
args = ["--project"]

[mcp_servers.%s.env]
MODE = "project"
`, entry, command, entry)
}

func serveGUIProjectAdoptRequest(t *testing.T, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	s := newEphemeralServer(t, Config{})
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func marshalGUIProjectAdoptRequest(t *testing.T, values map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func previewGUIProjectAdopt(t *testing.T, fixture guiProjectAdoptFixture) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := serveGUIProjectAdoptRequest(t, "/api/adopt/plan", marshalGUIProjectAdoptRequest(t, map[string]any{
		"entry":               fixture.entry,
		"client":              "codex-cli",
		"clients":             []string{"codex-cli"},
		"port":                9331,
		"project_source_root": fixture.projectRoot,
	}))
	return rec, decodeAdoptJSON(t, rec)
}

func snapshotGUIProjectAdoptTree(t *testing.T, root string) []string {
	t.Helper()
	var rows []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			rows = append(rows, "dir:"+relative)
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		rows = append(rows, "file:"+relative+":"+hex.EncodeToString(digest[:]))
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return []string{}
	}
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(rows)
	return rows
}

func snapshotGUIProjectAdoptState(t *testing.T, fixture guiProjectAdoptFixture) map[string][]string {
	t.Helper()
	return map[string][]string{
		"project":   snapshotGUIProjectAdoptTree(t, fixture.projectRoot),
		"global":    snapshotGUIProjectAdoptTree(t, filepath.Dir(fixture.globalConfig)),
		"manifests": snapshotGUIProjectAdoptTree(t, fixture.manifestRoot),
		"state":     snapshotGUIProjectAdoptTree(t, fixture.stateRoot),
	}
}

func TestGUIProjectAdoptPlanReturnsTokenWithoutSourceDisclosure(t *testing.T) {
	fixture := newGUIProjectAdoptFixture(t, "gui-project-preview")
	rec, body := previewGUIProjectAdopt(t, fixture)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	identitySHA256, err := clients.CodexProjectIdentitySHA256ForRoot(fixture.projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if body["ManifestName"] != fixture.entry+"-project-"+identitySHA256[:8] {
		t.Fatalf("ManifestName=%#v", body["ManifestName"])
	}
	token, _ := body["project_source_preimage_token"].(string)
	if len(token) != 64 || token != strings.ToLower(token) {
		t.Fatalf("project_source_preimage_token=%q", token)
	}
	if targets, _ := body["symlink_targets"].([]any); len(targets) != 0 {
		t.Fatalf("symlink_targets=%#v, want [] for project-only Codex target", body["symlink_targets"])
	}
	raw := rec.Body.String()
	for _, forbidden := range []string{fixture.projectRoot, filepath.Base(fixture.projectRoot), fixture.projectConfig, "config.toml", "raw-project-config-canary", "global-tool", "projectSource", "configBytes"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("plan response leaked %q: %s", forbidden, raw)
		}
	}
	if !strings.Contains(raw, "project-tool") {
		t.Fatalf("plan response omitted selected source command: %s", raw)
	}
}

func TestGUIProjectAdoptApplyRejectsMissingMalformedAndStaleTokenBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		supplied   func(string) string
		mutateFile bool
		wantCode   string
	}{
		{name: "missing", supplied: func(string) string { return "" }, wantCode: "CODEX_PROJECT_SOURCE_PREVIEW_REQUIRED"},
		{name: "malformed", supplied: func(string) string { return strings.Repeat("A", 64) }, wantCode: "CODEX_PROJECT_SOURCE_CHANGED"},
		{name: "stale", supplied: func(token string) string { return token }, mutateFile: true, wantCode: "CODEX_PROJECT_SOURCE_CHANGED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGUIProjectAdoptFixture(t, "gui-project-token-"+tc.name)
			preview, previewBody := previewGUIProjectAdopt(t, fixture)
			if preview.Code != http.StatusOK {
				t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
			}
			previewToken, _ := previewBody["project_source_preimage_token"].(string)
			if previewToken == "" {
				t.Fatalf("preview token missing: %s", preview.Body.String())
			}
			if tc.mutateFile {
				if err := os.WriteFile(fixture.projectConfig, []byte(guiProjectAdoptConfig(fixture.entry, "changed-project-tool")), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotGUIProjectAdoptState(t, fixture)
			rec := serveGUIProjectAdoptRequest(t, "/api/adopt", marshalGUIProjectAdoptRequest(t, map[string]any{
				"entry":                         fixture.entry,
				"client":                        "codex-cli",
				"clients":                       []string{"codex-cli"},
				"port":                          9331,
				"project_source_root":           fixture.projectRoot,
				"project_source_preimage_token": tc.supplied(previewToken),
			}))
			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			body := decodeAdoptJSON(t, rec)
			if body["code"] != tc.wantCode || !strings.Contains(fmt.Sprint(body["error"]), tc.wantCode) {
				t.Fatalf("body=%v, want code/error %s", body, tc.wantCode)
			}
			if strings.Contains(rec.Body.String(), fixture.projectRoot) || strings.Contains(rec.Body.String(), filepath.Base(fixture.projectRoot)) || strings.Contains(rec.Body.String(), "raw-project-config-canary") {
				t.Fatalf("token failure leaked source data: %s", rec.Body.String())
			}
			after := snapshotGUIProjectAdoptState(t, fixture)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected apply mutated state\nbefore=%v\nafter=%v", before, after)
			}
		})
	}
}

func TestGUIProjectAdoptGlobalWireOmitsTokenAndRejectsUnexpectedToken(t *testing.T) {
	entry := "gui-global-token-wire"
	globalConfig, manifestRoot, stateRoot := setupGUIAdoptTestEnv(t, entry, fmt.Sprintf("[mcp_servers.%s]\ncommand = \"global-tool\"\n", entry))
	plan := serveGUIProjectAdoptRequest(t, "/api/adopt/plan", marshalGUIProjectAdoptRequest(t, map[string]any{
		"entry":  entry,
		"client": "codex-cli",
		"port":   9332,
	}))
	if plan.Code != http.StatusOK || strings.Contains(plan.Body.String(), "project_source_preimage_token") {
		t.Fatalf("global plan status/body=%d/%s", plan.Code, plan.Body.String())
	}
	before := map[string][]string{
		"global":    snapshotGUIProjectAdoptTree(t, filepath.Dir(globalConfig)),
		"manifests": snapshotGUIProjectAdoptTree(t, manifestRoot),
		"state":     snapshotGUIProjectAdoptTree(t, stateRoot),
	}
	rec := serveGUIProjectAdoptRequest(t, "/api/adopt", marshalGUIProjectAdoptRequest(t, map[string]any{
		"entry":                         entry,
		"client":                        "codex-cli",
		"port":                          9332,
		"project_source_preimage_token": strings.Repeat("0", 64),
	}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeAdoptJSON(t, rec)
	if body["code"] != "PROJECT_ADOPT_SOURCE_PREIMAGE_UNEXPECTED" || !strings.Contains(fmt.Sprint(body["error"]), "PROJECT_ADOPT_SOURCE_PREIMAGE_UNEXPECTED") {
		t.Fatalf("body=%v", body)
	}
	after := map[string][]string{
		"global":    snapshotGUIProjectAdoptTree(t, filepath.Dir(globalConfig)),
		"manifests": snapshotGUIProjectAdoptTree(t, manifestRoot),
		"state":     snapshotGUIProjectAdoptTree(t, stateRoot),
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("unexpected global token mutated state\nbefore=%v\nafter=%v", before, after)
	}
}

func TestWriteGUIProjectAdoptTokenErrorRedactsUnknownFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	writeGUIProjectAdoptTokenError(rec, errors.New(`unknown validation failure at C:\private\config.toml`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeAdoptJSON(t, rec)
	if body["code"] != "ADOPT_FAILED" || body["error"] != "internal error" || strings.Contains(rec.Body.String(), `C:\private`) {
		t.Fatalf("body=%v", body)
	}
}

func TestWriteGUIProjectAdoptTokenErrorMapsInconsistentRepeatToConflict(t *testing.T) {
	rec := httptest.NewRecorder()
	writeGUIProjectAdoptTokenError(rec, api.ErrProjectAdoptRepeatStateInconsistent)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeAdoptJSON(t, rec)
	if body["code"] != "PROJECT_ADOPT_REPEAT_STATE_INCONSISTENT" || body["error"] != "PROJECT_ADOPT_REPEAT_STATE_INCONSISTENT" {
		t.Fatalf("body=%v", body)
	}
}

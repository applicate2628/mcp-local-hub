package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"

	toml "github.com/pelletier/go-toml/v2"
)

func setupGUIAdoptTestEnv(t *testing.T, entryName, codexBody string) (codexPath, manifestRoot, stateRoot string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(root, "localappdata"))
	t.Setenv("APPDATA", filepath.Join(root, "appdata"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "xdg-data"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
	t.Setenv("MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK", "")
	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "")
	t.Cleanup(api.ResetStrictModeIntentCacheForTest)

	canonical := filepath.Join(root, "bin", api.MCPHubBinaryName())
	if err := os.MkdirAll(filepath.Dir(canonical), 0o700); err != nil {
		t.Fatalf("mkdir canonical parent: %v", err)
	}
	if err := os.WriteFile(canonical, []byte("test mcphub"), 0o700); err != nil {
		t.Fatalf("write canonical mcphub stub: %v", err)
	}
	t.Cleanup(api.SetTestCanonicalMcphubPath(canonical))

	codexPath = filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o700); err != nil {
		t.Fatalf("mkdir codex config parent: %v", err)
	}
	if err := os.WriteFile(codexPath, []byte(codexBody), 0o600); err != nil {
		t.Fatalf("seed codex config: %v", err)
	}

	manifestRoot = guiAdoptDefaultManifestDir(t)
	if err := os.MkdirAll(manifestRoot, 0o700); err != nil {
		t.Fatalf("mkdir manifest root: %v", err)
	}
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", manifestRoot)

	stateRoot = filepath.Join(root, "state")
	t.Cleanup(api.SetDaemonStateRootForTest(stateRoot))
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(manifestRoot, entryName)) })
	return codexPath, manifestRoot, stateRoot
}

func guiAdoptDefaultManifestDir(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		return filepath.Join(string(os.PathSeparator), "nonexistent", "mcphub", "servers")
	}
	exeDir := filepath.Dir(exe)
	sibling := filepath.Join(exeDir, "servers")
	if st, err := os.Stat(sibling); err == nil && st.IsDir() {
		return sibling
	}
	parent := filepath.Join(exeDir, "..", "servers")
	if st, err := os.Stat(parent); err == nil && st.IsDir() {
		return parent
	}
	return sibling
}

func postAdoptTest(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	s := NewServer(Config{})
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

func decodeAdoptJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v\n%s", err, rec.Body.String())
	}
	return body
}

func TestAdoptPlanRouteReturnsPreviewWithoutMutating(t *testing.T) {
	entry := "gui-adopt-plan"
	codexPath, manifestRoot, stateRoot := setupGUIAdoptTestEnv(t, entry, `[profile.default]
model = "gpt-5"

[mcp_servers.gui-adopt-plan]
command = "go"
args = ["version"]
`)
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex config before plan: %v", err)
	}

	rec := postAdoptTest(t, "/api/adopt/plan", `{"entry":"gui-adopt-plan","client":"codex-cli","port":9321}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeAdoptJSON(t, rec)
	if body["ManifestName"] != entry {
		t.Fatalf("ManifestName=%#v, want %q", body["ManifestName"], entry)
	}
	if body["Port"] != float64(9321) {
		t.Fatalf("Port=%#v, want 9321", body["Port"])
	}
	adoptClients, _ := body["AdoptClients"].([]any)
	if len(adoptClients) != 1 || adoptClients[0] != "codex-cli" {
		t.Fatalf("AdoptClients=%#v, want [codex-cli]", body["AdoptClients"])
	}
	if targets, _ := body["symlink_targets"].([]any); len(targets) != 0 {
		t.Fatalf("symlink_targets=%#v, want []", body["symlink_targets"])
	}
	after, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex config after plan: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("plan route mutated codex config\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(err) {
		t.Fatalf("plan route wrote manifest; stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "supervisor-intent.json")); !os.IsNotExist(err) {
		t.Fatalf("plan route wrote supervisor intent; stat err=%v", err)
	}
}

func TestAdoptRouteExecutesManifestIntentAndRepointsClient(t *testing.T) {
	entry := "gui-adopt-execute"
	codexPath, manifestRoot, stateRoot := setupGUIAdoptTestEnv(t, entry, `[profile.default]
model = "gpt-5"

[mcp_servers.keep]
url = "http://example.invalid/mcp"

[mcp_servers.gui-adopt-execute]
command = "go"
args = ["version"]
`)

	rec := postAdoptTest(t, "/api/adopt", `{"entry":"gui-adopt-execute","client":"codex-cli","port":9322}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeAdoptJSON(t, rec)
	if body["name"] != entry || body["port"] != float64(9322) {
		t.Fatalf("response=%#v, want name %q port 9322", body, entry)
	}

	manifestBytes, err := os.ReadFile(filepath.Join(manifestRoot, entry, "manifest.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	manifest, err := config.ParseManifest(bytes.NewReader(manifestBytes))
	if err != nil {
		t.Fatalf("parse manifest: %v\n%s", err, manifestBytes)
	}
	if manifest.Name != entry || manifest.Daemons[0].Port != 9322 {
		t.Fatalf("manifest name/port = %q/%d, want %q/9322", manifest.Name, manifest.Daemons[0].Port, entry)
	}
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateRoot, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if len(intent.Daemons) != 1 || intent.Daemons[0].Server != entry {
		t.Fatalf("intent daemons=%#v, want one daemon for %q", intent.Daemons, entry)
	}

	var root map[string]any
	after, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatalf("read codex config: %v", err)
	}
	if err := toml.Unmarshal(after, &root); err != nil {
		t.Fatalf("decode codex TOML: %v\n%s", err, after)
	}
	servers := root["mcp_servers"].(map[string]any)
	adopted := servers[entry].(map[string]any)
	if adopted["url"] != "http://127.0.0.1:9322/mcp" {
		t.Fatalf("adopted url=%#v, want hub URL", adopted["url"])
	}
	if _, hasCommand := adopted["command"]; hasCommand {
		t.Fatalf("adopted client entry still has command: %#v", adopted)
	}
	if _, ok := servers["keep"]; !ok {
		t.Fatalf("foreign mcp_servers.keep table not preserved: %#v", servers)
	}
}

func TestAdoptPlanRouteNeverSerializesSecretValues(t *testing.T) {
	entry := "gui-adopt-secret"
	setupGUIAdoptTestEnv(t, entry, `[mcp_servers.gui-adopt-secret]
command = "go"
args = ["version"]

[mcp_servers.gui-adopt-secret.env]
API_KEY = "literal-secret-value"
VISIBLE = "not-secret"
`)

	rec := postAdoptTest(t, "/api/adopt/plan", `{"entry":"gui-adopt-secret","client":"codex-cli","port":9323}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	raw := rec.Body.String()
	if strings.Contains(raw, "literal-secret-value") {
		t.Fatalf("plan JSON leaked secret value:\n%s", raw)
	}
	if strings.Contains(raw, "secretValues") {
		t.Fatalf("plan JSON serialized unexported secretValues field:\n%s", raw)
	}
	if !strings.Contains(raw, "GUI_ADOPT_SECRET_API_KEY") {
		t.Fatalf("plan JSON omitted secret-routed key name:\n%s", raw)
	}
}

func TestAdoptRouteSymlinkConsentRequiredThenAllowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Log("attempting Windows symlink test; host may require Developer Mode or elevation")
	}
	entry := "gui-adopt-symlink"
	codexPath, _, _ := setupGUIAdoptTestEnv(t, entry, `[mcp_servers.gui-adopt-symlink]
command = "go"
args = ["version"]
`)
	realTarget := filepath.Join(filepath.Dir(filepath.Dir(codexPath)), "dotfiles", ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(realTarget), 0o700); err != nil {
		t.Fatalf("mkdir real target parent: %v", err)
	}
	if err := os.Rename(codexPath, realTarget); err != nil {
		t.Fatalf("move codex config to real target: %v", err)
	}
	if err := os.Symlink(realTarget, codexPath); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}

	plan := postAdoptTest(t, "/api/adopt/plan", `{"entry":"gui-adopt-symlink","client":"codex-cli","port":9324}`)
	if plan.Code != http.StatusOK {
		t.Fatalf("plan status=%d, want 200; body=%s", plan.Code, plan.Body.String())
	}
	planBody := decodeAdoptJSON(t, plan)
	targets, _ := planBody["symlink_targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("symlink_targets=%#v, want one target", planBody["symlink_targets"])
	}
	target := targets[0].(map[string]any)
	if target["client"] != "codex-cli" || !sameGUIAdoptPath(target["resolved_path"].(string), realTarget) {
		t.Fatalf("symlink target=%#v, want codex-cli -> %s", target, realTarget)
	}

	withoutConsent := postAdoptTest(t, "/api/adopt", `{"entry":"gui-adopt-symlink","client":"codex-cli","port":9324}`)
	if withoutConsent.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409; body=%s", withoutConsent.Code, withoutConsent.Body.String())
	}
	if code := decodeAdoptJSON(t, withoutConsent)["code"]; code != "SYMLINK_CONSENT_REQUIRED" {
		t.Fatalf("code=%#v, want SYMLINK_CONSENT_REQUIRED", code)
	}

	withConsentBody, err := json.Marshal(map[string]any{
		"entry":           "gui-adopt-symlink",
		"client":          "codex-cli",
		"port":            9324,
		"symlink_consent": targets,
	})
	if err != nil {
		t.Fatalf("marshal consent body: %v", err)
	}
	withConsent := postAdoptTest(t, "/api/adopt", string(withConsentBody))
	if withConsent.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201; body=%s", withConsent.Code, withConsent.Body.String())
	}
	if api.InteractiveSymlinkConsent != nil {
		t.Fatalf("GUI adopt must not install process-global InteractiveSymlinkConsent")
	}
	if info, err := os.Lstat(codexPath); err != nil {
		t.Fatalf("lstat original codex path: %v", err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("codex config link type not preserved; mode=%v", info.Mode())
	}
	targetBytes, err := os.ReadFile(realTarget)
	if err != nil {
		t.Fatalf("read real target after adopt: %v", err)
	}
	var root map[string]any
	if err := toml.Unmarshal(targetBytes, &root); err != nil {
		t.Fatalf("decode real target TOML: %v\n%s", err, targetBytes)
	}
	adopted := root["mcp_servers"].(map[string]any)[entry].(map[string]any)
	if adopted["url"] != "http://127.0.0.1:9324/mcp" {
		t.Fatalf("target adopted url=%#v, want hub URL", adopted["url"])
	}
}

func TestAdoptRouteRejectsFreshSymlinkTargetMissingFromReviewedConsent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Log("attempting Windows symlink test; host may require Developer Mode or elevation")
	}
	entry := "gui-adopt-stale-symlink"
	codexPath, _, _ := setupGUIAdoptTestEnv(t, entry, `[mcp_servers.gui-adopt-stale-symlink]
command = "go"
args = ["version"]
`)
	home := filepath.Dir(filepath.Dir(codexPath))
	cursorPath := filepath.Join(home, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cursorPath), 0o700); err != nil {
		t.Fatalf("mkdir cursor config parent: %v", err)
	}
	if err := os.WriteFile(cursorPath, []byte(`{
  "mcpServers": {
    "gui-adopt-stale-symlink": {
      "command": "go",
      "args": ["version"]
    }
  }
}
`), 0o600); err != nil {
		t.Fatalf("seed cursor config: %v", err)
	}

	planBody, err := json.Marshal(map[string]any{
		"entry":   entry,
		"client":  "codex-cli",
		"clients": []string{"codex-cli", "cursor"},
		"port":    9326,
	})
	if err != nil {
		t.Fatalf("marshal plan body: %v", err)
	}
	plan := postAdoptTest(t, "/api/adopt/plan", string(planBody))
	if plan.Code != http.StatusOK {
		t.Fatalf("plan status=%d, want 200; body=%s", plan.Code, plan.Body.String())
	}
	if targets, _ := decodeAdoptJSON(t, plan)["symlink_targets"].([]any); len(targets) != 0 {
		t.Fatalf("initial symlink_targets=%#v, want none", targets)
	}

	realTarget := filepath.Join(home, "dotfiles", ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(realTarget), 0o700); err != nil {
		t.Fatalf("mkdir cursor real target parent: %v", err)
	}
	if err := os.Rename(cursorPath, realTarget); err != nil {
		t.Fatalf("move cursor config to real target: %v", err)
	}
	if err := os.Symlink(realTarget, cursorPath); err != nil {
		t.Skipf("symlink unsupported on this host: %v", err)
	}

	executeBody, err := json.Marshal(map[string]any{
		"entry":           entry,
		"client":          "codex-cli",
		"clients":         []string{"codex-cli", "cursor"},
		"port":            9326,
		"symlink_consent": []any{},
	})
	if err != nil {
		t.Fatalf("marshal execute body: %v", err)
	}
	rec := postAdoptTest(t, "/api/adopt", string(executeBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want 409 for unreviewed fresh symlink target; body=%s", rec.Code, rec.Body.String())
	}
	if code := decodeAdoptJSON(t, rec)["code"]; code != "SYMLINK_CONSENT_REQUIRED" {
		t.Fatalf("code=%#v, want SYMLINK_CONSENT_REQUIRED", code)
	}
}

func TestAdoptRouteExecuteFailureRedactsAbsolutePath(t *testing.T) {
	entry := "gui-adopt-redact-path"
	codexPath, _, _ := setupGUIAdoptTestEnv(t, entry, `[mcp_servers.gui-adopt-redact-path]
command = "go"
args = ["version"]
`)
	leakyPath := filepath.Join(filepath.Dir(codexPath), "private-user-path", "config.toml")
	orig := clients.WriteConfigFile
	clients.WriteConfigFile = func(path string, contents []byte) error {
		return fmt.Errorf("synthetic write failure at %s", leakyPath)
	}
	t.Cleanup(func() { clients.WriteConfigFile = orig })

	rec := postAdoptTest(t, "/api/adopt", `{"entry":"gui-adopt-redact-path","client":"codex-cli","port":9327}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeAdoptJSON(t, rec)
	if body["code"] != "ADOPT_FAILED" {
		t.Fatalf("code=%#v, want ADOPT_FAILED", body["code"])
	}
	if body["error"] != "internal error" {
		t.Fatalf("error=%#v, want redacted internal error", body["error"])
	}
	if strings.Contains(rec.Body.String(), leakyPath) {
		t.Fatalf("execute error leaked absolute path %q in response: %s", leakyPath, rec.Body.String())
	}
}

func sameGUIAdoptPath(a, b string) bool {
	cleanA := filepath.Clean(a)
	cleanB := filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(cleanA, cleanB)
	}
	return cleanA == cleanB
}

func TestAdoptPlanBadInputMaps400(t *testing.T) {
	setupGUIAdoptTestEnv(t, "gui-adopt-bad", `[mcp_servers.gui-adopt-bad]
command = "go"
args = ["version"]
`)
	rec := postAdoptTest(t, "/api/adopt/plan", `{"entry":"gui-adopt-bad","client":"not-a-client"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	body := decodeAdoptJSON(t, rec)
	if code := body["code"]; code != "BAD_INPUT" {
		t.Fatalf("code=%#v, want BAD_INPUT", code)
	}
	// bot PR #516 P3: a path-free input-validation failure must surface the
	// ACTIONABLE message, not the opaque redacted "internal error" — otherwise
	// the operator cannot tell what input to fix.
	if msg, _ := body["error"].(string); msg == "internal error" || !strings.Contains(msg, "must be one of") {
		t.Fatalf("error=%#v, want actionable '--client must be one of ...' (not redacted)", body["error"])
	}
}

// TestAdoptErrorMessageHasPath is the P3a redaction discriminator: adopt's
// input-validation errors carry only ids/names (no separator) and must be
// treated as actionable, while any message embedding a filesystem path is
// path-bearing and must be redacted before reaching the wire.
func TestAdoptErrorMessageHasPath(t *testing.T) {
	actionable := []string{
		"adopt entry name is required",
		`--client must be one of claude-code | codex-cli | cursor`,
		`unknown adopt client "zed" (expected claude-code | codex-cli)`,
		`server "x" in source client "codex-cli" is disabled; enable it first before adopting`,
		`--clients must include source --client "codex-cli"`,
	}
	for _, msg := range actionable {
		if adoptErrorMessageHasPath(msg) {
			t.Errorf("actionable validation message wrongly flagged as path-bearing: %q", msg)
		}
	}
	pathBearing := []string{
		`resolve client config path for "codex-cli": open C:\Users\dima_\AppData\config.toml: denied`,
		`resolve client config path for "codex-cli": open /home/user/.config/x.json: denied`,
		`check existing disk manifest "x": open \\host\share\manifest.yaml: denied`,
		// fable PR #516 P3-A evasion shapes: a quoted POSIX path and a rooted
		// (single-backslash) Windows path the prior regex missed.
		`entry name "/home/evil/secret.toml" is not a valid manifest name`,
		`open \Users\dima_\AppData\Local\vault.age: access is denied`,
		`config=/etc/mcphub/secret.yaml unreadable`,
	}
	for _, msg := range pathBearing {
		if !adoptErrorMessageHasPath(msg) {
			t.Errorf("path-bearing message not flagged (would leak path): %q", msg)
		}
	}
}

// TestAdoptPlanErrorIsActionable pins the FAIL-CLOSED contract (fable PR #516
// P3-A/P3-B): a message is forwarded to the operator ONLY when it is a
// recognized, path-free validation/name-conflict phrase; an unrecognized
// message, or any message embedding a path (even one that also contains a
// recognized phrase), is redacted.
func TestAdoptPlanErrorIsActionable(t *testing.T) {
	actionable := []string{
		"adopt entry name is required",
		`--client must be one of claude-code | codex-cli`,
		`unknown adopt client "zed" (expected claude-code | codex-cli)`,
		`server "x" in source client "codex-cli" is disabled; enable it first before adopting`,
		`--clients must include source --client "codex-cli"`,
		`manifest "x" collides with a shipped (built-in) server`,
		`adopt refuses to create manifest "x" because a disk manifest already exists`,
	}
	for _, msg := range actionable {
		if !adoptPlanErrorIsActionable(msg) {
			t.Errorf("recognized path-free validation message wrongly redacted: %q", msg)
		}
	}
	redacted := []string{
		// Unrecognized shape -> default-redact (fail closed), not forwarded.
		`some brand new backend error we never enumerated`,
		// Recognized phrase BUT path-bearing -> redact wins (P3-B: a wrapped OS
		// "already exists: <path>" must not ride the actionable lane).
		`Cannot create a file when that file already exists: C:\Users\dima_\x.toml`,
		// Path-bearing, no recognized phrase.
		`open /home/user/.config/mcphub/x.json: permission denied`,
	}
	for _, msg := range redacted {
		if adoptPlanErrorIsActionable(msg) {
			t.Errorf("message should be redacted (fail closed) but was forwarded: %q", msg)
		}
	}
}

// TestAdoptRoutePublishesOperatorActionEvent guards the P3b audit contract: a
// committed GUI adopt (manifest create + client-config rewrite) must publish an
// operator-action row like the sibling migrate/demigrate routes, so
// gui-events.log can reconstruct that the GUI performed the adoption.
func TestAdoptRoutePublishesOperatorActionEvent(t *testing.T) {
	entry := "gui-adopt-audit"
	setupGUIAdoptTestEnv(t, entry, `[mcp_servers.gui-adopt-audit]
command = "go"
args = ["version"]
`)
	s := NewServer(Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Broadcaster().Subscribe(ctx)

	req := httptest.NewRequest(http.MethodPost, "/api/adopt",
		strings.NewReader(`{"entry":"gui-adopt-audit","client":"codex-cli","port":9328}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	select {
	case ev := <-ch:
		if ev.Type != "operator-action" {
			t.Fatalf("event type=%q, want operator-action", ev.Type)
		}
		if ev.Body["action"] != "adopt" {
			t.Errorf("action=%v, want adopt", ev.Body["action"])
		}
		if _, ok := ev.Body["actor"]; !ok {
			t.Errorf("body missing actor field: %+v", ev.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no operator-action event published after a successful adopt")
	}
}

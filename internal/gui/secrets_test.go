package gui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/secrets"
)

// fakeSecretsAPI is the test double for secretsAPI. It records all
// calls made by the handlers so tests can assert dispatch correctness.
type fakeSecretsAPI struct {
	initCalls      int
	panicOnInit    bool
	initResult     api.SecretsInitResult
	initErr        error
	listResult     api.SecretsEnvelope
	listErr        error
	setErr         error
	rotateResult   api.SecretsRotateResult
	rotateErr      error
	restartResults []api.RestartResult
	restartErr     error
	deleteErr      error

	calledSet    []struct{ Name, Value string }
	calledRotate []struct {
		Name, Value string
		Restart     bool
	}
	calledRestart []string
	calledDelete  []struct {
		Name    string
		Confirm bool
	}
}

func (f *fakeSecretsAPI) Init() (api.SecretsInitResult, error) {
	f.initCalls++
	if f.panicOnInit {
		panic("capability unexpectedly invoked initialization")
	}
	return f.initResult, f.initErr
}
func (f *fakeSecretsAPI) List() (api.SecretsEnvelope, error) { return f.listResult, f.listErr }
func (f *fakeSecretsAPI) Set(name, value string) error {
	f.calledSet = append(f.calledSet, struct{ Name, Value string }{name, value})
	return f.setErr
}
func (f *fakeSecretsAPI) Rotate(name, value string, restart bool) (api.SecretsRotateResult, error) {
	f.calledRotate = append(f.calledRotate, struct {
		Name, Value string
		Restart     bool
	}{name, value, restart})
	return f.rotateResult, f.rotateErr
}
func (f *fakeSecretsAPI) Restart(name string) ([]api.RestartResult, error) {
	f.calledRestart = append(f.calledRestart, name)
	return f.restartResults, f.restartErr
}
func (f *fakeSecretsAPI) Delete(name string, confirm bool) error {
	f.calledDelete = append(f.calledDelete, struct {
		Name    string
		Confirm bool
	}{name, confirm})
	return f.deleteErr
}

func newServerWithSecretsFake(t *testing.T, fake *fakeSecretsAPI) *Server {
	t.Helper()
	s := newEphemeralServer(t, Config{})
	s.secrets = fake
	return s
}

// --- Step 3.B.2: POST /api/secrets/init happy path ---

func TestSecretsInit_OK(t *testing.T) {
	fake := &fakeSecretsAPI{
		initResult: api.SecretsInitResult{VaultState: "ok"},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/init", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	var body api.SecretsInitResult
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.VaultState != "ok" {
		t.Errorf("vault_state=%q", body.VaultState)
	}
}

// --- Step 3.B.3: POST /api/secrets/init 409 BLOCKED ---

func TestSecretsInit_Blocked409(t *testing.T) {
	fake := &fakeSecretsAPI{
		initErr: &api.SecretsInitBlocked{KeyExists: true, VaultExists: true},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/init", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "SECRETS_INIT_BLOCKED" {
		t.Errorf("code=%q", body["code"])
	}
}

// --- Step 3.B.4: POST /api/secrets/init 200 cleanup-ok (case 2b) ---

func TestSecretsInit_PartialCleanupOK_Returns200(t *testing.T) {
	fake := &fakeSecretsAPI{
		initErr: &api.SecretsInitFailed{
			CleanupStatus: "ok",
			Cause:         fmt.Errorf("disk full"),
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/init", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200, body=%q", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["cleanup_status"] != "ok" {
		t.Errorf("cleanup_status=%v", body["cleanup_status"])
	}
	if body["vault_state"] != "missing" {
		t.Errorf("vault_state=%v, want missing", body["vault_state"])
	}
	if body["code"] != "SECRETS_INIT_FAILED" {
		t.Errorf("code=%v", body["code"])
	}
}

// --- Step 3.B.5: POST /api/secrets/init 500 cleanup-failed (case 2c) ---

func TestSecretsInit_PartialCleanupFailed_Returns500(t *testing.T) {
	fake := &fakeSecretsAPI{
		initErr: &api.SecretsInitFailed{
			CleanupStatus: "failed",
			OrphanPath:    "/some/path/secrets.age",
			Cause:         fmt.Errorf("disk full"),
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/init", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["cleanup_status"] != "failed" {
		t.Errorf("cleanup_status=%v", body["cleanup_status"])
	}
	if _, present := body["orphan_path"]; present {
		t.Errorf("orphan_path escaped the init HTTP boundary")
	}
}

func serveHTTPInitProof(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:9081"+path, nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func saveHTTPInitProof(t *testing.T, name string, rec *httptest.ResponseRecorder) {
	t.Helper()
	root := os.Getenv("MCPHUB_HTTP_PROOF_DIR")
	if root == "" {
		return
	}
	if !filepath.IsAbs(root) {
		t.Fatal("HTTP proof root is not absolute")
	}
	prefix := os.Getenv("MCPHUB_HTTP_EPOCH") + "." + name
	if err := os.WriteFile(filepath.Join(root, prefix+".body.json"), rec.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(struct {
		Status int         `json:"status"`
		Header http.Header `json:"header"`
	}{rec.Code, rec.Header()})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, prefix+".response.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
}

func assertHTTPInitContained(t *testing.T, rec *httptest.ResponseRecorder, status int, forbidden ...string) map[string]any {
	t.Helper()
	if rec.Code != status {
		t.Errorf("HTTP status=%d want=%d body=%q", rec.Code, status, rec.Body.String())
	}
	if rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("unexpected init Content-Type=%q", rec.Header().Get("Content-Type"))
	}
	for _, value := range forbidden {
		if value != "" && bytes.Contains(rec.Body.Bytes(), []byte(value)) {
			t.Errorf("private cause/path reflected: %q", value)
		}
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, present := body["orphan_path"]; present {
		t.Error("orphan_path emitted")
	}
	message, ok := body["error"].(string)
	for _, value := range forbidden {
		if value != "" && strings.Contains(message, value) {
			t.Errorf("decoded private cause/path reflected: %q", value)
		}
	}
	if !ok || !strings.Contains(message, "Reload") {
		t.Errorf("missing fixed compatible reload guidance: %v", body["error"])
	}
	return body
}

func TestSecretsInit_HTTPFailureProjection(t *testing.T) {
	cause := errors.New("HTTP-PRIVATE-CAUSE /synthetic-private/target")
	for _, c := range []struct {
		name    string
		err     error
		status  int
		cleanup string
	}{
		{"typed_cleanup_ok", &api.SecretsInitFailed{CleanupStatus: "ok", Cause: cause}, 200, "ok"},
		{"typed_cleanup_failed", &api.SecretsInitFailed{CleanupStatus: "failed", OrphanPath: "/synthetic-private/orphan", Cause: cause}, 500, "failed"},
		{"untyped", cause, 500, ""},
		{"wrapped_blocked", fmt.Errorf("HTTP-PRIVATE-WRAPPER: %w", &api.SecretsInitBlocked{KeyExists: true}), 409, ""},
		{"persistence_wrapping_cleanup_ok", &api.SecretsRecoveryPersistenceError{PersistenceCause: cause, InitError: &api.SecretsInitFailed{CleanupStatus: "ok", Cause: cause}}, 500, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newEphemeralServer(t, Config{Port: 9081})
			fake := &fakeSecretsAPI{initErr: c.err}
			s.secrets = fake
			rec := serveHTTPInitProof(t, s, http.MethodPost, "/api/secrets/init")
			saveHTTPInitProof(t, "mock."+c.name, rec)
			body := assertHTTPInitContained(t, rec, c.status, "HTTP-PRIVATE-CAUSE", "HTTP-PRIVATE-WRAPPER", "/synthetic-private")
			code := "SECRETS_INIT_FAILED"
			if c.status == 409 {
				code = "SECRETS_INIT_BLOCKED"
			}
			if body["code"] != code {
				t.Errorf("code=%v", body["code"])
			}
			if c.cleanup != "" && body["cleanup_status"] != c.cleanup {
				t.Errorf("cleanup status=%v", body["cleanup_status"])
			}
			if c.cleanup == "ok" && body["vault_state"] != "missing" {
				t.Error("cleanup-ok no longer reports missing")
			}
			if fake.initCalls != 1 {
				t.Errorf("init calls=%d", fake.initCalls)
			}
		})
	}
}

func TestSecretsInit_HTTPCapability(t *testing.T) {
	s := newEphemeralServer(t, Config{Port: 9081})
	fake := &fakeSecretsAPI{panicOnInit: true}
	s.secrets = fake
	rec := serveHTTPInitProof(t, s, http.MethodGet, "/api/secrets/init-capabilities")
	saveHTTPInitProof(t, "capability", rec)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || rec.Body.String() != `{"schema_version":1,"workflow":"cli_support_readonly","path_free_errors":true}`+"\n" {
		t.Errorf("capability=%d %q", rec.Code, rec.Body.String())
	}
	method := serveHTTPInitProof(t, s, http.MethodPost, "/api/secrets/init-capabilities")
	if method.Code != 405 || method.Header().Get("Allow") != "GET, PUT, DELETE" {
		t.Errorf("capability method gate=%d Allow=%q", method.Code, method.Header().Get("Allow"))
	}
	if fake.initCalls != 0 {
		t.Fatal("static capability invoked init")
	}
}

func TestSecretsCapabilityRouteMethodMetadata(t *testing.T) {
	s := newEphemeralServer(t, Config{Port: 9081})
	fake := &fakeSecretsAPI{panicOnInit: true}
	s.secrets = fake
	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodPatch, http.MethodOptions, "CUSTOM"} {
		for _, path := range []string{"/api/secrets/init-capabilities", "/api/secrets/K1"} {
			rec := serveHTTPInitProof(t, s, method, path)
			allow := "PUT, DELETE"
			if path == "/api/secrets/init-capabilities" {
				allow = "GET, PUT, DELETE"
			}
			if rec.Code != 405 || rec.Header().Get("Allow") != allow {
				t.Errorf("%s %s => %d Allow=%q", method, path, rec.Code, rec.Header().Get("Allow"))
			}
		}
	}
	if len(fake.calledDelete) != 0 || len(fake.calledRotate) != 0 || fake.initCalls != 0 {
		t.Fatal("rejected/read methods invoked mutation")
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		for _, kind := range []string{"host", "origin", "fetch-site"} {
			req := httptest.NewRequest(method, "http://127.0.0.1:9081/api/secrets/init-capabilities?confirm=true", nil)
			switch kind {
			case "host":
				req.Host = "evil.invalid:9081"
			case "origin":
				req.Header.Set("Origin", "http://evil.invalid:9081")
			case "fetch-site":
				req.Header.Set("Sec-Fetch-Site", "cross-site")
			}
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != 403 {
				t.Errorf("%s %s guard=%d", method, kind, rec.Code)
			}
		}
	}
	if len(fake.calledDelete) != 0 || len(fake.calledRotate) != 0 || fake.initCalls != 0 {
		t.Fatal("outer refusal invoked mutation")
	}
}

func TestSecretsCapabilityRouteLegacyEncryptedDelete(t *testing.T) {
	key, vaultPath, state := httpPrivateInitFixture(t)
	if result, err := api.NewAPI().SecretsInit(); err != nil || result.VaultState != "ok" {
		t.Fatal("private public initialization failed", err)
	}
	vault, err := secrets.OpenVault(key, vaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = vault.ImportYAML([]byte("init-capabilities: SYNTHETIC-LEGACY\nK1: SYNTHETIC-CONTROL\n")); err != nil {
		t.Fatal(err)
	}
	readState := func() ([]string, string) {
		t.Helper()
		v, err := secrets.OpenVault(key, vaultPath)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(vaultPath)
		if err != nil || !bytes.HasPrefix(raw, []byte("age-encryption.org/v1")) {
			t.Fatal("private vault is not valid encrypted storage", err)
		}
		return v.List(), fmt.Sprintf("%x", sha256.Sum256(raw))
	}
	initial, _ := readState()
	if strings.Join(initial, ",") != "K1,init-capabilities" {
		t.Fatal("supported import did not persist both exact names")
	}
	s := NewServer(Config{Port: 9081})
	t.Cleanup(s.Broadcaster().Close)
	var observations []map[string]any
	for _, c := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"get_before", http.MethodGet, "/api/secrets/init-capabilities", "", 200},
		{"legacy_put", http.MethodPut, "/api/secrets/init-capabilities", `{"value":"SYNTHETIC-ROTATE","restart":false}`, 400},
		{"legacy_delete", http.MethodDelete, "/api/secrets/init-capabilities?confirm=true", "", 204},
		{"ordinary_delete", http.MethodDelete, "/api/secrets/K1?confirm=true", "", 204},
		{"get_after", http.MethodGet, "/api/secrets/init-capabilities", "", 200},
	} {
		beforeNames, beforeHash := readState()
		req := httptest.NewRequest(c.method, "http://127.0.0.1:9081"+c.path, strings.NewReader(c.body))
		if c.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		afterNames, afterHash := readState()
		if rec.Code != c.status {
			t.Errorf("actual %s status=%d want=%d", c.name, rec.Code, c.status)
		}
		if strings.HasPrefix(c.name, "get_") {
			if rec.Body.String() != `{"schema_version":1,"workflow":"cli_support_readonly","path_free_errors":true}`+"\n" || rec.Header().Get("Cache-Control") != "no-store" || beforeHash != afterHash || strings.Join(beforeNames, ",") != strings.Join(afterNames, ",") {
				t.Error("capability bytes or encrypted store changed")
			}
		}
		if c.name == "legacy_put" && beforeHash != afterHash {
			t.Error("invalid setter acquired new write permission")
		}
		if c.name == "legacy_delete" && strings.Contains(strings.Join(afterNames, ","), "init-capabilities") {
			t.Error("stored legacy key remained after DELETE")
		}
		if c.name == "ordinary_delete" && strings.Contains(strings.Join(afterNames, ","), "K1") {
			t.Error("stored ordinary key remained after DELETE")
		}
		observations = append(observations, map[string]any{"case": c.name, "method": c.method, "status": rec.Code, "body": rec.Body.String(), "headers": rec.Header(), "before_names": beforeNames, "after_names": afterNames, "before_vault_sha256": beforeHash, "after_vault_sha256": afterHash})
	}
	s.Broadcaster().Close() // joins the existing durable persistence owner before inspection
	raw, err := os.ReadFile(filepath.Join(state, "gui-events.log"))
	if err != nil {
		t.Fatal("canonical delete events were not durably received", err)
	}
	deleteKeys := []string{}
	for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
		var ev struct {
			Type string `json:"type"`
			Body struct {
				Action string `json:"action"`
				Key    string `json:"key"`
			} `json:"body"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type == "operator-action" && ev.Body.Action == "secret-delete" {
			deleteKeys = append(deleteKeys, ev.Body.Key)
		}
	}
	if strings.Join(deleteKeys, ",") != "init-capabilities,K1" {
		t.Fatalf("durable post-commit delete events=%v", deleteKeys)
	}
	if proof := os.Getenv("MCPHUB_HTTP_PROOF_DIR"); proof != "" {
		summary, err := json.Marshal(map[string]any{"observations": observations, "durable_delete_keys": deleteKeys, "key_ciphertext_or_actor_retained": false})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(proof, "route-green-observations.json"), summary, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSecretsInit_HTTPOuterGuards(t *testing.T) {
	for _, path := range []string{"/api/secrets/init", "/api/secrets/init-capabilities"} {
		for _, kind := range []string{"host", "origin", "fetch_site"} {
			t.Run(path+"/"+kind, func(t *testing.T) {
				s := newEphemeralServer(t, Config{Port: 9081})
				fake := &fakeSecretsAPI{panicOnInit: true}
				s.secrets = fake
				method := http.MethodPost
				if strings.HasSuffix(path, "capabilities") {
					method = http.MethodGet
				}
				req := httptest.NewRequest(method, "http://127.0.0.1:9081"+path, nil)
				switch kind {
				case "host":
					req.Host = "evil.invalid:9081"
				case "origin":
					req.Header.Set("Origin", "http://evil.invalid:9081")
				case "fetch_site":
					req.Header.Set("Sec-Fetch-Site", "cross-site")
				}
				rec := httptest.NewRecorder()
				s.ServeHTTP(rec, req)
				if rec.Code != 403 || fake.initCalls != 0 {
					t.Errorf("outer guard=%d calls=%d", rec.Code, fake.initCalls)
				}
			})
		}
	}
}

func httpPrivateInitFixture(t *testing.T) (key, vault, state string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("HOME", dir)
	key, vault = secrets.DefaultKeyPath(), secrets.DefaultVaultPath()
	for _, p := range []string{key, vault, vault + ".lock"} {
		rel, err := filepath.Rel(os.TempDir(), p)
		if err != nil || !filepath.IsAbs(p) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatal("HTTP init fixture escaped private temporary targets")
		}
		for current := p; ; current = filepath.Dir(current) {
			info, err := os.Lstat(current)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir() && !info.Mode().IsRegular()) {
				t.Fatal("HTTP fixture ancestry is not ordinary")
			}
			if current == filepath.Clean(os.TempDir()) {
				break
			}
		}
	}
	state = apitest.HardenedTempDir(t)
	restore := api.SetDaemonStateRootForTest(state)
	t.Cleanup(restore)
	return
}

func TestSecretsInit_HTTPDefaultRealAdapter(t *testing.T) {
	key, vault, _ := httpPrivateInitFixture(t)
	s := newEphemeralServer(t, Config{Port: 9081})
	for i := 0; i < 2; i++ {
		rec := serveHTTPInitProof(t, s, http.MethodPost, "/api/secrets/init")
		saveHTTPInitProof(t, fmt.Sprintf("default-real-success-%d", i), rec)
		if rec.Code != 200 || rec.Body.String() != `{"vault_state":"ok"}`+"\n" {
			t.Fatalf("real adapter=%d %q", rec.Code, rec.Body.String())
		}
	}
	if _, err := secrets.OpenVault(key, vault); err != nil {
		t.Fatal("default real adapter did not produce an openable vault", err)
	}
}

func TestSecretsInit_HTTPActualUntyped(t *testing.T) {
	key, _, _ := httpPrivateInitFixture(t)
	if err := os.WriteFile(filepath.Dir(key), []byte("SYNTHETIC-NONDIRECTORY"), 0600); err != nil {
		t.Fatal(err)
	}
	_, cause := api.NewAPI().SecretsInit()
	if cause == nil {
		t.Fatal("ordinary-file vault parent did not produce actual API error")
	}
	var failed *api.SecretsInitFailed
	var persistence *api.SecretsRecoveryPersistenceError
	if errors.As(cause, &failed) || errors.As(cause, &persistence) {
		t.Fatalf("fixture returned typed instead of untyped error: %T", cause)
	}
	s := newEphemeralServer(t, Config{Port: 9081})
	s.secrets = &fakeSecretsAPI{initErr: cause}
	rec := serveHTTPInitProof(t, s, http.MethodPost, "/api/secrets/init")
	saveHTTPInitProof(t, "actual-api-untyped", rec)
	body := assertHTTPInitContained(t, rec, 500, cause.Error(), filepath.Dir(key))
	if body["code"] != "SECRETS_INIT_FAILED" {
		t.Errorf("untyped code=%v", body["code"])
	}
}

// --- Step 3.B.6: GET /api/secrets ---

func TestSecretsList_OK(t *testing.T) {
	fake := &fakeSecretsAPI{
		listResult: api.SecretsEnvelope{
			VaultState: "ok",
			Secrets: []api.SecretRow{
				{Name: "K1", State: "present", UsedBy: []api.UsageRef{{Server: "s1", EnvVar: "OPENAI_API_KEY"}}},
			},
			ManifestErrors: []api.ManifestError{},
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var body api.SecretsEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.VaultState != "ok" || len(body.Secrets) != 1 {
		t.Errorf("body=%+v", body)
	}
	if body.Secrets[0].UsedBy[0].Server != "s1" {
		t.Errorf("used_by=%+v", body.Secrets[0].UsedBy)
	}
}

// --- Step 3.B.7: POST /api/secrets happy + error codes ---

func TestSecretsAdd_201Created(t *testing.T) {
	fake := &fakeSecretsAPI{}
	s := newServerWithSecretsFake(t, fake)

	body := bytes.NewReader([]byte(`{"name":"K1","value":"v"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(fake.calledSet) != 1 || fake.calledSet[0].Name != "K1" {
		t.Errorf("calledSet=%+v", fake.calledSet)
	}
}

// TestSecretsAdd_EmitsGUIEventWithKeyNameOnlyNeverValue covers the
// deep-review P3 observability finding for secrets.go: the POST
// /api/secrets success path must emit a gui-events.log row (so the
// operator-significant mutation is auditable from the SSE/log surface,
// not just hub-mcp.log), and that row must carry the key NAME only —
// the secret VALUE must never appear in the persisted body.
//
// Asserts via the Broadcaster's own SSE subscription (the same channel
// that feeds the gui-events.log persist drain — see events.go Publish)
// rather than tailing the shared on-disk gui-events.log: internal/gui's
// TestMain fences the whole test BINARY to one state root, not one file
// per test, so multiple tests in this package append to the same
// physical file and a tail-read race against sibling tests is real (not
// hypothetical — observed directly: a sibling backups_actions_test.go
// row was the most-recent entry when this test's own row hadn't landed
// yet). Subscribing to this server's own Broadcaster instance isolates
// the assertion to exactly the event this test's handler call produced.
func TestSecretsAdd_EmitsGUIEventWithKeyNameOnlyNeverValue(t *testing.T) {
	fake := &fakeSecretsAPI{}
	s := newServerWithSecretsFake(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Broadcaster().Subscribe(ctx)

	const secretValue = "super-secret-credential-material-xyz"
	body := bytes.NewReader([]byte(`{"name":"MY_API_KEY","value":"` + secretValue + `"}`))
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}

	select {
	case ev := <-ch:
		if ev.Type != "operator-action" {
			t.Fatalf("event type = %q, want operator-action", ev.Type)
		}
		if ev.Body["action"] != "secret-set" {
			t.Errorf("action=%v, want secret-set", ev.Body["action"])
		}
		if ev.Body["key"] != "MY_API_KEY" {
			t.Errorf("key=%v, want MY_API_KEY", ev.Body["key"])
		}
		raw, _ := json.Marshal(ev.Body)
		if bytes.Contains(raw, []byte(secretValue)) {
			t.Fatalf("gui-events row leaked the secret VALUE: %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no operator-action event published after a successful secret Set")
	}
}

func TestSecretsAdd_409KeyExists(t *testing.T) {
	fake := &fakeSecretsAPI{setErr: &api.SecretsOpError{Code: "SECRETS_KEY_EXISTS", Msg: "exists"}}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader([]byte(`{"name":"K1","value":"v"}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsAdd_400InvalidName(t *testing.T) {
	fake := &fakeSecretsAPI{setErr: &api.SecretsOpError{Code: "SECRETS_INVALID_NAME", Msg: "bad"}}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader([]byte(`{"name":"123","value":"v"}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsAdd_400MalformedJSON(t *testing.T) {
	fake := &fakeSecretsAPI{}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader([]byte(`{not-json`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "SECRETS_INVALID_JSON" {
		t.Errorf("code=%q", body["code"])
	}
}

// --- Step 3.B.8: PUT /api/secrets/:key ---

func TestSecretsRotate_200WhenAllOK(t *testing.T) {
	fake := &fakeSecretsAPI{
		rotateResult: api.SecretsRotateResult{
			VaultUpdated:   true,
			RestartResults: []api.RestartResult{{TaskName: "x", Err: ""}},
		},
	}
	s := newServerWithSecretsFake(t, fake)

	body := bytes.NewReader([]byte(`{"value":"new","restart":true}`))
	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", body)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(fake.calledRotate) != 1 || fake.calledRotate[0].Name != "K1" {
		t.Errorf("calledRotate=%+v", fake.calledRotate)
	}
}

func TestSecretsRotate_207WhenAnyTaskFailed(t *testing.T) {
	fake := &fakeSecretsAPI{
		rotateResult: api.SecretsRotateResult{
			VaultUpdated: true,
			RestartResults: []api.RestartResult{
				{TaskName: "a", Err: ""},
				{TaskName: "b", Err: "timeout"},
			},
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", bytes.NewReader([]byte(`{"value":"v","restart":true}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status=%d, want 207", rec.Code)
	}
}

func TestSecretsRotate_500OrchestrationFailureCarriesPartial(t *testing.T) {
	fake := &fakeSecretsAPI{
		rotateResult: api.SecretsRotateResult{
			VaultUpdated:   true,
			RestartResults: []api.RestartResult{{TaskName: "a", Err: ""}},
		},
		rotateErr: errors.New("scheduler unavailable"),
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", bytes.NewReader([]byte(`{"value":"v","restart":true}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "RESTART_FAILED" {
		t.Errorf("code=%v", body["code"])
	}
	if body["vault_updated"] != true {
		t.Errorf("vault_updated=%v", body["vault_updated"])
	}
	if results, _ := body["restart_results"].([]any); len(results) != 1 {
		t.Errorf("partial results dropped: %v", body["restart_results"])
	}
}

// TestSecretsRotate_AuditsCommittedRotationEvenWhenRestartFails covers
// the PR #476 bot P2 regression: a rotate that successfully updates the
// vault (VaultUpdated:true) but then fails the restart orchestration
// (500 RESTART_FAILED) must STILL emit the secret-rotate gui-events
// audit row — the credential DID change, so it must be auditable. The
// old code emitted only on the clean-success path, leaving a committed
// credential mutation with no row. KEY NAME only, never value.
func TestSecretsRotate_AuditsCommittedRotationEvenWhenRestartFails(t *testing.T) {
	const secretValue = "rotated-credential-material-abc"
	fake := &fakeSecretsAPI{
		rotateResult: api.SecretsRotateResult{
			VaultUpdated:   true,
			RestartResults: []api.RestartResult{{TaskName: "a", Err: ""}},
		},
		rotateErr: errors.New("scheduler unavailable"),
	}
	s := newServerWithSecretsFake(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Broadcaster().Subscribe(ctx)

	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1",
		bytes.NewReader([]byte(`{"value":"`+secretValue+`","restart":true}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500 (restart failed), body=%q", rec.Code, rec.Body.String())
	}

	select {
	case ev := <-ch:
		if ev.Type != "operator-action" || ev.Body["action"] != "secret-rotate" {
			t.Fatalf("event = %+v, want operator-action/secret-rotate", ev)
		}
		if ev.Body["key"] != "K1" {
			t.Errorf("key=%v, want K1", ev.Body["key"])
		}
		raw, _ := json.Marshal(ev.Body)
		if bytes.Contains(raw, []byte(secretValue)) {
			t.Fatalf("secret-rotate audit row leaked the secret VALUE: %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no secret-rotate audit event after a committed rotate whose restart failed (PR #476 P2 regression)")
	}
}

// TestSecretsRotate_NoAuditOnPreCommitFailure guards the inverse: a
// pre-commit validation failure (*SecretsOpError, VaultUpdated:false)
// must NOT emit an audit row — nothing changed in the vault.
func TestSecretsRotate_NoAuditOnPreCommitFailure(t *testing.T) {
	fake := &fakeSecretsAPI{
		rotateResult: api.SecretsRotateResult{VaultUpdated: false},
		rotateErr:    &api.SecretsOpError{Code: "SECRETS_EMPTY_VALUE", Msg: "value must not be empty"},
	}
	s := newServerWithSecretsFake(t, fake)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := s.Broadcaster().Subscribe(ctx)

	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1",
		bytes.NewReader([]byte(`{"value":"x","restart":false}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (empty value), body=%q", rec.Code, rec.Body.String())
	}

	select {
	case ev := <-ch:
		t.Fatalf("unexpected audit event on pre-commit rotate failure: %+v", ev)
	case <-time.After(200 * time.Millisecond):
		// expected: no event (vault unchanged)
	}
}

// --- Step 3.B.9: POST /api/secrets/:key/restart ---

func TestSecretsRestart_200(t *testing.T) {
	fake := &fakeSecretsAPI{
		restartResults: []api.RestartResult{{TaskName: "x", Err: ""}},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/K1/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(fake.calledRestart) != 1 || fake.calledRestart[0] != "K1" {
		t.Errorf("calledRestart=%+v", fake.calledRestart)
	}
}

func TestSecretsRestart_207OnPartialFailure(t *testing.T) {
	fake := &fakeSecretsAPI{
		restartResults: []api.RestartResult{
			{TaskName: "a", Err: ""},
			{TaskName: "b", Err: "timeout"},
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/K1/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("status=%d", rec.Code)
	}
}

// --- Step 3.B.10: DELETE /api/secrets/:key ---

func TestSecretsDelete_204OnSuccess(t *testing.T) {
	fake := &fakeSecretsAPI{}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(fake.calledDelete) != 1 || fake.calledDelete[0].Confirm {
		t.Errorf("calledDelete=%+v", fake.calledDelete)
	}
}

func TestSecretsDelete_409HasRefs(t *testing.T) {
	fake := &fakeSecretsAPI{
		deleteErr: &api.SecretsDeleteError{
			Code:    "SECRETS_HAS_REFS",
			Message: "refs",
			UsedBy:  []api.UsageRef{{Server: "alpha", EnvVar: "OPENAI"}},
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "SECRETS_HAS_REFS" {
		t.Errorf("code=%v", body["code"])
	}
	usedBy, _ := body["used_by"].([]any)
	if len(usedBy) != 1 {
		t.Errorf("used_by=%+v", usedBy)
	}
}

func TestSecretsDelete_409UsageScanIncomplete(t *testing.T) {
	fake := &fakeSecretsAPI{
		deleteErr: &api.SecretsDeleteError{
			Code:           "SECRETS_USAGE_SCAN_INCOMPLETE",
			Message:        "scan incomplete",
			ManifestErrors: []api.ManifestError{{Path: "broken/manifest.yaml", Error: "yaml: line 1"}},
		},
	}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "SECRETS_USAGE_SCAN_INCOMPLETE" {
		t.Errorf("code=%v", body["code"])
	}
}

func TestSecretsDelete_204WithConfirmTrue(t *testing.T) {
	fake := &fakeSecretsAPI{}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1?confirm=true", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d", rec.Code)
	}
	if len(fake.calledDelete) != 1 || !fake.calledDelete[0].Confirm {
		t.Errorf("calledDelete=%+v (confirm=true expected)", fake.calledDelete)
	}
}

// --- Step 3.B.11: Same-origin guard smoke ---

func TestSecrets_RejectsCrossOrigin(t *testing.T) {
	fake := &fakeSecretsAPI{}
	s := newServerWithSecretsFake(t, fake)

	req := httptest.NewRequest(http.MethodPost, "/api/secrets/init", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "CROSS_ORIGIN" {
		t.Errorf("code=%q, want CROSS_ORIGIN", body["code"])
	}
}

// --- Step 3.B.11b: Full handler-error-matrix coverage ---

// GET /api/secrets — vault state branches.

func TestSecretsList_VaultMissingState(t *testing.T) {
	fake := &fakeSecretsAPI{
		listResult: api.SecretsEnvelope{
			VaultState: "missing", Secrets: []api.SecretRow{}, ManifestErrors: []api.ManifestError{},
		},
	}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var body api.SecretsEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.VaultState != "missing" {
		t.Errorf("vault_state=%q", body.VaultState)
	}
}

func TestSecretsList_500ListFailed(t *testing.T) {
	fake := &fakeSecretsAPI{listErr: fmt.Errorf("scan blew up")}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "SECRETS_LIST_FAILED" {
		t.Errorf("code=%q", body["code"])
	}
}

// POST /api/secrets — full SecretsOpError matrix.

func TestSecretsAdd_400EmptyValue(t *testing.T) {
	fake := &fakeSecretsAPI{setErr: &api.SecretsOpError{Code: "SECRETS_EMPTY_VALUE", Msg: "empty"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader([]byte(`{"name":"K","value":""}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsAdd_409VaultNotInitialized(t *testing.T) {
	fake := &fakeSecretsAPI{setErr: &api.SecretsOpError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: "no vault"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader([]byte(`{"name":"K","value":"v"}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsAdd_500SetFailed(t *testing.T) {
	fake := &fakeSecretsAPI{setErr: &api.SecretsOpError{Code: "SECRETS_SET_FAILED", Msg: "disk"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader([]byte(`{"name":"K","value":"v"}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
}

// PUT /api/secrets/:key — error matrix.

func TestSecretsRotate_404KeyNotFound(t *testing.T) {
	fake := &fakeSecretsAPI{rotateErr: &api.SecretsOpError{Code: "SECRETS_KEY_NOT_FOUND", Msg: "missing"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", bytes.NewReader([]byte(`{"value":"v","restart":false}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsRotate_409VaultNotInitialized(t *testing.T) {
	fake := &fakeSecretsAPI{rotateErr: &api.SecretsOpError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: "no vault"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", bytes.NewReader([]byte(`{"value":"v","restart":false}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsRotate_500SetFailed(t *testing.T) {
	fake := &fakeSecretsAPI{rotateErr: &api.SecretsOpError{Code: "SECRETS_SET_FAILED", Msg: "disk"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", bytes.NewReader([]byte(`{"value":"v","restart":false}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsRotate_400EmptyValue(t *testing.T) {
	fake := &fakeSecretsAPI{rotateErr: &api.SecretsOpError{Code: "SECRETS_EMPTY_VALUE", Msg: "empty"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPut, "/api/secrets/K1", bytes.NewReader([]byte(`{"value":"","restart":false}`)))
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rec.Code)
	}
}

// POST /api/secrets/:key/restart — error matrix.

func TestSecretsRestart_404KeyNotFound(t *testing.T) {
	fake := &fakeSecretsAPI{restartErr: &api.SecretsOpError{Code: "SECRETS_KEY_NOT_FOUND", Msg: "missing"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets/K1/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsRestart_409VaultNotInitialized(t *testing.T) {
	fake := &fakeSecretsAPI{restartErr: &api.SecretsOpError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: "no vault"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets/K1/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsRestart_500OrchestrationFailure(t *testing.T) {
	fake := &fakeSecretsAPI{
		restartResults: []api.RestartResult{{TaskName: "a", Err: ""}},
		restartErr:     fmt.Errorf("scheduler unavailable"),
	}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodPost, "/api/secrets/K1/restart", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "RESTART_FAILED" {
		t.Errorf("code=%v", body["code"])
	}
	if results, _ := body["restart_results"].([]any); len(results) != 1 {
		t.Errorf("partial results dropped")
	}
}

// DELETE /api/secrets/:key — error matrix.

func TestSecretsDelete_404KeyNotFound(t *testing.T) {
	fake := &fakeSecretsAPI{deleteErr: &api.SecretsOpError{Code: "SECRETS_KEY_NOT_FOUND", Msg: "missing"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsDelete_409VaultNotInitialized(t *testing.T) {
	fake := &fakeSecretsAPI{deleteErr: &api.SecretsOpError{Code: "SECRETS_VAULT_NOT_INITIALIZED", Msg: "no vault"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestSecretsDelete_500DeleteFailed(t *testing.T) {
	fake := &fakeSecretsAPI{deleteErr: &api.SecretsOpError{Code: "SECRETS_DELETE_FAILED", Msg: "disk"}}
	s := newServerWithSecretsFake(t, fake)
	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/K1?confirm=true", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", rec.Code)
	}
}

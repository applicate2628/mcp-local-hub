package gui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/api"
)

// fakeSecretsAPI is the test double for secretsAPI. It records all
// calls made by the handlers so tests can assert dispatch correctness.
type fakeSecretsAPI struct {
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

	calledSet     []struct{ Name, Value string }
	calledRotate  []struct {
		Name, Value string
		Restart     bool
	}
	calledRestart []string
	calledDelete  []struct {
		Name    string
		Confirm bool
	}
}

func (f *fakeSecretsAPI) Init() (api.SecretsInitResult, error) { return f.initResult, f.initErr }
func (f *fakeSecretsAPI) List() (api.SecretsEnvelope, error)   { return f.listResult, f.listErr }
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
	s := NewServer(Config{})
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
	if body["orphan_path"] != "/some/path/secrets.age" {
		t.Errorf("orphan_path=%v", body["orphan_path"])
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

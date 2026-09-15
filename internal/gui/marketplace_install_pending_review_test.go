package gui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// Creation and name presence share real isolated manifest storage. Only the
// process-install side effect is substituted; it must consume that same file.
type pendingMarketplaceFixture struct {
	dir           string
	creates       int
	installs      int
	installErr    error
	installedYAML []byte
}

func (f *pendingMarketplaceFixture) ManifestCreate(name, yaml string) error {
	if err := api.NewAPI().ManifestCreateIn(f.dir, name, yaml); err != nil {
		return err
	}
	f.creates++
	return nil
}

func (f *pendingMarketplaceFixture) ServerExists(name string) (bool, error) {
	_, err := os.Stat(filepath.Join(f.dir, name, "manifest.yaml"))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (f *pendingMarketplaceFixture) Install(name string, _ int) error {
	f.installs++
	raw, err := os.ReadFile(filepath.Join(f.dir, name, "manifest.yaml"))
	if err != nil {
		return err
	}
	if f.installErr != nil {
		return f.installErr
	}
	f.installedYAML = raw
	return nil
}

func TestMarketplaceInstallPendingUsesExistingManifestContinuation(t *testing.T) {
	const name = "pending-review-fixture"
	const canary = "C:\\private\\operator\\lease"
	fixture := &pendingMarketplaceFixture{dir: t.TempDir(), installErr: fmt.Errorf("%s: %w", canary, &api.LeaseFailure{FailureID: "E_ADOPT_LEASE_BUSY", Retryable: true})}
	s := newMarketplaceInstallTestServer(&fakeMarketplaceEntryLoader{entry: stdioEntry(name), found: true}, &fakeGlobalPortPicker{port: 9207}, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, &fakeManifestCreator{}, &fakeInstaller{})
	s.manifestCreator, s.marketplaceNamePresence, s.installer = fixture, fixture, fixture
	registerInstallRoutes(s)
	previous := installShadowWarnFn
	installShadowWarnFn = func(string) string { return "" }
	t.Cleanup(func() { installShadowWarnFn = previous })

	rec := postInstall(t, s, `{"id":"`+name+`","mode":"hub"}`, "same-origin")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "MANIFEST_CREATED_INSTALL_PENDING" || body["retryable"] != false || body["name"] != name || body["failure_id"] != "E_ADOPT_LEASE_BUSY" {
		t.Errorf("post-create refusal advertises wrong recovery: %s", rec.Body.String())
	}
	message, _ := body["error"].(string)
	if !strings.Contains(message, "Servers") || !strings.Contains(message, "Install") || !strings.Contains(message, name) {
		t.Errorf("missing existing-server recovery action: %q", message)
	}
	if strings.Contains(rec.Body.String(), canary) || body["suggested_name"] != nil {
		t.Errorf("response leaked backend details or suggests duplicate creation: %s", rec.Body.String())
	}
	path := filepath.Join(fixture.dir, name, "manifest.yaml")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if exists, err := fixture.ServerExists(name); err != nil || !exists {
		t.Fatalf("created manifest not visible: exists=%v err=%v", exists, err)
	}

	// The recovery action is install-existing, not a second marketplace create.
	fixture.installErr = nil
	req := httptest.NewRequest(http.MethodPost, "/api/install?name="+name, nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	continued := httptest.NewRecorder()
	s.mux.ServeHTTP(continued, req)
	if continued.Code != http.StatusNoContent {
		t.Fatalf("existing install status=%d body=%s", continued.Code, continued.Body.String())
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || !bytes.Equal(before, fixture.installedYAML) {
		t.Fatalf("continuation changed or replaced the created manifest: %v", err)
	}
	entries, err := os.ReadDir(fixture.dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != name || fixture.creates != 1 || fixture.installs != 2 {
		t.Fatalf("continuation duplicated work: entries=%v creates=%d installs=%d err=%v", entries, fixture.creates, fixture.installs, err)
	}
}

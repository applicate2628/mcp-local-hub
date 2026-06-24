package gui

import (
	"encoding/json"
	"net/http"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestMarketplaceInstall_InertEntryBlockedBeforeDispatch is the per-path D-3
// regression for the marketplace one-click install handler (Tier-0 catalog
// findings, finding 1). An inert (availability=watch / disabled-until-probe)
// catalog entry whose install-probe has NOT passed must be refused by the shared
// availability admission gate (api.AvailabilityAdmissionEntry) immediately after
// LoadEntry and BEFORE either dispatch path runs — the direct path writes client
// configs without ever reaching the manifest AdmissionCheck, so without this gate
// it would bypass D-3 entirely. The probe binary is a definitely-absent name so
// it cannot pass on any host. Asserts BOTH modes are blocked and that no
// downstream seam (direct writer / installer) was reached.
func TestMarketplaceInstall_InertEntryBlockedBeforeDispatch(t *testing.T) {
	inertProbe := &api.CatalogAvailabilityProbe{Binaries: []string{"definitely-not-on-path-xyz"}}

	t.Run("direct", func(t *testing.T) {
		entry := &api.MarketplaceEntry{
			ID:           "inert-http",
			Name:         "Inert HTTP",
			Transport:    "http",
			URL:          "https://example.com/mcp",
			Availability: "watch",
			InstallProbe: inertProbe,
		}
		loader := &fakeMarketplaceEntryLoader{entry: entry, found: true}
		writer := &fakeDirectClientWriter{updated: []string{"claude-code"}}
		s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, writer, &fakeManifestCreator{}, &fakeInstaller{})

		rec := postInstall(t, s, `{"id":"inert-http","mode":"direct","clients":["claude-code"]}`, "same-origin")
		// 412 Precondition Failed (NOT 409) — the host-probe precondition is unmet.
		// 409 is the NAME_CONFLICT status the frontend branches on by HTTP code, so
		// the probe-pending gate uses a distinct status to avoid misrouting.
		if rec.Code != http.StatusPreconditionFailed {
			t.Fatalf("status = %d, want 412 (inert entry blocked), body=%q", rec.Code, rec.Body.String())
		}
		if writer.called {
			t.Fatalf("direct client writer was reached for an inert entry — D-3 gate bypassed")
		}
		var body struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Code != "AVAILABILITY_PROBE_PENDING" {
			t.Errorf("code = %q, want AVAILABILITY_PROBE_PENDING; body=%q", body.Code, rec.Body.String())
		}
	})

	t.Run("hub", func(t *testing.T) {
		entry := &api.MarketplaceEntry{
			ID:           "inert-stdio",
			Name:         "Inert Stdio",
			Transport:    "stdio",
			Command:      "npx",
			Args:         []string{"-y", "@example/inert"},
			Availability: "disabled-until-probe",
			InstallProbe: inertProbe,
		}
		loader := &fakeMarketplaceEntryLoader{entry: entry, found: true}
		creator := &fakeManifestCreator{}
		installer := &fakeInstaller{}
		s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{port: 9207}, &fakeServerNamePresence{}, &fakeDirectClientWriter{}, creator, installer)

		rec := postInstall(t, s, `{"id":"inert-stdio","mode":"hub"}`, "same-origin")
		// 412 Precondition Failed (NOT 409) — see the direct subtest rationale.
		if rec.Code != http.StatusPreconditionFailed {
			t.Fatalf("status = %d, want 412 (inert entry blocked), body=%q", rec.Code, rec.Body.String())
		}
		if creator.name != "" {
			t.Fatalf("ManifestCreate was reached for an inert entry (name=%q) — D-3 gate bypassed", creator.name)
		}
		if installer.called {
			t.Fatalf("Install was reached for an inert entry — D-3 gate bypassed")
		}
	})
}

// TestMarketplaceInstall_AbsentAvailabilityFieldsByteIdentical confirms the
// additive guarantee: an entry with NO availability / install_probe (every
// current catalog row) passes the new gate untouched and reaches the same
// downstream dispatch it did before Tier-0. The direct-http happy path must
// still 200 and reach the client writer.
func TestMarketplaceInstall_AbsentAvailabilityFieldsByteIdentical(t *testing.T) {
	entry := &api.MarketplaceEntry{
		ID:        "qt-docs",
		Name:      "Qt Docs",
		Transport: "http",
		URL:       "https://example.com/qt-docs/mcp",
		// No Availability / InstallProbe — absent == ready.
	}
	loader := &fakeMarketplaceEntryLoader{entry: entry, found: true}
	writer := &fakeDirectClientWriter{updated: []string{"claude-code"}}
	s := newMarketplaceInstallTestServer(loader, &fakeGlobalPortPicker{}, &fakeServerNamePresence{}, writer, &fakeManifestCreator{}, &fakeInstaller{})

	rec := postInstall(t, s, `{"id":"qt-docs","mode":"direct","clients":["claude-code"]}`, "same-origin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (absent fields == ready, byte-identical), body=%q", rec.Code, rec.Body.String())
	}
	if !writer.called {
		t.Fatalf("direct client writer not reached for a ready entry — gate over-blocked")
	}
}

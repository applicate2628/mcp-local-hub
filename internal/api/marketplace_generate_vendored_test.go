package api

import (
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// TestGenerateDraft_ProjectsVendoredAndAvailability proves the Tier-0 projection
// (D-2 + D-3): a stdio catalog entry carrying vendored_source + availability +
// install_probe drafts a manifest that carries those keys, so the persisted
// manifest's gate can re-evaluate the pin/probe post-install. The drafted YAML
// (with a real port substituted for the placeholder 0) also passes
// ParseManifest+Validate, proving the projection emits a SCHEMA-VALID shape.
func TestGenerateDraft_ProjectsVendoredAndAvailability(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "mathcad",
		Name:      "Mathcad MCP",
		Transport: "stdio",
		Command:   "python",
		Args:      []string{"-m", "mathcad_mcp"},
		VendoredSource: &CatalogVendoredSource{
			Repo:          "https://github.com/puran-water/mathcad-mcp",
			PinnedRef:     "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			InstallCmd:    "uv pip install .",
			RunCmd:        "python -m mathcad_mcp",
			LicenseStatus: "confirmed",
		},
		Availability: "watch",
		InstallProbe: &CatalogAvailabilityProbe{Binaries: []string{"matlab"}},
	}
	got, _, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: "/ws"})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	for _, want := range []string{
		"vendored_source:",
		"pinned_ref: a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
		"license_status: confirmed",
		"availability: watch",
		"install_probe:",
		"matlab",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("draft missing %q\n---\n%s", want, got)
		}
	}

	// The draft (with the placeholder port 0 replaced by a real port) must parse
	// + validate as a manifest — proving the projection is schema-valid.
	parseReady := strings.Replace(got, "port: 0", "port: 9999", 1)
	m, err := config.ParseManifest(strings.NewReader(parseReady))
	if err != nil {
		t.Fatalf("drafted manifest failed ParseManifest+Validate: %v\n---\n%s", err, parseReady)
	}
	if m.VendoredSource == nil || m.VendoredSource.PinnedRef == "" {
		t.Fatalf("parsed manifest lost vendored_source: %#v", m.VendoredSource)
	}
	if m.Availability != config.AvailabilityWatch || m.InstallProbe == nil {
		t.Fatalf("parsed manifest lost availability/probe: av=%q probe=%#v", m.Availability, m.InstallProbe)
	}
}

// TestGenerateDraft_NoVendoredFieldsByteIdentical proves a catalog entry WITHOUT
// the Tier-0 fields drafts a manifest that carries none of the new keys — the
// additive projection guarantee.
func TestGenerateDraft_NoVendoredFieldsByteIdentical(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "plain",
		Name:      "Plain",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"-y", "x"},
	}
	got, _, err := GenerateDraftManifest(e, GenerateOpts{WorkspaceFolder: "/ws"})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	for _, key := range []string{"vendored_source", "availability", "install_probe"} {
		if strings.Contains(got, key) {
			t.Fatalf("plain entry draft contains Tier-0 key %q\n---\n%s", key, got)
		}
	}
}

// TestGenerateDraft_RemoteHTTPDoesNotProjectVendored proves an http (remote-http
// S2) entry does NOT emit vendored_source — a vendored fork is a local-stdio S1
// concern. The remote draft path is left untouched by Tier-0.
func TestGenerateDraft_RemoteHTTPDoesNotProjectVendored(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "remote",
		Name:      "Remote",
		Transport: "http",
		URL:       "https://mcp.example.com/mcp",
		// Even if a (nonsensical) vendored_source were present on an http entry,
		// the remote draft path must not project it.
		VendoredSource: &CatalogVendoredSource{PinnedRef: "v1"},
	}
	got, _, err := GenerateDraftManifest(e, GenerateOpts{})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	if strings.Contains(got, "vendored_source") {
		t.Fatalf("remote-http draft projected vendored_source:\n---\n%s", got)
	}
}

// TestGenerateDraft_RemoteHTTPProjectsAvailability is the FINDING-3 (D-3)
// regression: an inert (watch / disabled-until-probe) http catalog entry's
// generated remote-http draft MUST carry the D-3 availability + install_probe
// gate, so the manual generate→`manifest create`→install workflow cannot bypass
// the readiness gate (the persisted remote-http manifest would otherwise look
// ready). vendored_source must still be ABSENT (a local-stdio concern only). The
// drafted YAML must also parse+validate as a remote-http manifest, proving the
// projection is schema-valid for this transport.
func TestGenerateDraft_RemoteHTTPProjectsAvailability(t *testing.T) {
	// HOST-absolute probe path: the D-3 validator now requires install_probe.files
	// to be absolute per filepath.IsAbs, which is GOOS-specific (a POSIX
	// "/opt/host-token.json" is NOT absolute on Windows). t.TempDir is absolute on
	// every OS, so the drafted manifest validates cross-platform.
	probeFile := filepath.Join(t.TempDir(), "host-token.json")
	e := &MarketplaceEntry{
		ID:           "remote-gated",
		Name:         "Remote Gated",
		Transport:    "http",
		URL:          "https://mcp.example.com/mcp",
		Availability: "watch",
		InstallProbe: &CatalogAvailabilityProbe{Files: []string{probeFile}},
		// A vendored_source on an http entry must NOT be projected even though the
		// D-3 fields now are.
		VendoredSource: &CatalogVendoredSource{PinnedRef: "v1"},
	}
	got, _, err := GenerateDraftManifest(e, GenerateOpts{})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	for _, want := range []string{"availability: watch", "install_probe:", probeFile} {
		if !strings.Contains(got, want) {
			t.Fatalf("remote-http draft missing D-3 key %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "vendored_source") {
		t.Fatalf("remote-http draft projected vendored_source (local-stdio concern)\n---\n%s", got)
	}
	// The draft must parse+validate as a remote-http manifest carrying the gate.
	m, err := config.ParseManifest(strings.NewReader(got))
	if err != nil {
		t.Fatalf("drafted remote-http manifest failed ParseManifest+Validate: %v\n---\n%s", err, got)
	}
	if m.Transport != config.TransportRemoteHTTP {
		t.Fatalf("drafted manifest transport = %q, want remote-http", m.Transport)
	}
	if m.Availability != config.AvailabilityWatch || m.InstallProbe == nil || len(m.InstallProbe.Files) != 1 {
		t.Fatalf("drafted remote-http manifest lost the D-3 gate: av=%q probe=%#v", m.Availability, m.InstallProbe)
	}
	if m.VendoredSource != nil {
		t.Fatalf("drafted remote-http manifest carried vendored_source: %#v", m.VendoredSource)
	}
}

// TestGenerateDraft_RemoteHTTPNoAvailabilityByteIdentical proves an http entry
// WITHOUT the D-3 fields drafts a remote-http manifest carrying none of them —
// the additive guarantee for the existing remote-http draft path.
func TestGenerateDraft_RemoteHTTPNoAvailabilityByteIdentical(t *testing.T) {
	e := &MarketplaceEntry{
		ID:        "remote-plain",
		Name:      "Remote Plain",
		Transport: "http",
		URL:       "https://mcp.example.com/mcp",
	}
	got, _, err := GenerateDraftManifest(e, GenerateOpts{})
	if err != nil {
		t.Fatalf("GenerateDraftManifest: %v", err)
	}
	for _, key := range []string{"availability", "install_probe", "vendored_source"} {
		if strings.Contains(got, key) {
			t.Fatalf("plain http draft contains key %q\n---\n%s", key, got)
		}
	}
}

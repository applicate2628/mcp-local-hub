package config

import (
	"os"
	"testing"
)

// TestVcpkgManifestParses guards the shape of servers/vcpkg/manifest.yaml —
// the vcpkg/CMake diagnostic MCP server, moved in-hub per decision
// work-items/decisions/2026-07-26-vcpkg-mcp-must-follow-the-in-hub-server-pattern.md.
// Mirrors TestSerenaManifestParses's structure for the other single-daemon
// global servers (godbolt, perftools, ...).
func TestVcpkgManifestParses(t *testing.T) {
	f, err := os.Open("../../servers/vcpkg/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := ParseManifest(f)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Name != "vcpkg" {
		t.Errorf("Name = %q, want vcpkg", m.Name)
	}
	if m.Kind != KindGlobal {
		t.Errorf("Kind = %q, want %q", m.Kind, KindGlobal)
	}
	if m.Transport != TransportStdioBridge {
		t.Errorf("Transport = %q, want %q", m.Transport, TransportStdioBridge)
	}
	// command: mcphub + base_args: [vcpkg] is the load-bearing portability
	// property: no machine-specific absolute path anywhere in the manifest,
	// unlike the retired standalone cmd/vcpkg-mcp executable's hand-written
	// client entries.
	if m.Command != "mcphub" {
		t.Errorf("Command = %q, want mcphub", m.Command)
	}
	if len(m.BaseArgs) != 1 || m.BaseArgs[0] != "vcpkg" {
		t.Errorf("BaseArgs = %v, want [vcpkg]", m.BaseArgs)
	}
	if len(m.Daemons) != 1 {
		t.Fatalf("len(Daemons) = %d, want 1", len(m.Daemons))
	}
	if m.Daemons[0].Name != "default" {
		t.Errorf("Daemons[0].Name = %q, want default", m.Daemons[0].Name)
	}
	if len(m.ClientBindings) != 7 {
		t.Errorf("len(ClientBindings) = %d, want 7 managed clients", len(m.ClientBindings))
	}
	wantClients := map[string]bool{
		"claude-code": false,
		"codex-cli":   false,
		"cursor":      false,
		"vscode":      false,
		"gemini-cli":  false,
		"qwen-cli":    false,
		"antigravity": false,
	}
	for _, b := range m.ClientBindings {
		if _, ok := wantClients[b.Client]; ok {
			if b.Daemon != "default" {
				t.Errorf("binding %s.daemon = %q, want default", b.Client, b.Daemon)
			}
			wantClients[b.Client] = true
		}
	}
	for client, seen := range wantClients {
		if !seen {
			t.Errorf("binding for client %q not found", client)
		}
	}
}

// TestVcpkgManifestPortDoesNotCollide walks every OTHER shipped
// servers/*/manifest.yaml and asserts none of their global daemons declare
// the same port as servers/vcpkg/manifest.yaml. This is a direct,
// registry-independent proof of the Gates requirement ("a test asserting
// the manifest parses and its port does not collide with another shipped
// manifest") — independent of (and in addition to) the pre-existing
// TestPortsRegistryValid / TestPortsRegistryCoversAllShippedManifests, which
// enforce collision-freedom transitively via configs/ports.yaml.
func TestVcpkgManifestPortDoesNotCollide(t *testing.T) {
	vf, err := os.Open("../../servers/vcpkg/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer vf.Close()
	vm, err := ParseManifest(vf)
	if err != nil {
		t.Fatalf("ParseManifest vcpkg: %v", err)
	}
	if len(vm.Daemons) != 1 {
		t.Fatalf("len(vcpkg Daemons) = %d, want 1", len(vm.Daemons))
	}
	vcpkgPort := vm.Daemons[0].Port

	serversDir := "../../servers"
	entries, err := os.ReadDir(serversDir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", serversDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "vcpkg" {
			continue
		}
		mPath := serversDir + "/" + e.Name() + "/manifest.yaml"
		mFile, err := os.Open(mPath)
		if err != nil {
			continue // not a server dir
		}
		m, parseErr := ParseManifest(mFile)
		mFile.Close()
		if parseErr != nil {
			continue // asserted elsewhere; not this test's concern
		}
		for _, d := range m.Daemons {
			if d.Port == vcpkgPort {
				t.Errorf("vcpkg daemon port %d collides with %s/%s", vcpkgPort, m.Name, d.Name)
			}
		}
	}
}

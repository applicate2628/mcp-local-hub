package reversedepgraph

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writePortFixture(t *testing.T, dir, name string, dependencies string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"` + name + `","version-string":"1"` + dependencies + `}`
	if err := os.WriteFile(filepath.Join(dir, "vcpkg.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "portfile.cmake"), []byte("set(VCPKG_POLICY_EMPTY_PACKAGE enabled)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUniverseOverlayBuiltinPrecedence(t *testing.T) {
	root := t.TempDir()
	overlay := t.TempDir()
	writePortFixture(t, filepath.Join(root, "ports", "target"), "target", "")
	writePortFixture(t, filepath.Join(root, "ports", "consumer"), "consumer", `,"dependencies":["wrong"]`)
	writePortFixture(t, filepath.Join(overlay, "consumer"), "consumer", `,"dependencies":["target"]`)

	outcome := EnumerateUniverse(context.Background(), Args{Port: "target", VcpkgRoot: root, Triplet: "x64-windows", HostTriplet: "x64-windows", OverlayPorts: []string{overlay}, ScratchRoot: t.TempDir()})
	if !outcome.Complete || outcome.Reason != "" {
		t.Fatalf("universe incomplete: %#v", outcome)
	}
	if got := candidateNames(outcome.Candidates); !reflect.DeepEqual(got, []string{"consumer", "target"}) {
		t.Fatalf("candidates = %#v", got)
	}
	var consumer Candidate
	for _, candidate := range outcome.Candidates {
		if candidate.Name == "consumer" {
			consumer = candidate
		}
	}
	if consumer.WinnerDirectory != filepath.Join(overlay, "consumer") || !reflect.DeepEqual(consumer.DeclaredDependencies, []string{"target"}) {
		t.Fatalf("overlay winner not consumed: %#v", consumer)
	}
}

func TestRemoteRegistryRefused(t *testing.T) {
	root := t.TempDir()
	manifest := t.TempDir()
	if err := os.WriteFile(filepath.Join(manifest, "vcpkg.json"), []byte(`{"name":"app","version-string":"1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifest, "vcpkg-configuration.json"), []byte(`{"default-registry":{"kind":"git","repository":"https://example.invalid/registry"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	outcome := EnumerateUniverse(context.Background(), Args{Port: "target", VcpkgRoot: root, Triplet: "x64-windows", HostTriplet: "x64-windows", ManifestRoot: manifest, ScratchRoot: t.TempDir()})
	if outcome.Complete || outcome.Reason != ReasonNetworkDisabledRegistry || outcome.Failure == nil || outcome.Failure.ID != FailureNetworkRegistryRefused {
		t.Fatalf("remote registry outcome = %#v", outcome)
	}
}

func TestUniverseLocalRegistryUnion(t *testing.T) {
	root := t.TempDir()
	manifest := t.TempDir()
	registry := filepath.Join(manifest, "registry")
	writeVersionIndexFixture(t, filepath.Join(root, "versions", "t-", "target.json"))
	writeVersionIndexFixture(t, filepath.Join(registry, "versions", "c-", "consumer.json"))
	if err := os.WriteFile(filepath.Join(manifest, "vcpkg.json"), []byte(`{"name":"app","version-string":"1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	configuration := `{"default-registry":{"kind":"builtin","baseline":"fixture"},"registries":[{"kind":"filesystem","path":"registry","packages":["*"]}]}`
	if err := os.WriteFile(filepath.Join(manifest, "vcpkg-configuration.json"), []byte(configuration), 0o644); err != nil {
		t.Fatal(err)
	}
	outcome := EnumerateUniverse(context.Background(), Args{Port: "target", VcpkgRoot: root, Triplet: "x64-windows", HostTriplet: "x64-windows", ManifestRoot: manifest, ScratchRoot: t.TempDir()})
	if !outcome.Complete || outcome.Reason != "" {
		t.Fatalf("local registry union failed: %#v", outcome)
	}
	if got := candidateNames(outcome.Candidates); !reflect.DeepEqual(got, []string{"consumer", "target"}) {
		t.Fatalf("registry candidates = %#v", got)
	}
}

func writeVersionIndexFixture(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"versions":[{"version-string":"1","path":"fixture"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

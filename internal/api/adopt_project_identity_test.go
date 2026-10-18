package api

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestProjectAdoptRecordMatchesCanonicalProjectIdentity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "MixedCaseProject")
	fingerprint, err := clients.CodexProjectIdentitySHA256ForRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := "demo-project-" + fingerprint[:8]
	makeRecord := func() *AdoptProvenanceRecord {
		return &AdoptProvenanceRecord{
			ManifestName: manifest, SourceClient: "codex-cli", SourceEntryName: "demo",
			Clients: []AdoptClientProvenance{{
				Client: "codex-cli",
				ConfigTarget: &AdoptClientConfigTargetV1{
					Kind: adoptClientConfigTargetKindCodexProject,
					ProjectRoot: root, ProjectIdentitySHA256: fingerprint,
				},
			}},
		}
	}
	for _, requested := range []string{root, strings.ToUpper(root), strings.ToLower(root)} {
		want := requested == root || runtime.GOOS == "windows" || runtime.GOOS == "darwin"
		if got := projectAdoptRecordMatchesIdentity(makeRecord(), requested, fingerprint, "demo", manifest); got != want {
			t.Fatalf("root %q: matched=%v want=%v", requested, got, want)
		}
	}
	t.Run("different root cannot borrow matching digest", func(t *testing.T) {
		if projectAdoptRecordMatchesIdentity(makeRecord(), root+"-other", fingerprint, "demo", manifest) {
			t.Fatal("different canonical root accepted")
		}
	})
	t.Run("mismatched fingerprint", func(t *testing.T) {
		if projectAdoptRecordMatchesIdentity(makeRecord(), root, strings.Repeat("0", 64), "demo", manifest) {
			t.Fatal("mismatched fingerprint accepted")
		}
	})
	t.Run("wrong logical entry", func(t *testing.T) {
		if projectAdoptRecordMatchesIdentity(makeRecord(), root, fingerprint, "other", manifest) {
			t.Fatal("wrong logical entry accepted")
		}
	})
	t.Run("duplicate project targets", func(t *testing.T) {
		rec := makeRecord()
		rec.Clients = append(rec.Clients, rec.Clients[0])
		if projectAdoptRecordMatchesIdentity(rec, root, fingerprint, "demo", manifest) {
			t.Fatal("ambiguous project ownership accepted")
		}
	})
}

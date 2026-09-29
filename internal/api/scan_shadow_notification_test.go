package api

import (
	"bytes"
	"crypto/sha256"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"mcp-local-hub/servers"
)

func TestDiskShadowWarningTrackerLifecycle(t *testing.T) {
	tracker := NewDiskShadowWarningTracker()
	first := DiskShadowWarningObservation{Name: "wolfram", Message: "warning", Identity: sha256.Sum256([]byte("first"))}
	changed := first
	changed.Identity = sha256.Sum256([]byte("changed"))

	if got := tracker.Complete([]DiskShadowWarningObservation{first}); len(got) != 1 {
		t.Fatalf("first appearance emitted %d warnings, want 1", len(got))
	}
	if got := tracker.Complete([]DiskShadowWarningObservation{first}); len(got) != 0 {
		t.Fatalf("unchanged repeat emitted %d warnings, want 0", len(got))
	}
	if got := tracker.Complete([]DiskShadowWarningObservation{changed}); len(got) != 1 {
		t.Fatalf("changed raw identity emitted %d warnings, want 1", len(got))
	}
	if got := tracker.Complete(nil); len(got) != 0 {
		t.Fatalf("absence emitted %d warnings, want 0", len(got))
	}
	if got := tracker.Complete([]DiskShadowWarningObservation{changed}); len(got) != 1 {
		t.Fatalf("reappearance emitted %d warnings, want 1", len(got))
	}

	// A failed enumeration does not call Complete, so it cannot erase the
	// remembered identity. The next complete identical snapshot stays quiet.
	if got := tracker.Complete([]DiskShadowWarningObservation{changed}); len(got) != 0 {
		t.Fatalf("post-error identical snapshot emitted %d warnings, want 0", len(got))
	}
}

func TestDiskShadowWarningTrackerConcurrentIdenticalSnapshotEmitsOnce(t *testing.T) {
	tracker := NewDiskShadowWarningTracker()
	observation := DiskShadowWarningObservation{Name: "wolfram", Message: "warning", Identity: sha256.Sum256([]byte("same"))}

	const callers = 16
	var wg sync.WaitGroup
	emitted := make(chan int, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			emitted <- len(tracker.Complete([]DiskShadowWarningObservation{observation}))
		}()
	}
	wg.Wait()
	close(emitted)
	count := 0
	for n := range emitted {
		count += n
	}
	if count != 1 {
		t.Fatalf("concurrent identical snapshot emissions = %d, want 1", count)
	}
}

func TestEmbeddedDiskShadowObservationUsesRawBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wolfram", "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	embed, err := fs.ReadFile(servers.Manifests, "wolfram/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(bytes.Clone(embed), []byte("\n# comment-only edit\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	first, ok := embeddedDiskShadowObservation("wolfram", dir)
	if !ok || first.Message == "" {
		t.Fatalf("first raw observation = %#v, %v; want warning", first, ok)
	}
	if err := os.WriteFile(path, append(bytes.Clone(embed), []byte("\n# another comment-only edit\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	second, ok := embeddedDiskShadowObservation("wolfram", dir)
	if !ok || first.Identity == second.Identity {
		t.Fatalf("comment-only raw edit did not change observation identity: first=%x second=%x", first.Identity, second.Identity)
	}
}

func TestScanFromShadowTrackerSuppressesUnchangedEmbedFirstWarning(t *testing.T) {
	root := t.TempDir()
	t.Cleanup(SetTestCanonicalMcphubPath(filepath.Join(root, "bin", "mcphub.exe")))
	dir := mustCanonicalManifestDir(t)
	path := filepath.Join(dir, "wolfram", "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	embed, err := fs.ReadFile(servers.Manifests, "wolfram/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(bytes.Clone(embed), []byte("\n# operator edit\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLog) })
	tracker := NewDiskShadowWarningTracker()
	for i := 0; i < 2; i++ {
		if _, err := NewAPI().ScanFrom(ScanOpts{ConfigPaths: map[string]string{}, ShadowWarningTracker: tracker}); err != nil {
			t.Fatalf("ScanFrom %d: %v", i, err)
		}
	}
	if got := strings.Count(logs.String(), `disk manifest for shipped server "wolfram" is ignored`); got != 1 {
		t.Fatalf("unchanged embed-first warnings = %d, want 1", got)
	}
}

func TestScanFromExplicitManifestDirDoesNotEmitShadowWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wolfram", "manifest.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("name: wolfram\nkind: global\ntransport: stdio-bridge\ncommand: node\ndaemons:\n  - name: default\n    port: 9431\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previousLog := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousLog) })
	if _, err := NewAPI().ScanFrom(ScanOpts{ConfigPaths: map[string]string{}, ManifestDir: dir, ShadowWarningTracker: NewDiskShadowWarningTracker()}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "is ignored") {
		t.Fatalf("explicit disk-wins scan emitted shadow warning: %s", logs.String())
	}
}

package serena_routing

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

// The fixture has explicit read-only broadening on both POSIX and Windows.
// The process policy and default audit path are isolated from the operator.
func markerAuditStateDir(t *testing.T) string {
	t.Helper()
	root := apitest.HardenedTempDir(t)
	restore := api.SetDaemonStateRootForTest(root)
	api.ResetStrictModeIntentCacheForTest()
	t.Cleanup(func() {
		restore()
		api.ResetStrictModeIntentCacheForTest()
	})
	t.Setenv(api.RequireSingleUserHomeEnv, "")
	return root
}

func TestReadDefaultWorkspaceWithAuditSinkRoutesFallbackDiagnostics(t *testing.T) {
	for _, failSink := range []bool{false, true} {
		name := "sink-success"
		if failSink {
			name = "sink-error"
		}
		t.Run(name, func(t *testing.T) {
			root := markerAuditStateDir(t)
			path := filepath.Join(root, api.DefaultWorkspaceFilename)
			contents := []byte("  /workspace\r\n")
			writeReadRelaxedMarker(t, path, contents)
			logPath := filepath.Join(root, "hub-mcp.log")
			sentinel := []byte("marker audit sentinel\n")
			if err := api.WriteStateFileBytesAtomic(logPath, sentinel); err != nil {
				t.Fatalf("seed isolated GUI log: %v", err)
			}

			var markerEvents atomic.Int32
			got, err := api.ReadDefaultWorkspaceWithAuditSink(root, func(_ string, event string, fields map[string]any) error {
				if event == "hub-mcp-state-read-unhardened-file-fallback" && fields["path"] == path {
					markerEvents.Add(1)
				}
				if failSink {
					return errors.New("test diagnostic sink failure")
				}
				return nil
			})
			if err != nil || got != "/workspace" {
				t.Fatalf("marker read = %q, %v; want /workspace, nil", got, err)
			}
			if markerEvents.Load() != 1 {
				t.Fatalf("configured sink marker events = %d, want 1", markerEvents.Load())
			}
			var nextEvents atomic.Int32
			nextSink := func(_ string, event string, fields map[string]any) error {
				if event == "hub-mcp-state-read-unhardened-file-fallback" && fields["path"] == path {
					nextEvents.Add(1)
				}
				return nil
			}
			got, err = api.ReadDefaultWorkspaceWithAuditSink(root, nextSink)
			if err != nil || got != "/workspace" || markerEvents.Load() != 1 || nextEvents.Load() != 1 {
				t.Fatalf("per-call sink isolation: read=%q err=%v events=(%d,%d)", got, err, markerEvents.Load(), nextEvents.Load())
			}

			// Audit redirection must not bypass the existing permission refusal.
			t.Setenv(api.RequireSingleUserHomeEnv, "1")
			got, err = api.ReadDefaultWorkspaceWithAuditSink(root, nextSink)
			wantErr := api.ErrTooLoose
			if runtime.GOOS == "windows" {
				wantErr = api.ErrDaclOutsideAllowlist
			}
			if got != "" || !errors.Is(err, wantErr) || nextEvents.Load() != 1 {
				t.Fatalf("strict read=%q err=%v events=%d, want %v without a fallback event", got, err, nextEvents.Load(), wantErr)
			}
			assertMarkerAuditFileUnchanged(t, path, contents)
			assertMarkerAuditFileUnchanged(t, logPath, sentinel)
		})
	}
}

func TestResolveDefaultWorkspaceRoutesMarkerFallbackToConfiguredAuditSink(t *testing.T) {
	root := markerAuditStateDir(t)
	workspace := makeWorkspace(t, root, "Only")
	regPath := makeRegistryWithSerena(t, root, []api.WorkspaceEntry{{
		WorkspaceKey: api.WorkspaceKey(workspace), WorkspacePath: workspace, Backend: "serena", Port: 9301,
	}})
	marker := filepath.Join(root, api.DefaultWorkspaceFilename)
	contents := []byte(workspace)
	writeReadRelaxedMarker(t, marker, contents)

	var markerEvents atomic.Int32
	sink := func(_ string, event string, fields map[string]any) error {
		if event == "hub-mcp-state-read-unhardened-file-fallback" && fields["path"] == marker {
			markerEvents.Add(1)
		}
		return nil
	}
	reg := api.NewRegistry(regPath)
	// The registry's own reads must not accidentally use the global log either.
	reg.SetAuditSink(sink)
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	resolver := NewReadOnlyWorkspaceResolver(reg, regPath)
	resolver.SetAuditSink(sink)
	registryBefore, err := os.ReadFile(regPath)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "hub-mcp.log")
	sentinel := []byte("resolver audit sentinel\n")
	if err := api.WriteStateFileBytesAtomic(logPath, sentinel); err != nil {
		t.Fatalf("seed isolated GUI log: %v", err)
	}

	got, err := resolver.ResolveDefaultWorkspace()
	if err != nil || got == nil || got.WorkspacePath != workspace {
		t.Fatalf("default workspace = %+v, %v; want %q", got, err, workspace)
	}
	if markerEvents.Load() != 1 {
		t.Fatalf("resolver marker events = %d, want 1", markerEvents.Load())
	}
	var nextEvents atomic.Int32
	resolver.SetAuditSink(func(_ string, event string, fields map[string]any) error {
		if event == "hub-mcp-state-read-unhardened-file-fallback" && fields["path"] == marker {
			nextEvents.Add(1)
		}
		return nil
	})
	got, err = resolver.ResolveDefaultWorkspace()
	if err != nil || got == nil || got.WorkspacePath != workspace || markerEvents.Load() != 1 || nextEvents.Load() != 1 {
		t.Fatalf("resolver sink replacement: workspace=%+v err=%v events=(%d,%d)", got, err, markerEvents.Load(), nextEvents.Load())
	}

	// A cached workspace must not make a now-forbidden marker readable.
	t.Setenv(api.RequireSingleUserHomeEnv, "1")
	got, err = resolver.ResolveDefaultWorkspace()
	if got != nil || !errors.Is(err, ErrDefaultWorkspaceUnavailable) || nextEvents.Load() != 1 {
		t.Fatalf("strict resolver read=%+v err=%v events=%d, want unavailable without fallback", got, err, nextEvents.Load())
	}
	assertMarkerAuditFileUnchanged(t, marker, contents)
	assertMarkerAuditFileUnchanged(t, regPath, registryBefore)
	assertMarkerAuditFileUnchanged(t, logPath, sentinel)
}

func assertMarkerAuditFileUnchanged(t *testing.T, path string, expected []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, expected) {
		t.Fatalf("read-only operation changed %s: err=%v, got=%q, want=%q", path, err, got, expected)
	}
}

package api

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeProductUnderSupervisorLockRequiresExactHeldToken(t *testing.T) {
	root := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	lock, err := AcquireSupervisorLockQuiet(filepath.Join(root, "supervisor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Release)
	bypass := lock.AllowSpecBearingWriteBypass()
	if path, err := runtimeMcphubExecutableUnderSupervisorLock(bypass); err != nil || path != testCanonicalMcphubPathOverride {
		t.Fatalf("exact quiet holder lost its admitted test image: path=%q err=%v", path, err)
	}
	if _, err := os.Stat(filepath.Join(root, "supervisor.lock.owner.json")); !os.IsNotExist(err) {
		t.Fatalf("quiet image resolution wrote or required an owner sidecar: %v", err)
	}
	other, err := AcquireSupervisorLockQuiet(filepath.Join(hardenedTempDir(t), "supervisor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Release)
	for name, token := range map[string]InstallParsedManifestBypass{"absent": {}, "other state root": other.AllowSpecBearingWriteBypass()} {
		t.Run(name, func(t *testing.T) {
			path, err := runtimeMcphubExecutableUnderSupervisorLock(token)
			var imageErr *RuntimeProductImageError
			if path != "" || !errors.As(err, &imageErr) || !strings.Contains(err.Error(), "exclusion") {
				t.Fatalf("invalid token admitted an image: path=%q err=%v", path, err)
			}
		})
	}
	lock.Release()
	if path, err := runtimeMcphubExecutableUnderSupervisorLock(bypass); path != "" || err == nil {
		t.Fatalf("released token admitted an image: path=%q err=%v", path, err)
	}
}

func TestRuntimeProductUnderSupervisorLockPreservesNativeAdmission(t *testing.T) {
	root := daemonIntentTestHelper(t)
	t.Cleanup(SetTestCanonicalMcphubPath(""))
	lock, err := AcquireSupervisorLockQuiet(filepath.Join(root, "supervisor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Release)
	self, err := SelfMcphubExecutable()
	if err != nil {
		t.Fatal(err)
	}
	admissionErr := admitRuntimeMcphubImage(self)
	path, err := runtimeMcphubExecutableUnderSupervisorLock(lock.AllowSpecBearingWriteBypass())
	if admissionErr == nil {
		if err != nil || path != self {
			t.Fatalf("admitted native self was lost: path=%q err=%v", path, err)
		}
		return
	}
	// A console test binary on Windows is deliberately not a Product. The
	// quiet-lock branch must retain that real PE admission refusal rather than
	// accepting the image merely because this process holds exclusion.
	var imageErr *RuntimeProductImageError
	if path != "" || !errors.As(err, &imageErr) || !strings.Contains(err.Error(), admissionErr.Error()) {
		t.Fatalf("native image admission changed: path=%q err=%v want cause=%v", path, err, admissionErr)
	}
}

func TestParsedInstallUnderQuietLockKeepsPreacquiredInvocationGate(t *testing.T) {
	root := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	lock, err := AcquireSupervisorLockQuiet(filepath.Join(root, "supervisor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Release)
	workspace := t.TempDir()
	_, err = NewAPI().InstallParsedManifest(context.Background(), serenaTemplateManifest(), InstallParsedManifestOpts{
		ManifestHash: testExactManifestHash, Writer: io.Discard,
		Workspaces:           []WorkspaceEntry{{WorkspaceKey: WorkspaceKey(workspace), WorkspacePath: workspace, Language: SerenaLanguageSentinel, Backend: SerenaServerName, Port: 9401}},
		SupervisorLockBypass: lock.AllowSpecBearingWriteBypass(),
	})
	if err == nil || !strings.Contains(err.Error(), "SERENA_PARSED_PREACQUIRED_INVOCATION_REQUIRED") {
		t.Fatalf("quiet Product selection bypassed prepared invocation admission: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, supervisorIntentFileLeaf)); !os.IsNotExist(err) {
		t.Fatalf("missing invocation mutated intent: %v", err)
	}
}

func TestParsedInstallWithoutWorkspacesUnderQuietLock(t *testing.T) {
	root := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)
	lock, err := AcquireSupervisorLockQuiet(filepath.Join(root, "supervisor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Release)
	path, err := NewAPI().InstallParsedManifest(context.Background(), serenaTemplateManifest(), InstallParsedManifestOpts{
		Writer: io.Discard, Workspaces: []WorkspaceEntry{}, SupervisorLockBypass: lock.AllowSpecBearingWriteBypass(),
	})
	if err != nil {
		t.Fatal(err)
	}
	intent, err := ReadSupervisorIntent(path)
	if err != nil || len(intent.Daemons) != 0 {
		t.Fatalf("empty workspace install under exact exclusion failed: intent=%+v err=%v", intent, err)
	}
	if _, err := os.Stat(filepath.Join(root, "supervisor.lock.owner.json")); !os.IsNotExist(err) {
		t.Fatalf("quiet install created a supervisor identity: %v", err)
	}
}

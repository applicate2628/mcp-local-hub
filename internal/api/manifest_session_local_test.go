package api

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const privateManifestFixture = "name: private-fixture\nkind: global\ntransport: stdio-bridge\ncommand: fixture\ndaemons:\n  - name: default\n    port: 9124\n    mcp_stdio_state: session-local\n"

func TestGuardedManifestLifecyclePreservesPrivateDescriptorBeforeEffects(t *testing.T) {
	root := t.TempDir()
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
	a := NewAPI()
	if err := a.ManifestCreateIn(root, "private-fixture", privateManifestFixture); !errors.Is(err, ErrGuardedManifestLifecycle) {
		t.Fatalf("ordinary create: %v", err)
	}
	path := filepath.Join(root, "private-fixture", "manifest.yaml")
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("refused create created directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(privateManifestFixture), 0600); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{
		strings.ReplaceAll(privateManifestFixture, "session-local", "shared"),
		strings.ReplaceAll(privateManifestFixture, "9124", "9125"),
		strings.ReplaceAll(privateManifestFixture, "name: default", "name: replacement"),
		strings.ReplaceAll(privateManifestFixture, "stdio-bridge", "native-http"),
	} {
		if err := a.ManifestEditIn(root, "private-fixture", candidate); !errors.Is(err, ErrGuardedManifestLifecycle) {
			t.Fatalf("ordinary edit changed private identity: %v", err)
		}
		if _, err := a.ManifestEditInWithHash(root, "private-fixture", candidate, ManifestHashContent([]byte(privateManifestFixture))); !errors.Is(err, ErrGuardedManifestLifecycle) {
			t.Fatalf("hash edit changed private identity: %v", err)
		}
	}
	if err := a.ManifestDeleteIn(root, "private-fixture"); !errors.Is(err, ErrGuardedManifestLifecycle) {
		t.Fatalf("ordinary delete: %v", err)
	}
	if err := a.ManifestDeleteInWithHash(root, "private-fixture", ManifestHashContent([]byte(privateManifestFixture))); !errors.Is(err, ErrGuardedManifestLifecycle) {
		t.Fatalf("hash delete: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, []byte(privateManifestFixture)) {
		t.Fatalf("refusal changed preimage: %v", err)
	}
	// Non-identity edits remain owned by the ordinary manifest editor; active
	// sessions separately reject a changed launch descriptor at dispatch.
	metadata := privateManifestFixture + "description: revised\n"
	if err := a.ManifestEditIn(root, "private-fixture", metadata); err != nil {
		t.Fatalf("descriptor-preserving edit: %v", err)
	}
}

func TestGuardedManifestIdentityIsDecodedBeforeEnvironmentAndPartialTypeErrors(t *testing.T) {
	missing := strings.ReplaceAll(privateManifestFixture, "command: fixture", "command: fixture\nbase_args: ['${MCPHUB_UNSET_PRIVATE_GUARD_FIXTURE}']")
	if err := checkGuardedManifestMutation([]byte(missing), nil); !errors.Is(err, ErrGuardedManifestLifecycle) {
		t.Fatalf("missing environment hid private descriptor: %v", err)
	}
	partial := strings.ReplaceAll(privateManifestFixture, "port: 9124", "port: [9124]")
	if err := checkGuardedManifestMutation([]byte(partial), []byte(privateManifestFixture)); !errors.Is(err, ErrGuardedManifestLifecycle) {
		t.Fatalf("partial identity authorized repair: %v", err)
	}
	ordinary := strings.ReplaceAll(privateManifestFixture, "session-local", "shared")
	if err := checkGuardedManifestMutation([]byte("not: [yaml"), []byte(ordinary)); err != nil {
		t.Fatalf("ordinary repair regressed: %v", err)
	}
}

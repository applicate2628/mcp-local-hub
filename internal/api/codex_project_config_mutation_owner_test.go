package api

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"mcp-local-hub/internal/clients"
)

type apiImmediateCodexProjectRunner struct{}

func (apiImmediateCodexProjectRunner) RunRead(fn func() error) error     { return fn() }
func (apiImmediateCodexProjectRunner) RunMutation(fn func() error) error { return fn() }

func codexProjectIdentityForOwnerTest(t *testing.T) clients.CodexProjectIdentity {
	t.Helper()
	root := hardenedTempDir(t)
	configPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.Mkdir(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("[mcp_servers.graphify]\ncommand = 'graphify-mcp'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var captured clients.CodexProjectIdentity
	_, identity, err := clients.NewCodexCLIForProject(root, "graphify", func(got clients.CodexProjectIdentity) (clients.CodexProjectConfigLockRunner, error) {
		captured = got
		return apiImmediateCodexProjectRunner{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured.ProjectIdentitySHA256() == "" || captured.ProjectIdentitySHA256() != identity.ProjectIdentitySHA256() {
		t.Fatalf("captured identity mismatch: captured=%q returned=%q", captured.ProjectIdentitySHA256(), identity.ProjectIdentitySHA256())
	}
	return identity
}

func useCodexProjectOwnerStateDir(t *testing.T) string {
	t.Helper()
	stateDir := hardenedTempDir(t)
	previous := daemonStateRootOverride
	daemonStateRootOverride = stateDir
	t.Cleanup(func() { daemonStateRootOverride = previous })
	return stateDir
}

func concreteCodexProjectOwner(t *testing.T, identity clients.CodexProjectIdentity) *CodexProjectConfigMutationOwner {
	t.Helper()
	runner, err := NewCodexProjectConfigMutationOwner(identity)
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := runner.(*CodexProjectConfigMutationOwner)
	if !ok {
		t.Fatalf("owner type = %T, want *CodexProjectConfigMutationOwner", runner)
	}
	return owner
}

func TestCodexProjectConfigMutationOwnerUsesFullIdentityStateLeaf(t *testing.T) {
	identity := codexProjectIdentityForOwnerTest(t)
	stateDir := useCodexProjectOwnerStateDir(t)
	owner := concreteCodexProjectOwner(t, identity)
	wantPath := filepath.Join(stateDir, "codex-project-config-"+identity.ProjectIdentitySHA256()+".lock")
	if owner.lockPath != wantPath || len(identity.ProjectIdentitySHA256()) != 64 || identity.ProjectIdentitySHA256() != strings.ToLower(identity.ProjectIdentitySHA256()) {
		t.Fatalf("owner lock path = %q, want full lowercase identity path %q", owner.lockPath, wantPath)
	}
	readCalls := 0
	if err := owner.RunRead(func() error { readCalls++; return nil }); err != nil {
		t.Fatalf("RunRead: %v", err)
	}
	mutationCalls := 0
	if err := owner.RunMutation(func() error { mutationCalls++; return nil }); err != nil {
		t.Fatalf("RunMutation: %v", err)
	}
	if readCalls != 1 || mutationCalls != 1 {
		t.Fatalf("body calls = read %d mutation %d, want 1/1", readCalls, mutationCalls)
	}
	info, err := os.Lstat(wantPath)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("state lock leaf = %v/%v, want regular file", info, err)
	}
	if _, statErr := os.Stat(filepath.Join(identity.CanonicalRoot(), ".codex", "config.toml.lock")); !os.IsNotExist(statErr) {
		t.Fatalf("adjacent project lock exists: %v", statErr)
	}
}

func TestCodexProjectConfigMutationOwnerRejectsInvalidIdentityBeforeStateMutation(t *testing.T) {
	stateDir := useCodexProjectOwnerStateDir(t)
	runner, err := NewCodexProjectConfigMutationOwner(clients.CodexProjectIdentity{})
	if runner != nil || !errors.Is(err, clients.ErrCodexProjectConfigLockUnavailable) {
		t.Fatalf("owner = %T/%v, want nil/CODEX_PROJECT_CONFIG_LOCK_UNAVAILABLE", runner, err)
	}
	entries, readErr := os.ReadDir(stateDir)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("state directory changed for invalid identity: read=%v entries=%v", readErr, entries)
	}
}

func TestCodexProjectConfigMutationOwnerAcquisitionFailureSkipsBody(t *testing.T) {
	identity := codexProjectIdentityForOwnerTest(t)
	useCodexProjectOwnerStateDir(t)
	owner := concreteCodexProjectOwner(t, identity)
	cause := errors.New("injected prior unconfirmed release")
	unconfirmedLockReleasesMu.Lock()
	unconfirmedLockReleases[owner.lockPath] = cause
	unconfirmedLockReleasesMu.Unlock()
	t.Cleanup(func() {
		unconfirmedLockReleasesMu.Lock()
		delete(unconfirmedLockReleases, owner.lockPath)
		unconfirmedLockReleasesMu.Unlock()
	})
	bodyCalls := 0
	err := owner.RunRead(func() error { bodyCalls++; return nil })
	if !errors.Is(err, clients.ErrCodexProjectConfigLockUnavailable) || !errors.Is(err, cause) || bodyCalls != 0 {
		t.Fatalf("RunRead = %v, body calls=%d; want unavailable+cause/0", err, bodyCalls)
	}
}

func TestCodexProjectConfigMutationOwnerReleaseFailurePreservesSettlement(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mutation    bool
		bodyErr     error
		wantApplied bool
	}{
		{name: "read", mutation: false},
		{name: "mutation applied", mutation: true, wantApplied: true},
		{name: "mutation body failed", mutation: true, bodyErr: errors.New("injected body failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := codexProjectIdentityForOwnerTest(t)
			useCodexProjectOwnerStateDir(t)
			owner := concreteCodexProjectOwner(t, identity)
			releaseCause := errors.New("injected project lock release failure")
			previousUnlock := flockUnlockFn
			var stranded *flock.Flock
			flockUnlockFn = func(fl *flock.Flock) error {
				if fl.Path() == owner.lockPath {
					stranded = fl
					return releaseCause
				}
				return previousUnlock(fl)
			}
			t.Cleanup(func() {
				flockUnlockFn = previousUnlock
				if stranded != nil {
					_ = stranded.Unlock()
				}
				unconfirmedLockReleasesMu.Lock()
				delete(unconfirmedLockReleases, owner.lockPath)
				unconfirmedLockReleasesMu.Unlock()
			})
			var err error
			if tc.mutation {
				err = owner.RunMutation(func() error { return tc.bodyErr })
			} else {
				err = owner.RunRead(func() error { return tc.bodyErr })
			}
			if !errors.Is(err, clients.ErrCodexProjectConfigLockReleaseUnconfirmed) || !errors.Is(err, releaseCause) {
				t.Fatalf("runner error = %v, want release sentinel and cause", err)
			}
			if tc.bodyErr != nil && !errors.Is(err, tc.bodyErr) {
				t.Fatalf("runner error = %v, want body cause %v", err, tc.bodyErr)
			}
			if got := IsAppliedLockReleaseUnconfirmed(err); got != tc.wantApplied {
				t.Fatalf("applied release classification = %v, want %v", got, tc.wantApplied)
			}
			secondBodyCalls := 0
			secondErr := owner.RunMutation(func() error { secondBodyCalls++; return nil })
			if !errors.Is(secondErr, clients.ErrCodexProjectConfigLockUnavailable) || secondBodyCalls != 0 {
				t.Fatalf("poisoned reacquire = %v, body calls=%d; want unavailable/0", secondErr, secondBodyCalls)
			}
		})
	}
}

func TestCodexProjectConfigMutationOwnerSerializesSameIdentityAndIsolatesDifferentProjects(t *testing.T) {
	firstIdentity := codexProjectIdentityForOwnerTest(t)
	secondIdentity := codexProjectIdentityForOwnerTest(t)
	useCodexProjectOwnerStateDir(t)
	firstOwner := concreteCodexProjectOwner(t, firstIdentity)
	sameOwner := concreteCodexProjectOwner(t, firstIdentity)
	differentOwner := concreteCodexProjectOwner(t, secondIdentity)
	if firstOwner.lockPath == differentOwner.lockPath {
		t.Fatalf("different project identities share lock path %q", firstOwner.lockPath)
	}

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- firstOwner.RunMutation(func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	waitProjectOwnerSignal(t, firstEntered, "first owner body")

	sameStarted := make(chan struct{})
	sameEntered := make(chan struct{})
	sameDone := make(chan error, 1)
	go func() {
		close(sameStarted)
		sameDone <- sameOwner.RunMutation(func() error { close(sameEntered); return nil })
	}()
	waitProjectOwnerSignal(t, sameStarted, "same-project caller start")

	differentEntered := make(chan struct{})
	differentDone := make(chan error, 1)
	go func() {
		differentDone <- differentOwner.RunMutation(func() error { close(differentEntered); return nil })
	}()
	waitProjectOwnerSignal(t, differentEntered, "different-project body")
	if err := waitProjectOwnerResult(t, differentDone, "different-project completion"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sameEntered:
		t.Fatal("same-project body entered while first owner still held the lock")
	default:
	}
	close(releaseFirst)
	if err := waitProjectOwnerResult(t, firstDone, "first-project completion"); err != nil {
		t.Fatal(err)
	}
	waitProjectOwnerSignal(t, sameEntered, "same-project body after release")
	if err := waitProjectOwnerResult(t, sameDone, "same-project completion"); err != nil {
		t.Fatal(err)
	}
}

func TestCodexProjectConfigReleaseFailureClassifiesAppliedClientMutation(t *testing.T) {
	root := hardenedTempDir(t)
	configPath := filepath.Join(root, ".codex", "config.toml")
	if err := os.Mkdir(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("[mcp_servers.graphify]\ncommand = 'graphify-mcp'\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	useCodexProjectOwnerStateDir(t)
	var owner *CodexProjectConfigMutationOwner
	client, _, err := clients.NewCodexCLIForProject(root, "graphify", func(identity clients.CodexProjectIdentity) (clients.CodexProjectConfigLockRunner, error) {
		runner, ownerErr := NewCodexProjectConfigMutationOwner(identity)
		if ownerErr == nil {
			owner = runner.(*CodexProjectConfigMutationOwner)
		}
		return runner, ownerErr
	})
	if err != nil {
		t.Fatal(err)
	}
	releaseCause := errors.New("injected project lock release failure")
	previousUnlock := flockUnlockFn
	var stranded *flock.Flock
	flockUnlockFn = func(fl *flock.Flock) error {
		if fl.Path() == owner.lockPath {
			stranded = fl
			return releaseCause
		}
		return previousUnlock(fl)
	}
	t.Cleanup(func() {
		flockUnlockFn = previousUnlock
		if stranded != nil {
			_ = stranded.Unlock()
		}
		unconfirmedLockReleasesMu.Lock()
		delete(unconfirmedLockReleases, owner.lockPath)
		unconfirmedLockReleasesMu.Unlock()
	})
	created, mutationErr := client.InitEmpty()
	if created || !errors.Is(mutationErr, clients.ErrCodexProjectConfigLockReleaseUnconfirmed) {
		t.Fatalf("InitEmpty = %v/%v, want false/release-unconfirmed", created, mutationErr)
	}
	if settlement := clients.ClassifyClientMutation(mutationErr); settlement != clients.ClientMutationAppliedReleaseUnconfirmed {
		t.Fatalf("client mutation settlement = %v, want applied-release-unconfirmed", settlement)
	}
	got, readErr := os.ReadFile(configPath)
	if readErr != nil || !bytes.Equal(got, original) {
		t.Fatalf("config changed: read=%v before=%q after=%q", readErr, original, got)
	}
}

func waitProjectOwnerSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
}

func waitProjectOwnerResult(t *testing.T, result <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		return fmt.Errorf("timeout waiting for %s", what)
	}
}

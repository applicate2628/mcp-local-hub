package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestClearDefaultWorkspaceIfMatchesReadsUnderStateFileFlock(t *testing.T) {
	stateDir := t.TempDir()
	oldDefault := filepath.Join(stateDir, "old")
	newDefault := filepath.Join(stateDir, "new")
	if err := WriteDefaultWorkspace(stateDir, oldDefault); err != nil {
		t.Fatalf("seed default workspace: %v", err)
	}

	path := filepath.Join(stateDir, DefaultWorkspaceFilename)
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatalf("lock default workspace marker: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- ClearDefaultWorkspaceIfMatches(stateDir, oldDefault)
	}()

	select {
	case err := <-done:
		_ = lock.Unlock()
		t.Fatalf("ClearDefaultWorkspaceIfMatches returned before the flock holder published the new default; err=%v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := WriteStateFileBytesLockHeld(path, []byte(newDefault)); err != nil {
		_ = lock.Unlock()
		t.Fatalf("publish new default under held flock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("unlock default workspace marker: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("ClearDefaultWorkspaceIfMatches after flock release: %v", err)
	}
	got, err := ReadDefaultWorkspace(stateDir)
	if err != nil {
		t.Fatalf("read default workspace: %v", err)
	}
	if got != newDefault {
		t.Fatalf("default workspace = %q, want concurrent writer value %q", got, newDefault)
	}
}

func TestClearDefaultWorkspaceForAbsentSerenaRegistration(t *testing.T) {
	tests := []struct {
		name        string
		seed        func(reg *Registry, workspaceKey, canonical string)
		wantOutcome DefaultMarkerCompensationOutcome
		wantErr     bool
		wantMarker  string
	}{
		{
			name:        "clears absent registration",
			wantOutcome: DefaultMarkerCompensationCleared,
			wantMarker:  "",
		},
		{
			name: "preserves exact current registration",
			seed: func(reg *Registry, workspaceKey, canonical string) {
				reg.Workspaces = append(reg.Workspaces, WorkspaceEntry{
					WorkspaceKey:  workspaceKey,
					WorkspacePath: canonical,
					Language:      SerenaLanguageSentinel,
				})
			},
			wantOutcome: DefaultMarkerCompensationPreservedRegistrationPresent,
			wantMarker:  "canonical",
		},
		{
			name: "preserves same canonical registration under a different key",
			seed: func(reg *Registry, _ string, canonical string) {
				reg.Workspaces = append(reg.Workspaces, WorkspaceEntry{
					WorkspaceKey:  "legacy-key",
					WorkspacePath: canonical,
					Language:      SerenaLanguageSentinel,
				})
			},
			wantOutcome: DefaultMarkerCompensationPreservedRegistrationPresent,
			wantMarker:  "canonical",
		},
		{
			name: "fails closed on exact-key identity contradiction",
			seed: func(reg *Registry, workspaceKey, _ string) {
				reg.Workspaces = append(reg.Workspaces, WorkspaceEntry{
					WorkspaceKey:  workspaceKey,
					WorkspacePath: filepath.Join(t.TempDir(), "different-workspace"),
					Language:      SerenaLanguageSentinel,
				})
			},
			wantErr:    true,
			wantMarker: "canonical",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateDir := t.TempDir()
			canonical := filepath.Join(stateDir, "workspace")
			if err := os.MkdirAll(canonical, 0o700); err != nil {
				t.Fatalf("mkdir workspace: %v", err)
			}
			workspaceKey := WorkspaceKey(canonical)
			regPath := filepath.Join(stateDir, "workspaces.yaml")
			if err := WriteDefaultWorkspace(stateDir, canonical); err != nil {
				t.Fatalf("seed default marker: %v", err)
			}

			if tt.seed != nil {
				reg := NewRegistry(regPath)
				unlock, err := reg.Lock()
				if err != nil {
					t.Fatalf("lock registry: %v", err)
				}
				if err := reg.Load(); err != nil {
					assertRegistryReleased(t, unlock)
					t.Fatalf("load registry: %v", err)
				}
				tt.seed(reg, workspaceKey, canonical)
				if err := reg.Save(); err != nil {
					assertRegistryReleased(t, unlock)
					t.Fatalf("save registry: %v", err)
				}
				assertRegistryReleased(t, unlock)
			}

			gotOutcome, err := ClearDefaultWorkspaceForAbsentSerenaRegistration(regPath, workspaceKey, canonical)
			if tt.wantErr {
				if err == nil {
					t.Fatal("compensation unexpectedly succeeded despite a registry identity contradiction")
				}
			} else {
				if err != nil {
					t.Fatalf("compensation: %v", err)
				}
				if gotOutcome != tt.wantOutcome {
					t.Errorf("outcome = %q, want %q", gotOutcome, tt.wantOutcome)
				}
			}

			gotMarker, err := ReadDefaultWorkspace(stateDir)
			if err != nil {
				t.Fatalf("read default marker: %v", err)
			}
			wantMarker := tt.wantMarker
			if wantMarker == "canonical" {
				wantMarker = canonical
			}
			if gotMarker != wantMarker {
				t.Errorf("default marker = %q, want %q", gotMarker, wantMarker)
			}
		})
	}
}

func TestDefaultMarkerCompensation_HoldsRegistryThroughMarkerCAS(t *testing.T) {
	stateDir := t.TempDir()
	canonical := filepath.Join(stateDir, "workspace")
	if err := os.MkdirAll(canonical, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	workspaceKey := WorkspaceKey(canonical)
	regPath := filepath.Join(stateDir, "workspaces.yaml")
	if err := WriteDefaultWorkspace(stateDir, canonical); err != nil {
		t.Fatalf("seed default marker: %v", err)
	}

	reachedMarkerCAS := make(chan struct{})
	releaseMarkerCAS := make(chan struct{})
	orig := defaultMarkerCompensationMarkerLockHeldFn
	defaultMarkerCompensationMarkerLockHeldFn = func() {
		close(reachedMarkerCAS)
		<-releaseMarkerCAS
	}
	t.Cleanup(func() { defaultMarkerCompensationMarkerLockHeldFn = orig })

	type result struct {
		outcome DefaultMarkerCompensationOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := ClearDefaultWorkspaceForAbsentSerenaRegistration(regPath, workspaceKey, canonical)
		done <- result{outcome: outcome, err: err}
	}()

	select {
	case <-reachedMarkerCAS:
	case <-time.After(2 * time.Second):
		t.Fatal("compensation did not reach the marker CAS")
	}

	markerLock := flock.New(filepath.Join(stateDir, DefaultWorkspaceFilename) + ".lock")
	markerLocked, err := markerLock.TryLock()
	if err != nil {
		t.Fatalf("probe marker lock: %v", err)
	}
	if markerLocked {
		_ = markerLock.Unlock()
		t.Fatal("marker lock was not held while compensation paused at its CAS")
	}

	registryProbe := NewRegistry(regPath)
	registryUnlock, registryLocked, err := registryProbe.TryLock()
	if err != nil {
		t.Fatalf("probe registry lock: %v", err)
	}
	if registryLocked {
		assertRegistryReleased(t, registryUnlock)
		t.Fatal("registry lock was released before the marker CAS completed")
	}

	close(releaseMarkerCAS)
	got := <-done
	if got.err != nil {
		t.Fatalf("compensation: %v", got.err)
	}
	if got.outcome != DefaultMarkerCompensationCleared {
		t.Fatalf("outcome = %q, want %q", got.outcome, DefaultMarkerCompensationCleared)
	}
}

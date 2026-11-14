//go:build windows

package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSerenaOwnedTreeCleanupDeadline(t *testing.T) {
	bound := time.Now().Add(time.Minute)
	for _, tc := range []struct {
		name      string
		deadline  time.Time
		bound     time.Time
		cancel    bool
		wantError bool
	}{
		{"same captured bound", bound, bound, false, false},
		{"shorter receiving context", bound.Add(-time.Second), bound, false, false},
		{"renewed receiving context", bound.Add(time.Nanosecond), bound, false, true},
		{"missing captured bound", bound, time.Time{}, false, true},
		{"canceled receiving context", bound, bound, true, true},
		{"expired receiving context", time.Now().Add(-time.Second), bound, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithDeadline(context.Background(), tc.deadline)
			defer cancel()
			if tc.cancel {
				cancel()
			}
			if err := validateSerenaTreeCleanupContext(ctx, tc.bound); (err != nil) != tc.wantError {
				t.Fatalf("cleanup bound validation = %v, want error %t", err, tc.wantError)
			}
		})
	}
	for name, ctx := range map[string]context.Context{"nil": nil, "unbounded": context.Background()} {
		t.Run(name, func(t *testing.T) {
			if err := validateSerenaTreeCleanupContext(ctx, bound); err == nil {
				t.Fatal("accepted cleanup without a finite receiving context")
			}
		})
	}
}

type serenaCleanupParentBindingUnit struct {
	original, current *ConfigParentPolicyObservation
	retained          SerenaTreeObject
}

// These are data-only inputs to the binding predicate. They never pass native
// observation Validate, acquire handles, or authorize filesystem operations.
func serenaCleanupParentBindingUnitFixture() serenaCleanupParentBindingUnit {
	identity := ConfigParentDirectoryIdentityV1{VolumeSerialNumber: 0x1122334455667788, FileID: [16]byte{0: 1, 8: 2, 15: 3}}
	stateDir, parent := `D:\unit-state`, `D:\unit-state\runtime\package\attempts`
	makeParent := func(create bool) *ConfigParentPolicyObservation {
		id := identity
		resolution := &StateDirResolutionObservation{
			issuer: &stateFileReadIssuer{marker: 1}, path: stateDir, principal: "unit-principal",
			view: StateDirResolutionSnapshotV1{Schema: "state-dir-resolution-observation-v1", Version: 1,
				PathSHA256:   stateFileReadObservationDigest([]byte(stateDir)),
				SourceSHA256: stateFileReadObservationDigest([]byte("unit-source")), ReleaseKind: "CoTaskMemFree-void"},
		}
		return &ConfigParentPolicyObservation{
			issuer: &stateFileReadIssuer{marker: 1}, path: parent, createMissing: create,
			finalized: true, finishedInBound: true,
			view: ConfigParentPolicySnapshotV1{CreateMissing: create, Exists: true, FinalIdentity: &id,
				ResourcesComplete: true, Complete: true, GateDisposition: "strict-gate"},
			policy: &StateReadPolicyObservation{issuer: &stateFileReadIssuer{marker: 1}, stateDir: stateDir, resolution: resolution,
				view: StateReadPolicySnapshotV1{EnvIdentitySHA256: stateFileReadObservationDigest([]byte("unit-env")),
					EffectiveStrict: true, BootstrapReadComplete: true, ResourcesComplete: true}},
		}
	}
	return serenaCleanupParentBindingUnit{
		original: makeParent(true), current: makeParent(false),
		retained: SerenaTreeObject{Directory: true, Device: fmt.Sprintf("%016x", identity.VolumeSerialNumber), FileID: fmt.Sprintf("%x", identity.FileID)},
	}
}

func TestSerenaOwnedTreeCleanupParentBinding(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func(*serenaCleanupParentBindingUnit)
		wantError string
	}{
		{"same native namespace with per-call issuers", func(*serenaCleanupParentBindingUnit) {}, ""},
		{"expired original work observation stays immutable", func(f *serenaCleanupParentBindingUnit) {
			f.original.deadline = time.Now().Add(-time.Minute)
			f.original.deadlineDefined = true
			f.original.policy.deadline = f.original.deadline
			f.original.policy.deadlineDefined = true
		}, ""},
		{"intent changes without weaker protection", func(f *serenaCleanupParentBindingUnit) {
			changed := "different-intent-content"
			f.current.policy.view.IntentContentSHA256 = &changed
			f.current.policy.view.IntentState = "parsed"
		}, ""},
		{"relaxed ancestor but strict retained parent", func(f *serenaCleanupParentBindingUnit) {
			f.original.view.GateDisposition = "default-relax"
		}, ""},
		{"strictness increases", func(f *serenaCleanupParentBindingUnit) {
			f.original.policy.view.EffectiveStrict = false
		}, ""},
		{"original close incomplete", func(f *serenaCleanupParentBindingUnit) {
			f.original.view.ResourcesComplete = false
		}, "EVIDENCE_INCOMPLETE"},
		{"fresh close incomplete", func(f *serenaCleanupParentBindingUnit) {
			f.current.cleanupErr = errors.New("unit close failure")
		}, "EVIDENCE_INCOMPLETE"},
		{"fresh resources incomplete", func(f *serenaCleanupParentBindingUnit) {
			f.current.view.ResourcesComplete = false
		}, "EVIDENCE_INCOMPLETE"},
		{"parent path drift", func(f *serenaCleanupParentBindingUnit) {
			f.current.path += "-changed"
		}, "PARENT_BINDING_DIFFERS"},
		{"fresh parent missing", func(f *serenaCleanupParentBindingUnit) {
			f.current.view.Exists = false
		}, "PARENT_BINDING_DIFFERS"},
		{"cleanup would create parent", func(f *serenaCleanupParentBindingUnit) {
			f.current.createMissing = true
		}, "PARENT_BINDING_DIFFERS"},
		{"cleanup created component", func(f *serenaCleanupParentBindingUnit) {
			f.current.view.CreatedComponents = []string{"created"}
		}, "PARENT_BINDING_DIFFERS"},
		{"retained object is not directory", func(f *serenaCleanupParentBindingUnit) {
			f.retained.Directory = false
		}, "PARENT_BINDING_DIFFERS"},
		{"fresh full file identity high half differs", func(f *serenaCleanupParentBindingUnit) {
			f.current.view.FinalIdentity.FileID[15] ^= 1
		}, "PARENT_BINDING_DIFFERS"},
		{"original full file identity differs", func(f *serenaCleanupParentBindingUnit) {
			f.original.view.FinalIdentity.FileID[8] ^= 1
		}, "PARENT_BINDING_DIFFERS"},
		{"volume high bits differ", func(f *serenaCleanupParentBindingUnit) {
			f.current.view.FinalIdentity.VolumeSerialNumber ^= uint64(1) << 63
		}, "PARENT_BINDING_DIFFERS"},
		{"state root differs", func(f *serenaCleanupParentBindingUnit) {
			f.current.policy.stateDir += "-changed"
		}, "POLICY_BINDING_DIFFERS"},
		{"environment differs", func(f *serenaCleanupParentBindingUnit) {
			f.current.policy.view.EnvIdentitySHA256 += "-changed"
		}, "POLICY_BINDING_DIFFERS"},
		{"strictness decreases", func(f *serenaCleanupParentBindingUnit) {
			f.current.policy.view.EffectiveStrict = false
		}, "POLICY_BINDING_DIFFERS"},
		{"fresh parent needs relaxed gate", func(f *serenaCleanupParentBindingUnit) {
			f.current.view.GateDisposition = "default-relax"
		}, "PARENT_PROTECTION_WEAKENED"},
		{"both ancestor and fresh parent relaxed", func(f *serenaCleanupParentBindingUnit) {
			f.original.view.GateDisposition = "default-relax"
			f.current.view.GateDisposition = "default-relax"
		}, "PARENT_PROTECTION_WEAKENED"},
		{"resolution schema differs", func(f *serenaCleanupParentBindingUnit) {
			f.current.policy.resolution.view.Schema += "-changed"
		}, "NAMESPACE_BINDING_DIFFERS"},
		{"resolution version differs", func(f *serenaCleanupParentBindingUnit) {
			f.current.policy.resolution.view.Version++
		}, "NAMESPACE_BINDING_DIFFERS"},
		{"resolution release semantics differ", func(f *serenaCleanupParentBindingUnit) {
			f.current.policy.resolution.view.ReleaseKind = "borrowed-retained-directory"
		}, "NAMESPACE_BINDING_DIFFERS"},
		{"resolution path differs", func(f *serenaCleanupParentBindingUnit) {
			f.current.policy.resolution.path += "-changed"
		}, "NAMESPACE_BINDING_DIFFERS"},
		{"resolution source differs", func(f *serenaCleanupParentBindingUnit) {
			f.current.policy.resolution.view.SourceSHA256 += "-changed"
		}, "NAMESPACE_BINDING_DIFFERS"},
		{"resolution principal differs", func(f *serenaCleanupParentBindingUnit) {
			f.current.policy.resolution.principal += "-changed"
		}, "NAMESPACE_BINDING_DIFFERS"},
		{"unknown release semantics fail closed", func(f *serenaCleanupParentBindingUnit) {
			f.original.policy.resolution.view.ReleaseKind = "unknown"
			f.current.policy.resolution.view.ReleaseKind = "unknown"
		}, "NAMESPACE_OWNER_DIFFERS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := serenaCleanupParentBindingUnitFixture()
			tc.change(&f)
			originalDeadline := f.original.deadline
			err := validateSerenaCleanupParentBinding(f.original, f.current, f.retained)
			if tc.wantError == "" && err != nil || tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("binding = %v, want %q", err, tc.wantError)
			}
			if f.original.deadline != originalDeadline {
				t.Fatal("original work deadline changed")
			}
		})
	}
}

func TestSerenaOwnedTreeCleanupBorrowedNamespaceKeepsOriginalIssuer(t *testing.T) {
	f := serenaCleanupParentBindingUnitFixture()
	oldRoot, newRoot := f.original.policy.resolution, f.current.policy.resolution
	for _, root := range []*StateDirResolutionObservation{oldRoot, newRoot} {
		root.view.Schema = "state-dir-serena-qualification-v1"
		root.view.ReleaseKind = "borrowed-retained-directory"
	}
	newRoot.issuer = oldRoot.issuer
	if err := validateSerenaCleanupParentBinding(f.original, f.current, f.retained); err != nil {
		t.Fatalf("same issuer binding: %v", err)
	}
	// Reopening the identical directory may reproduce every serialized fact,
	// but cannot reproduce the original scope's private nonzero issuer.
	newRoot.issuer = &stateFileReadIssuer{marker: 1}
	if err := validateSerenaCleanupParentBinding(f.original, f.current, f.retained); err == nil || !strings.Contains(err.Error(), "NAMESPACE_OWNER_DIFFERS") {
		t.Fatalf("replacement issuer accepted: %v", err)
	}
}

func TestSerenaOwnedTreeCleanupRefusesBeforeParentObservation(t *testing.T) {
	bound := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), bound)
	cancel()
	owner := &windowsSerenaOwnedTree{request: SerenaOwnedTreeRequest{AbsoluteCleanupDeadline: bound}}
	if _, err := owner.observeCleanupParent(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup must fail before parent observation: %v", err)
	}
	if owner.parentPolicy != nil || owner.root != nil || owner.parent != nil {
		t.Fatal("refusal acquired or replaced tree evidence")
	}
	if owner.cleanupParentErr != nil {
		t.Fatal("request rejected before acquisition created a cleanup obligation")
	}
}

func TestSerenaOwnedTreeCleanupRejectsPublishedReader(t *testing.T) {
	bound := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), bound)
	defer cancel()
	owner := &windowsSerenaOwnedTree{readExisting: true, request: SerenaOwnedTreeRequest{AbsoluteCleanupDeadline: bound}}
	if _, err := inventorySerenaOwnedTreeForCleanup(ctx, &SerenaOwnedTree{platform: owner}); err == nil || !strings.Contains(err.Error(), "NO_CLEANUP_AUTHORITY") {
		t.Fatalf("published read received cleanup lifetime: %v", err)
	}
	receipt, err := owner.remove(ctx, PreparedSerenaExecutionClosure{})
	if err == nil || !strings.Contains(err.Error(), "NO_DELETE_AUTHORITY") || receipt.DeleteAttempts != 0 || owner.removeAttempted {
		t.Fatalf("published read received delete authority: %+v, %v", receipt, err)
	}
}

func TestSerenaOwnedTreeCleanupClosedOwnerDoesNotReopen(t *testing.T) {
	bound := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), bound)
	defer cancel()
	owner := &windowsSerenaOwnedTree{
		request:     SerenaOwnedTreeRequest{AbsoluteCleanupDeadline: bound},
		closeResult: SerenaTreeClose{Attempted: true, Closed: true},
	}
	closed := owner.closeResult
	if _, err := owner.observeCleanupParent(ctx); err == nil || !strings.Contains(err.Error(), "ROOT_UNAVAILABLE") {
		t.Fatalf("closed owner reached parent observation admission: %v", err)
	}
	if owner.cleanupParentErr != nil || owner.closeResult != closed {
		t.Fatal("closed owner's terminal evidence changed")
	}
}

func TestSerenaOwnedTreeCleanupStickyCloseFailurePreventsNewObservation(t *testing.T) {
	bound := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), bound)
	defer cancel()
	for _, slot := range []string{"parent", "inventory"} {
		t.Run(slot, func(t *testing.T) {
			closeErr := errors.New("unit retained close failure")
			owner := &windowsSerenaOwnedTree{request: SerenaOwnedTreeRequest{AbsoluteCleanupDeadline: bound}}
			if slot == "parent" {
				owner.cleanupParentErr = closeErr
			} else {
				owner.inventoryCloseError = closeErr
			}
			if _, err := owner.observeCleanupParent(ctx); !errors.Is(err, closeErr) {
				t.Fatalf("earlier close failure did not block new observation: %v", err)
			}
			closed, err := owner.close()
			if !errors.Is(err, closeErr) || closed.Closed || !closed.Attempted {
				t.Fatalf("observed close error lost from owner close: %+v, %v", closed, err)
			}
			again, err := owner.close()
			if again != closed || err != closed.Error {
				t.Fatalf("repeated close changed its recorded result: %+v, %v", again, err)
			}
		})
	}
}

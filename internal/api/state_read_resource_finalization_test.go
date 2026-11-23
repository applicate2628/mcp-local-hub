package api

import (
	"context"
	"errors"
	"testing"
)

func TestCanceledStateReadKeepsOriginalPolicyClosureSeparate(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	bootstrap := newStateFileReadBuilder(ctx, "", 1)
	_, read, _ := bootstrap.finish(nil, context.Canceled)
	resolution := &StateDirResolutionObservation{issuer: &stateFileReadIssuer{}, finalized: true,
		view: StateDirResolutionSnapshotV1{AllocationAcquired: true, FreeCalled: true, FreeReturned: true}}
	policy := &StateReadPolicyObservation{issuer: &stateFileReadIssuer{}, finalized: true, resolution: resolution, bootstrap: read,
		view: StateReadPolicySnapshotV1{ResourcesComplete: true, BootstrapReadComplete: true, EnvIdentitySHA256: StateReadPolicyEnvironmentIdentity()}}
	if err := policy.Validate(ctx, policy.view.EnvIdentitySHA256); !errors.Is(err, context.Canceled) || !policy.ResourcesFinalized() {
		t.Fatalf("canceled policy authorization lost original physical closure: %v", err)
	}
	b := newStateFileReadBuilder(ctx, "", 1)
	b.policy = policy
	b.borrowed = append(b.borrowed, read)
	_, final, err := b.finish(nil, context.Canceled)
	if !errors.Is(err, context.Canceled) || final.Snapshot().Complete || final.Snapshot().BorrowedResourcesComplete ||
		!final.ResourcesFinalized() || final.policy != policy || final.borrowed[0] != read {
		t.Fatalf("read result conflated authorized policy and physical closure: %v", err)
	}
	unreturned := *resolution
	unreturned.view.FreeReturned = false
	uncertainPolicy := *policy
	uncertainPolicy.resolution = &unreturned
	b = newStateFileReadBuilder(ctx, "", 1)
	b.policy = &uncertainPolicy
	_, uncertain, _ := b.finish(nil, context.Canceled)
	if uncertain.ResourcesFinalized() {
		t.Fatal("an unreturned original native free was credited as closure")
	}
}

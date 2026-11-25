//go:build !windows || !serena_qualification || !test_state_path_env

package api

import (
	"context"
	"testing"
	"time"

	"mcp-local-hub/internal/binaryadmission"
)

type unavailableQualificationEvidence struct{ called bool }

func (e *unavailableQualificationEvidence) validateStateDirResolution(context.Context, *StateDirResolutionObservation, string) error {
	e.called = true
	return nil
}

func TestSerenaQualificationNamespaceUnavailableInOrdinaryBuild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, kind := range []string{"schema", "snapshot", "issuer"} {
		t.Run(kind, func(t *testing.T) {
			zero := uint32(0)
			probe := &unavailableQualificationEvidence{}
			o := &StateDirResolutionObservation{
				issuer: &stateFileReadIssuer{marker: 1}, path: "private-root",
				finalized: true, finishedInBound: true,
				principalCheck: func() error { return nil },
				view: StateDirResolutionSnapshotV1{
					PathSHA256: stateFileReadObservationDigest([]byte("private-root")), SourceSHA256: "source",
					Complete: true, HRESULTDefined: true, HRESULT: &zero,
					AllocationAcquired: true, DecodeSucceeded: true, FreeCalled: true, FreeReturned: true,
					ReleaseKind: "CoTaskMemFree-void",
				},
			}
			switch kind {
			case "schema":
				o.view.Schema = serenaQualificationResolutionSchema
			case "snapshot":
				o.view.Qualification = &StateDirQualificationSnapshotV1{}
			case "issuer":
				o.qualification = probe
			}
			if err := o.Validate(ctx, "private-root"); err == nil {
				t.Fatal("qualification data granted ordinary resolver authority")
			}
			if probe.called {
				t.Fatal("ordinary build delegated authority to qualification evidence")
			}
		})
	}
}

func TestSerenaPublisherRolePolicyProduction(t *testing.T) {
	if !serenaPublisherRoleAllowed(binaryadmission.WindowsArtifactRoleProduct) {
		t.Fatal("ordinary Product publisher was refused")
	}
	for _, role := range []binaryadmission.WindowsArtifactRole{
		"", binaryadmission.WindowsArtifactRoleCLI,
		binaryadmission.WindowsArtifactRoleWindowless,
		binaryadmission.WindowsArtifactRoleSerenaQualification,
	} {
		if serenaPublisherRoleAllowed(role) {
			t.Fatalf("role %q acquired ordinary publisher authority", role)
		}
	}
}

package api

import (
	"context"
	"errors"
	"runtime/debug"
	"testing"

	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

const publisherTestEntrypoint = "mcp-local-hub/internal/cli.test"

// Ordinary builds retain Product-only public publication. The existing opt-in
// qualification build keeps its distinct legacy role; publisher-test never
// enters this public issuer. Read-only
// CurrentPublisherImageObservation remains a separate type that cannot be
// supplied to BindPublisherIncarnation or replace the private owner's capture.
func qualifyPublisherImage(ctx context.Context, path string, native process.PublisherNativeIncarnationV1, epoch string) (*publisherImageQualification, error) {
	qualified, err := qualifyPublisherImageFile(ctx, path, native, epoch)
	if err != nil {
		return nil, err
	}
	if !serenaPublisherRoleAllowed(qualified.role) {
		return nil, errors.New("SERENA_PUBLISHER_IMAGE_SOURCE_DRIFT")
	}
	return qualified, nil
}

func qualifyCurrentPublisherImage(ctx context.Context, path string, native process.PublisherNativeIncarnationV1) (*publisherImageQualification, error) {
	qualified, err := qualifyPublisherImageFile(ctx, path, native, "")
	if err != nil {
		return nil, err
	}
	switch qualified.role {
	case binaryadmission.WindowsArtifactRoleProduct, binaryadmission.WindowsArtifactRoleSerenaQualification:
		if !serenaPublisherRoleAllowed(qualified.role) {
			return nil, errors.New("SERENA_PUBLISHER_IMAGE_SOURCE_DRIFT")
		}
		return qualified, nil
	case binaryadmission.WindowsArtifactRolePublisherTest:
		if err := requireCurrentPublisherTestEntrypoint(qualified.goEntrypoint); err != nil {
			return nil, err
		}
		return qualified, nil
	default:
		return nil, errors.New("SERENA_PUBLISHER_IMAGE_SOURCE_DRIFT")
	}
}

// Both paths are actual Go build information: the captured original image and
// this running process. A supplied string alone cannot select the private path.
func requireCurrentPublisherTestEntrypoint(captured string) error {
	current, ok := debug.ReadBuildInfo()
	if !testing.Testing() || !ok || captured != publisherTestEntrypoint || current.Path != publisherTestEntrypoint {
		return errors.New("SERENA_PUBLISHER_IMAGE_ENTRYPOINT_UNQUALIFIED")
	}
	return nil
}

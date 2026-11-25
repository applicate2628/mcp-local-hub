package api

import (
	"strings"
	"testing"
)

func TestPrivatePublisherRequiresActualCLIEntrypoint(t *testing.T) {
	// This suite is api.test. Even the expected string cannot turn this process
	// into the fixed CLI test image or produce a native/image observation.
	for _, captured := range []string{"", "mcp-local-hub/cmd/mcphub", "mcp-local-hub/internal/api.test", publisherTestEntrypoint} {
		if err := requireCurrentPublisherTestEntrypoint(captured); err == nil || !strings.Contains(err.Error(), "SERENA_PUBLISHER_IMAGE_ENTRYPOINT_UNQUALIFIED") {
			t.Fatalf("captured entrypoint %q accepted in api.test: %v", captured, err)
		}
	}
}

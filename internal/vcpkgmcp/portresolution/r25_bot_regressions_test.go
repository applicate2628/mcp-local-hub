package portresolution

import (
	"context"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR25CancellationHasDedicatedReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := ResolvePortContext(ctx, Args{Port: "zlib", OverlayPorts: []string{filepath.Join(t.TempDir(), "overlay")}}, DefaultDeps())
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonRequestCanceled {
		t.Fatalf("result=%+v, want unknown/%s", result, ReasonRequestCanceled)
	}
}

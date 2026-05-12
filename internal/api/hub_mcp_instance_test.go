// hub_mcp_instance_test.go — Phase 2 Task 2.3 (G4 unified hub MCP).
//
// Tests for the persistent hub-instance-id + endpoint-state machinery.
// Three core invariants:
//
//  1. EnsureHubEndpoint generates instance_id once on first call and
//     preserves it across simulated restarts. The "install once,
//     restart any number of times without re-installing" workflow
//     depends on this (spec §"Hub instance ID — persistent across
//     restarts", codex r3 F-S1 closure).
//  2. RotateHubInstanceID is the only path that may change instance_id;
//     after rotate, a subsequent EnsureHubEndpoint reads back the
//     rotated value. This is the operator-driven credential burn-down
//     after a suspected pre-bind compromise.
//  3. ResetHubPort clears the persisted Port (so the next ephemeral
//     bind picks a fresh OS allocation) WITHOUT touching instance_id.
//     Used by `mcphub gui --reset-port` after a same-uid pre-bind.
//
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 2.3.

package api

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureHubEndpointGeneratesInstanceIDOnce pins the first-call
// behavior: instance_id is 64 lower-hex (32 random bytes hex-encoded)
// and the on-disk file matches the returned struct.
func TestEnsureHubEndpointGeneratesInstanceIDOnce(t *testing.T) {
	dir := hubMcpStateTestHelper(t)

	ep, err := EnsureHubEndpoint(9120, 1234)
	if err != nil {
		t.Fatalf("EnsureHubEndpoint: %v", err)
	}
	if got := len(ep.InstanceID); got != 64 {
		t.Errorf("instance_id len = %d, want 64", got)
	}
	if strings.ToLower(ep.InstanceID) != ep.InstanceID {
		t.Errorf("instance_id must be lower-hex; got %q", ep.InstanceID)
	}
	if ep.Port != 9120 {
		t.Errorf("Port = %d, want 9120", ep.Port)
	}
	if ep.PID != 1234 {
		t.Errorf("PID = %d, want 1234", ep.PID)
	}
	if ep.StartedAt == "" {
		t.Errorf("StartedAt is empty")
	}

	// Round-trip: file contents must match the returned struct.
	raw, err := readHubMcpStateFile(hubMcpEndpointFileLeaf)
	if err != nil {
		t.Fatalf("readHubMcpStateFile: %v", err)
	}
	var loaded HubEndpoint
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if loaded.InstanceID != ep.InstanceID {
		t.Errorf("on-disk instance_id %q != returned %q", loaded.InstanceID, ep.InstanceID)
	}

	// And sanity: the file was actually written at the documented path.
	if _, err := readHubMcpStateFile("hub-mcp.endpoint.json"); err != nil {
		t.Fatalf("read by literal name: %v", err)
	}
	_ = filepath.Join(dir, "hub-mcp.endpoint.json")
}

// TestEnsureHubEndpointPersistsAcrossSimulatedRestarts asserts the
// "install once, restart freely" workflow: 10 simulated restarts
// (every call with a fresh PID) must reuse the same instance_id.
func TestEnsureHubEndpointPersistsAcrossSimulatedRestarts(t *testing.T) {
	hubMcpStateTestHelper(t)

	ep1, err := EnsureHubEndpoint(0, 1234)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if len(ep1.InstanceID) != 64 {
		t.Fatalf("instance_id len = %d, want 64", len(ep1.InstanceID))
	}
	for i := 0; i < 10; i++ {
		epN, err := EnsureHubEndpoint(0, 1234+i)
		if err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
		if epN.InstanceID != ep1.InstanceID {
			t.Errorf("restart %d: instance_id rotated unexpectedly (%q -> %q)", i, ep1.InstanceID, epN.InstanceID)
		}
	}
}

// TestEnsureHubEndpointPreservesPortAcrossRestartsWhenZero pins the
// "ephemeralPort=0 means use persisted Port" branch — callers that
// haven't yet started a listener pass 0 and expect the previous
// allocation back. Listener-factory callers pass listener.Addr().Port
// (> 0) which overwrites.
func TestEnsureHubEndpointPreservesPortAcrossRestartsWhenZero(t *testing.T) {
	hubMcpStateTestHelper(t)

	ep1, err := EnsureHubEndpoint(9120, 1234)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	ep2, err := EnsureHubEndpoint(0, 9999)
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if ep2.Port != ep1.Port {
		t.Errorf("port not preserved when ephemeralPort=0: %d -> %d", ep1.Port, ep2.Port)
	}
	// Now overwrite via a non-zero port.
	ep3, err := EnsureHubEndpoint(9121, 9999)
	if err != nil {
		t.Fatalf("third Ensure: %v", err)
	}
	if ep3.Port != 9121 {
		t.Errorf("overwrite via ephemeralPort=9121: got %d", ep3.Port)
	}
	if ep3.InstanceID != ep1.InstanceID {
		t.Errorf("instance_id rotated on port overwrite: %q -> %q", ep1.InstanceID, ep3.InstanceID)
	}
}

// TestRotateHubInstanceIDRewritesFile pins the operator-driven
// rotation: after RotateHubInstanceID, the instance_id differs and
// any subsequent Ensure reads the rotated value back.
func TestRotateHubInstanceIDRewritesFile(t *testing.T) {
	hubMcpStateTestHelper(t)

	ep1, err := EnsureHubEndpoint(9120, 1234)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	ep2, err := RotateHubInstanceID()
	if err != nil {
		t.Fatalf("RotateHubInstanceID: %v", err)
	}
	if ep2.InstanceID == ep1.InstanceID {
		t.Errorf("RotateHubInstanceID did not rotate; got %q twice", ep1.InstanceID)
	}
	if len(ep2.InstanceID) != 64 {
		t.Errorf("rotated instance_id len = %d, want 64", len(ep2.InstanceID))
	}

	ep3, err := EnsureHubEndpoint(0, 1234)
	if err != nil {
		t.Fatalf("post-rotate Ensure: %v", err)
	}
	if ep3.InstanceID != ep2.InstanceID {
		t.Errorf("post-rotate Ensure read = %q, want %q", ep3.InstanceID, ep2.InstanceID)
	}
}

// TestResetHubPortClearsPortKeepsInstanceID pins the
// `mcphub gui --reset-port` recovery contract: Port is zeroed so the
// next ephemeral bind picks a fresh allocation, but instance_id is
// preserved (rotating it is a separate `regenerate-instance-id`
// command per the credential burn-down workflow in the spec).
func TestResetHubPortClearsPortKeepsInstanceID(t *testing.T) {
	hubMcpStateTestHelper(t)

	ep1, err := EnsureHubEndpoint(9120, 1234)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if err := ResetHubPort(); err != nil {
		t.Fatalf("ResetHubPort: %v", err)
	}

	// Read directly to confirm Port was cleared on disk.
	raw, err := readHubMcpStateFile(hubMcpEndpointFileLeaf)
	if err != nil {
		t.Fatalf("read after reset: %v", err)
	}
	var loaded HubEndpoint
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if loaded.Port != 0 {
		t.Errorf("Port after Reset = %d, want 0", loaded.Port)
	}
	if loaded.InstanceID != ep1.InstanceID {
		t.Errorf("instance_id rotated by ResetHubPort: %q -> %q", ep1.InstanceID, loaded.InstanceID)
	}

	// And the next Ensure with ephemeralPort=0 sees Port=0 (caller
	// supplies the listener-assigned port via a subsequent call).
	ep2, err := EnsureHubEndpoint(0, 1234)
	if err != nil {
		t.Fatalf("post-reset Ensure: %v", err)
	}
	if ep2.Port != 0 {
		t.Errorf("post-reset Ensure Port = %d, want 0", ep2.Port)
	}
	if ep2.InstanceID != ep1.InstanceID {
		t.Errorf("post-reset Ensure instance_id rotated: %q -> %q", ep1.InstanceID, ep2.InstanceID)
	}
}

// TestLoadHubEndpointReturnsErrWhenMissing verifies the
// non-generating read helper surfaces the underlying I/O error rather
// than fabricating a zero struct. Phase 4/5 callers branch on this
// error to decide between "hub is running" and "hub never started".
func TestLoadHubEndpointReturnsErrWhenMissing(t *testing.T) {
	hubMcpStateTestHelper(t)

	if _, err := LoadHubEndpoint(); err == nil {
		t.Fatalf("LoadHubEndpoint on empty state-dir must return error; got nil")
	}
}

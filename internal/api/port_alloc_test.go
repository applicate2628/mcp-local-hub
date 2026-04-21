package api

import (
	"errors"
	"testing"

	"mcp-local-hub/internal/config"
)

// TestAllocatePort_SkipsOSBoundPorts guards against the "registry says
// free but something else holds the port" hazard. Without the OS-level
// bind check, Register would return success but the proxy would
// immediately fail to bind — a silent-broken registration.
func TestAllocatePort_SkipsOSBoundPorts(t *testing.T) {
	origAvail := portAvailable
	defer func() { portAvailable = origAvail }()
	// Simulate 9200 + 9201 being held by unrelated processes. Allocator
	// must skip both and return 9202 even though registry is empty.
	portAvailable = func(port int) bool { return port >= 9202 }
	reg := NewRegistry(t.TempDir() + "/reg.yaml")
	got, err := AllocatePort(reg, config.PortPool{Start: 9200, End: 9299})
	if err != nil {
		t.Fatalf("AllocatePort: %v", err)
	}
	if got != 9202 {
		t.Errorf("got %d, want 9202 (9200+9201 OS-bound should be skipped)", got)
	}
}

// TestAllocatePort_ExhaustedWhenEntirePoolOSBound guards the error path:
// if every pool port is bound externally, AllocatePort must return
// ErrPortPoolExhausted rather than silently returning a bad port.
func TestAllocatePort_ExhaustedWhenEntirePoolOSBound(t *testing.T) {
	origAvail := portAvailable
	defer func() { portAvailable = origAvail }()
	portAvailable = func(int) bool { return false }
	reg := NewRegistry(t.TempDir() + "/reg.yaml")
	_, err := AllocatePort(reg, config.PortPool{Start: 9200, End: 9202})
	if err == nil {
		t.Fatal("expected ErrPortPoolExhausted when every port is OS-bound")
	}
	if !errors.Is(err, ErrPortPoolExhausted) {
		t.Errorf("error should unwrap to ErrPortPoolExhausted; got: %v", err)
	}
}

func TestAllocatePort_FirstFreeInEmptyRegistry(t *testing.T) {
	reg := NewRegistry(t.TempDir() + "/reg.yaml")
	got, err := AllocatePort(reg, config.PortPool{Start: 9200, End: 9299})
	if err != nil {
		t.Fatalf("AllocatePort: %v", err)
	}
	if got != 9200 {
		t.Errorf("got %d, want 9200", got)
	}
}

func TestAllocatePort_SkipsAllocated(t *testing.T) {
	reg := NewRegistry(t.TempDir() + "/reg.yaml")
	reg.Put(WorkspaceEntry{WorkspaceKey: "a", Language: "python", Port: 9200})
	reg.Put(WorkspaceEntry{WorkspaceKey: "b", Language: "python", Port: 9201})
	got, err := AllocatePort(reg, config.PortPool{Start: 9200, End: 9299})
	if err != nil {
		t.Fatal(err)
	}
	if got != 9202 {
		t.Errorf("got %d, want 9202 (first free after 9200,9201)", got)
	}
}

func TestAllocatePort_FillsHoles(t *testing.T) {
	reg := NewRegistry(t.TempDir() + "/reg.yaml")
	reg.Put(WorkspaceEntry{WorkspaceKey: "a", Language: "python", Port: 9200})
	reg.Put(WorkspaceEntry{WorkspaceKey: "b", Language: "go", Port: 9202})
	got, err := AllocatePort(reg, config.PortPool{Start: 9200, End: 9299})
	if err != nil {
		t.Fatal(err)
	}
	if got != 9201 {
		t.Errorf("got %d, want 9201 (hole between 9200 and 9202)", got)
	}
}

func TestAllocatePort_ExhaustedPoolReturnsError(t *testing.T) {
	reg := NewRegistry(t.TempDir() + "/reg.yaml")
	for p := 9200; p <= 9202; p++ {
		reg.Put(WorkspaceEntry{WorkspaceKey: "k", Language: "l" + string(rune('a'+p-9200)), Port: p})
	}
	_, err := AllocatePort(reg, config.PortPool{Start: 9200, End: 9202})
	if err == nil {
		t.Fatal("expected ErrPortPoolExhausted")
	}
}

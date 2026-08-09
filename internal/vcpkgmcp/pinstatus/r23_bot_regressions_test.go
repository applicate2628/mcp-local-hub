package pinstatus

import (
	"context"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR23ExternalRemoteHelperSchemeIsRejected(t *testing.T) {
	approved, reason := approveRemoteURL("evil://host/repo.git")
	if reason != ReasonRemoteURLTransportUnapproved {
		t.Fatalf("reason=%q, want %q", reason, ReasonRemoteURLTransportUnapproved)
	}
	if raw, ok := approved.transportArgument(); ok || raw != "" {
		t.Fatalf("unapproved helper scheme gained transport authority: raw=%q ok=%v", raw, ok)
	}
}

func TestR23ExternalRemoteHelperSchemeStartsNoRemoteQuery(t *testing.T) {
	dir := newPort(t, "unapproved-transport", `vcpkg_from_git(
    OUT_SOURCE_PATH SOURCE_PATH
    URL evil://host/repo.git
    REF 0123456789abcdef0123456789abcdef01234567
    SHA512 0
)`)
	calls := 0
	result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS: DefaultFS(),
		RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
			calls++
			return nil, nil
		},
		Now: fixedNow(),
	}).Ports[0]
	if calls != 0 {
		t.Fatalf("remote queries=%d, want 0", calls)
	}
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonRemoteURLTransportUnapproved {
		t.Fatalf("status/reason=%s/%s, want unknown/%s", result.Status, result.Reason, ReasonRemoteURLTransportUnapproved)
	}
}

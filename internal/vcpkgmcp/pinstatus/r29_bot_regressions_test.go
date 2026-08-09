package pinstatus

import (
	"context"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR29EnvironmentAndCacheRefsNeverReachRemote(t *testing.T) {
	for _, ref := range []string{"$ENV{SOURCE_TAG}", "$CACHE{SOURCE_TAG}"} {
		t.Run(ref, func(t *testing.T) {
			dir := newPort(t, "dynamic-ref", "vcpkg_from_github(REPO acme/demo REF \""+ref+"\" SHA512 0)\n")
			calls := 0
			port := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
				FS:         DefaultFS(),
				RemoteRefs: countingRemote(&calls, nil),
				Now:        fixedNow(),
			}).Ports[0]
			if port.Status != evidence.StatusUnknown || port.Reason != ReasonRefUnresolvable {
				t.Fatalf("port=%+v, want unknown/ref_unresolvable", port)
			}
			if calls != 0 {
				t.Fatalf("remote calls=%d, want 0 for unresolved %s", calls, ref)
			}
		})
	}
}

func TestR29EnvironmentHeadRefNeverFallsBackToHEAD(t *testing.T) {
	dir := newPort(t, "dynamic-head-ref", "vcpkg_from_github(REPO acme/demo REF "+commitA+" HEAD_REF \"$ENV{SOURCE_TAG}\" SHA512 0)\n")
	calls := 0
	port := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:         DefaultFS(),
		RemoteRefs: countingRemote(&calls, nil),
		Now:        fixedNow(),
	}).Ports[0]
	if port.Status != evidence.StatusUnknown || port.Reason != ReasonHeadRefUnresolvable {
		t.Fatalf("port=%+v, want unknown/head_ref_unresolvable", port)
	}
	if calls != 0 {
		t.Fatalf("remote calls=%d, want 0 for unresolved environment HEAD_REF", calls)
	}
}

func TestR29StringMutationInvalidatesRetainedRef(t *testing.T) {
	dir := newPort(t, "string-ref", `
set(REF release)
string(APPEND REF -hotfix)
vcpkg_from_github(REPO acme/demo REF ${REF} SHA512 0)
`)
	calls := 0
	port := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:         DefaultFS(),
		RemoteRefs: countingRemote(&calls, nil),
		Now:        fixedNow(),
	}).Ports[0]
	if port.Status != evidence.StatusUnknown || port.Reason != ReasonRefUnresolvable {
		t.Fatalf("port=%+v, want unknown/ref_unresolvable", port)
	}
	if calls != 0 {
		t.Fatalf("remote calls=%d, want 0 after unmodeled string mutation", calls)
	}
}

func TestR29ActiveBlockScopeFailsClosed(t *testing.T) {
	dir := newPort(t, "block-ref", `
set(REF old)
block()
  set(REF new)
endblock()
vcpkg_from_github(REPO acme/demo REF ${REF} SHA512 0)
`)
	calls := 0
	port := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:         DefaultFS(),
		RemoteRefs: countingRemote(&calls, nil),
		Now:        fixedNow(),
	}).Ports[0]
	if port.Status != evidence.StatusUnknown || port.Reason != ReasonPortfileUnparsable {
		t.Fatalf("port=%+v, want unknown/portfile_unparsable for unsupported active block scope", port)
	}
	if calls != 0 {
		t.Fatalf("remote calls=%d, want 0 for unsupported active block scope", calls)
	}
}

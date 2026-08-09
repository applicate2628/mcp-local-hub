package pinstatus

import (
	"context"
	"fmt"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR32LowercaseNotConditionIsStructurallyUnparsable(t *testing.T) {
	const portfile = `
if(not OFF)
  vcpkg_from_github(REPO example/repo REF 0123456789abcdef0123456789abcdef01234567 SHA512 0)
endif()
`
	if parsed, ok := parsePortfile(portfile); ok {
		t.Fatalf("parsePortfile accepted lowercase not as CMake NOT: %+v", parsed)
	}

	dir := newPort(t, "lowercase-not", portfile)
	calls := 0
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:         DefaultFS(),
		RemoteRefs: countingRemote(&calls, nil),
		Now:        fixedNow(),
	}).Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonPortfileUnparsable {
		t.Fatalf("result = %+v, want unknown/%s for invalid lowercase not", p, ReasonPortfileUnparsable)
	}
	if calls != 0 {
		t.Fatalf("remote calls = %d, want 0 for structurally invalid condition", calls)
	}
}

func TestR32ListPopInvalidatesInputAndEveryOutputVariable(t *testing.T) {
	const thirdCommit = "fedcba9876543210fedcba9876543210fedcba98"
	for _, operation := range []string{"POP_FRONT", "POP_BACK"} {
		for _, target := range []string{"PIN_LIST", "OUT_ONE", "OUT_TWO"} {
			t.Run(operation+"/"+target, func(t *testing.T) {
				portfile := fmt.Sprintf(`
set(PIN_LIST %s)
set(OUT_ONE %s)
set(OUT_TWO %s)
list(%s PIN_LIST OUT_ONE OUT_TWO)
vcpkg_from_github(REPO example/repo REF ${%s} SHA512 0)
`, commitA, commitB, thirdCommit, operation, target)
				parsed, ok := parsePortfile(portfile)
				if !ok {
					t.Fatalf("parsePortfile rejected structurally valid %s", operation)
				}
				if parsed.Pin.ResolvedRef != "" || parsed.Pin.UnresolvedVariable != target {
					t.Fatalf("pin = %+v, want stale %s binding invalidated", parsed.Pin, target)
				}
			})
		}
	}
}

package pinstatus

import "testing"

func TestVariableMutationInvalidatesEarlierSetBinding(t *testing.T) {
	for _, mutation := range []string{
		"unset(PIN_REF)",
		"list(APPEND PIN_REF suffix)",
	} {
		portfile := "set(PIN_REF deadbeef)\n" + mutation + `
vcpkg_from_github(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO owner/repo
    REF "${PIN_REF}"
    SHA512 0
)
`
		parsed, ok := parsePortfileWithManifest(portfile, nil, "demo")
		if !ok {
			t.Fatalf("parsePortfileWithManifest(%q) rejected structurally valid input", mutation)
		}
		if parsed.Pin.ResolvedRef == "deadbeef" {
			t.Fatalf("%s left stale set() binding authoritative: %+v", mutation, parsed.Pin)
		}
		if parsed.Pin.UnresolvedVariable != "PIN_REF" {
			t.Fatalf("%s produced pin %+v, want explicit unresolved PIN_REF", mutation, parsed.Pin)
		}
	}
}

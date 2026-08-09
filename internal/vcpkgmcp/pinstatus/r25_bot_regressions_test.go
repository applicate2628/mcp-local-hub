package pinstatus

import "testing"

func TestR25UninvokedDeclarationDoesNotMutateTopLevelVariables(t *testing.T) {
	parsed, ok := parsePortfile(`
set(REF 0123456789abcdef0123456789abcdef01234567)
function(unused)
  set(REF fedcba9876543210fedcba9876543210fedcba98)
  unset(REF)
  list(APPEND REF ignored)
endfunction()
vcpkg_from_github(REPO owner/repo REF ${REF} SHA512 0)
`)
	if !ok || parsed.Pin.ResolvedRef != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("parsed=%+v ok=%t, want original top-level REF", parsed, ok)
	}
}

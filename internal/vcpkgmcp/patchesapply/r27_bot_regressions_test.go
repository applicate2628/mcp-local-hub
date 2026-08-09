package patchesapply

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestR27ExecutableTripletIncludeInvalidatesRetainedFacts(t *testing.T) {
	facts := parseTripletFacts("set(VCPKG_LIBRARY_LINKAGE static)\ninclude(shared.cmake)\n", "", "demo", "")
	if facts != nil {
		t.Fatalf("facts=%v, executable include must invalidate caller-scope facts", facts)
	}
}

func TestR27AuthorizationTokenTerminatesPatchesWithoutEcho(t *testing.T) {
	const opaqueValue = "opaque-token-value"
	dir := writeFixture(t, `vcpkg_from_github(
  OUT_SOURCE_PATH SOURCE_PATH
  REPO acme/demo
  REF v1
  SHA512 0
  PATCHES real.patch
   AUTHORIZATION_TOKEN `+opaqueValue+`
)`, "real.patch")
	result := ApplyOrder(Args{PortDir: dir, PortName: "demo", Triplet: "x64-windows"})
	if got := filenames(result.Applied); len(got) != 1 || got[0] != "real.patch" {
		t.Fatalf("applied=%v, want only real.patch", got)
	}
	if len(result.Missing) != 0 {
		t.Fatalf("missing=%+v, authorization option/value are not patch filenames", result.Missing)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), opaqueValue) {
		t.Fatalf("authorization token leaked into result: %s", body)
	}
}

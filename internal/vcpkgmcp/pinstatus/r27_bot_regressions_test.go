package pinstatus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestR27RejectedUnclassifiedFragmentValueIsNeverEmitted(t *testing.T) {
	const opaqueValue = "opaque-fragment-value"
	raw := "https://host/repo.git#jwt=" + opaqueValue
	if hasEmbeddedCredential(raw) {
		t.Fatal("unclassified fragment must not change the positive credential verdict")
	}
	if got := redactURL(raw); strings.Contains(got, opaqueValue) {
		t.Fatalf("redactURL leaked fragment value: %q", got)
	}
	dir := newPort(t, "fragment-redaction", `vcpkg_from_git(URL "`+raw+`" REF `+commitA+` SHA512 0)`)
	result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{Now: fixedNow()})
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), opaqueValue) {
		t.Fatalf("fragment value leaked into result: %s", body)
	}
	if got := result.Ports[0].Reason; got != ReasonPortfileUnparsable {
		t.Fatalf("reason=%s, want portfile_unparsable", got)
	}
}

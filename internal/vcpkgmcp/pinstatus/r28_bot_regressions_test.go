package pinstatus

import (
	"context"
	"testing"
)

func TestR28CommandNameCannotCrossLineEndingBeforeParen(t *testing.T) {
	dir := newPort(t, "newline-before-paren", "vcpkg_from_github\n(REPO acme/demo REF "+commitA+" SHA512 0)\n")
	result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{Now: fixedNow(), RemoteRefs: fakeRemote(nil, nil)})
	if len(result.Ports) != 1 || result.Ports[0].Reason != ReasonPortfileUnparsable {
		t.Fatalf("ports=%+v, want portfile_unparsable", result.Ports)
	}
}

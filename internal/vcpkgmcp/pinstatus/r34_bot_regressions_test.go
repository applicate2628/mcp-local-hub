package pinstatus

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

var _ publicresult.ProjectionAdmitter = Result{}

func TestR34StringJSONInvalidatesOutputAndErrorDestinations(t *testing.T) {
	for _, destination := range []string{"OUT", "ERR"} {
		t.Run(destination, func(t *testing.T) {
			portfile := `
set(OUT stale)
set(ERR stale)
string(JSON OUT ERROR_VARIABLE ERR GET [[{}]] missing)
vcpkg_from_github(REPO example/repo REF ${` + destination + `} SHA512 0)
`
			parsed, ok := parsePortfile(portfile)
			if !ok {
				t.Fatal("parsePortfile rejected structurally valid string(JSON) input")
			}
			if parsed.Pin.ResolvedRef != "" || parsed.Pin.UnresolvedVariable != destination {
				t.Fatalf("pin = %+v, want stale %s binding invalidated", parsed.Pin, destination)
			}
		})
	}
}

func TestR34PinStatusPreAdmissionRejectsLargeCandidateAggregate(t *testing.T) {
	result := Result{Ports: make([]PortResult, MaxPortDirs)}
	for portIndex := range result.Ports {
		result.Ports[portIndex].Candidates = make([]FetchCandidate, MaxFetchCandidatesPerPort)
		for candidateIndex := range result.Ports[portIndex].Candidates {
			result.Ports[portIndex].Candidates[candidateIndex].Guard = strings.Repeat("x", 32)
		}
	}
	if !result.PublicResultRequiresProjection(publicresult.MaxEncodedBytes) {
		t.Fatal("large bounded candidate aggregate was admitted for full JSON encoding")
	}
}

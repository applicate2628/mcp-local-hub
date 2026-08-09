package pinstatus

import (
	"context"
	"strings"
	"testing"
)

func TestR31EscapedQuotedVariableReferenceRemainsLiteral(t *testing.T) {
	parsed, ok := parsePortfileWithManifest(
		`vcpkg_from_github(REPO owner/repo REF "\${VERSION}")`,
		[]byte(`{"version":"9.9.9"}`),
		"demo",
	)
	if !ok {
		t.Fatal("escaped quoted variable reference made a valid portfile unparsable")
	}
	if got := parsed.Pin.Ref; got != `${VERSION}` {
		t.Fatalf("pin.ref = %q, want literal %q", got, `${VERSION}`)
	}
	if parsed.Pin.ResolvedRef != "" || parsed.Pin.UnresolvedVariable != "" {
		t.Fatalf("escaped literal was treated as a variable: %+v", parsed.Pin)
	}
	if !parsed.Pin.Literal {
		t.Fatalf("escaped literal provenance was lost: %+v", parsed.Pin)
	}

	mixed, mixedOK := parsePortfileWithManifest(
		`vcpkg_from_github(REPO owner/repo REF "\${VERSION}-${PORT}")`,
		[]byte(`{"version":"9.9.9"}`),
		"demo",
	)
	if !mixedOK || mixed.Pin.Ref != `${VERSION}-${PORT}` || mixed.Pin.ResolvedRef != `${VERSION}-demo` || !mixed.Pin.Literal {
		t.Fatalf("mixed escaped/expandable provenance = %+v ok=%v", mixed.Pin, mixedOK)
	}

	head, headOK := parsePortfileWithManifest(
		`vcpkg_from_github(REPO owner/repo REF main HEAD_REF "\${LITERAL}-${UNKNOWN}")`,
		nil,
		"demo",
	)
	if !headOK || head.UnresolvedHeadRefVariable != "UNKNOWN" {
		t.Fatalf("head=%+v ok=%v, want only the unescaped HEAD_REF variable reported", head, headOK)
	}
}

func TestR31RefExpansionStopsAtCandidateByteLimit(t *testing.T) {
	value := strings.Repeat("x", MaxRetainedFetchCandidateBytesPerPort/2+1)
	manifest := []byte(`{"version-string":"` + value + `"}`)

	expanded, _, _, ok, limitExceeded := expandVariablesBounded(
		newVariableEnvironment(), manifest, "demo",
		argValue{Text: `${VERSION}${VERSION}`},
		MaxRetainedFetchCandidateBytesPerPort,
	)
	if ok || !limitExceeded || expanded != "" {
		t.Fatalf("expanded bytes=%d ok=%v limit=%v, want no materialized result and explicit limit", len(expanded), ok, limitExceeded)
	}

	parsed, parsedOK := parsePortfileWithManifest(
		`vcpkg_from_github(REPO owner/repo REF "${VERSION}${VERSION}")`,
		manifest,
		"demo",
	)
	if !parsedOK || !parsed.CandidateLimitExceeded || len(parsed.Candidates) != 0 {
		t.Fatalf("parsed=%+v ok=%v, want explicit pre-retention candidate limit", parsed, parsedOK)
	}

	dir := newPort(t, "bounded", `vcpkg_from_github(REPO owner/repo REF "${VERSION}${VERSION}")`)
	writeManifest(t, dir, string(manifest))
	remoteCalls := 0
	result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:  DefaultFS(),
		Now: fixedNow(),
		RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
			remoteCalls++
			return nil, nil
		},
	})
	if len(result.Ports) != 1 || result.Ports[0].Reason != ReasonFetchCandidateLimit || remoteCalls != 0 {
		t.Fatalf("result=%+v remoteCalls=%d, want fetch_candidate_limit before network", result, remoteCalls)
	}
}

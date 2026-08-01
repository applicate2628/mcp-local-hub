package pinstatus

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func countingRemote(calls *int, refs map[string]map[string]string) remoteRefsFn {
	base := fakeRemote(refs, nil)
	return func(ctx context.Context, remote approvedRemoteURL) (map[string]string, error) {
		*calls++
		return base(ctx, remote)
	}
}

func TestPinStatusVariableEnvironmentUsesEachFetchCallSite(t *testing.T) {
	dir := newPort(t, "call-site-values", `
set(MY_REF "`+commitA+`")
vcpkg_from_github(REPO example/first REF ${MY_REF} SHA512 0)
set(MY_REF "`+commitB+`")
vcpkg_from_github(OUT_SOURCE_PATH SOURCE_PATH REPO example/second REF ${MY_REF} SHA512 0)
`)
	remote := "https://github.com/example/second.git"
	calls := 0
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:         DefaultFS(),
		RemoteRefs: countingRemote(&calls, map[string]map[string]string{remote: {"HEAD": commitB}}),
		Now:        fixedNow(),
	}).Ports[0]

	if p.Status != evidence.StatusOK || p.Pin.ResolvedRef != commitB || p.Pin.ResolvedFrom != RefValueSourceLocalSet {
		t.Fatalf("selected result = %+v, want second call-site value %q", p, commitB)
	}
	if len(p.Candidates) != 2 || p.Candidates[0].Pin.ResolvedRef != commitA || p.Candidates[1].Pin.ResolvedRef != commitB {
		t.Fatalf("candidate pins = %+v, want first=%q second=%q", p.Candidates, commitA, commitB)
	}
	if calls != 1 {
		t.Fatalf("remote calls = %d, want 1 for the selected source", calls)
	}
}

func TestPinStatusVariableEnvironmentDoesNotSeeLaterAssignment(t *testing.T) {
	dir := newPort(t, "after-fetch", `
vcpkg_from_github(REPO example/after REF ${MY_REF} SHA512 0)
set(MY_REF "`+commitA+`")
`)
	calls := 0
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:         DefaultFS(),
		RemoteRefs: countingRemote(&calls, nil),
		Now:        fixedNow(),
	}).Ports[0]

	if p.Status != evidence.StatusUnknown || p.Reason != ReasonRefUnresolvable || p.Pin.UnresolvedVariable != "MY_REF" {
		t.Fatalf("result = %+v, want unknown/ref_unresolvable for MY_REF", p)
	}
	if calls != 0 {
		t.Fatalf("remote calls = %d, want 0 when a later assignment cannot resolve REF", calls)
	}
}

func TestPinStatusVariableEnvironmentUnknownAssignmentBlocksFallback(t *testing.T) {
	dir := newPort(t, "unknown-fallback", `
if(PORT_SOURCE_SWITCH)
  set(VERSION "v9.9.9")
endif()
vcpkg_from_github(REPO example/fallback REF ${VERSION} SHA512 0)
`)
	writeManifest(t, dir, `{"version":"v1.2.3"}`)
	calls := 0
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:         DefaultFS(),
		RemoteRefs: countingRemote(&calls, nil),
		Now:        fixedNow(),
	}).Ports[0]

	if p.Status != evidence.StatusUnknown || p.Reason != ReasonRefUnresolvable || p.Pin.ResolvedFrom != "" || p.Pin.UnresolvedVariable != "VERSION" {
		t.Fatalf("result = %+v, want unknown local VERSION rather than manifest fallback", p)
	}
	if calls != 0 {
		t.Fatalf("remote calls = %d, want 0 when a guarded local binding is unknown", calls)
	}
}

func TestPinStatusVariableEnvironmentLaterDefiniteWriteClosesUnknown(t *testing.T) {
	dir := newPort(t, "unknown-then-known", `
if(PORT_SOURCE_SWITCH)
  set(MY_REF "`+commitA+`")
endif()
set(MY_REF "`+commitB+`")
vcpkg_from_github(REPO example/known REF ${MY_REF} SHA512 0)
`)
	remote := "https://github.com/example/known.git"
	calls := 0
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:         DefaultFS(),
		RemoteRefs: countingRemote(&calls, map[string]map[string]string{remote: {"HEAD": commitB}}),
		Now:        fixedNow(),
	}).Ports[0]

	if p.Status != evidence.StatusOK || p.Pin.ResolvedRef != commitB || p.Pin.ResolvedFrom != RefValueSourceLocalSet {
		t.Fatalf("result = %+v, want later definite local binding", p)
	}
	if calls != 1 {
		t.Fatalf("remote calls = %d, want 1 after a later definite binding", calls)
	}
}

func TestPinStatusVariableEnvironmentConditionAndScopeStates(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantOK  bool
	}{
		{
			name: "true conditional assignment is definite",
			content: `
if(ON)
  set(MY_REF "` + commitA + `")
endif()
vcpkg_from_github(REPO example/on REF ${MY_REF} SHA512 0)
`,
			wantOK: true,
		},
		{
			name: "false conditional assignment is absent",
			content: `
if(OFF)
  set(MY_REF "` + commitA + `")
endif()
vcpkg_from_github(REPO example/off REF ${MY_REF} SHA512 0)
`,
		},
		{
			name: "unsupported function scope is unknown",
			content: `
function(configure_ref)
  set(MY_REF "` + commitA + `")
endfunction()
vcpkg_from_github(REPO example/function REF ${MY_REF} SHA512 0)
`,
		},
		{
			name: "dynamic target taints older values and fallback",
			content: `
set(${DYNAMIC_TARGET} "` + commitA + `")
vcpkg_from_github(REPO example/dynamic REF ${VERSION} SHA512 0)
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := newPort(t, "states", tc.content)
			writeManifest(t, dir, `{"version":"v1.2.3"}`)
			remote := "https://github.com/example/on.git"
			calls := 0
			p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
				FS:         DefaultFS(),
				RemoteRefs: countingRemote(&calls, map[string]map[string]string{remote: {"HEAD": commitA}}),
				Now:        fixedNow(),
			}).Ports[0]
			if tc.wantOK {
				if p.Status != evidence.StatusOK || p.Pin.ResolvedRef != commitA || calls != 1 {
					t.Fatalf("result = %+v calls=%d, want active local assignment", p, calls)
				}
				return
			}
			if p.Status != evidence.StatusUnknown || p.Reason != ReasonRefUnresolvable || calls != 0 {
				t.Fatalf("result = %+v calls=%d, want unknown/ref_unresolvable without remote call", p, calls)
			}
		})
	}
}

func TestPinStatusVariableEnvironmentFailsClosedAcrossFetchFields(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    Reason
	}{
		{
			name: "REF",
			content: `
vcpkg_from_github(REPO example/ref REF ${LATE} SHA512 0)
set(LATE "` + commitA + `")
`,
			want: ReasonRefUnresolvable,
		},
		{
			name: "HEAD_REF",
			content: `
vcpkg_from_github(REPO example/head REF ` + commitA + ` HEAD_REF ${LATE} SHA512 0)
set(LATE HEAD)
`,
			want: ReasonHeadRefUnresolvable,
		},
		{
			name: "REPO",
			content: `
vcpkg_from_github(REPO ${LATE} REF ` + commitA + ` SHA512 0)
set(LATE example/repo)
`,
			want: ReasonPortfileUnparsable,
		},
		{
			name: "URL",
			content: `
vcpkg_from_git(URL ${LATE} REF ` + commitA + ` SHA512 0)
set(LATE https://example.com/repo.git)
`,
			want: ReasonPortfileUnparsable,
		},
		{
			name: "GITLAB_URL",
			content: `
vcpkg_from_gitlab(GITLAB_URL ${LATE} REPO example/repo REF ` + commitA + ` SHA512 0)
set(LATE https://gitlab.example.com)
`,
			want: ReasonPortfileUnparsable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := newPort(t, "late-"+strings.ToLower(tc.name), tc.content)
			calls := 0
			p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
				FS:         DefaultFS(),
				RemoteRefs: countingRemote(&calls, nil),
				Now:        fixedNow(),
			}).Ports[0]
			if p.Status != evidence.StatusUnknown || p.Reason != tc.want {
				t.Fatalf("result = %+v, want unknown/%s", p, tc.want)
			}
			if calls != 0 {
				t.Fatalf("remote calls = %d, want 0 for unresolved %s", calls, tc.name)
			}
		})
	}
}

func TestPortfileVariableResolutionHasNoWholeFileLookup(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "portfile.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"func resolveSetVariable(",
		"func expandVariables(content ",
		"func resolveRefVariable(content ",
		"func resolveMaybeVariable(content ",
		"func buildPin(content ",
		"func parseFetchCandidate(name, content ",
	} {
		if strings.Contains(string(source), forbidden) {
			t.Fatalf("whole-file variable lookup residue %q remains in portfile.go", forbidden)
		}
	}
	if !strings.Contains(string(source), "recordSetAssignment(&variables") {
		t.Fatal("parsePortfileWithManifest does not update the call-site variable environment")
	}
}

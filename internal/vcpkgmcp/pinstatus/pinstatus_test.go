package pinstatus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

// --- fixture helpers --------------------------------------------------------

// newPort writes content as <tempdir>/<name>/portfile.cmake using t.TempDir()
// (never a real repo path) and returns the port directory.
func newPort(t *testing.T, name, content string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "portfile.cmake"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return dir
}

func writeManifest(t *testing.T, portDir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(portDir, "vcpkg.json"), []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile vcpkg.json: %v", err)
	}
}

// fakeRemote builds a remoteRefsFn that returns refsByRemote[remote] (or an
// error when errsByRemote[remote] is set), and never touches the network —
// this is the injectable seam every test in this file substitutes.
func fakeRemote(refsByRemote map[string]map[string]string, errsByRemote map[string]error) remoteRefsFn {
	return func(ctx context.Context, remote string) (map[string]string, error) {
		if err, ok := errsByRemote[remote]; ok {
			return nil, err
		}
		if refs, ok := refsByRemote[remote]; ok {
			return refs, nil
		}
		return nil, errors.New("fakeRemote: no fixture registered for " + remote)
	}
}

func fixedNow() func() time.Time {
	t := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

const (
	commitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	commitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	commitC = "cccccccccccccccccccccccccccccccccccccccc"
	commitD = "dddddddddddddddddddddddddddddddddddddddd"
)

// --- tests -------------------------------------------------------------------

func TestGitHubPinEqualToTrackedTip_Current(t *testing.T) {
	dir := newPort(t, "json", `
vcpkg_from_github(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO nlohmann/json
    REF `+commitA+`
    SHA512 0
)
`)
	remote := "https://github.com/nlohmann/json.git"
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitA}}, nil),
		Now:        fixedNow(),
	}

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps)
	if len(res.Ports) != 1 {
		t.Fatalf("got %d ports, want 1", len(res.Ports))
	}
	p := res.Ports[0]
	if p.Status != evidence.StatusOK {
		t.Fatalf("status = %v, reason = %v, want ok", p.Status, p.Reason)
	}
	if p.Remote.Kind != RemoteGitHub || p.Remote.Repo != "nlohmann/json" || p.Remote.URL != remote {
		t.Fatalf("remote = %+v", p.Remote)
	}
	if p.Pin.Shape != RefShapeCommit40Hex || p.Pin.Ref != commitA {
		t.Fatalf("pin = %+v", p.Pin)
	}
	if p.PinnedSHA != commitA || p.TipSHA != commitA || p.TrackedRef != "HEAD" {
		t.Fatalf("SHAs = pinned=%q tip=%q tracked=%q", p.PinnedSHA, p.TipSHA, p.TrackedRef)
	}
}

func TestGitHubPinDifferent_UnknownPinNotAtTip_CarriesBothSHAs(t *testing.T) {
	dir := newPort(t, "json", `
vcpkg_from_github(
    REPO nlohmann/json
    REF `+commitB+`
    SHA512 0
)
`)
	remote := "https://github.com/nlohmann/json.git"
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitA}}, nil),
		Now:        fixedNow(),
	}

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps)
	p := res.Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonPinNotAtTip {
		t.Fatalf("status=%v reason=%v, want unknown/pin_not_at_tip", p.Status, p.Reason)
	}
	if p.PinnedSHA != commitB {
		t.Fatalf("pinned_sha = %q, want %q", p.PinnedSHA, commitB)
	}
	if p.TipSHA != commitA {
		t.Fatalf("tip_sha = %q, want %q", p.TipSHA, commitA)
	}
	wantCompare := "https://github.com/nlohmann/json/compare/" + commitB + "..." + commitA
	if p.CompareURL != wantCompare {
		t.Fatalf("compare_url = %q, want %q", p.CompareURL, wantCompare)
	}
}

func TestVcpkgFromGit_VariableRefResolvedFromLocalSet(t *testing.T) {
	dir := newPort(t, "somelib", `
set(MY_REF "`+commitC+`")
vcpkg_from_git(
    URL https://example.com/foo/bar.git
    REF ${MY_REF}
)
`)
	remote := "https://example.com/foo/bar.git"
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitC}}, nil),
		Now:        fixedNow(),
	}

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps)
	p := res.Ports[0]
	if p.Remote.Kind != RemoteGit || p.Remote.URL != remote {
		t.Fatalf("remote = %+v", p.Remote)
	}
	if p.Pin.Shape != RefShapeVariableResolved {
		t.Fatalf("pin.shape = %v, want variable_resolved", p.Pin.Shape)
	}
	if p.Pin.ResolvedRef != commitC {
		t.Fatalf("pin.resolved_ref = %q, want %q", p.Pin.ResolvedRef, commitC)
	}
	if p.Pin.ResolvedFrom != RefValueSourceLocalSet {
		t.Fatalf("pin.resolved_from = %q, want %q", p.Pin.ResolvedFrom, RefValueSourceLocalSet)
	}
	if p.Status != evidence.StatusOK {
		t.Fatalf("status = %v reason = %v, want ok (resolved ref equals fake HEAD)", p.Status, p.Reason)
	}
}

func TestVersionRefResolvedFromSiblingManifestFields(t *testing.T) {
	fields := []struct {
		name, json, version string
	}{
		{"version", `{"version":"4.2.2"}`, "4.2.2"},
		{"version-string", `{"version-string":"4.2.2-string"}`, "4.2.2-string"},
		{"version-date", `{"version-date":"2026-07-26"}`, "2026-07-26"},
		{"version-semver", `{"version-semver":"4.2.2-beta.1"}`, "4.2.2-beta.1"},
	}
	for _, tc := range fields {
		t.Run(tc.name, func(t *testing.T) {
			dir := newPort(t, "continuable", `
vcpkg_from_github(REPO continuable/continuable REF "${VERSION}" SHA512 0)
`)
			writeManifest(t, dir, tc.json)
			remote := "https://github.com/continuable/continuable.git"
			deps := Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"refs/tags/" + tc.version: commitA}}, nil), Now: fixedNow()}

			p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps).Ports[0]
			if p.Status != evidence.StatusUnknown || p.Reason != ReasonNamedRefNotComparable {
				t.Fatalf("status=%v reason=%v, want unknown/named_ref_not_comparable", p.Status, p.Reason)
			}
			if p.Pin.Ref != "${VERSION}" || p.Pin.ResolvedRef != tc.version || p.Pin.ResolvedFrom != RefValueSourceManifest {
				t.Fatalf("pin = %+v, want manifest-resolved %q", p.Pin, tc.version)
			}
		})
	}
}

func TestDefaultGuardChoosesSpeexdspReleaseDistfile(t *testing.T) {
	dir := newPort(t, "speexdsp", `
if(VCPKG_USE_HEAD_VERSION)
    vcpkg_from_gitlab(GITLAB_URL "https://gitlab.xiph.org" REPO xiph/speexdsp REF master HEAD_REF master)
else()
    vcpkg_download_distfile(ARCHIVE URLS "http://downloads.xiph.org/releases/speex/speexdsp-1.2.1.tar.gz" SHA512 0)
endif()
`)
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(nil, nil), Now: fixedNow()}).Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonNotGitComparable || p.Remote.Kind != RemoteDistfile {
		t.Fatalf("got status=%v reason=%v remote=%+v; want release distfile", p.Status, p.Reason, p.Remote)
	}
	if p.Remote.Repo == "xiph/speexdsp" || p.Remote.URL == "https://gitlab.xiph.org/xiph/speexdsp.git" {
		t.Fatalf("default false guard must not select GitLab source: %+v", p.Remote)
	}
	if len(p.Candidates) != 2 {
		t.Fatalf("candidates=%+v, want both guarded calls", p.Candidates)
	}
	if p.Candidates[0].Remote.Repo != "xiph/speexdsp" || p.Candidates[0].Guard != "VCPKG_USE_HEAD_VERSION" || p.Candidates[0].ActiveForDefault {
		t.Fatalf("head candidate=%+v, want recorded false GitLab guard", p.Candidates[0])
	}
	if p.Candidates[1].Remote.Kind != RemoteDistfile || p.Candidates[1].Guard != "NOT (VCPKG_USE_HEAD_VERSION)" || !p.Candidates[1].ActiveForDefault {
		t.Fatalf("release candidate=%+v, want recorded true release guard", p.Candidates[1])
	}
}

func TestConditionalSourceRecordsNestedElseIfElseGuard(t *testing.T) {
	dir := newPort(t, "guarded", `
if(VCPKG_USE_HEAD_VERSION)
  vcpkg_from_github(REPO example/head REF `+commitA+` SHA512 0)
elseif(OFF)
  vcpkg_from_github(REPO example/disabled REF `+commitA+` SHA512 0)
else()
  if(ON)
    vcpkg_from_github(REPO example/release REF `+commitB+` SHA512 0)
  endif()
endif()
`)
	remote := "https://github.com/example/release.git"
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitB}}, nil), Now: fixedNow()}).Ports[0]
	if p.Status != evidence.StatusOK || p.Remote.Repo != "example/release" {
		t.Fatalf("status=%v reason=%v remote=%+v, want guarded release source", p.Status, p.Reason, p.Remote)
	}
	if len(p.Candidates) != 3 {
		t.Fatalf("candidates=%+v, want every guarded source call", p.Candidates)
	}
	wantGuard := "NOT (VCPKG_USE_HEAD_VERSION) AND NOT (OFF) AND ON"
	if p.Candidates[2].Guard != wantGuard || !p.Candidates[2].ActiveForDefault {
		t.Fatalf("release candidate=%+v, want active guard %q", p.Candidates[2], wantGuard)
	}
}

func TestUnresolvedGuardReturnsCandidatesRatherThanGuess(t *testing.T) {
	dir := newPort(t, "conditional", `
if(PORT_SOURCE_SWITCH)
  vcpkg_from_github(REPO example/first REF `+commitA+` SHA512 0)
else()
  vcpkg_from_github(REPO example/second REF `+commitA+` SHA512 0)
endif()
`)
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(nil, nil), Now: fixedNow()}).Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonGuardUnresolvable || p.UnresolvedGuardVariable != "PORT_SOURCE_SWITCH" {
		t.Fatalf("status=%v reason=%v variable=%q", p.Status, p.Reason, p.UnresolvedGuardVariable)
	}
	if len(p.Candidates) != 2 || p.Candidates[0].Remote.Repo != "example/first" || p.Candidates[1].Remote.Repo != "example/second" {
		t.Fatalf("candidates = %+v, want both guarded forks", p.Candidates)
	}
}

func TestMultipleFetchCallsSelectsSourcePathAndReportsAllCandidates(t *testing.T) {
	dir := newPort(t, "cryptopp-cmake", `
vcpkg_from_github(REPO abdes/cryptopp-cmake REF `+commitA+` SHA512 0)
vcpkg_from_github(OUT_SOURCE_PATH SOURCE_PATH REPO weidai11/cryptopp REF `+commitB+` SHA512 0)
`)
	remote := "https://github.com/weidai11/cryptopp.git"
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitB}}, nil), Now: fixedNow()}).Ports[0]
	if p.Status != evidence.StatusOK || p.Remote.Repo != "weidai11/cryptopp" {
		t.Fatalf("status=%v reason=%v remote=%+v, want bound source", p.Status, p.Reason, p.Remote)
	}
	if len(p.Candidates) != 2 || p.Candidates[0].BindsSourcePath || !p.Candidates[1].BindsSourcePath {
		t.Fatalf("candidates=%+v, want all calls and second source binding", p.Candidates)
	}
}

func TestMultipleUnboundFetchCallsReturnUnknownWithAllCandidates(t *testing.T) {
	dir := newPort(t, "multiple", `
vcpkg_from_github(REPO example/first REF `+commitA+` SHA512 0)
vcpkg_from_github(REPO example/second REF `+commitB+` SHA512 0)
`)
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(nil, nil), Now: fixedNow()}).Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonMultipleFetchCalls {
		t.Fatalf("status=%v reason=%v, want unknown/multiple_fetch_calls", p.Status, p.Reason)
	}
	if len(p.Candidates) != 2 || p.Candidates[0].Remote.Repo != "example/first" || p.Candidates[1].Remote.Repo != "example/second" {
		t.Fatalf("candidates = %+v, want both unbound calls", p.Candidates)
	}
}

func TestBracketCommentDecoyFetchIsInert(t *testing.T) {
	dir := newPort(t, "commented", `
#[[
vcpkg_from_github(REPO decoy/never REF `+commitA+` SHA512 0)
]]
vcpkg_from_github(REPO real/source REF `+commitB+` SHA512 0)
`)
	remote := "https://github.com/real/source.git"
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitB}}, nil), Now: fixedNow()}).Ports[0]
	if p.Status != evidence.StatusOK || p.Remote.Repo != "real/source" {
		t.Fatalf("status=%v reason=%v remote=%+v, bracket-comment decoy was not inert", p.Status, p.Reason, p.Remote)
	}
}

func TestEqualsBracketArgumentRetainsLiteralDoubleClose(t *testing.T) {
	dir := newPort(t, "bracket", `
set(VERSION "must-not-expand")
vcpkg_from_github(REPO example/bracket REF [=[${VERSION}]]literal]=] SHA512 0)
`)
	ref := "${VERSION}]]literal"
	remote := "https://github.com/example/bracket.git"
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"refs/tags/" + ref: commitA}}, nil), Now: fixedNow()}).Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonNamedRefNotComparable || p.Pin.Ref != ref {
		t.Fatalf("status=%v reason=%v pin=%+v, want unknown named ref with bracket literal %q", p.Status, p.Reason, p.Pin, ref)
	}
}

func TestQuotedArgumentContinuationJoinsLines(t *testing.T) {
	dir := newPort(t, "continued", "\nvcpkg_from_github(REPO example/continued REF \"v1.\\\n2.3\" SHA512 0)\n")
	remote := "https://github.com/example/continued.git"
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"refs/tags/v1.2.3": commitA}}, nil), Now: fixedNow()}).Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonNamedRefNotComparable || p.Pin.Ref != "v1.2.3" {
		t.Fatalf("status=%v reason=%v pin=%+v, want unknown named ref with continued quoted REF", p.Status, p.Reason, p.Pin)
	}
}

func TestCallScannerKeepsCloseParenInsideQuotedRef(t *testing.T) {
	parsed, ok := parsePortfile(`vcpkg_from_github(REPO example/quoted REF "release)candidate" SHA512 0)`)
	if !ok || parsed.Pin.Ref != "release)candidate" {
		t.Fatalf("ok=%v pin=%+v, want quoted REF with literal close paren", ok, parsed.Pin)
	}
}

func TestCallScannerKeepsCloseParenInsideBracketArgument(t *testing.T) {
	parsed, ok := parsePortfile(`vcpkg_from_github(REPO example/bracket REF [[release)candidate]] SHA512 0)`)
	if !ok || parsed.Pin.Ref != "release)candidate" {
		t.Fatalf("ok=%v pin=%+v, want bracket REF with literal close paren", ok, parsed.Pin)
	}
}

func TestVcpkgFromGitLab_AllThreeFieldsVariables(t *testing.T) {
	dir := newPort(t, "vtklike", `
set(MY_GITLAB_URL "https://gitlab.example.com")
set(MY_REPO "group/project")
set(MY_REF "`+commitD+`")
vcpkg_from_gitlab(
    GITLAB_URL ${MY_GITLAB_URL}
    REPO ${MY_REPO}
    REF ${MY_REF}
)
`)
	remote := "https://gitlab.example.com/group/project.git"
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitD}}, nil),
		Now:        fixedNow(),
	}

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps)
	p := res.Ports[0]
	if p.Remote.Kind != RemoteGitLab {
		t.Fatalf("remote.kind = %v, want gitlab", p.Remote.Kind)
	}
	if p.Remote.Repo != "group/project" || p.Remote.URL != remote {
		t.Fatalf("remote = %+v", p.Remote)
	}
	if p.Pin.Shape != RefShapeVariableResolved || p.Pin.ResolvedRef != commitD {
		t.Fatalf("pin = %+v", p.Pin)
	}
	if p.Status != evidence.StatusOK {
		t.Fatalf("status = %v reason = %v, want ok", p.Status, p.Reason)
	}
}

func TestDistfileOnly_UnknownNotGitComparable(t *testing.T) {
	dir := newPort(t, "distfileport", `
vcpkg_download_distfile(ARCHIVE
    URLS "https://example.com/foo-1.0.tar.gz"
    FILENAME "foo-1.0.tar.gz"
    SHA512 0
)
`)
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(nil, nil), // must never be called
		Now:        fixedNow(),
	}

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps)
	p := res.Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonNotGitComparable {
		t.Fatalf("status=%v reason=%v, want unknown/not_git_comparable", p.Status, p.Reason)
	}
	if p.Remote.Kind != RemoteDistfile {
		t.Fatalf("remote.kind = %v, want distfile", p.Remote.Kind)
	}
}

func TestMetapackage_FetchesNothing_UnknownNotGitComparable(t *testing.T) {
	dir := newPort(t, "metapkg", `
# This is a provider/metapackage port: it fetches nothing of its own.
set(VCPKG_POLICY_EMPTY_PACKAGE enabled)
`)
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(nil, nil), // must never be called
		Now:        fixedNow(),
	}

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps)
	p := res.Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonNotGitComparable {
		t.Fatalf("status=%v reason=%v, want unknown/not_git_comparable", p.Status, p.Reason)
	}
	if p.Remote.Kind != RemoteNone {
		t.Fatalf("remote.kind = %v, want none", p.Remote.Kind)
	}
}

func TestRemoteQueryError_UnknownRemoteQueryFailed_NeverCurrent(t *testing.T) {
	dir := newPort(t, "json", `
vcpkg_from_github(
    REPO nlohmann/json
    REF `+commitA+`
    SHA512 0
)
`)
	remote := "https://github.com/nlohmann/json.git"
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(nil, map[string]error{remote: errors.New("network unreachable (fake)")}),
		Now:        fixedNow(),
	}

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps)
	p := res.Ports[0]
	if p.Status == evidence.StatusOK {
		t.Fatalf("status = ok, must NEVER be ok when the remote query itself failed")
	}
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonRemoteQueryFailed {
		t.Fatalf("status=%v reason=%v, want unknown/remote_query_failed", p.Status, p.Reason)
	}
}

func TestNetworkDisabled_EveryPortReturnsUnknownNetworkDisabled(t *testing.T) {
	dir := newPort(t, "json", `
vcpkg_from_github(
    REPO nlohmann/json
    REF `+commitA+`
    SHA512 0
)
`)
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(nil, nil), // must never be called
		Now:        fixedNow(),
	}

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}, DisableNetwork: true}, deps)
	p := res.Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonNetworkDisabled {
		t.Fatalf("status=%v reason=%v, want unknown/network_disabled", p.Status, p.Reason)
	}
}

func TestNetworkDisabled_FiresEvenForAPortDirThatDoesNotExist(t *testing.T) {
	// DisableNetwork is checked first and unconditionally, before the
	// portfile is even read — a nonexistent port dir still gets
	// network_disabled, not portfile_unparsable, proving the check really
	// is unconditional per the input contract ("every port returns
	// unknown(network_disabled)").
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(nil, nil),
		Now:        fixedNow(),
	}
	res := PinStatus(context.Background(), Args{PortDirs: []string{filepath.Join(t.TempDir(), "does-not-exist")}, DisableNetwork: true}, deps)
	p := res.Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonNetworkDisabled {
		t.Fatalf("status=%v reason=%v, want unknown/network_disabled", p.Status, p.Reason)
	}
}

func TestMissingPortfile_UnknownPortfileUnparsable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ghost-port")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	deps := Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(nil, nil), Now: fixedNow()}

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps)
	p := res.Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonPortfileUnparsable {
		t.Fatalf("status=%v reason=%v, want unknown/portfile_unparsable", p.Status, p.Reason)
	}
}

func TestGitHubTagRef_FoundOnRemote_UnknownNamedRefNotComparable(t *testing.T) {
	dir := newPort(t, "taggedlib", `
vcpkg_from_github(
    REPO example/taggedlib
    REF v1.2.3
    SHA512 0
)
`)
	remote := "https://github.com/example/taggedlib.git"
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"refs/tags/v1.2.3": commitA}}, nil),
		Now:        fixedNow(),
	}

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps)
	p := res.Ports[0]
	if p.Pin.Shape != RefShapeTag {
		t.Fatalf("pin.shape = %v, want tag", p.Pin.Shape)
	}
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonNamedRefNotComparable {
		t.Fatalf("status = %v reason = %v, want unknown/named_ref_not_comparable", p.Status, p.Reason)
	}
	if got := namedRefEvidence(t, p); got.ref != "v1.2.3" || got.sha != commitA {
		t.Fatalf("named ref = %q sha=%q, want v1.2.3/%q", got.ref, got.sha, commitA)
	}
}

func TestNamedRefsFoundOnRemoteAreUnknownWithTheirObservedCommit(t *testing.T) {
	tests := []struct {
		name, ref, remoteRef string
	}{
		{name: "tag", ref: "v1.2.3", remoteRef: "refs/tags/v1.2.3"},
		{name: "branch", ref: "release", remoteRef: "refs/heads/release"},
		{name: "resolved variable tag", ref: "${RELEASE}", remoteRef: "refs/tags/v9.8.7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prefix := ""
			wantRef := tc.ref
			if tc.ref == "${RELEASE}" {
				prefix = "set(RELEASE v9.8.7)\n"
				wantRef = "v9.8.7"
			}
			dir := newPort(t, tc.name, prefix+"vcpkg_from_github(REPO example/"+strings.ReplaceAll(tc.name, " ", "-")+" REF "+tc.ref+" SHA512 0)")
			remote := "https://github.com/example/" + strings.ReplaceAll(tc.name, " ", "-") + ".git"
			p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {tc.remoteRef: commitB}}, nil), Now: fixedNow()}).Ports[0]
			if p.Status != evidence.StatusUnknown || p.Reason != ReasonNamedRefNotComparable {
				t.Fatalf("status=%v reason=%v, want unknown/named_ref_not_comparable", p.Status, p.Reason)
			}
			if got := namedRefEvidence(t, p); got.ref != wantRef || got.sha != commitB {
				t.Fatalf("named ref=%q sha=%q, want %q/%q", got.ref, got.sha, wantRef, commitB)
			}
		})
	}
}

func TestGitHubBranchRef_NotFoundOnRemote_UnknownRefNotFound(t *testing.T) {
	dir := newPort(t, "branchlib", `
vcpkg_from_github(
    REPO example/branchlib
    REF feature-branch
    SHA512 0
)
`)
	remote := "https://github.com/example/branchlib.git"
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"refs/heads/main": commitA}}, nil),
		Now:        fixedNow(),
	}

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps)
	p := res.Ports[0]
	if p.Pin.Shape != RefShapeBranch {
		t.Fatalf("pin.shape = %v, want branch", p.Pin.Shape)
	}
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonRefNotFoundOnRemote {
		t.Fatalf("status=%v reason=%v, want unknown/ref_not_found_on_remote", p.Status, p.Reason)
	}
}

func TestUnresolvableVariableRef_UnknownRefUnresolvable(t *testing.T) {
	dir := newPort(t, "unresolvable", `
vcpkg_from_github(
    REPO example/unresolvable
    REF ${NEVER_DEFINED}
    SHA512 0
)
`)
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(nil, nil), // must never be called: resolution fails before any query
		Now:        fixedNow(),
	}

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps)
	p := res.Ports[0]
	if p.Pin.Shape != RefShapeVariableResolved || p.Pin.ResolvedRef != "" {
		t.Fatalf("pin = %+v, want variable_resolved with empty resolved_ref", p.Pin)
	}
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonRefUnresolvable {
		t.Fatalf("status=%v reason=%v, want unknown/ref_unresolvable", p.Status, p.Reason)
	}
}

func TestUnresolvedHeadRefDoesNotFallBackToRemoteHEAD(t *testing.T) {
	dir := newPort(t, "unresolved-head-ref", `
vcpkg_from_github(REPO example/head-ref REF `+commitA+` HEAD_REF ${MISSING_HEAD_REF} SHA512 0)
`)
	remote := "https://github.com/example/head-ref.git"
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitA}}, nil), Now: fixedNow()}).Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonHeadRefUnresolvable {
		t.Fatalf("status=%v reason=%v, want unknown/head_ref_unresolvable", p.Status, p.Reason)
	}
	if got := unresolvedHeadRefVariable(t, p); got != "MISSING_HEAD_REF" {
		t.Fatalf("unresolved head ref variable=%q, want MISSING_HEAD_REF", got)
	}
}

func TestAbsentHeadRefStillUsesRemoteHEAD(t *testing.T) {
	dir := newPort(t, "absent-head-ref", `vcpkg_from_github(REPO example/absent-head REF `+commitA+` SHA512 0)`)
	remote := "https://github.com/example/absent-head.git"
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitA}}, nil), Now: fixedNow()}).Ports[0]
	if p.Status != evidence.StatusOK || p.TrackedRef != "HEAD" {
		t.Fatalf("status=%v reason=%v tracked=%q, want current against HEAD", p.Status, p.Reason, p.TrackedRef)
	}
}

// F23: an empty batch must be REFUSED with the tool-level tri-state, not
// answered with {"ports":[]} — which is byte-identical to a successful
// zero-work call and therefore indistinguishable from "all checked, all
// fine". Ports still marshals as [] rather than null so the shape is stable.
func TestF23_EmptyBatchIsRefusedWithATopLevelTriState(t *testing.T) {
	res := PinStatus(context.Background(), Args{}, Deps{Now: fixedNow()})

	if res.Status != evidence.StatusUnknown || res.Reason != BatchReasonNoPortDirs {
		t.Fatalf("status=%v reason=%v, want unknown/no_port_dirs", res.Status, res.Reason)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(data) != `{"status":"unknown","reason":"no_port_dirs","ports":[]}` {
		t.Fatalf("empty result JSON = %s, want the tri-state form with ports as an array", data)
	}
}

// F23 (companion): a batch that DID run reports ok at the tool level even
// when individual ports are unknown — the two levels answer different
// questions and must not be conflated.
func TestF23_NonEmptyBatchReportsOKAtTheToolLevel(t *testing.T) {
	dir := newPort(t, "dist-only", `vcpkg_download_distfile(A URLS "https://x/y.tar.gz" FILENAME "y.tar.gz" SHA512 0)`)
	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(nil, nil), Now: fixedNow()})

	if res.Status != evidence.StatusOK || res.Reason != "" {
		t.Fatalf("batch status=%v reason=%v, want ok with no batch reason", res.Status, res.Reason)
	}
	if len(res.Ports) != 1 || res.Ports[0].Status != evidence.StatusUnknown {
		t.Fatalf("ports = %+v, want one unknown port under an ok batch", res.Ports)
	}
}

type namedRefObservation struct{ ref, sha string }

func namedRefEvidence(t *testing.T, p PortResult) namedRefObservation {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	ref, _ := raw["named_ref"].(string)
	sha, _ := raw["named_ref_sha"].(string)
	return namedRefObservation{ref: ref, sha: sha}
}

func unresolvedHeadRefVariable(t *testing.T, p PortResult) string {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	value, _ := raw["unresolved_head_ref_variable"].(string)
	return value
}

// --- the hard limit: no "behind" verdict anywhere ---------------------------

// TestNoCodePathProducesBehind is a static assertion over the CLOSED enums:
// since Status and Reason are the only vocabulary any code path in this
// package can assign (see pinstatus.go), proving no declared constant is or
// contains "behind" proves no code path can ever produce it.
//
// VACUOUS-TEST FIX (2026-07-27): that argument only holds if the set really is
// the closed enum, and it was not. The test iterated a HAND-MAINTAINED list of
// 11 Reason literals while types.go declares 16 — commit_pin_abbreviated,
// remote_query_timeout, remote_query_canceled, remote_ref_limit and
// remote_url_credential_bearing were never checked, and a newly added reason
// would simply be absent from the list too. Comparing literals against a
// literal is also a tautology: nothing in the package is exercised.
//
// The set is now DERIVED from types.go, so the closure claim is one the test
// actually establishes and a new member is covered the moment it is declared.
// The count guard is what stops the derivation from silently matching nothing
// and turning the loop back into a no-op.
func TestNoCodePathProducesBehind(t *testing.T) {
	src, err := os.ReadFile("types.go")
	if err != nil {
		t.Fatalf("read the enum declaration: %v", err)
	}
	// Matches the `ReasonX Reason = "value"` / `BatchReasonX BatchReason =
	// "value"` const declarations, capturing the wire value.
	declRE := regexp.MustCompile(`(?m)^\s*\w+\s+(?:Batch)?Reason\s*=\s*"([a-z0-9_]+)"`)
	matches := declRE.FindAllStringSubmatch(string(src), -1)

	// Guard the instrument before trusting it: if the regexp ever stops
	// matching (a rename, a reformat, a move to another file), an empty set
	// would make every assertion below pass without checking anything.
	const minKnownReasons = 16
	if len(matches) < minKnownReasons {
		t.Fatalf("derived only %d reason constants from types.go, want at least %d — the extraction is broken, so "+
			"this test would otherwise pass over an empty set", len(matches), minKnownReasons)
	}

	for _, m := range matches {
		if strings.Contains(strings.ToLower(m[1]), "behind") {
			t.Fatalf("reason %q must never be/contain %q — this package cannot prove a differing pin is BEHIND "+
				"rather than diverged or rebased away", m[1], "behind")
		}
	}
	statuses := []Status{evidence.StatusOK, evidence.StatusFailed, evidence.StatusUnknown}
	for _, s := range statuses {
		if strings.Contains(strings.ToLower(string(s)), "behind") {
			t.Fatalf("status %q must never be/contain %q", s, "behind")
		}
	}
}

// TestNoCodePathProducesBehind_RuntimeSweep runs every branch this package
// has (equal commit, diverged commit, variable-resolved git/gitlab, tag
// found, branch not found, distfile, metapackage, query error, network
// disabled, unresolvable variable) in one batch and greps the whole
// marshaled Result for "behind" — a second, runtime-level net alongside the
// static enum check above.
func TestNoCodePathProducesBehind_RuntimeSweep(t *testing.T) {
	ghEqual := newPort(t, "gh-equal", `vcpkg_from_github(REPO a/b REF `+commitA+` SHA512 0)`)
	ghDiff := newPort(t, "gh-diff", `vcpkg_from_github(REPO a/b REF `+commitB+` SHA512 0)`)
	gitVar := newPort(t, "git-var", `set(MY_REF "`+commitC+`")
vcpkg_from_git(URL https://example.com/x/y.git REF ${MY_REF})`)
	distfile := newPort(t, "dist", `vcpkg_download_distfile(A URLS "https://x/y.tar.gz" FILENAME "y.tar.gz" SHA512 0)`)
	meta := newPort(t, "meta", `set(VCPKG_POLICY_EMPTY_PACKAGE enabled)`)
	tagFound := newPort(t, "tag-found", `vcpkg_from_github(REPO a/tagged REF v2.0.0 SHA512 0)`)
	branchMissing := newPort(t, "branch-missing", `vcpkg_from_github(REPO a/branched REF gone SHA512 0)`)
	unresolvable := newPort(t, "unresolvable2", `vcpkg_from_github(REPO a/unresolvable REF ${NOPE} SHA512 0)`)
	unresolvedHeadRef := newPort(t, "unresolved-head-ref", `vcpkg_from_github(REPO a/unresolved-head REF `+commitA+` HEAD_REF ${NOPE} SHA512 0)`)
	queryErr := newPort(t, "query-err", `vcpkg_from_github(REPO a/errors REF `+commitA+` SHA512 0)`)
	guardUnresolvable := newPort(t, "guard-unresolvable", `if(PORT_SWITCH)
vcpkg_from_github(REPO a/first REF `+commitA+` SHA512 0)
else()
vcpkg_from_github(REPO a/second REF `+commitA+` SHA512 0)
endif()`)
	multipleFetch := newPort(t, "multiple-fetch", `vcpkg_from_github(REPO a/one REF `+commitA+` SHA512 0)
vcpkg_from_github(REPO a/two REF `+commitB+` SHA512 0)`)

	refs := map[string]map[string]string{
		"https://github.com/a/b.git":               {"HEAD": commitA},
		"https://example.com/x/y.git":              {"HEAD": commitC},
		"https://github.com/a/tagged.git":          {"refs/tags/v2.0.0": commitA},
		"https://github.com/a/branched.git":        {"refs/heads/main": commitA},
		"https://github.com/a/unresolved-head.git": {"HEAD": commitA},
	}
	errs := map[string]error{
		"https://github.com/a/errors.git": errors.New("fake network error"),
	}
	deps := Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(refs, errs), Now: fixedNow()}

	res := PinStatus(context.Background(), Args{PortDirs: []string{
		ghEqual, ghDiff, gitVar, distfile, meta, tagFound, branchMissing, unresolvable, unresolvedHeadRef, queryErr, guardUnresolvable, multipleFetch,
	}}, deps)

	if len(res.Ports) != 12 {
		t.Fatalf("got %d port results, want 12", len(res.Ports))
	}

	// Scrub harness-only fields before the substring scan: PortDir and
	// Evidence embed this test's own t.TempDir() path, which contains this
	// subtest's name ("...RuntimeSweep") — that name is test-harness noise,
	// not tool-produced verdict content, and must not produce a false
	// positive against the "never behind" assertion this test exists to
	// check. Every remaining field (status/reason/remote/pin/SHAs/compare
	// URL) is genuine tool output.
	scrubbed := Result{Ports: append([]PortResult(nil), res.Ports...)}
	for i := range scrubbed.Ports {
		scrubbed.Ports[i].PortDir = ""
		scrubbed.Ports[i].Evidence = evidence.Evidence{}
	}
	data, err := json.Marshal(scrubbed)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(strings.ToLower(string(data)), "behind") {
		t.Fatalf("marshaled Result contains %q:\n%s", "behind", data)
	}

	// Sanity: the diverged-commit port really did take the pin_not_at_tip
	// path (proving this sweep actually exercises the comparison branch the
	// "never behind" assertion is protecting, not just skipping past it).
	found, foundGuard, foundMultiple, foundUnresolvedHeadRef := false, false, false, false
	for _, p := range res.Ports {
		if p.PortDir == ghDiff {
			found = true
			if p.Reason != ReasonPinNotAtTip {
				t.Fatalf("ghDiff reason = %v, want pin_not_at_tip", p.Reason)
			}
		}
		if p.PortDir == guardUnresolvable && p.Reason == ReasonGuardUnresolvable {
			foundGuard = true
		}
		if p.PortDir == multipleFetch && p.Reason == ReasonMultipleFetchCalls {
			foundMultiple = true
		}
		if p.PortDir == unresolvedHeadRef && p.Reason == ReasonHeadRefUnresolvable {
			foundUnresolvedHeadRef = true
		}
	}
	if !found {
		t.Fatalf("ghDiff result missing from batch")
	}
	if !foundGuard || !foundMultiple || !foundUnresolvedHeadRef {
		t.Fatalf("runtime sweep missed unknown branches: guard=%v multiple=%v unresolvedHeadRef=%v", foundGuard, foundMultiple, foundUnresolvedHeadRef)
	}
}

// --- batch semantics ---------------------------------------------------------

func TestPinStatus_BatchOrderAndIndependence(t *testing.T) {
	ok := newPort(t, "ok", `vcpkg_from_github(REPO a/b REF `+commitA+` SHA512 0)`)
	broken := filepath.Join(t.TempDir(), "broken") // no portfile.cmake at all
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{"https://github.com/a/b.git": {"HEAD": commitA}}, nil),
		Now:        fixedNow(),
	}
	res := PinStatus(context.Background(), Args{PortDirs: []string{ok, broken}}, deps)
	if len(res.Ports) != 2 {
		t.Fatalf("got %d ports, want 2", len(res.Ports))
	}
	if res.Ports[0].PortDir != ok || res.Ports[0].Status != evidence.StatusOK {
		t.Fatalf("ports[0] = %+v", res.Ports[0])
	}
	if res.Ports[1].PortDir != broken || res.Ports[1].Reason != ReasonPortfileUnparsable {
		t.Fatalf("ports[1] = %+v", res.Ports[1])
	}
}

// =====================================================================
// Pre-submission cross-family review, round 2 (F8/F24/F25).
// =====================================================================

// credentialPortfile builds a port whose vcpkg_from_git URL embeds a
// credential — valid CMake, and exactly the shape that used to be copied
// verbatim into the result, the evidence and the compare URL.
const credentialRemote = "https://user:s3cr3t-token@example.invalid/org/repo.git"

// F24: the credential must not appear ANYWHERE in the marshaled result. The
// assertion is deliberately a whole-document scan rather than a per-field
// check: the whole point of the finding is that patching the two named
// emission sites leaves the others (Candidates[].Remote.URL, CompareURL)
// still leaking.
func TestF24_CredentialNeverAppearsAnywhereInTheMarshaledResult(t *testing.T) {
	dir := newPort(t, "cred", `vcpkg_from_git(
    OUT_SOURCE_PATH SOURCE_PATH
    URL `+credentialRemote+`
    REF `+commitA+`
)`)

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}},
		Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(nil, nil), Now: fixedNow()})

	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	doc := string(data)
	for _, secret := range []string{"s3cr3t-token", "user:s3cr3t-token"} {
		if strings.Contains(doc, secret) {
			t.Fatalf("credential %q leaked into the MCP result document: %s", secret, doc)
		}
	}
	if !strings.Contains(doc, "REDACTED") {
		t.Fatalf("result does not show that a credential was present at all; redaction must be visible, not silent: %s", doc)
	}
}

// F24: the query itself is refused. Redaction cannot reach argv — a
// credential handed to `git ls-remote` is readable by every local account
// for the lifetime of the child process.
func TestF24_CredentialBearingRemoteIsRefusedNotQueried(t *testing.T) {
	dir := newPort(t, "cred-refused", `vcpkg_from_git(
    OUT_SOURCE_PATH SOURCE_PATH
    URL `+credentialRemote+`
    REF `+commitA+`
)`)

	var queried []string
	spy := func(ctx context.Context, remote string) (map[string]string, error) {
		queried = append(queried, remote)
		return map[string]string{"HEAD": commitA}, nil
	}

	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}},
		Deps{FS: DefaultFS(), RemoteRefs: spy, Now: fixedNow()}).Ports[0]

	if len(queried) != 0 {
		t.Fatalf("the credential-bearing remote was handed to the query function (%v); it must be refused "+
			"before the child process is ever built", queried)
	}
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonRemoteURLCredentialBearing {
		t.Fatalf("status=%v reason=%v, want unknown/remote_url_credential_bearing", p.Status, p.Reason)
	}
}

// F24: a NON-selected candidate can carry a credential too. This is the
// emission point a per-site patch misses entirely, because the selected
// remote is clean and nothing about the happy path looks wrong.
func TestF24_CredentialOnANonSelectedCandidateIsAlsoRedacted(t *testing.T) {
	dir := newPort(t, "cred-candidate", `if(SOME_UNSET_OPTION)
vcpkg_from_git(OUT_SOURCE_PATH SOURCE_PATH URL `+credentialRemote+` REF `+commitA+`)
else()
vcpkg_from_github(OUT_SOURCE_PATH SOURCE_PATH REPO clean/repo REF `+commitA+` SHA512 0)
endif()`)

	res := PinStatus(context.Background(), Args{PortDirs: []string{dir}},
		Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{
			"https://github.com/clean/repo.git": {"HEAD": commitA},
		}, nil), Now: fixedNow()})

	data, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(data), "s3cr3t-token") {
		t.Fatalf("credential leaked via an audited but NON-selected candidate: %s", data)
	}
}

// F24: the redactor itself, exercised directly over the shapes it must
// survive — including ones url.Parse rejects, where failing to parse must
// never mean failing to redact.
func TestF24_RedactURLCoversUserinfoQueryParamsAndUnparsableInput(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		secret  string
		wantSub string
	}{
		{"user and password", "https://alice:hunter2@host/r.git", "hunter2", "REDACTED@host/r.git"},
		{"bare token as username", "https://ghp_abc123@host/r.git", "ghp_abc123", "REDACTED@host/r.git"},
		{"secret query parameter", "https://host/r.git?access_token=abc123", "abc123", "access_token=REDACTED"},
		{"unparsable authority", "https://bob:pw@ho st/r.git", "pw", "REDACTED@"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactURL(tc.in)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("redactURL(%q) = %q, still contains %q", tc.in, got, tc.secret)
			}
			if !strings.Contains(got, tc.wantSub) {
				t.Fatalf("redactURL(%q) = %q, want it to contain %q", tc.in, got, tc.wantSub)
			}
			if !hasEmbeddedCredential(tc.in) {
				t.Fatalf("hasEmbeddedCredential(%q) = false, want true", tc.in)
			}
		})
	}
}

// F24: a clean URL must round-trip BYTE-FOR-BYTE. An over-eager redactor
// that re-encodes every URL would break copy-pasteability and churn results.
func TestF24_CleanURLRoundTripsUnchanged(t *testing.T) {
	for _, clean := range []string{
		"https://github.com/nlohmann/json.git",
		"https://gitlab.com/group/sub/proj.git",
		"git@github.com:owner/repo.git",
		"",
	} {
		if got := redactURL(clean); got != clean {
			t.Fatalf("redactURL(%q) = %q, want it unchanged", clean, got)
		}
		if hasEmbeddedCredential(clean) {
			t.Fatalf("hasEmbeddedCredential(%q) = true, want false", clean)
		}
	}
}

// F8: the request context must reach the query function. Discarding it for
// context.Background() meant an MCP cancellation left the git child running
// and the request pinned open.
func TestF8_RequestContextReachesTheRemoteQuery(t *testing.T) {
	dir := newPort(t, "ctx-threaded", `vcpkg_from_github(REPO a/b REF `+commitA+` SHA512 0)`)

	type ctxKey string
	const key ctxKey = "probe"
	parent := context.WithValue(context.Background(), key, "carried")

	var sawValue any
	spy := func(ctx context.Context, remote string) (map[string]string, error) {
		sawValue = ctx.Value(key)
		return map[string]string{"HEAD": commitA}, nil
	}

	PinStatus(parent, Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: spy, Now: fixedNow()})

	if sawValue != "carried" {
		t.Fatalf("query function received ctx.Value = %v, want %q — the CALLER's context must be threaded "+
			"through (a context.Background() substitute carries nothing)", sawValue, "carried")
	}
}

// F8: an already-canceled request stops the batch instead of firing a git
// subprocess per port, and reports the honest closed reason.
func TestF8_CanceledRequestStopsTheBatchAndNeverQueries(t *testing.T) {
	a := newPort(t, "cancel-a", `vcpkg_from_github(REPO a/b REF `+commitA+` SHA512 0)`)
	b := newPort(t, "cancel-b", `vcpkg_from_github(REPO c/d REF `+commitA+` SHA512 0)`)

	var queries int
	spy := func(ctx context.Context, remote string) (map[string]string, error) {
		queries++
		return map[string]string{"HEAD": commitA}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := PinStatus(ctx, Args{PortDirs: []string{a, b}}, Deps{FS: DefaultFS(), RemoteRefs: spy, Now: fixedNow()})

	if queries != 0 {
		t.Fatalf("%d remote queries fired under a canceled context, want 0", queries)
	}
	if len(res.Ports) != 2 {
		t.Fatalf("ports = %d, want 2 (every requested port still gets an explicit verdict)", len(res.Ports))
	}
	for _, p := range res.Ports {
		if p.Status != evidence.StatusUnknown || p.Reason != ReasonRemoteQueryCanceled {
			t.Fatalf("port %s = %v/%v, want unknown/remote_query_canceled", p.PortDir, p.Status, p.Reason)
		}
	}
}

// F8: a deadline and a caller cancellation are DIFFERENT facts and get
// different closed reasons — an operator acts on them differently.
func TestF8_TimeoutAndCancellationAreDistinctReasons(t *testing.T) {
	dir := newPort(t, "deadline", `vcpkg_from_github(REPO a/b REF `+commitA+` SHA512 0)`)

	deadlineDeps := Deps{FS: DefaultFS(), Now: fixedNow(), RemoteRefs: func(ctx context.Context, remote string) (map[string]string, error) {
		return nil, context.DeadlineExceeded
	}}
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deadlineDeps).Ports[0]
	if p.Reason != ReasonRemoteQueryTimeout {
		t.Fatalf("reason = %v, want remote_query_timeout", p.Reason)
	}

	canceledDeps := Deps{FS: DefaultFS(), Now: fixedNow(), RemoteRefs: func(ctx context.Context, remote string) (map[string]string, error) {
		return nil, context.Canceled
	}}
	p2 := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, canceledDeps).Ports[0]
	if p2.Reason != ReasonRemoteQueryCanceled {
		t.Fatalf("reason = %v, want remote_query_canceled", p2.Reason)
	}
}

// F25: a remote advertising an enormous ref set must trip a bound and report
// a closed resource-limit reason, NOT be indexed into an unbounded map.
func TestF25_RefCountCeilingTripsAndSurfacesAClosedReason(t *testing.T) {
	var payload strings.Builder
	for i := 0; i <= MaxRemoteRefs; i++ {
		fmt.Fprintf(&payload, "%s\trefs/heads/b%d\n", commitA, i)
	}

	_, err := parseLsRemoteStream(strings.NewReader(payload.String()))
	if !errors.Is(err, ErrRemoteRefLimit) {
		t.Fatalf("err = %v, want ErrRemoteRefLimit for a ref set above MaxRemoteRefs=%d", err, MaxRemoteRefs)
	}

	dir := newPort(t, "hostile-remote", `vcpkg_from_github(REPO a/b REF `+commitA+` SHA512 0)`)
	deps := Deps{FS: DefaultFS(), Now: fixedNow(), RemoteRefs: func(ctx context.Context, remote string) (map[string]string, error) {
		return nil, err
	}}
	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps).Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonRemoteRefLimit {
		t.Fatalf("status=%v reason=%v, want unknown/remote_ref_limit", p.Status, p.Reason)
	}
}

// F25: one absurdly long line must be refused rather than buffered whole. A
// truncated half-ref would be worse than an error — it could silently turn
// "this tag exists" into "this tag is gone".
func TestF25_OverlongRefLineIsRefusedNotBuffered(t *testing.T) {
	long := commitA + "\trefs/heads/" + strings.Repeat("x", MaxRemoteRefLineBytes+1) + "\n"

	_, err := parseLsRemoteStream(strings.NewReader(long))
	if !errors.Is(err, ErrRemoteRefLimit) {
		t.Fatalf("err = %v, want ErrRemoteRefLimit for a line above MaxRemoteRefLineBytes=%d", err, MaxRemoteRefLineBytes)
	}
}

// F25 (companion): an ordinary advertisement still parses exactly as before,
// including the peeled-annotated-tag form. The ceilings must not change the
// answer for real input.
func TestF25_NormalAdvertisementStillParsesUnchanged(t *testing.T) {
	payload := commitA + "\tHEAD\n" +
		commitA + "\trefs/heads/main\n" +
		commitB + "\trefs/tags/v1.0\n" +
		commitC + "\trefs/tags/v1.0^{}\n"

	refs, err := parseLsRemoteStream(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("parseLsRemoteStream: %v", err)
	}
	want := map[string]string{
		"HEAD":              commitA,
		"refs/heads/main":   commitA,
		"refs/tags/v1.0":    commitB,
		"refs/tags/v1.0^{}": commitC,
	}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %+v, want %+v", refs, want)
	}
}

// F24 (defence in depth): the gitlab CompareURL is built by string-editing
// the remote URL, so it is a THIRD place a credential could re-enter the
// result. End-to-end this path is currently unreachable for a
// credential-bearing remote — the refusal in pinStatusOne fires first — so
// this asserts the WIRING directly instead of pretending an end-to-end
// fixture exercises it. If the refusal is ever relaxed, this is the guard
// that keeps the compare link clean.
func TestF24_CompareURLIsBuiltFromTheRedactedRemoteNotTheRawOne(t *testing.T) {
	raw := Remote{Kind: RemoteGitLab, Repo: "group/proj", URL: "https://user:s3cr3t-token@gitlab.example/group/proj.git"}

	leaky := buildCompareURL(raw, commitA, commitB)
	if !strings.Contains(leaky, "s3cr3t-token") {
		t.Fatalf("fixture no longer demonstrates the hazard: buildCompareURL over a RAW remote = %q", leaky)
	}

	safe := buildCompareURL(redactRemote(raw), commitA, commitB)
	if strings.Contains(safe, "s3cr3t-token") {
		t.Fatalf("compare URL built from the redacted remote still leaks the credential: %q", safe)
	}
	if !strings.Contains(safe, "/-/compare/"+commitA+"..."+commitB) {
		t.Fatalf("redaction broke the compare link shape: %q", safe)
	}
}

// VACUOUS-TEST FIX (2026-07-27): TestF24_CompareURLDerivesFromTheEmittedRemote
// was DELETED here rather than repaired.
//
// It claimed to pin that "whatever CompareURL a real call produces is derived
// from the EMITTED (redacted) remote, never from a separately-held raw
// spelling", but its fixture remote was credential-free
// (https://gitlab.example/group/proj.git). redactURL returns the ORIGINAL
// spelling byte-for-byte when nothing changed, so p.Remote.URL and the raw URL
// were the SAME STRING and its
// strings.HasPrefix(p.CompareURL, TrimSuffix(p.Remote.URL, ".git")) assertion
// held identically whether production fed buildCompareURL the raw remote or the
// redacted one. It could not distinguish the two, which is the only thing its
// name promised.
//
// No end-to-end fixture can distinguish them either: the only remote for which
// redaction changes the string is one pinStatusOne refuses before CompareURL is
// ever built (hasEmbeddedCredential), which the sibling test's own comment
// records. That sibling —
// TestF24_CompareURLIsBuiltFromTheRedactedRemoteNotTheRawOne, immediately above
// — already asserts the wiring DIRECTLY with a credential-bearing remote, and
// verifies its own fixture still demonstrates the hazard before asserting. It
// is the binding version of the same claim, so nothing is lost.

// F25: the whole-output byte ceiling is the backstop for the case the LINE
// ceiling cannot catch — a remote streaming endlessly in lines that are each
// individually legal.
//
// VACUOUS-TEST FIX (2026-07-27): this test used to feed
// strings.Repeat("x", MaxRemoteRefLineBytes+1) — 8193 bytes with no newline —
// and assert only errors.Is(err, ErrRemoteRefLimit). That trips the LINE
// ceiling in readBoundedLine at 8 KiB; MaxRemoteOutputBytes is 32 MiB and its
// check was never reached. Both ceilings wrap the same sentinel, so the
// assertion could not tell them apart: deleting the whole
// `consumed > MaxRemoteOutputBytes` branch left the test green, and it was a
// byte-for-byte duplicate of TestF25_OverlongRefLineIsRefusedNotBuffered.
//
// The test's old premise was wrong in the same way: an unterminated stream is
// caught by the line ceiling precisely BECAUSE it has no newline. The output
// ceiling's real job is the opposite shape, which this now exercises — lines
// short enough to pass the line ceiling, and (being tab-free) not refs, so the
// MaxRemoteRefs ceiling is not reached either. The reader generates its bytes
// rather than materializing 32 MiB.
func TestF25_TotalOutputCeilingTripsOnManyIndividuallyLegalLines(t *testing.T) {
	const lineLen = 4096
	line := strings.Repeat("x", lineLen-1) + "\n" // no tab: not a ref, so refs stays empty

	if lineLen > MaxRemoteRefLineBytes {
		t.Fatalf("precondition failed: each line must PASS the line ceiling (%d), or this test just repeats the "+
			"line-ceiling test; lineLen=%d", MaxRemoteRefLineBytes, lineLen)
	}
	if wouldBeRefs := MaxRemoteOutputBytes/lineLen + 1; wouldBeRefs <= MaxRemoteRefs {
		// Not fatal — it only matters that these lines are not refs at all —
		// but state the reasoning so the fixture's shape is not accidental.
		t.Logf("note: %d lines are needed; they carry no tab so none is counted as a ref", wouldBeRefs)
	}

	_, err := parseLsRemoteStream(&repeatingReader{chunk: []byte(line)})
	if !errors.Is(err, ErrRemoteRefLimit) {
		t.Fatalf("err = %v, want ErrRemoteRefLimit — a remote streaming past the %d-byte whole-output ceiling in "+
			"individually legal lines must be refused, or it can size our memory without ever tripping the line cap",
			err, MaxRemoteOutputBytes)
	}
	if !strings.Contains(err.Error(), "output exceeded") {
		t.Fatalf("err = %v, want the OUTPUT ceiling (\"output exceeded\"); a line- or ref-ceiling error here means the "+
			"fixture is testing a different bound than the one this test is named for", err)
	}
}

// repeatingReader emits chunk forever, so a multi-megabyte ceiling can be
// exercised without allocating a multi-megabyte fixture.
type repeatingReader struct {
	chunk []byte
	off   int
}

func (r *repeatingReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.chunk[r.off:])
		n += c
		r.off += c
		if r.off >= len(r.chunk) {
			r.off = 0
		}
	}
	return n, nil
}

// =====================================================================
// FIELD P1 (operator, vcpkg-mcp 0.1.0, real vcpkg tree): a ref that
// CONTAINS a CMake variable was compared VERBATIM against the remote and
// reported as a definite negative. All three refs below exist upstream.
//
// Root cause: variableRefRE is anchored ^...$, so it matches only a ref
// that is ENTIRELY one variable ("${VTK_GIT_REF}" — the vtk case that
// correctly reported unresolvable) and never one that merely CONTAINS a
// variable, which fell through to the literal-token path.
//
// A confident wrong NEGATIVE is the worst output this contract can emit:
// it sends a maintainer to "fix" a pin that is already correct.
// =====================================================================

func TestFieldP1_RefContainingAVariableIsNeverReportedAsMissingUpstream(t *testing.T) {
	cases := []struct {
		port      string
		portfile  string
		manifest  string
		remoteURL string
		// remoteRefs is what the LIVE remote actually advertises.
		remoteRefs map[string]string
		wantRef    string
	}{
		{
			port:       "double-conversion",
			portfile:   "vcpkg_from_github(OUT_SOURCE_PATH SOURCE_PATH REPO google/double-conversion REF \"v${VERSION}\" SHA512 0)",
			manifest:   `{"name":"double-conversion","version":"3.4.0"}`,
			remoteURL:  "https://github.com/google/double-conversion.git",
			remoteRefs: map[string]string{"refs/tags/v3.4.0": commitA, "HEAD": commitB},
			wantRef:    "v3.4.0",
		},
		{
			port:       "libxcrypt",
			portfile:   "vcpkg_from_github(OUT_SOURCE_PATH SOURCE_PATH REPO besser82/libxcrypt REF \"v${VERSION}\" SHA512 0)",
			manifest:   `{"name":"libxcrypt","version":"4.5.2"}`,
			remoteURL:  "https://github.com/besser82/libxcrypt.git",
			remoteRefs: map[string]string{"refs/tags/v4.5.2": commitA, "HEAD": commitB},
			wantRef:    "v4.5.2",
		},
		{
			port:       "xorg-macros",
			portfile:   "vcpkg_from_git(OUT_SOURCE_PATH SOURCE_PATH URL https://gitlab.freedesktop.org/xorg/util/macros REF \"util-macros-${VERSION}\")",
			manifest:   `{"name":"xorg-macros","version":"1.20.2"}`,
			remoteURL:  "https://gitlab.freedesktop.org/xorg/util/macros",
			remoteRefs: map[string]string{"refs/tags/util-macros-1.20.2": commitA, "HEAD": commitB},
			wantRef:    "util-macros-1.20.2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.port, func(t *testing.T) {
			dir := newPort(t, tc.port, tc.portfile)
			writeManifest(t, dir, tc.manifest)

			p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
				FS:         DefaultFS(),
				RemoteRefs: fakeRemote(map[string]map[string]string{tc.remoteURL: tc.remoteRefs}, nil),
				Now:        fixedNow(),
			}).Ports[0]

			if p.Reason == ReasonRefNotFoundOnRemote {
				t.Fatalf("reported ref_not_found_on_remote for %q, but that ref EXISTS upstream — a "+
					"definite negative derived from an unexpanded variable is the worst possible output "+
					"(pin=%+v)", p.Pin.Ref, p.Pin)
			}
			if p.Pin.ResolvedRef != tc.wantRef {
				t.Fatalf("resolved_ref = %q, want %q expanded from the sibling vcpkg.json", p.Pin.ResolvedRef, tc.wantRef)
			}
			if p.Reason != ReasonNamedRefNotComparable {
				t.Fatalf("reason = %v, want named_ref_not_comparable (the tag exists; a tag pin still cannot "+
					"be compared for currency)", p.Reason)
			}
			if p.NamedRef != tc.wantRef || p.NamedRefSHA != commitA {
				t.Fatalf("named_ref/%s named_ref_sha/%s, want %s/%s", p.NamedRef, p.NamedRefSHA, tc.wantRef, commitA)
			}
		})
	}
}

// FIELD P1 (the FLOOR): even when expansion is impossible, the answer must
// never be a negative. This is the guarantee that does not depend on the
// expander understanding any particular variable.
func TestFieldP1_UnexpandableRefFallsBackToUnresolvableNeverToANegative(t *testing.T) {
	// No vcpkg.json, and no local set() — ${VERSION} cannot be resolved.
	dir := newPort(t, "no-manifest", "vcpkg_from_github(REPO a/b REF \"v${VERSION}\" SHA512 0)")

	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{"https://github.com/a/b.git": {"HEAD": commitA}}, nil),
		Now:        fixedNow(),
	}).Ports[0]

	if p.Reason == ReasonRefNotFoundOnRemote {
		t.Fatalf("an UNEXPANDABLE ref was reported as missing upstream; it must degrade to unresolvable: %+v", p.Pin)
	}
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonRefUnresolvable {
		t.Fatalf("status=%v reason=%v, want unknown/ref_unresolvable", p.Status, p.Reason)
	}
	if p.Pin.UnresolvedVariable != "VERSION" {
		t.Fatalf("unresolved_variable = %q, want %q", p.Pin.UnresolvedVariable, "VERSION")
	}
}

// FIELD P1: the vtk shape (the ref that is ENTIRELY one variable) must keep
// behaving exactly as it already did — that path was correct.
func TestFieldP1_WholeStringVariableRefStillReportsUnresolvable(t *testing.T) {
	dir := newPort(t, "vtk", "vcpkg_from_github(REPO Kitware/VTK REF ${VTK_GIT_REF} SHA512 0)")

	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{"https://github.com/Kitware/VTK.git": {"HEAD": commitA}}, nil),
		Now:        fixedNow(),
	}).Ports[0]

	if p.Status != evidence.StatusUnknown || p.Reason != ReasonRefUnresolvable {
		t.Fatalf("status=%v reason=%v, want unknown/ref_unresolvable (unchanged vtk behaviour)", p.Status, p.Reason)
	}
}

// FIELD P1: ${PORT} is the other ubiquitous vcpkg ref idiom.
func TestFieldP1_PortVariableExpandsFromThePortDirectoryName(t *testing.T) {
	dir := newPort(t, "mylib", "vcpkg_from_github(REPO org/mylib REF \"${PORT}-${VERSION}\" SHA512 0)")
	writeManifest(t, dir, `{"name":"mylib","version":"2.1"}`)

	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{"https://github.com/org/mylib.git": {"refs/tags/mylib-2.1": commitA}}, nil),
		Now:        fixedNow(),
	}).Ports[0]

	if p.Pin.ResolvedRef != "mylib-2.1" {
		t.Fatalf("resolved_ref = %q, want %q (${PORT} from the port dir, ${VERSION} from vcpkg.json)", p.Pin.ResolvedRef, "mylib-2.1")
	}
	if p.Pin.ResolvedFrom != RefValueSourceMixed {
		t.Fatalf("resolved_from = %q, want %q for a ref drawing on two different sources", p.Pin.ResolvedFrom, RefValueSourceMixed)
	}
}

// FIELD P1: the same defect class lived in every OTHER variable-bearing
// field, because they all shared the one anchored regex. A REPO that merely
// CONTAINS a variable used to yield a bogus remote URL built from the
// unexpanded literal.
func TestFieldP1_EmbeddedVariablesExpandInRepoAndUrlFieldsToo(t *testing.T) {
	dir := newPort(t, "embedded-repo", `set(ORG_NAME "acme")
vcpkg_from_github(REPO ${ORG_NAME}/widget REF `+commitA+` SHA512 0)`)

	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{"https://github.com/acme/widget.git": {"HEAD": commitA}}, nil),
		Now:        fixedNow(),
	}).Ports[0]

	if p.Remote.Repo != "acme/widget" {
		t.Fatalf("repo = %q, want %q — an embedded variable must expand here too", p.Remote.Repo, "acme/widget")
	}
	if p.Status != evidence.StatusOK {
		t.Fatalf("status=%v reason=%v, want ok", p.Status, p.Reason)
	}
}

// PR #591 P1 (redact.go): url.Parse rejects a URL for reasons that have
// NOTHING to do with its query — an invalid %-escape in the path, a control
// character, a space in the host, a bad escape in the fragment. The old
// unparsable fallback scrubbed only the userinfo, so every case below was
// returned VERBATIM, secret and all, and hasEmbeddedCredential likewise saw
// nothing to refuse. This file's whole contract is that it is TOTAL: a URL we
// cannot model is the case where we must redact MORE, never less.
//
// Each case asserts its own precondition (url.Parse really does reject it) so
// the test cannot quietly degrade into an exercise of the parse path if Go's
// URL parser ever becomes more permissive.
func TestRedactURL_UnparsableURLStillScrubsQueryCredentials(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		secret string
	}{
		{"invalid percent escape in path", "https://host/re%zzpo?access_token=SECRETVAL", "SECRETVAL"},
		{"control character in query", "https://host/repo?access_token=SECRETVAL\x7f", "SECRETVAL"},
		{"space in host", "https://ho st/repo?access_token=SECRETVAL", "SECRETVAL"},
		{"invalid escape in fragment", "https://host/repo?access_token=SECRETVAL#%zz", "SECRETVAL"},
		{"unparsable userinfo plus secret query", "https://%zz@host/repo?token=SECRETVAL", "SECRETVAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := url.Parse(tc.in); err == nil {
				t.Fatalf("precondition failed: url.Parse(%q) succeeded, so this case never reaches redactUnparsable and proves nothing", tc.in)
			}
			got := redactURL(tc.in)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("redactURL(%q) = %q, still leaks %q — failing to parse must never mean failing to redact", tc.in, got, tc.secret)
			}
			if !hasEmbeddedCredential(tc.in) {
				t.Fatalf("hasEmbeddedCredential(%q) = false, want true — an unparsable credential-bearing remote must be REFUSED, not handed to git ls-remote's argv", tc.in)
			}
		})
	}
}

// PR #591 P1 (redact.go), the equal-and-opposite guard: making the unparsable
// path total must not make it destructive. An unparsable URL carrying NO
// credential must come back byte-for-byte, so an operator can still read the
// malformed value that caused the failure.
func TestRedactURL_UnparsableURLWithoutCredentialIsUnchanged(t *testing.T) {
	// Query-FREE fixtures only. The query-BEARING case moved to
	// TestRedactURL_QueryValueRedactionIsAnAllowlist, where a redacted value
	// is now the ASSERTED behaviour rather than a regression — see that
	// test's comment for why the polarity had to flip, and note that the KEY
	// still round-trips, so the malformed value this test exists to preserve
	// is still readable.
	for _, clean := range []string{
		"https://ho st/repo.git",
		"https://host/re%zzpo.git",
	} {
		if got := redactURL(clean); got != clean {
			t.Fatalf("redactURL(%q) = %q, want it unchanged — over-redaction destroys the diagnostic", clean, got)
		}
		if hasEmbeddedCredential(clean) {
			t.Fatalf("hasEmbeddedCredential(%q) = true, want false — a credential-free remote must not be refused", clean)
		}
	}
}

// The EMISSION rule is an allowlist: a query parameter's value is printed only
// if its key is on emitSafeQueryKeys (currently empty), so an unrecognized key
// is redacted rather than forwarded.
//
// The previous rule was a denylist of secret-SHAPED key names, which by
// construction printed every credential spelling nobody had enumerated. Each
// key below is a real spelling that leaked verbatim into an MCP result — and
// per the file header an MCP result is copied into a model transcript, a
// provider's request log, and whatever the caller persists.
//
// The three complements are what stop the fix from being a blunt instrument:
// the KEY must survive (so the operator still sees which parameters existed),
// a query-free URL must still round-trip byte-for-byte, and the ARGV-refusal
// verdict must NOT move — hasEmbeddedCredential answers a different question
// ("does this URL embed a credential") whose answer is a wire enum value, and
// it must not silently become a shadow of the emission rule.
func TestRedactURL_QueryValueRedactionIsAnAllowlist(t *testing.T) {
	leaked := []struct{ key, why string }{
		{"code", "OAuth 2.0 authorization code"},
		{"jwt", "a bare JSON Web Token"},
		{"assertion", "RFC 7523 JWT bearer assertion"},
		{"pat", "Azure DevOps personal access token"},
		{"session", "session identifier"},
		{"sid", "short-form session identifier"},
		{"ticket", "CAS / Kerberos service ticket"},
		{"refresh", "OAuth refresh token"},
	}
	for _, tc := range leaked {
		for _, raw := range []string{
			"https://host/r.git?" + tc.key + "=s3cr3t",      // parsable
			"https://ho st/r.git?" + tc.key + "=s3cr3t",     // unparsable
			"https://host/r.git?" + tc.key + "=s3cr3t#frag", // fragment after query
		} {
			got := redactURL(raw)
			if strings.Contains(got, "s3cr3t") {
				t.Errorf("redactURL(%q) = %q — the value leaked; %q is %s and matches no secret-SHAPED name, which is exactly what a denylist cannot cover", raw, got, tc.key, tc.why)
			}
			if !strings.Contains(got, tc.key+"=") {
				t.Errorf("redactURL(%q) = %q — the KEY must survive so the operator still sees which parameters were present", raw, got)
			}
		}
	}

	// Complement 1: a query-free URL still round-trips byte-for-byte.
	for _, clean := range []string{
		"https://github.com/nlohmann/json.git",
		"git@github.com:owner/repo.git",
	} {
		if got := redactURL(clean); got != clean {
			t.Errorf("redactURL(%q) = %q — a query-free URL must stay copy-pasteable", clean, got)
		}
	}

	// Complement 2: a parameter carrying NO value invents no secret.
	for _, empty := range []string{"https://host/r.git?flag", "https://host/r.git?token="} {
		if got := redactURL(empty); got != empty {
			t.Errorf("redactURL(%q) = %q — there is no value to redact, so REDACTED would fabricate one", empty, got)
		}
	}

	// Complement 3: the ARGV-refusal verdict is unmoved by the emission
	// inversion. `?depth=1` is not a credential, and reporting
	// unknown(remote_url_credential_bearing) for it would be a conclusion the
	// tool never observed.
	for _, notACredential := range []string{
		"https://host/repo.git?depth=1&branch=main",
		"https://ho st/repo.git?depth=1&branch=main",
	} {
		if hasEmbeddedCredential(notACredential) {
			t.Errorf("hasEmbeddedCredential(%q) = true — the refusal must stay a POSITIVE credential identification, not a shadow of the emission allowlist", notACredential)
		}
		if got := redactURL(notACredential); !strings.Contains(got, "depth=REDACTED") {
			t.Errorf("redactURL(%q) = %q — an unclassifiable value is still redacted on EMISSION even though it is not refused on argv", notACredential, got)
		}
	}
	// ...and a positively-identified one is still refused on both paths.
	for _, credential := range []string{
		"https://host/repo.git?access_token=abc123",
		"https://ho st/repo.git?access_token=abc123",
	} {
		if !hasEmbeddedCredential(credential) {
			t.Errorf("hasEmbeddedCredential(%q) = false — a secret-shaped parameter must still be refused before it reaches git ls-remote's argv", credential)
		}
	}
}

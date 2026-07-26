package pinstatus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/cmd/vcpkg-mcp/internal/evidence"
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

	res := PinStatus(Args{PortDirs: []string{dir}}, deps)
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

	res := PinStatus(Args{PortDirs: []string{dir}}, deps)
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

	res := PinStatus(Args{PortDirs: []string{dir}}, deps)
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

			p := PinStatus(Args{PortDirs: []string{dir}}, deps).Ports[0]
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
	p := PinStatus(Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(nil, nil), Now: fixedNow()}).Ports[0]
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
	p := PinStatus(Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitB}}, nil), Now: fixedNow()}).Ports[0]
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
	p := PinStatus(Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(nil, nil), Now: fixedNow()}).Ports[0]
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
	p := PinStatus(Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitB}}, nil), Now: fixedNow()}).Ports[0]
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
	p := PinStatus(Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(nil, nil), Now: fixedNow()}).Ports[0]
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
	p := PinStatus(Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitB}}, nil), Now: fixedNow()}).Ports[0]
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
	p := PinStatus(Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"refs/tags/" + ref: commitA}}, nil), Now: fixedNow()}).Ports[0]
	if p.Status != evidence.StatusUnknown || p.Reason != ReasonNamedRefNotComparable || p.Pin.Ref != ref {
		t.Fatalf("status=%v reason=%v pin=%+v, want unknown named ref with bracket literal %q", p.Status, p.Reason, p.Pin, ref)
	}
}

func TestQuotedArgumentContinuationJoinsLines(t *testing.T) {
	dir := newPort(t, "continued", "\nvcpkg_from_github(REPO example/continued REF \"v1.\\\n2.3\" SHA512 0)\n")
	remote := "https://github.com/example/continued.git"
	p := PinStatus(Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"refs/tags/v1.2.3": commitA}}, nil), Now: fixedNow()}).Ports[0]
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

	res := PinStatus(Args{PortDirs: []string{dir}}, deps)
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

	res := PinStatus(Args{PortDirs: []string{dir}}, deps)
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

	res := PinStatus(Args{PortDirs: []string{dir}}, deps)
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

	res := PinStatus(Args{PortDirs: []string{dir}}, deps)
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

	res := PinStatus(Args{PortDirs: []string{dir}, DisableNetwork: true}, deps)
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
	res := PinStatus(Args{PortDirs: []string{filepath.Join(t.TempDir(), "does-not-exist")}, DisableNetwork: true}, deps)
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

	res := PinStatus(Args{PortDirs: []string{dir}}, deps)
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

	res := PinStatus(Args{PortDirs: []string{dir}}, deps)
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
			p := PinStatus(Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {tc.remoteRef: commitB}}, nil), Now: fixedNow()}).Ports[0]
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

	res := PinStatus(Args{PortDirs: []string{dir}}, deps)
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

	res := PinStatus(Args{PortDirs: []string{dir}}, deps)
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
	p := PinStatus(Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitA}}, nil), Now: fixedNow()}).Ports[0]
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
	p := PinStatus(Args{PortDirs: []string{dir}}, Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitA}}, nil), Now: fixedNow()}).Ports[0]
	if p.Status != evidence.StatusOK || p.TrackedRef != "HEAD" {
		t.Fatalf("status=%v reason=%v tracked=%q, want current against HEAD", p.Status, p.Reason, p.TrackedRef)
	}
}

func TestEmptyResultMarshalsPortsAsArray(t *testing.T) {
	data, err := json.Marshal(PinStatus(Args{}, Deps{Now: fixedNow()}))
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(data) != `{"ports":[]}` {
		t.Fatalf("empty result JSON = %s, want {\"ports\":[]}", data)
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
// package can assign (see pinstatus.go), proving none of the declared
// constants is or contains "behind" proves no code path can ever produce
// it.
func TestNoCodePathProducesBehind(t *testing.T) {
	reasons := []Reason{
		ReasonNotGitComparable,
		ReasonPinNotAtTip,
		ReasonRefUnresolvable,
		ReasonRefNotFoundOnRemote,
		ReasonNamedRefNotComparable,
		ReasonHeadRefUnresolvable,
		ReasonRemoteQueryFailed,
		ReasonNetworkDisabled,
		ReasonPortfileUnparsable,
		ReasonGuardUnresolvable,
		ReasonMultipleFetchCalls,
	}
	for _, r := range reasons {
		if strings.Contains(strings.ToLower(string(r)), "behind") {
			t.Fatalf("reason %q must never be/contain %q", r, "behind")
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

	res := PinStatus(Args{PortDirs: []string{
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
	res := PinStatus(Args{PortDirs: []string{ok, broken}}, deps)
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

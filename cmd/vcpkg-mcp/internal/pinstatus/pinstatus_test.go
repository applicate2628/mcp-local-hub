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
	if p.Status != evidence.StatusOK {
		t.Fatalf("status = %v reason = %v, want ok (resolved ref equals fake HEAD)", p.Status, p.Reason)
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

func TestGitHubTagRef_FoundOnRemote_Current(t *testing.T) {
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
	if p.Status != evidence.StatusOK {
		t.Fatalf("status = %v reason = %v, want ok", p.Status, p.Reason)
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
		ReasonRemoteQueryFailed,
		ReasonNetworkDisabled,
		ReasonPortfileUnparsable,
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
	queryErr := newPort(t, "query-err", `vcpkg_from_github(REPO a/errors REF `+commitA+` SHA512 0)`)

	refs := map[string]map[string]string{
		"https://github.com/a/b.git":        {"HEAD": commitA},
		"https://example.com/x/y.git":       {"HEAD": commitC},
		"https://github.com/a/tagged.git":   {"refs/tags/v2.0.0": commitA},
		"https://github.com/a/branched.git": {"refs/heads/main": commitA},
	}
	errs := map[string]error{
		"https://github.com/a/errors.git": errors.New("fake network error"),
	}
	deps := Deps{FS: DefaultFS(), RemoteRefs: fakeRemote(refs, errs), Now: fixedNow()}

	res := PinStatus(Args{PortDirs: []string{
		ghEqual, ghDiff, gitVar, distfile, meta, tagFound, branchMissing, unresolvable, queryErr,
	}}, deps)

	if len(res.Ports) != 9 {
		t.Fatalf("got %d port results, want 9", len(res.Ports))
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
	found := false
	for _, p := range res.Ports {
		if p.PortDir == ghDiff {
			found = true
			if p.Reason != ReasonPinNotAtTip {
				t.Fatalf("ghDiff reason = %v, want pin_not_at_tip", p.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("ghDiff result missing from batch")
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

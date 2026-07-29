package pinstatus

import (
	"context"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

type recordingPinFS struct{ reads []string }

func (f *recordingPinFS) ReadFile(path string) ([]byte, error) {
	f.reads = append(f.reads, path)
	return []byte(`vcpkg_from_github(REPO acme/widget REF ` + commitA + ` SHA512 0)`), nil
}

// PR #591 P1 (portfile.go resolveSetVariable): a set() inside an if() branch
// that did not fire is NOT the variable's value. Resolving the pin from it and
// then comparing that pin against the live remote produces a confident
// "ref_not_found_on_remote" for a ref that was never the pin — the
// wrong-negative class this package is built to refuse. The honest answer is
// unknown(ref_unresolvable), naming the variable the operator must pin down.
func TestConditionallyAssignedRefVariableIsUnresolvableNotGuessed(t *testing.T) {
	dir := newPort(t, "conditional", `
if(VCPKG_TARGET_IS_WINDOWS)
    set(MY_REF v1.0.0)
endif()
vcpkg_from_github(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO acme/widget
    REF ${MY_REF}
    SHA512 0
)
`)
	remote := "https://github.com/acme/widget.git"
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitA, "refs/tags/v2.0.0": commitB}}, nil),
		Now:        fixedNow(),
	}

	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps).Ports[0]
	if p.Status != evidence.StatusUnknown {
		t.Fatalf("status = %v, want unknown", p.Status)
	}
	if p.Reason != ReasonRefUnresolvable {
		t.Fatalf("reason = %v, want %v — a set() under an undecided if() must not be treated as the pin's value",
			p.Reason, ReasonRefUnresolvable)
	}
	if p.Pin.ResolvedRef != "" {
		t.Fatalf("pin.resolved_ref = %q, want empty — a conditionally assigned value was resolved as if certain", p.Pin.ResolvedRef)
	}
	if p.Pin.UnresolvedVariable != "MY_REF" {
		t.Fatalf("pin.unresolved_variable = %q, want MY_REF so the operator is told which variable to supply", p.Pin.UnresolvedVariable)
	}
}

// The equal-and-opposite guard for the fix above: an UNCONDITIONAL set() must
// still resolve. Failing closed on everything would be just as useless as
// guessing.
func TestUnconditionallyAssignedRefVariableStillResolves(t *testing.T) {
	dir := newPort(t, "unconditional", `
set(MY_REF v1.0.0)
vcpkg_from_github(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO acme/widget
    REF ${MY_REF}
    SHA512 0
)
`)
	remote := "https://github.com/acme/widget.git"
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitA, "refs/tags/v1.0.0": commitB}}, nil),
		Now:        fixedNow(),
	}

	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps).Ports[0]
	if p.Pin.ResolvedRef != "v1.0.0" {
		t.Fatalf("pin.resolved_ref = %q, want v1.0.0 — an unconditional set() is certain and must still resolve", p.Pin.ResolvedRef)
	}
	if p.Reason != ReasonNamedRefNotComparable {
		t.Fatalf("reason = %v, want %v", p.Reason, ReasonNamedRefNotComparable)
	}
}

// PR #591 P2 (portfile.go): "Command names are case-insensitive"
// (cmake-language(7)). The if/endif handling was already folded but the
// source-acquisition dispatch was not, so an upper-case VCPKG_FROM_GITHUB(...)
// was invisible to the parser — the port looked like a metapackage that
// fetches nothing (not_git_comparable) instead of being checked at all.
func TestUpperCaseCommandNamesAreRecognized(t *testing.T) {
	dir := newPort(t, "shouty", `
IF(NOT VCPKG_USE_HEAD_VERSION)
    SET(UNUSED_MARKER 1)
ENDIF()
VCPKG_FROM_GITHUB(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO acme/widget
    REF `+commitA+`
    SHA512 0
)
`)
	remote := "https://github.com/acme/widget.git"
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitA}}, nil),
		Now:        fixedNow(),
	}

	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps).Ports[0]
	if p.Remote.Kind != RemoteGitHub {
		t.Fatalf("remote.kind = %q, want github — an upper-case VCPKG_FROM_GITHUB call was not recognized as a fetch call at all", p.Remote.Kind)
	}
	if p.Status != evidence.StatusOK {
		t.Fatalf("status = %v reason = %v, want ok", p.Status, p.Reason)
	}
	if p.PinnedSHA != commitA || p.TipSHA != commitA {
		t.Fatalf("SHAs = pinned=%q tip=%q", p.PinnedSHA, p.TipSHA)
	}
}

// PR #591 P1 (pinstatus.go): an abbreviated commit pin is a COMMIT. Only
// exactly-40-hex entered the comparison path, so a 7..39-hex pin fell out to
// the named-ref lookup, found nothing (ls-remote advertises full SHAs and ref
// NAMES, never abbreviations), and was reported as ref_not_found_on_remote —
// asserting that the remote had DELETED a ref that never existed as a name.
func TestAbbreviatedCommitPinIsAnUnresolvableCommitNotAMissingRef(t *testing.T) {
	const abbrev = "aaaaaaa" // 7 hex: git's own default short-SHA length
	dir := newPort(t, "abbrev", `
vcpkg_from_github(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO acme/widget
    REF `+abbrev+`
    SHA512 0
)
`)
	remote := "https://github.com/acme/widget.git"
	deps := Deps{
		FS:         DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{remote: {"HEAD": commitA}}, nil),
		Now:        fixedNow(),
	}

	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps).Ports[0]
	if p.Reason != ReasonCommitPinAbbreviated {
		t.Fatalf("reason = %v, want %v — an abbreviated commit reported as a missing ref sends the operator hunting for a deleted tag that never existed",
			p.Reason, ReasonCommitPinAbbreviated)
	}
	if p.Pin.Shape != RefShapeCommitAbbrev {
		t.Fatalf("pin.shape = %q, want %q — the pin's KIND must be named honestly", p.Pin.Shape, RefShapeCommitAbbrev)
	}
	if p.PinnedSHA != abbrev {
		t.Fatalf("pinned_sha = %q, want %q", p.PinnedSHA, abbrev)
	}
}

// The evidence-beats-shape guard for the fix above: a branch or tag that
// happens to be pure hex and IS advertised by the remote must be classified by
// that evidence, not by its spelling.
func TestHexShapedNameThatTheRemoteAdvertisesIsANamedRefNotACommit(t *testing.T) {
	const hexName = "deadbeef"
	dir := newPort(t, "hexname", `
vcpkg_from_github(
    OUT_SOURCE_PATH SOURCE_PATH
    REPO acme/widget
    REF `+hexName+`
    SHA512 0
)
`)
	remote := "https://github.com/acme/widget.git"
	deps := Deps{
		FS: DefaultFS(),
		RemoteRefs: fakeRemote(map[string]map[string]string{
			remote: {"HEAD": commitA, "refs/tags/" + hexName: commitB},
		}, nil),
		Now: fixedNow(),
	}

	p := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, deps).Ports[0]
	if p.Reason != ReasonNamedRefNotComparable {
		t.Fatalf("reason = %v, want %v — the remote ADVERTISES this name, so shape must not override evidence", p.Reason, ReasonNamedRefNotComparable)
	}
	if p.NamedRef != hexName || p.NamedRefSHA != commitB {
		t.Fatalf("named_ref = %q / %q", p.NamedRef, p.NamedRefSHA)
	}
}

// PR #591 P2 (remote.go): io.Writer requires "Write must return a non-nil
// error if it returns n < len(p)". boundedWriter returned the TRUNCATED length
// with a nil error once the retention budget was crossed — a short write,
// which os/exec reports as an I/O failure on the stderr copy. Capping the
// diagnostic tail must stay invisible to the caller.
func TestBoundedWriterReportsFullWritesWhileDiscardingExcess(t *testing.T) {
	var sink strings.Builder
	bw := &boundedWriter{w: &sink, remaining: 4}

	payload := []byte("0123456789")
	n, err := bw.Write(payload)
	if err != nil {
		t.Fatalf("Write returned error %v, want nil — discarding excess is not a failure", err)
	}
	if n != len(payload) {
		t.Fatalf("Write returned n=%d for a %d-byte payload: a short write with a nil error violates io.Writer and makes os/exec fail the stderr copy",
			n, len(payload))
	}
	if sink.String() != "0123" {
		t.Fatalf("retained %q, want %q — the cap must still bound what is kept", sink.String(), "0123")
	}

	// Past the budget entirely: still a full-length, error-free report.
	n, err = bw.Write(payload)
	if err != nil || n != len(payload) {
		t.Fatalf("post-budget Write = (%d, %v), want (%d, nil)", n, err, len(payload))
	}
	if sink.String() != "0123" {
		t.Fatalf("retained %q after the budget was exhausted, want it unchanged", sink.String())
	}
}

// PR #591: every port_dirs entry is an absolute-path contract. A relative
// entry must not inherit the hub daemon's private cwd, even when network is
// disabled; invalid-input classification owns precedence over runtime policy.
func TestRelativePortDirIsRefusedBeforeFilesystemOrNetwork(t *testing.T) {
	for _, disableNetwork := range []bool{false, true} {
		t.Run(map[bool]string{false: "network enabled", true: "network disabled"}[disableNetwork], func(t *testing.T) {
			fsys := &recordingPinFS{}
			var queries int
			remote := func(context.Context, approvedRemoteURL) (map[string]string, error) {
				queries++
				return map[string]string{"HEAD": commitA}, nil
			}

			res := PinStatus(context.Background(), Args{
				PortDirs:       []string{"relative/port"},
				DisableNetwork: disableNetwork,
			}, Deps{FS: fsys, RemoteRefs: remote, Now: fixedNow()})

			if res.Status != evidence.StatusOK || len(res.Ports) != 1 {
				t.Fatalf("batch = %v with %d ports, want ok with one explicit per-port verdict", res.Status, len(res.Ports))
			}
			port := res.Ports[0]
			if port.Status != evidence.StatusFailed || port.Reason != ReasonRelativePortDir {
				t.Fatalf("port status/reason = %v/%v, want failed/%v", port.Status, port.Reason, ReasonRelativePortDir)
			}
			if port.PortDir != "relative/port" {
				t.Fatalf("port_dir = %q, want the caller input echoed unchanged", port.PortDir)
			}
			if len(port.Evidence.Paths) != 0 {
				t.Fatalf("evidence.paths = %v, want empty: relative paths are not filesystem evidence", port.Evidence.Paths)
			}
			if len(fsys.reads) != 0 || queries != 0 {
				t.Fatalf("relative input touched filesystem %v and made %d remote queries; want neither", fsys.reads, queries)
			}
		})
	}
}

func TestApproveRemoteURLClassification(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantReason Reason
	}{
		{"https", "https://github.com/acme/widget.git", ""},
		{"scp", "git@github.com:acme/widget.git", ""},
		{"local path", "../widget.git", ""},
		{"valueless query", "https://host/widget.git?flag", ""},
		{"empty query value", "https://host/widget.git?token=", ""},
		{"userinfo credential", "https://user:secret@host/widget.git", ReasonRemoteURLCredentialBearing},
		{"credential query", "https://host/widget.git?access_token=secret", ReasonRemoteURLCredentialBearing},
		{"unknown query value", "https://host/widget.git?depth=1", ReasonRemoteURLQueryUnclassified},
		{"unknown malformed query", "https://ho st/widget.git?depth=1", ReasonRemoteURLQueryUnclassified},
		{"malformed shape", "https://ho st/widget.git", ReasonPortfileUnparsable},
		{"missing host", "https:///widget.git", ReasonPortfileUnparsable},
		{"fragment", "https://host/widget.git#main", ReasonPortfileUnparsable},
		{"empty", "", ReasonPortfileUnparsable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			approved, reason := approveRemoteURL(tc.raw)
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			raw, ok := approved.transportArgument()
			if tc.wantReason != "" {
				if ok || raw != "" {
					t.Fatalf("rejected URL gained transport authority: raw=%q ok=%v", raw, ok)
				}
				return
			}
			if !ok || raw != tc.raw {
				t.Fatalf("approved transport = %q/%v, want %q/true", raw, ok, tc.raw)
			}
		})
	}
}

func TestRemoteURLAdmissionRejectionsMakeZeroRemoteCalls(t *testing.T) {
	tests := []struct {
		name       string
		remote     string
		wantReason Reason
	}{
		{"credential", "https://host/widget.git?token=secret", ReasonRemoteURLCredentialBearing},
		{"unclassified query", "https://host/widget.git?depth=1", ReasonRemoteURLQueryUnclassified},
		{"invalid shape", "https://ho st/widget.git", ReasonPortfileUnparsable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := newPort(t, "approval-"+strings.ReplaceAll(tc.name, " ", "-"), `vcpkg_from_git(
    OUT_SOURCE_PATH SOURCE_PATH
    URL `+tc.remote+`
    REF `+commitA+`
    SHA512 0
)`)
			var calls int
			result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
				FS: DefaultFS(),
				RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
					calls++
					return map[string]string{"HEAD": commitA}, nil
				},
				Now: fixedNow(),
			}).Ports[0]
			if calls != 0 {
				t.Fatalf("remote calls = %d, want 0", calls)
			}
			if result.Status != evidence.StatusUnknown || result.Reason != tc.wantReason {
				t.Fatalf("status/reason = %s/%s, want unknown/%s",
					result.Status, result.Reason, tc.wantReason)
			}
			if strings.Contains(result.Remote.URL, "secret") || strings.Contains(result.Remote.URL, "depth=1") {
				t.Fatalf("rejected value leaked in public remote %q", result.Remote.URL)
			}
		})
	}
}

func TestApprovedRemoteURLHasOneConstructorAndTypedConsumers(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(thisFile), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := gotoken.NewFileSet()
	var declarations, literals, typedCalls int
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range file.Decls {
			switch node := declaration.(type) {
			case *ast.GenDecl:
				for _, spec := range node.Specs {
					typeSpec, ok := spec.(*ast.TypeSpec)
					if ok && typeSpec.Name.Name == "approvedRemoteURL" {
						declarations++
					}
				}
			case *ast.FuncDecl:
				functionName := node.Name.Name
				ast.Inspect(node.Body, func(n ast.Node) bool {
					switch expression := n.(type) {
					case *ast.CompositeLit:
						identifier, ok := expression.Type.(*ast.Ident)
						if ok && identifier.Name == "approvedRemoteURL" {
							literals++
							if functionName != "approveRemoteURL" {
								t.Errorf("%s constructs approvedRemoteURL outside approveRemoteURL", fset.Position(expression.Pos()))
							}
						}
					case *ast.CallExpr:
						if identifier, ok := expression.Fun.(*ast.Ident); ok {
							if identifier.Name == "approvedRemoteURL" {
								t.Errorf("%s converts directly to approvedRemoteURL", fset.Position(expression.Pos()))
							}
							if identifier.Name == "remoteRefs" {
								typedCalls++
								if len(expression.Args) != 2 {
									t.Errorf("%s remoteRefs call has %d arguments", fset.Position(expression.Pos()), len(expression.Args))
								} else if argument, ok := expression.Args[1].(*ast.Ident); !ok || argument.Name != "approvedRemote" {
									t.Errorf("%s remoteRefs receives something other than the approved value", fset.Position(expression.Args[1].Pos()))
								}
							}
						}
					}
					return true
				})
			}
		}
	}
	if declarations != 1 {
		t.Fatalf("approvedRemoteURL declarations = %d, want 1", declarations)
	}
	if literals != 4 {
		t.Fatalf("approvedRemoteURL literals = %d, want 4 all inside approveRemoteURL", literals)
	}
	if typedCalls != 1 {
		t.Fatalf("remoteRefs production calls = %d, want 1 typed call", typedCalls)
	}
	if _, ok := (approvedRemoteURL{}).transportArgument(); ok {
		t.Fatal("zero approvedRemoteURL gained transport authority")
	}
	forged := approvedRemoteURL{
		raw:   "https://host/widget.git?depth=1",
		proof: remoteURLApprovalProof,
	}
	if _, ok := forged.transportArgument(); ok {
		t.Fatal("forged approvedRemoteURL bypassed policy revalidation")
	}
}

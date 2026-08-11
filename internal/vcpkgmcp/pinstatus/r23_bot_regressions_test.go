package pinstatus

import (
	"context"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/vcpkgmcp/evidence"
)

func TestR23ExternalRemoteHelperSchemeIsRejected(t *testing.T) {
	approved, reason := approveRemoteURL("evil://host/repo.git")
	if reason != ReasonRemoteURLTransportUnapproved {
		t.Fatalf("reason=%q, want %q", reason, ReasonRemoteURLTransportUnapproved)
	}
	if raw, ok := approved.transportArgument(); ok || raw != "" {
		t.Fatalf("unapproved helper scheme gained transport authority: raw=%q ok=%v", raw, ok)
	}
}

func TestR23ExternalRemoteHelperSchemeStartsNoRemoteQuery(t *testing.T) {
	dir := newPort(t, "unapproved-transport", `vcpkg_from_git(
    OUT_SOURCE_PATH SOURCE_PATH
    URL evil://host/repo.git
    REF 0123456789abcdef0123456789abcdef01234567
    SHA512 0
)`)
	calls := 0
	result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS: DefaultFS(),
		RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
			calls++
			return nil, nil
		},
		Now: fixedNow(),
	}).Ports[0]
	if calls != 0 {
		t.Fatalf("remote queries=%d, want 0", calls)
	}
	if result.Status != evidence.StatusUnknown || result.Reason != ReasonRemoteURLTransportUnapproved {
		t.Fatalf("status/reason=%s/%s, want unknown/%s", result.Status, result.Reason, ReasonRemoteURLTransportUnapproved)
	}
}

func TestRemoteHelperSyntaxIsRejectedBeforeRemoteQuery(t *testing.T) {
	for _, remote := range []string{"evil::attacker-controlled-address", "ext::sh -c exploit"} {
		t.Run(remote, func(t *testing.T) {
			approved, reason := approveRemoteURL(remote)
			if reason != ReasonRemoteURLTransportUnapproved {
				t.Fatalf("reason=%q, want %q", reason, ReasonRemoteURLTransportUnapproved)
			}
			if raw, ok := approved.transportArgument(); ok || raw != "" {
				t.Fatalf("remote-helper syntax gained transport authority: raw=%q ok=%v", raw, ok)
			}

			dir := newPort(t, "remote-helper", `vcpkg_from_git(
    OUT_SOURCE_PATH SOURCE_PATH
    URL `+remote+`
    REF 0123456789abcdef0123456789abcdef01234567
    SHA512 0
)`)
			calls := 0
			result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
				FS: DefaultFS(),
				RemoteRefs: func(context.Context, approvedRemoteURL) (map[string]string, error) {
					calls++
					return nil, nil
				},
				Now: fixedNow(),
			}).Ports[0]
			if calls != 0 {
				t.Fatalf("remote queries=%d, want 0", calls)
			}
			if result.Status != evidence.StatusUnknown || result.Reason != ReasonRemoteURLTransportUnapproved {
				t.Fatalf("status/reason=%s/%s, want unknown/%s", result.Status, result.Reason, ReasonRemoteURLTransportUnapproved)
			}
		})
	}
}

func TestRemoteHelperGuardPreservesNonHelperDoubleColonRemotes(t *testing.T) {
	absoluteLocal := "/tmp/repo::mirror"
	if filepath.Separator == '\\' {
		absoluteLocal = `\\server\share\repo::mirror`
	}

	for _, remote := range []string{
		absoluteLocal,
		"[::1]:owner/repo",
		"https://host/owner/repo::mirror",
		"example.com:owner/repo::mirror",
	} {
		t.Run(remote, func(t *testing.T) {
			approved, reason := approveRemoteURL(remote)
			if reason != "" {
				t.Fatalf("reason=%q, want admitted", reason)
			}
			if raw, ok := approved.transportArgument(); !ok || raw != remote {
				t.Fatalf("transport argument=%q/%v, want %q/true", raw, ok, remote)
			}
		})
	}
}

func TestRemoteHelperGuardMatchesGitLeadingTransportGrammar(t *testing.T) {
	tests := []struct {
		remote string
		want   bool
	}{
		{remote: "evil::address", want: true},
		{remote: "9evil::address", want: true},
		{remote: "git+foo::address", want: true},
		{remote: "git.foo::address", want: true},
		{remote: "git-foo::address", want: true},
		{remote: "::address", want: true},
		{remote: ".evil::address", want: false},
		{remote: "+evil::address", want: false},
		{remote: "-evil::address", want: false},
		{remote: "git_foo::address", want: false},
		{remote: "/tmp/repo::mirror", want: false},
		{remote: "[::1]:owner/repo", want: false},
		{remote: "https://host/repo::mirror", want: false},
		{remote: "example.com:owner/repo::mirror", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.remote, func(t *testing.T) {
			if got := isGitRemoteHelperSyntax(tc.remote); got != tc.want {
				t.Fatalf("isGitRemoteHelperSyntax(%q)=%v, want %v", tc.remote, got, tc.want)
			}
		})
	}
}

package pinstatus

import (
	"context"
	"path/filepath"
	"testing"
)

func TestR35SCPLikeRemoteWithoutUserIsAdmitted(t *testing.T) {
	const remote = "example.com:owner/repo"

	approved, reason := approveRemoteURL(remote)
	if reason != "" {
		t.Fatalf("approveRemoteURL(%q) reason = %q, want admitted", remote, reason)
	}
	if argument, ok := approved.transportArgument(); !ok || argument != remote {
		t.Fatalf("transport argument = %q/%t, want %q/true", argument, ok, remote)
	}

	dir := newPort(t, "scp-without-user", `vcpkg_from_git(
    OUT_SOURCE_PATH SOURCE_PATH
    URL `+remote+`
    REF `+commitA+`
    SHA512 0
)`)
	var calls int
	result := PinStatus(context.Background(), Args{PortDirs: []string{dir}}, Deps{
		FS:  DefaultFS(),
		Now: fixedNow(),
		RemoteRefs: func(_ context.Context, got approvedRemoteURL) (map[string]string, error) {
			calls++
			argument, ok := got.transportArgument()
			if !ok || argument != remote {
				t.Fatalf("remote query argument = %q/%t, want %q/true", argument, ok, remote)
			}
			return map[string]string{commitA: commitA}, nil
		},
	})
	if calls != 1 {
		t.Fatalf("remote query calls = %d, want 1", calls)
	}
	if len(result.Ports) != 1 || result.Ports[0].Reason == ReasonPortfileUnparsable {
		t.Fatalf("result = %#v, SCP-like remote remained unparsable", result)
	}
}

func TestR35SCPLikeAdmissionPreservesLocalPathGuards(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Reason
	}{
		{name: "relative path", raw: "../owner/repo", want: ReasonRemoteURLRelative},
		{name: "slash before colon", raw: "owner/repo:branch", want: ReasonRemoteURLRelative},
		{name: "windows drive relative", raw: `C:owner\repo`, want: ReasonPortfileUnparsable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			approved, reason := approveRemoteURL(tc.raw)
			if reason != tc.want {
				t.Fatalf("approveRemoteURL(%q) reason = %q, want %q", tc.raw, reason, tc.want)
			}
			if argument, ok := approved.transportArgument(); ok || argument != "" {
				t.Fatalf("rejected local path gained transport authority: %q/%t", argument, ok)
			}
		})
	}

	absoluteLocal := filepath.Join(t.TempDir(), "repo")
	approved, reason := approveRemoteURL(absoluteLocal)
	if reason != "" {
		t.Fatalf("absolute local remote reason = %q, want admitted", reason)
	}
	if argument, ok := approved.transportArgument(); !ok || argument != absoluteLocal {
		t.Fatalf("absolute local transport = %q/%t, want %q/true", argument, ok, absoluteLocal)
	}
}

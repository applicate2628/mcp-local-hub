package pinstatus

import (
	"context"
	"os/exec"
	"strings"
)

// remoteRefsFn queries a git remote's currently-advertised refs, returning
// a map of ref name (as `git ls-remote` prints it — "HEAD", "refs/heads/main",
// "refs/tags/v1.0", "refs/tags/v1.0^{}" for a peeled annotated tag, ...) to
// the commit SHA it currently points at. Production code shells out to a
// real `git ls-remote`; every test in this package injects a fake so tests
// never touch the network — see the package doc "hard limit": this is the
// ONLY thing a remote can honestly tell us (what a ref points at NOW), which
// is why the type returns a snapshot map rather than any kind of ordering
// or ancestry answer.
type remoteRefsFn func(ctx context.Context, remote string) (map[string]string, error)

// defaultRemoteRefs is the real `git ls-remote` implementation.
func defaultRemoteRefs(ctx context.Context, remote string) (map[string]string, error) {
	out, err := exec.CommandContext(ctx, "git", "ls-remote", remote).Output()
	if err != nil {
		return nil, err
	}
	return parseLsRemote(out), nil
}

// parseLsRemote parses `git ls-remote`'s "<sha>\t<refname>" line output.
func parseLsRemote(out []byte) map[string]string {
	refs := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		refs[parts[1]] = parts[0]
	}
	return refs
}

// resolveTrackedTip picks which ref's SHA a commit-shaped pin is compared
// against: vcpkg's own HEAD_REF override when the portfile supplied one
// (checked as both "refs/heads/<HEAD_REF>" and the literal token, since a
// HEAD_REF may already be a fully-qualified ref path), falling back to the
// remote's own "HEAD" entry (its default branch) otherwise.
func resolveTrackedTip(refs map[string]string, headRef string) (sha, tracked string, ok bool) {
	if headRef != "" {
		if sha, ok := refs["refs/heads/"+headRef]; ok {
			return sha, "refs/heads/" + headRef, true
		}
		if sha, ok := refs[headRef]; ok {
			return sha, headRef, true
		}
		return "", "", false
	}
	if sha, ok := refs["HEAD"]; ok {
		return sha, "HEAD", true
	}
	return "", "", false
}

// refFound reports whether the remote currently advertises ANY ref named
// ref — tried as an annotated-tag-peeled entry, a plain tag, a branch, and a
// literal key, in that order. This is an EXISTENCE check only; it never
// claims currency or ordering (see package doc "hard limit") — a tag or
// branch pin has no fixed baseline commit to compare against without
// downloading the source, which this package never does.
func refFound(refs map[string]string, ref string) (string, bool) {
	if sha, ok := refs["refs/tags/"+ref+"^{}"]; ok {
		return sha, true
	}
	if sha, ok := refs["refs/tags/"+ref]; ok {
		return sha, true
	}
	if sha, ok := refs["refs/heads/"+ref]; ok {
		return sha, true
	}
	if sha, ok := refs[ref]; ok {
		return sha, true
	}
	return "", false
}

// buildCompareURL returns a browsable diff link on a known forge (github,
// gitlab), or "" when Remote.Kind is not one of those (including the
// generic vcpkg_from_git shape, whose URL is not guaranteed to be any
// particular forge's web format) or either SHA is unknown. Never expresses
// a direction — see package doc "hard limit".
func buildCompareURL(remote Remote, pinnedSHA, tipSHA string) string {
	if pinnedSHA == "" || tipSHA == "" {
		return ""
	}
	switch remote.Kind {
	case RemoteGitHub:
		if remote.Repo == "" {
			return ""
		}
		return "https://github.com/" + remote.Repo + "/compare/" + pinnedSHA + "..." + tipSHA
	case RemoteGitLab:
		webBase := strings.TrimSuffix(remote.URL, ".git")
		if webBase == "" {
			return ""
		}
		return webBase + "/-/compare/" + pinnedSHA + "..." + tipSHA
	default:
		return ""
	}
}

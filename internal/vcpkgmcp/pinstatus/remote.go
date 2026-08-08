package pinstatus

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	hubprocess "mcp-local-hub/internal/process"
	"mcp-local-hub/internal/vcpkgmcp/boundedio"
)

// Bounds on one `git ls-remote` invocation. Every one of these exists because
// the remote is UNTRUSTED input on a SHARED daemon: it chooses how long to
// stall, how many refs to advertise, and how long a single line is.
const (
	// RemoteQueryTimeout bounds one child process wall-clock. A stalled
	// remote must not pin a request (or a git process) open indefinitely; the
	// live scout pass measured a worst case of 11.7s, so 60s is generous
	// headroom rather than a tight budget.
	RemoteQueryTimeout = 60 * time.Second
	// RemoteWaitDelay bounds how long Wait tolerates our stdout pipe staying
	// open AFTER the child was killed or exited. `git ls-remote` spawns
	// helpers (git-remote-https, ssh, a credential helper) that inherit that
	// pipe; without this, Wait blocks on a grandchild we never tracked.
	RemoteWaitDelay = 5 * time.Second
	// MaxRemoteRefs bounds how many advertised refs are indexed. A hostile or
	// merely enormous remote must not be able to size our map.
	MaxRemoteRefs = 100000
	// MaxRemoteRefLineBytes bounds one advertised ref line ("<40-hex>\t<name>").
	// Real ref names are far below this; a longer line is not a ref.
	MaxRemoteRefLineBytes = 8 << 10 // 8 KiB
	// MaxRemoteOutputBytes bounds total bytes consumed from the child. It is
	// the backstop for a remote that streams forever without ever emitting a
	// newline our line cap could trip on.
	MaxRemoteOutputBytes = 32 << 20 // 32 MiB
	// maxRemoteStderrBytes bounds the diagnostic tail we keep from stderr.
	maxRemoteStderrBytes = 4 << 10
)

// ErrRemoteRefLimit is returned when a remote's advertisement exceeded one of
// the bounds above. It is a SENTINEL so pinStatusOne can map it to its own
// closed reason with errors.Is, rather than string-matching an error message.
var ErrRemoteRefLimit = errors.New("pinstatus: remote ref advertisement exceeded configured limits")

// remoteRefsFn queries a git remote's currently-advertised refs, returning
// a map of ref name (as `git ls-remote` prints it — "HEAD", "refs/heads/main",
// "refs/tags/v1.0", "refs/tags/v1.0^{}" for a peeled annotated tag, ...) to
// the commit SHA it currently points at. Production code shells out to a
// real `git ls-remote`; every test in this package injects a fake so tests
// never touch the network — see the package doc "hard limit": this is the
// ONLY thing a remote can honestly tell us (what a ref points at NOW), which
// is why the type returns a snapshot map rather than any kind of ordering
// or ancestry answer.
type remoteRefsFn func(ctx context.Context, remote approvedRemoteURL) (map[string]string, error)

// defaultRemoteRefs is the real `git ls-remote` implementation.
//
// It honours the CALLER'S ctx (so an MCP cancellation actually stops the
// work), imposes its own child deadline on top (so a caller with no deadline
// still cannot be pinned open by a stalled remote), STREAMS the response
// under hard byte/line/ref ceilings instead of buffering it whole, and calls
// Wait on EVERY exit path — success, parse limit, error, cancellation — so
// the child is reaped and our pipes are released rather than leaked.
func defaultRemoteRefs(ctx context.Context, remote approvedRemoteURL) (map[string]string, error) {
	rawRemote, ok := remote.transportArgument()
	if !ok {
		return nil, errRemoteURLApprovalMissing
	}
	ctx, cancel := context.WithTimeout(ctx, RemoteQueryTimeout)
	defer cancel()

	cmd := exec.Command("git", "ls-remote", rawRemote)
	// A credential prompt is the single most common way `git ls-remote`
	// stalls: it blocks on a terminal that is not there and burns the whole
	// deadline. Refuse to prompt so an auth failure is a fast, honest error.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
	)
	return remoteRefsFromCommand(ctx, rawRemote, cmd)
}

// remoteRefsFromCommand is the package-private deterministic seam for the
// exact process lifecycle. Production passes only the git command above;
// tests pass the current test helper executable without changing remoteRefsFn.
func remoteRefsFromCommand(ctx context.Context, redactedDiagnosticRemote string, cmd *exec.Cmd) (map[string]string, error) {
	var stderr strings.Builder
	var refs map[string]string
	runErr := hubprocess.RunContainedStream(
		ctx,
		cmd,
		hubprocess.ContainedStreamOptions{
			CleanupTimeout: RemoteWaitDelay,
			Stderr:         &boundedWriter{w: &stderr, remaining: maxRemoteStderrBytes},
		},
		func(stdout io.Reader) error {
			parsed, parseErr := parseLsRemoteStream(stdout)
			if parseErr == nil {
				refs = parsed
			}
			return parseErr
		},
	)
	if runErr != nil {
		var lifecycle *hubprocess.ContainedRunError
		if errors.As(runErr, &lifecycle) && lifecycle.Stage == hubprocess.ContainedStageExit {
			// Preserve the established internal Git-exit diagnostic while
			// keeping the public lifecycle projection fixed and sanitized.
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return nil, fmt.Errorf("git ls-remote %s: %w: %s", redactURL(redactedDiagnosticRemote), runErr, msg)
			}
			return nil, fmt.Errorf("git ls-remote %s: %w", redactURL(redactedDiagnosticRemote), runErr)
		}
		return nil, runErr
	}
	return refs, nil
}

// boundedWriter caps how much of a stream is retained. Used for stderr, whose
// only purpose here is a short diagnostic tail — a remote that streams
// megabytes of error text must not be able to size our memory.
type boundedWriter struct {
	w         io.Writer
	remaining int
}

// Write retains at most `remaining` further bytes and DISCARDS the rest, but
// always reports the full len(p) on success.
//
// io.Writer's contract is "Write must return a non-nil error if it returns
// n < len(p)". Returning the truncated length with a nil error is a SHORT
// WRITE, which os/exec treats as an I/O failure on the stderr copy — so
// capping the diagnostic tail would have started failing the very ls-remote
// calls it exists to keep cheap. The bug was masked only because the previous
// return used len(p) AFTER p had been resliced to the retained prefix, which
// reads like the full length but is not.
//
// A real error from the underlying writer is still propagated with the count
// actually written, which is the correct short-write report.
func (b *boundedWriter) Write(p []byte) (int, error) {
	full := len(p)
	if b.remaining <= 0 {
		return full, nil // discard, but never signal an error back to exec
	}
	retained := p
	if len(retained) > b.remaining {
		retained = retained[:b.remaining]
	}
	n, err := b.w.Write(retained)
	b.remaining -= n
	if err != nil {
		return n, err
	}
	return full, nil
}

// parseLsRemoteStream parses `git ls-remote`'s "<sha>\t<refname>" output as it
// arrives, under the ceilings declared above. It returns ErrRemoteRefLimit
// (wrapped, with which ceiling tripped) rather than a partial map, because a
// truncated ref set could silently turn "the ref exists" into "the ref is
// gone" — the exact false verdict this package's contract forbids.
func parseLsRemoteStream(r io.Reader) (map[string]string, error) {
	limited := &io.LimitedReader{R: r, N: MaxRemoteOutputBytes + 1}
	lines, err := boundedio.NewLineReader(limited, MaxRemoteRefLineBytes)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	consumed := int64(0)

	for {
		line, lineErr := lines.ReadLine()
		if line.Limited {
			return nil, fmt.Errorf("%w: a single ref line exceeded %d bytes", ErrRemoteRefLimit, MaxRemoteRefLineBytes)
		}
		consumed += int64(len(line.Data))
		if consumed > MaxRemoteOutputBytes {
			return nil, fmt.Errorf("%w: output exceeded %d bytes", ErrRemoteRefLimit, MaxRemoteOutputBytes)
		}
		if lineErr != nil && !errors.Is(lineErr, io.EOF) {
			return nil, lineErr
		}

		if trimmed := strings.TrimSpace(string(line.Data)); trimmed != "" {
			sha, name, found := strings.Cut(trimmed, "\t")
			if found {
				if len(refs) >= MaxRemoteRefs {
					return nil, fmt.Errorf("%w: more than %d refs advertised", ErrRemoteRefLimit, MaxRemoteRefs)
				}
				refs[name] = sha
			}
		}

		if errors.Is(lineErr, io.EOF) {
			return refs, nil
		}
	}
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

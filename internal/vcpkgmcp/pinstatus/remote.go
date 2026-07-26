package pinstatus

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
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
type remoteRefsFn func(ctx context.Context, remote string) (map[string]string, error)

// defaultRemoteRefs is the real `git ls-remote` implementation.
//
// It honours the CALLER'S ctx (so an MCP cancellation actually stops the
// work), imposes its own child deadline on top (so a caller with no deadline
// still cannot be pinned open by a stalled remote), STREAMS the response
// under hard byte/line/ref ceilings instead of buffering it whole, and calls
// Wait on EVERY exit path — success, parse limit, error, cancellation — so
// the child is reaped and our pipes are released rather than leaked.
func defaultRemoteRefs(ctx context.Context, remote string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, RemoteQueryTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "ls-remote", remote)
	// A credential prompt is the single most common way `git ls-remote`
	// stalls: it blocks on a terminal that is not there and burns the whole
	// deadline. Refuse to prompt so an auth failure is a fast, honest error.
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GCM_INTERACTIVE=never",
	)
	// WaitDelay is what makes "wait for cleanup" bounded rather than a hope:
	// after the context ends (or the child exits), Wait force-closes the I/O
	// pipes if a surviving grandchild still holds them, instead of blocking
	// on a process we never tracked.
	cmd.WaitDelay = RemoteWaitDelay

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &boundedWriter{w: &stderr, remaining: maxRemoteStderrBytes}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	refs, parseErr := parseLsRemoteStream(stdout)
	if parseErr != nil {
		// We are abandoning the read. Kill the child FIRST: it may still be
		// writing into a pipe nobody is draining, and Wait would otherwise
		// block until WaitDelay expired.
		cancel()
	}
	// Unconditional: this is the only place the child is reaped, and it must
	// run whether the parse succeeded, tripped a limit, or the context died.
	waitErr := cmd.Wait()

	if parseErr != nil {
		return nil, parseErr
	}
	if waitErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("git ls-remote %s: %w: %s", redactURL(remote), waitErr, msg)
		}
		return nil, fmt.Errorf("git ls-remote %s: %w", redactURL(remote), waitErr)
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

func (b *boundedWriter) Write(p []byte) (int, error) {
	if b.remaining <= 0 {
		return len(p), nil // discard, but never signal an error back to exec
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.w.Write(p)
	b.remaining -= n
	return len(p), err
}

// parseLsRemoteStream parses `git ls-remote`'s "<sha>\t<refname>" output as it
// arrives, under the ceilings declared above. It returns ErrRemoteRefLimit
// (wrapped, with which ceiling tripped) rather than a partial map, because a
// truncated ref set could silently turn "the ref exists" into "the ref is
// gone" — the exact false verdict this package's contract forbids.
func parseLsRemoteStream(r io.Reader) (map[string]string, error) {
	limited := &io.LimitedReader{R: r, N: MaxRemoteOutputBytes + 1}
	br := bufio.NewReader(limited)
	refs := map[string]string{}
	consumed := int64(0)

	for {
		line, err := readBoundedLine(br, MaxRemoteRefLineBytes)
		consumed += int64(len(line))
		if consumed > MaxRemoteOutputBytes {
			return nil, fmt.Errorf("%w: output exceeded %d bytes", ErrRemoteRefLimit, MaxRemoteOutputBytes)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}

		if trimmed := strings.TrimSpace(line); trimmed != "" {
			sha, name, found := strings.Cut(trimmed, "\t")
			if found {
				if len(refs) >= MaxRemoteRefs {
					return nil, fmt.Errorf("%w: more than %d refs advertised", ErrRemoteRefLimit, MaxRemoteRefs)
				}
				refs[name] = sha
			}
		}

		if errors.Is(err, io.EOF) {
			return refs, nil
		}
	}
}

// readBoundedLine reads one newline-terminated line of at most maxBytes. A
// longer line is not a ref (real ref names are orders of magnitude shorter),
// so it is refused outright rather than truncated into a plausible-looking
// half-ref.
func readBoundedLine(br *bufio.Reader, maxBytes int) (string, error) {
	var b strings.Builder
	for {
		chunk, err := br.ReadString('\n')
		if b.Len()+len(chunk) > maxBytes {
			return "", fmt.Errorf("%w: a single ref line exceeded %d bytes", ErrRemoteRefLimit, maxBytes)
		}
		b.WriteString(chunk)
		if err != nil {
			return b.String(), err
		}
		if strings.HasSuffix(chunk, "\n") {
			return b.String(), nil
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

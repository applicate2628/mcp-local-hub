package api

import (
	"context"
	"fmt"
	"io"
	"os"
)

// serena_marker_read.go — the SINGLE hardened entry point for reading a
// cloned `.serena/project.yml` marker.
//
// SECURITY BOUNDARY. A `.serena/project.yml` marker lives inside a CLONED
// repository tree. That tree is UNTRUSTED INPUT: a malicious upstream, a
// poisoned dependency checked out into a workspace, or any path an MCP
// tool-call can name and reach (the GUI serena router invokes
// AutoRegisterSerenaWorkspace on a path miss) can ship a hostile marker.
// A hostile marker can be a symlink/FIFO/device redirecting the read, a
// huge file aimed at memory/CPU exhaustion, or a deeply-nested YAML
// document. Every code path that reads `.serena/project.yml` MUST funnel
// through ReadUntrustedSerenaProjectYML so the hardening is applied once,
// in one place, with no inconsistent sibling reader drifting out of sync.
// It is exported because BOTH call-sites must funnel through it and one of
// them lives in package cli (internal/cli/workspace_cmd.go's
// readSerenaProjectLanguages, the `mcphub workspace register` path).
//
// The hardening shape below is TOCTOU-sound: Lstat-refuse-non-regular →
// open → re-Stat the OPENED handle → os.SameFile(lstat, opened) to close
// the Lstat→Open swap window → bounded read via io.LimitReader. The
// LimitReader+len check is the authoritative size bound; a pre-open
// Size() fast-path is a cheap early reject only.

// maxSerenaProjectYMLBytes caps the marker size. A legitimate
// `.serena/project.yml` is a handful of lines (project name + a short
// language list); 64 KiB is far above any real marker and well below a
// memory-exhaustion threat. This size cap is ALSO the resource bound on
// the downstream YAML parse: gopkg.in/yaml.v3 exposes no public
// depth/alias/anchor knob (no decoder option for nesting depth or
// billion-laughs alias expansion as of v3.0.1 — see go.mod), so bounding
// the INPUT to 64 KiB is the available, sufficient bound. A 64 KiB
// document cannot encode a nesting depth or alias-expansion fan-out large
// enough to matter for this two-field struct decode; if yaml.v3 ever
// gains explicit limits, apply them here in addition to the size cap.
const maxSerenaProjectYMLBytes = 64 * 1024

// ReadUntrustedSerenaProjectYML reads the marker file at path, treating it
// as untrusted supply-chain input, and returns its raw bytes for the
// caller to YAML-unmarshal into its own minimal shape. It is the single
// hardened entry for `.serena/project.yml` reads (see the file header).
//
//   - ctx == nil is normalized to context.Background(); ctx.Err() is
//     checked before the Lstat and after the read so a cancelled
//     registration aborts promptly. Cancellation is wrapped with %w so
//     errors.Is(err, context.Canceled) / context.DeadlineExceeded holds.
//   - The path must be a REGULAR file. A symlink, FIFO, device, socket, or
//     directory is refused with an explicit error and is NEVER followed
//     (os.Lstat, not os.Stat).
//   - After open, the OPENED handle is re-Stat'd and compared to the
//     pre-open Lstat result via os.SameFile so a swap between the Lstat
//     and the open (attacker replaces the regular file with a symlink in
//     the window) is caught; the opened handle is also re-checked to be a
//     regular file. f.Close() runs on every return path (no handle leak).
//   - The read is bounded by maxSerenaProjectYMLBytes via
//     io.LimitReader(f, max+1); a result longer than max is rejected. This
//     LimitReader+len check is the authority; the pre-open Size() check is
//     only a cheap fast-path early reject.
func ReadUntrustedSerenaProjectYML(ctx context.Context, path string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("serena marker: aborted before reading %s: %w", path, err)
	}

	// Lstat (NOT Stat) so a symlink at the leaf is observed as a symlink
	// and refused, never followed.
	lstatFI, err := os.Lstat(path)
	if err != nil {
		// Propagate the bare error (no %w wrap) so callers can still use
		// both errors.Is(err, os.ErrNotExist) AND os.IsNotExist(err) to
		// detect a missing marker — the `mcphub workspace register` path
		// distinguishes not-found to print "run bootstrap first" guidance,
		// and os.IsNotExist does not unwrap %w chains.
		return nil, err
	}
	if !lstatFI.Mode().IsRegular() {
		return nil, fmt.Errorf("serena marker: %s must be a regular file, not %s (untrusted clone marker; symlinks/FIFOs/devices/dirs are refused, not followed)", path, lstatFI.Mode().Type())
	}
	// Cheap fast-path: reject an obviously-oversized file before opening.
	// The authoritative bound is the LimitReader+len check below.
	if lstatFI.Size() > maxSerenaProjectYMLBytes {
		return nil, fmt.Errorf("serena marker: %s is %d bytes, exceeds the %d-byte cap (untrusted clone marker)", path, lstatFI.Size(), maxSerenaProjectYMLBytes)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("serena marker: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	openedFI, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("serena marker: stat opened %s: %w", path, err)
	}
	if !openedFI.Mode().IsRegular() {
		return nil, fmt.Errorf("serena marker: opened %s is not a regular file (%s); refusing (untrusted clone marker)", path, openedFI.Mode().Type())
	}
	// Close the Lstat→Open swap window: the handle we opened must be the
	// same filesystem object we Lstat'd. If an attacker swapped the path to
	// a different file (e.g. a symlink) between the Lstat and the Open,
	// SameFile is false and we refuse.
	if !os.SameFile(lstatFI, openedFI) {
		return nil, fmt.Errorf("serena marker: %s changed between inspection and open (possible TOCTOU swap); refusing", path)
	}

	// Authoritative size bound: read at most max+1 bytes, then reject if the
	// content exceeded the cap. Robust even if the file grew after the
	// pre-open Size() check.
	data, err := io.ReadAll(io.LimitReader(f, maxSerenaProjectYMLBytes+1))
	if err != nil {
		return nil, fmt.Errorf("serena marker: read %s: %w", path, err)
	}
	if len(data) > maxSerenaProjectYMLBytes {
		return nil, fmt.Errorf("serena marker: %s exceeds the %d-byte cap (untrusted clone marker)", path, maxSerenaProjectYMLBytes)
	}

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("serena marker: aborted after reading %s: %w", path, err)
	}
	return data, nil
}

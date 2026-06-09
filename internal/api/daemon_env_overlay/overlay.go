// overlay.go — Overlay struct + Load() parser for the per-daemon env
// overlay file (Task 2.2 of the v0.5.x Servers matrix revamp).
//
// Spec: docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"Env overlay file".
//
// The overlay file is a YAML document indexed by canonical task name
// (leading backslash form — see NormalizeOverlayKey in normalize.go).
// Each daemon row carries:
//   - env: literal KEY=VALUE pairs (the only template token supported
//     in values is ${parent_path}, handled by ExpandParentPath in
//     parent_path_expand.go).
//   - source: provenance marker ("auto-discovery" or "operator").
//   - discovered_at / modified_at: RFC3339Nano timestamps for the
//     GUI's Servers matrix.
//
// Load enforces four safety properties before YAML decode:
//   - The path must open via hardenedOpen (platform-specific
//     symlink/reparse-point refusal in read_hardening_{posix,windows}.go).
//   - The opened file must be a regular file (rejects symlinks /
//     devices / pipes via f.Stat().Mode().IsRegular()).
//   - The file's PARENT directory must pass the read-side parent-DACL
//     gate (checkStateDirParentReadSafe in parent_check.go) — the
//     read-side mirror of the write-side gate. This is what makes the
//     MCPHUB_REQUIRE_SINGLE_USER_HOME strict posture and the
//     MCPHUB_ALLOW_UNHARDENED_STATE_READ relax opt-out actually fire on
//     the load path: on a broadened parent, strict mode refuses the
//     read so a co-resident principal who swapped the directory entry
//     cannot inject attacker-controlled env into supervised daemons.
//   - The file body must be UTF-8 and at most MaxOverlaySize bytes.
//
// Decoding uses yaml.v3's KnownFields(true) so unknown keys are
// rejected — operators see typo errors at load time, not as silent
// drops.

package daemon_env_overlay

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// errTransientOverlayRead is the sentinel that marks an open/stat I/O
// failure consistent with a concurrent atomic-rename of the overlay
// file by a writer (GUI WriteOverlay → api.WriteStateFileBytesLockHeld).
// The writer publishes via an atomic rename (POSIX renameat /
// Windows FileRenameInformationEx with FILE_RENAME_POSIX_SEMANTICS), so
// a reader can only ever observe the COMPLETE old file, the COMPLETE
// new file, or — on Windows, during the brief in-kernel replace window
// — a transient open/stat error (sharing violation / momentary
// not-found). There is never a torn/partial read.
//
// Load retries exactly once when loadOnce returns an error matching this
// sentinel, so the spawn-time "live overlay reload" reads the
// just-applied edit instead of silently degrading to the stale cached
// startup snapshot (the PR#222 P1-1 regression). Genuine parse failures
// (oversize, non-UTF-8, malformed YAML, irregular file, parent-gate
// rejection) are NOT tagged transient and are NOT retried — they surface
// to the caller unchanged so the fail-loud (loadOverlayAtStartup) and
// graceful-degradation (spawn closure) behaviors both hold.
var errTransientOverlayRead = errors.New("daemon_env_overlay: transient overlay read (concurrent rename); retry candidate")

// transientReadError tags an open/stat I/O failure as a retry candidate
// while preserving the underlying error verbatim. It satisfies
// errors.Is(err, errTransientOverlayRead) (for the retry decision) and
// exposes the underlying error via Unwrap so the final surfaced error,
// after retries are exhausted, is the clean open/stat error with no
// internal "retry candidate" framing.
type transientReadError struct{ inner error }

func (e *transientReadError) Error() string { return e.inner.Error() }
func (e *transientReadError) Unwrap() error { return e.inner }
func (e *transientReadError) Is(target error) bool { return target == errTransientOverlayRead }

// underlyingReadError returns the open/stat error a transientReadError
// wraps, or err unchanged when err is not a transientReadError.
func underlyingReadError(err error) error {
	var t *transientReadError
	if errors.As(err, &t) {
		return t.inner
	}
	return err
}

// MaxOverlaySize is the per-file size cap enforced by Load. Anything
// larger is rejected before YAML decode runs. 64 KiB is comfortably
// above the realistic ceiling — a typical row is well under 1 KiB and
// a hundred daemons stays under 16 KiB — while preventing accidental
// or hostile huge files from being read into memory.
const MaxOverlaySize = 64 * 1024

// openOverlayFile is the indirection loadOnce uses to open the overlay
// file. In production it is hardenedOpen (platform-specific
// symlink/reparse-point refusal). It is a package var solely so tests can
// inject a deterministic transient-then-present sequence — e.g. an
// fs.ErrNotExist on the first call and a real open on the retry — to
// exercise Load's retry/`still-missing → emptyOverlay()` classification
// without relying on a real OS race window (which only surfaces on
// Windows). Tests MUST restore it; production never reassigns it.
var openOverlayFile = hardenedOpen

// Overlay is the top-level YAML document persisted at
// daemon-env-overrides.yaml under the state dir.
type Overlay struct {
	Version int                   `yaml:"version"`
	Daemons map[string]DaemonRow `yaml:"daemons"`
}

// DaemonRow holds the per-daemon overlay payload. Keys in the parent
// Overlay.Daemons map are canonical task names (see NormalizeOverlayKey).
type DaemonRow struct {
	Env          map[string]string `yaml:"env"`
	Source       string            `yaml:"source,omitempty"`
	DiscoveredAt string            `yaml:"discovered_at,omitempty"`
	ModifiedAt   string            `yaml:"modified_at,omitempty"`
}

// Load reads and parses the overlay file at path.
//
// Behavior:
//   - Missing file (errors.Is(err, fs.ErrNotExist) on hardenedOpen,
//     STILL missing after the one transient retry below) returns an
//     empty Overlay{Version: 1, Daemons: {}} and nil error. This lets
//     callers treat "no file" as "no overlay" without a wrapping
//     exists-check on every call site.
//   - File present: open via hardenedOpen (Task 2.4 replaces the stub
//     with platform-specific reparse-point / O_NOFOLLOW refusal);
//     reject non-regular files; cap read at MaxOverlaySize+1 via
//     io.LimitReader and fail if the actual byte count exceeds the
//     cap; reject non-UTF-8 bytes; decode with KnownFields(true) so
//     unknown YAML keys surface as errors.
//
// The returned Overlay always has a non-nil Daemons map.
//
// Concurrency note: a single transient open/stat failure (consistent
// with a writer's in-flight atomic rename — see errTransientOverlayRead)
// is retried exactly once before surfacing. fs.ErrNotExist is PART of
// that transient class: the Windows atomic-replace window (a writer's
// FileRenameInformationEx) can briefly surface a momentary not-found, so
// converting the FIRST ErrNotExist straight to emptyOverlay() would
// silently drop a just-applied operator override when a spawn-time reload
// lands in that window. Instead the first ErrNotExist is retried once; an
// overlay that is STILL missing after the retry is the legitimate "no
// overlay file exists" case and returns emptyOverlay(). Genuine parse
// failures are never retried.
func Load(path string) (*Overlay, error) {
	ov, err := loadOnce(path)
	if err == nil {
		return ov, nil
	}
	if errors.Is(err, errTransientOverlayRead) {
		// One retry: the only error class tagged transient is an
		// open/stat I/O failure during a concurrent atomic rename
		// (including the momentary not-found the Windows replace window
		// can surface). The rename is atomic, so the second attempt
		// observes the COMPLETE old or COMPLETE new file (never partial),
		// or — for a genuinely absent file — the same not-found again.
		ov, err = loadOnce(path)
		if err == nil {
			return ov, nil
		}
		// Surface the underlying open/stat error (strip the transient
		// tag) so callers never see the internal "retry candidate"
		// framing. underlyingReadError is a no-op for a non-transient
		// error from the retry (e.g. the file now exists but is
		// malformed), so that surfaces verbatim.
		surfaced := underlyingReadError(err)
		// Genuinely-absent file: the not-found persisted across the
		// retry, so this is the legitimate "no overlay file" case rather
		// than a momentary replace-window miss. Return the empty overlay
		// exactly as the pre-retry missing-file contract did. (The
		// stripped error preserves the fs.ErrNotExist chain via %w.)
		if errors.Is(surfaced, fs.ErrNotExist) {
			return emptyOverlay(), nil
		}
		return nil, surfaced
	}
	// Non-transient failure on the first attempt (parent-gate rejection,
	// irregular file, oversize, non-UTF-8, malformed YAML) — surface
	// verbatim.
	return nil, err
}

// loadOnce performs a single open → stat → IsRegular → parent-gate →
// size-cap → UTF-8 → decode pass. Open/stat I/O failures — INCLUDING
// fs.ErrNotExist — are tagged with transientReadError (matches
// errTransientOverlayRead) so Load can decide whether to retry; all other
// failures (parent-gate rejection, irregular file, oversize, non-UTF-8,
// malformed YAML) are returned untagged and are NOT retried.
//
// fs.ErrNotExist is deliberately NOT short-circuited to emptyOverlay()
// here: on Windows the writer's atomic-replace window can surface a
// momentary not-found, and converting the first miss to an empty overlay
// would drop a just-applied override before Load gets a chance to retry.
// loadOnce stays a pure single pass; Load owns the retry decision and the
// "still missing after the retry → emptyOverlay()" interpretation. The
// transient wrapper preserves the underlying error via %w, so the
// fs.ErrNotExist chain survives for Load's post-retry errors.Is check.
func loadOnce(path string) (*Overlay, error) {
	f, err := openOverlayFile(path)
	if err != nil {
		// Open failure: either "missing file" (fs.ErrNotExist) or — on
		// Windows — the sharing-violation/not-found class that can
		// transiently appear during a writer's atomic rename. Both are
		// tagged transient so Load retries once. A genuinely-absent file
		// still reports fs.ErrNotExist on the retry, which Load maps to
		// emptyOverlay(); a momentary replace-window miss resolves to the
		// COMPLETE new file on the retry.
		return nil, &transientReadError{inner: fmt.Errorf("%s: open: %w", path, err)}
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		// Stat on an open handle failing is likewise consistent with a
		// concurrent rename swapping the underlying entry. Tag transient
		// for retry.
		return nil, &transientReadError{inner: fmt.Errorf("%s: stat: %w", path, err)}
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file (mode=%s)", path, info.Mode())
	}

	// Read-side parent-DACL gate (Sec-F1): the read-side mirror of the
	// write-side secureWriteStateFileWithOperatorOpt gate. Runs AFTER
	// the regular-file check and BEFORE the size read, exactly per the
	// plan's wiring (servers-matrix revamp plan §"Load ... ->
	// checkStateDirParentReadSafe(parentDir) -> size cap"). The gate
	// already encodes strict / explicit-relax / default-refuse
	// semantics (MCPHUB_REQUIRE_SINGLE_USER_HOME /
	// MCPHUB_ALLOW_UNHARDENED_STATE_READ); propagate its error verbatim
	// so the operator posture is identical on the read and write paths.
	// Without this, a broadened %LOCALAPPDATA% parent that grants a
	// co-resident principal FILE_DELETE_CHILD would let that principal
	// swap daemon-env-overrides.yaml for an attacker-owned regular file
	// that opens fine here, injecting attacker-controlled env (PATH
	// binary-planting) into every supervised daemon.
	if gateErr := checkStateDirParentReadSafe(filepath.Dir(path)); gateErr != nil {
		return nil, gateErr
	}

	// Read at most MaxOverlaySize+1 so we can detect overrun even when
	// the OS reports a smaller size than the actual byte count
	// (concurrent appends, sparse files, etc.).
	raw, err := io.ReadAll(io.LimitReader(f, int64(MaxOverlaySize)+1))
	if err != nil {
		return nil, fmt.Errorf("%s: read: %w", path, err)
	}
	if len(raw) > MaxOverlaySize {
		return nil, fmt.Errorf("%s: file exceeds %d-byte cap", path, MaxOverlaySize)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%s: not valid UTF-8", path)
	}

	// Empty file → empty overlay (idiomatic for "operator created the
	// file but has not added any rows yet"). yaml.Decode on an empty
	// reader returns io.EOF; map that to the empty default instead of
	// failing the caller.
	if len(raw) == 0 {
		return emptyOverlay(), nil
	}

	var overlay Overlay
	dec := yaml.NewDecoder(bytesReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&overlay); err != nil {
		if errors.Is(err, io.EOF) {
			return emptyOverlay(), nil
		}
		return nil, fmt.Errorf("%s: decode: %w", path, err)
	}

	// Ensure the returned struct's map is non-nil even when the YAML
	// document omits the daemons key entirely.
	if overlay.Daemons == nil {
		overlay.Daemons = map[string]DaemonRow{}
	}
	return &overlay, nil
}

// emptyOverlay returns the canonical zero-rows overlay. Centralized so
// every "no file" / "empty file" / "missing daemons key" path returns
// the same shape.
func emptyOverlay() *Overlay {
	return &Overlay{Version: 1, Daemons: map[string]DaemonRow{}}
}

// bytesReader wraps a []byte in an io.Reader without pulling in
// bytes.Reader's full surface. Using a small helper keeps the import
// list tight and avoids leaking a *bytes.Reader value the caller does
// not need. (Kept private; only used inside Load.)
func bytesReader(b []byte) io.Reader {
	return &sliceReader{data: b}
}

type sliceReader struct {
	data []byte
	off  int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}


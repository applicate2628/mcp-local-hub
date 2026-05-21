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
// Load enforces three safety properties before YAML decode:
//   - The path must open via hardenedOpen (Task 2.4 swaps in the
//     platform-specific implementation; this task ships a stub).
//   - The opened file must be a regular file (rejects symlinks /
//     devices / pipes via f.Stat().Mode().IsRegular()).
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
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// MaxOverlaySize is the per-file size cap enforced by Load. Anything
// larger is rejected before YAML decode runs. 64 KiB is comfortably
// above the realistic ceiling — a typical row is well under 1 KiB and
// a hundred daemons stays under 16 KiB — while preventing accidental
// or hostile huge files from being read into memory.
const MaxOverlaySize = 64 * 1024

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
//   - Missing file (errors.Is(err, fs.ErrNotExist) on hardenedOpen)
//     returns an empty Overlay{Version: 1, Daemons: {}} and nil error.
//     This lets callers treat "no file" as "no overlay" without a
//     wrapping exists-check on every call site.
//   - File present: open via hardenedOpen (Task 2.4 replaces the stub
//     with platform-specific reparse-point / O_NOFOLLOW refusal);
//     reject non-regular files; cap read at MaxOverlaySize+1 via
//     io.LimitReader and fail if the actual byte count exceeds the
//     cap; reject non-UTF-8 bytes; decode with KnownFields(true) so
//     unknown YAML keys surface as errors.
//
// The returned Overlay always has a non-nil Daemons map.
func Load(path string) (*Overlay, error) {
	f, err := hardenedOpen(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return emptyOverlay(), nil
		}
		return nil, fmt.Errorf("%s: open: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s: stat: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file (mode=%s)", path, info.Mode())
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


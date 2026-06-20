// internal/gui/decode_body.go
package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// Body-size caps for the GUI's POST/PUT JSON handlers. The GUI listener is
// bound to 127.0.0.1 (same-origin/local-page threat only), so this is
// hardening hygiene rather than a remote-DoS fix: it bounds the decode/parse
// work a same-origin local page can force, and it rejects a small-valid-JSON
// body padded with megabytes of trailing garbage (the streaming json.Decoder
// would otherwise have read the leading value before the trailing junk is
// even seen). Two named caps keep the per-endpoint choice explicit instead of
// scattering magic literals:
//
//   - maxManifestBodyBytes — generous, for the YAML-bearing bodies the
//     Add/Edit-server + manifest-validate paths carry (a full manifest can be
//     a few KiB of YAML, but an operator pasting a large config should not be
//     clipped).
//   - maxControlBodyBytes — tight, for the small control bodies (a key/value,
//     a client list, a server name, a schedule string, a flag).
const (
	maxManifestBodyBytes int64 = 1 << 20  // 1 MiB
	maxControlBodyBytes  int64 = 64 << 10 // 64 KiB
)

// Sentinel errors returned by decodeJSONBodyLimited so callers can map each
// failure to the HTTP status their endpoint's error convention expects
// (writeAPIError JSON envelope vs http.Error plain text). The decode-failure
// path (malformed/empty JSON) is returned wrapped so a caller that previously
// surfaced err.Error() keeps a useful detail string.
var (
	// errBodyTooLarge — the request body exceeded the per-endpoint cap.
	// Caller maps to 413 Request Entity Too Large.
	errBodyTooLarge = errors.New("request body too large")
	// errBodyTrailingData — a complete JSON value was decoded but extra
	// non-whitespace bytes followed it (a second value, or trailing garbage
	// under the cap). Caller maps to 400 Bad Request.
	errBodyTrailingData = errors.New("request body has trailing data after JSON value")
)

// decodeJSONBodyLimited caps r.Body at maxBytes via http.MaxBytesReader,
// decodes a single JSON value into v, and rejects any trailing data after it.
// It returns one of:
//
//   - errBodyTooLarge        → the body exceeded maxBytes (map to 413).
//   - errBodyTrailingData    → a valid JSON value was followed by extra bytes
//     (a second value or non-whitespace garbage under the cap; map to 400).
//   - any other non-nil err  → malformed or empty JSON (map to 400, same as the
//     pre-existing bare-decode error path; the wrapped err carries detail).
//   - nil                    → exactly one JSON value decoded, nothing trailing.
//
// http.MaxBytesReader also writes the 413 response headers itself when used by
// the net/http server, but GUI handlers own their error envelope, so callers
// must inspect the returned sentinel and write the response. The MaxBytesReader
// still performs the load-bearing job here: bounding how many bytes the decoder
// will read before failing.
func decodeJSONBodyLimited(w http.ResponseWriter, r *http.Request, v any, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errBodyTooLarge
		}
		return err
	}
	// Reject trailing data: a second Decode of the same stream must return
	// io.EOF (nothing but optional whitespace left). A megabytes-of-garbage
	// body past a small leading value trips this as a json.SyntaxError (the
	// streaming decoder fails on the first trailing token before reading far
	// enough to hit the cap), so it is rejected here rather than via 413 —
	// either way the oversized-trailing body never reaches the handler.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errBodyTooLarge
		}
		return errBodyTrailingData
	}
	return nil
}

// decodeBodyStatusCode maps a decodeJSONBodyLimited error to the HTTP status a
// handler should return: 413 for the size cap, 400 for trailing-data and for
// any malformed/empty-JSON decode error. It is the single owner of that
// status mapping so every handler agrees on 413-vs-400 without re-deriving it.
func decodeBodyStatusCode(err error) int {
	if errors.Is(err, errBodyTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

// decodeBodyErrorText maps a decodeJSONBodyLimited error to a plain-text
// message for handlers that respond via http.Error (or a {detail} envelope
// field). It keeps the size/trailing sentinels' stable wording and, for the
// generic decode failure, preserves the "invalid JSON: <detail>" form the
// pre-existing bare-decode handlers emitted.
func decodeBodyErrorText(err error) string {
	switch {
	case errors.Is(err, errBodyTooLarge):
		return errBodyTooLarge.Error()
	case errors.Is(err, errBodyTrailingData):
		return errBodyTrailingData.Error()
	default:
		return "invalid JSON: " + err.Error()
	}
}

// decodeBodyErrorCode maps a decodeJSONBodyLimited error to the short string
// code used by the daemons.go {error, detail} envelope. The generic decode
// failure keeps the pre-existing "bad_json" code; the size cap gets a distinct
// "body_too_large" code so the 413 case is machine-distinguishable.
func decodeBodyErrorCode(err error) string {
	switch {
	case errors.Is(err, errBodyTooLarge):
		return "body_too_large"
	case errors.Is(err, errBodyTrailingData):
		return "trailing_data"
	default:
		return "bad_json"
	}
}

// writeDecodeBodyError is the writeAPIError-convention shim for the common case
// (handlers that already surface decode failures through the JSON error
// envelope). It maps the sentinel to 413/400 via decodeBodyStatusCode and emits
// the envelope with the caller-supplied code prefix used for the size/trailing
// cases; the underlying decode error detail is preserved for the 400 path.
func writeDecodeBodyError(w http.ResponseWriter, err error, code string) {
	status := decodeBodyStatusCode(err)
	switch {
	case errors.Is(err, errBodyTooLarge):
		writeAPIError(w, errBodyTooLarge, status, code)
	case errors.Is(err, errBodyTrailingData):
		writeAPIError(w, errBodyTrailingData, status, code)
	default:
		writeAPIError(w, fmt.Errorf("invalid JSON: %w", err), status, code)
	}
}

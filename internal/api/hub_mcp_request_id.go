// hub_mcp_request_id.go — Phase 3 Task 3.1 (G4 unified hub MCP).
//
// requestIDKey: a discriminated, losslessly canonicalized, comparable
// string form of a JSON-RPC 2.0 id field. Used as the Go map key for
// hubSession.InFlightRequests because json.RawMessage ([]byte) is not
// comparable.
//
// The key carries TWO pieces of information:
//
//   1. The id's type ("s:" for JSON string, "n:" for JSON number) so
//      `"1"` and `1` never collide.
//   2. A canonical representation that collapses numerically-equivalent
//      encodings (e.g., `1`, `1.0`, `1e0` all bucket as `n:1`) WITHOUT
//      going through float64 — which would lose precision for integers
//      beyond 2^53 and conflate distinct ids.
//
// MCP 2025-11-25 narrows the JSON-RPC base spec by FORBIDDING null
// request ids (the JSON-RPC 2.0 spec merely discouraged them).
// newRequestIDKey enforces that narrower rule by returning an error
// on `null`. Arrays, objects, and booleans are also rejected; the
// JSON-RPC grammar permits only string and number.
//
// The number canonicalization follows seven pure-string steps so we
// never allocate big.Int / big.Rat on the hot path:
//
//   1. Reject leading `+` (JSON grammar forbids it; json.Decoder
//      grammar-validates).
//   2. Strip leading zeros from the integer part (keep a single `0`
//      if integer part is otherwise empty: `0.5` stays `0.5`).
//   3. If an exponent is present: normalize sign (drop `+`, keep `-`),
//      strip leading zeros from the exponent's magnitude (`1e0010` →
//      `1e10`), and drop the entire `eN` suffix if the magnitude
//      is zero (`1e0` → `1`).
//   4. If a fractional part is present: strip trailing zeros after the
//      decimal point. If the fractional becomes empty after the strip,
//      drop the decimal point entirely (`1.50` → `1.5`; `1.00` → `1`).
//   5. Normalize letter case for the exponent prefix to lowercase `e`
//      (`1E10` → `1e10`).
//   6. Treat `-0` (with any fractional/exponent that still evaluates
//      to zero) as `0` (no negative zero in the canonical form).
//   7. Preserve every other digit verbatim — never apply numeric
//      arithmetic.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Per-hub session model" requestIDKey definition (codex r4 F6 +
// r5 P1 + r6 MED closures). Plan: Task 3.1.

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// requestIDKey is the comparable form of a JSON-RPC id. NEVER store
// this anywhere readable by an attacker — the underlying id may carry
// caller-chosen content the operator considers sensitive. It's an
// in-memory routing key, period.
type requestIDKey string

// newRequestIDKey validates + canonicalizes a raw JSON-RPC id field.
// Returns ("", err) on:
//
//   - empty / whitespace-only input
//   - JSON null (MCP forbids null ids)
//   - JSON array / object / boolean (JSON-RPC grammar forbids these)
//   - leading `+` in a number (JSON grammar forbids)
//   - any json.Decoder parse error
//
// On success, the returned key carries:
//
//   - `s:` followed by the unescaped JSON string body, OR
//   - `n:` followed by the canonical decimal-string form (see
//     canonicalizeJSONNumber for the seven canonicalization rules).
func newRequestIDKey(raw json.RawMessage) (requestIDKey, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", errors.New("invalid request: empty id")
	}

	// JSON null: MCP narrows the JSON-RPC base spec to forbid null
	// ids on the request path. Return an error so the handler can
	// emit -32600 "Invalid Request: MCP requires non-null id".
	if bytes.Equal(trimmed, []byte("null")) {
		return "", errors.New("invalid request: MCP requires non-null id")
	}

	// JSON string: starts with `"`. Decode through json.Unmarshal so
	// escape sequences resolve, then re-prefix with the type tag.
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", fmt.Errorf("invalid request: %w", err)
		}
		return requestIDKey("s:" + s), nil
	}

	// JSON number — discriminator covers anything that's neither
	// string nor null. Catch booleans, arrays, objects explicitly so
	// the error messages stay useful. json.Decoder.UseNumber preserves
	// the raw decimal string instead of demoting to float64.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var t any
	if err := dec.Decode(&t); err != nil {
		return "", fmt.Errorf("invalid request: %w", err)
	}
	// dec.Decode consumes one token. If there's still data left in
	// the input (e.g. `1 2` or `1.5.6`), it's malformed — reject.
	if dec.More() {
		return "", errors.New("invalid request: id contains trailing data")
	}
	num, ok := t.(json.Number)
	if !ok {
		// arrays / objects / booleans — rejected by MCP grammar.
		return "", errors.New("invalid request: id must be string or number")
	}

	// json.Number permits a leading `+` despite the JSON grammar
	// forbidding it. Reject defensively so the canonical form never
	// admits a value the spec wouldn't allow.
	s := string(num)
	if len(s) > 0 && s[0] == '+' {
		return "", errors.New("invalid request: number id may not have leading +")
	}

	canon, err := canonicalizeJSONNumber(s)
	if err != nil {
		return "", err
	}
	return requestIDKey("n:" + canon), nil
}

// canonicalizeJSONNumber returns the canonical decimal-string form of
// a JSON number per the seven rules documented in the package comment.
//
// PRE: s is a json.Number string accepted by json.Decoder.UseNumber.
//      The leading-plus check has ALREADY been applied by the caller.
//
// The implementation is intentionally pure string manipulation —
// never any numeric arithmetic — so values larger than uint64
// canonicalize without precision loss.
func canonicalizeJSONNumber(s string) (string, error) {
	if s == "" {
		return "", errors.New("invalid number: empty")
	}

	// Step 1 — Capture sign. JSON grammar permits a leading `-` only.
	neg := false
	if s[0] == '-' {
		neg = true
		s = s[1:]
		if s == "" {
			return "", errors.New("invalid number: lone minus")
		}
	}

	// Step 2 — Split into integer / fractional / exponent parts.
	// Defensive: re-validate the rough shape json.Decoder already
	// accepts. Use lowercase compare for the exponent prefix.
	intPart, fracPart, expPart := splitJSONNumber(s)
	if intPart == "" {
		// json grammar requires at least one digit before `.` and
		// before `e`. json.Decoder enforces this — defensive check.
		return "", fmt.Errorf("invalid number: %q", s)
	}

	// Step 3 — Validate the digit-only sub-parts. Defensive (the
	// stdlib should have caught this, but a malformed json.Number
	// slipping through would corrupt the canonical map key).
	if !allDigits(intPart) {
		return "", fmt.Errorf("invalid number: integer part %q", intPart)
	}
	if fracPart != "" && !allDigits(fracPart) {
		return "", fmt.Errorf("invalid number: fractional part %q", fracPart)
	}
	// expPart still carries its sign; validate the digit body.
	expSign, expMag, err := splitExponent(expPart)
	if err != nil {
		return "", err
	}

	// Step 4 — Strip leading zeros from the integer magnitude. Keep
	// a single `0` if the integer body would otherwise be empty.
	intMag := strings.TrimLeft(intPart, "0")
	if intMag == "" {
		intMag = "0"
	}

	// Step 5 — Strip trailing zeros from the fractional part. Drop
	// the decimal point altogether if nothing remains.
	fracMag := strings.TrimRight(fracPart, "0")

	// Step 6 — Strip leading zeros from the exponent magnitude.
	expMag = strings.TrimLeft(expMag, "0")
	// An empty exponent magnitude after stripping (e.g. `1e0`) → no
	// exponent suffix in the canonical form.

	// Step 7 — Negative zero: if the integer body is "0" and the
	// fractional body is empty (or all-zeros, already stripped),
	// the result is +0. Drop the sign.
	if neg && intMag == "0" && fracMag == "" {
		neg = false
	}

	// Reassemble.
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(intMag)
	if fracMag != "" {
		b.WriteByte('.')
		b.WriteString(fracMag)
	}
	if expMag != "" {
		b.WriteByte('e')
		if expSign == '-' {
			b.WriteByte('-')
		}
		b.WriteString(expMag)
	}
	return b.String(), nil
}

// splitJSONNumber breaks a json number's body into (integer, fractional,
// exponent) sub-strings. The exponent sub-string still carries any sign
// byte (`+` or `-` or "" for unsigned).
//
// PRE: s has the leading `-` already stripped by the caller.
func splitJSONNumber(s string) (intPart, fracPart, expPart string) {
	dotIdx := -1
	expIdx := -1
	for i, c := range s {
		switch {
		case c == '.' && dotIdx < 0 && expIdx < 0:
			dotIdx = i
		case (c == 'e' || c == 'E') && expIdx < 0:
			expIdx = i
		}
	}
	switch {
	case dotIdx < 0 && expIdx < 0:
		intPart = s
	case dotIdx < 0 && expIdx >= 0:
		intPart = s[:expIdx]
		expPart = s[expIdx+1:]
	case dotIdx >= 0 && expIdx < 0:
		intPart = s[:dotIdx]
		fracPart = s[dotIdx+1:]
	default:
		// both present, and dotIdx < expIdx (json grammar guarantees).
		intPart = s[:dotIdx]
		fracPart = s[dotIdx+1 : expIdx]
		expPart = s[expIdx+1:]
	}
	return intPart, fracPart, expPart
}

// splitExponent returns (sign-byte, magnitude, err). sign-byte is `+`,
// `-`, or 0 when the exponent has no explicit sign. expPart "" returns
// (0, "", nil) — caller treats empty magnitude as "no exponent".
func splitExponent(expPart string) (byte, string, error) {
	if expPart == "" {
		return 0, "", nil
	}
	sign := byte(0)
	rest := expPart
	if expPart[0] == '+' || expPart[0] == '-' {
		sign = expPart[0]
		rest = expPart[1:]
	}
	if rest == "" {
		return 0, "", fmt.Errorf("invalid number: empty exponent")
	}
	if !allDigits(rest) {
		return 0, "", fmt.Errorf("invalid number: exponent %q", expPart)
	}
	return sign, rest, nil
}

// allDigits returns true iff every byte of s is in '0'..'9'.
// Empty string returns false (an empty digit run is invalid).
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

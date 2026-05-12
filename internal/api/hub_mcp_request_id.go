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
// Number canonicalization is pure-string mantissa+exponent reduction
// — never any float64 arithmetic — so values larger than 2^53
// canonicalize without precision loss. The algorithm:
//
//  1. Reject leading `+` (JSON grammar forbids it; json.Decoder
//     grammar-validates).
//  2. Split into intPart / fracPart / expPart sub-strings.
//  3. Validate the digit-only sub-parts (defense in depth).
//  4. Combine intPart + fracPart into a single digit-mantissa, and
//     compute mantExp = (parsed exp) − len(fracPart). Each digit
//     moved past the decimal point reduces the effective exponent.
//  5. Strip leading zeros from the mantissa (no exponent shift).
//  6. If the stripped mantissa is empty, the value is zero. Emit
//     `0` with no sign and no exponent.
//  7. Strip trailing zeros from the mantissa, shifting mantExp UP by
//     the stripped count. This is what collapses `100`, `10e1`, `1e2`,
//     `0.1e3` onto a single key — and what closes the r7 P1
//     "exponent-shifted IDs map to different keys" finding.
//  8. Emit the canonical form: optional leading `-` for negative,
//     then mantissa, then `e<signed-exp>` if mantExp != 0. Exponents
//     are signed via `e-<mag>` (no leading `+`); `e0` is never
//     emitted (mantExp != 0 is the precondition).
//
// Two JSON numbers that denote the same mathematical value MUST map
// to the same canonical key. Examples that bucket together:
//
//     `1`, `1.0`, `1e0`, `10e-1`, `100e-2`, `0.1e1` all → `n:1`
//     `1.5`, `15e-1`, `150e-2`, `0.15e1` all → `n:15e-1`
//     `100`, `1e2`, `0.01e4`, `10e1` all → `n:1e2`
//     `0`, `-0`, `0e5`, `-0e5`, `0.0`, `-0.000e2` all → `n:0`
//     `9007199254740993` (2^53+1) → `n:9007199254740993`
//
// The canonical form is purely an internal Go map key. It is NOT
// surfaced in logs, responses, or external interfaces; only its
// uniqueness contract matters.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Per-hub session model" requestIDKey definition (codex r4 F6 +
// r5 P1 + r6 MED + r7 P1 closures). Plan: Task 3.1.

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// maxCanonicalExponentMagnitude caps the absolute exponent magnitude
// accepted by canonicalizeJSONNumber. JSON-RPC request IDs are
// expected to be small integers in practice (often 0..1000 monotonic);
// fractional or scientific forms are vanishingly rare. The cap
// (1<<20) is well below int32's range so strconv.Atoi can never
// overflow on platforms where int is 32-bit, and reasonable JSON
// numbers like `1e308` (max float64) still fit. A 7-digit exponent
// like `1e9999999` is rejected as malformed rather than parsed.
//
// The bound is applied to the PARSED exponent only — not to the post-
// shift mantExp emitted in the canonical form. After `mantExp -=
// len(fracPart)` and the trailing-zero strip shift, the final emitted
// exponent can exceed `maxCanonicalExponentMagnitude` by up to
// `len(mantissa)`. That is intentional: the parsed bound prevents int
// overflow inside the algorithm, while the post-shift value is
// bounded by the input mantissa length and therefore stays linear in
// the JSON number's byte length (no exponential blow-up of the output
// canonical string). codex r7 NIT closure on PR #157.
const maxCanonicalExponentMagnitude = 1 << 20

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
	// dec.Decode consumes one JSON value. We MUST verify there is
	// nothing after it. codex bot r11 P2 closure on PR #157: do not
	// rely on dec.More() — More returns true only when the next byte
	// is a plausible value-start. For inputs like `1]` or `1}` the
	// trailing byte is NOT a value-start, More returns false, and the
	// malformed id slips through canonicalized as `n:1`. Different
	// callers passing `1` vs `1]` would alias onto the same in-flight
	// slot. Use a second Decode and demand io.EOF — any other outcome
	// (a successful decode meaning a second value, OR a non-EOF
	// error like "invalid character ']'") means leftover input.
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", errors.New("invalid request: id contains a second JSON value")
		}
		return "", fmt.Errorf("invalid request: id contains trailing data: %w", err)
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

// canonicalizeJSONNumber returns a canonical mantissa+exponent form
// keyed so two numerically-equal JSON numbers map to the same string.
//
// PRE: s is a json.Number string accepted by json.Decoder.UseNumber.
//      The leading-plus check has ALREADY been applied by the caller.
//
// The implementation is pure string manipulation plus a single bounded
// integer arithmetic for the exponent — never any float math — so
// values whose mantissa exceeds 2^53 canonicalize without precision
// loss. See the package comment for the algorithm and bucketing
// examples (`1` / `10e-1` / `0.1e1` all → `n:1`).
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
	expSign, expMag, err := splitExponent(expPart)
	if err != nil {
		return "", err
	}

	// Step 4 — Parse the exponent into a signed integer; combine
	// intPart and fracPart into a single mantissa with mantExp
	// adjusted by -len(fracPart) so the value is preserved.
	mantExp := 0
	if expMag != "" {
		// Strip leading zeros from the exponent magnitude before
		// parsing so json grammar variants like `1e0010` and `1e10`
		// resolve to the same Go int (Atoi accepts leading zeros,
		// but explicit stripping bounds the input length).
		expMag = strings.TrimLeft(expMag, "0")
		if expMag != "" {
			if len(expMag) > 8 {
				// Bound the input length before Atoi — defense
				// against a hypothetically-huge exponent like
				// `1e99999999999999` that would overflow int.
				return "", fmt.Errorf("invalid number: exponent magnitude %q exceeds canonicalizer bound", expMag)
			}
			parsed, perr := strconv.Atoi(expMag)
			if perr != nil {
				return "", fmt.Errorf("invalid number: exponent magnitude %q: %w", expMag, perr)
			}
			if parsed > maxCanonicalExponentMagnitude {
				return "", fmt.Errorf("invalid number: exponent magnitude %d exceeds canonicalizer bound %d", parsed, maxCanonicalExponentMagnitude)
			}
			if expSign == '-' {
				parsed = -parsed
			}
			mantExp = parsed
		}
	}
	mantissa := intPart + fracPart
	mantExp -= len(fracPart)

	// Step 5 — Strip leading zeros from the mantissa. No exponent
	// shift — leading zeros do not change the value.
	mantissa = strings.TrimLeft(mantissa, "0")

	// Step 6 — All-zero mantissa means the number is zero. Drop the
	// sign (no negative zero) AND the exponent (0e5 = 0, period).
	if mantissa == "" {
		return "0", nil
	}

	// Step 7 — Strip trailing zeros from the mantissa, shifting
	// mantExp UP by the stripped count. THIS is the step that closes
	// the r7 P1 finding: `10e-1` → mantissa "10" → strip 1 trailing
	// zero → mantissa "1", mantExp = -1 + 1 = 0 → canonical `1`,
	// identical to bare `1`.
	stripped := 0
	for strings.HasSuffix(mantissa, "0") {
		mantissa = mantissa[:len(mantissa)-1]
		stripped++
	}
	mantExp += stripped

	// Step 8 — Emit canonical form. Exponent 0 is implicit (omitted).
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(mantissa)
	if mantExp != 0 {
		b.WriteByte('e')
		if mantExp < 0 {
			b.WriteByte('-')
			b.WriteString(strconv.Itoa(-mantExp))
		} else {
			b.WriteString(strconv.Itoa(mantExp))
		}
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

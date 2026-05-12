// hub_mcp_request_id_test.go — Phase 3 Task 3.1 (G4 unified hub MCP).
//
// Tests for losslessly-canonicalized JSON-RPC id keys. Each case maps
// directly to a numbered rule in spec §"requestIDKey" (steps 1-7 of
// the canonicalization algorithm), with extra coverage for the
// MCP-narrower null-rejection (codex r6 MED) and grammar edges the
// json stdlib accepts but the spec rejects (leading `+`).
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Per-hub session model" requestIDKey definition (~lines 101-145).
// Plan: Task 3.1.

package api

import (
	"encoding/json"
	"testing"
)

// String form — `"abc"` → `s:abc`. Escape sequences resolved via
// json.Unmarshal so the prefix is the unescaped Go string.
func TestNewRequestIDKeyStringForm(t *testing.T) {
	key, err := newRequestIDKey(json.RawMessage(`"abc"`))
	if err != nil {
		t.Fatal(err)
	}
	if key != "s:abc" {
		t.Errorf("got %q want s:abc", key)
	}
}

// String with escape sequences — `"a\"b"` → `s:a"b`. Confirms
// json.Unmarshal pass-through (spec rule: string canonicalization
// passes through json.Unmarshal).
func TestNewRequestIDKeyStringEscapes(t *testing.T) {
	cases := map[string]requestIDKey{
		`"a\"b"`:    `s:a"b`,
		`"line\n"`:  "s:line\n",
		`"é"`:  "s:é",
		`""`:        "s:",
	}
	for in, want := range cases {
		key, err := newRequestIDKey(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if key != want {
			t.Errorf("%s -> %q, want %q", in, key, want)
		}
	}
}

// Integer canonical form: every equivalent encoding of `1` collapses
// to `n:1` (spec rule 5).
func TestNewRequestIDKeyIntegerCanonical(t *testing.T) {
	// Note: `01`, `0001` are NOT valid JSON numbers (grammar forbids
	// leading zeros on integer part); json.Decoder rejects them, so we
	// don't include them here. The "strip leading zeros" canonicalization
	// rule applies to grammar-valid inputs (e.g. those reaching us after
	// the decoder accepts them).
	cases := []string{`1`, `1.0`, `1.00`, `1e0`, `1E+0`, `1E-0`}
	for _, in := range cases {
		key, err := newRequestIDKey(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if key != "n:1" {
			t.Errorf("%s -> %q, want n:1", in, key)
		}
	}
}

// Zero canonical form: `0`, `0.0`, `0e0`, `-0`, `-0.0` collapse to
// `n:0` (spec rule 2 + rule 5 zero-handling).
func TestNewRequestIDKeyZeroCanonical(t *testing.T) {
	cases := []string{`0`, `0.0`, `0e0`, `0E+0`, `-0`, `-0.0`, `-0e0`}
	for _, in := range cases {
		key, err := newRequestIDKey(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if key != "n:0" {
			t.Errorf("%s -> %q, want n:0", in, key)
		}
	}
}

// Cross-form bucketing — every JSON-RPC number that denotes the
// same mathematical value MUST collapse onto the same canonical key.
// codex bot r7 P1 closure on PR #157.
//
// The earlier per-form-only normalization (strip leading zeros on
// intMag, trailing zeros on fracMag, leading zeros on expMag) was
// insufficient because it preserved each input's structural shape:
// `1` and `10e-1` and `0.1e1` all denote 1, but mapped to three
// distinct keys (`n:1`, `n:10e-1`, `n:0.1e1`). A client switching
// encodings between requests would bypass duplicate-in-flight
// detection AND cancellation lookup. The mantissa+exponent reduction
// is what closes that gap.
func TestNewRequestIDKeyCrossFormBucketing(t *testing.T) {
	groups := map[requestIDKey][]string{
		// Integer 1 in every plausible JSON-RPC encoding.
		"n:1": {
			`1`, `1.0`, `1.00`, `1e0`, `1E+0`, `10e-1`, `100e-2`,
			`0.1e1`, `0.01e2`, `0.001e3`, `1000e-3`,
		},
		// Negative 1.
		"n:-1": {`-1`, `-1.0`, `-1e0`, `-10e-1`, `-0.1e1`},
		// Fractional 1.5.
		"n:15e-1": {
			`1.5`, `1.50`, `1.500`, `15e-1`, `150e-2`, `0.15e1`,
			`0.015e2`, `1.5e0`,
		},
		// Negative fractional.
		"n:-15e-1": {`-1.5`, `-15e-1`, `-0.15e1`, `-1.50`},
		// Integer 100.
		"n:1e2": {`100`, `1e2`, `10e1`, `0.1e3`, `1000e-1`, `100.0`, `100e0`},
		// Tiny fractional.
		"n:5e-2": {`0.05`, `5e-2`, `0.005e1`, `50e-3`},
	}
	for want, members := range groups {
		for _, in := range members {
			got, err := newRequestIDKey(json.RawMessage(in))
			if err != nil {
				t.Errorf("%s: %v", in, err)
				continue
			}
			if got != want {
				t.Errorf("%s -> %q, want %q (cross-form bucketing)", in, got, want)
			}
		}
	}
}

// Reject implausibly-large exponent magnitudes (>1<<20 in absolute
// value). The canonicalizer is for JSON-RPC request IDs which are
// expected to be small integers; an exponent like `1e9999999999`
// is more likely a malformed input than legitimate. Defense against
// `strconv.Atoi` overflow on 32-bit platforms — the bound (1<<20)
// is well below int32's range. codex bot r7 P1 closure on PR #157.
func TestNewRequestIDKeyRejectsExtremeExponent(t *testing.T) {
	cases := []string{
		`1e99999999`,           // 8-digit exponent body
		`1e9999999999`,         // longer
		`1e1048577`,            // 1<<20 + 1
		`1e-99999999`,
	}
	for _, in := range cases {
		if _, err := newRequestIDKey(json.RawMessage(in)); err == nil {
			t.Errorf("must reject extreme exponent %s", in)
		}
	}
}

// Boundary at the parsed exponent cap (1<<20). codex r7 NIT closure
// on PR #157. The bound check rejects values STRICTLY greater than
// the cap, so the cap itself (1048576) must be accepted. Document
// the boundary behavior so a future tightening of the limit doesn't
// silently regress this contract.
func TestNewRequestIDKeyExponentBoundary(t *testing.T) {
	cases := map[string]requestIDKey{
		`1e1048576`:  "n:1e1048576",  // exactly at the bound — accepted
		`1e-1048576`: "n:1e-1048576", // negative bound — accepted
	}
	for in, want := range cases {
		key, err := newRequestIDKey(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: unexpected error: %v", in, err)
			continue
		}
		if key != want {
			t.Errorf("%s -> %q, want %q (boundary value must be accepted)", in, key, want)
		}
	}
}

// Negative fractional × negative exponent combinations exercise the
// `mantExp = parsedExp - len(fracPart)` shift in the underspecified
// quadrant. codex r7 NIT closure on PR #157. Each pair below denotes
// the same mathematical value via different fracPart/expPart splits.
func TestNewRequestIDKeyNegativeFractionalExponentShift(t *testing.T) {
	groups := map[requestIDKey][]string{
		// -1.5e-3 = -0.0015 = -15e-4 = -150e-5 = -1500e-6
		"n:-15e-4": {`-1.5e-3`, `-15e-4`, `-150e-5`, `-1500e-6`, `-0.0015`},
		// -0.001 = -1e-3 = -10e-4 = -100e-5
		"n:-1e-3": {`-0.001`, `-1e-3`, `-10e-4`, `-100e-5`, `-1.0e-3`},
		// 1.05e-2 = 0.0105 = 105e-4
		"n:105e-4": {`1.05e-2`, `0.0105`, `105e-4`, `1.050e-2`},
		// Post-shift exponent exceeds parsed bound (intentional —
		// see maxCanonicalExponentMagnitude doc comment): the parsed
		// `1048575` plus the trailing-zero strip of "10" yields a
		// final exponent of 1048576. Bound is on PARSED only.
		"n:1e1048576": {`10e1048575`, `100e1048574`, `1e1048576`},
	}
	for want, members := range groups {
		for _, in := range members {
			got, err := newRequestIDKey(json.RawMessage(in))
			if err != nil {
				t.Errorf("%s: %v", in, err)
				continue
			}
			if got != want {
				t.Errorf("%s -> %q, want %q", in, got, want)
			}
		}
	}
}

// Zero with non-zero exponent magnitude — `0e5`, `-0e5`, `0.0e3`,
// `-0.000e-5`, `0e10` all denote mathematical zero and MUST collapse
// onto `n:0`. Otherwise a client sending `id: -0e5` and another sending
// `id: 0` would route to distinct in-flight slots even though the
// JSON-RPC values are equal. codex bot r6 P2 closure on PR #157.
//
// Note: `0e0` already canonicalized to `n:0` before the fix because
// Step 6 trimmed the exponent magnitude to empty. The fix matters for
// any non-empty exponent magnitude (e.g. `0e5`, `-0e1`).
func TestNewRequestIDKeyZeroDropsNonZeroExponent(t *testing.T) {
	cases := []string{
		`0e5`, `0E5`, `0E+5`, `0e-5`,
		`-0e5`, `-0e-5`, `-0E10`,
		`0.0e3`, `0.00e-7`, `-0.000e2`,
		`0e10`, `-0e1`,
	}
	for _, in := range cases {
		key, err := newRequestIDKey(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if key != "n:0" {
			t.Errorf("%s -> %q, want n:0", in, key)
		}
	}
}

// Fractional canonicalizes to `<mantissa>e<signed-exp>` form so all
// cross-form encodings of the same value bucket together. codex bot
// r7 P1 closure on PR #157: the prior decimal-preserving form let
// `1.5` and `15e-1` (both denote 1.5) map to distinct keys.
func TestNewRequestIDKeyFractionalPreserves(t *testing.T) {
	cases := map[string]requestIDKey{
		`1.5`:   "n:15e-1",
		`1.50`:  "n:15e-1",
		`1.5e0`: "n:15e-1",
		`1.500`: "n:15e-1",
		`1.05`:  "n:105e-2",
	}
	for in, want := range cases {
		key, err := newRequestIDKey(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if key != want {
			t.Errorf("%s -> %q, want %q", in, key, want)
		}
	}
}

// Negative numbers preserve sign through mantissa+exponent
// canonicalization (codex bot r7 P1 closure on PR #157).
func TestNewRequestIDKeyNegative(t *testing.T) {
	cases := map[string]requestIDKey{
		`-1`:    "n:-1",
		`-1.0`:  "n:-1",
		`-1.5`:  "n:-15e-1",
		`-1e1`:  "n:-1e1",
	}
	for in, want := range cases {
		key, err := newRequestIDKey(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if key != want {
			t.Errorf("%s -> %q, want %q", in, key, want)
		}
	}
}

// Spec rule 7 — big integers stay distinct. 9007199254740993 (= 2^53+1)
// MUST NOT collapse onto 9007199254740992; float64 would conflate them
// and that's exactly the bug this canonicalizer prevents.
func TestNewRequestIDKeyBigIntegerStaysDistinct(t *testing.T) {
	k1, err := newRequestIDKey(json.RawMessage(`9007199254740992`))
	if err != nil {
		t.Fatalf("9007199254740992: %v", err)
	}
	k2, err := newRequestIDKey(json.RawMessage(`9007199254740993`))
	if err != nil {
		t.Fatalf("9007199254740993: %v", err)
	}
	if k1 == k2 {
		t.Errorf("big integers collapsed: %q == %q", k1, k2)
	}
	if k1 != "n:9007199254740992" {
		t.Errorf("k1=%q want n:9007199254740992", k1)
	}
	if k2 != "n:9007199254740993" {
		t.Errorf("k2=%q want n:9007199254740993", k2)
	}
}

// Even larger integers — beyond uint64 — must still canonicalize
// without precision loss.
func TestNewRequestIDKeyHugeIntegerPreserved(t *testing.T) {
	huge := "999999999999999999999999999999"
	key, err := newRequestIDKey(json.RawMessage(huge))
	if err != nil {
		t.Fatalf("%s: %v", huge, err)
	}
	want := requestIDKey("n:" + huge)
	if key != want {
		t.Errorf("got %q want %q", key, want)
	}
}

// Exponents normalize: `1e10` stays compact, `1E+10` → `n:1e10`,
// leading zeros stripped from the exponent (`1e0010` → `n:1e10`).
// Mantissas with fractional digits collapse onto the canonical
// mantissa+exp form via trailing-zero strip (`1.5e2` → `n:15e1`,
// equivalent to `150`). codex bot r7 P1 closure on PR #157.
func TestNewRequestIDKeyExponentNormalize(t *testing.T) {
	cases := map[string]requestIDKey{
		`1e10`:    "n:1e10",
		`1E10`:    "n:1e10",
		`1E+10`:   "n:1e10",
		`1e0010`:  "n:1e10",
		`1e-5`:    "n:1e-5",
		`1E-05`:   "n:1e-5",
		`1.5e2`:   "n:15e1",
		`1.50e2`:  "n:15e1",
		`1.500E2`: "n:15e1",
	}
	for in, want := range cases {
		key, err := newRequestIDKey(json.RawMessage(in))
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if key != want {
			t.Errorf("%s -> %q, want %q", in, key, want)
		}
	}
}

// Reject null — MCP narrower than JSON-RPC base (codex r6 MED closure).
func TestNewRequestIDKeyRejectsNull(t *testing.T) {
	if _, err := newRequestIDKey(json.RawMessage(`null`)); err == nil {
		t.Errorf("must reject null")
	}
}

// Reject array / object / boolean — MCP inherits JSON-RPC grammar.
func TestNewRequestIDKeyRejectsArrayObjectBoolean(t *testing.T) {
	for _, in := range []string{`[]`, `{}`, `[1,2]`, `{"a":1}`, `true`, `false`} {
		if _, err := newRequestIDKey(json.RawMessage(in)); err == nil {
			t.Errorf("must reject %s", in)
		}
	}
}

// Reject leading `+` (spec rule 1 — JSON grammar forbids it; the
// stdlib decoder accepts/rejects vary, the canonicalizer must reject
// explicitly to be defensive).
func TestNewRequestIDKeyRejectsLeadingPlus(t *testing.T) {
	if _, err := newRequestIDKey(json.RawMessage(`+1`)); err == nil {
		t.Errorf("must reject leading +")
	}
}

// Reject empty / whitespace-only — neither is a valid JSON-RPC id.
func TestNewRequestIDKeyRejectsEmpty(t *testing.T) {
	for _, in := range []string{``, ` `, "\t", "\n"} {
		if _, err := newRequestIDKey(json.RawMessage(in)); err == nil {
			t.Errorf("must reject empty/whitespace %q", in)
		}
	}
}

// Reject inputs with trailing tokens after a valid JSON value.
// codex bot r11 P2 closure on PR #157: the prior `dec.More()` check
// returned false for `1]` because `]` is not a JSON value-start, so
// malformed inputs aliased onto valid ids (`1]` and `1` both → n:1)
// and broke duplicate-in-flight detection. The fix issues a second
// dec.Decode that MUST return io.EOF; anything else (success or
// non-EOF error) is leftover input.
//
// `dec.More() == true` cases (`1 2`, `42  43`) are also covered for
// completeness; they were the cases the prior implementation DID
// catch.
func TestNewRequestIDKeyRejectsTrailingTokens(t *testing.T) {
	cases := []string{
		`1]`,
		`1}`,
		`1,`,
		`1[`,
		`1{`,
		`42)`,
		`1 2`,    // dec.More() catches this
		`42  43`, // dec.More() with whitespace
		`"abc"]`, // string path (json.Unmarshal rejects natively)
		`1.5x`,   // garbage after number
	}
	for _, in := range cases {
		if _, err := newRequestIDKey(json.RawMessage(in)); err == nil {
			t.Errorf("must reject trailing-data input %s", in)
		}
	}
}

// Reject malformed numbers — `1.`, `.5`, `1e`, `1e+`, hex, etc.
// json grammar refuses these; canonicalizer surfaces a parse error.
func TestNewRequestIDKeyRejectsMalformedNumber(t *testing.T) {
	for _, in := range []string{`1.`, `.5`, `1e`, `1e+`, `0x10`, `--1`, `1.5.6`, `1ee5`} {
		if _, err := newRequestIDKey(json.RawMessage(in)); err == nil {
			t.Errorf("must reject malformed number %s", in)
		}
	}
}

// String-vs-number discriminator — `"1"` and `1` MUST NOT collapse.
// Strings get `s:`, numbers get `n:`.
func TestNewRequestIDKeyStringNumberDistinct(t *testing.T) {
	str, err := newRequestIDKey(json.RawMessage(`"1"`))
	if err != nil {
		t.Fatal(err)
	}
	num, err := newRequestIDKey(json.RawMessage(`1`))
	if err != nil {
		t.Fatal(err)
	}
	if str == num {
		t.Errorf("string %q collapsed onto number %q", str, num)
	}
	if str != "s:1" {
		t.Errorf("str=%q want s:1", str)
	}
	if num != "n:1" {
		t.Errorf("num=%q want n:1", num)
	}
}

// Trailing whitespace inside a numeric input is tolerated by bytes.TrimSpace.
// (We trim leading + trailing whitespace before discriminating.)
func TestNewRequestIDKeyTrimsWhitespace(t *testing.T) {
	key, err := newRequestIDKey(json.RawMessage(`  42  `))
	if err != nil {
		t.Fatal(err)
	}
	if key != "n:42" {
		t.Errorf("got %q want n:42", key)
	}
}

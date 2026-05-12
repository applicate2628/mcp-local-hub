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

// Fractional preserves significant digits, strips trailing zeros
// (spec rule 4 + rule 6).
func TestNewRequestIDKeyFractionalPreserves(t *testing.T) {
	cases := map[string]requestIDKey{
		`1.5`:   "n:1.5",
		`1.50`:  "n:1.5",
		`1.5e0`: "n:1.5",
		`1.500`: "n:1.5",
		`1.05`:  "n:1.05",
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

// Negative numbers preserve sign (spec rule 5).
func TestNewRequestIDKeyNegative(t *testing.T) {
	cases := map[string]requestIDKey{
		`-1`:    "n:-1",
		`-1.0`:  "n:-1",
		`-1.5`:  "n:-1.5",
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
func TestNewRequestIDKeyExponentNormalize(t *testing.T) {
	cases := map[string]requestIDKey{
		`1e10`:    "n:1e10",
		`1E10`:    "n:1e10",
		`1E+10`:   "n:1e10",
		`1e0010`:  "n:1e10",
		`1e-5`:    "n:1e-5",
		`1E-05`:   "n:1e-5",
		`1.5e2`:   "n:1.5e2",
		`1.50e2`:  "n:1.5e2",
		`1.500E2`: "n:1.5e2",
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

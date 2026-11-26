package cmakelex

import (
	"strings"
	"testing"
)

func TestBracketBoundaries(t *testing.T) {
	for _, tc := range []struct {
		src        string
		at, equals int
		want       string
		closed     bool
	}{
		{"[[]]", 0, 0, "", true},
		{"#[=[left]]right]=] suffix", 1, 1, "left]]right", true},
		{"[==[first]=]second]==] tail", 0, 2, "first]=]second", true},
		{"[=[missing]]", 0, 1, "", false},
	} {
		eq, start, ok := MatchBracketOpen(tc.src, tc.at)
		if !ok || eq != tc.equals {
			t.Fatalf("open %q: equal count %d, matched %v", tc.src, eq, ok)
		}
		end, after, closed := FindBracketClose(tc.src, start, eq)
		if closed != tc.closed {
			t.Fatalf("close %q: matched %v, want %v", tc.src, closed, tc.closed)
		}
		if closed && (tc.src[start:end] != tc.want || tc.src[end:after] != "]"+strings.Repeat("=", eq)+"]") {
			t.Fatalf("wrong close span %q %d:%d:%d", tc.src, start, end, after)
		}
	}
	if _, _, ok := MatchBracketOpen("[==", 0); ok {
		t.Fatal("accepted unterminated opening bracket")
	}
	if _, _, ok := MatchBracketOpen("plain", 5); ok {
		t.Fatal("accepted end of input")
	}
}
func TestSkipQuoted(t *testing.T) {
	for _, s := range []string{"\"left\\\"right\" tail", "\"left\\\nright\" tail"} {
		if got, want := SkipQuoted(s, 0), strings.Index(s, " tail"); got != want {
			t.Fatalf("skip %q: %d, want %d", s, got, want)
		}
	}
	if s := "\"unterminated\\"; SkipQuoted(s, 0) != len(s) {
		t.Fatal("unclosed quote does not consume through end")
	}
}

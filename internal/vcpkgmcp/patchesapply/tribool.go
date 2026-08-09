package patchesapply

import (
	"strconv"
	"strings"
)

// Tri is a three-valued (Kleene) logic value used to evaluate portfile.cmake
// if() guards against a PARTIAL variable environment. Unlike a plain bool,
// Unknown propagates honestly through AND/OR/NOT instead of forcing a guess
// — this is the mechanism behind the "undecidable" bucket in Result: a guard
// referencing a variable this package cannot derive from the triplet name or
// an explicit override becomes Unknown, and Unknown only collapses to
// True/False when Kleene logic proves it does not matter (e.g. `unknown AND
// false == false`).
type Tri int

const (
	TriFalse Tri = iota
	TriTrue
	TriUnknown
)

func (t Tri) String() string {
	switch t {
	case TriTrue:
		return "true"
	case TriFalse:
		return "false"
	default:
		return "unknown"
	}
}

// kleeneAnd implements three-valued AND: a definite false on either side
// forces false even if the other side is unknown (short-circuit is sound
// here because both sides are pure boolean expressions with no side effects).
func kleeneAnd(a, b Tri) Tri {
	if a == TriFalse || b == TriFalse {
		return TriFalse
	}
	if a == TriUnknown || b == TriUnknown {
		return TriUnknown
	}
	return TriTrue
}

// kleeneOr implements three-valued OR: a definite true on either side forces
// true even if the other side is unknown.
func kleeneOr(a, b Tri) Tri {
	if a == TriTrue || b == TriTrue {
		return TriTrue
	}
	if a == TriUnknown || b == TriUnknown {
		return TriUnknown
	}
	return TriFalse
}

func kleeneNot(a Tri) Tri {
	switch a {
	case TriTrue:
		return TriFalse
	case TriFalse:
		return TriTrue
	default:
		return TriUnknown
	}
}

// cmakeFalseConstants are the exact-match (case-insensitive) string values
// CMake's if() command treats as boolean false; every other non-empty value
// (except a NOTFOUND-suffixed one) is true. This is standard CMake if()
// semantics (see CMake's own "Basic Expressions" if() documentation), not a
// vcpkg-specific rule.
var cmakeFalseConstants = map[string]bool{
	"":         true,
	"0":        true,
	"OFF":      true,
	"FALSE":    true,
	"N":        true,
	"NO":       true,
	"IGNORE":   true,
	"NOTFOUND": true,
}

// truthy converts a resolved scalar value to a Tri per CMake if() constant
// rules. val == nil means "value could not be resolved" -> Unknown.
func truthy(val *string) Tri {
	if val == nil {
		return TriUnknown
	}
	v := strings.ToUpper(strings.TrimSpace(*val))
	if cmakeFalseConstants[v] {
		return TriFalse
	}
	if number, err := strconv.ParseFloat(v, 64); err == nil && number == 0 {
		return TriFalse
	}
	if strings.HasSuffix(v, "-NOTFOUND") {
		return TriFalse
	}
	return TriTrue
}

// compareVersions mirrors CMake's cmSystemTools::VersionCompare: compare
// integer components without overflow, ignore leading zeros, treat omitted
// components as zero, and stop when neither side begins another integer
// component. Thus a non-integer component or trailing part truncates the
// version at that point; it is never compared lexically as a prerelease.
// Returns -1, 0, or 1.
func compareVersions(a, b string) int {
	ai, bi := 0, 0
	for isVersionDigit(a, ai) || isVersionDigit(b, bi) {
		for byteAt(a, ai) == '0' {
			ai++
		}
		for byteAt(b, bi) == '0' {
			bi++
		}

		ab, bb := ai, bi
		for isVersionDigit(a, ai) {
			ai++
		}
		for isVersionDigit(b, bi) {
			bi++
		}

		ad, bd := a[ab:ai], b[bb:bi]
		if len(ad) != len(bd) {
			if len(ad) < len(bd) {
				return -1
			}
			return 1
		}
		if ad != bd {
			if ad < bd {
				return -1
			}
			return 1
		}

		if byteAt(b, bi) == '.' {
			bi++
		}
		if byteAt(a, ai) == '.' {
			ai++
		}
	}
	return 0
}

func byteAt(s string, i int) byte {
	if i >= len(s) {
		return 0
	}
	return s[i]
}

func isVersionDigit(s string, i int) bool {
	b := byteAt(s, i)
	return b >= '0' && b <= '9'
}

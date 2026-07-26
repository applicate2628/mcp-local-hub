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
	if strings.HasSuffix(v, "-NOTFOUND") {
		return TriFalse
	}
	return TriTrue
}

// compareVersions compares two dot-separated version strings component-wise,
// numerically where possible (falling back to string comparison for a
// non-numeric component), the same behaviour CMake's VERSION_* if()
// operators document. Returns -1, 0, or 1.
func compareVersions(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv string
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		ai, aerr := strconv.Atoi(av)
		bi, berr := strconv.Atoi(bv)
		if aerr == nil && berr == nil {
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
			continue
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

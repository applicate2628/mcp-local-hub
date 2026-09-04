package archguard

import "go/build/constraint"

type constraintTruth uint8

const (
	constraintUnknown constraintTruth = iota
	constraintFalse
	constraintTrue
)

const maxConstraintSATSteps = 1_000_000

// constraintSatisfiable solves the remaining custom-tag portion of a build
// constraint after platform, compiler, release, and toolchain tags are fixed.
// It uses tri-state evaluation and branches only on a tag that still affects
// the result, avoiding eager 2^N enumeration.
func constraintSatisfiable(expr constraint.Expr, fixed func(string) (bool, bool)) bool {
	if expr == nil {
		return true
	}
	assignments := make(map[string]bool)
	steps := maxConstraintSATSteps
	var solve func() bool
	solve = func() bool {
		if steps <= 0 {
			// Complexity fails conservatively: never claim two variants are
			// disjoint when the exact proof budget is exhausted.
			return true
		}
		state := evaluateConstraintPartial(expr, fixed, assignments, &steps)
		switch state {
		case constraintTrue:
			return true
		case constraintFalse:
			return false
		}
		tag, ok := firstUnknownConstraintTag(expr, fixed, assignments)
		if !ok {
			return false
		}
		assignments[tag] = false
		if solve() {
			delete(assignments, tag)
			return true
		}
		assignments[tag] = true
		result := solve()
		delete(assignments, tag)
		return result
	}
	return solve()
}

func evaluateConstraintPartial(expr constraint.Expr, fixed func(string) (bool, bool), assignments map[string]bool, steps *int) constraintTruth {
	if expr == nil {
		return constraintTrue
	}
	*steps = *steps - 1
	if *steps < 0 {
		return constraintUnknown
	}
	switch value := expr.(type) {
	case *constraint.TagExpr:
		if enabled, known := fixed(value.Tag); known {
			if enabled {
				return constraintTrue
			}
			return constraintFalse
		}
		if enabled, assigned := assignments[canonicalCustomBuildTag(value.Tag)]; assigned {
			if enabled {
				return constraintTrue
			}
			return constraintFalse
		}
		return constraintUnknown
	case *constraint.NotExpr:
		switch evaluateConstraintPartial(value.X, fixed, assignments, steps) {
		case constraintTrue:
			return constraintFalse
		case constraintFalse:
			return constraintTrue
		default:
			return constraintUnknown
		}
	case *constraint.AndExpr:
		left := evaluateConstraintPartial(value.X, fixed, assignments, steps)
		if left == constraintFalse {
			return constraintFalse
		}
		right := evaluateConstraintPartial(value.Y, fixed, assignments, steps)
		if right == constraintFalse {
			return constraintFalse
		}
		if left == constraintTrue && right == constraintTrue {
			return constraintTrue
		}
		return constraintUnknown
	case *constraint.OrExpr:
		left := evaluateConstraintPartial(value.X, fixed, assignments, steps)
		if left == constraintTrue {
			return constraintTrue
		}
		right := evaluateConstraintPartial(value.Y, fixed, assignments, steps)
		if right == constraintTrue {
			return constraintTrue
		}
		if left == constraintFalse && right == constraintFalse {
			return constraintFalse
		}
		return constraintUnknown
	default:
		return constraintUnknown
	}
}

func firstUnknownConstraintTag(expr constraint.Expr, fixed func(string) (bool, bool), assignments map[string]bool) (string, bool) {
	if expr == nil {
		return "", false
	}
	switch value := expr.(type) {
	case *constraint.TagExpr:
		if _, known := fixed(value.Tag); known {
			return "", false
		}
		tag := canonicalCustomBuildTag(value.Tag)
		if _, assigned := assignments[tag]; assigned {
			return "", false
		}
		return tag, true
	case *constraint.NotExpr:
		return firstUnknownConstraintTag(value.X, fixed, assignments)
	case *constraint.AndExpr:
		if tag, ok := firstUnknownConstraintTag(value.X, fixed, assignments); ok {
			return tag, true
		}
		return firstUnknownConstraintTag(value.Y, fixed, assignments)
	case *constraint.OrExpr:
		if tag, ok := firstUnknownConstraintTag(value.X, fixed, assignments); ok {
			return tag, true
		}
		return firstUnknownConstraintTag(value.Y, fixed, assignments)
	default:
		return "", false
	}
}

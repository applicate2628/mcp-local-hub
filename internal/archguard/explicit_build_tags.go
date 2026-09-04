package archguard

import "go/build/constraint"

const explicitAdditionalTagPrefix = "\x00archcheck:explicit-tag:"

// allowExplicitAutomaticTagOverrides models an automatic tag used in a
// build constraint as true either because of the active build context or
// because the same spelling was supplied through -tags. Both directives and
// filename suffixes use this helper, matching go/build.Context.matchTag.
// Sharing the additional-tag variable preserves negation across both sources.
func allowExplicitAutomaticTagOverrides(expr constraint.Expr) constraint.Expr {
	switch value := expr.(type) {
	case *constraint.TagExpr:
		if !isAutomaticBuildTag(value.Tag) {
			return &constraint.TagExpr{Tag: value.Tag}
		}
		return &constraint.OrExpr{
			X: &constraint.TagExpr{Tag: value.Tag},
			Y: &constraint.TagExpr{Tag: explicitAdditionalTagPrefix + value.Tag},
		}
	case *constraint.NotExpr:
		return &constraint.NotExpr{X: allowExplicitAutomaticTagOverrides(value.X)}
	case *constraint.AndExpr:
		return &constraint.AndExpr{
			X: allowExplicitAutomaticTagOverrides(value.X),
			Y: allowExplicitAutomaticTagOverrides(value.Y),
		}
	case *constraint.OrExpr:
		return &constraint.OrExpr{
			X: allowExplicitAutomaticTagOverrides(value.X),
			Y: allowExplicitAutomaticTagOverrides(value.Y),
		}
	default:
		return expr
	}
}

func isAutomaticBuildTag(tag string) bool {
	switch {
	case isKnownGOOS(tag), isKnownGOARCH(tag), tag == "unix":
		return true
	case tag == "gc" || tag == "gccgo" || tag == "cgo":
		return true
	}
	if _, ok := parseGoReleaseTag(tag); ok {
		return true
	}
	_, ok := parseArchitectureFeatureTag(tag)
	return ok
}

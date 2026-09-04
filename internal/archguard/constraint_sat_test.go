package archguard

import (
	"go/build/constraint"
	"testing"
)

func TestBuildCompatibilitySolvesMoreThanTenCustomTags(t *testing.T) {
	var all constraint.Expr
	for i := 0; i < 20; i++ {
		tag := &constraint.TagExpr{Tag: string(rune('a' + i))}
		all = andBuildConstraints(all, tag)
	}
	contradiction := &constraint.NotExpr{X: &constraint.TagExpr{Tag: "a"}}
	if buildConstraintsOverlap(all, contradiction) {
		t.Fatal("twenty-tag contradiction was treated as overlapping")
	}
}

func TestConstraintSATPrunesLargeDisjunction(t *testing.T) {
	var alternatives constraint.Expr
	for i := 0; i < 30; i++ {
		tag := &constraint.TagExpr{Tag: string(rune('A' + i))}
		if alternatives == nil {
			alternatives = tag
		} else {
			alternatives = &constraint.OrExpr{X: alternatives, Y: tag}
		}
	}
	if !buildConstraintsOverlap(alternatives, &constraint.TagExpr{Tag: "Z"}) {
		t.Fatal("large satisfiable custom-tag formula was rejected")
	}
}

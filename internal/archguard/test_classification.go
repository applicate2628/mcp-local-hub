package archguard

import (
	"go/ast"
	"go/build/constraint"
	"go/token"
)

func isTestOnlyEffectiveBuildFile(filePath string, file *ast.File, fset *token.FileSet, source []byte, testTags []string) bool {
	if len(testTags) == 0 {
		return false
	}

	expr := buildConstraintForFile(fileContext{
		Path:   filePath,
		File:   file,
		FSet:   fset,
		Source: source,
	})
	if expr == nil {
		return false
	}

	allTags := map[string]struct{}{}
	collectConstraintTags(expr, allTags)
	referencesTestTag := false
	for _, tag := range testTags {
		if _, ok := allTags[tag]; !ok {
			continue
		}
		referencesTestTag = true
		expr = &constraint.AndExpr{
			X: expr,
			Y: &constraint.NotExpr{X: &constraint.TagExpr{Tag: tag}},
		}
	}
	if !referencesTestTag {
		return false
	}

	// The file is test-only exactly when its effective constraint cannot match
	// any valid target/compiler/custom-tag assignment after every test tag that
	// the file actually references is forced off. Unused policy tags are not
	// injected into the expression, so a large registry cannot trip the custom-
	// tag complexity guard for a simple single-tag file.
	return !buildConstraintsOverlap(expr, nil)
}

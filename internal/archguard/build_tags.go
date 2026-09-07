package archguard

import (
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"strings"
)

type buildDirective struct {
	line int
	text string
}

func isTestOnlyBuildSource(source []byte, testTags []string) bool {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "source.go", source, parser.ParseComments)
	if err != nil {
		return false
	}
	return isTestOnlyBuildFile(file, fset, source, testTags)
}

func isTestOnlyBuildFile(file *ast.File, fset *token.FileSet, source []byte, testTags []string) bool {
	return isTestOnlyEffectiveBuildFile("source.go", file, fset, source, testTags)
}

func leadingBuildConstraint(file *ast.File, fset *token.FileSet, source []byte) (constraint.Expr, bool) {
	if file == nil || fset == nil || file.Package == token.NoPos {
		return nil, false
	}
	var goBuild []buildDirective
	var plusBuild []buildDirective
	firstLine := 0
	lastLine := 0
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			continue
		}
		for _, comment := range group.List {
			text := strings.TrimSuffix(comment.Text, "\r")
			if !strings.HasPrefix(text, "//") {
				continue
			}
			isGoBuild := constraint.IsGoBuild(text)
			isPlusBuild := constraint.IsPlusBuild(text)
			if !isGoBuild && !isPlusBuild {
				continue
			}
			line := fset.Position(comment.Pos()).Line
			if firstLine == 0 || line < firstLine {
				firstLine = line
			}
			if line > lastLine {
				lastLine = line
			}
			directive := buildDirective{line: line, text: text}
			if isGoBuild {
				goBuild = append(goBuild, directive)
			} else {
				plusBuild = append(plusBuild, directive)
			}
		}
	}
	if firstLine == 0 || !validBuildDirectivePlacement(file, fset, source, firstLine, lastLine, fset.Position(file.Package).Line, len(plusBuild) > 0) {
		return nil, false
	}
	if len(goBuild) > 0 {
		if len(goBuild) != 1 {
			return nil, false
		}
		expr, err := constraint.Parse(goBuild[0].text)
		if err != nil {
			return nil, false
		}
		return allowExplicitAutomaticTagOverrides(expr), true
	}
	var legacy constraint.Expr
	for _, directive := range plusBuild {
		expr, err := constraint.Parse(directive.text)
		if err != nil {
			return nil, false
		}
		if legacy == nil {
			legacy = expr
		} else {
			legacy = &constraint.AndExpr{X: legacy, Y: expr}
		}
	}
	if legacy == nil {
		return nil, false
	}
	return allowExplicitAutomaticTagOverrides(legacy), true
}

func validBuildDirectivePlacement(file *ast.File, fset *token.FileSet, source []byte, firstLine, lastLine, packageLine int, requireBlankLine bool) bool {
	if file == nil || fset == nil || firstLine <= 0 || lastLine < firstLine || packageLine <= lastLine {
		return false
	}
	lines := strings.Split(strings.ReplaceAll(string(source), "\r\n", "\n"), "\n")
	lineAt := func(number int) string {
		if number <= 0 || number > len(lines) {
			return ""
		}
		return strings.TrimSpace(strings.TrimSuffix(lines[number-1], "\r"))
	}
	commentLines := make(map[int]struct{})
	for _, group := range file.Comments {
		if group.Pos() >= file.Package {
			continue
		}
		for _, comment := range group.List {
			start := fset.Position(comment.Pos()).Line
			end := fset.Position(comment.End()).Line
			for line := start; line <= end; line++ {
				commentLines[line] = struct{}{}
			}
		}
	}
	for line := 1; line < firstLine; line++ {
		if lineAt(line) == "" {
			continue
		}
		if _, covered := commentLines[line]; covered {
			continue
		}
		return false
	}
	if !requireBlankLine {
		return true
	}
	for line := lastLine + 1; line < packageLine; line++ {
		if lineAt(line) == "" {
			return true
		}
	}
	return false
}

func collectConstraintTags(expr constraint.Expr, out map[string]struct{}) {
	switch value := expr.(type) {
	case *constraint.TagExpr:
		out[value.Tag] = struct{}{}
	case *constraint.NotExpr:
		collectConstraintTags(value.X, out)
	case *constraint.AndExpr:
		collectConstraintTags(value.X, out)
		collectConstraintTags(value.Y, out)
	case *constraint.OrExpr:
		collectConstraintTags(value.X, out)
		collectConstraintTags(value.Y, out)
	}
}

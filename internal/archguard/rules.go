package archguard

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func ruleAPIConstructors(ctx fileContext, policy Policy) []Violation {
	return ruleSymbolCalls(ctx, policy, policy.APIConstructors, KindAPIConstruction)
}

func ruleProductionConstructors(ctx fileContext, policy Policy) []Violation {
	return ruleSymbolCalls(ctx, policy, policy.ProductionConstructors, KindProductionConstructor)
}

func ruleSymbolCalls(ctx fileContext, policy Policy, rules []SymbolRule, kind ViolationKind) []Violation {
	if ctx.IsTest || ctx.Generated {
		return nil
	}
	var out []Violation
	blockedImports := make(map[string]struct{})
	for _, rule := range rules {
		if matchesAnyGlob(rule.AllowedGlobs, ctx.Path, false) {
			continue
		}
		line, unresolved := ctx.UnaliasedImports[rule.ImportPath]
		if !unresolved {
			continue
		}
		blockedImports[rule.ImportPath] = struct{}{}
		evidence := rule.ImportPath + "." + rule.Symbol
		out = append(out, Violation{
			Kind:     kind,
			Location: Location{Path: ctx.Path, Symbol: ctx.File.Name.Name, Line: line},
			Evidence: evidence,
			Message:  fmt.Sprintf("constructor package %s must use an explicit alias because its declared package name could not be resolved from the scanned module", rule.ImportPath),
		})
	}
	ast.Inspect(ctx.File, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		for _, rule := range rules {
			if matchesAnyGlob(rule.AllowedGlobs, ctx.Path, false) {
				continue
			}
			if _, blocked := blockedImports[rule.ImportPath]; blocked {
				continue
			}
			if !callMatchesSymbolRule(ctx, call, rule) {
				continue
			}
			evidence := rule.ImportPath + "." + rule.Symbol
			out = append(out, Violation{
				Kind:     kind,
				Location: Location{Path: ctx.Path, Symbol: enclosingSymbol(ctx, call.Pos()), Line: lineOf(ctx, call.Pos())},
				Evidence: evidence,
				Message:  fmt.Sprintf("constructor %s is called outside its allowed composition paths", evidence),
			})
		}
		return true
	})
	return out
}

func callMatchesSymbolRule(ctx fileContext, call *ast.CallExpr, rule SymbolRule) bool {
	switch target := unwrapCallTarget(call.Fun).(type) {
	case *ast.SelectorExpr:
		alias, ok := target.X.(*ast.Ident)
		if !ok || alias.Obj != nil || target.Sel.Name != rule.Symbol {
			return false
		}
		return ctx.Imports[alias.Name] == rule.ImportPath
	case *ast.Ident:
		if target.Obj != nil || target.Name != rule.Symbol {
			return false
		}
		_, ok := ctx.DotImports[rule.ImportPath]
		return ok
	default:
		return false
	}
}

func unwrapCallTarget(expr ast.Expr) ast.Expr {
	for {
		switch target := expr.(type) {
		case *ast.ParenExpr:
			expr = target.X
		case *ast.IndexExpr:
			expr = target.X
		case *ast.IndexListExpr:
			expr = target.X
		default:
			return expr
		}
	}
}

func ruleProductionTestHooks(ctx fileContext, policy Policy) []Violation {
	if ctx.IsTest || ctx.Generated {
		return nil
	}
	var out []Violation
	appendHook := func(declaredName, symbol string, pos token.Pos) {
		if declaredName == "" || !regexMatchesAny(policy.compiledTestHooks, declaredName) {
			return
		}
		out = append(out, Violation{
			Kind:     KindProductionTestHook,
			Location: Location{Path: ctx.Path, Symbol: symbol, Line: lineOf(ctx, pos)},
			Evidence: declaredName,
			Message:  fmt.Sprintf("test hook %s is compiled into production code", symbol),
		})
	}
	for _, decl := range ctx.File.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			appendHook(d.Name.Name, functionSymbol(ctx, d), d.Name.Pos())
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.ValueSpec:
					for _, name := range s.Names {
						appendHook(name.Name, name.Name, name.Pos())
					}
				case *ast.TypeSpec:
					appendHook(s.Name.Name, s.Name.Name, s.Name.Pos())
				}
			}
		}
	}
	return out
}

func ruleHistoryComments(ctx fileContext, policy Policy) []Violation {
	if ctx.Generated || matchesAnyGlob(policy.HistoryAllowedGlobs, ctx.Path, false) {
		return nil
	}
	var out []Violation
	for _, group := range ctx.File.Comments {
		text := group.Text()
		for _, pattern := range policy.compiledHistory {
			match := pattern.FindString(text)
			if match == "" {
				continue
			}
			out = append(out, Violation{
				Kind:     KindHistoryComment,
				Location: Location{Path: ctx.Path, Symbol: enclosingSymbol(ctx, group.Pos()), Line: lineOf(ctx, group.Pos())},
				Evidence: match,
				Message:  fmt.Sprintf("production comment contains review history marker %q; move history to an ADR or archive", match),
			})
		}
	}
	return out
}

func ruleEmbeddedDocuments(ctx fileContext, policy Policy) []Violation {
	if ctx.Generated {
		return nil
	}
	var out []Violation
	ast.Inspect(ctx.File, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, expr := range spec.Values {
			value, ok := stringConstant(expr)
			if !ok || len(value) < policy.EmbeddedDocumentMinBytes || markdownMarkers(value) < 2 {
				continue
			}
			symbol := enclosingSymbol(ctx, spec.Pos())
			if i < len(spec.Names) {
				symbol = spec.Names[i].Name
			} else if len(spec.Names) > 0 {
				symbol = spec.Names[0].Name
			}
			out = append(out, Violation{
				Kind:     KindEmbeddedDocument,
				Location: Location{Path: ctx.Path, Symbol: symbol, Line: lineOf(ctx, expr.Pos())},
				Evidence: "markdown string constant",
				Metric:   len(value),
				Message:  fmt.Sprintf("embedded Markdown document is %d bytes; move it to an embedded .md file", len(value)),
			})
		}
		return true
	})
	return out
}

func stringConstant(expr ast.Expr) (string, bool) {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return "", false
		}
		decoded, err := strconv.Unquote(value.Value)
		return decoded, err == nil
	case *ast.BinaryExpr:
		if value.Op != token.ADD {
			return "", false
		}
		left, ok := stringConstant(value.X)
		if !ok {
			return "", false
		}
		right, ok := stringConstant(value.Y)
		if !ok {
			return "", false
		}
		return left + right, true
	case *ast.ParenExpr:
		return stringConstant(value.X)
	default:
		return "", false
	}
}

func ruleFileBudgets(ctx fileContext, policy Policy) []Violation {
	if ctx.Generated {
		return nil
	}
	budgetClass := ""
	if ctx.IsTest {
		if ctx.LineCount > policy.FileBudgets.TestReviewLines {
			budgetClass = "test_review_lines"
		}
	} else {
		switch {
		case ctx.LineCount > policy.FileBudgets.ProductionHardLines:
			budgetClass = "production_hard_lines"
		case ctx.LineCount > policy.FileBudgets.ProductionAdvisoryLines:
			budgetClass = "production_advisory_lines"
		}
	}
	if budgetClass == "" {
		return nil
	}
	return []Violation{{
		Kind:     KindFileBudget,
		Location: Location{Path: ctx.Path, Symbol: ctx.File.Name.Name, Line: 1},
		Evidence: budgetClass,
		Metric:   ctx.LineCount,
		Message:  fmt.Sprintf("file has %d lines and exceeds %s", ctx.LineCount, budgetClass),
	}}
}

func ruleGenericPackages(ctx fileContext, policy Policy) []Violation {
	name := ctx.File.Name.Name
	base := path.Base(ctx.PackageDir)
	names := append([]string(nil), policy.GenericPackageNames...)
	sort.Strings(names)
	for _, generic := range names {
		if name != generic && name != generic+"_test" && base != generic {
			continue
		}
		locationPath := ctx.PackageDir
		if locationPath == "" {
			locationPath = "."
		}
		return []Violation{{
			Kind:     KindGenericPackage,
			Location: Location{Path: locationPath, Symbol: generic, Line: 1},
			Evidence: generic,
			Message:  fmt.Sprintf("generic package name %q requires an explicit architectural owner or waiver", generic),
		}}
	}
	return nil
}

func ruleMutableGlobals(ctx fileContext, policy Policy) []Violation {
	if ctx.IsTest || ctx.Generated {
		return nil
	}
	var out []Violation
	for _, decl := range ctx.File.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				if name.Name == "_" || regexMatchesAny(policy.compiledAllowedGlobals, name.Name) {
					continue
				}
				out = append(out, Violation{
					Kind:     KindMutableGlobal,
					Location: Location{Path: ctx.Path, Symbol: name.Name, Line: lineOf(ctx, name.Pos())},
					Evidence: "var " + name.Name,
					Message:  fmt.Sprintf("production package-level var %s requires an explicit policy allowance or instance-owned dependency", name.Name),
				})
			}
		}
	}
	return out
}

func lineOf(ctx fileContext, pos token.Pos) int {
	if pos == token.NoPos {
		return 0
	}
	return ctx.FSet.Position(pos).Line
}

func nodeText(ctx fileContext, node any) string {
	var b bytes.Buffer
	if err := format.Node(&b, ctx.FSet, node); err != nil {
		return ""
	}
	return normalizeEvidence(b.String())
}

func regexMatchesAny(patterns []*regexp.Regexp, value string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

func functionSymbol(ctx fileContext, fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	receiver := nodeText(ctx, fn.Recv.List[0].Type)
	if receiver == "" {
		return fn.Name.Name
	}
	return receiver + "." + fn.Name.Name
}

func enclosingFunction(ctx fileContext, pos token.Pos) string {
	for _, decl := range ctx.File.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Pos() <= pos && pos <= fn.End() {
			return functionSymbol(ctx, fn)
		}
	}
	return ""
}

func enclosingSymbol(ctx fileContext, pos token.Pos) string {
	for _, decl := range ctx.File.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Doc != nil && d.Doc.Pos() <= pos && pos <= d.Doc.End() {
				return functionSymbol(ctx, d)
			}
			if d.Pos() <= pos && pos <= d.End() {
				return functionSymbol(ctx, d)
			}
		case *ast.GenDecl:
			if d.Doc != nil && d.Doc.Pos() <= pos && pos <= d.Doc.End() {
				return firstGenDeclName(d)
			}
			if d.Pos() <= pos && pos <= d.End() {
				for _, spec := range d.Specs {
					if spec.Pos() <= pos && pos <= spec.End() {
						if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
							return vs.Names[0].Name
						}
						if ts, ok := spec.(*ast.TypeSpec); ok {
							return ts.Name.Name
						}
					}
				}
				return firstGenDeclName(d)
			}
		}
	}
	return ctx.File.Name.Name
}

func firstGenDeclName(decl *ast.GenDecl) string {
	for _, spec := range decl.Specs {
		switch s := spec.(type) {
		case *ast.ValueSpec:
			if len(s.Names) > 0 {
				return s.Names[0].Name
			}
		case *ast.TypeSpec:
			return s.Name.Name
		}
	}
	return ""
}

func sortedImportPaths(paths []string) []string {
	out := append([]string(nil), paths...)
	sort.Strings(out)
	return out
}

func repositoryImportPath(module, importPath string) string {
	module = strings.TrimSuffix(strings.TrimSpace(module), "/")
	if importPath == module {
		return "."
	}
	if module != "" && strings.HasPrefix(importPath, module+"/") {
		return strings.TrimPrefix(importPath, module+"/")
	}
	return importPath
}

func markdownMarkers(s string) int {
	markers := 0
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "# ") || strings.Contains(s, "\n# ") || strings.Contains(s, "\n## ") {
		markers++
	}
	if strings.Contains(s, "```") {
		markers++
	}
	if strings.Contains(s, "\n|") && strings.Contains(s, "|\n") {
		markers++
	}
	return markers
}

func ruleImports(ctx fileContext, policy Policy) []Violation {
	var out []Violation
	packageDir := ctx.PackageDir
	if packageDir == "" {
		packageDir = "."
	}
	for _, rule := range policy.ImportRules {
		if !matchesAnyGlob(rule.From, packageDir, false) {
			continue
		}
		for _, importPath := range sortedImportPaths(ctx.ImportPaths) {
			repositoryPath := repositoryImportPath(policy.Module, importPath)
			if !matchesAnyGlob(rule.Deny, repositoryPath, false) && !matchesAnyGlob(rule.Deny, importPath, false) {
				continue
			}
			out = append(out, Violation{
				Kind:     KindImport,
				Location: Location{Path: ctx.Path, Symbol: ctx.File.Name.Name, Line: 1},
				Evidence: importPath,
				Message:  fmt.Sprintf("package %s imports denied package %s", packageDir, importPath),
			})
		}
	}
	return out
}

func callableEvidence(ctx fileContext, expr ast.Expr) string {
	switch expr.(type) {
	case *ast.FuncLit:
		return "func literal"
	default:
		callable := nodeText(ctx, expr)
		if callable == "" {
			return "<unknown>"
		}
		return callable
	}
}

func workerScopeSymbol(ctx fileContext, pos token.Pos) string {
	if symbol := enclosingFunction(ctx, pos); symbol != "" {
		return symbol
	}
	return enclosingSymbol(ctx, pos)
}

type workerOccurrenceKey struct {
	scope    string
	callable string
}

func ruleWorkers(ctx fileContext, policy Policy) []Violation {
	if ctx.IsTest || ctx.Generated {
		return nil
	}
	var out []Violation
	occurrences := make(map[workerOccurrenceKey]int)
	ast.Inspect(ctx.File, func(node ast.Node) bool {
		stmt, ok := node.(*ast.GoStmt)
		if !ok {
			return true
		}
		callable := callableEvidence(ctx, stmt.Call.Fun)
		scope := workerScopeSymbol(ctx, stmt.Pos())
		key := workerOccurrenceKey{scope: scope, callable: callable}
		occurrences[key]++
		evidence := fmt.Sprintf("go %s #%d", callable, occurrences[key])
		out = append(out, Violation{
			Kind:     KindWorker,
			Location: Location{Path: ctx.Path, Symbol: scope, Line: lineOf(ctx, stmt.Pos())},
			Evidence: evidence,
			Message:  fmt.Sprintf("goroutine %s requires an owner, cancellation, join, and bound contract", callable),
		})
		return true
	})
	return out
}

package archguard

import (
	"fmt"
	"go/ast"
	"go/token"
)

func ruleAPIConstructorReferences(ctx fileContext, policy Policy) []Violation {
	return ruleConstructorReferences(ctx, policy, policy.APIConstructors, KindAPIConstruction)
}

func ruleProductionConstructorReferences(ctx fileContext, policy Policy) []Violation {
	return ruleConstructorReferences(ctx, policy, policy.ProductionConstructors, KindProductionConstructor)
}

func ruleConstructorReferences(ctx fileContext, policy Policy, rules []SymbolRule, kind ViolationKind) []Violation {
	if ctx.IsTest || ctx.Generated {
		return nil
	}

	parents := make(map[ast.Node]ast.Node)
	selectorMembers := make(map[token.Pos]struct{})
	var stack []ast.Node
	ast.Inspect(ctx.File, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			parents[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		if selector, ok := node.(*ast.SelectorExpr); ok {
			selectorMembers[selector.Sel.Pos()] = struct{}{}
		}
		return true
	})

	appendReference := func(expr ast.Expr, rule SymbolRule, detail string) Violation {
		evidence := rule.ImportPath + "." + rule.Symbol
		return Violation{
			Kind:     kind,
			Location: Location{Path: ctx.Path, Symbol: enclosingSymbol(ctx, expr.Pos()), Line: lineOf(ctx, expr.Pos())},
			Evidence: evidence,
			Message:  fmt.Sprintf("constructor %s is referenced %s outside its allowed composition paths", evidence, detail),
		}
	}

	var out []Violation
	ast.Inspect(ctx.File, func(node ast.Node) bool {
		expr, ok := node.(ast.Expr)
		if !ok || isImmediateCallTarget(expr, parents) {
			return true
		}
		if ident, ok := expr.(*ast.Ident); ok && identifierIsDeclaration(ident, parents) {
			return true
		}

		matched := false
		for _, rule := range rules {
			if matchesAnyGlob(rule.AllowedGlobs, ctx.Path, false) ||
				!referenceMatchesSymbolRule(ctx, policy.Module, expr, selectorMembers, rule) {
				continue
			}
			out = append(out, appendReference(expr, rule, ""))
			matched = true
		}
		if matched {
			return true
		}

		selector, ok := expr.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if rule := unresolvedConstructorRuleForSelector(ctx, selector, rules); rule != nil {
			out = append(out, appendReference(expr, *rule, "through an unresolved unaliased import"))
		}
		return true
	})
	return out
}

func referenceMatchesSymbolRule(ctx fileContext, module string, expr ast.Expr, selectorMembers map[token.Pos]struct{}, rule SymbolRule) bool {
	switch target := expr.(type) {
	case *ast.SelectorExpr:
		alias, ok := target.X.(*ast.Ident)
		if !ok || alias.Obj != nil || target.Sel.Name != rule.Symbol {
			return false
		}
		return ctx.Imports[alias.Name] == rule.ImportPath
	case *ast.Ident:
		if _, isSelectorMember := selectorMembers[target.Pos()]; isSelectorMember || target.Name != rule.Symbol {
			return false
		}
		if ruleMatchesCurrentPackage(ctx, module, rule) && identifierMayReferToCurrentPackageSymbol(ctx, target) {
			return true
		}
		if target.Obj != nil {
			return false
		}
		_, ok := ctx.DotImports[rule.ImportPath]
		return ok
	default:
		return false
	}
}

func isImmediateCallTarget(expr ast.Expr, parents map[ast.Node]ast.Node) bool {
	current := ast.Node(expr)
	for {
		parent := parents[current]
		switch wrapped := parent.(type) {
		case *ast.ParenExpr:
			if wrapped.X != current {
				return false
			}
			current = parent
		case *ast.IndexExpr:
			if wrapped.X != current {
				return false
			}
			current = parent
		case *ast.IndexListExpr:
			if wrapped.X != current {
				return false
			}
			current = parent
		case *ast.CallExpr:
			return wrapped.Fun == current
		default:
			return false
		}
	}
}

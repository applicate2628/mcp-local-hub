package archguard

import (
	"fmt"
	"go/ast"
)

func ruleAPICurrentPackageConstructorCalls(ctx fileContext, policy Policy) []Violation {
	return ruleCurrentPackageConstructorCalls(ctx, policy, policy.APIConstructors, KindAPIConstruction)
}

func ruleProductionCurrentPackageConstructorCalls(ctx fileContext, policy Policy) []Violation {
	return ruleCurrentPackageConstructorCalls(ctx, policy, policy.ProductionConstructors, KindProductionConstructor)
}

func ruleCurrentPackageConstructorCalls(ctx fileContext, policy Policy, rules []SymbolRule, kind ViolationKind) []Violation {
	if ctx.IsTest || ctx.Generated {
		return nil
	}
	var out []Violation
	ast.Inspect(ctx.File, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := unwrapCallTarget(call.Fun).(*ast.Ident)
		if !ok {
			return true
		}
		for _, rule := range rules {
			if matchesAnyGlob(rule.AllowedGlobs, ctx.Path, false) ||
				!ruleMatchesCurrentPackage(ctx, policy.Module, rule) ||
				ident.Name != rule.Symbol ||
				!identifierMayReferToCurrentPackageSymbol(ctx, ident) {
				continue
			}
			evidence := rule.ImportPath + "." + rule.Symbol
			out = append(out, Violation{
				Kind:     kind,
				Location: Location{Path: ctx.Path, Symbol: enclosingSymbol(ctx, call.Pos()), Line: lineOf(ctx, call.Pos())},
				Evidence: evidence,
				Message:  fmt.Sprintf("constructor %s is called inside its defining package outside the allowed composition paths", evidence),
			})
		}
		return true
	})
	return out
}

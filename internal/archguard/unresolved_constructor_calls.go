package archguard

import (
	"fmt"
	"go/ast"
)

func ruleAPIUnresolvedConstructorCalls(ctx fileContext, policy Policy) []Violation {
	return ruleUnresolvedConstructorCalls(ctx, policy.APIConstructors, KindAPIConstruction)
}

func ruleProductionUnresolvedConstructorCalls(ctx fileContext, policy Policy) []Violation {
	return ruleUnresolvedConstructorCalls(ctx, policy.ProductionConstructors, KindProductionConstructor)
}

func ruleUnresolvedConstructorCalls(ctx fileContext, rules []SymbolRule, kind ViolationKind) []Violation {
	if ctx.IsTest || ctx.Generated {
		return nil
	}
	var out []Violation
	ast.Inspect(ctx.File, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := unwrapCallTarget(call.Fun).(*ast.SelectorExpr)
		if !ok {
			return true
		}
		matched := unresolvedConstructorRuleForSelector(ctx, selector, rules)
		if matched == nil {
			return true
		}
		evidence := matched.ImportPath + "." + matched.Symbol
		out = append(out, Violation{
			Kind:     kind,
			Location: Location{Path: ctx.Path, Symbol: enclosingSymbol(ctx, call.Pos()), Line: lineOf(ctx, call.Pos())},
			Evidence: evidence,
			Message:  fmt.Sprintf("constructor %s is called through an unresolved unaliased import outside its allowed composition paths", evidence),
		})
		return true
	})
	return out
}

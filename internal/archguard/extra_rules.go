package archguard

import (
	"fmt"
	"go/ast"
	"go/token"
)

func ruleProductionTypeFieldHooks(ctx fileContext, policy Policy) []Violation {
	if ctx.IsTest || ctx.Generated {
		return nil
	}
	var out []Violation
	for _, decl := range ctx.File.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, rawSpec := range gen.Specs {
			spec, ok := rawSpec.(*ast.TypeSpec)
			if !ok {
				continue
			}
			inspectProductionHookType(ctx, policy, spec.Type, spec.Name.Name, &out)
		}
	}
	return out
}

func inspectProductionHookType(ctx fileContext, policy Policy, expr ast.Expr, prefix string, out *[]Violation) {
	if expr == nil || out == nil {
		return
	}
	switch value := expr.(type) {
	case *ast.StructType:
		inspectProductionHookFields(ctx, policy, value.Fields, prefix, out)
	case *ast.InterfaceType:
		inspectProductionHookFields(ctx, policy, value.Methods, prefix, out)
	case *ast.StarExpr:
		inspectProductionHookType(ctx, policy, value.X, prefix, out)
	case *ast.ParenExpr:
		inspectProductionHookType(ctx, policy, value.X, prefix, out)
	case *ast.ArrayType:
		inspectProductionHookType(ctx, policy, value.Elt, prefix, out)
	case *ast.MapType:
		inspectProductionHookType(ctx, policy, value.Key, prefix, out)
		inspectProductionHookType(ctx, policy, value.Value, prefix, out)
	case *ast.ChanType:
		inspectProductionHookType(ctx, policy, value.Value, prefix, out)
	case *ast.Ellipsis:
		inspectProductionHookType(ctx, policy, value.Elt, prefix, out)
	case *ast.IndexExpr:
		inspectProductionHookType(ctx, policy, value.X, prefix, out)
		inspectProductionHookType(ctx, policy, value.Index, prefix, out)
	case *ast.IndexListExpr:
		inspectProductionHookType(ctx, policy, value.X, prefix, out)
		for _, index := range value.Indices {
			inspectProductionHookType(ctx, policy, index, prefix, out)
		}
	case *ast.FuncType:
		// Parameter and result identifiers are not fields of the containing
		// production type, but anonymous structural types inside their type
		// expressions are still compiled into production and must be checked.
		inspectProductionHookSignatureTypes(ctx, policy, value.Params, prefix, out)
		inspectProductionHookSignatureTypes(ctx, policy, value.Results, prefix, out)
	}
}

func inspectProductionHookSignatureTypes(ctx fileContext, policy Policy, fields *ast.FieldList, prefix string, out *[]Violation) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		if field == nil {
			continue
		}
		// Deliberately ignore field.Names: they are parameter/result identifiers,
		// not exported members of the containing production type.
		inspectProductionHookType(ctx, policy, field.Type, prefix, out)
	}
}

func inspectProductionHookFields(ctx fileContext, policy Policy, fields *ast.FieldList, prefix string, out *[]Violation) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		names := field.Names
		if len(names) == 0 {
			if name, pos := embeddedFieldName(field.Type); name != "" {
				names = []*ast.Ident{{Name: name, NamePos: pos}}
			}
		}
		if len(names) == 0 {
			inspectProductionHookType(ctx, policy, field.Type, prefix, out)
			continue
		}
		for _, name := range names {
			if name == nil || name.Name == "" {
				continue
			}
			symbol := prefix + "." + name.Name
			if regexMatchesAny(policy.compiledTestHooks, name.Name) {
				*out = append(*out, Violation{
					Kind:     KindProductionTestHook,
					Location: Location{Path: ctx.Path, Symbol: symbol, Line: lineOf(ctx, name.Pos())},
					Evidence: name.Name,
					Message:  fmt.Sprintf("test hook %s is compiled into production code", symbol),
				})
			}
			inspectProductionHookType(ctx, policy, field.Type, symbol, out)
		}
	}
}

func embeddedFieldName(expr ast.Expr) (string, token.Pos) {
	for {
		switch value := expr.(type) {
		case *ast.Ident:
			return value.Name, value.Pos()
		case *ast.SelectorExpr:
			return value.Sel.Name, value.Sel.Pos()
		case *ast.StarExpr:
			expr = value.X
		case *ast.ParenExpr:
			expr = value.X
		case *ast.IndexExpr:
			expr = value.X
		case *ast.IndexListExpr:
			expr = value.X
		default:
			return "", token.NoPos
		}
	}
}

func ruleHistoryEmptyMatches(ctx fileContext, policy Policy) []Violation {
	if ctx.Generated || matchesAnyGlob(policy.HistoryAllowedGlobs, ctx.Path, false) {
		return nil
	}
	var out []Violation
	for _, group := range ctx.File.Comments {
		text := group.Text()
		for _, pattern := range policy.compiledHistory {
			matches := pattern.FindAllStringIndex(text, -1)
			for _, match := range matches {
				if match[0] != match[1] {
					continue
				}
				evidence := "regex:" + pattern.String()
				out = append(out, Violation{
					Kind:     KindHistoryComment,
					Location: Location{Path: ctx.Path, Symbol: enclosingSymbol(ctx, group.Pos()), Line: lineOf(ctx, group.Pos())},
					Evidence: evidence,
					Message:  fmt.Sprintf("production comment matches zero-length review-history pattern %q; move history to an ADR or archive", pattern.String()),
				})
			}
		}
	}
	return out
}

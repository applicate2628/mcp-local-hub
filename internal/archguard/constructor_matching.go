package archguard

import "go/ast"

func currentPackageImportPath(ctx fileContext, module string) string {
	module = stringsTrimTrailingSlash(module)
	if module == "" {
		return ""
	}
	if ctx.PackageDir == "" {
		return module
	}
	return module + "/" + ctx.PackageDir
}

func stringsTrimTrailingSlash(value string) string {
	for len(value) > 0 && value[len(value)-1] == '/' {
		value = value[:len(value)-1]
	}
	return value
}

func identifierMayReferToCurrentPackageSymbol(ctx fileContext, ident *ast.Ident) bool {
	if ident == nil {
		return false
	}
	if ident.Obj == nil {
		// The parser resolves only file scope. An unresolved identifier can
		// therefore legally name a package declaration from a sibling file.
		return true
	}
	return isPackageLevelObject(ctx.File, ident.Obj)
}

func isPackageLevelObject(file *ast.File, object *ast.Object) bool {
	if file == nil || object == nil {
		return false
	}
	for _, decl := range file.Decls {
		switch value := decl.(type) {
		case *ast.FuncDecl:
			if value.Name != nil && value.Name.Obj == object {
				return true
			}
		case *ast.GenDecl:
			for _, rawSpec := range value.Specs {
				switch spec := rawSpec.(type) {
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						if name.Obj == object {
							return true
						}
					}
				case *ast.TypeSpec:
					if spec.Name != nil && spec.Name.Obj == object {
						return true
					}
				}
			}
		}
	}
	return false
}

func identifierIsDeclaration(ident *ast.Ident, parents map[ast.Node]ast.Node) bool {
	if ident == nil {
		return false
	}
	switch parent := parents[ident].(type) {
	case *ast.FuncDecl:
		return parent.Name == ident
	case *ast.TypeSpec:
		return parent.Name == ident
	case *ast.ValueSpec:
		for _, name := range parent.Names {
			if name == ident {
				return true
			}
		}
	case *ast.Field:
		for _, name := range parent.Names {
			if name == ident {
				return true
			}
		}
	}
	return false
}

func ruleMatchesCurrentPackage(ctx fileContext, module string, rule SymbolRule) bool {
	return currentPackageImportPath(ctx, module) == rule.ImportPath
}

func unresolvedConstructorRuleForSelector(ctx fileContext, selector *ast.SelectorExpr, rules []SymbolRule) *SymbolRule {
	if selector == nil || selector.Sel == nil {
		return nil
	}
	alias, ok := selector.X.(*ast.Ident)
	if !ok || alias.Obj != nil {
		return nil
	}
	if importPath, bound := ctx.Imports[alias.Name]; bound {
		for i := range rules {
			rule := &rules[i]
			if rule.ImportPath != importPath || selector.Sel.Name != rule.Symbol || matchesAnyGlob(rule.AllowedGlobs, ctx.Path, false) {
				continue
			}
			if _, unresolved := ctx.UnaliasedImports[rule.ImportPath]; unresolved {
				return rule
			}
		}
		return nil
	}

	// When the declared package name differs from the final import-path
	// element, the parser has no binding for the selector's package name.
	// Attribute it only if exactly one unresolved constructor identity with
	// the requested symbol is possible in this file.
	var matched *SymbolRule
	for i := range rules {
		rule := &rules[i]
		if selector.Sel.Name != rule.Symbol || matchesAnyGlob(rule.AllowedGlobs, ctx.Path, false) {
			continue
		}
		if _, unresolved := ctx.UnaliasedImports[rule.ImportPath]; !unresolved {
			continue
		}
		if matched != nil {
			return nil
		}
		matched = rule
	}
	return matched
}

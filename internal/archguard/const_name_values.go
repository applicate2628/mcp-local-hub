package archguard

import (
	"fmt"
	"go/ast"
)

// evaluateDeclaredStringConstants records resolved values against each declared
// constant identifier. Unlike the expression side table, this includes const
// specifications that implicitly repeat the preceding expression.
func evaluateDeclaredStringConstants(files []fileContext, module string) error {
	groups := make(map[string][]int)
	for i := range files {
		if files[i].File == nil {
			continue
		}
		key := namespaceForFile(module, files[i]).localKey
		groups[key] = append(groups[key], i)
	}

	evaluator := newStringConstantEvaluator(files, module)
	for _, key := range packageStringEvaluationKeys(groups) {
		for _, index := range groups[key] {
			file := &files[index]
			if file.File == nil {
				continue
			}
			if file.StringValues == nil {
				file.StringValues = make(map[ast.Expr][]string)
			}
			evaluator.currentNamespace = namespaceForFile(module, *file)
			referenceConstraint := buildConstraintForFile(*file)
			var evaluationErr error
			ast.Inspect(file.File, func(node ast.Node) bool {
				if evaluationErr != nil {
					return false
				}
				spec, ok := node.(*ast.ValueSpec)
				if !ok {
					return true
				}
				for _, name := range spec.Names {
					if name == nil || name.Name == "_" || name.Obj == nil {
						continue
					}
					definition, ok := evaluator.definitions[name.Obj]
					if !ok {
						continue
					}
					variants, err := evaluator.resolveDefinition(
						definition,
						referenceConstraint,
						file.IsTest,
						make(map[*stringConstantDefinition]bool),
					)
					if err != nil {
						evaluationErr = fmt.Errorf("%s: evaluate declared string constant %s: %w", file.Path, name.Name, err)
						return false
					}
					values := distinctVariantValues(variants)
					if len(values) > 0 {
						file.StringValues[name] = values
					}
				}
				return true
			})
			if evaluationErr != nil {
				return evaluationErr
			}
		}
	}
	return nil
}

package archguard

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/constant"
	"go/token"
	"sort"
	"unicode/utf8"
)

type integerVariant struct {
	value      constant.Value
	constraint constraint.Expr
}

func (e *stringConstantEvaluator) resolveStringConversionArgument(expr ast.Expr, context constraint.Expr, includeTests bool, visiting map[*stringConstantDefinition]bool) ([]stringVariant, error) {
	stringsResolved, err := e.resolve(expr, context, includeTests, visiting)
	if err != nil || len(stringsResolved) > 0 {
		return stringsResolved, err
	}
	integers, err := e.resolveInteger(expr, context, includeTests, visiting)
	if err != nil {
		return nil, err
	}
	converted := make([]stringVariant, 0, len(integers))
	for _, variant := range integers {
		converted = append(converted, stringVariant{
			value:      integerConstantString(variant.value),
			constraint: variant.constraint,
		})
	}
	return mergeStringVariants(converted)
}

func (e *stringConstantEvaluator) resolveInteger(expr ast.Expr, context constraint.Expr, includeTests bool, visiting map[*stringConstantDefinition]bool) ([]integerVariant, error) {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind != token.INT && value.Kind != token.CHAR {
			return nil, nil
		}
		integer := constant.MakeFromLiteral(value.Value, value.Kind, 0)
		if integer.Kind() != constant.Int {
			return nil, nil
		}
		return []integerVariant{{value: integer, constraint: context}}, nil
	case *ast.ParenExpr:
		return e.resolveInteger(value.X, context, includeTests, visiting)
	case *ast.UnaryExpr:
		values, err := e.resolveInteger(value.X, context, includeTests, visiting)
		if err != nil {
			return nil, err
		}
		if value.Op != token.ADD && value.Op != token.SUB && value.Op != token.XOR {
			return nil, nil
		}
		out := make([]integerVariant, 0, len(values))
		for _, variant := range values {
			result, ok := safeConstantUnary(value.Op, variant.value)
			if ok {
				out = append(out, integerVariant{value: result, constraint: variant.constraint})
			}
		}
		return mergeIntegerVariants(out)
	case *ast.BinaryExpr:
		left, err := e.resolveInteger(value.X, context, includeTests, visiting)
		if err != nil {
			return nil, err
		}
		right, err := e.resolveInteger(value.Y, context, includeTests, visiting)
		if err != nil {
			return nil, err
		}
		return combineIntegerVariants(left, right, value.Op)
	case *ast.Ident:
		if value.Obj != nil {
			if value.Obj.Kind != ast.Con {
				return nil, nil
			}
			definition, ok := e.definitions[value.Obj]
			if !ok {
				return nil, nil
			}
			return e.resolveIntegerDefinition(definition, context, includeTests, visiting)
		}
		if value.Name == "iota" && e.currentIota != nil {
			return []integerVariant{{value: constant.MakeInt64(*e.currentIota), constraint: context}}, nil
		}
		return e.resolveIntegerDefinitions(e.currentConstantDefinitions(value.Name, context, includeTests), context, includeTests, visiting)
	case *ast.SelectorExpr:
		return e.resolveIntegerDefinitions(e.importedConstantDefinitions(value), context, includeTests, visiting)
	case *ast.CallExpr:
		if len(value.Args) != 1 {
			return nil, nil
		}
		typeConstraint, ok := e.integerConversionConstraint(value.Fun, context, includeTests, make(map[*stringTypeDefinition]bool))
		if !ok {
			return nil, nil
		}
		return e.resolveInteger(value.Args[0], typeConstraint, includeTests, visiting)
	default:
		return nil, nil
	}
}

func (e *stringConstantEvaluator) resolveIntegerDefinitions(definitions []*stringConstantDefinition, context constraint.Expr, includeTests bool, visiting map[*stringConstantDefinition]bool) ([]integerVariant, error) {
	var variants []integerVariant
	for _, definition := range definitions {
		resolved, err := e.resolveIntegerDefinition(definition, context, includeTests, visiting)
		if err != nil {
			return nil, err
		}
		variants = append(variants, resolved...)
	}
	return mergeIntegerVariants(variants)
}

func (e *stringConstantEvaluator) integerConversionConstraint(expr ast.Expr, context constraint.Expr, includeTests bool, visiting map[*stringTypeDefinition]bool) (constraint.Expr, bool) {
	switch value := expr.(type) {
	case *ast.ParenExpr:
		return e.integerConversionConstraint(value.X, context, includeTests, visiting)
	case *ast.IndexExpr:
		return e.integerConversionConstraint(value.X, context, includeTests, visiting)
	case *ast.IndexListExpr:
		return e.integerConversionConstraint(value.X, context, includeTests, visiting)
	case *ast.Ident:
		if value.Obj == nil {
			if isPredeclaredIntegerTypeName(value.Name) {
				return context, true
			}
			return e.packageIntegerTypeConstraint(e.currentTypeDefinitions(value.Name, context, includeTests), context, includeTests, visiting)
		}
		if value.Obj.Kind != ast.Typ {
			return nil, false
		}
		definition, ok := e.typeDefinitions[value.Obj]
		if !ok {
			return nil, false
		}
		return e.resolveIntegerTypeDefinition(definition, context, includeTests, visiting)
	case *ast.SelectorExpr:
		return e.packageIntegerTypeConstraint(e.importedTypeDefinitionsForSelector(value), context, includeTests, visiting)
	default:
		return nil, false
	}
}

func (e *stringConstantEvaluator) packageIntegerTypeConstraint(definitions []*stringTypeDefinition, context constraint.Expr, includeTests bool, visiting map[*stringTypeDefinition]bool) (constraint.Expr, bool) {
	var combined constraint.Expr
	found := false
	for _, definition := range definitions {
		candidate, ok := e.resolveIntegerTypeDefinition(definition, context, includeTests, visiting)
		if !ok {
			continue
		}
		if !found {
			combined = candidate
			found = true
			continue
		}
		combined = orBuildConstraints(combined, candidate)
	}
	return combined, found
}

func (e *stringConstantEvaluator) resolveIntegerTypeDefinition(definition *stringTypeDefinition, context constraint.Expr, includeTests bool, visiting map[*stringTypeDefinition]bool) (constraint.Expr, bool) {
	if definition == nil || visiting[definition] || (definition.testOnly && !includeTests) {
		return nil, false
	}
	combined := andBuildConstraints(context, definition.constraint)
	if !buildConstraintsOverlap(combined, nil) {
		return nil, false
	}
	visiting[definition] = true
	previousNamespace := e.currentNamespace
	e.currentNamespace = definition.namespace
	resolved, ok := e.integerConversionConstraint(definition.expr, combined, includeTests, visiting)
	e.currentNamespace = previousNamespace
	delete(visiting, definition)
	return resolved, ok
}

func isPredeclaredIntegerTypeName(name string) bool {
	switch name {
	case "byte", "rune", "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "uintptr":
		return true
	default:
		return false
	}
}

func (e *stringConstantEvaluator) resolveIntegerDefinition(definition *stringConstantDefinition, context constraint.Expr, includeTests bool, visiting map[*stringConstantDefinition]bool) ([]integerVariant, error) {
	if definition == nil || visiting[definition] || (definition.testOnly && !includeTests) {
		return nil, nil
	}
	combined := andBuildConstraints(context, definition.constraint)
	if !buildConstraintsOverlap(combined, nil) {
		return nil, nil
	}
	visiting[definition] = true
	previousIota := e.currentIota
	previousNamespace := e.currentNamespace
	iotaValue := definition.iotaValue
	e.currentIota = &iotaValue
	e.currentNamespace = definition.namespace
	resolved, err := e.resolveInteger(definition.expr, combined, includeTests, visiting)
	e.currentNamespace = previousNamespace
	e.currentIota = previousIota
	delete(visiting, definition)
	return resolved, err
}

func combineIntegerVariants(left, right []integerVariant, op token.Token) ([]integerVariant, error) {
	if len(left) == 0 || len(right) == 0 {
		return nil, nil
	}
	combined := make([]integerVariant, 0, len(left)*len(right))
	for _, lhs := range left {
		for _, rhs := range right {
			buildConstraint := andBuildConstraints(lhs.constraint, rhs.constraint)
			if !buildConstraintsOverlap(buildConstraint, nil) {
				continue
			}
			result, ok := safeConstantBinary(op, lhs.value, rhs.value)
			if !ok {
				continue
			}
			combined = append(combined, integerVariant{value: result, constraint: buildConstraint})
			if len(combined) > maxResolvedStringVariants*4 {
				return nil, fmt.Errorf("integer constant expands to too many build variants")
			}
		}
	}
	return mergeIntegerVariants(combined)
}

func safeConstantUnary(op token.Token, value constant.Value) (result constant.Value, ok bool) {
	defer func() {
		if recover() != nil {
			result = nil
			ok = false
		}
	}()
	result = constant.UnaryOp(op, value, 0)
	return result, result != nil && result.Kind() == constant.Int
}

func safeConstantBinary(op token.Token, left, right constant.Value) (result constant.Value, ok bool) {
	defer func() {
		if recover() != nil {
			result = nil
			ok = false
		}
	}()
	switch op {
	case token.SHL, token.SHR:
		shift, valid := constant.Uint64Val(right)
		if !valid || shift > 1<<20 {
			return nil, false
		}
		result = constant.Shift(left, op, uint(shift))
	case token.ADD, token.SUB, token.MUL, token.QUO, token.REM, token.AND, token.OR, token.XOR, token.AND_NOT:
		result = constant.BinaryOp(left, op, right)
	default:
		return nil, false
	}
	return result, result != nil && result.Kind() == constant.Int
}

func mergeIntegerVariants(variants []integerVariant) ([]integerVariant, error) {
	if len(variants) == 0 {
		return nil, nil
	}
	byValue := make(map[string]integerVariant)
	for _, variant := range variants {
		if variant.value == nil || variant.value.Kind() != constant.Int {
			continue
		}
		key := variant.value.ExactString()
		if prior, ok := byValue[key]; ok {
			prior.constraint = orBuildConstraints(prior.constraint, variant.constraint)
			byValue[key] = prior
		} else {
			byValue[key] = variant
		}
		if len(byValue) > maxResolvedStringVariants {
			return nil, fmt.Errorf("integer constant resolves to more than %d distinct values", maxResolvedStringVariants)
		}
	}
	keys := make([]string, 0, len(byValue))
	for key := range byValue {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]integerVariant, 0, len(keys))
	for _, key := range keys {
		out = append(out, byValue[key])
	}
	return out, nil
}

func integerConstantString(value constant.Value) string {
	if value == nil || value.Kind() != constant.Int {
		return string(utf8.RuneError)
	}
	if signed, ok := constant.Int64Val(value); ok {
		return string(validConvertedRune(signed))
	}
	if unsigned, ok := constant.Uint64Val(value); ok {
		if unsigned > uint64(utf8.MaxRune) {
			return string(utf8.RuneError)
		}
		return string(validConvertedRune(int64(unsigned)))
	}
	return string(utf8.RuneError)
}

func validConvertedRune(value int64) rune {
	if value < 0 || value > int64(utf8.MaxRune) {
		return utf8.RuneError
	}
	r := rune(value)
	if !utf8.ValidRune(r) {
		return utf8.RuneError
	}
	return r
}

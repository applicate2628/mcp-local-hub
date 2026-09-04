package archguard

import (
	"fmt"
	"go/ast"
	"go/build/constraint"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

const maxResolvedStringVariants = 256

type resolutionNamespace struct {
	packagePath string
	localKey    string
	imports     map[string]string
	dotImports  map[string]struct{}
}

type stringConstantDefinition struct {
	object     *ast.Object
	expr       ast.Expr
	constraint constraint.Expr
	testOnly   bool
	iotaValue  int64
	namespace  resolutionNamespace
}

type stringTypeDefinition struct {
	object     *ast.Object
	expr       ast.Expr
	constraint constraint.Expr
	testOnly   bool
	namespace  resolutionNamespace
}

type stringVariant struct {
	value      string
	constraint constraint.Expr
}

type stringConstantEvaluator struct {
	definitions             map[*ast.Object]*stringConstantDefinition
	localDefinitions        map[string]map[string][]*stringConstantDefinition
	importedDefinitions     map[string]map[string][]*stringConstantDefinition
	typeDefinitions         map[*ast.Object]*stringTypeDefinition
	localTypeDefinitions    map[string]map[string][]*stringTypeDefinition
	importedTypeDefinitions map[string]map[string][]*stringTypeDefinition
	iotaBySpec              map[*ast.ValueSpec]int64
	currentIota             *int64
	currentNamespace        resolutionNamespace
}

func evaluatePackageStringConstants(files []fileContext, module string) error {
	groups := make(map[string][]int)
	for i := range files {
		if files[i].File == nil {
			continue
		}
		ns := namespaceForFile(module, files[i])
		groups[ns.localKey] = append(groups[ns.localKey], i)
	}
	evaluator := newStringConstantEvaluator(files, module)
	for _, key := range packageStringEvaluationKeys(groups) {
		for _, index := range groups[key] {
			file := &files[index]
			file.StringValues = make(map[ast.Expr][]string)
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
				previousIota := evaluator.currentIota
				if iotaValue, isConstSpec := evaluator.iotaBySpec[spec]; isConstSpec {
					currentIota := iotaValue
					evaluator.currentIota = &currentIota
				} else {
					evaluator.currentIota = nil
				}
				for _, expr := range spec.Values {
					variants, err := evaluator.resolve(expr, referenceConstraint, file.IsTest, make(map[*stringConstantDefinition]bool))
					if err != nil {
						evaluationErr = fmt.Errorf("%s: evaluate string constant: %w", file.Path, err)
						break
					}
					if values := distinctVariantValues(variants); len(values) > 0 {
						file.StringValues[expr] = values
					}
				}
				evaluator.currentIota = previousIota
				return evaluationErr == nil
			})
			if evaluationErr != nil {
				return evaluationErr
			}
		}
	}
	return nil
}

func namespaceForFile(module string, file fileContext) resolutionNamespace {
	packagePath := strings.TrimSuffix(strings.TrimSpace(module), "/")
	if file.PackageDir != "" {
		packagePath += "/" + file.PackageDir
	}
	packageName := ""
	if file.File != nil && file.File.Name != nil {
		packageName = file.File.Name.Name
	}
	return resolutionNamespace{
		packagePath: packagePath,
		localKey:    packagePath + "\x00" + packageName,
		imports:     file.Imports,
		dotImports:  file.DotImports,
	}
}

func newStringConstantEvaluator(files []fileContext, module string) *stringConstantEvaluator {
	evaluator := &stringConstantEvaluator{
		definitions:             make(map[*ast.Object]*stringConstantDefinition),
		localDefinitions:        make(map[string]map[string][]*stringConstantDefinition),
		importedDefinitions:     make(map[string]map[string][]*stringConstantDefinition),
		typeDefinitions:         make(map[*ast.Object]*stringTypeDefinition),
		localTypeDefinitions:    make(map[string]map[string][]*stringTypeDefinition),
		importedTypeDefinitions: make(map[string]map[string][]*stringTypeDefinition),
		iotaBySpec:              make(map[*ast.ValueSpec]int64),
	}
	for _, file := range files {
		if file.File == nil {
			continue
		}
		fileConstraint := buildConstraintForFile(file)
		namespace := namespaceForFile(module, file)

		ast.Inspect(file.File, func(node ast.Node) bool {
			gen, ok := node.(*ast.GenDecl)
			if !ok {
				return true
			}
			if gen.Tok != token.CONST {
				return true
			}
			evaluator.collectGenDecl(gen, fileConstraint, file.IsTest, namespace, false, false)
			return false
		})

		ast.Inspect(file.File, func(node ast.Node) bool {
			spec, ok := node.(*ast.TypeSpec)
			if !ok {
				return true
			}
			evaluator.collectTypeSpec(spec, fileConstraint, file.IsTest, namespace, false, false)
			return true
		})

		importable := !strings.HasSuffix(file.Path, "_test.go")
		for _, decl := range file.File.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			switch gen.Tok {
			case token.CONST:
				evaluator.collectGenDecl(gen, fileConstraint, file.IsTest, namespace, true, importable)
			case token.TYPE:
				for _, rawSpec := range gen.Specs {
					if spec, ok := rawSpec.(*ast.TypeSpec); ok {
						evaluator.collectTypeSpec(spec, fileConstraint, file.IsTest, namespace, true, importable)
					}
				}
			}
		}
	}
	return evaluator
}

func (e *stringConstantEvaluator) collectGenDecl(gen *ast.GenDecl, fileConstraint constraint.Expr, testOnly bool, namespace resolutionNamespace, packageLevel, importable bool) {
	var inherited []ast.Expr
	for specIndex, rawSpec := range gen.Specs {
		spec, ok := rawSpec.(*ast.ValueSpec)
		if !ok {
			continue
		}
		iotaValue := int64(specIndex)
		e.iotaBySpec[spec] = iotaValue
		values := spec.Values
		if len(values) == 0 {
			values = inherited
		} else {
			inherited = values
		}
		for i, name := range spec.Names {
			expr, ok := valueForConstantName(values, i, len(spec.Names))
			if !ok {
				continue
			}
			definition := &stringConstantDefinition{
				object: name.Obj, expr: expr, constraint: fileConstraint,
				testOnly: testOnly, iotaValue: iotaValue, namespace: namespace,
			}
			if name.Obj != nil {
				e.definitions[name.Obj] = definition
			}
			if packageLevel {
				appendConstantDefinition(e.localDefinitions, namespace.localKey, name.Name, definition)
				if importable && ast.IsExported(name.Name) {
					appendConstantDefinition(e.importedDefinitions, namespace.packagePath, name.Name, definition)
				}
			}
		}
	}
}

func appendConstantDefinition(index map[string]map[string][]*stringConstantDefinition, packageKey, name string, definition *stringConstantDefinition) {
	if index[packageKey] == nil {
		index[packageKey] = make(map[string][]*stringConstantDefinition)
	}
	index[packageKey][name] = append(index[packageKey][name], definition)
}

func (e *stringConstantEvaluator) collectTypeSpec(spec *ast.TypeSpec, fileConstraint constraint.Expr, testOnly bool, namespace resolutionNamespace, packageLevel, importable bool) {
	if spec == nil || spec.Name == nil || spec.Type == nil {
		return
	}
	definition := &stringTypeDefinition{
		object: spec.Name.Obj, expr: spec.Type, constraint: fileConstraint,
		testOnly: testOnly, namespace: namespace,
	}
	if spec.Name.Obj != nil {
		e.typeDefinitions[spec.Name.Obj] = definition
	}
	if packageLevel {
		appendTypeDefinition(e.localTypeDefinitions, namespace.localKey, spec.Name.Name, definition)
		if importable && ast.IsExported(spec.Name.Name) {
			appendTypeDefinition(e.importedTypeDefinitions, namespace.packagePath, spec.Name.Name, definition)
		}
	}
}

func appendTypeDefinition(index map[string]map[string][]*stringTypeDefinition, packageKey, name string, definition *stringTypeDefinition) {
	if index[packageKey] == nil {
		index[packageKey] = make(map[string][]*stringTypeDefinition)
	}
	index[packageKey][name] = append(index[packageKey][name], definition)
}

func valueForConstantName(values []ast.Expr, index, nameCount int) (ast.Expr, bool) {
	if index < len(values) {
		return values[index], true
	}
	if len(values) == 1 && nameCount == 1 {
		return values[0], true
	}
	return nil, false
}

func (e *stringConstantEvaluator) resolve(expr ast.Expr, context constraint.Expr, includeTests bool, visiting map[*stringConstantDefinition]bool) ([]stringVariant, error) {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind != token.STRING {
			return nil, nil
		}
		decoded, err := strconv.Unquote(value.Value)
		if err != nil {
			return nil, nil
		}
		return []stringVariant{{value: decoded, constraint: context}}, nil
	case *ast.BinaryExpr:
		if value.Op != token.ADD {
			return nil, nil
		}
		left, err := e.resolve(value.X, context, includeTests, visiting)
		if err != nil {
			return nil, err
		}
		right, err := e.resolve(value.Y, context, includeTests, visiting)
		if err != nil {
			return nil, err
		}
		return combineStringVariants(left, right)
	case *ast.ParenExpr:
		return e.resolve(value.X, context, includeTests, visiting)
	case *ast.CallExpr:
		if len(value.Args) != 1 {
			return nil, nil
		}
		typeConstraint, ok := e.stringConversionConstraint(value.Fun, context, includeTests, make(map[*stringTypeDefinition]bool))
		if !ok {
			return nil, nil
		}
		return e.resolveStringConversionArgument(value.Args[0], typeConstraint, includeTests, visiting)
	case *ast.Ident:
		if value.Obj != nil {
			if value.Obj.Kind != ast.Con {
				return nil, nil
			}
			definition, ok := e.definitions[value.Obj]
			if !ok {
				return nil, nil
			}
			return e.resolveDefinition(definition, context, includeTests, visiting)
		}
		return e.resolveConstantDefinitions(e.currentConstantDefinitions(value.Name, context, includeTests), context, includeTests, visiting)
	case *ast.SelectorExpr:
		return e.resolveConstantDefinitions(e.importedConstantDefinitions(value), context, includeTests, visiting)
	default:
		return nil, nil
	}
}

func (e *stringConstantEvaluator) resolveConstantDefinitions(definitions []*stringConstantDefinition, context constraint.Expr, includeTests bool, visiting map[*stringConstantDefinition]bool) ([]stringVariant, error) {
	var variants []stringVariant
	for _, definition := range definitions {
		resolved, err := e.resolveDefinition(definition, context, includeTests, visiting)
		if err != nil {
			return nil, err
		}
		variants = append(variants, resolved...)
	}
	return mergeStringVariants(variants)
}

func (e *stringConstantEvaluator) currentConstantDefinitions(name string, context constraint.Expr, includeTests bool) []*stringConstantDefinition {
	local := compatibleConstantDefinitions(e.localDefinitions[e.currentNamespace.localKey][name], context, includeTests)
	if len(local) > 0 {
		return local
	}
	var selected []*stringConstantDefinition
	providers := 0
	for packagePath := range e.currentNamespace.dotImports {
		definitions := compatibleConstantDefinitions(e.importedDefinitions[packagePath][name], context, includeTests)
		if len(definitions) == 0 {
			continue
		}
		providers++
		selected = definitions
	}
	if providers != 1 {
		return nil
	}
	return selected
}

func compatibleConstantDefinitions(definitions []*stringConstantDefinition, context constraint.Expr, includeTests bool) []*stringConstantDefinition {
	out := make([]*stringConstantDefinition, 0, len(definitions))
	for _, definition := range definitions {
		if definition == nil || definition.testOnly && !includeTests {
			continue
		}
		if buildConstraintsOverlap(andBuildConstraints(context, definition.constraint), nil) {
			out = append(out, definition)
		}
	}
	return out
}

func (e *stringConstantEvaluator) importedConstantDefinitions(selector *ast.SelectorExpr) []*stringConstantDefinition {
	if selector == nil || selector.Sel == nil {
		return nil
	}
	alias, ok := selector.X.(*ast.Ident)
	if !ok || alias.Obj != nil {
		return nil
	}
	packagePath := e.currentNamespace.imports[alias.Name]
	if packagePath == "" {
		return nil
	}
	return e.importedDefinitions[packagePath][selector.Sel.Name]
}

func (e *stringConstantEvaluator) resolveDefinition(definition *stringConstantDefinition, context constraint.Expr, includeTests bool, visiting map[*stringConstantDefinition]bool) ([]stringVariant, error) {
	if definition == nil || visiting[definition] || definition.testOnly && !includeTests {
		return nil, nil
	}
	combined := andBuildConstraints(context, definition.constraint)
	if !buildConstraintsOverlap(combined, nil) {
		return nil, nil
	}
	visiting[definition] = true
	previousIota, previousNamespace := e.currentIota, e.currentNamespace
	iotaValue := definition.iotaValue
	e.currentIota, e.currentNamespace = &iotaValue, definition.namespace
	resolved, err := e.resolve(definition.expr, combined, includeTests, visiting)
	e.currentIota, e.currentNamespace = previousIota, previousNamespace
	delete(visiting, definition)
	return resolved, err
}

func (e *stringConstantEvaluator) stringConversionConstraint(expr ast.Expr, context constraint.Expr, includeTests bool, visiting map[*stringTypeDefinition]bool) (constraint.Expr, bool) {
	switch value := expr.(type) {
	case *ast.ParenExpr:
		return e.stringConversionConstraint(value.X, context, includeTests, visiting)
	case *ast.IndexExpr:
		return e.stringConversionConstraint(value.X, context, includeTests, visiting)
	case *ast.IndexListExpr:
		return e.stringConversionConstraint(value.X, context, includeTests, visiting)
	case *ast.Ident:
		if value.Obj == nil {
			if value.Name == "string" {
				return context, true
			}
			return e.packageStringTypeConstraint(e.currentTypeDefinitions(value.Name, context, includeTests), context, includeTests, visiting)
		}
		if value.Obj.Kind != ast.Typ {
			return nil, false
		}
		definition, ok := e.typeDefinitions[value.Obj]
		if !ok {
			return nil, false
		}
		return e.resolveStringTypeDefinition(definition, context, includeTests, visiting)
	case *ast.SelectorExpr:
		return e.packageStringTypeConstraint(e.importedTypeDefinitionsForSelector(value), context, includeTests, visiting)
	default:
		return nil, false
	}
}

func (e *stringConstantEvaluator) currentTypeDefinitions(name string, context constraint.Expr, includeTests bool) []*stringTypeDefinition {
	local := compatibleTypeDefinitions(e.localTypeDefinitions[e.currentNamespace.localKey][name], context, includeTests)
	if len(local) > 0 {
		return local
	}
	var selected []*stringTypeDefinition
	providers := 0
	for packagePath := range e.currentNamespace.dotImports {
		definitions := compatibleTypeDefinitions(e.importedTypeDefinitions[packagePath][name], context, includeTests)
		if len(definitions) == 0 {
			continue
		}
		providers++
		selected = definitions
	}
	if providers != 1 {
		return nil
	}
	return selected
}

func compatibleTypeDefinitions(definitions []*stringTypeDefinition, context constraint.Expr, includeTests bool) []*stringTypeDefinition {
	out := make([]*stringTypeDefinition, 0, len(definitions))
	for _, definition := range definitions {
		if definition == nil || definition.testOnly && !includeTests {
			continue
		}
		if buildConstraintsOverlap(andBuildConstraints(context, definition.constraint), nil) {
			out = append(out, definition)
		}
	}
	return out
}

func (e *stringConstantEvaluator) importedTypeDefinitionsForSelector(selector *ast.SelectorExpr) []*stringTypeDefinition {
	if selector == nil || selector.Sel == nil {
		return nil
	}
	alias, ok := selector.X.(*ast.Ident)
	if !ok || alias.Obj != nil {
		return nil
	}
	packagePath := e.currentNamespace.imports[alias.Name]
	if packagePath == "" {
		return nil
	}
	return e.importedTypeDefinitions[packagePath][selector.Sel.Name]
}

func (e *stringConstantEvaluator) packageStringTypeConstraint(definitions []*stringTypeDefinition, context constraint.Expr, includeTests bool, visiting map[*stringTypeDefinition]bool) (constraint.Expr, bool) {
	var combined constraint.Expr
	found := false
	for _, definition := range definitions {
		candidate, ok := e.resolveStringTypeDefinition(definition, context, includeTests, visiting)
		if !ok {
			continue
		}
		if !found {
			combined, found = candidate, true
		} else {
			combined = orBuildConstraints(combined, candidate)
		}
	}
	return combined, found
}

func (e *stringConstantEvaluator) resolveStringTypeDefinition(definition *stringTypeDefinition, context constraint.Expr, includeTests bool, visiting map[*stringTypeDefinition]bool) (constraint.Expr, bool) {
	if definition == nil || visiting[definition] || definition.testOnly && !includeTests {
		return nil, false
	}
	combined := andBuildConstraints(context, definition.constraint)
	if !buildConstraintsOverlap(combined, nil) {
		return nil, false
	}
	visiting[definition] = true
	previousNamespace := e.currentNamespace
	e.currentNamespace = definition.namespace
	resolved, ok := e.stringConversionConstraint(definition.expr, combined, includeTests, visiting)
	e.currentNamespace = previousNamespace
	delete(visiting, definition)
	return resolved, ok
}

func combineStringVariants(left, right []stringVariant) ([]stringVariant, error) {
	if len(left) == 0 || len(right) == 0 {
		return nil, nil
	}
	combined := make([]stringVariant, 0, len(left)*len(right))
	for _, lhs := range left {
		for _, rhs := range right {
			buildConstraint := andBuildConstraints(lhs.constraint, rhs.constraint)
			if !buildConstraintsOverlap(buildConstraint, nil) {
				continue
			}
			combined = append(combined, stringVariant{value: lhs.value + rhs.value, constraint: buildConstraint})
			if len(combined) > maxResolvedStringVariants*4 {
				return nil, fmt.Errorf("string constant expands to too many build variants")
			}
		}
	}
	return mergeStringVariants(combined)
}

func mergeStringVariants(variants []stringVariant) ([]stringVariant, error) {
	if len(variants) == 0 {
		return nil, nil
	}
	byValue := make(map[string]constraint.Expr)
	for _, variant := range variants {
		if prior, ok := byValue[variant.value]; ok {
			byValue[variant.value] = orBuildConstraints(prior, variant.constraint)
		} else {
			byValue[variant.value] = variant.constraint
		}
		if len(byValue) > maxResolvedStringVariants {
			return nil, fmt.Errorf("string constant resolves to more than %d distinct values", maxResolvedStringVariants)
		}
	}
	values := make([]string, 0, len(byValue))
	for value := range byValue {
		values = append(values, value)
	}
	sort.Strings(values)
	out := make([]stringVariant, 0, len(values))
	for _, value := range values {
		out = append(out, stringVariant{value: value, constraint: byValue[value]})
	}
	return out, nil
}

func distinctVariantValues(variants []stringVariant) []string {
	seen := make(map[string]struct{}, len(variants))
	values := make([]string, 0, len(variants))
	for _, variant := range variants {
		if _, ok := seen[variant.value]; ok {
			continue
		}
		seen[variant.value] = struct{}{}
		values = append(values, variant.value)
	}
	sort.Strings(values)
	return values
}

func andBuildConstraints(left, right constraint.Expr) constraint.Expr {
	switch {
	case left == nil:
		return right
	case right == nil:
		return left
	default:
		return &constraint.AndExpr{X: left, Y: right}
	}
}

func orBuildConstraints(left, right constraint.Expr) constraint.Expr {
	if left == nil || right == nil {
		return nil
	}
	return &constraint.OrExpr{X: left, Y: right}
}

package api

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Only these reviewed forwarding contracts receive contextual analysis. This
// is not a terminal boundary: every selected body and every reachable local
// callback body still contributes its calls and settings-lock acquisitions.
func weeklyProductionContextParameters(t *testing.T, inventory weeklyTypedInventory) map[*types.Func][]int {
	t.Helper()
	parameters := map[string][]int{
		"readStateFileInodeAnchoredWithStrictPolicy":        {1},
		"readStateFileInodeAnchoredWithStrictPolicyNoAudit": {1},
		"readStateFileInodeAnchoredWithOptions":             {1, 3, 5, 6},
		"readStateFileInodeAnchoredWithObservation":         {1, 3, 5, 6, 8},
		"observedStateReadBoundary":                         {1},
		"readStateFileBeneathRootNoFollow":                  {4},
		"readStateFileBeneathRootNoFollowWithPolicy":        {4},
		"invokeStateReadBeneathRootStep":                    {0},
	}
	if runtime.GOOS == "windows" {
		for name, indices := range map[string][]int{
			"windowsDACLAllowlistViolationsFromDescriptor": {3},
			"verifyWindowsDACLFromHandleMaskedObserved":    {2},
			"ntCreateRelativeObserved":                     {6},
			"ntCreateRelativeWithShareAccessObserved":      {7},
			"secureParentDirAnywhereModeObserved":          {4},
			"mkdirOrVerifyRealDirWindowsObserved":          {5},
			"openExistingRealDirAtObserved":                {2},
			"parentDescentSlot":                            {0},
			"registerParentDirectory":                      {0},
			"closeParentDirectory":                         {0},
			"parentObservedError":                          {0},
			"joinParentObserved":                           {0},
			"isAlreadyExistsErr":                           {0},
			"errnoIs":                                      {0},
			"ntStatusIs":                                   {0},
		} {
			parameters[name] = indices
		}
	}
	functions := make(map[*types.Func][]int, len(parameters))
	for name, indices := range parameters {
		functions[weeklyRequiredPackageFunc(t, inventory, name)] = indices
	}
	return functions
}

type weeklyStaticBinding struct {
	kind         string
	boolean      bool
	function     *types.Func
	errorOrigins *weeklyErrorOrigins
}

func weeklyExpressionBinding(inventory weeklyTypedInventory, node *weeklyTypedNode, expression ast.Expr) (weeklyStaticBinding, bool) {
	identifier, ok := weeklyUnwrapParentheses(expression).(*ast.Ident)
	if !ok {
		return weeklyStaticBinding{}, false
	}
	object := inventory.info.Uses[identifier]
	if object == types.Universe.Lookup("nil") {
		return weeklyStaticBinding{kind: "nil"}, true
	}
	switch value := object.(type) {
	case *types.Const:
		if value.Val().Kind() == constant.Bool {
			return weeklyStaticBinding{kind: "bool", boolean: constant.BoolVal(value.Val())}, true
		}
	case *types.Func:
		signature, ok := value.Type().(*types.Signature)
		if ok && signature.Recv() == nil && inventory.byFunction[value] != nil {
			return weeklyStaticBinding{kind: "function", function: value}, true
		}
	case *types.Var:
		binding, known := node.bindings[value]
		return binding, known
	}
	return weeklyStaticBinding{}, false
}

func weeklyParenthesizedUse(index weeklyTypedASTIndex, identifier *ast.Ident) ast.Node {
	var current ast.Node = identifier
	for {
		parent, ok := index.parent[current].(*ast.ParenExpr)
		if !ok || parent.X != current {
			return current
		}
		current = parent
	}
}

// An unchanged parameter may be tested, invoked, inspected, or forwarded to a
// local body. Assignment, address escape, aliases, returned callbacks, and
// storage in another object discard the proof, even in a nested closure.
func weeklyBindingIsImmutable(inventory weeklyTypedInventory, index weeklyTypedASTIndex, node *weeklyTypedNode, variable *types.Var) bool {
	if len(weeklyNamedFunctionControlFlowPositions(inventory, node)) != 0 {
		return false
	}
	for identifier, object := range inventory.info.Uses {
		if object != variable {
			continue
		}
		current := weeklyParenthesizedUse(index, identifier)
		switch parent := index.parent[current].(type) {
		case *ast.SelectorExpr:
			if parent.X == current {
				continue
			}
		case *ast.BinaryExpr:
			continue
		case *ast.UnaryExpr:
			if parent.Op == token.NOT {
				continue
			}
		case *ast.IfStmt:
			if parent.Cond == current {
				continue
			}
		case *ast.ForStmt:
			if parent.Cond == current {
				continue
			}
		case *ast.CallExpr:
			if parent.Fun == current {
				continue
			}
			target, _ := weeklyCallTarget(inventory, parent)
			if function, ok := target.(*types.Func); ok && inventory.byFunction[function] != nil {
				continue
			}
		}
		return false
	}
	return true
}

func weeklySpecializeNode(inventory *weeklyTypedInventory, base *weeklyTypedNode, bindings map[*types.Var]weeklyStaticBinding) (*weeklyTypedNode, error) {
	if len(bindings) == 0 {
		return base, nil
	}
	parts := make([]string, 0, len(bindings))
	for variable, binding := range bindings {
		value := binding.kind
		switch binding.kind {
		case "bool":
			value = fmt.Sprint(binding.boolean)
		case "function":
			value = inventory.byFunction[binding.function].key
		case "error":
			value = binding.errorOrigins.key()
		}
		parts = append(parts, fmt.Sprintf("%s@%d=%s", variable.Name(), variable.Pos(), value))
	}
	sort.Strings(parts)
	key := base.key + "[" + strings.Join(parts, ",") + "]"
	if existing := inventory.nodes[key]; existing != nil {
		return existing, nil
	}
	// Recursive forwarding reuses an identical context. Mixed origins never
	// merge; excessive combinations fail closed instead of widening to a leaf.
	contexts := 0
	for _, node := range inventory.nodes {
		if node.origin == base {
			contexts++
		}
	}
	if contexts >= 16 {
		return nil, fmt.Errorf("local context limit exceeded for %s", base.key)
	}
	node := &weeklyTypedNode{key: key, function: base.function, body: base.body, origin: base, bindings: bindings}
	inventory.nodes[key] = node
	return node, nil
}

func weeklyLocalCallContext(inventory *weeklyTypedInventory, index weeklyTypedASTIndex, options weeklyTypedGraphOptions, caller, callee *weeklyTypedNode, call *ast.CallExpr, selection *types.Selection) (*weeklyTypedNode, error) {
	if callee == nil {
		return nil, errors.New("known local callback has no indexed body")
	}
	indices, reviewed := options.contextParameters[callee.function]
	if !reviewed {
		return callee, nil
	}
	signature, ok := callee.function.Type().(*types.Signature)
	if !ok || signature.Variadic() || signature.Recv() != nil || selection != nil || signature.Params().Len() != len(call.Args) {
		return nil, fmt.Errorf("unsupported contextual call shape for %s", callee.key)
	}
	bindings := map[*types.Var]weeklyStaticBinding{}
	for _, i := range indices {
		if i < 0 || i >= signature.Params().Len() {
			return nil, fmt.Errorf("context parameter %d is outside %s signature", i, callee.key)
		}
		argument := call.Args[i]
		parameter := signature.Params().At(i)
		if weeklyIsErrorType(parameter.Type()) {
			resolver := weeklyNewErrorOriginResolver(*inventory, index, options.errorContracts)
			if !resolver.readOnly(callee, parameter, nil) {
				return nil, fmt.Errorf("error context parameter %s.%s is mutable or captured", callee.key, parameter.Name())
			}
			origins, err := resolver.expression(caller, argument, 0)
			if err != nil {
				return nil, fmt.Errorf("error context parameter %s.%s: %w", callee.key, parameter.Name(), err)
			}
			bindings[parameter] = weeklyStaticBinding{kind: "error", errorOrigins: origins}
			continue
		}
		binding, known := weeklyExpressionBinding(*inventory, caller, argument)
		if !known || !weeklyBindingIsImmutable(*inventory, index, callee, parameter) {
			return nil, fmt.Errorf("context parameter %s.%s has an unknown, aliased, or mutable origin", callee.key, parameter.Name())
		}
		// Do not interpret a typed-nil pointer stored in an interface as a nil
		// interface. Only identical forwarding types or the literal nil qualify.
		if binding.kind == "nil" && inventory.info.TypeOf(argument) != types.Typ[types.UntypedNil] && !types.Identical(inventory.info.TypeOf(argument), parameter.Type()) {
			return nil, fmt.Errorf("context parameter %s.%s changes the type of a nil value", callee.key, parameter.Name())
		}
		switch parameter.Type().Underlying().(type) {
		case *types.Pointer, *types.Interface, *types.Signature:
			if binding.kind == "nil" || binding.kind == "function" {
				bindings[parameter] = binding
			}
		case *types.Basic:
			if binding.kind == "bool" && types.Identical(parameter.Type(), types.Typ[types.Bool]) {
				bindings[parameter] = binding
			}
		}
		if _, bound := bindings[parameter]; !bound {
			return nil, fmt.Errorf("context parameter %s.%s has unsupported value type", callee.key, parameter.Name())
		}
	}
	return weeklySpecializeNode(inventory, callee, bindings)
}

func weeklyLiteralContext(inventory *weeklyTypedInventory, caller, literal *weeklyTypedNode) (*weeklyTypedNode, error) {
	bindings := map[*types.Var]weeklyStaticBinding{}
	if len(weeklyNamedFunctionControlFlowPositions(*inventory, literal)) != 0 {
		return literal, nil
	}
	for identifier, object := range inventory.info.Uses {
		variable, ok := object.(*types.Var)
		if !ok || identifier.Pos() < literal.body.Pos() || identifier.Pos() > literal.body.End() {
			continue
		}
		if binding, known := caller.bindings[variable]; known {
			bindings[variable] = binding
		}
	}
	return weeklySpecializeNode(inventory, literal, bindings)
}

func weeklyLocalLiteralOrigin(inventory *weeklyTypedInventory, index weeklyTypedASTIndex, node *weeklyTypedNode, variable *types.Var) (*weeklyTypedNode, error) {
	if len(weeklyNamedFunctionControlFlowPositions(*inventory, node)) != 0 {
		return nil, errors.New("local literal origin crosses label/goto control flow")
	}
	var declaration *ast.Ident
	for identifier, object := range inventory.info.Defs {
		if object == variable {
			declaration = identifier
			break
		}
	}
	assignment, ok := index.parent[declaration].(*ast.AssignStmt)
	if !ok || assignment.Tok != token.DEFINE || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 || index.parent[assignment] != node.body {
		return nil, errors.New("local literal origin is not one top-level := definition")
	}
	literal, ok := weeklyUnwrapParentheses(assignment.Rhs[0]).(*ast.FuncLit)
	if !ok {
		return nil, errors.New("local literal origin is not an exact function literal")
	}
	_, definitionIndex := weeklyTopLevelStatementFromIndex(index, node.body, assignment)
	uses := 0
	for identifier, object := range inventory.info.Uses {
		if object != variable {
			continue
		}
		current := weeklyParenthesizedUse(index, identifier)
		call, ok := index.parent[current].(*ast.CallExpr)
		if !ok || call.Fun != current || weeklyTopLevelStatementIndexContaining(node.body, call) <= definitionIndex {
			return nil, errors.New("local literal has a write, alias, capture, or non-dominated call")
		}
		uses++
	}
	if uses == 0 {
		return nil, errors.New("local literal has no direct call use")
	}
	child, err := inventory.addSyntheticNode("local-literal", literal)
	if err != nil {
		return nil, err
	}
	return weeklyLiteralContext(inventory, node, child)
}

func weeklyConditionInContext(inventory weeklyTypedInventory, node *weeklyTypedNode, expression ast.Expr) (bool, bool) {
	if expression == nil {
		return false, false
	}
	if binding, known := weeklyExpressionBinding(inventory, node, expression); known && binding.kind == "bool" {
		return binding.boolean, true
	}
	switch value := weeklyUnwrapParentheses(expression).(type) {
	case *ast.UnaryExpr:
		if value.Op == token.NOT {
			result, known := weeklyConditionInContext(inventory, node, value.X)
			return !result, known
		}
	case *ast.BinaryExpr:
		if value.Op == token.LAND || value.Op == token.LOR {
			left, leftKnown := weeklyConditionInContext(inventory, node, value.X)
			right, rightKnown := weeklyConditionInContext(inventory, node, value.Y)
			if value.Op == token.LAND {
				return left && right, (leftKnown && rightKnown) || (leftKnown && !left) || (rightKnown && !right)
			}
			return left || right, (leftKnown && rightKnown) || (leftKnown && left) || (rightKnown && right)
		}
		if value.Op == token.EQL || value.Op == token.NEQ {
			left, leftKnown := weeklyExpressionBinding(inventory, node, value.X)
			right, rightKnown := weeklyExpressionBinding(inventory, node, value.Y)
			if leftKnown && rightKnown && (left.kind == "nil" || right.kind == "nil") {
				// A closed error set may contain both nil and concrete values.
				// Do not prune either path merely because the context is known.
				if left.kind == "error" || right.kind == "error" {
					return false, false
				}
				equal := left.kind == right.kind
				if value.Op == token.NEQ {
					equal = !equal
				}
				return equal, true
			}
		}
	}
	return false, false
}

func weeklyStatementReturns(inventory weeklyTypedInventory, node *weeklyTypedNode, statement ast.Stmt) bool {
	switch value := statement.(type) {
	case *ast.ReturnStmt:
		return true
	case *ast.BlockStmt:
		for _, child := range value.List {
			if weeklyStatementReturns(inventory, node, child) {
				return true
			}
		}
	case *ast.IfStmt:
		condition, known := weeklyConditionInContext(inventory, node, value.Cond)
		if known {
			if condition {
				return weeklyStatementReturns(inventory, node, value.Body)
			}
			return weeklyStatementReturns(inventory, node, value.Else)
		}
		return weeklyStatementReturns(inventory, node, value.Body) && weeklyStatementReturns(inventory, node, value.Else)
	}
	return false
}

func weeklyCallsInContext(inventory weeklyTypedInventory, node *weeklyTypedNode) []*ast.CallExpr {
	calls := []*ast.CallExpr{}
	if len(node.bindings) == 0 {
		weeklyForEachDirectCall(node, func(call *ast.CallExpr) { calls = append(calls, call) })
		return calls
	}
	var walk func(ast.Node)
	walk = func(candidate ast.Node) {
		if candidate == nil {
			return
		}
		switch value := candidate.(type) {
		case *ast.FuncLit:
			return
		case *ast.BlockStmt:
			for _, statement := range value.List {
				walk(statement)
				if weeklyStatementReturns(inventory, node, statement) {
					break
				}
			}
			return
		case *ast.IfStmt:
			walk(value.Init)
			walk(value.Cond)
			condition, known := weeklyConditionInContext(inventory, node, value.Cond)
			if !known || condition {
				walk(value.Body)
			}
			if !known || !condition {
				walk(value.Else)
			}
			return
		case *ast.BinaryExpr:
			if value.Op == token.LAND || value.Op == token.LOR {
				walk(value.X)
				left, known := weeklyConditionInContext(inventory, node, value.X)
				if !known || (value.Op == token.LAND && left) || (value.Op == token.LOR && !left) {
					walk(value.Y)
				}
				return
			}
		case *ast.CallExpr:
			calls = append(calls, value)
		}
		ast.Inspect(candidate, func(child ast.Node) bool {
			if child == candidate {
				return true
			}
			walk(child)
			return false
		})
	}
	walk(node.body)
	return calls
}

// Error origins are a finite set of concrete dynamic types, plus possible nil.
// They are not a list of methods found elsewhere in the package. Unknown
// producers, fields, mutations, and unindexed method bodies fail closed.
type weeklyErrorOrigins struct {
	types       map[string]types.Type
	includesNil bool
	// This is a proved elimination of standard formatting/unwrap links, not
	// an invented concrete Go type. Its leaves and formatting methods must
	// satisfy the exact installed SDK contract before it can be constructed.
	// A non-nil map denotes nil, its listed native leaves, and finite standard
	// wrappers around those values. It retains terminal provenance after
	// eliminating standard links; the "nil" entry has a nil types.Type.
	nativeChain map[string]types.Type
}

func weeklyNewErrorOrigins() *weeklyErrorOrigins {
	return &weeklyErrorOrigins{types: map[string]types.Type{}}
}

func weeklyErrorTypeKey(typ types.Type) string {
	return types.TypeString(typ, func(pkg *types.Package) string { return pkg.Path() })
}

func (origins *weeklyErrorOrigins) names() []string {
	names := make([]string, 0, len(origins.types))
	for name := range origins.types {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (origins *weeklyErrorOrigins) key() string {
	parts := make([]string, 0, len(origins.types)+1)
	if origins.includesNil {
		parts = append(parts, "nil")
	}
	if origins.nativeChain != nil {
		leaves := make([]string, 0, len(origins.nativeChain))
		for name := range origins.nativeChain {
			leaves = append(leaves, name)
		}
		sort.Strings(leaves)
		parts = append(parts, "reviewed-standard-chain{"+strings.Join(leaves, "|")+"}")
	}
	for name := range origins.types {
		parts = append(parts, name)
	}
	sort.Strings(parts)
	return "error{" + strings.Join(parts, "|") + "}"
}

func (origins *weeklyErrorOrigins) merge(other *weeklyErrorOrigins) error {
	origins.includesNil = origins.includesNil || other.includesNil
	if other.nativeChain != nil {
		if origins.nativeChain == nil {
			origins.nativeChain = map[string]types.Type{}
		}
		for name, typ := range other.nativeChain {
			origins.nativeChain[name] = typ
		}
	}
	for name, typ := range other.types {
		origins.types[name] = typ
	}
	if len(origins.types) > 16 {
		return errors.New("error origin type limit exceeded")
	}
	return nil
}

func weeklyIsErrorType(typ types.Type) bool {
	return types.Identical(typ, types.Universe.Lookup("error").Type())
}

func weeklyErrorUnwrapSelection(selection *types.Selection) bool {
	if selection == nil || selection.Kind() != types.MethodVal {
		return false
	}
	if _, ok := selection.Recv().Underlying().(*types.Interface); !ok {
		return false
	}
	return weeklyErrorUnwrapMethod(selection.Obj())
}

func weeklyErrorUnwrapMethod(object types.Object) bool {
	function, ok := object.(*types.Func)
	if !ok || function.Name() != "Unwrap" {
		return false
	}
	signature, ok := function.Type().(*types.Signature)
	return ok && !signature.Variadic() && signature.Params().Len() == 0 && signature.Results().Len() == 1 && weeklyIsErrorType(signature.Results().At(0).Type())
}

type weeklyErrorOriginResolver struct {
	inventory        weeklyTypedInventory
	index            weeklyTypedASTIndex
	active           map[string]bool
	depth            int
	contracts        *weeklyNativeErrorContracts
	standardDispatch bool
}

func weeklyNewErrorOriginResolver(inventory weeklyTypedInventory, index weeklyTypedASTIndex, contracts *weeklyNativeErrorContracts) *weeklyErrorOriginResolver {
	return &weeklyErrorOriginResolver{inventory: inventory, index: index, active: map[string]bool{}, contracts: contracts}
}

// Copying an error interface cannot change its dynamic type. Writes to the
// original variable, address escape, and closure capture can, so none is
// accepted. The one separately validated unwrap-loop assignment is optional.
func (resolver *weeklyErrorOriginResolver) readOnly(node *weeklyTypedNode, variable *types.Var, allowed *ast.AssignStmt) bool {
	return resolver.readOnlyBefore(node, variable, allowed, nil)
}

func (resolver *weeklyErrorOriginResolver) readOnlyBefore(node *weeklyTypedNode, variable *types.Var, allowed *ast.AssignStmt, read ast.Expr) bool {
	if len(weeklyNamedFunctionControlFlowPositions(resolver.inventory, node)) != 0 {
		return false
	}
	// A later assignment cannot change an already copied interface value.
	// Do not use source order across a loop back edge or closure capture.
	if read != nil {
		for ancestor := resolver.index.parent[read]; ancestor != nil && ancestor != node.body; ancestor = resolver.index.parent[ancestor] {
			switch ancestor.(type) {
			case *ast.ForStmt, *ast.RangeStmt, *ast.FuncLit:
				read = nil
			}
		}
	}
	for identifier, object := range resolver.inventory.info.Uses {
		if object != variable {
			continue
		}
		current := weeklyParenthesizedUse(resolver.index, identifier)
		switch parent := resolver.index.parent[current].(type) {
		case *ast.AssignStmt:
			for _, lhs := range parent.Lhs {
				if lhs == current && parent != allowed && (read == nil || parent.Pos() <= read.Pos()) {
					return false
				}
			}
		case *ast.UnaryExpr:
			if parent.Op == token.AND {
				return false
			}
		case *ast.IncDecStmt, *ast.RangeStmt:
			return false
		}
		for ancestor := resolver.index.parent[current]; ancestor != nil && ancestor != node.body; ancestor = resolver.index.parent[ancestor] {
			if _, nested := ancestor.(*ast.FuncLit); nested {
				return false
			}
		}
	}
	return true
}

func (resolver *weeklyErrorOriginResolver) definition(variable *types.Var) (*ast.AssignStmt, int, error) {
	for identifier, object := range resolver.inventory.info.Defs {
		if object != variable {
			continue
		}
		assignment, ok := resolver.index.parent[identifier].(*ast.AssignStmt)
		if !ok || assignment.Tok != token.DEFINE {
			break
		}
		for i, lhs := range assignment.Lhs {
			if lhs == identifier {
				return assignment, i, nil
			}
		}
	}
	return nil, 0, fmt.Errorf("error variable %s has no exact local := origin", variable.Name())
}

// A concrete result type does not prove the behavior of its producer. Only
// constant values and literal construction with no variable/call inputs can
// bypass origin tracing; calls, aliases, and fields must retain that boundary.
func weeklyPureErrorLiteral(inventory weeklyTypedInventory, expression ast.Expr, depth int) bool {
	if depth > 16 {
		return false
	}
	expression = weeklyUnwrapParentheses(expression)
	if inventory.info.Types[expression].Value != nil || weeklyExpressionObject(inventory, expression) == types.Universe.Lookup("nil") {
		return true
	}
	switch value := expression.(type) {
	case *ast.CompositeLit:
		typ := inventory.info.TypeOf(value)
		if typ == nil {
			return false
		}
		_, structure := typ.Underlying().(*types.Struct)
		for _, element := range value.Elts {
			if keyed, ok := element.(*ast.KeyValueExpr); ok {
				if !structure && !weeklyPureErrorLiteral(inventory, keyed.Key, depth+1) {
					return false
				}
				element = keyed.Value
			}
			if !weeklyPureErrorLiteral(inventory, element, depth+1) {
				return false
			}
		}
		return true
	case *ast.UnaryExpr:
		return value.Op == token.AND && weeklyPureErrorLiteral(inventory, value.X, depth+1)
	case *ast.CallExpr:
		// Boxing a typed nil pointer must retain its concrete method set.
		return inventory.info.Types[value.Fun].IsType() && len(value.Args) == 1 && weeklyExpressionObject(inventory, value.Args[0]) == types.Universe.Lookup("nil")
	}
	return false
}

func (resolver *weeklyErrorOriginResolver) expression(node *weeklyTypedNode, expression ast.Expr, resultIndex int) (*weeklyErrorOrigins, error) {
	resolver.depth++
	defer func() { resolver.depth-- }()
	if resolver.depth > 48 {
		return nil, errors.New("error origin depth limit exceeded")
	}
	expression = weeklyUnwrapParentheses(expression)
	origins := weeklyNewErrorOrigins()
	typ := resolver.inventory.info.TypeOf(expression)
	if weeklyExpressionObject(resolver.inventory, expression) == types.Universe.Lookup("nil") {
		origins.includesNil = true
		return origins, nil
	}
	if typ != nil {
		if _, tuple := typ.(*types.Tuple); !tuple {
			if _, dynamic := typ.Underlying().(*types.Interface); !dynamic && types.Implements(typ, types.Universe.Lookup("error").Type().Underlying().(*types.Interface)) && weeklyPureErrorLiteral(resolver.inventory, expression, 0) {
				origins.types[weeklyErrorTypeKey(typ)] = typ
				return origins, nil
			}
		}
	}
	switch value := expression.(type) {
	case *ast.Ident:
		variable, ok := resolver.inventory.info.Uses[value].(*types.Var)
		if !ok {
			return nil, fmt.Errorf("error expression %s has no variable origin", value.Name)
		}
		if binding, known := node.bindings[variable]; known {
			if binding.kind == "error" {
				return binding.errorOrigins, nil
			}
			if binding.kind == "nil" && weeklyIsErrorType(variable.Type()) {
				origins.includesNil = true
				return origins, nil
			}
		}
		definition, position, err := resolver.definition(variable)
		if err != nil {
			return nil, err
		}
		if loop, ok := resolver.index.parent[definition].(*ast.ForStmt); ok && loop.Init == definition {
			return resolver.loopOrigins(node, variable, definition, loop)
		}
		if !resolver.readOnlyBefore(node, variable, nil, value) {
			return nil, fmt.Errorf("error variable %s is mutable or captured", variable.Name())
		}
		if len(definition.Rhs) == len(definition.Lhs) {
			return resolver.expression(node, definition.Rhs[position], 0)
		}
		if len(definition.Rhs) == 1 {
			return resolver.expression(node, definition.Rhs[0], position)
		}
		return nil, fmt.Errorf("error variable %s has an unsupported definition", variable.Name())
	case *ast.TypeAssertExpr:
		origins, err := resolver.expression(node, value.X, 0)
		if err != nil {
			return nil, err
		}
		assertedType := resolver.inventory.info.TypeOf(value.Type)
		if assertedType == nil {
			return nil, errors.New("error assertion has no exact asserted type")
		}
		asserted, ok := assertedType.Underlying().(*types.Interface)
		if !ok {
			return nil, errors.New("error assertion does not have a known interface type")
		}
		filtered := weeklyNewErrorOrigins()
		if origins.nativeChain != nil {
			if !weeklyNativeChainAssertion(asserted) {
				return nil, errors.New("standard error chain crosses an unreviewed interface assertion")
			}
			filtered.nativeChain = origins.nativeChain
		}
		for name, typ := range origins.types {
			if types.Implements(typ, asserted) {
				filtered.types[name] = typ
			}
		}
		return filtered, nil
	case *ast.CallExpr:
		if resolver.inventory.info.Types[value.Fun].IsType() && len(value.Args) == 1 {
			if typ == nil {
				return nil, errors.New("error conversion has no exact result type")
			}
			if _, dynamic := typ.Underlying().(*types.Interface); !dynamic && !types.Identical(typ, resolver.inventory.info.TypeOf(value.Args[0])) {
				return nil, errors.New("error origin crosses an unreviewed concrete type conversion")
			}
			return resolver.expression(node, value.Args[0], 0)
		}
		target, selection := weeklyCallTarget(resolver.inventory, value)
		if function, ok := target.(*types.Func); ok && resolver.contracts != nil {
			if origin, exists := resolver.contracts.producers[weeklyFunctionResultOrigin{factory: function, resultIndex: resultIndex}]; exists {
				return origin, nil
			}
			if function == resolver.contracts.formatter {
				return resolver.nativeFormattedError(node, value)
			}
		}
		if weeklyErrorUnwrapSelection(selection) {
			selector := weeklyUnwrapCallExpression(value.Fun).(*ast.SelectorExpr)
			receiver, err := resolver.expression(node, selector.X, 0)
			if err != nil {
				return nil, err
			}
			origins.nativeChain = receiver.nativeChain
			targets, err := resolver.dispatchTargets(node, value)
			if err != nil {
				return nil, err
			}
			for _, target := range targets {
				returned, err := resolver.returns(target, 0)
				if err != nil {
					return nil, err
				}
				if err := origins.merge(returned); err != nil {
					return nil, err
				}
			}
			return origins, nil
		}
		function, ok := target.(*types.Func)
		if !ok {
			return nil, errors.New("error result comes from an unknown callback or call target")
		}
		callee := resolver.inventory.byFunction[function]
		if callee == nil {
			return nil, fmt.Errorf("error result from %s has no indexed producer body", function.FullName())
		}
		signature := function.Type().(*types.Signature)
		if signature.Variadic() || signature.TypeParams().Len() != 0 || signature.Params().Len() != len(value.Args) {
			return nil, fmt.Errorf("error producer %s has an unsupported call shape", callee.key)
		}
		context := &weeklyTypedNode{key: callee.key, function: function, body: callee.body, bindings: map[*types.Var]weeklyStaticBinding{}}
		for i, argument := range value.Args {
			parameter := signature.Params().At(i)
			if weeklyIsErrorType(parameter.Type()) {
				if !resolver.readOnly(callee, parameter, nil) {
					return nil, fmt.Errorf("error producer %s mutates or captures %s", callee.key, parameter.Name())
				}
				argumentOrigins, err := resolver.expression(node, argument, 0)
				if err != nil {
					return nil, err
				}
				context.bindings[parameter] = weeklyStaticBinding{kind: "error", errorOrigins: argumentOrigins}
			} else if binding, known := weeklyExpressionBinding(resolver.inventory, node, argument); known && weeklyBindingIsImmutable(resolver.inventory, resolver.index, callee, parameter) {
				// Preserve the same typed-nil rule as graph specialization.
				if binding.kind != "nil" || resolver.inventory.info.TypeOf(argument) == types.Typ[types.UntypedNil] || types.Identical(resolver.inventory.info.TypeOf(argument), parameter.Type()) {
					context.bindings[parameter] = binding
				}
			}
		}
		return resolver.returns(context, resultIndex)
	}
	return nil, fmt.Errorf("error expression %T has no closed origin", expression)
}

func (resolver *weeklyErrorOriginResolver) returns(node *weeklyTypedNode, resultIndex int) (*weeklyErrorOrigins, error) {
	if len(weeklyNamedFunctionControlFlowPositions(resolver.inventory, node)) != 0 {
		return nil, fmt.Errorf("error producer %s crosses label/goto control flow", node.key)
	}
	signature := node.function.Type().(*types.Signature)
	if resultIndex < 0 || resultIndex >= signature.Results().Len() || signature.Results().At(resultIndex).Name() != "" {
		return nil, fmt.Errorf("error producer %s has an unsupported or named result", node.key)
	}
	key := fmt.Sprintf("%s:%d", node.key, resultIndex)
	if resolver.active[key] {
		return nil, fmt.Errorf("error producer cycle at %s", key)
	}
	resolver.active[key] = true
	defer delete(resolver.active, key)
	origins := weeklyNewErrorOrigins()
	var failure error
	count := 0
	var walk func(ast.Node)
	walk = func(candidate ast.Node) {
		if candidate == nil || failure != nil {
			return
		}
		switch value := candidate.(type) {
		case *ast.FuncLit:
			return
		case *ast.BlockStmt:
			for _, statement := range value.List {
				walk(statement)
				if weeklyStatementReturns(resolver.inventory, node, statement) {
					break
				}
			}
			return
		case *ast.IfStmt:
			if condition, known := weeklyConditionInContext(resolver.inventory, node, value.Cond); known {
				if condition {
					walk(value.Body)
				} else {
					walk(value.Else)
				}
			} else {
				walk(value.Body)
				walk(value.Else)
			}
			return
		case *ast.ReturnStmt:
			count++
			var returned *weeklyErrorOrigins
			if len(value.Results) == signature.Results().Len() {
				returned, failure = resolver.expression(node, value.Results[resultIndex], 0)
			} else if len(value.Results) == 1 {
				returned, failure = resolver.expression(node, value.Results[0], resultIndex)
			} else {
				failure = fmt.Errorf("error producer %s has an unsupported return", node.key)
			}
			if failure == nil {
				failure = origins.merge(returned)
			}
			return
		}
		ast.Inspect(candidate, func(child ast.Node) bool {
			if child == candidate {
				return true
			}
			walk(child)
			return false
		})
	}
	walk(node.body)
	if failure != nil {
		return nil, failure
	}
	if count == 0 {
		return nil, fmt.Errorf("error producer %s has no proven return", node.key)
	}
	return origins, nil
}

func (resolver *weeklyErrorOriginResolver) concreteUnwrap(typ types.Type) (*weeklyTypedNode, error) {
	selection := types.NewMethodSet(typ).Lookup(nil, "Unwrap")
	if selection == nil || !weeklyErrorUnwrapMethod(selection.Obj()) {
		return nil, nil
	}
	function := selection.Obj().(*types.Func)
	base := typ
	if pointer, ok := base.(*types.Pointer); ok {
		base = pointer.Elem()
	}
	if named, ok := base.(*types.Named); ok && named.TypeArgs().Len() != 0 {
		return nil, fmt.Errorf("error origin %s has an unreviewed instantiated Unwrap body", weeklyErrorTypeKey(typ))
	}
	node := resolver.inventory.byFunction[function]
	if node == nil {
		return nil, fmt.Errorf("error origin %s has unindexed Unwrap body %s", weeklyErrorTypeKey(typ), function.FullName())
	}
	return node, nil
}

func (resolver *weeklyErrorOriginResolver) dispatchTargets(node *weeklyTypedNode, call *ast.CallExpr) ([]*weeklyTypedNode, error) {
	selector, ok := weeklyUnwrapCallExpression(call.Fun).(*ast.SelectorExpr)
	if !ok || len(call.Args) != 0 {
		return nil, errors.New("unsupported error dispatch shape")
	}
	origins, err := resolver.expression(node, selector.X, 0)
	if err != nil {
		return nil, err
	}
	if origins.nativeChain != nil {
		if resolver.contracts == nil {
			return nil, errors.New("standard error chain has no admitted SDK contract")
		}
		resolver.standardDispatch = true
	}
	var targets []*weeklyTypedNode
	seen := map[string]bool{}
	for _, name := range origins.names() {
		target, err := resolver.concreteUnwrap(origins.types[name])
		if err != nil {
			return nil, err
		}
		if target != nil && !seen[target.key] {
			targets = append(targets, target)
			seen[target.key] = true
		}
	}
	return targets, nil
}

// The reviewed loop shape has one initial error and one back-edge assignment
// from its own asserted Unwrap receiver. Every next concrete type is followed;
// reaching an unknown return never turns the chain into an accepted leaf.
func (resolver *weeklyErrorOriginResolver) loopOrigins(node *weeklyTypedNode, variable *types.Var, definition *ast.AssignStmt, loop *ast.ForStmt) (*weeklyErrorOrigins, error) {
	if len(definition.Lhs) != 1 || len(definition.Rhs) != 1 || loop.Post != nil {
		return nil, errors.New("unsupported error loop initialization")
	}
	condition, ok := weeklyUnwrapParentheses(loop.Cond).(*ast.BinaryExpr)
	if !ok || condition.Op != token.NEQ || weeklyExpressionObject(resolver.inventory, condition.X) != variable || weeklyExpressionObject(resolver.inventory, condition.Y) != types.Universe.Lookup("nil") {
		return nil, errors.New("error loop does not test its original variable against nil")
	}
	var backEdge *ast.AssignStmt
	for identifier, object := range resolver.inventory.info.Uses {
		if object != variable {
			continue
		}
		assignment, ok := resolver.index.parent[identifier].(*ast.AssignStmt)
		if !ok {
			continue
		}
		for _, lhs := range assignment.Lhs {
			if lhs != identifier {
				continue
			}
			if backEdge != nil {
				return nil, errors.New("error loop has multiple writes")
			}
			backEdge = assignment
		}
	}
	if backEdge == nil || len(backEdge.Lhs) != 1 || len(backEdge.Rhs) != 1 || backEdge.Pos() < loop.Body.Pos() || backEdge.End() > loop.Body.End() || !resolver.readOnly(node, variable, backEdge) {
		return nil, errors.New("error loop has no unique immutable unwrap back edge")
	}
	call, ok := weeklyUnwrapParentheses(backEdge.Rhs[0]).(*ast.CallExpr)
	if !ok {
		return nil, errors.New("error loop back edge is not a call")
	}
	_, selection := weeklyCallTarget(resolver.inventory, call)
	selector, ok := weeklyUnwrapCallExpression(call.Fun).(*ast.SelectorExpr)
	if !ok || !weeklyErrorUnwrapSelection(selection) {
		return nil, errors.New("error loop back edge is not an exact Unwrap selection")
	}
	receiver, ok := weeklyExpressionObject(resolver.inventory, selector.X).(*types.Var)
	if !ok || !resolver.readOnly(node, receiver, nil) {
		return nil, errors.New("error loop unwrap receiver is mutable or unknown")
	}
	receiverDefinition, receiverIndex, err := resolver.definition(receiver)
	if err != nil || receiverIndex != 0 || len(receiverDefinition.Rhs) != 1 {
		return nil, errors.New("error loop unwrap receiver has no exact assertion origin")
	}
	assertion, ok := weeklyUnwrapParentheses(receiverDefinition.Rhs[0]).(*ast.TypeAssertExpr)
	if !ok || weeklyExpressionObject(resolver.inventory, assertion.X) != variable {
		return nil, errors.New("error loop unwrap receiver does not originate from its loop variable")
	}
	origins := weeklyNewErrorOrigins()
	initial, err := resolver.expression(node, definition.Rhs[0], 0)
	if err != nil {
		return nil, err
	}
	if err := origins.merge(initial); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for {
		progress := false
		for _, name := range origins.names() {
			if seen[name] {
				continue
			}
			seen[name], progress = true, true
			target, err := resolver.concreteUnwrap(origins.types[name])
			if err != nil {
				return nil, err
			}
			if target == nil {
				continue
			}
			returned, err := resolver.returns(target, 0)
			if err != nil {
				return nil, err
			}
			if err := origins.merge(returned); err != nil {
				return nil, err
			}
		}
		if !progress {
			return origins, nil
		}
	}
}

// The standard-chain summary is admitted only for the reviewed Windows SDK.
// It eliminates fmt.wrapError links whose Error returns a stored string and
// whose Unwrap returns the already closed operand. Native leaves have no
// Unwrap, Format, GoString, or String method. This is deliberately narrower
// than a general fmt or imported-function exception.
type weeklyNativeErrorContracts struct {
	producers map[weeklyFunctionResultOrigin]*weeklyErrorOrigins
	leaves    []types.Type
	formatter *types.Func
}

func weeklyVerifyErrorSource(path, want string) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read reviewed error source %s: %w", path, err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(contents)); got != want {
		return fmt.Errorf("reviewed error source fingerprint changed for %s: got %s, want %s", path, got, want)
	}
	return nil
}

func weeklyImportedPackage(root *types.Package, path string) *types.Package {
	seen := map[*types.Package]bool{}
	var find func(*types.Package) *types.Package
	find = func(pkg *types.Package) *types.Package {
		if pkg == nil || seen[pkg] {
			return nil
		}
		seen[pkg] = true
		if pkg.Path() == path {
			return pkg
		}
		for _, child := range pkg.Imports() {
			if found := find(child); found != nil {
				return found
			}
		}
		return nil
	}
	return find(root)
}

func weeklyProductionNativeErrorContracts(t *testing.T, inventory weeklyTypedInventory) *weeklyNativeErrorContracts {
	t.Helper()
	if runtime.GOOS != "windows" {
		return nil
	}
	const windowsPath = "golang.org/x/sys/windows"
	module := inventory.packageModules[windowsPath]
	if runtime.Version() != "go1.27.4" || module == nil || module.Path != "golang.org/x/sys" || module.Version != "v0.47.0" || module.Replace != nil {
		t.Fatal("weekly error-origin proof requires reviewed Windows Go 1.27.4 and unreplaced golang.org/x/sys v0.47.0; a different SDK remains unqualified")
	}
	// Full-file fingerprints bind the producer, conversion, formatting, native
	// Error, and DLL helper bodies read in the installed-source capture. The
	// package directories come from this inventory's actual module-aware list.
	for _, source := range []struct{ pkg, file, sha256 string }{
		{"errors", "errors.go", "865b9cce008bf68d4d2cc4146455a9c5692756bbe57be29ee1ef0de345dafe1e"},
		{"fmt", "errors.go", "01fd304868493a3aa1198cc79a56274930f687353e9405211ca063c1b8195ea6"},
		{"fmt", "print.go", "10d7cd625d83a15f70b6e61641d3cebd4e4d49d7e0f2a24d5d2b59988074d2f7"},
		{"syscall", "syscall_windows.go", "d1d263b03c6296ca78344bf981242020a9ec344c23e558b80c03fbc4ad777620"},
		{"syscall", "zsyscall_windows.go", "111e00a4bf0cfad03076efb0067c014f349255015b455aa2f99709dd7c147c99"},
		{"syscall", "dll_windows.go", "fea84c96a3b8cbe83e7670fdf87db829de71e192d099d9e8ef1539dd9991a5ca"},
		{windowsPath, "syscall_windows.go", "589a6e28bca016f69eb25b75e82a808162d707de98d3d50367337303dcdd7018"},
		{windowsPath, "zsyscall_windows.go", "c40bdeba110bf46782feb55976a00c88c2ae51d8c812de5ae023326eeb083c92"},
		{windowsPath, "types_windows.go", "afa1ce9faf38958759a3cabca512e105bf937970e8ed411e6d8a7a9fa9de4558"},
		{windowsPath, "dll_windows.go", "1c62d9345dd0c1d8e660792c1955a407b2049210c3bd2d8000004375f07afed9"},
	} {
		directory := inventory.packageDirectories[source.pkg]
		if directory == "" {
			t.Fatalf("reviewed error source has no listed directory for %s", source.pkg)
		}
		if err := weeklyVerifyErrorSource(filepath.Join(directory, source.file), source.sha256); err != nil {
			t.Fatal(err)
		}
	}
	contracts, err := weeklyBindNativeErrorContracts(weeklyImportedPackage(inventory.pkg, windowsPath), weeklyImportedPackage(inventory.pkg, "syscall"), weeklyImportedPackage(inventory.pkg, "fmt"))
	if err != nil {
		t.Fatal(err)
	}
	return contracts
}

func weeklyBindNativeErrorContracts(windows, system, formatting *types.Package) (*weeklyNativeErrorContracts, error) {
	if windows == nil || windows.Path() != "golang.org/x/sys/windows" || system == nil || system.Path() != "syscall" || formatting == nil || formatting.Path() != "fmt" {
		return nil, errors.New("native error contracts require the exact reviewed package paths")
	}
	lookupType := func(pkg *types.Package, name string) types.Type {
		object, ok := pkg.Scope().Lookup(name).(*types.TypeName)
		if !ok || object.Pkg() != pkg {
			return nil
		}
		return object.Type()
	}
	errno, status := lookupType(system, "Errno"), lookupType(windows, "NTStatus")
	if errno == nil || status == nil || !types.Identical(errno.Underlying(), types.Typ[types.Uintptr]) || !types.Identical(status.Underlying(), types.Typ[types.Uint32]) {
		return nil, errors.New("native error contracts have a different Errno or NTStatus type")
	}
	for _, typ := range []types.Type{errno, status} {
		methods := types.NewMethodSet(typ)
		if !types.Implements(typ, types.Universe.Lookup("error").Type().Underlying().(*types.Interface)) {
			return nil, errors.New("native error leaf no longer implements error")
		}
		for _, name := range []string{"Unwrap", "Format", "GoString", "String"} {
			if methods.Lookup(nil, name) != nil {
				return nil, fmt.Errorf("native error leaf %s has unexpected %s method", weeklyErrorTypeKey(typ), name)
			}
		}
	}
	var required = map[string]types.Type{}
	for _, name := range []string{"Handle", "OBJECT_ATTRIBUTES", "IO_STATUS_BLOCK", "NTUnicodeString"} {
		required[name] = lookupType(windows, name)
		if required[name] == nil {
			return nil, fmt.Errorf("native producer is missing Windows type %s", name)
		}
	}
	contracts := &weeklyNativeErrorContracts{producers: map[weeklyFunctionResultOrigin]*weeklyErrorOrigins{}, leaves: []types.Type{errno, status}}
	u32, ptr := types.Typ[types.Uint32], types.NewPointer
	for _, producer := range []struct {
		name                string
		parameters, results []types.Type
		slot                int
		leaf                types.Type
	}{
		{"UTF16FromString", []types.Type{types.Typ[types.String]}, []types.Type{types.NewSlice(types.Typ[types.Uint16]), types.Universe.Lookup("error").Type()}, 1, errno},
		{"NewNTUnicodeString", []types.Type{types.Typ[types.String]}, []types.Type{ptr(required["NTUnicodeString"]), types.Universe.Lookup("error").Type()}, 1, errno},
		{"NtCreateFile", []types.Type{ptr(required["Handle"]), u32, ptr(required["OBJECT_ATTRIBUTES"]), ptr(required["IO_STATUS_BLOCK"]), ptr(types.Typ[types.Int64]), u32, u32, u32, u32, types.Typ[types.Uintptr], u32}, []types.Type{types.Universe.Lookup("error").Type()}, 0, status},
		{"NtSetInformationFile", []types.Type{required["Handle"], ptr(required["IO_STATUS_BLOCK"]), ptr(types.Typ[types.Uint8]), u32, u32}, []types.Type{types.Universe.Lookup("error").Type()}, 0, status},
	} {
		function, ok := windows.Scope().Lookup(producer.name).(*types.Func)
		if !ok || function.Pkg() != windows {
			return nil, fmt.Errorf("missing exact native producer %s", producer.name)
		}
		signature := function.Type().(*types.Signature)
		matches := signature.Recv() == nil && !signature.Variadic() && signature.TypeParams().Len() == 0 && signature.Params().Len() == len(producer.parameters) && signature.Results().Len() == len(producer.results)
		if matches {
			for i, typ := range producer.parameters {
				matches = matches && types.Identical(signature.Params().At(i).Type(), typ)
			}
			for i, typ := range producer.results {
				matches = matches && types.Identical(signature.Results().At(i).Type(), typ)
			}
		}
		if !matches {
			return nil, fmt.Errorf("native producer %s changed its reviewed signature", producer.name)
		}
		origin := weeklyNewErrorOrigins()
		origin.includesNil = true
		origin.types[weeklyErrorTypeKey(producer.leaf)] = producer.leaf
		contracts.producers[weeklyFunctionResultOrigin{factory: function, resultIndex: producer.slot}] = origin
	}
	formatter, ok := formatting.Scope().Lookup("Errorf").(*types.Func)
	if !ok || formatter.Pkg() != formatting {
		return nil, errors.New("missing exact fmt.Errorf producer")
	}
	signature := formatter.Type().(*types.Signature)
	if signature.Recv() != nil || !signature.Variadic() || signature.Params().Len() != 2 || !types.Identical(signature.Params().At(0).Type(), types.Typ[types.String]) || !types.Identical(signature.Params().At(1).Type(), types.NewSlice(types.Universe.Lookup("any").Type())) || signature.Results().Len() != 1 || !weeklyIsErrorType(signature.Results().At(0).Type()) {
		return nil, errors.New("fmt.Errorf changed its reviewed signature")
	}
	contracts.formatter = formatter
	return contracts, nil
}

func weeklyNativeChainAssertion(asserted *types.Interface) bool {
	for i := 0; i < asserted.NumMethods(); i++ {
		method := asserted.Method(i)
		if weeklyErrorUnwrapMethod(method) {
			continue
		}
		signature := method.Type().(*types.Signature)
		if method.Name() != "Error" || signature.Params().Len() != 0 || signature.Results().Len() != 1 || !types.Identical(signature.Results().At(0).Type(), types.Typ[types.String]) {
			return false
		}
	}
	return true
}

func (resolver *weeklyErrorOriginResolver) nativeFormattedError(node *weeklyTypedNode, call *ast.CallExpr) (*weeklyErrorOrigins, error) {
	if call.Ellipsis != token.NoPos || len(call.Args) == 0 {
		return nil, errors.New("fmt error origin has an unknown operand list")
	}
	format := resolver.inventory.info.Types[call.Args[0]].Value
	if format == nil || format.Kind() != constant.String {
		return nil, errors.New("fmt error origin has an unknown format")
	}
	text := constant.StringVal(format)
	argument, wraps := 1, 0
	leaves := map[string]types.Type{}
	for i := 0; i < len(text); i++ {
		if text[i] != '%' {
			continue
		}
		i++
		if i == len(text) {
			return nil, errors.New("fmt error origin has an incomplete format")
		}
		if text[i] == '%' {
			continue
		}
		if argument >= len(call.Args) {
			return nil, errors.New("fmt error origin has a missing operand")
		}
		operand := call.Args[argument]
		argument++
		switch text[i] {
		case 'q', 's':
			typ := resolver.inventory.info.TypeOf(operand)
			if !types.Identical(typ, types.Typ[types.String]) && !types.Identical(typ, types.Typ[types.UntypedString]) {
				return nil, errors.New("fmt string operand may invoke an unreviewed formatting method")
			}
		case 'w':
			wraps++
			origins, err := resolver.expression(node, operand, 0)
			if err != nil {
				return nil, err
			}
			for _, typ := range origins.types {
				known := false
				for _, leaf := range resolver.contracts.leaves {
					known = known || types.Identical(typ, leaf)
				}
				if !known {
					return nil, fmt.Errorf("fmt error operand %s has unreviewed Error or formatting behavior", weeklyErrorTypeKey(typ))
				}
				leaves[weeklyErrorTypeKey(typ)] = typ
			}
			if origins.includesNil {
				leaves["nil"] = nil
			}
			for name, typ := range origins.nativeChain {
				leaves[name] = typ
			}
		default:
			return nil, errors.New("fmt error origin uses an unreviewed verb, flag, width, precision, or index")
		}
	}
	if argument != len(call.Args) || wraps != 1 {
		return nil, errors.New("fmt error origin requires its exact operands and one wrapping verb")
	}
	// The checked formatter may store nil for a nil error operand. Both the
	// stored-message Error and subsequent Unwrap remain within this summary.
	origins := weeklyNewErrorOrigins()
	origins.nativeChain = leaves
	return origins, nil
}

func weeklyNativeErrorFixturePackages(t *testing.T) weeklyErrorFixtureImporter {
	t.Helper()
	packages := weeklyErrorFixtureImporter{}
	// These small packages supply type identities for model regressions. Their
	// bodies are fixture data; they do not qualify an installed SDK or product.
	for _, source := range []struct{ path, text string }{
		{"syscall", `package syscall
type Errno uintptr
func (Errno) Error() string { return "native" }
`},
		{"fmt", `package fmt
type State interface { Write([]byte) (int, error); Width() (int, bool); Precision() (int, bool); Flag(int) bool }
func Errorf(format string, a ...any) error { return nil }
`},
		{"golang.org/x/sys/windows", `package windows
type NTStatus uint32
func (NTStatus) Error() string { return "status" }
type Handle uintptr
type OBJECT_ATTRIBUTES struct{}
type IO_STATUS_BLOCK struct{}
type NTUnicodeString struct{}
func UTF16FromString(s string) ([]uint16, error) { return nil, nil }
func NewNTUnicodeString(s string) (*NTUnicodeString, error) { return nil, nil }
func NtCreateFile(handle *Handle, access uint32, oa *OBJECT_ATTRIBUTES, iosb *IO_STATUS_BLOCK, allocationSize *int64, attributes uint32, share uint32, disposition uint32, options uint32, eabuffer uintptr, ealength uint32) error { return nil }
func NtSetInformationFile(handle Handle, iosb *IO_STATUS_BLOCK, inBuffer *byte, inBufferLen uint32, class uint32) error { return nil }
`},
	} {
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, "native_error_package_fixture.go", source.text, 0)
		if err != nil {
			t.Fatal(err)
		}
		pkg, err := (&types.Config{}).Check(source.path, fileSet, []*ast.File{file}, nil)
		if err != nil {
			t.Fatal(err)
		}
		packages[source.path] = pkg
	}
	return packages
}

func TestWeeklyTypedNativeErrorContracts(t *testing.T) {
	t.Run("changed installed source fingerprint refuses admission", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "reviewed.go")
		original := []byte("package reviewed\n")
		want := fmt.Sprintf("%x", sha256.Sum256(original))
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Fatal(err)
		}
		if err := weeklyVerifyErrorSource(path, want); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package changed\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := weeklyVerifyErrorSource(path, want); err == nil || !strings.Contains(err.Error(), "fingerprint changed") {
			t.Fatalf("changed source admission = %v", err)
		}
	})
	t.Run("same package name at another path is not the producer", func(t *testing.T) {
		packages := weeklyNativeErrorFixturePackages(t)
		_, err := weeklyBindNativeErrorContracts(types.NewPackage("replacement/windows", "windows"), packages["syscall"], packages["fmt"])
		if err == nil || !strings.Contains(err.Error(), "exact reviewed package paths") {
			t.Fatalf("different package admission = %v", err)
		}
	})
	t.Run("same error name with another underlying type is not native", func(t *testing.T) {
		packages := weeklyNativeErrorFixturePackages(t)
		fileSet := token.NewFileSet()
		file, err := parser.ParseFile(fileSet, "changed_errno.go", `package syscall; type Errno string; func (Errno) Error() string { return "changed" }`, 0)
		if err != nil {
			t.Fatal(err)
		}
		changed, err := (&types.Config{}).Check("syscall", fileSet, []*ast.File{file}, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = weeklyBindNativeErrorContracts(packages["golang.org/x/sys/windows"], changed, packages["fmt"])
		if err == nil || !strings.Contains(err.Error(), "different Errno or NTStatus type") {
			t.Fatalf("different type admission = %v", err)
		}
	})
}

func TestWeeklyTypedNativeErrorFormatting(t *testing.T) {
	for _, test := range []struct{ name, source, wantReason, wantLeaf string }{
		{
			name:     "nested standard wrappers retain the conversion result origin",
			source:   `func root() { _, err := windows.UTF16FromString("name"); classify(fmt.Errorf("outer %q: %w", "path", fmt.Errorf("inner %w", err))) }`,
			wantLeaf: "syscall.Errno",
		},
		{
			name:     "native status result uses its own result slot and type",
			source:   `func root() { err := windows.NtSetInformationFile(0, nil, nil, 0, 0); classify(fmt.Errorf("status %w", err)) }`,
			wantLeaf: "golang.org/x/sys/windows.NTStatus",
		},
		{
			name:       "extra operand cannot hide a formatting callback",
			source:     `func root() { classify(fmt.Errorf("error %w", syscall.Errno(1), syscall.Errno(2))) }`,
			wantReason: "exact operands and one wrapping verb",
		},
		{
			name:       "unknown format is not a reviewed single wrapper",
			source:     `func root(format string) { classify(fmt.Errorf(format, syscall.Errno(1))) }`,
			wantReason: "unknown format",
		},
		{
			name:       "unknown inner error cannot inherit an outer native proof",
			source:     `func root(err error) { classify(fmt.Errorf("outer %w", fmt.Errorf("inner %w", err))) }`,
			wantReason: "no exact local := origin",
		},
		{
			name:       "user Error method with settings acquisition is refused",
			source:     `type custom struct{}; func (custom) Error() string { hidden(); return "custom" }; func root() { classify(fmt.Errorf("error %w", custom{})) }`,
			wantReason: "unreviewed Error or formatting behavior",
		},
		{
			name:       "user Formatter with settings acquisition is refused",
			source:     `type custom struct{}; func (custom) Error() string { return "custom" }; func (custom) Format(fmt.State, rune) { hidden() }; func root() { classify(fmt.Errorf("error %w", custom{})) }`,
			wantReason: "unreviewed Error or formatting behavior",
		},
		{
			name:       "named string operand cannot invoke its String method",
			source:     `type custom string; func (custom) String() string { hidden(); return "custom" }; func root() { classify(fmt.Errorf("%s: %w", custom("name"), syscall.Errno(1))) }`,
			wantReason: "string operand may invoke an unreviewed formatting method",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			packages := weeklyNativeErrorFixturePackages(t)
			contracts, err := weeklyBindNativeErrorContracts(packages["golang.org/x/sys/windows"], packages["syscall"], packages["fmt"])
			if err != nil {
				t.Fatal(err)
			}
			// Blank references keep all three exact package identities available
			// without adding any extra call or error origin to the fixture.
			prelude := strings.Replace(weeklyOriginFixturePrelude, "package fixture", "package fixture\nimport (\"fmt\"; \"syscall\"; \"golang.org/x/sys/windows\")\nvar _ = syscall.Errno(0)\nvar _ = windows.NTStatus(0)", 1)
			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, "native_error_fixture.go", prelude+weeklyErrorOriginFixture+test.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			info := weeklyNewTypesInfo()
			pkg, err := (&types.Config{Importer: packages}).Check("fixture", fileSet, []*ast.File{file}, info)
			if err != nil {
				t.Fatal(err)
			}
			inventory := weeklyTypedInventoryFromFiles(t, fileSet, pkg, info, []*ast.File{file})
			classify := weeklyRequiredPackageFunc(t, inventory, "classify")
			graph, err := weeklyBuildTypedCallGraph(&inventory, weeklyTypedGraphOptions{contextParameters: map[*types.Func][]int{classify: {0}}, errorContracts: contracts})
			if err != nil {
				t.Fatal(err)
			}
			root := weeklyRequiredPackageFunc(t, inventory, "root")
			reachable, err := weeklyReachableTypedNodes(inventory, graph, root)
			if test.wantReason != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantReason) {
					t.Fatalf("native format origin = %v, want %q", err, test.wantReason)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			proved := false
			for key := range reachable {
				if len(graph.standardErrorChainDispatches[key]) == 0 {
					continue
				}
				for _, binding := range inventory.nodes[key].bindings {
					if binding.kind == "error" && binding.errorOrigins.nativeChain[test.wantLeaf] != nil {
						proved = true
					}
				}
			}
			if !proved {
				t.Fatalf("standard unwrap summary lost terminal provenance %s", test.wantLeaf)
			}
			closure := weeklySettingsHelperClosure(inventory, graph, weeklyRequiredPackageVar(t, inventory, "settingsMu"))
			if closure.nodes[inventory.byFunction[root].key] {
				t.Fatal("native fixture acquired settings mutex")
			}
		})
	}
}

const weeklyOriginFixturePrelude = `package fixture
type fixtureMutex struct{}
func (fixtureMutex) Lock() {}
var settingsMu fixtureMutex
func hidden() { settingsMu.Lock() }
type resource struct { close func() }
`

func weeklyRequireOriginFixture(t *testing.T, source string, contextNames []string, wantUnresolved, wantSettings bool) {
	t.Helper()
	inventory := weeklyTypedFixtureInventory(t, weeklyOriginFixturePrelude+source)
	options := weeklyTypedGraphOptions{contextParameters: map[*types.Func][]int{}}
	for _, name := range contextNames {
		function := weeklyRequiredPackageFunc(t, inventory, name)
		signature := function.Type().(*types.Signature)
		for i := 0; i < signature.Params().Len(); i++ {
			options.contextParameters[function] = append(options.contextParameters[function], i)
		}
	}
	graph, err := weeklyBuildTypedCallGraph(&inventory, options)
	if err != nil {
		t.Fatalf("build origin fixture: %v", err)
	}
	root := weeklyRequiredPackageFunc(t, inventory, "root")
	_, err = weeklyReachableTypedNodes(inventory, graph, root)
	if wantUnresolved {
		if err == nil || !strings.Contains(err.Error(), "unresolved typed boundary") {
			t.Fatalf("origin fixture reachability = %v, want unresolved typed boundary", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("resolve origin fixture: %v", err)
	}
	closure := weeklySettingsHelperClosure(inventory, graph, weeklyRequiredPackageVar(t, inventory, "settingsMu"))
	if got := closure.nodes[inventory.byFunction[root].key]; got != wantSettings {
		t.Fatalf("settings acquisition reachable = %v, want %v; chain %s", got, wantSettings, strings.Join(closure.chain(inventory.byFunction[root].key), " -> "))
	}
}

func TestWeeklyTypedLocalLiteralOrigins(t *testing.T) {
	tests := []struct {
		name           string
		source         string
		wantUnresolved bool
		wantSettings   bool
	}{
		{
			name: "four direct cleanup calls retain one literal body",
			source: `func root(flag bool) {
	cleanup := func() {}
	if flag { cleanup() }
	if !flag { cleanup() }
	defer cleanup()
	cleanup()
}`,
		},
		{
			name: "direct settings lock in cleanup is visible",
			source: `func root() {
	cleanup := func() { settingsMu.Lock() }
	cleanup()
	cleanup()
}`,
			wantSettings: true,
		},
		{
			name: "transitive settings lock in cleanup is visible",
			source: `func root() {
	cleanup := func() { hidden() }
	defer cleanup()
}`,
			wantSettings: true,
		},
		{
			name: "reassigned literal fails closed",
			source: `func root() {
	cleanup := func() {}
	cleanup = hidden
	cleanup()
}`,
			wantUnresolved: true,
		},
		{
			name: "literal alias fails closed",
			source: `func root() {
	cleanup := func() {}
	alias := cleanup
	alias()
	cleanup()
}`,
			wantUnresolved: true,
		},
		{
			name: "address escape fails closed",
			source: `func replace(p *func()) { *p = hidden }
func root() {
	cleanup := func() {}
	replace(&cleanup)
	cleanup()
}`,
			wantUnresolved: true,
		},
		{
			name: "captured callable fails closed",
			source: `func root() {
	cleanup := func() { hidden() }
	func() { cleanup() }()
}`,
			wantUnresolved: true,
		},
		{
			name: "conditional definition fails closed",
			source: `func root(flag bool) {
	var cleanup func()
	if flag { cleanup = func() {} } else { cleanup = hidden }
	cleanup()
}`,
			wantUnresolved: true,
		},
		{
			name: "goto prevents a dominance assumption",
			source: `func root(flag bool) {
	cleanup := func() { hidden() }
again:
	cleanup()
	if flag { goto again }
}`,
			wantUnresolved: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			weeklyRequireOriginFixture(t, test.source, nil, test.wantUnresolved, test.wantSettings)
		})
	}
}

func TestWeeklyTypedContextOrigins(t *testing.T) {
	tests := []struct {
		name           string
		source         string
		functions      []string
		wantUnresolved bool
		wantSettings   bool
	}{
		{
			name: "nil error-returning step propagates through reader policy",
			source: `type stepFunc func() error
func invoke(step stepFunc) error { if step == nil { return nil }; return step() }
func policy(step stepFunc) error { return invoke(step) }
func reader(step stepFunc) error { return policy(step) }
func root() { _ = reader(nil) }`,
			functions: []string{"reader", "policy", "invoke"},
		},
		{
			name: "error-returning step retains its settings acquisition",
			source: `type stepFunc func() error
func invoke(step stepFunc) error { if step == nil { return nil }; return step() }
func policy(step stepFunc) error { return invoke(step) }
func observer() error { hidden(); return nil }
func root() { _ = policy(observer) }`,
			functions:    []string{"policy", "invoke"},
			wantSettings: true,
		},
		{
			name: "unknown error-returning step cannot inherit nil proof",
			source: `type stepFunc func() error
func invoke(step stepFunc) error { if step == nil { return nil }; return step() }
func policy(step stepFunc) error { return invoke(step) }
func root(step stepFunc) { _ = policy(nil); _ = policy(step) }`,
			functions:      []string{"policy", "invoke"},
			wantUnresolved: true,
		},
		{
			name: "nil and disabled audit propagate through reader chain",
			source: `func reader(b *resource, audit bool, callback func()) {
	if b != nil { b.close() }
	if audit && callback != nil { callback() }
}
func forward(b *resource, audit bool, callback func()) { reader(b, audit, callback) }
func root() { forward(nil, false, nil) }`,
			functions: []string{"forward", "reader"},
		},
		{
			name: "named formatter retains transitive settings edge",
			source: `func formatter(callback func()) { callback() }
func root() { formatter(hidden) }`,
			functions:    []string{"formatter"},
			wantSettings: true,
		},
		{
			name: "enabled audit retains callback body",
			source: `func reader(audit bool, callback func()) {
	if audit && callback != nil { callback() }
}
func root() { reader(true, hidden) }`,
			functions:    []string{"reader"},
			wantSettings: true,
		},
		{
			name: "disabled audit excludes only unreachable callback",
			source: `func reader(audit bool, callback func()) {
	if audit && callback != nil { callback() }
}
func root() { reader(false, hidden) }`,
			functions: []string{"reader"},
		},
		{
			name: "settings lock in nil branch remains visible",
			source: `func reader(b *resource) {
	if b == nil { hidden(); return }
	b.close()
}
func root() { reader(nil) }`,
			functions:    []string{"reader"},
			wantSettings: true,
		},
		{
			name: "deferred closure preserves immutable nil capture",
			source: `func reader(b *resource) {
	defer func() {
		if b == nil { hidden(); return }
		b.close()
	}()
}
func root() { reader(nil) }`,
			functions:    []string{"reader"},
			wantSettings: true,
		},
		{
			name: "unreviewed forwarding helper stays unresolved",
			source: `func reader(callback func()) { callback() }
func root() { reader(hidden) }`,
			wantUnresolved: true,
		},
		{
			name: "unguarded nil invocation fails closed",
			source: `func reader(callback func()) { callback() }
func root() { reader(nil) }`,
			functions:      []string{"reader"},
			wantUnresolved: true,
		},
		{
			name: "callback reassignment discards binding",
			source: `func reader(callback func()) { callback = hidden; callback() }
func root() { reader(nil) }`,
			functions:      []string{"reader"},
			wantUnresolved: true,
		},
		{
			name: "callback alias discards binding",
			source: `func reader(callback func()) { alias := callback; alias() }
func root() { reader(hidden) }`,
			functions:      []string{"reader"},
			wantUnresolved: true,
		},
		{
			name: "callback address escape discards binding",
			source: `func replace(p *func()) { *p = hidden }
func reader(callback func()) { replace(&callback); callback() }
func root() { reader(nil) }`,
			functions:      []string{"reader"},
			wantUnresolved: true,
		},
		{
			name: "callback field storage remains unresolved",
			source: `func reader(callback func()) { r := resource{close: callback}; r.close() }
func root() { reader(hidden) }`,
			functions:      []string{"reader"},
			wantUnresolved: true,
		},
		{
			name: "unknown input remains unresolved",
			source: `func reader(callback func()) { callback() }
func root(callback func()) { reader(callback) }`,
			functions:      []string{"reader"},
			wantUnresolved: true,
		},
		{
			name: "known caller does not erase unknown caller",
			source: `func reader(callback func()) { callback() }
func root(callback func()) { reader(hidden); reader(callback) }`,
			functions:      []string{"reader"},
			wantUnresolved: true,
		},
		{
			name: "nil context does not erase resource context",
			source: `func reader(b *resource) { if b != nil { b.close() } }
func root(b *resource) { reader(nil); reader(b) }`,
			functions:      []string{"reader"},
			wantUnresolved: true,
		},
		{
			name: "typed nil pointer is not nil interface",
			source: `func inspect(value interface{}) { if value != nil { hidden() } }
func forward(b *resource) { inspect(b) }
func root() { forward(nil) }`,
			functions:      []string{"forward", "inspect"},
			wantUnresolved: true,
		},
		{
			name: "nested write prevents captured nil proof",
			source: `func reader(b *resource) {
	func() { b = &resource{close: hidden} }()
	if b != nil { b.close() }
}
func root() { reader(nil) }`,
			functions:      []string{"reader"},
			wantUnresolved: true,
		},
		{
			name: "named function alias is not exact origin",
			source: `func reader(callback func()) { callback() }
func root() { alias := hidden; reader(alias) }`,
			functions:      []string{"reader"},
			wantUnresolved: true,
		},
		{
			name: "recursive mixed origin stays unresolved",
			source: `func reader(callback func()) {
	var b resource
	reader(b.close)
	callback()
}
func root() { reader(hidden) }`,
			functions:      []string{"reader"},
			wantUnresolved: true,
		},
		{
			name: "short circuit retains condition call side effects",
			source: `func effect() bool { hidden(); return true }
func reader(b *resource) { if effect() && b != nil { b.close() } }
func root() { reader(nil) }`,
			functions:    []string{"reader"},
			wantSettings: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			weeklyRequireOriginFixture(t, test.source, test.functions, test.wantUnresolved, test.wantSettings)
		})
	}
	t.Run("context budget fails closed", func(t *testing.T) {
		var source strings.Builder
		source.WriteString("func reader(a, b, c, d, e bool) { if a && b && c && d && e { hidden() } }\nfunc root() {\n")
		for bits := 0; bits < 17; bits++ {
			fmt.Fprintf(&source, "reader(%t, %t, %t, %t, %t)\n", bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0, bits&16 != 0)
		}
		source.WriteString("}\n")
		weeklyRequireOriginFixture(t, source.String(), []string{"reader"}, true, false)
	})
}

const weeklyErrorOriginFixture = `
type nativeError int
func (nativeError) Error() string { return "native" }
func classify(err error) {
	for cur := err; cur != nil; {
		if u, ok := cur.(interface { Unwrap() error }); ok {
			cur = u.Unwrap()
			continue
		}
		return
	}
}
`

func TestWeeklyTypedErrorOrigins(t *testing.T) {
	tests := []struct {
		name           string
		source         string
		functions      []string
		wantUnresolved bool
		wantSettings   bool
	}{
		{
			name:   "concrete native leaf has no unwrap target",
			source: `func root() { classify(nativeError(1)) }`,
		},
		{
			name:   "nil remains an empty dynamic target set",
			source: `func root() { classify(nil) }`,
		},
		{
			name: "known local return and forwarding preserve origin",
			source: `func produce() error { return nativeError(1) }
func forward(err error) error { return err }
func root() { classify(forward(produce())) }`,
		},
		{
			name: "value unwrap retains transitive settings acquisition",
			source: `type wrapped struct{}
func (wrapped) Error() string { return "wrapped" }
func (wrapped) Unwrap() error { hidden(); return nil }
func root() { classify(wrapped{}) }`,
			wantSettings: true,
		},
		{
			name: "pointer unwrap retains direct settings acquisition",
			source: `type wrapped struct{}
func (*wrapped) Error() string { return "wrapped" }
func (*wrapped) Unwrap() error { settingsMu.Lock(); return nil }
func root() { classify(&wrapped{}) }`,
			wantSettings: true,
		},
		{
			name: "typed nil error retains its concrete unwrap method",
			source: `type wrapped struct{}
func (*wrapped) Error() string { return "wrapped" }
func (*wrapped) Unwrap() error { hidden(); return nil }
func root() { classify((*wrapped)(nil)) }`,
			wantSettings: true,
		},
		{
			name: "concrete pointer conversion cannot preserve the previous method set",
			source: `type before struct{}
func (*before) Error() string { return "before" }
type after struct{}
func (*after) Error() string { return "after" }
func (*after) Unwrap() error { hidden(); return nil }
func root() { classify((*after)(&before{})) }`,
			wantUnresolved: true,
		},
		{
			name: "promoted unwrap retains the original method body",
			source: `type wrapped struct{}
func (*wrapped) Error() string { return "wrapped" }
func (*wrapped) Unwrap() error { hidden(); return nil }
type outer struct { *wrapped }
func root() { classify(outer{&wrapped{}}) }`,
			wantSettings: true,
		},
		{
			name: "second unwrap generation retains settings acquisition",
			source: `type outer struct{}
func (outer) Error() string { return "outer" }
func (outer) Unwrap() error { return inner{} }
type inner struct{}
func (inner) Error() string { return "inner" }
func (inner) Unwrap() error { hidden(); return nativeError(2) }
func root() { classify(outer{}) }`,
			wantSettings: true,
		},
		{
			name: "tuple result selects the error slot",
			source: `type wrapped struct{}
func (wrapped) Error() string { return "wrapped" }
func (wrapped) Unwrap() error { hidden(); return nil }
func produce() (int, error) { return 1, wrapped{} }
func root() { _, err := produce(); classify(err) }`,
			wantSettings: true,
		},
		{
			name: "all possible local returns contribute their methods",
			source: `type wrapped struct{}
func (wrapped) Error() string { return "wrapped" }
func (wrapped) Unwrap() error { hidden(); return nil }
func produce(flag bool) error { if flag { return wrapped{} }; return nativeError(1) }
func root(flag bool) { classify(produce(flag)) }`,
			wantSettings: true,
		},
		{
			name: "nil union does not prune a nonnil settings branch",
			source: `func produce(flag bool) error { if flag { return nil }; return nativeError(1) }
func consume(err error) { if err != nil { hidden() } }
func root(flag bool) { consume(produce(flag)) }`,
			functions:    []string{"consume"},
			wantSettings: true,
		},
		{
			name: "unrelated local unwrap is not a possible dynamic target",
			source: `type unrelated struct{}
func (unrelated) Error() string { return "unrelated" }
func (unrelated) Unwrap() error { hidden(); return nil }
func root() { classify(nativeError(1)) }`,
		},
		{
			name:           "unknown caller parameter fails closed",
			source:         `func root(err error) { classify(err) }`,
			wantUnresolved: true,
		},
		{
			name:           "known caller cannot hide a second unknown context",
			source:         `func root(err error) { classify(nativeError(1)); classify(err) }`,
			wantUnresolved: true,
		},
		{
			name: "mutable package callback result stays unknown",
			source: `var callback func() error
func root() { classify(callback()) }`,
			wantUnresolved: true,
		},
		{
			name: "unknown callback inside a returned chain fails closed",
			source: `var callback func() error
type wrapped struct{}
func (wrapped) Error() string { return "wrapped" }
func (wrapped) Unwrap() error { return callback() }
func root() { classify(wrapped{}) }`,
			wantUnresolved: true,
		},
		{
			name:   "later assignment cannot change an already copied error argument",
			source: `func root(unknown error) { err := error(nativeError(1)); classify(err); err = unknown; _ = err }`,
		},
		{
			name:           "later assignment across a loop back edge remains unknown",
			source:         `func root(unknown error) { err := error(nativeError(1)); for { classify(err); err = unknown } }`,
			wantUnresolved: true,
		},
		{
			name:           "reassigned argument loses its original proof",
			source:         `func root(unknown error) { err := error(nativeError(1)); err = unknown; classify(err) }`,
			wantUnresolved: true,
		},
		{
			name:           "captured argument cannot be certified from initializer",
			source:         `func root(unknown error) { err := error(nativeError(1)); func() { err = unknown }(); classify(err) }`,
			wantUnresolved: true,
		},
		{
			name: "address escape prevents an immutable origin proof",
			source: `func change(err *error) { *err = nativeError(2) }
func root() { err := error(nativeError(1)); change(&err); classify(err) }`,
			wantUnresolved: true,
		},
		{
			name: "unknown wrapper field remains unknown after unwrap",
			source: `type wrapped struct { next error }
func (wrapped) Error() string { return "wrapped" }
func (w wrapped) Unwrap() error { return w.next }
func root(err error) { classify(wrapped{err}) }`,
			wantUnresolved: true,
		},
		{
			name: "generic method requires its own body correspondence",
			source: `type wrapped[T any] struct{}
func (wrapped[T]) Error() string { return "wrapped" }
func (wrapped[T]) Unwrap() error { hidden(); return nil }
func root() { classify(wrapped[int]{}) }`,
			wantUnresolved: true,
		},
		{
			name: "named result can be changed by deferred code",
			source: `func produce() (err error) { defer func() { err = nativeError(2) }(); return nativeError(1) }
func root() { classify(produce()) }`,
			wantUnresolved: true,
		},
		{
			name: "recursive producer does not invent a terminating origin",
			source: `func produce(flag bool) error { if flag { return nativeError(1) }; return produce(flag) }
func root(flag bool) { classify(produce(flag)) }`,
			wantUnresolved: true,
		},
		{
			name: "producer goto cannot bypass return-origin discovery",
			source: `func produce(flag bool) error {
if flag { goto other }
return nativeError(1)
other: return nativeError(2)
}
func root(flag bool) { classify(produce(flag)) }`,
			wantUnresolved: true,
		},
		{
			name: "a second loop write cannot use the unwrap recurrence proof",
			source: `func changed(err error) {
for cur := err; cur != nil; {
if u, ok := cur.(interface { Unwrap() error }); ok { cur = u.Unwrap() }
cur = err
}
}
func root() { changed(nativeError(1)) }`,
			functions:      []string{"changed"},
			wantUnresolved: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			functions := append([]string{"classify"}, test.functions...)
			weeklyRequireOriginFixture(t, weeklyErrorOriginFixture+test.source, functions, test.wantUnresolved, test.wantSettings)
		})
	}
	t.Run("finite error type budget fails closed", func(t *testing.T) {
		var source strings.Builder
		for i := 0; i < 17; i++ {
			fmt.Fprintf(&source, "type error%d struct{}\nfunc (error%d) Error() string { return \"error\" }\n", i, i)
		}
		source.WriteString("func produce(which int) error { switch which {\n")
		for i := 0; i < 16; i++ {
			fmt.Fprintf(&source, "case %d: return error%d{}\n", i, i)
		}
		source.WriteString("default: return error16{}\n} }\nfunc root(which int) { classify(produce(which)) }\n")
		weeklyRequireOriginFixture(t, weeklyErrorOriginFixture+source.String(), []string{"classify"}, true, false)
	})
}

type weeklyErrorFixtureImporter map[string]*types.Package

func (imports weeklyErrorFixtureImporter) Import(path string) (*types.Package, error) {
	if pkg := imports[path]; pkg != nil {
		return pkg, nil
	}
	return nil, fmt.Errorf("unrecognized error-origin fixture import %s", path)
}

func TestWeeklyTypedImportedErrorOrigins(t *testing.T) {
	fileSet := token.NewFileSet()
	foreign, err := parser.ParseFile(fileSet, "foreign.go", `package foreign
type Wrapped struct{}
func (Wrapped) Error() string { return "wrapped" }
func (Wrapped) Unwrap() error { return nil }
func Source() error { return Wrapped{} }
func Wrap(err error) error { return err }
type NativeError int
func (NativeError) Error() string { return "native" }
func ConcreteSource(callback func()) NativeError { callback(); return 1 }
type Holder struct { Err NativeError }
func HolderSource(callback func()) Holder { callback(); return Holder{1} }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	foreignPackage, err := (&types.Config{}).Check("foreign", fileSet, []*ast.File{foreign}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, source, wantReason string }{
		{
			name:       "concrete imported result cannot hide a callback inline",
			source:     `func root() { classify(foreign.ConcreteSource(hidden)) }`,
			wantReason: "foreign.ConcreteSource has no indexed producer body",
		},
		{
			name:       "concrete local alias retains its imported producer",
			source:     `func root() { err := foreign.ConcreteSource(hidden); classify(err) }`,
			wantReason: "foreign.ConcreteSource has no indexed producer body",
		},
		{
			name:       "concrete field does not hide its opaque construction",
			source:     `func root() { classify(foreign.HolderSource(hidden).Err) }`,
			wantReason: "has no closed origin",
		},
		{
			name: "concrete literal does not ignore evaluated map keys",
			source: `type mappedError map[foreign.NativeError]int
func (mappedError) Error() string { return "mapped" }
func root() { classify(mappedError{foreign.ConcreteSource(hidden): 1}) }`,
			wantReason: "has no closed origin",
		},
		{
			name:       "imported interface result has no producer proof",
			source:     `func root() { classify(foreign.Source()) }`,
			wantReason: "foreign.Source has no indexed producer body",
		},
		{
			name:       "known imported type still requires its unwrap body",
			source:     `func root() { classify(foreign.Wrapped{}) }`,
			wantReason: "unindexed Unwrap body",
		},
		{
			name:       "unreviewed external wrapper does not erase input provenance",
			source:     `func root() { classify(foreign.Wrap(nativeError(1))) }`,
			wantReason: "foreign.Wrap has no indexed producer body",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			prelude := strings.Replace(weeklyOriginFixturePrelude, "package fixture", "package fixture\nimport \"foreign\"", 1)
			file, err := parser.ParseFile(fileSet, "imported_error_fixture.go", prelude+weeklyErrorOriginFixture+test.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			info := weeklyNewTypesInfo()
			pkg, err := (&types.Config{Importer: weeklyErrorFixtureImporter{"foreign": foreignPackage}}).Check("fixture", fileSet, []*ast.File{file}, info)
			if err != nil {
				t.Fatal(err)
			}
			inventory := weeklyTypedInventoryFromFiles(t, fileSet, pkg, info, []*ast.File{file})
			classify := weeklyRequiredPackageFunc(t, inventory, "classify")
			graph, err := weeklyBuildTypedCallGraph(&inventory, weeklyTypedGraphOptions{contextParameters: map[*types.Func][]int{classify: {0}}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = weeklyReachableTypedNodes(inventory, graph, weeklyRequiredPackageFunc(t, inventory, "root"))
			if err == nil || !strings.Contains(err.Error(), test.wantReason) {
				t.Fatalf("imported error origin = %v, want %q", err, test.wantReason)
			}
		})
	}
}

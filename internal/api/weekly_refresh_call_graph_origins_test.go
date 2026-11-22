package api

import (
	"errors"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
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
	kind     string
	boolean  bool
	function *types.Func
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

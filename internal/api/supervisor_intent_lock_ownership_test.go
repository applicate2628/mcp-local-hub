package api

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const flockImportPath = "github.com/gofrs/flock"

var reviewedRawFlockOwners = map[string]int{
	"internal/api/adopted_entries.go:func:withAdoptedEntriesLock":                                    1,
	"internal/api/daemon_env_overlay/write.go:func:WriteOverlay":                                     1,
	"internal/api/daemon_intent.go:func:readDaemonIntentPathWithTimeout":                             1,
	"internal/api/daemon_intent.go:method:*API.ClearDaemonIntent":                                    1,
	"internal/api/daemon_intent.go:method:*API.ReadDaemonIntent":                                     1,
	"internal/api/daemon_intent.go:method:*API.WriteDaemonIntent":                                    1,
	"internal/api/default_workspace_marker.go:func:clearDefaultWorkspaceIfMatchesWithMarkerLockHook": 1,
	"internal/api/gui_event_log.go:method:*API.AppendGUIEventLog":                                    1,
	"internal/api/hub_mcp_log.go:func:appendHubMcpLogLine":                                           1,
	"internal/api/hub_mcp_state.go:func:acquireHubMcpLock":                                           1,
	"internal/api/hub_mcp_state.go:func:acquireHubMcpLockContext":                                    1,
	"internal/api/intent_audit.go:method:*API.AppendIntentAudit":                                     1,
	"internal/api/intent_collapse.go:func:runDaemonIntentCollapse":                                   1,
	"internal/api/lock_release_ledger.go:func:lockLeafLedgeredWithUnlock":                            1,
	"internal/api/lock_release_ledger.go:func:tryLockLeafLedgeredWithUnlock":                         1,
	"internal/api/lsp_trusted_roots.go:func:BlessTrustedRootDetailed":                                1,
	"internal/api/lsp_trusted_roots.go:func:RemoveTrustedRootDetailed":                               1,
	"internal/api/managed_entries.go:func:withManagedEntriesLock":                                    1,
	"internal/api/mcp_front_routing_target.go:func:withMCPFrontRoutingFileLease":                     1,
	"internal/api/state_file_helper.go:func:WriteStateFileBytesAtomic":                               1,
	"internal/api/supervisor_events.go:func:OpenSupervisorEventLog":                                  1,
	"internal/api/supervisor_lock.go:func:AcquireSupervisorLock":                                     1,
	"internal/api/supervisor_lock.go:func:AcquireSupervisorLockQuiet":                                1,
	"internal/api/supervisor_lock.go:func:SupervisorRunningUnderStateDir/func-lit#1":                 1,
	"internal/api/supervisor_state.go:func:MutateSupervisorStateIfChangedContext":                    1,
	"internal/api/supervisor_state.go:func:WithEmptyStopSettlementFence":                              1,
}

var reviewedAppliedReleaseClassifierOwners = map[string]int{
	"internal/api/lock_release_ledger.go:func:releaseAndJoinApplied": 1,
}

func TestSupervisorIntentLockOwnershipProductionInventory(t *testing.T) {
	repoRoot := filepath.Clean(filepath.Join(packageDirForOwnershipTest(t), "..", ".."))
	got, err := rawFlockOwnerInventory(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := compareOwnerInventory(got, reviewedRawFlockOwners); err != nil {
		t.Fatal(err)
	}

	classifiers, err := appliedReleaseClassifierInventory(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := compareOwnerInventory(classifiers, reviewedAppliedReleaseClassifierOwners); err != nil {
		t.Fatal(err)
	}

	refs, err := productionSupervisorLockSuffixReferences(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"internal/api/supervisor_intent_lock.go"}
	if strings.Join(refs, ",") != strings.Join(want, ",") {
		t.Fatalf("supervisorIntentLockSuffix production owners=%v, want %v", refs, want)
	}
}

func TestSupervisorIntentLockOwnershipFixtureMatrix(t *testing.T) {
	tests := []struct {
		name            string
		source          string
		allow           map[string]int
		wantErrContains string
	}{
		{
			name:   "typed supervisor caller needs no census update",
			source: "package api\nfunc typed(path string) { _, _ = lockSupervisorIntent(path) }\n",
			allow:  map[string]int{},
		},
		{
			name:   "comments strings and local shadow are not target calls",
			source: "package api\nimport \"github.com/gofrs/flock\"\n// flock.New(path)\nvar note = `flock.New(path)`\ntype localFlock struct{}\nfunc (localFlock) New(string){}\nfunc shadow(path string) { flock := localFlock{}; flock.New(path) }\n",
			allow:  map[string]int{},
		},
		{
			name:            "normal target alias",
			source:          "package api\nimport \"github.com/gofrs/flock\"\nfunc bypass(path string) { _ = flock.New(path) }\n",
			allow:           map[string]int{},
			wantErrContains: "func:bypass",
		},
		{
			name:            "explicit target alias",
			source:          "package api\nimport f \"github.com/gofrs/flock\"\nfunc bypass(path string) { _ = f.New(path) }\n",
			allow:           map[string]int{},
			wantErrContains: "func:bypass",
		},
		{
			name:            "dot import rejected explicitly",
			source:          "package api\nimport . \"github.com/gofrs/flock\"\nfunc bypass(path string) { _ = New(path) }\n",
			allow:           map[string]int{},
			wantErrContains: "dot import",
		},
		{
			name:            "direct package initializer",
			source:          "package api\nimport \"github.com/gofrs/flock\"\nvar packageLock = flock.New(\"x\")\n",
			allow:           map[string]int{},
			wantErrContains: "init:packageLock",
		},
		{
			name:            "package initializer function literal",
			source:          "package api\nimport \"github.com/gofrs/flock\"\nvar packageFn = func() { _ = flock.New(\"x\") }\n",
			allow:           map[string]int{},
			wantErrContains: "init:packageFn/func-lit#1",
		},
		{
			name:            "package initializer literal ordinal spans value list",
			source:          "package api\nimport \"github.com/gofrs/flock\"\nvar first, second = func() {}, func() { _ = flock.New(\"x\") }\n",
			allow:           map[string]int{},
			wantErrContains: "init:first,second/func-lit#2",
		},
		{
			name:            "nested function literal",
			source:          "package api\nimport \"github.com/gofrs/flock\"\nfunc outer() { _ = func() func() { return func() { _ = flock.New(\"x\") } } }\n",
			allow:           map[string]int{},
			wantErrContains: "func:outer/func-lit#1/func-lit#1",
		},
		{
			name:            "repeated init function has ordinal owner",
			source:          "package api\nimport \"github.com/gofrs/flock\"\nfunc init() {}\nfunc init() { _ = flock.New(\"x\") }\n",
			allow:           map[string]int{},
			wantErrContains: "func:init#2",
		},
		{
			name:   "same method name is receiver qualified",
			source: "package api\nimport \"github.com/gofrs/flock\"\ntype A struct{}\ntype B struct{}\nfunc (*A) Lock(){ _ = flock.New(\"a\") }\nfunc (*B) Lock(){ _ = flock.New(\"b\") }\n",
			allow: map[string]int{
				"internal/api/fixture.go:method:*A.Lock": 1,
				"internal/api/fixture.go:method:*B.Lock": 1,
			},
		},
		{
			name:            "fake generated marker has no exemption",
			source:          "package api\n// Code generated-ish DO NOT EDIT\nimport \"github.com/gofrs/flock\"\nfunc bypass(){ _ = flock.New(\"x\") }\n",
			allow:           map[string]int{},
			wantErrContains: "func:bypass",
		},
		{
			name:            "canonical generated marker has no exemption",
			source:          "// Code generated by fixture. DO NOT EDIT.\npackage api\nimport \"github.com/gofrs/flock\"\nfunc bypass(){ _ = flock.New(\"x\") }\n",
			allow:           map[string]int{},
			wantErrContains: "func:bypass",
		},
		{
			name:            "second call in allowlisted owner rejected",
			source:          "package api\nimport \"github.com/gofrs/flock\"\nfunc reviewed(path string) { _ = flock.New(path); _ = flock.New(path) }\n",
			allow:           map[string]int{"internal/api/fixture.go:func:reviewed": 1},
			wantErrContains: "calls=2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "internal", "api")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(tc.source), 0o600); err != nil {
				t.Fatal(err)
			}
			got, scanErr := rawFlockOwnerInventory(root)
			if scanErr == nil {
				scanErr = compareOwnerInventory(got, tc.allow)
			}
			if tc.wantErrContains == "" {
				if scanErr != nil {
					t.Fatalf("unexpected error: %v; inventory=%v", scanErr, got)
				}
				return
			}
			if scanErr == nil || !strings.Contains(scanErr.Error(), tc.wantErrContains) {
				t.Fatalf("error=%v, want containing %q; inventory=%v", scanErr, tc.wantErrContains, got)
			}
		})
	}
}

func TestAppliedReleaseClassifierFixtureRejectsSibling(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := "package api\nfunc sibling(err error) { _ = markAppliedLockReleaseUnconfirmed(err) }\n"
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := appliedReleaseClassifierInventory(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := compareOwnerInventory(got, map[string]int{}); err == nil || !strings.Contains(err.Error(), "func:sibling") {
		t.Fatalf("extra applied-release classifier error=%v; inventory=%v", err, got)
	}
}

func packageDirForOwnershipTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func rawFlockOwnerInventory(repoRoot string) (map[string]int, error) {
	return productionOwnedCallInventory(repoRoot, func(fset *token.FileSet, file *ast.File, path string) (ownedCallMatcher, error) {
		aliases := map[string]struct{}{}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil || importPath != flockImportPath {
				continue
			}
			if spec.Name != nil && spec.Name.Name == "." {
				return nil, fmt.Errorf("raw flock owner inventory rejects dot import: %s", filepath.ToSlash(path))
			}
			alias := "flock"
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			if alias != "_" {
				aliases[alias] = struct{}{}
			}
		}
		if len(aliases) == 0 {
			return func(*ast.CallExpr) (bool, error) { return false, nil }, nil
		}

		info := &types.Info{Uses: map[*ast.Ident]types.Object{}}
		config := types.Config{
			Importer: ownershipTestImporter{},
			Error:    func(error) {},
		}
		_, _ = config.Check(file.Name.Name, fset, []*ast.File{file}, info)
		return func(call *ast.CallExpr) (bool, error) {
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "New" {
				return false, nil
			}
			qualifier, ok := sel.X.(*ast.Ident)
			if !ok {
				return false, nil
			}
			if _, targetAlias := aliases[qualifier.Name]; !targetAlias {
				return false, nil
			}
			obj := info.Uses[qualifier]
			if obj == nil {
				return false, fmt.Errorf("unresolved target binding %s at %s", qualifier.Name, fset.Position(qualifier.Pos()))
			}
			pkgName, ok := obj.(*types.PkgName)
			if !ok {
				return false, nil
			}
			return pkgName.Imported().Path() == flockImportPath, nil
		}, nil
	})
}

func appliedReleaseClassifierInventory(repoRoot string) (map[string]int, error) {
	return productionOwnedCallInventory(repoRoot, func(_ *token.FileSet, _ *ast.File, _ string) (ownedCallMatcher, error) {
		return func(call *ast.CallExpr) (bool, error) {
			ident, ok := call.Fun.(*ast.Ident)
			return ok && ident.Name == "markAppliedLockReleaseUnconfirmed", nil
		}, nil
	})
}

type ownedCallMatcher func(*ast.CallExpr) (bool, error)

func productionOwnedCallInventory(
	repoRoot string,
	matcherFactory func(*token.FileSet, *ast.File, string) (ownedCallMatcher, error),
) (map[string]int, error) {
	apiRoot := filepath.Join(repoRoot, "internal", "api")
	result := map[string]int{}
	err := filepath.WalkDir(apiRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		matcher, err := matcherFactory(fset, file, path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		if err := inventoryOwnedCalls(fset, file, filepath.ToSlash(rel), matcher, result); err != nil {
			return err
		}
		return nil
	})
	return result, err
}

func inventoryOwnedCalls(
	fset *token.FileSet,
	file *ast.File,
	rel string,
	matcher ownedCallMatcher,
	result map[string]int,
) error {
	attributed := map[token.Pos]struct{}{}
	var firstErr error
	var walkOwner func([]ast.Node, string)
	walkOwner = func(roots []ast.Node, owner string) {
		literalOrdinal := 0
		for _, root := range roots {
			ast.Inspect(root, func(node ast.Node) bool {
				if node == nil || firstErr != nil {
					return false
				}
				if literal, ok := node.(*ast.FuncLit); ok {
					literalOrdinal++
					walkOwner([]ast.Node{literal.Body}, fmt.Sprintf("%s/func-lit#%d", owner, literalOrdinal))
					return false
				}
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				matched, err := matcher(call)
				if err != nil {
					firstErr = err
					return false
				}
				if matched {
					result[rel+":"+owner]++
					attributed[call.Pos()] = struct{}{}
				}
				return true
			})
		}
	}

	initOrdinal := 0
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if decl.Body == nil {
				continue
			}
			owner, err := functionOwner(fset, decl, &initOrdinal)
			if err != nil {
				return err
			}
			walkOwner([]ast.Node{decl.Body}, owner)
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				valueSpec, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				names := make([]string, 0, len(valueSpec.Names))
				for _, name := range valueSpec.Names {
					names = append(names, name.Name)
				}
				owner := "init:" + strings.Join(names, ",")
				values := make([]ast.Node, 0, len(valueSpec.Values))
				for _, value := range valueSpec.Values {
					values = append(values, value)
				}
				walkOwner(values, owner)
			}
		}
		if firstErr != nil {
			return firstErr
		}
	}

	ast.Inspect(file, func(node ast.Node) bool {
		if firstErr != nil {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		matched, err := matcher(call)
		if err != nil {
			firstErr = err
			return false
		}
		if matched {
			if _, ok := attributed[call.Pos()]; !ok {
				firstErr = fmt.Errorf("matched call at %s has no lexical owner", fset.Position(call.Pos()))
				return false
			}
		}
		return true
	})
	return firstErr
}

func functionOwner(fset *token.FileSet, decl *ast.FuncDecl, initOrdinal *int) (string, error) {
	if decl.Recv == nil {
		if decl.Name.Name == "init" {
			*initOrdinal++
			return fmt.Sprintf("func:init#%d", *initOrdinal), nil
		}
		return "func:" + decl.Name.Name, nil
	}
	if len(decl.Recv.List) != 1 {
		return "", fmt.Errorf("method %s at %s has %d receiver fields", decl.Name.Name, fset.Position(decl.Pos()), len(decl.Recv.List))
	}
	var receiver bytes.Buffer
	if err := format.Node(&receiver, fset, decl.Recv.List[0].Type); err != nil {
		return "", fmt.Errorf("format receiver for %s: %w", decl.Name.Name, err)
	}
	return "method:" + receiver.String() + "." + decl.Name.Name, nil
}

func compareOwnerInventory(got, want map[string]int) error {
	keys := map[string]struct{}{}
	for key := range got {
		keys[key] = struct{}{}
	}
	for key := range want {
		keys[key] = struct{}{}
	}
	var ordered []string
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		if got[key] != want[key] {
			return fmt.Errorf("owned call %s calls=%d, reviewed=%d", key, got[key], want[key])
		}
	}
	return nil
}

type ownershipTestImporter struct{}

func (ownershipTestImporter) Import(path string) (*types.Package, error) {
	if path != flockImportPath {
		return importer.Default().Import(path)
	}
	pkg := types.NewPackage(path, "flock")
	flockType := types.NewNamed(types.NewTypeName(token.NoPos, pkg, "Flock", nil), types.NewStruct(nil, nil), nil)
	pkg.Scope().Insert(flockType.Obj())
	params := types.NewTuple(types.NewParam(token.NoPos, pkg, "path", types.Typ[types.String]))
	results := types.NewTuple(types.NewParam(token.NoPos, pkg, "lock", types.NewPointer(flockType)))
	newFn := types.NewFunc(token.NoPos, pkg, "New", types.NewSignatureType(nil, nil, nil, params, results, false))
	pkg.Scope().Insert(newFn)
	pkg.MarkComplete()
	return pkg, nil
}

func productionSupervisorLockSuffixReferences(repoRoot string) ([]string, error) {
	apiRoot := filepath.Join(repoRoot, "internal", "api")
	owners := map[string]struct{}{}
	err := filepath.WalkDir(apiRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		found := false
		ast.Inspect(file, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && ident.Name == "supervisorIntentLockSuffix" {
				found = true
				return false
			}
			return true
		})
		if !found {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, path)
		if err != nil {
			return err
		}
		owners[filepath.ToSlash(rel)] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var result []string
	for owner := range owners {
		result = append(result, owner)
	}
	sort.Strings(result)
	return result, nil
}

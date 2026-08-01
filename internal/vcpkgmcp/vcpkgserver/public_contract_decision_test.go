package vcpkgserver

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type publicContractDecisionRecord struct {
	path         string
	sha256       string
	blob         string
	identity     string
	statusLine   string
	semanticBody string
}

func TestVcpkgPublicContractDecisionReference(t *testing.T) {
	root := publicContractRepositoryRoot(t)
	records := []publicContractDecisionRecord{
		{
			path:       "work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md",
			sha256:     "9B68F91B072CBBBF816B84A08B7BEF4709053E086CA239C7ADE9DE7F45C98B06",
			blob:       "7ac411e3c52c9c292f3c0007d709325db0b7ae72",
			identity:   "title: vcpkg MCP — tool contracts, discovery, and the behavioural invariants",
			statusLine: "status: accepted",
		},
		{
			path:       "work-items/decisions/2026-07-29-vcpkg-pinstatus-remote-query-admission.md",
			sha256:     "EB3A8C2DD6E07ADD99D81DF324CB6EF552E5FD360D62EFFBBC84F7C50DCC75D6",
			blob:       "381a3195bbc07a4bc8de8e49270dfb460004b2a4",
			identity:   "# Vcpkg pin-status remote-query admission",
			statusLine: "Status: accepted",
		},
		{
			path:         "work-items/decisions/2026-07-30-vcpkg-mcp-bounded-public-result-contract.md",
			sha256:       "2B8B714B4F86C5C8504ED45454856EC40D7EB8C4AB806C65C9C96CE318277D20",
			blob:         "b436b8136a6ae455677bcc0f6988a7bbf23abbcf",
			identity:     "id: 2026-07-30-vcpkg-mcp-bounded-public-result-contract",
			statusLine:   "status: accepted",
			semanticBody: "D4510321F00C5F057D5F698E31D46706BF877787C873348460B29B1761BEEC85",
		},
	}

	for _, record := range records {
		record := record
		t.Run(filepath.Base(record.path), func(t *testing.T) {
			raw := publicContractReadFile(t, root, record.path)
			if got := fmt.Sprintf("%X", sha256.Sum256(raw)); got != record.sha256 {
				t.Fatalf("SHA-256 = %s, want %s", got, record.sha256)
			}
			if got := publicContractGitBlob(raw); got != record.blob {
				t.Fatalf("Git blob = %s, want %s", got, record.blob)
			}
			if !publicContractHasExactLine(raw, record.statusLine) {
				t.Fatalf("missing exact status line %q", record.statusLine)
			}
			publicContractRequireIndexedBlob(t, root, record.path, record.blob)
			if record.semanticBody != "" {
				publicContractCheckAcceptedMetadata(t, raw, record.semanticBody)
			}
		})
	}

	publicContractCheckDecisionRelations(t, root, records)
	publicContractCheckSourceContract(t, root)
}

func publicContractRepositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root containing go.mod and .git not found")
		}
		dir = parent
	}
}

func publicContractReadFile(t *testing.T, root, relative string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return raw
}

func publicContractGitBlob(raw []byte) string {
	header := fmt.Sprintf("blob %d\x00", len(raw))
	sum := sha1.Sum(append([]byte(header), raw...))
	return fmt.Sprintf("%x", sum)
}

func publicContractHasExactLine(raw []byte, want string) bool {
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if line == want {
			return true
		}
	}
	return false
}

// The index is the candidate tree during the predecessor's pre-commit check;
// after admission it is identical to HEAD. Requiring the exact indexed blob and
// no worktree/index delta rejects untracked or locally modified decision paths
// in both states without granting a dirty-tree bypass.
func publicContractRequireIndexedBlob(t *testing.T, root, relative, wantBlob string) {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "ls-files", "--stage", "--", filepath.ToSlash(relative))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files %s: %v", relative, err)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) != 4 || lines[1] != wantBlob || lines[2] != "0" {
		t.Fatalf("%s index entry = %q, want stage-0 blob %s", relative, strings.TrimSpace(string(out)), wantBlob)
	}
	cmd = exec.Command("git", "-C", root, "diff", "--quiet", "--", filepath.ToSlash(relative))
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s differs between worktree and candidate index: %v", relative, err)
	}
}

func publicContractCheckAcceptedMetadata(t *testing.T, raw []byte, wantSemantic string) {
	t.Helper()
	for _, line := range []string{
		"revision: 5",
		"owner: architect",
		"review-result: PASS",
		"lead-acceptance: accepted",
		"accepted-date: 2026-07-30",
		"semantic-body-sha256: " + wantSemantic,
		"supersedes: none",
		"superseded-by: none",
	} {
		if !publicContractHasExactLine(raw, line) {
			t.Fatalf("accepted bounded-result record missing immutable metadata %q", line)
		}
	}
	bodyStart := bytes.Index(raw, []byte("# Bounded Public Result and Filesystem Ingress Contract for vcpkg MCP"))
	if bodyStart < 0 {
		t.Fatal("accepted bounded-result record has no canonical first Markdown heading")
	}
	if got := fmt.Sprintf("%X", sha256.Sum256(raw[bodyStart:])); got != wantSemantic {
		t.Fatalf("semantic body SHA-256 = %s, want %s", got, wantSemantic)
	}
}

func publicContractCheckDecisionRelations(t *testing.T, root string, records []publicContractDecisionRecord) {
	t.Helper()
	decisionRoot := filepath.Join(root, "work-items", "decisions")
	claims := make(map[string][]string)
	var liveFiles []string
	err := filepath.WalkDir(decisionRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := strings.ToLower(entry.Name())
			if path != decisionRoot && (name == "archive" || name == "legacy" || name == "_archive") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".md" {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		liveFiles = append(liveFiles, relative)
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if identity := publicContractStructuralIdentity(raw); identity != "" {
			claims[identity] = append(claims[identity], relative)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan live decisions: %v", err)
	}
	sort.Strings(liveFiles)

	for _, record := range records {
		got := claims[record.identity]
		if len(got) != 1 || got[0] != record.path {
			t.Errorf("decision identity %q claimed by %v, want only %s", record.identity, got, record.path)
		}
		// Only the accepted bounded-result decision declares an immutable
		// "supersedes: none" / "superseded-by: none" relation. The two earlier
		// records are pinned historical inputs and may have legitimate relations
		// outside this decision's scope.
		if record.semanticBody == "" {
			continue
		}
		stem := strings.TrimSuffix(filepath.Base(record.path), ".md")
		for _, relative := range liveFiles {
			if relative == record.path {
				continue
			}
			raw := publicContractReadFile(t, root, relative)
			for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
				lower := strings.ToLower(strings.TrimSpace(line))
				if (strings.HasPrefix(lower, "supersedes:") || strings.HasPrefix(lower, "superseded-by:")) &&
					(strings.Contains(line, stem) || strings.Contains(line, record.identity)) {
					t.Errorf("%s changes supersession of %s: %q", relative, record.path, line)
				}
			}
		}
	}
}

func publicContractStructuralIdentity(raw []byte) string {
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	inFrontmatter := len(lines) > 0 && lines[0] == "---"
	for i, line := range lines {
		if i > 0 && inFrontmatter && line == "---" {
			inFrontmatter = false
			continue
		}
		if inFrontmatter && (strings.HasPrefix(line, "id: ") || strings.HasPrefix(line, "title: ")) {
			if strings.HasPrefix(line, "id: 2026-07-30-vcpkg-mcp-bounded-public-result-contract") ||
				strings.HasPrefix(line, "title: vcpkg MCP — tool contracts, discovery, and the behavioural invariants") {
				return line
			}
		}
		if !inFrontmatter && strings.HasPrefix(line, "# ") {
			return line
		}
	}
	return ""
}

func publicContractCheckSourceContract(t *testing.T, root string) {
	t.Helper()
	anchors := map[string][]string{
		"internal/vcpkgmcp/publicresult/publicresult.go": {
			"const MaxEncodedBytes = 256 << 10",
			`var ErrBudgetInvariant = errors.New("VCPKG_RESULT_BUDGET_INVARIANT")`,
			`const InternalProjectionLimit OmissionReason = "internal_projection_limit"`,
			"type Projectable interface {\n\tPublicResultProjection() any\n}",
			"func MarshalIndent(result Projectable) ([]byte, error)",
		},
		"internal/vcpkgmcp/boundedio/boundedio.go": {
			"func ReadFile(ctx context.Context, fsys FS, path string, maxBytes, pageBytes int64)",
			"func ReadDirComplete(ctx context.Context, fsys FS, path string, maxEntries, pageEntries int)",
			"return DirResult{Limited: true, TotalKnown: false}, nil",
		},
		"internal/vcpkgmcp/vcpkgserver/helpers.go": {
			"func jsonResult(v publicresult.Projectable) (*mcp.CallToolResult, error)",
			"body, err := publicresult.MarshalIndent(v)",
		},
		"internal/vcpkgmcp/lastfailure/limits.go": {
			"func (p evidenceProjection) diagnosticsDroppedExact() bool",
			"p.directoryEntries == domainNotApplicable",
			"p.relevantLogs == domainNotApplicable",
			"p.logBytes == domainNotApplicable",
			"p.diagnostics == domainNotApplicable",
			"p.directoryEntries == domainSettledComplete",
			"p.relevantLogs == domainSettledComplete",
			"(p.logBytes == domainSettledComplete || p.logBytes == domainNotApplicable)",
			"(p.diagnostics == domainSettledComplete || p.diagnostics == domainNotApplicable)",
		},
		"internal/vcpkgmcp/pinstatus/types.go": {
			"type PublicFailure struct {",
			`ID       FailureID   ` + "`json:\"id\"`",
			`CauseIDs []FailureID ` + "`json:\"cause_ids,omitempty\"`",
			`ExitCode *int        ` + "`json:\"exit_code,omitempty\"`",
			`Detail   string      ` + "`json:\"detail,omitempty\"`",
		},
	}
	for relative, required := range anchors {
		source := strings.ReplaceAll(string(publicContractReadFile(t, root, relative)), "\r\n", "\n")
		for _, anchor := range required {
			if !strings.Contains(source, anchor) {
				t.Errorf("%s missing contract anchor %q", relative, anchor)
			}
		}
	}

	publicContractCheckClosedVocabularies(t, root)
	publicContractCheckFunctionOwners(t, root)
	publicContractCheckDependencyOwners(t, root)
}

func publicContractCheckClosedVocabularies(t *testing.T, root string) {
	t.Helper()
	omissions := publicContractTypedStringConstants(
		t,
		root,
		"internal/vcpkgmcp/publicresult/publicresult.go",
		"OmissionReason",
	)
	if got := strings.Join(omissions, ","); got != "internal_projection_limit" {
		t.Errorf("public omission vocabulary = %q, want internal_projection_limit", got)
	}

	failures := publicContractTypedStringConstants(
		t,
		root,
		"internal/vcpkgmcp/pinstatus/types.go",
		"FailureID",
	)
	want := []string{
		"VCPKG_REMOTE_START_FAILED",
		"VCPKG_PROCESS_CONTAINMENT_UNAVAILABLE",
		"VCPKG_REMOTE_PARSE_LIMIT",
		"VCPKG_REMOTE_CANCELED",
		"VCPKG_REMOTE_TIMEOUT",
		"VCPKG_PROCESS_CLEANUP_TIMEOUT",
		"VCPKG_GIT_EXIT_NONZERO",
		"VCPKG_REMOTE_QUERY_FAILED",
	}
	if strings.Join(failures, "\n") != strings.Join(want, "\n") {
		t.Errorf("pin-status failure vocabulary = %v, want %v", failures, want)
	}
}

func publicContractTypedStringConstants(t *testing.T, root, relative, typeName string) []string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", relative, err)
	}
	values, err := publicContractTypedStringConstantsFromAST(file, typeName)
	if err != nil {
		t.Fatalf("%s %s vocabulary: %v", relative, typeName, err)
	}
	return values
}

func publicContractTypedStringConstantsFromAST(file *ast.File, typeName string) ([]string, error) {
	var values []string
	for _, declaration := range file.Decls {
		constants, ok := declaration.(*ast.GenDecl)
		if !ok || constants.Tok != token.CONST {
			continue
		}
		for _, spec := range constants.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			declaredType, ok := valueSpec.Type.(*ast.Ident)
			if !ok || declaredType.Name != typeName {
				continue
			}
			if len(valueSpec.Names) != len(valueSpec.Values) {
				return nil, fmt.Errorf(
					"typed const declaration has %d names and %d values",
					len(valueSpec.Names),
					len(valueSpec.Values),
				)
			}
			for i, expression := range valueSpec.Values {
				literal, ok := expression.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return nil, fmt.Errorf("%s is not a string literal", valueSpec.Names[i].Name)
				}
				value, err := strconv.Unquote(literal.Value)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", valueSpec.Names[i].Name, err)
				}
				values = append(values, value)
			}
		}
	}
	return values, nil
}

func publicContractCheckFunctionOwners(t *testing.T, root string) {
	t.Helper()
	want := map[string]string{
		"function MarshalIndent":                            "internal/vcpkgmcp/publicresult/publicresult.go",
		"function ReadFile":                                 "internal/vcpkgmcp/boundedio/boundedio.go",
		"function ReadDirComplete":                          "internal/vcpkgmcp/boundedio/boundedio.go",
		"function jsonResult":                               "internal/vcpkgmcp/vcpkgserver/helpers.go",
		"method evidenceProjection.diagnosticsDroppedExact": "internal/vcpkgmcp/lastfailure/limits.go",
	}
	got := make(map[string][]string)
	publicContractWalkProductionGo(t, root, func(relative string, file *ast.File) {
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			owner := publicContractFunctionOwnerIdentity(fn)
			if _, tracked := want[owner]; tracked {
				got[owner] = append(got[owner], relative)
			}
		}
	})
	for identity, owner := range want {
		if len(got[identity]) != 1 || got[identity][0] != owner {
			t.Errorf("%s owners = %v, want only %s", identity, got[identity], owner)
		}
	}
}

func publicContractFunctionOwnerIdentity(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return "function " + fn.Name.Name
	}
	receiver := fn.Recv.List[0].Type
	if pointer, ok := receiver.(*ast.StarExpr); ok {
		receiver = pointer.X
	}
	if name, ok := receiver.(*ast.Ident); ok {
		return "method " + name.Name + "." + fn.Name.Name
	}
	return "method <unknown>." + fn.Name.Name
}

func publicContractCheckDependencyOwners(t *testing.T, root string) {
	t.Helper()
	requiredProcessOwnerCall := false
	publicContractWalkProductionGo(t, root, func(relative string, file *ast.File) {
		for _, imported := range file.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			if (strings.HasPrefix(relative, "internal/vcpkgmcp/publicresult/") ||
				strings.HasPrefix(relative, "internal/vcpkgmcp/boundedio/")) &&
				strings.HasPrefix(path, "mcp-local-hub/internal/") {
				t.Errorf("dependency leaf %s imports repository package %s", relative, path)
			}
			if path == "mcp-local-hub/internal/vcpkgmcp/vcpkgserver" &&
				!publicContractCompositionRootImporterAllowed(relative) {
				t.Errorf("non-composition source %s imports composition root %s", relative, path)
			}
		}
		if strings.HasPrefix(relative, "internal/vcpkgmcp/pinstatus/") {
			callsOwner, violations := publicContractProcessOwnershipUsage(file)
			requiredProcessOwnerCall = requiredProcessOwnerCall || callsOwner
			for _, violation := range violations {
				t.Errorf("%s bypasses RunContainedStream ownership with %s", relative, violation)
			}
		}
	})
	if !requiredProcessOwnerCall {
		t.Error("pinstatus has no production call to internal/process.RunContainedStream")
	}
}

func publicContractCompositionRootImporterAllowed(relative string) bool {
	return relative == "internal/vcpkgmcp/cmd.go"
}

func publicContractProcessOwnershipUsage(file *ast.File) (bool, []string) {
	imports := make(map[string]string)
	var violations []string
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			violations = append(violations, "invalid import path")
			continue
		}
		alias := filepath.Base(path)
		if imported.Name != nil {
			alias = imported.Name.Name
		}
		if alias == "." && publicContractProcessSensitiveImport(path) {
			violations = append(violations, "dot import of "+path)
			continue
		}
		if alias != "_" {
			imports[alias] = path
		}
		if path == "syscall" ||
			path == "golang.org/x/sys/windows" ||
			path == "golang.org/x/sys/unix" {
			violations = append(violations, "platform process import "+path)
		}
	}

	execVariables := publicContractExecCommandVariables(file, imports)
	callsOwner := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if receiver, ok := selector.X.(*ast.Ident); ok {
			switch imports[receiver.Name] {
			case "mcp-local-hub/internal/process":
				switch selector.Sel.Name {
				case "RunContainedStream":
					callsOwner = true
				case "RunUnderKillJob", "StartWithJob", "NewKillOnCloseJob":
					violations = append(violations, "internal/process."+selector.Sel.Name)
				}
			case "os":
				if selector.Sel.Name == "StartProcess" {
					violations = append(violations, "os.StartProcess")
				}
			case "syscall":
				switch selector.Sel.Name {
				case "Exec", "ForkExec", "StartProcess", "Kill":
					violations = append(violations, "syscall."+selector.Sel.Name)
				}
			}
		}
		if publicContractExecExpression(selector.X, imports, execVariables) &&
			publicContractExecLifecycleMethod(selector.Sel.Name) {
			violations = append(violations, "exec.Cmd."+selector.Sel.Name)
		}
		return true
	})
	sort.Strings(violations)
	return callsOwner, violations
}

func publicContractProcessSensitiveImport(path string) bool {
	switch path {
	case "os", "os/exec", "syscall", "golang.org/x/sys/windows", "golang.org/x/sys/unix",
		"mcp-local-hub/internal/process":
		return true
	default:
		return false
	}
}

func publicContractExecLifecycleMethod(name string) bool {
	switch name {
	case "Start", "Wait", "Run", "Output", "CombinedOutput",
		"StdoutPipe", "StderrPipe", "StdinPipe",
		"Kill", "Signal", "Release":
		return true
	default:
		return false
	}
}

func publicContractExecCommandVariables(file *ast.File, imports map[string]string) map[string]bool {
	execVariables := make(map[string]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		field, ok := node.(*ast.Field)
		if !ok || !publicContractExecCmdType(field.Type, imports) {
			return true
		}
		for _, name := range field.Names {
			execVariables[name.Name] = true
		}
		return true
	})

	for {
		changed := false
		ast.Inspect(file, func(node ast.Node) bool {
			switch statement := node.(type) {
			case *ast.AssignStmt:
				for i, left := range statement.Lhs {
					if i >= len(statement.Rhs) ||
						!publicContractExecExpression(statement.Rhs[i], imports, execVariables) {
						continue
					}
					if name, ok := left.(*ast.Ident); ok && !execVariables[name.Name] {
						execVariables[name.Name] = true
						changed = true
					}
				}
			case *ast.ValueSpec:
				for i, name := range statement.Names {
					if i < len(statement.Values) &&
						publicContractExecExpression(statement.Values[i], imports, execVariables) &&
						!execVariables[name.Name] {
						execVariables[name.Name] = true
						changed = true
					}
				}
			}
			return true
		})
		if !changed {
			return execVariables
		}
	}
}

func publicContractExecCmdType(expression ast.Expr, imports map[string]string) bool {
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Cmd" {
		return false
	}
	receiver, ok := selector.X.(*ast.Ident)
	return ok && imports[receiver.Name] == "os/exec"
}

func publicContractExecExpression(
	expression ast.Expr,
	imports map[string]string,
	execVariables map[string]bool,
) bool {
	switch expression := expression.(type) {
	case *ast.Ident:
		return execVariables[expression.Name]
	case *ast.ParenExpr:
		return publicContractExecExpression(expression.X, imports, execVariables)
	case *ast.SelectorExpr:
		return publicContractExecExpression(expression.X, imports, execVariables)
	case *ast.CallExpr:
		selector, ok := expression.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		receiver, ok := selector.X.(*ast.Ident)
		return ok &&
			imports[receiver.Name] == "os/exec" &&
			(selector.Sel.Name == "Command" || selector.Sel.Name == "CommandContext")
	default:
		return false
	}
}

func publicContractWalkProductionGo(t *testing.T, root string, visit func(relative string, file *ast.File)) {
	t.Helper()
	base := filepath.Join(root, "internal", "vcpkgmcp")
	err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		visit(filepath.ToSlash(relative), file)
		return nil
	})
	if err != nil {
		t.Fatalf("scan production Go sources: %v", err)
	}
}

func TestPublicContractTypedStringVocabularyGuard(t *testing.T) {
	source := `package fixture
type OmissionReason string
const Single OmissionReason = "single"
const (
	First OmissionReason = "first"
	Second OmissionReason = "second"
)`
	file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := publicContractTypedStringConstantsFromAST(file, "OmissionReason")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"single", "first", "second"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("typed vocabulary = %v, want %v", got, want)
	}
}

func TestPublicContractCompositionRootImportGuard(t *testing.T) {
	cases := []struct {
		relative string
		allowed  bool
	}{
		{relative: "internal/vcpkgmcp/cmd.go", allowed: true},
		{relative: "internal/vcpkgmcp/pinstatus/remote.go", allowed: false},
		{relative: "internal/vcpkgmcp/lastfailure/lastfailure.go", allowed: false},
	}
	for _, test := range cases {
		if got := publicContractCompositionRootImporterAllowed(test.relative); got != test.allowed {
			t.Errorf("composition-root import allowed for %s = %v, want %v", test.relative, got, test.allowed)
		}
	}
}

func TestPublicContractProcessOwnershipGuard(t *testing.T) {
	cases := []struct {
		name           string
		source         string
		wantOwnerCall  bool
		wantViolations []string
	}{
		{
			name: "command construction delegates lifecycle",
			source: `package fixture
import (
	"os/exec"
	hubprocess "mcp-local-hub/internal/process"
)
func run() {
	cmd := exec.Command("git", "ls-remote")
	hubprocess.RunContainedStream(ctx, cmd, opts, consume)
}`,
			wantOwnerCall: true,
		},
		{
			name: "typed command parameter start bypass",
			source: `package fixture
import "os/exec"
func run(cmd *exec.Cmd) { _ = cmd.Start() }`,
			wantViolations: []string{"exec.Cmd.Start"},
		},
		{
			name: "typed command parameter wait bypass",
			source: `package fixture
import "os/exec"
func run(cmd *exec.Cmd) { _ = cmd.Wait() }`,
			wantViolations: []string{"exec.Cmd.Wait"},
		},
		{
			name: "chained run bypass",
			source: `package fixture
import "os/exec"
func run() { _ = exec.Command("git").Run() }`,
			wantViolations: []string{"exec.Cmd.Run"},
		},
		{
			name: "platform start bypass",
			source: `package fixture
import "os"
func run() { _, _ = os.StartProcess("git", nil, nil) }`,
			wantViolations: []string{"os.StartProcess"},
		},
		{
			name: "legacy process owner bypass",
			source: `package fixture
import hubprocess "mcp-local-hub/internal/process"
func run() { _ = hubprocess.RunUnderKillJob(ctx, cmd) }`,
			wantViolations: []string{"internal/process.RunUnderKillJob"},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), "fixture.go", test.source, 0)
			if err != nil {
				t.Fatal(err)
			}
			gotOwnerCall, gotViolations := publicContractProcessOwnershipUsage(file)
			if gotOwnerCall != test.wantOwnerCall {
				t.Errorf("owner call = %v, want %v", gotOwnerCall, test.wantOwnerCall)
			}
			if strings.Join(gotViolations, "\n") != strings.Join(test.wantViolations, "\n") {
				t.Errorf("violations = %v, want %v", gotViolations, test.wantViolations)
			}
		})
	}
}

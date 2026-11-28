package api

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"
)

// Inspect the production leaves BEFORE executing one with a positive PID.
// If a regression reinstates taskkill/TreeKillByPID, this test must fail before
// it can touch any real process. The intentionally strict effect-free body is
// the safety boundary; changing it requires a different owner-aware API.
func requireNumericalTerminationRefusalLeaves(t *testing.T) {
	t.Helper()
	for file, name := range map[string]string{"install.go": "taskkillProcessTreeByPID", "stop_force_supervisor_other.go": "stopForceKillSupervisorPIDTree", "stop_force_supervisor_windows.go": "stopForceKillSupervisorPIDTree"} {
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Name.Name != name {
				continue
			}
			found = true
			if function.Body == nil || len(function.Body.List) != 1 {
				t.Fatalf("%s reintroduced work before its owner refusal", name)
			}
			returned, ok := function.Body.List[0].(*ast.ReturnStmt)
			if !ok || len(returned.Results) != 1 {
				t.Fatalf("%s lacks unconditional refusal", name)
			}
			if file == "stop_force_supervisor_windows.go" {
				call, ok := returned.Results[0].(*ast.CallExpr)
				if !ok || len(call.Args) != 1 {
					t.Fatal("Windows numeric adapter bypasses the guarded leaf")
				}
				functionName, functionOK := call.Fun.(*ast.Ident)
				argument, argumentOK := call.Args[0].(*ast.Ident)
				if !functionOK || functionName.Name != "taskkillProcessTreeByPIDFn" || !argumentOK || argument.Name != "pid" {
					t.Fatal("Windows numeric adapter changed its effect boundary")
				}
				continue
			}
			pointer, ok := returned.Results[0].(*ast.UnaryExpr)
			if !ok || pointer.Op != token.AND {
				t.Fatalf("%s no longer returns a data-only refusal", name)
			}
			literal, ok := pointer.X.(*ast.CompositeLit)
			if !ok {
				t.Fatalf("%s can execute a termination helper", name)
			}
			typ, ok := literal.Type.(*ast.Ident)
			if !ok || typ.Name != "OriginalProcessOwnerRequiredError" || len(literal.Elts) != 1 {
				t.Fatalf("%s refusal shape changed", name)
			}
			field, ok := literal.Elts[0].(*ast.KeyValueExpr)
			if !ok {
				t.Fatalf("%s refusal has an effectful field", name)
			}
			key, keyOK := field.Key.(*ast.Ident)
			value, valueOK := field.Value.(*ast.Ident)
			if !keyOK || key.Name != "PID" || !valueOK || value.Name != "pid" {
				t.Fatalf("%s refusal evaluates more than the supplied PID", name)
			}
		}
		if !found {
			t.Fatalf("production leaf %s absent", name)
		}
	}
}

func TestNumericalTerminationLeavesRequireOriginalOwner(t *testing.T) {
	requireNumericalTerminationRefusalLeaves(t)
	previous := taskkillProcessTreeByPIDFn
	taskkillProcessTreeByPIDFn = taskkillProcessTreeByPID
	t.Cleanup(func() { taskkillProcessTreeByPIDFn = previous })
	for _, leaf := range []func(int) error{taskkillProcessTreeByPID, stopForceKillSupervisorPIDTree} {
		err := leaf(4242)
		var refused *OriginalProcessOwnerRequiredError
		if !errors.Is(err, ErrOriginalProcessOwnerRequired) || !errors.As(err, &refused) || refused.PID != 4242 {
			t.Fatalf("numeric termination did not return its typed refusal: %v", err)
		}
	}
}

func TestRemovedTargetCannotPromotePIDHintToTerminationAuthority(t *testing.T) {
	requireNumericalTerminationRefusalLeaves(t)
	previousPID, previousTree, previousIdentity, previousSID, previousPort := stopForceKillPIDFn, taskkillProcessTreeByPIDFn, processIdentityByPID, processOwnerSIDMatchesCurrentFn, forceKillByPortFn
	stopForceKillPIDFn, taskkillProcessTreeByPIDFn = stopForceKillSupervisorPIDTree, taskkillProcessTreeByPID
	processIdentityByPID = func(int) (string, string, bool) { return "mcphub.exe", "mcphub.exe", true }
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil }
	forceKillByPortFn = func(int, time.Duration) (portKillOutcome, error) {
		t.Fatal("original-owner refusal fell through to port termination")
		return portKillNoListener, nil
	}
	t.Cleanup(func() {
		stopForceKillPIDFn, taskkillProcessTreeByPIDFn, processIdentityByPID, processOwnerSIDMatchesCurrentFn, forceKillByPortFn = previousPID, previousTree, previousIdentity, previousSID, previousPort
	})
	target := SupervisorDaemon{TaskName: `\mcp-local-hub-force-fixture-default`, Port: 33105}
	row := forceKillOneSupervisorTarget(target, map[string]int{strings.TrimPrefix(target.TaskName, `\`): 4242})
	if row.Code != ForceStopOwnerUnavailable || row.Err == "" {
		t.Fatalf("observation became authority: %+v", row)
	}
}

func TestPortOnlyTerminationPreservesOwnerRequiredError(t *testing.T) {
	requireNumericalTerminationRefusalLeaves(t)
	previousTree, previousLookup, previousIdentity, previousSID := taskkillProcessTreeByPIDFn, lookupPortPID, processIdentityByPID, processOwnerSIDMatchesCurrentFn
	taskkillProcessTreeByPIDFn = taskkillProcessTreeByPID
	lookupPortPID = func(int) (int, bool, error) { return 4242, true, nil }
	processIdentityByPID = func(int) (string, string, bool) { return "mcphub.exe", "mcphub.exe", true }
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil }
	t.Cleanup(func() {
		taskkillProcessTreeByPIDFn, lookupPortPID, processIdentityByPID, processOwnerSIDMatchesCurrentFn = previousTree, previousLookup, previousIdentity, previousSID
	})
	outcome, err := killDaemonByPortOutcome(33105, 0)
	if !errors.Is(err, ErrOriginalProcessOwnerRequired) || outcome != portKillProbeUnknown {
		t.Fatalf("port observation became termination: outcome=%v err=%v", outcome, err)
	}
}

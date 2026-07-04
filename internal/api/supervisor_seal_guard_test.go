package api

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/autostart"
)

// TestTestMainSealsLiveFleetAndSlowSeams is the RUNTIME half of the recurrence
// guard for the internal/api TestMain default-seal (see main_test.go). It asserts
// the live-fleet / slow real-I/O seams are neutralized in the running test binary.
//
// It verifies by IDENTITY (reflect pointer), NEVER by executing the seam. If a
// seal were dropped (the exact regression this guards against), EXECUTING
// installAutostart* / taskkill / stopForceKill / a readiness poll would run
// `schtasks /Run` / `systemctl start`, `taskkill`, a real SIGKILL, or a live
// localhost HTTP poll against the developer's real supervisor/processes BEFORE the
// assertion could fail (Codex #503 r2). Pointer comparison touches nothing.
//
// lookupProcess / lookupProcessBatch are checked == nil. The dial + readiness +
// snapshot seams have func-literal defaults (not comparable to a named production
// function); they are covered by the AST guard below instead.
func TestTestMainSealsLiveFleetAndSlowSeams(t *testing.T) {
	if lookupProcess != nil {
		t.Error("lookupProcess is not nil-sealed by TestMain — port-kill paths shell out to netstat/wmic and can taskkill a live daemon")
	}
	if lookupProcessBatch != nil {
		t.Error("lookupProcessBatch is not nil-sealed by TestMain")
	}

	sealed := func(name string, seam, production any) {
		t.Helper()
		if reflect.ValueOf(seam).Pointer() == reflect.ValueOf(production).Pointer() {
			t.Errorf("%s is NOT sealed by TestMain — it still points at its production default; a test reaching it would touch the live fleet", name)
		}
	}
	sealed("installAutostartOwnerStartFn", installAutostartOwnerStartFn, autostart.StartOwner)
	sealed("installAutostartBackendFactoryFn", installAutostartBackendFactoryFn, autostart.New)
	sealed("proxyReadinessFn", proxyReadinessFn, verifyProxyReady)
	sealed("loopbackPortOwnerFn", loopbackPortOwnerFn, loopbackPortOwnerPID)
	sealed("taskkillProcessTreeByPIDFn", taskkillProcessTreeByPIDFn, taskkillProcessTreeByPID)
	sealed("stopForceKillPIDFn", stopForceKillPIDFn, stopForceKillSupervisorPIDTree)
}

// liveSeamSinkNames are the production functions whose presence in a package-var
// initializer marks that var as a live-fleet / slow real-I/O seam that TestMain
// MUST default-seal. Extend this set when a new such sink is added.
var liveSeamSinkNames = map[string]bool{
	"verifyProxyReady": true, // localhost /mcp readiness HTTP poll
	// NOTE: verifySerenaWakeReady is intentionally NOT a sink here — its seam
	// (serenaWakeReadinessFn) is deliberately left unsealed (integration-exercised
	// by the WakeIdleSerena tests via controlled deps); see main_test.go.
	"runProcessSnapshot":             true, // wmic/ps process-table scan
	"loopbackPortOwnerPID":           true, // netstat loopback owner lookup
	"taskkillProcessTreeByPID":       true, // taskkill /T
	"stopForceKillSupervisorPIDTree": true, // POSIX process-group SIGKILL
	"StartOwner":                     true, // autostart.StartOwner → schtasks /Run
}

// TestNoUnsealedLiveSeam is the AST half of the recurrence guard (architect
// design, extended per Codex #503 r2 to the full sink registry): it fails the
// build if a new package-level var defaults to a live/slow sink function
// (directly, or via a wrapping func literal) but is NOT re-assigned inside
// TestMain. This closes the gap the runtime half cannot — a brand-new seam nobody
// remembered to seal — the same enforcement class as port_kill_guard_test.go.
func TestNoUnsealedLiveSeam(t *testing.T) {
	fset := token.NewFileSet()
	seamVars := map[string]string{} // varName -> file:line

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/api dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Logf("skipping unparseable file %s: %v", name, parseErr)
			continue
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, val := range vs.Values {
					if i >= len(vs.Names) || !referencesLiveSeamSink(val) {
						continue
					}
					seamVars[vs.Names[i].Name] = fmt.Sprintf("%s:%d", name, fset.Position(vs.Names[i].Pos()).Line)
				}
			}
		}
	}

	if len(seamVars) == 0 {
		t.Fatal("recurrence guard found ZERO live-seam vars — the AST detection is broken (it should find registerSupervisorReconcileFn, proxyReadinessFn, et al.)")
	}

	mainSrc, err := os.ReadFile("main_test.go")
	if err != nil {
		t.Fatalf("read main_test.go: %v", err)
	}
	for v, loc := range seamVars {
		if !bytes.Contains(mainSrc, []byte(v+" =")) {
			t.Errorf("live-fleet/slow seam %s (%s) defaults to a real sink function but is NOT sealed in TestMain (main_test.go). Add a hermetic default-stub to the seal block so `go test ./internal/api/` never dials the live supervisor / scans or kills real processes / HTTP-polls a real port.", v, loc)
		}
	}
}

// referencesLiveSeamSink reports whether an expression references a live-seam sink
// function anywhere — a DialSupervisorIPC* dial (by prefix) or any name in
// liveSeamSinkNames, whether directly (`= verifyProxyReady`) or inside a wrapping
// func literal (`= func(...) { return verifyProxyReady(...) }`).
func referencesLiveSeamSink(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if strings.HasPrefix(id.Name, "DialSupervisorIPC") || liveSeamSinkNames[id.Name] {
			found = true
			return false
		}
		return true
	})
	return found
}

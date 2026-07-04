package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

// TestTestMainSealsLiveFleetAndSlowSeams is the RUNTIME half of the recurrence
// guard for the internal/api TestMain default-seal (see main_test.go). It asserts
// every live-fleet / slow real-I/O seam is actually neutralized in the running
// test binary. If a refactor drops a seal, this fails loudly instead of silently
// re-arming the aggregate-slowness + live-daemon-kill class that #501/#502 and the
// TestMain seal fixed. (A partial seal previously let the suite taskkill the live
// memory/time daemons on 9123/9128 and blow the 5-minute gate on wmic shell-outs.)
func TestTestMainSealsLiveFleetAndSlowSeams(t *testing.T) {
	if lookupProcess != nil {
		t.Error("lookupProcess is not nil-sealed by TestMain — port-kill paths shell out to netstat/wmic and can taskkill a live daemon")
	}
	if lookupProcessBatch != nil {
		t.Error("lookupProcessBatch is not nil-sealed by TestMain")
	}

	ctx := context.Background()
	if _, err := registerSupervisorReconcileFn(ctx, false); !errors.Is(err, ErrSupervisorIPCUnavailable) {
		t.Errorf("registerSupervisorReconcileFn not sealed to ErrSupervisorIPCUnavailable: err=%v", err)
	}
	if _, err := supervisorReconcileApplyFn(ctx, false); !errors.Is(err, ErrSupervisorIPCUnavailable) {
		t.Errorf("supervisorReconcileApplyFn not sealed: err=%v", err)
	}
	if _, err := serenaWakeReconcileFn(ctx, false); !errors.Is(err, ErrSupervisorIPCUnavailable) {
		t.Errorf("serenaWakeReconcileFn not sealed: err=%v", err)
	}
	if _, err := autoRegisterReconcileFn(ctx, false); !errors.Is(err, ErrSupervisorIPCUnavailable) {
		t.Errorf("autoRegisterReconcileFn not sealed: err=%v", err)
	}
	// Respawn mirrors DialSupervisorIPCRespawn's contract: populated result, NIL
	// error — the seal must be a non-success populated result, never an error.
	if r, err := supervisorRestartRespawnFn(ctx, "x", false, 1); err != nil || r.Success {
		t.Errorf("supervisorRestartRespawnFn not sealed to a non-success populated result: r=%+v err=%v", r, err)
	}
	if _, err := statusInternalDialFn(ctx); !errors.Is(err, ErrSupervisorIPCUnavailable) {
		t.Errorf("statusInternalDialFn not sealed: err=%v", err)
	}
	if _, err := supervisorIPCStatusFn(ctx); !errors.Is(err, ErrSupervisorIPCUnavailable) {
		t.Errorf("supervisorIPCStatusFn not sealed: err=%v", err)
	}
	if _, ok, err := loopbackPortOwnerFn(9128); ok || err != nil {
		t.Errorf("loopbackPortOwnerFn not sealed to (0,false,nil): ok=%v err=%v", ok, err)
	}
	if err := installAutostartOwnerStartFn(); err == nil {
		t.Error("installAutostartOwnerStartFn not sealed — the default test path would `schtasks /Run` the live supervisor task")
	}
	if _, err := installAutostartBackendFactoryFn(); err == nil {
		t.Error("installAutostartBackendFactoryFn not sealed")
	}
	if err := proxyReadinessFn(9128, time.Second); err == nil {
		t.Error("proxyReadinessFn not sealed — the default test path would HTTP-poll a real port for up to 10s")
	}
	if err := taskkillProcessTreeByPIDFn(0); err == nil {
		t.Error("taskkillProcessTreeByPIDFn not sealed — the default test path could reap a real process tree")
	}
}

// TestNoUnsealedSupervisorIPCDialVar is the AST half of the recurrence guard
// (architect design): it fails the build if a new package-level var defaults to a
// DialSupervisorIPC* function (directly, or via a wrapping func literal) but is
// NOT re-assigned inside TestMain. This closes the gap the runtime half cannot —
// a brand-new dial seam nobody remembered to seal — the same enforcement class as
// port_kill_guard_test.go for derived-port kill sinks.
func TestNoUnsealedSupervisorIPCDialVar(t *testing.T) {
	fset := token.NewFileSet()
	dialVars := map[string]string{} // varName -> file:line

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
					if i >= len(vs.Names) || !refersToDialSupervisorIPC(val) {
						continue
					}
					dialVars[vs.Names[i].Name] = fmt.Sprintf("%s:%d", name, fset.Position(vs.Names[i].Pos()).Line)
				}
			}
		}
	}

	if len(dialVars) == 0 {
		t.Fatal("recurrence guard found ZERO DialSupervisorIPC* seam vars — the AST detection is broken (it should find registerSupervisorReconcileFn et al.)")
	}

	mainSrc, err := os.ReadFile("main_test.go")
	if err != nil {
		t.Fatalf("read main_test.go: %v", err)
	}
	for v, loc := range dialVars {
		if !bytes.Contains(mainSrc, []byte(v+" =")) {
			t.Errorf("supervisor-IPC dial seam %s (%s) defaults to a real DialSupervisorIPC* function but is NOT sealed in TestMain (main_test.go). Add a hermetic default-stub to the seal block so `go test ./internal/api/` never dials the developer's live supervisor.", v, loc)
		}
	}
}

// refersToDialSupervisorIPC reports whether an expression references a
// DialSupervisorIPC* function anywhere (a direct `= DialSupervisorIPCReconcile`
// or a wrapping `= func(...){ ... DialSupervisorIPCReconcile(...) ... }`).
func refersToDialSupervisorIPC(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && strings.HasPrefix(id.Name, "DialSupervisorIPC") {
			found = true
			return false
		}
		return true
	})
	return found
}

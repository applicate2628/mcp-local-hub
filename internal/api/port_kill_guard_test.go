package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoLiveBandLiteralReachesKillOrListenSink is the §8a falsification guard
// for the "test-Port:9200-killed-live-daemon" bug class (v0.6 redesign spec
// §2.x / §15 P2 / §12 Phase 2). It walks the ENTIRE Go source tree — both the
// non-test tree AND the *_test.go tree — and fails if any live-band port
// literal appears as a DIRECT ARGUMENT to a process-kill or port-bind sink.
//
// # Why the test tree is load-bearing (the §15-P2 / fable finding)
//
// The actual incident (CLAUDE.md memory `feedback_common_logic_flexible_defaults_via_gui`,
// "Port:9200 literal → killed live daemon") was a TEST literal that reached the
// real kill path. Go tests run against the developer's REAL state dir unless a
// test sets the `test_state_path_env` override, so a test that feeds a live-band
// port to the real `killByPortFn` → `killDaemonByPort` (`install.go:2660`,
// `:2669`) — which has NO identity gate and TREE-KILLS whatever listens on that
// port via `taskkill /F /T` — can kill a running production daemon on the dev
// machine. The PRE-EXISTING drift-guard in `internal/config/serena_test.go`
// greps only the NON-test tree, so it would NOT have caught the incident. This
// guard closes that gap by scanning the test tree too.
//
// # AST-based, not raw text (PR #282 review findings P2/P3)
//
// The scan parses every .go file with go/parser and inspects only real
// *ast.CallExpr nodes — never raw source lines. This deliberately fixes two
// failure modes of an earlier raw-line+regex version:
//
//   - Comment / string immunity (P2). A doc comment or string literal that
//     mentions a sink shape — e.g. `// call killByPortFn(9200, ...)` or a
//     sample address `"127.0.0.1:9200"` written next to `.ListenAndServe(` —
//     is NOT a call expression, so it can never be flagged. (The earlier
//     raw-text scan false-positived on such comments and hard-failed the whole
//     internal/api package; this team routinely writes such explanatory
//     comments, so that was a latent build-breaker.) Because of this immunity
//     there is no longer any need to self-exclude this very file's docs.
//   - Multi-line / nested-call coverage (P3). A gofmt-splittable sink call
//     whose literal sits on a different physical line (e.g. `killByPortFn(\n
//     9200,\n ...)`) is a single CallExpr in the AST, so it is caught
//     regardless of line layout. A nested `net.JoinHostPort("127.0.0.1",
//     "9200")` inside a listen sink is also caught because we recurse into the
//     listen call's argument subtree, treating both `"host:9200"` and the bare
//     `"9200"` port string as port candidates (range-checked against the live
//     bands).
//
// # Live bands (anchored to real runtime code, not magic numbers)
//
//   - 9121–9132  global daemon ports, incl. the legacy serena 9121/9122 split.
//     Source: configs/ports.yaml (10 globals) + serena_dynamic_pool.go:69
//     ("clears 9121–9132") + defaultLegacySerenaPort = 9121.
//   - 9150–9199  serena dynamic pool. Source: serena_dynamic_pool.go:77-78
//     (serenaDefaultPortPoolStart / serenaDefaultPortPoolEnd).
//   - 9200–9299  LSP workspace-proxy pool. Source: serena_dynamic_pool.go:69
//     comment + servers/mcp-language-server/manifest.yaml `start: 9200`.
//
// Note 9133–9149 is an intentional gap (not a live band), so the guard checks
// two ranges: [9121,9132] and [9150,9299].
//
// # Scope: SINK-SCOPED, deliberately NOT a blanket `9xxx` grep
//
// A blanket grep for any live-band literal false-positives on the ~130 files
// that legitimately carry these numbers as HARMLESS DATA FIXTURES — e.g.
// `DaemonStatus{Port:9100}`, `canonicalDaemonRef{Port:9200}`, manifest YAML
// `port: 9200`, JSON URL strings `http://localhost:9200/mcp`, resolver/status/
// health test data. None of those reach a kill or a real network bind, so the
// guard MUST NOT flag them. Instead it flags ONLY a live-band literal that is a
// DIRECT ARGUMENT (or a nested-call argument) to one of the danger sinks the
// spec names:
//
//   - killByPortFn(<live-band>, ...)            the production kill seam
//   - killDaemonByPort(<live-band>, ...)        the real taskkill /F /T target
//   - net.Listen(..., "...:<live-band>")        a real loopback bind (collision)
//   - <x>.ListenAndServe()-style with a "...:<live-band>" address literal
//   - NewListenerWithSOExclusive[Context]("...:<live-band>")  the production
//     hub-MCP loopback bind (hub_mcp_listener_{windows,posix}.go), called at
//     hub_mcp_bind.go:158 and directly in tests
//   - <x>.Listen(..., "...:<live-band>")        any .Listen-suffixed call,
//     covering the `lc.Listen(ctx, "tcp", addr)` net.ListenConfig form the
//     factory above wraps
//
// The convention this enforces (spec §2.x #3): anything reaching a kill/listen
// sink in a test MUST use `pickFreeLocalPort(t)` (install_test.go:559) or
// `Port: 0` / `:0` — never a live-band literal.
//
// # Known limitation (documented, not a hidden gap)
//
// This guard catches the DIRECT-ARGUMENT (and nested-call-argument) vector. It
// does NOT do data-flow analysis of the indirect vector where a live-band
// literal is stored in a struct field (`WorkspaceEntry{Port: 9200}`) that a
// later unfaked teardown passes to the seam (`killByPortFn(entry.Port, ...)`).
// That indirect vector is not cleanly detectable without false-positiving on
// every data fixture, so it is covered by CONVENTION instead: see the
// load-bearing comment + `Port: 0` discipline in
// internal/cli/workspace_cmd_test.go seedTwoBackends, which keeps the one test
// that uses the REAL `(*api.API).Unregister` teardown
// (TestWorkspaceUnregister_RemovesRegistryRowAndSupervisorIntentDescriptor) on
// a zero port so the `entry.Port != 0` guard skips the kill.
func TestNoLiveBandLiteralReachesKillOrListenSink(t *testing.T) {
	// Resolve the repo root from this test's working directory
	// (go test runs in the package dir internal/api → root is ../../).
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	// Sanity: confirm we actually found the repo root, not some random dir.
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root sanity check failed (no go.mod at %s): %v", root, err)
	}

	// inLiveBand reports whether p falls in a live daemon-port band.
	inLiveBand := func(p int) bool {
		return (p >= 9121 && p <= 9132) || (p >= 9150 && p <= 9299)
	}

	// litPort extracts the integer port from an *ast.BasicLit if it is a
	// live-band literal. Two literal shapes carry a port:
	//   - an INT literal in a kill-sink arg position: killByPortFn(9200, ...)
	//   - a STRING literal address in a listen-sink arg: "127.0.0.1:9200",
	//     ":9200", or a bare "9200" (the JoinHostPort port string). The STRING
	//     branch takes the text after the last ':' (whole string if no ':').
	// Returns (port, true) only when the parsed number is in a live band.
	litPort := func(lit *ast.BasicLit) (int, bool) {
		switch lit.Kind {
		case token.INT:
			p, convErr := strconv.Atoi(lit.Value)
			if convErr != nil {
				return 0, false
			}
			return p, inLiveBand(p)
		case token.STRING:
			s, convErr := strconv.Unquote(lit.Value)
			if convErr != nil {
				return 0, false
			}
			// Take the substring after the last ':' so an address literal of the
			// form "host:port" or ":port" yields the port. When there is no ':'
			// (a bare numeric port string, e.g. the "9200" passed to
			// net.JoinHostPort("127.0.0.1", "9200") inside a listen sink) the
			// whole string is the candidate. Because litPort is only reached for
			// literals inside a kill/listen sink's argument subtree, a bare
			// 4-digit numeric string there is treated as a port candidate; the
			// inLiveBand range-check then filters non-live numbers.
			if idx := strings.LastIndex(s, ":"); idx >= 0 {
				s = s[idx+1:]
			}
			p, convErr := strconv.Atoi(s)
			if convErr != nil {
				return 0, false
			}
			return p, inLiveBand(p)
		}
		return 0, false
	}

	// killSinkName reports whether the called function is a process-kill sink.
	// Both sinks are package-level functions in internal/api, so the callee is
	// a bare *ast.Ident.
	killSinkName := func(fun ast.Expr) bool {
		id, ok := fun.(*ast.Ident)
		if !ok {
			return false
		}
		return id.Name == "killByPortFn" || id.Name == "killDaemonByPort"
	}

	// listenSinkName reports whether the called function is a port-bind sink.
	// Covers the bare-ident factory forms (NewListenerWithSOExclusive[Context])
	// and any selector call whose final element is Listen / ListenAndServe
	// (net.Listen, lc.Listen, srv.ListenAndServe, ...).
	listenSinkName := func(fun ast.Expr) bool {
		switch f := fun.(type) {
		case *ast.Ident:
			return f.Name == "NewListenerWithSOExclusive" ||
				f.Name == "NewListenerWithSOExclusiveContext"
		case *ast.SelectorExpr:
			return f.Sel.Name == "Listen" || f.Sel.Name == "ListenAndServe"
		}
		return false
	}

	// findLiveBandLitInArgs recursively scans a sink call's argument subtree for
	// a live-band literal. Recursion covers nested-call address builders such as
	// net.JoinHostPort("127.0.0.1", "9200") passed to a listen sink. It returns
	// the first matching port found, or (0, false).
	var findLiveBandLitInArgs func(args []ast.Expr) (int, bool)
	findLiveBandLitInArgs = func(args []ast.Expr) (int, bool) {
		for _, arg := range args {
			var found int
			var ok bool
			ast.Inspect(arg, func(n ast.Node) bool {
				if ok {
					return false
				}
				if lit, isLit := n.(*ast.BasicLit); isLit {
					if p, inBand := litPort(lit); inBand {
						found, ok = p, true
						return false
					}
				}
				return true
			})
			if ok {
				return found, true
			}
		}
		return 0, false
	}

	type finding struct {
		path string
		line int
		port int
		kind string
	}
	var findings []finding

	fset := token.NewFileSet()

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip dirs that never hold first-party Go source we own: vendored
			// deps, VCS internals, build/scratch output, and node_modules under
			// the GUI frontend. Keeps the walk fast and avoids scanning code we
			// do not control.
			switch d.Name() {
			case "vendor", ".git", "node_modules", ".scratch", ".codegraph":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// parser.ParseFile is syntax-only (build tags do not affect parsing),
		// so it succeeds on any syntactically-valid Go file regardless of GOOS.
		// A parse failure means the file would not compile anyway — go build /
		// go test already fail loudly on it elsewhere — so we skip it (logged)
		// rather than turn this guard into a second, redundant build-breaker.
		f, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			rel, _ := filepath.Rel(root, path)
			t.Logf("skipping unparseable file %s: %v", rel, parseErr)
			return nil
		}
		rel, _ := filepath.Rel(root, path)

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch {
			case killSinkName(call.Fun):
				if p, found := findLiveBandLitInArgs(call.Args); found {
					findings = append(findings, finding{
						path: rel, line: fset.Position(call.Pos()).Line, port: p, kind: "kill-sink",
					})
				}
			case listenSinkName(call.Fun):
				if p, found := findLiveBandLitInArgs(call.Args); found {
					findings = append(findings, finding{
						path: rel, line: fset.Position(call.Pos()).Line, port: p, kind: "listen-sink",
					})
				}
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repo tree: %v", walkErr)
	}

	if len(findings) > 0 {
		for _, f := range findings {
			t.Errorf("LIVE-BAND PORT %d reaches a %s in %s:%d — use pickFreeLocalPort(t) or :0 / Port:0 instead.",
				f.port, f.kind, f.path, f.line)
		}
		t.Fatalf("%d live-band literal(s) reach a kill/listen sink; a test or prod path could kill/collide with a LIVE production daemon (see §2.x test-Port:9200-killed-live-daemon).", len(findings))
	}
}

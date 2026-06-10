package api

import (
	"os"
	"path/filepath"
	"regexp"
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
// DIRECT ARGUMENT to one of the danger sinks the spec names:
//
//   - killByPortFn(<live-band>, ...)        the production kill seam
//   - killDaemonByPort(<live-band>, ...)    the real taskkill /F /T target
//   - net.Listen(..., "...:<live-band>")    a real loopback bind (collision)
//   - <x>.ListenAndServe()-style with a "...:<live-band>" address literal
//
// The convention this enforces (spec §2.x #3): anything reaching a kill/listen
// sink in a test MUST use `pickFreeLocalPort(t)` (install_test.go:559) or
// `Port: 0` / `:0` — never a live-band literal.
//
// # Known limitation (documented, not a hidden gap)
//
// This guard catches the DIRECT-ARGUMENT vector. It does NOT do data-flow
// analysis of the indirect vector where a live-band literal is stored in a
// struct field (`WorkspaceEntry{Port: 9200}`) that a later unfaked teardown
// passes to the seam (`killByPortFn(entry.Port, ...)`). That indirect vector is
// not cleanly greppable without false-positiving on every data fixture, so it is
// covered by CONVENTION instead: see the load-bearing comment + `Port: 0`
// discipline in internal/cli/workspace_cmd_test.go seedTwoBackends, which keeps
// the one test that uses the REAL `(*api.API).Unregister` teardown
// (TestWorkspaceUnregister_RemovesRegistryRowAndSupervisorIntentDescriptor) on a
// zero port so the `entry.Port != 0` guard skips the kill.
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

	// Sink patterns. Each captures the FIRST live-band-shaped (4-digit, 9xxx)
	// literal sitting in the argument position of a kill/listen sink on a single
	// source line. We then range-check the captured number against the live
	// bands so a future 9000/9999 non-live literal does not false-positive.
	//
	//   killSinkRe   — `killByPortFn(` / `killDaemonByPort(` immediately followed
	//                  by a numeric literal first argument.
	//   listenSinkRe — `net.Listen(` / `.ListenAndServe(` with a `:<port>` or
	//                  `host:<port>` address literal anywhere in the call's
	//                  remaining same-line text (covers `"127.0.0.1:9200"`;
	//                  `fmt.Sprintf("127.0.0.1:%d", ...)` is variable-port so it
	//                  is correctly NOT matched).
	killSinkRe := regexp.MustCompile(`\bkill(?:ByPortFn|DaemonByPort)\(\s*(\d{4})\b`)
	listenSinkRe := regexp.MustCompile(`(?:\bnet\.Listen|\.ListenAndServe)\([^)]*:(\d{4})\b`)

	// selfBase is this guard file; it is excluded from the scan because its own
	// documentation contains live-band literals (9121, 9200, ...) that are not
	// real sinks.
	const selfBase = "port_kill_guard_test.go"

	type finding struct {
		path string
		line int
		text string
		port int
		kind string
	}
	var findings []finding

	checkLine := func(re *regexp.Regexp, kind, path string, lineNo int, line string) {
		for _, m := range re.FindAllStringSubmatch(line, -1) {
			p, convErr := strconv.Atoi(m[1])
			if convErr != nil {
				continue
			}
			if inLiveBand(p) {
				findings = append(findings, finding{
					path: path, line: lineNo, text: strings.TrimSpace(line), port: p, kind: kind,
				})
			}
		}
	}

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
		if filepath.Base(path) == selfBase {
			return nil // self-exclude (this file's docs carry band literals)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(data), "\n") {
			checkLine(killSinkRe, "kill-sink", rel, i+1, line)
			checkLine(listenSinkRe, "listen-sink", rel, i+1, line)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repo tree: %v", walkErr)
	}

	if len(findings) > 0 {
		for _, f := range findings {
			t.Errorf("LIVE-BAND PORT %d reaches a %s in %s:%d — use pickFreeLocalPort(t) or :0 / Port:0 instead.\n    %s",
				f.port, f.kind, f.path, f.line, f.text)
		}
		t.Fatalf("%d live-band literal(s) reach a kill/listen sink; a test or prod path could kill/collide with a LIVE production daemon (see §2.x test-Port:9200-killed-live-daemon).", len(findings))
	}
}

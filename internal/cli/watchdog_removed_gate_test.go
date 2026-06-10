package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGateD_NoWatchdogSymbolSurvivesInLiveCode is the v0.6 Phase D gate
// (spec §5.x Phase D falsification test): after the watchdog engine is
// deleted, NO watchdog-engine symbol may appear in live Go code anywhere in
// the tree. Doc references are allowed (and the docs are updated in the same
// change); comments and the intentional `LegacyWatchdogTaskName` cleanup
// constant are exempt.
//
// The test walks the repo's .go files and fails if any forbidden symbol
// appears on a non-comment line. It is the cheap, portable form of the
// `go tool nm` zero-watchdog-symbol assertion (which needs a built binary):
// a surviving reference would mean Phase D is incomplete (the engine half-
// deleted, or a reader still wired to the deleted recovery path).
func TestGateD_NoWatchdogSymbolSurvivesInLiveCode(t *testing.T) {
	repoRoot := findRepoRoot(t)

	// Forbidden watchdog-engine identifiers. Word-bounded so substrings of a
	// surviving allowed name do not false-match. LegacyWatchdogTaskName /
	// legacyWatchdogTaskName (the existing-host cleanup constant) are NOT
	// forbidden — they are the deliberate Phase C survivors.
	forbidden := regexp.MustCompile(`\b(?:` + strings.Join([]string{
		"WatchdogTaskName",           // standalone; Legacy* / legacy* excluded below
		"InstallWatchdogTask",        // covers UninstallWatchdogTask(Internal)
		"UninstallWatchdogTask",      //
		"RecoverStoppedDaemons",      //
		"BuildWatchdogXML",           // covers buildWatchdogXML
		"buildWatchdogXML",           //
		"AppendWatchdogLog",          //
		"WatchdogLogEntry",           //
		"ReadWatchdogLogTail",        //
		"ReadIntentAuditTail",        // watchdog-status-only reader, deleted with cli/watchdog.go
		"newWatchdogCmd",             //
		"RecoveryDecision",           //
		"newCooldownEngine",          //
		"RestartContextWithSnapshot", //
		"WaitDaemonRunning",          // watchdog-driver-only helper
		"NewOwnedXMLValidatorFromSnapshot",
		"LoadDaemonRegistry",
		"LoadOwnershipSnapshot",
		"watchdogXMLEscape",
	}, "|") + `)\b`)

	// Allowed survivors that the word-boundary regex above would otherwise
	// match (they legitimately contain a forbidden substring).
	allowed := []string{
		"LegacyWatchdogTaskName",
		"legacyWatchdogTaskName",
	}

	walkErr := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip unreadable entries; not the gate's concern
		}
		if info.IsDir() {
			// Skip vendor + VCS + scratch dirs.
			base := info.Name()
			if base == ".git" || base == "vendor" || base == ".scratch" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Exempt THIS gate file (it names the forbidden symbols on purpose).
		if filepath.Base(path) == "watchdog_removed_gate_test.go" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			// Skip pure comment lines (doc references are allowed).
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			loc := forbidden.FindStringIndex(line)
			if loc == nil {
				continue
			}
			matched := line[loc[0]:loc[1]]
			// The word-boundary match starts inside an allowed identifier
			// (e.g. LegacyWatchdogTaskName ends with WatchdogTaskName). Skip
			// if the surrounding token is an allowed survivor.
			if isWithinAllowed(line, loc[0], allowed) {
				continue
			}
			rel, _ := filepath.Rel(repoRoot, path)
			t.Errorf("Gate-D: forbidden watchdog symbol %q survives in live code at %s:%d\n  %s",
				matched, filepath.ToSlash(rel), i+1, trimmed)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("tree walk: %v", walkErr)
	}
}

// isWithinAllowed reports whether the forbidden match at byte offset `at`
// in `line` is actually part of one of the allowed survivor identifiers.
func isWithinAllowed(line string, at int, allowed []string) bool {
	for _, a := range allowed {
		idx := 0
		for {
			j := strings.Index(line[idx:], a)
			if j < 0 {
				break
			}
			start := idx + j
			end := start + len(a)
			if at >= start && at < end {
				return true
			}
			idx = end
		}
	}
	return false
}

// findRepoRoot walks up from the cwd until it finds go.mod, the repo root.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("findRepoRoot: go.mod not found walking up from cwd")
		}
		dir = parent
	}
}

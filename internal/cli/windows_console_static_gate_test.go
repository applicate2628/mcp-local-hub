package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWindowsSingleConsoleControlStaticGate(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve static-gate source path")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	roots := []string{"cmd", "internal", "npm", ".github", "README.md", "INSTALL.md", "CLAUDE.md", "build.ps1", "build.sh"}

	functionalOwners := 0
	markerResidues := 0
	alternateControls := 0
	childPropagations := 0
	var findings []string

	visit := func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == "node_modules" || base == "packages" && strings.Contains(filepath.ToSlash(path), "/npm/packages/") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repo, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasSuffix(rel, "_test.go") || strings.HasSuffix(rel, ".test.js") {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(rel))
		if ext != ".go" && ext != ".js" && ext != ".md" && ext != ".yml" && ext != ".yaml" && ext != ".ps1" && ext != ".sh" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		functionalOwners += strings.Count(text, `const WindowsDebugConsolePrefix = "--debug-console"`)

		for _, marker := range []string{"MCPHUB_NO_CONSOLE_ATTACH", "SuppressConsoleAttach", "ConsoleAttachSuppressed", "attachParentConsoleIfAvailable"} {
			if n := strings.Count(text, marker); n > 0 {
				markerResidues += n
				findings = append(findings, fmt.Sprintf("marker %s x%d in %s", marker, n, rel))
			}
		}

		if strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, ".js") {
			for _, shape := range []string{
				`"debug_console"`, `"debug-console"`, "MCPHUB_DEBUG_CONSOLE",
				`.Bool("debug-console"`, `.BoolVar("debug-console"`, `.String("debug-console"`, `.StringVar("debug-console"`,
			} {
				if n := strings.Count(text, shape); n > 0 {
					allowedOwnerLiteral := rel == "internal/cli/windows_console_policy.go" && shape == `"debug-console"`
					if !allowedOwnerLiteral {
						alternateControls += n
						findings = append(findings, fmt.Sprintf("alternate control %q x%d in %s", shape, n, rel))
					}
				}
			}
			if strings.Contains(text, "WindowsDebugConsolePrefix") &&
				rel != "internal/cli/windows_console_policy.go" &&
				rel != "internal/cli/windows_console_policy_windows.go" &&
				rel != "internal/cli/windows_console_policy_other.go" {
				childPropagations++
				findings = append(findings, "console token propagated into "+rel)
			}
		}
		return nil
	}

	for _, root := range roots {
		path := filepath.Join(repo, filepath.FromSlash(root))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat allowlisted live root %s: %v", root, err)
		}
		if info.IsDir() {
			if err := filepath.Walk(path, visit); err != nil {
				t.Fatalf("scan %s: %v", root, err)
			}
		} else if err := visit(path, info, nil); err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
	}

	t.Logf("console static counts functional-owner/marker/alternate/child = %d/%d/%d/%d", functionalOwners, markerResidues, alternateControls, childPropagations)
	if functionalOwners != 1 || markerResidues != 0 || alternateControls != 0 || childPropagations != 0 {
		t.Fatalf("console static counts=%d/%d/%d/%d, want 1/0/0/0; findings=%v", functionalOwners, markerResidues, alternateControls, childPropagations, findings)
	}
}

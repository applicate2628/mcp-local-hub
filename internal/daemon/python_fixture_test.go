package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Integration fixtures need an installed interpreter, not the Windows Store
// execution alias. An explicit absolute path keeps CI independent of PATH.
func daemonTestPython(t *testing.T) string {
	t.Helper()
	name := os.Getenv("MCPHUB_TEST_PYTHON")
	if name == "" {
		name = "python"
	} else if !filepath.IsAbs(name) {
		t.Fatal("MCPHUB_TEST_PYTHON must name an absolute installed Python interpreter")
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("Python fixture prerequisite: set MCPHUB_TEST_PYTHON to an installed interpreter: %v", err)
	}
	if runtime.GOOS == "windows" && strings.Contains(strings.ToLower(filepath.ToSlash(path)), "/windowsapps/") {
		t.Fatal("Python fixture prerequisite: Windows Store alias is not an interpreter; set MCPHUB_TEST_PYTHON")
	}
	return path
}

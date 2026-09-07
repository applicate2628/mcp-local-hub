package archguard

import "testing"

func TestTestOnlyBuildSourceRequiresConfiguredTagReference(t *testing.T) {
	if isTestOnlyBuildSource([]byte("//go:build windows && !windows\n\npackage x\n"), []string{"test_state_path_env"}) {
		t.Fatal("an unrelated unsatisfiable expression must not be classified as test-only")
	}
}

func TestTestOnlyBuildSourceRecognizesConjunction(t *testing.T) {
	if !isTestOnlyBuildSource([]byte("//go:build test_state_path_env && windows\n\npackage x\n"), []string{"test_state_path_env"}) {
		t.Fatal("a file requiring the configured test tag must be classified as test-only")
	}
}

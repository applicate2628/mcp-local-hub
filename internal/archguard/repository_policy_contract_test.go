package archguard

import (
	"path/filepath"
	"testing"
)

func TestRepositoryPolicyDoesNotAllowMutableGlobalsByName(t *testing.T) {
	root := repositoryContractRoot(t)
	policy, err := LoadPolicy(filepath.Join(root, "architecture", "policy.yaml"))
	if err != nil {
		t.Fatalf("load repository architecture policy: %v", err)
	}
	if len(policy.AllowedGlobalNamePatterns) != 0 {
		t.Fatalf(
			"allowed_global_name_patterns must stay empty; baseline exact fingerprints instead, got %v",
			policy.AllowedGlobalNamePatterns,
		)
	}
}

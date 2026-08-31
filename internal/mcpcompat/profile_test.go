package mcpcompat

import "testing"

func TestResolveProtocolCompatibilityProfile(t *testing.T) {
	tests := []struct {
		name        string
		profile     string
		versions    []string
		unsupported string
		wantErr     bool
	}{
		{
			name:        "zero value is strict",
			versions:    []string{"2025-11-25", "2025-06-18", "2025-03-26"},
			unsupported: "2024-11-05",
		},
		{
			name:        "legacy stdio profile adds only 2024",
			profile:     "stdio-http-legacy-2024-11-05",
			versions:    []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
			unsupported: "1900-01-01",
		},
		{name: "unknown profile", profile: "legacy-all-versions", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := ResolveProtocolCompatibilityProfile(tt.profile)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ResolveProtocolCompatibilityProfile unexpectedly succeeded")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveProtocolCompatibilityProfile(%q): %v", tt.profile, err)
			}
			for _, version := range tt.versions {
				if !policy.Supports(version) {
					t.Errorf("policy %q does not support %q", tt.profile, version)
				}
			}
			if policy.Supports(tt.unsupported) {
				t.Errorf("policy %q unexpectedly supports %q", tt.profile, tt.unsupported)
			}
		})
	}
}

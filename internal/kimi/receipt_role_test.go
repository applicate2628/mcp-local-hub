package kimi

import (
	"strings"
	"testing"
)

func TestProviderReceiptBindsRequestedRoleAndExecutionLane(t *testing.T) {
	for _, tc := range []struct {
		name, requested, assigned, execution string
		valid bool
	}{
		{"legacy direct reviewer", "external-reviewer", "external-reviewer", "external-reviewer", true},
		{"reviewer", "qa-engineer", "qa-engineer", "external-reviewer", true},
		{"worker", "backend-engineer", "backend-engineer", "external-worker", true},
		{"consultant", "consultant", "consultant", "consultant", true},
		{"wrong assignment same lane", "qa-engineer", "security-reviewer", "external-reviewer", false},
		{"wrong lane", "qa-engineer", "qa-engineer", "external-worker", false},
		{"worker promoted to reviewer", "backend-engineer", "backend-engineer", "external-reviewer", false},
		{"unknown role", "invented-role", "invented-role", "external-worker", false},
		{"missing requested role", "", "external-reviewer", "external-reviewer", false},
		{"unsupported role", "lead", "lead", "none", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := ownerReceiptRecord(t, "high", "result")
			record = strings.Replace(record, `"assignedRole":"external-reviewer"`, `"assignedRole":"`+tc.assigned+`"`, 1)
			record = strings.Replace(record, `"executionRole":"external-reviewer"`, `"executionRole":"`+tc.execution+`"`, 1)
			got, err := parseProviderReceipt([]byte(record), "high", tc.requested, CapabilityChoice{Kind: "none"})
			if (err == nil) != tc.valid {
				t.Fatalf("receipt validity = %v, want %v: %v", err == nil, tc.valid, err)
			}
			if tc.valid && got.Payload != strings.TrimSuffix(strings.TrimPrefix(record, providerResultPrefix), "\n") {
				t.Fatal("role validation rewrote the authoritative payload")
			}
		})
	}
}

package kimi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderReceiptRequiresMatchingPrimaryOutcome(t *testing.T) {
	for _, failing := range []bool{false, true} {
		for _, field := range []string{"matching", "exitCode", "status", "gate"} {
			name := "success/" + field
			if failing {
				name = "failure/" + field
			}
			t.Run(name, func(t *testing.T) {
				record := ownerReceiptRecord(t, "high", "result")
				var payload map[string]any
				if err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(record, providerResultPrefix), "\n")), &payload); err != nil {
					t.Fatal(err)
				}
				primary := payload["primaryOutcome"].(map[string]any)
				if failing {
					for key, value := range map[string]any{"exitCode": 1, "status": "failed", "gate": "FAIL"} {
						payload[key], primary[key] = value, value
					}
				}
				if field != "matching" {
					value := map[string]any{"exitCode": 1, "status": "failed", "gate": "FAIL"}[field]
					if failing {
						value = map[string]any{"exitCode": 0, "status": "completed", "gate": "PASS"}[field]
					}
					primary[field] = value
				}
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				got, err := parseProviderReceipt([]byte(providerResultPrefix+string(raw)+"\n"), "high", "external-reviewer", CapabilityChoice{Kind: "none"})
				if field != "matching" {
					if err == nil {
						t.Fatal("contradictory primary outcome accepted")
					}
					return
				}
				if err != nil || got.IsError() != failing || got.Payload != string(raw) {
					t.Fatalf("matching outcome: receipt=%+v error=%v", got, err)
				}
			})
		}
	}
}

func TestProviderReceiptFailedGateIsErrorEvenWithSuccessfulExit(t *testing.T) {
	for _, gate := range []string{"PASS", "FAIL", "BLOCKED", "unknown"} {
		t.Run(gate, func(t *testing.T) {
			record := ownerReceiptRecord(t, "high", "result")
			// Change both top-level and primaryOutcome gates, keeping them consistent.
			record = strings.ReplaceAll(record, `"gate":"PASS"`, `"gate":"`+gate+`"`)
			got, err := parseProviderReceipt([]byte(record), "high", "external-reviewer", CapabilityChoice{Kind: "none"})
			if err != nil {
				t.Fatal(err)
			}
			if got.IsError() != (gate != "PASS") {
				t.Fatalf("gate %q with exit=0/completed: IsError=%v", gate, got.IsError())
			}
		})
	}
}

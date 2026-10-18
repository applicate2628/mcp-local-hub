package kimi

import (
	"encoding/json"
	"strings"
	"testing"
)

func ownerReceiptRecord(t *testing.T, effort, resultText string) string {
	t.Helper()
	payload := map[string]any{
		"schema":                          providerResultSchemaV2,
		"provider":                        "kimi",
		"model":                           "kimi-code/k3",
		"effort":                          effort,
		"resultText":                      resultText,
		"exitCode":                        0,
		"status":                          "completed",
		"gate":                            "PASS",
		"cancelled":                       false,
		"timedOut":                        false,
		"cleanupStatus":                   "complete",
		"cleanupIssueCount":               0,
		"captureRecoveryRetained":         false,
		"authorizing":                     false,
		"closesRunIds":                    []string{},
		"independentVerificationRequired": true,
		"terminalClass":                   "external-nonauthorizing",
		"actualExecutionPath":             "direct-external-cli",
		"assignedRole":                    "external-reviewer",
		"executionRole":                   "external-reviewer",
		"primaryOutcome": map[string]any{
			"exitCode": 0,
			"status":   "completed",
			"gate":     "PASS",
		},
		"launchFlags": []string{},
		"selected": map[string]any{
			"tools":       []string{},
			"mcpNames":    []string{},
			"subagents":   []string{},
			"permission":  "reject",
			"cwdSelected": false,
		},
		"observed": map[string]any{
			"toolCalls":           []any{},
			"permissionDecisions": []any{},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return providerResultPrefix + string(raw) + "\n"
}

func TestParseProviderReceiptPreservesOwnerPayloadAndEffort(t *testing.T) {
	for _, effort := range []string{"high", "max"} {
		t.Run(effort, func(t *testing.T) {
			record := ownerReceiptRecord(t, effort, "synthetic result")
			got, err := parseProviderReceipt([]byte(record), effort, "external-reviewer", CapabilityChoice{Kind: "none"})
			if err != nil {
				t.Fatalf("parseProviderReceipt: %v", err)
			}
			if got.Payload != strings.TrimSuffix(strings.TrimPrefix(record, providerResultPrefix), "\n") {
				t.Fatal("authoritative JSON payload was not returned byte-for-byte")
			}
			if got.Effort != effort || got.ResultText != "synthetic result" || got.IsError() {
				t.Fatalf("receipt = %+v", got)
			}
		})
	}
}

func TestParseProviderReceiptRejectsMalformedOrMismatchedProvenance(t *testing.T) {
	valid := ownerReceiptRecord(t, "high", "synthetic result")
	tests := map[string]string{
		"missing newline":          strings.TrimSuffix(valid, "\n"),
		"extra line":               valid + "extra\n",
		"trailing JSON whitespace": strings.TrimSuffix(valid, "\n") + " \n",
		"bad prefix":               "OTHER=" + strings.TrimPrefix(valid, providerResultPrefix),
		"bad schema":               strings.Replace(valid, providerResultSchemaV2, "orchestrarium.provider-result.v1", 1),
		"bad provider":             strings.Replace(valid, `"provider":"kimi"`, `"provider":"other"`, 1),
		"bad model":                strings.Replace(valid, `"model":"kimi-code/k3"`, `"model":"other"`, 1),
		"bad effort":               strings.Replace(valid, `"effort":"high"`, `"effort":"max"`, 1),
		"launch flags":             strings.Replace(valid, `"launchFlags":[]`, `"launchFlags":["--model","other"]`, 1),
		"selected extra":           strings.Replace(valid, `"cwdSelected":false`, `"cwdSelected":false,"extra":true`, 1),
		"observed invalid status":  strings.Replace(valid, `"toolCalls":[]`, `"toolCalls":[{"id":"x","title":"tool","status":"unknown"}]`, 1),
		"authorizing":              strings.Replace(valid, `"authorizing":false`, `"authorizing":true`, 1),
		"nonempty none":            strings.Replace(valid, `"tools":[]`, `"tools":["Read"]`, 1),
	}
	for name, record := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseProviderReceipt([]byte(record), "high", "external-reviewer", CapabilityChoice{Kind: "none"}); err == nil {
				t.Fatal("invalid receipt accepted")
			}
		})
	}
}

func TestProviderReceiptIsErrorUsesStructuralOutcomeNotResultText(t *testing.T) {
	record := ownerReceiptRecord(t, "high", "text that contains the word failure but is legitimate")
	got, err := parseProviderReceipt([]byte(record), "high", "external-reviewer", CapabilityChoice{Kind: "none"})
	if err != nil || got.IsError() {
		t.Fatalf("legitimate result text changed success: got=%+v err=%v", got, err)
	}
	record = strings.Replace(record, `"cleanupStatus":"complete"`, `"cleanupStatus":"incomplete"`, 1)
	got, err = parseProviderReceipt([]byte(record), "high", "external-reviewer", CapabilityChoice{Kind: "none"})
	if err != nil || !got.IsError() {
		t.Fatalf("incomplete cleanup did not set tool error: got=%+v err=%v", got, err)
	}
}

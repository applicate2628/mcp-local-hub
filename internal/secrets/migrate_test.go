package secrets

import (
	"strings"
	"testing"
)

func TestScanCodexConfig_FindsApiKey(t *testing.T) {
	toml := `[mcp_servers.wolfram.env]
WOLFRAM_LLM_APP_ID = "ABCDEF123456"
`
	candidates := ScanConfigText(toml)
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d: %+v", len(candidates), candidates)
	}
	if candidates[0].Key != "WOLFRAM_LLM_APP_ID" {
		t.Errorf("Key = %q, want WOLFRAM_LLM_APP_ID", candidates[0].Key)
	}
	if candidates[0].Value != "ABCDEF123456" {
		t.Errorf("Value = %q, want ABCDEF123456", candidates[0].Value)
	}
}

func TestScanConfigText_SkipsPlaceholders(t *testing.T) {
	toml := `
KEY_1 = "CONTEXT7_API_KEY"
KEY_2 = "your-api-key-here"
`
	candidates := ScanConfigText(toml)
	for _, c := range candidates {
		if strings.ToUpper(c.Value) == c.Key {
			t.Errorf("placeholder %q should be skipped", c.Value)
		}
		if strings.Contains(strings.ToLower(c.Value), "your-api-key") {
			t.Errorf("placeholder-style %q should be skipped", c.Value)
		}
	}
}

package secrets

import (
	"regexp"
	"strings"
)

// Candidate is a plausible secret value found in an MCP client config file.
// Callers typically show candidates to the user interactively and let them
// approve each import into the vault.
type Candidate struct {
	Key   string // e.g., "WOLFRAM_LLM_APP_ID"
	Value string // e.g., "ABCDEF123456"
}

// secretLineRe matches lines that look like `KEY = "VALUE"` (TOML) or `"KEY": "VALUE"` (JSON).
// The key is captured as group 1, the value as group 2.
var secretLineRe = regexp.MustCompile(`(?m)["\s]*([A-Z][A-Z0-9_]*(?:API_KEY|TOKEN|SECRET|APP_ID|PASSWORD|PASS))["\s]*[:=]\s*"([^"]+)"`)

// ScanConfigText examines raw TOML or JSON text and returns secret-shaped
// key/value pairs. Placeholder values (equal to their own key, containing
// common scaffolding strings like "your-", "example", "changeme") are filtered.
func ScanConfigText(text string) []Candidate {
	var out []Candidate
	for _, match := range secretLineRe.FindAllStringSubmatch(text, -1) {
		key := match[1]
		value := match[2]
		if isPlaceholder(key, value) {
			continue
		}
		out = append(out, Candidate{Key: key, Value: value})
	}
	return out
}

func isPlaceholder(key, value string) bool {
	lv := strings.ToLower(value)
	if strings.EqualFold(value, key) {
		return true
	}
	for _, needle := range []string{"your-", "your_", "example", "changeme", "replace-me", "<", ">", "xxx"} {
		if strings.Contains(lv, needle) {
			return true
		}
	}
	return false
}

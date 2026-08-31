// Package mcpcompat owns opt-in MCP protocol compatibility profiles.
package mcpcompat

import "fmt"

const LegacyStdioHTTP20241105Profile = "stdio-http-legacy-2024-11-05"

// ProtocolCompatibilityPolicy is the resolved protocol allowlist for one
// daemon. Its zero value is deliberately strict: it accepts only the current
// Streamable HTTP protocol versions.
type ProtocolCompatibilityPolicy struct {
	legacy20241105 bool
}

// ResolveProtocolCompatibilityProfile resolves the manifest-selected profile
// once at composition time. Empty preserves strict current behavior.
func ResolveProtocolCompatibilityProfile(profile string) (ProtocolCompatibilityPolicy, error) {
	switch profile {
	case "":
		return ProtocolCompatibilityPolicy{}, nil
	case LegacyStdioHTTP20241105Profile:
		return ProtocolCompatibilityPolicy{legacy20241105: true}, nil
	default:
		return ProtocolCompatibilityPolicy{}, fmt.Errorf("unknown MCP protocol compatibility profile %q", profile)
	}
}

// Supports reports whether version is admitted by this resolved profile.
func (p ProtocolCompatibilityPolicy) Supports(version string) bool {
	switch version {
	case "2025-11-25", "2025-06-18", "2025-03-26":
		return true
	case "2024-11-05":
		return p.legacy20241105
	default:
		return false
	}
}

package lastfailure

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

const redactedCommand = "REDACTED"

var commandURLRE = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s"']+`)

// redactResultCommands is the final owner for every command-shaped public
// field. A credential-bearing command is not reproducible without the secret,
// so publishing a partial approximation is less useful than an explicit,
// fail-closed redaction.
func redactResultCommands(result Result) Result {
	result.ExactCommand = redactCommandForWire(result.ExactCommand)
	result.BuildCommand = redactCommandForWire(result.BuildCommand)
	if len(result.Evidence.Commands) != 0 {
		result.Evidence.Commands = append([]string(nil), result.Evidence.Commands...)
		for i := range result.Evidence.Commands {
			result.Evidence.Commands[i] = redactCommandForWire(result.Evidence.Commands[i])
		}
	}
	return result
}

func redactCommandForWire(command string) string {
	if command == "" || !commandCarriesCredential(command) {
		return command
	}
	return redactedCommand
}

func commandCarriesCredential(command string) bool {
	for _, raw := range commandURLRE.FindAllString(command, -1) {
		if u, err := url.Parse(raw); err == nil {
			if u.User != nil {
				return true
			}
			for _, values := range u.Query() {
				for _, value := range values {
					if value != "" {
						return true
					}
				}
			}
		} else if strings.Contains(raw, "@") {
			return true
		}
	}

	fields := strings.Fields(command)
	for i, field := range fields {
		field = strings.Trim(field, `"'`)
		isOption := strings.HasPrefix(field, "-") && field != "-"
		key, value, hasValue := strings.Cut(field, "=")
		key = strings.TrimLeft(key, "-/")
		if hasValue && value != "" {
			if credentialCommandKey(key) || !safeCommandValueKey(key) {
				return true
			}
			continue
		}
		if !hasValue && isOption && i+1 < len(fields) {
			if safeCommandFlagKey(key) {
				continue
			}
			next := strings.Trim(fields[i+1], `"'`)
			if next != "" && !strings.HasPrefix(next, "-") &&
				(credentialCommandKey(key) || !safeCommandValueKey(key)) {
				return true
			}
		}
	}
	return false
}

// safeCommandFlagKey lists value-less flags whose following token remains a
// positional argument. Without this distinction, ninja's ordinary "-v all"
// build command would be mistaken for an unsafe option/value pair.
func safeCommandFlagKey(key string) bool {
	switch strings.ToLower(strings.TrimLeft(key, "-/")) {
	case "v", "verbose":
		return true
	default:
		return false
	}
}

// safeCommandValueKey is deliberately a small allowlist of public vcpkg
// routing/context fields. Every other value-bearing assignment or option is
// redacted: an open-ended denylist cannot classify tool-specific credentials.
func safeCommandValueKey(key string) bool {
	var normalized strings.Builder
	for _, r := range strings.ToLower(key) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			normalized.WriteRune(r)
		}
	}
	switch normalized.String() {
	case "triplet", "hosttriplet", "overlayports", "overlaytriplets",
		"xbuildtreesroot", "xpackagesroot", "xinstallroot", "downloadsroot":
		return true
	default:
		return false
	}
}

func credentialCommandKey(key string) bool {
	var normalized strings.Builder
	for _, r := range strings.ToLower(key) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			normalized.WriteRune(r)
		}
	}
	name := normalized.String()
	switch name {
	case "token", "accesstoken", "refreshtoken", "secret", "clientsecret",
		"password", "passwd", "pwd", "key", "apikey", "xapikey",
		"privatekey", "accesskey", "auth", "authorization", "credential",
		"signature", "sig", "jwt", "assertion", "pat", "session", "sid", "ticket":
		return true
	default:
		for _, marker := range []string{"token", "secret", "password", "passwd", "apikey",
			"privatekey", "accesskey", "auth", "credential", "signature", "jwt",
			"assertion", "session", "ticket"} {
			if strings.Contains(name, marker) {
				return true
			}
		}
		return strings.HasSuffix(name, "pat")
	}
}

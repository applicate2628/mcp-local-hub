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
			for key, values := range u.Query() {
				if credentialCommandKey(key) {
					for _, value := range values {
						if value != "" {
							return true
						}
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
		key, value, hasValue := strings.Cut(field, "=")
		key = strings.TrimLeft(key, "-/")
		if credentialCommandKey(key) {
			if hasValue && value != "" {
				return true
			}
			if !hasValue && i+1 < len(fields) && strings.Trim(fields[i+1], `"'`) != "" {
				return true
			}
		}
	}
	return false
}

func credentialCommandKey(key string) bool {
	var normalized strings.Builder
	for _, r := range strings.ToLower(key) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			normalized.WriteRune(r)
		}
	}
	switch normalized.String() {
	case "token", "accesstoken", "refreshtoken", "secret", "clientsecret",
		"password", "passwd", "pwd", "key", "apikey", "xapikey",
		"privatekey", "accesskey", "auth", "authorization", "credential",
		"signature", "sig", "jwt", "assertion", "pat", "session", "sid", "ticket":
		return true
	default:
		return false
	}
}

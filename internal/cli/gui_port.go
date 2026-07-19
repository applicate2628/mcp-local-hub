package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"mcp-local-hub/internal/api"

	"github.com/spf13/pflag"
)

type guiPortIntentKind string

const (
	guiPortIntentUnset   guiPortIntentKind = "unset"
	guiPortIntentValid   guiPortIntentKind = "valid"
	guiPortIntentInvalid guiPortIntentKind = "invalid"
)

type guiPortInvalidReason string

const (
	guiPortInvalidNotInteger guiPortInvalidReason = "not-an-integer"
	guiPortInvalidOutOfRange guiPortInvalidReason = "out-of-range"
)

type guiPortIntent struct {
	Kind   guiPortIntentKind
	Port   int
	Raw    string
	Reason guiPortInvalidReason
}

type guiPortFallbackSource string

const (
	guiPortFallbackExplicitFlag guiPortFallbackSource = "explicit-flag"
	guiPortFallbackEphemeral    guiPortFallbackSource = "ephemeral"
)

// validPersistedGUIPort is the single owner of the persisted GUI-port range.
// Callers consume the typed classification rather than repeating this check.
func validPersistedGUIPort(port int) bool {
	return port >= 1024 && port <= 65535
}

// validateExplicitGUIPortFlag rejects an explicit `--port` outside the
// supported [1024,65535] range (0 = auto-pick an ephemeral port is allowed).
// It is the front-door counterpart of validPersistedGUIPort: the persisted
// config already forbids privileged ports (<1024), and the restart-handoff
// protocol (validateSelfRestartHandoff, the readiness identity gate) only
// carries [1024,65535], so a GUI launched on a privileged port via the flag
// could start but never restart. Enforcing the same range on the flag keeps a
// running GUI on a port every downstream owner can carry — closing the bot
// #563 privileged-port restart inconsistency at the single point of entry
// rather than lifting the floor across the whole handoff protocol.
func validateExplicitGUIPortFlag(flagChanged bool, port int) error {
	if !flagChanged || port == 0 {
		return nil
	}
	if !validPersistedGUIPort(port) {
		return fmt.Errorf("gui --port %d is outside the supported range [1024,65535] (0 = auto-pick an ephemeral port)", port)
	}
	return nil
}

func classifyPersistedGUIPort(raw string) guiPortIntent {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return guiPortIntent{Kind: guiPortIntentUnset}
	}
	port, err := strconv.Atoi(trimmed)
	if err != nil {
		return guiPortIntent{Kind: guiPortIntentInvalid, Raw: trimmed, Reason: guiPortInvalidNotInteger}
	}
	if !validPersistedGUIPort(port) {
		return guiPortIntent{Kind: guiPortIntentInvalid, Raw: trimmed, Reason: guiPortInvalidOutOfRange}
	}
	return guiPortIntent{Kind: guiPortIntentValid, Port: port}
}

// resolveGuiPort preserves manual-launch precedence: an explicit flag wins,
// otherwise valid persisted intent wins, otherwise the OS picks an ephemeral
// port. Persisted zero is Invalid, not Unset and not Valid.
func resolveGuiPort(flagChanged bool, flagValue int, settingValue string) int {
	if flagChanged {
		return flagValue
	}
	intent := classifyPersistedGUIPort(settingValue)
	if intent.Kind == guiPortIntentValid {
		return intent.Port
	}
	return 0
}

// RebuildSelfRestartArgv removes inherited GUI --port spans only when valid
// persisted intent exists. It consults the actual GUI pflag metadata for the
// registered long flag, preserves the terminator tail byte-for-byte, and does
// not own or repeat the persisted-port range predicate.
//
// This foundation helper is intentionally not called by the live v1 restart
// path. The gated handoff coordinator will wire it in a later phase.
func RebuildSelfRestartArgv(argv []string, flags *pflag.FlagSet, intent guiPortIntent) ([]string, error) {
	if flags == nil {
		return nil, fmt.Errorf("rebuild self-restart argv: nil GUI flag set")
	}
	portFlag := flags.Lookup("port")
	if portFlag == nil || portFlag.Name != "port" {
		return nil, fmt.Errorf("rebuild self-restart argv: GUI flag set has no registered long --port flag")
	}
	if portFlag.Value.Type() != "int" || portFlag.NoOptDefVal != "" {
		return nil, fmt.Errorf("rebuild self-restart argv: unsupported --port metadata type=%q no-opt=%q", portFlag.Value.Type(), portFlag.NoOptDefVal)
	}

	dropPort := intent.Kind == guiPortIntentValid
	rebuilt := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		token := argv[i]
		if token == "--" {
			rebuilt = append(rebuilt, argv[i:]...)
			return rebuilt, nil
		}
		if token == "-port" || strings.HasPrefix(token, "-port=") {
			return nil, fmt.Errorf("unknown shorthand flag: 'p' in %s", token)
		}
		if token == "--port" {
			if i+1 >= len(argv) || argv[i+1] == "--" {
				return nil, fmt.Errorf("flag needs an argument: --port")
			}
			value := argv[i+1]
			if _, err := strconv.ParseInt(value, 0, 64); err != nil {
				return nil, fmt.Errorf("invalid argument %q for --port: %w", value, err)
			}
			if !dropPort {
				rebuilt = append(rebuilt, token, value)
			}
			i++
			continue
		}
		if strings.HasPrefix(token, "--port=") {
			value := strings.TrimPrefix(token, "--port=")
			if _, err := strconv.ParseInt(value, 0, 64); err != nil {
				return nil, fmt.Errorf("invalid argument %q for --port: %w", value, err)
			}
			if !dropPort {
				rebuilt = append(rebuilt, token)
			}
			continue
		}
		rebuilt = append(rebuilt, token)
	}
	return rebuilt, nil
}

func safeGUIPortWarningRaw(raw string) string {
	const maxRunes = 64
	trimmed := strings.TrimSpace(raw)
	runes := []rune(trimmed)
	if len(runes) > maxRunes {
		trimmed = string(runes[:maxRunes-3]) + "..."
	}
	return trimmed
}

func formatInvalidGUIPortWarning(intent guiPortIntent, fallback guiPortFallbackSource) string {
	return fmt.Sprintf(
		"warning: gui-port-persisted-invalid raw=%q reason=%s fallback=%s; ignored invalid persisted GUI port\n",
		safeGUIPortWarningRaw(intent.Raw), intent.Reason, fallback,
	)
}

func emitInvalidGUIPortWarning(w io.Writer, intent guiPortIntent, fallback guiPortFallbackSource) {
	if intent.Kind != guiPortIntentInvalid {
		return
	}
	_, _ = io.WriteString(w, formatInvalidGUIPortWarning(intent, fallback))
	// Fire-and-forget the structured log: LogHubMcpEvent takes the blocking
	// hub-mcp.log.lock flock, and this runs on the readiness-critical GUI
	// startup / activation-ping path, so a concurrent log writer must never
	// stall GUI readiness (matches the detectSeparateProcessOnce remediation,
	// bot #423 P2). The synchronous stderr write above is the reliable operator
	// surface pre-bind; the log event is best-effort.
	go func() {
		_ = api.LogHubMcpEvent("warn", "gui-port-persisted-invalid", map[string]any{
			"raw":             safeGUIPortWarningRaw(intent.Raw),
			"reason":          string(intent.Reason),
			"fallback_source": string(fallback),
		})
	}()
}

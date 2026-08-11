package cli

// WindowsDebugConsolePrefix is the sole public spelling for opting the
// current Windows mcphub process into a console. It is a startup prefix, not a
// Cobra/pflag option.
const WindowsDebugConsolePrefix = "--debug-console"

// WindowsConsolePolicy is the closed process-local startup policy. The zero
// value is deliberately console-free.
type WindowsConsolePolicy uint8

const (
	WindowsConsoleDisabled WindowsConsolePolicy = iota
	WindowsConsoleDebugExplicit
)

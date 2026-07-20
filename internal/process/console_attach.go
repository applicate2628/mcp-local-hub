package process

import (
	"os"
	"os/exec"
	"strings"
)

// SuppressConsoleAttachEnv is the single owner of the environment-variable
// name that tells a freshly-spawned mcphub process "you are a detached
// background process: do NOT attach yourself to any console".
//
// Why an env var and not a CLI flag: the attach happens in main() as the
// FIRST statement, before cobra has parsed anything (it must, because the
// attach is what makes stdout usable for everything that follows). A flag
// is therefore unreadable at the only moment the decision can be made,
// whereas the environment is fully available at process entry. It also
// crosses the CreateProcess boundary without the parent having to know
// which subcommand shape the child was given.
//
// Why the child must be told at all: DETACHED_PROCESS blocks console
// INHERITANCE at create time, but it does not stop a child from calling
// AttachConsole(ATTACH_PARENT_PROCESS) afterwards and becoming a console
// client on purpose — which is exactly what mcphub.exe's own main() does.
// A GUI-spawned supervisor therefore re-attached to the very terminal the
// GUI was launched from and died with it (CTRL_CLOSE_EVENT), taking every
// daemon under its Job Object with it. See
// work-items/bugs/2026-07-20-gui-spawned-supervisor-console-client.md.
//
// Scope: this suppresses the ATTACH only. An `mcphub supervise` typed
// directly into a terminal does not carry the variable and keeps its
// console exactly as before.
const SuppressConsoleAttachEnv = "MCPHUB_NO_CONSOLE_ATTACH"

// ConsoleAttachSuppressed reports whether this process was launched with
// the console attach suppressed.
//
// COMPOSITION ROOT ONLY. This is an ambient-environment read, so it is
// legitimate exactly once, at process entry (cmd/mcphub's main), which
// then injects the resolved value downward. No library module may call
// it to re-derive console policy for itself.
//
// Truthy parsing mirrors strictJobProtectionEnabled / autoCleanupOptedOut:
// "1" or "true" after trim+lowercase.
func ConsoleAttachSuppressed() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(SuppressConsoleAttachEnv)))
	return v == "1" || v == "true"
}

// SuppressConsoleAttach marks cmd so the spawned mcphub child never makes
// itself a console client. Pair it with the platform detach flags: the
// flags stop inheritance, this stops the deliberate re-attach, and only
// both together actually deliver "this child is not a CTRL_CLOSE_EVENT
// target".
//
// Seeding from os.Environ() when cmd.Env is nil preserves exec.Cmd's
// inherit-the-parent-environment default; os/exec de-duplicates the
// composed environment keeping the LAST occurrence of a key, so appending
// is safe even if the parent already carries the variable.
//
// Setting it on non-Windows is harmless (the attach is a no-op there) and
// keeps callers build-tag free.
func SuppressConsoleAttach(cmd *exec.Cmd) {
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env = append(env, SuppressConsoleAttachEnv+"=1")
}

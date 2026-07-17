package daemonrecovery

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

// Verdict is the fail-closed result of classifying a process that owns a
// daemon's configured port. Only VerdictOwnTask authorizes an identity-gated
// reap.
type Verdict int

const (
	VerdictOwnTask Verdict = iota
	VerdictForeign
	VerdictUnverified
)

func (v Verdict) String() string {
	switch v {
	case VerdictOwnTask:
		return "own_task"
	case VerdictForeign:
		return "foreign"
	default:
		return "unverified"
	}
}

// RuntimeEntry contains the only persisted PID fields relevant to the
// sibling/current-child exclusion gates.
type RuntimeEntry struct {
	CurrentPID int
	OrphanPID  int
}

// ClassifierDependencies are the live-process probes used by ClassifyPortOwner.
// LookupIdentity may be nil on platforms without a start-time-proof identity
// reader; that fails closed to VerdictUnverified.
type ClassifierDependencies struct {
	LookupIdentity    func(pid int) (process.ProcessIdentity, error)
	ExecutableMatches func(pid int, expectedPath string) bool
	// Generation, when present, is the process handle acquired before identity
	// lookup. It binds executable/start verification to the same generation that
	// a destructive caller will terminate.
	Generation process.HeldPIDGeneration
}

// ClassifyPortOwner proves whether ownerPID is a disowned child for this exact
// descriptor. Any missing proof, tracked sibling, executable mismatch, or argv
// mismatch refuses reap authority.
func ClassifyPortOwner(
	d api.SupervisorDaemon,
	ownerPID, selfPID int,
	tracked map[string]RuntimeEntry,
	deps ClassifierDependencies,
) (Verdict, process.ProcessIdentity) {
	canon := CanonicalTaskName(d.TaskName)
	if ownerPID <= 0 || (selfPID != 0 && ownerPID == selfPID) {
		return VerdictUnverified, process.ProcessIdentity{}
	}
	if entry, ok := tracked[canon]; ok && ownerPID == entry.CurrentPID {
		return VerdictUnverified, process.ProcessIdentity{}
	}
	for task, entry := range tracked {
		if CanonicalTaskName(task) == canon {
			continue
		}
		if ownerPID == entry.CurrentPID || (entry.OrphanPID != 0 && ownerPID == entry.OrphanPID) {
			return VerdictForeign, process.ProcessIdentity{}
		}
	}
	if deps.LookupIdentity == nil {
		return VerdictUnverified, process.ProcessIdentity{}
	}
	id, err := deps.LookupIdentity(ownerPID)
	if err != nil {
		return VerdictUnverified, process.ProcessIdentity{}
	}
	if id.PID != ownerPID {
		return VerdictUnverified, process.ProcessIdentity{}
	}
	expectedExe := expectedIdentityExecutable(d.Command)
	if expectedExe == "" {
		return VerdictForeign, id
	}
	if deps.Generation != nil {
		proof := KillProof(id)
		proof.ExecutablePath = expectedExe
		if deps.Generation.PID() != ownerPID || deps.Generation.VerifyIdentity(proof) != nil {
			return VerdictUnverified, id
		}
	} else if deps.ExecutableMatches == nil || !deps.ExecutableMatches(ownerPID, expectedExe) {
		return VerdictForeign, id
	}
	if !CommandLineMatchesTaskArgv(TokenizeWindowsCommandLine(id.CommandLine), d) {
		return VerdictForeign, id
	}
	return VerdictOwnTask, id
}

// CanonicalTaskName returns the supervisor intent's leading-backslash form.
func CanonicalTaskName(taskName string) string {
	if taskName == "" || strings.HasPrefix(taskName, `\`) {
		return taskName
	}
	return `\` + taskName
}

func expectedIdentityExecutable(command string) string {
	exe := command
	if exe == "" {
		exe, _ = os.Executable()
	} else if filepath.Base(exe) == exe {
		if looked, err := exec.LookPath(exe); err == nil {
			exe = looked
		} else {
			exe, _ = os.Executable()
		}
	}
	if exe == "" {
		return ""
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Clean(exe)
}

// KillProof derives the identity-reverifying termination proof exclusively from
// the fresh identity returned by ClassifyPortOwner.
func KillProof(id process.ProcessIdentity) process.PIDIdentityProof {
	proof := process.PIDIdentityProof{
		PID:            id.PID,
		ExecutablePath: id.ExecutablePath,
		CommandLine:    id.CommandLine,
		StartedAt:      StartedAt(id),
	}
	if proof.PID > 0 && proof.StartedAt != "" {
		proof.StartTolerance = time.Second
	}
	return proof
}

// StartedAt renders the identity reader's second-precision creation time in the
// timestamp shape consumed by process.TerminatePIDWithIdentity.
func StartedAt(id process.ProcessIdentity) string {
	if id.CreationDateUnix <= 0 {
		return ""
	}
	return time.Unix(id.CreationDateUnix, 0).UTC().Format(time.RFC3339Nano)
}

// CommandLineMatchesTaskArgv applies the exact task discriminator for global,
// serena-proxy, and workspace-proxy daemon descriptors.
func CommandLineMatchesTaskArgv(tokens []string, d api.SupervisorDaemon) bool {
	if api.IsSerenaProxyDescriptor(d) {
		taskName := CanonicalTaskName(d.TaskName)
		descriptorTaskName, descriptorOK := parserEffectiveUniqueFlagValue(d.Args, "--task-name")
		return taskName != "" &&
			descriptorOK && CanonicalTaskName(descriptorTaskName) == taskName &&
			hasSubcommandAnchor(tokens, "daemon", "serena-proxy") &&
			CommandLineHasAdjacentTokenPair(tokens, "--task-name", descriptorTaskName)
	}
	if api.IsWorkspaceLSPProxyDescriptor(d) {
		workspace, workspaceOK := descriptorArgValue(d, "--workspace")
		language, languageOK := descriptorArgValue(d, "--language")
		return workspaceOK && languageOK && workspace != "" && language != "" &&
			hasSubcommandAnchor(tokens, "daemon", "workspace-proxy") &&
			CommandLineHasAdjacentTokenPair(tokens, "--workspace", workspace) &&
			CommandLineHasAdjacentTokenPair(tokens, "--language", language)
	}
	if api.DescriptorHasGlobalDaemonArgv(d) {
		server, daemon, ok := api.DescriptorServerDaemon(d)
		descriptorServer, serverOK := descriptorArgValue(d, "--server")
		descriptorDaemon, daemonOK := descriptorArgValue(d, "--daemon")
		return ok && serverOK && daemonOK && server != "" && daemon != "" &&
			descriptorServer == server && descriptorDaemon == daemon &&
			hasGlobalDaemonAnchor(tokens) &&
			CommandLineHasAdjacentTokenPair(tokens, "--server", server) &&
			CommandLineHasAdjacentTokenPair(tokens, "--daemon", daemon)
	}
	return false
}

func descriptorArgValue(d api.SupervisorDaemon, flag string) (string, bool) {
	return parserEffectiveUniqueFlagValue(d.Args, flag)
}

func hasSubcommandAnchor(tokens []string, sub ...string) bool {
	if len(tokens) < 1+len(sub) {
		return false
	}
	for i, value := range sub {
		if tokens[1+i] != value {
			return false
		}
	}
	return true
}

func hasGlobalDaemonAnchor(tokens []string) bool {
	return len(tokens) >= 3 && tokens[1] == "daemon" && strings.HasPrefix(tokens[2], "-")
}

// CommandLineHasAdjacentTokenPair preserves the historical helper name while
// matching the daemon flag parser's effective long-flag value. Exactly one
// occurrence is required; duplicate same-value and conflicting flags both fail
// closed. Both --flag value and --flag=value forms are accepted, and parsing
// stops at the first unconsumed -- delimiter.
func CommandLineHasAdjacentTokenPair(tokens []string, flag, value string) bool {
	effective, ok := parserEffectiveUniqueFlagValue(tokens, flag)
	return ok && effective == value
}

func parserEffectiveUniqueFlagValue(tokens []string, flag string) (string, bool) {
	prefix := flag + "="
	found := false
	value := ""
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if token == "--" {
			break
		}
		var candidate string
		matched := false
		switch {
		case token == flag:
			if i+1 >= len(tokens) {
				return "", false
			}
			i++
			candidate = tokens[i]
			matched = true
		case strings.HasPrefix(token, prefix):
			candidate = strings.TrimPrefix(token, prefix)
			matched = true
		}
		if !matched {
			continue
		}
		if found {
			return "", false
		}
		found = true
		value = candidate
	}
	return value, found
}

// TokenizeWindowsCommandLine is the fail-closed pure-Go argv parser used by the
// exact task discriminator.
func TokenizeWindowsCommandLine(commandLine string) []string {
	var tokens []string
	for len(commandLine) > 0 {
		if commandLine[0] == ' ' || commandLine[0] == '\t' {
			commandLine = commandLine[1:]
			continue
		}
		var arg []byte
		arg, commandLine = readNextWindowsArg(commandLine)
		tokens = append(tokens, string(arg))
	}
	return tokens
}

func readNextWindowsArg(commandLine string) (arg []byte, rest string) {
	var out []byte
	var inQuote bool
	var slashCount int
	for ; len(commandLine) > 0; commandLine = commandLine[1:] {
		char := commandLine[0]
		switch char {
		case ' ', '\t':
			if !inQuote {
				return appendWindowsBackslashes(out, slashCount), commandLine[1:]
			}
		case '"':
			out = appendWindowsBackslashes(out, slashCount/2)
			if slashCount%2 == 0 {
				if inQuote && len(commandLine) > 1 && commandLine[1] == '"' {
					out = append(out, char)
					commandLine = commandLine[1:]
				}
				inQuote = !inQuote
			} else {
				out = append(out, char)
			}
			slashCount = 0
			continue
		case '\\':
			slashCount++
			continue
		}
		out = appendWindowsBackslashes(out, slashCount)
		slashCount = 0
		out = append(out, char)
	}
	return appendWindowsBackslashes(out, slashCount), ""
}

func appendWindowsBackslashes(out []byte, count int) []byte {
	for ; count > 0; count-- {
		out = append(out, '\\')
	}
	return out
}

const eventFieldCap = 2048

// BoundEventField prevents attacker-controlled identity text from evicting the
// fixed forensic fields in the bounded supervisor event body.
func BoundEventField(value string) string {
	if len(value) <= eventFieldCap {
		return value
	}
	cut := eventFieldCap
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…[truncated]"
}

// EmitAuditEvent writes the shared bounded identity event used by automatic
// sweeps and operator recovery.
func EmitAuditEvent(
	events *api.SupervisorEventLog,
	event, source string,
	verdict Verdict,
	d api.SupervisorDaemon,
	ownerPID int,
	id process.ProcessIdentity,
	extra map[string]any,
) {
	if events == nil {
		return
	}
	_ = events.Emit(auditEvent(event, source, verdict, d, ownerPID, id, extra))
}

// auditEvent is the single owner of the shared bounded identity envelope. The
// recovery path also uses it for its bounded pre-respawn emit and queued
// durable fallback so those two attempts cannot drift in body shape.
func auditEvent(
	event, source string,
	verdict Verdict,
	d api.SupervisorDaemon,
	ownerPID int,
	id process.ProcessIdentity,
	extra map[string]any,
) api.SupervisorEvent {
	body := map[string]any{
		"squatter_pid": ownerPID,
		"verdict":      verdict.String(),
		"port":         d.Port,
		"source":       source,
		"actor":        api.CurrentOSUser(),
	}
	if id.ExecutablePath != "" {
		body["executable_path"] = BoundEventField(id.ExecutablePath)
	}
	if id.CommandLine != "" {
		body["command_line"] = BoundEventField(id.CommandLine)
	}
	if startedAt := StartedAt(id); startedAt != "" {
		body["started_at"] = startedAt
	}
	for key, value := range extra {
		body[key] = value
	}
	envelopeSource := "liveness"
	if source == "prespawn" {
		envelopeSource = "restart-policy"
	}
	return api.SupervisorEvent{
		Severity: "warn",
		Source:   envelopeSource,
		Event:    event,
		TaskName: CanonicalTaskName(d.TaskName),
		Body:     body,
	}
}

package gdb

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/toolchain"
)

// versionProbeFunc resolves and probes the gdb binary for the debugger_status
// tool: it returns the resolved gdb path, the first line of `<gdb> --version`,
// and whether the probe succeeded. The production implementation is
// defaultVersionProbe (a real exec.Command — which WORKS in the console-less
// daemon, unlike GDB-MCP's python subprocess); tests inject a fake.
type versionProbeFunc func() (path, version string, available bool)

// GdbServer holds the MCP server plus the session registry and the injectable
// seams the tool handlers use:
//
//   - resolveGdbPath resolves the default gdb path (default:
//     toolchain.DefaultGdbPath); gdb_start uses it when the caller omits gdb_path.
//   - startSession spawns a new MI session (default: startSession); injected so
//     tests never spawn real gdb.
//   - probeVersion backs debugger_status (default: defaultVersionProbe).
//
// The registry maps a monotonic session id ("gdb-1", "gdb-2", …) to its live
// session, guarded by mu. Session ids come from a counter rather than time/random
// so they are deterministic and easy to reason about in logs and tests.
//
// The server is UNGATED: gdb is a debugger and the external GDB-MCP it replaces
// was ungated, so registration is unconditional (matching that prior surface).
type GdbServer struct {
	server *mcp.Server

	resolveGdbPath func() string
	startSession   startSessionFunc
	probeVersion   versionProbeFunc

	mu       sync.Mutex
	sessions map[string]*session
	counter  int
}

// Run wires up a fresh GdbServer with the production seams, registers the tools,
// and serves the MCP protocol over stdio until ctx is cancelled or the transport
// closes. Single source of truth for every entry point.
func Run(ctx context.Context) error {
	gs := newServer()
	gs.server = mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-local-hub-gdb",
		Version: "1.0.0",
	}, nil)

	registerTools(gs)

	if err := gs.server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("gdb server run: %w", err)
	}
	return nil
}

// newServer constructs a GdbServer with the production seams and an empty
// session registry. Split out from Run so tests build a server with fake seams
// and exercise the handlers without an mcp.Server or a real gdb.
func newServer() *GdbServer {
	return &GdbServer{
		resolveGdbPath: toolchain.DefaultGdbPath,
		startSession:   startSession,
		probeVersion:   defaultVersionProbe,
		sessions:       map[string]*session{},
	}
}

// registerTools attaches the gdb tools. Called once from Run during startup.
//
// Each session tool is registered under BOTH its gdb_* name AND a generic
// debugger_* alias. The external GDB-MCP this server replaced exposed both
// families (debugger_start / debugger_command / debugger_terminate /
// debugger_list_sessions plus the gdb_* names), so agents — and clients holding
// a cached GDB-MCP tool list — naturally call debugger_start. Registering the
// aliases to the SAME handlers means those calls succeed instead of failing
// "unknown tool debugger_start" (the regression that prompted this: the native
// bridge had dropped the debugger_* family). debugger_status keeps its name (it
// is already the canonical generic status tool; gdb_status is added as a
// symmetric alias so an agent that prefixes everything with gdb_ also resolves).
func registerTools(gs *GdbServer) {
	startSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"gdb_path": map[string]any{
				"type":        "string",
				"description": "Optional absolute path to the gdb binary. Defaults to the auto-detected toolchain gdb (MSYS2 ucrt64 on Windows, system gdb on POSIX).",
			},
			"program": map[string]any{
				"type":        "string",
				"description": "Optional path to the target program to load into the session (gdb's positional arg). You can also load it later with the 'file' command.",
			},
		},
	}
	commandSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session_id": map[string]any{
				"type":        "string",
				"description": "The session id returned by gdb_start/debugger_start (e.g. \"gdb-1\").",
			},
			"command": map[string]any{
				"type":        "string",
				"description": "The gdb command to run (MI or plain CLI).",
			},
		},
		"required": []string{"session_id", "command"},
	}
	sessionIDSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"session_id": map[string]any{
				"type":        "string",
				"description": "The session id to terminate.",
			},
		},
		"required": []string{"session_id"},
	}
	emptySchema := map[string]any{"type": "object", "properties": map[string]any{}}

	startDesc := "Start a new GDB debugging session. Spawns gdb in GDB/MI machine-interface mode " +
		"(gdb --interpreter=mi3 --nx -q) by spawning the binary directly via Go exec — so it works " +
		"inside the console-less mcphub daemon where the external python GDB-MCP server failed its " +
		"availability probe. Returns JSON {session_id, gdb_path, version}; pass the session_id to " +
		"gdb_command/debugger_command and gdb_terminate/debugger_terminate. Optional gdb_path " +
		"overrides the auto-detected gdb binary; optional program loads a target."
	commandDesc := "Run a command in an existing GDB session and return its console output. Accepts both " +
		"GDB/MI commands (e.g. -break-insert main, -exec-run, -exec-continue) and plain CLI commands " +
		"(e.g. 'break main', 'info registers', 'backtrace') — gdb's MI interpreter accepts CLI commands " +
		"and echoes their output on the console stream. For run/continue/step commands the call blocks " +
		"until the inferior next stops (or a 30s deadline) and includes the stop reason/location. Returns " +
		"the human-readable output text, or an error result if the session_id is unknown."
	terminateDesc := "Terminate a GDB session (sends -gdb-exit, then force-kills if it does not exit promptly) " +
		"and remove it from the session registry. Returns a confirmation, or an error result if the " +
		"session_id is unknown."
	listDesc := "List the ids of all active GDB sessions. Returns JSON {sessions: [\"gdb-1\", ...]}."
	statusDesc := "Report whether gdb is available and at what version. Resolves the toolchain gdb path " +
		"and runs `<gdb> --version` via Go exec (which works in the console-less mcphub daemon, unlike " +
		"the python subprocess probe in the external GDB-MCP server that reported gdb 'not available'). " +
		"Returns JSON {available, gdb_path, version}."

	add := func(name, desc string, schema map[string]any, h mcp.ToolHandler) {
		gs.server.AddTool(&mcp.Tool{Name: name, Description: desc, InputSchema: schema}, h)
	}

	// Canonical gdb_* names + generic debugger_* aliases share handlers.
	add("gdb_start", startDesc, startSchema, gs.startTool)
	add("debugger_start", "Alias of gdb_start. "+startDesc, startSchema, gs.startTool)
	add("gdb_command", commandDesc, commandSchema, gs.commandTool)
	add("debugger_command", "Alias of gdb_command. "+commandDesc, commandSchema, gs.commandTool)
	add("gdb_terminate", terminateDesc, sessionIDSchema, gs.terminateTool)
	add("debugger_terminate", "Alias of gdb_terminate. "+terminateDesc, sessionIDSchema, gs.terminateTool)
	add("gdb_list_sessions", listDesc, emptySchema, gs.listTool)
	add("debugger_list_sessions", "Alias of gdb_list_sessions. "+listDesc, emptySchema, gs.listTool)
	add("debugger_status", statusDesc, emptySchema, gs.statusTool)
	add("gdb_status", "Alias of debugger_status. "+statusDesc, emptySchema, gs.statusTool)
}

// startTool handles gdb_start: it resolves the gdb path (caller override or the
// default seam), starts a new MI session via the injectable seam, registers it
// under a fresh monotonic id, and returns {session_id, gdb_path, version}.
func (gs *GdbServer) startTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		GdbPath string `json:"gdb_path"`
		Program string `json:"program"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	gdbPath := strings.TrimSpace(args.GdbPath)
	if gdbPath == "" {
		gdbPath = gs.resolveGdbPath()
	}

	sess, err := gs.startSession(gdbPath, strings.TrimSpace(args.Program))
	if err != nil {
		return errResult(fmt.Sprintf("start gdb session: %v", err)), nil
	}

	gs.mu.Lock()
	gs.counter++
	id := fmt.Sprintf("gdb-%d", gs.counter)
	gs.sessions[id] = sess
	gs.mu.Unlock()

	body, _ := json.Marshal(map[string]string{
		"session_id": id,
		"gdb_path":   sess.gdbPath,
		"version":    sess.version,
	})
	return textResult(string(body)), nil
}

// commandTool handles gdb_command: it looks up the session and runs the command,
// returning the console output (or a tool error for an unknown session / a gdb
// error result).
func (gs *GdbServer) commandTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		SessionID string `json:"session_id"`
		Command   string `json:"command"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.SessionID) == "" {
		return errResult("missing required parameter: session_id"), nil
	}
	if strings.TrimSpace(args.Command) == "" {
		return errResult("missing required parameter: command"), nil
	}

	sess := gs.lookup(args.SessionID)
	if sess == nil {
		return errResult(fmt.Sprintf("unknown session_id %q (use gdb_list_sessions to see active sessions)", args.SessionID)), nil
	}

	out, err := sess.Command(args.Command)
	if err != nil {
		// A gdb-level error (^error,msg=...) is a real result the caller should
		// see, not a transport failure. Surface the message plus any partial
		// console output captured before the error.
		msg := err.Error()
		if out != "" {
			msg = out + "\n" + msg
		}
		return errResult(msg), nil
	}
	return textResult(out), nil
}

// terminateTool handles gdb_terminate: it removes the session from the registry
// and terminates the gdb process.
func (gs *GdbServer) terminateTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if strings.TrimSpace(args.SessionID) == "" {
		return errResult("missing required parameter: session_id"), nil
	}

	gs.mu.Lock()
	sess, ok := gs.sessions[args.SessionID]
	if ok {
		delete(gs.sessions, args.SessionID)
	}
	gs.mu.Unlock()

	if !ok {
		return errResult(fmt.Sprintf("unknown session_id %q", args.SessionID)), nil
	}
	sess.Terminate()

	body, _ := json.Marshal(map[string]string{
		"session_id": args.SessionID,
		"status":     "terminated",
	})
	return textResult(string(body)), nil
}

// listTool handles gdb_list_sessions: it returns the sorted active session ids.
func (gs *GdbServer) listTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	gs.mu.Lock()
	ids := make([]string, 0, len(gs.sessions))
	for id := range gs.sessions {
		ids = append(ids, id)
	}
	gs.mu.Unlock()
	sort.Strings(ids)

	body, _ := json.Marshal(map[string][]string{"sessions": ids})
	return textResult(string(body)), nil
}

// statusTool handles debugger_status: it probes gdb availability via the
// injectable seam and returns {available, gdb_path, version}. This is the tool
// that failed under GDB-MCP; here the probe is a Go exec that works in the
// console-less daemon.
func (gs *GdbServer) statusTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	path, version, available := gs.probeVersion()
	body, _ := json.Marshal(map[string]any{
		"available": available,
		"gdb_path":  path,
		"version":   version,
	})
	return textResult(string(body)), nil
}

// lookup returns the session registered under id, or nil if none.
func (gs *GdbServer) lookup(id string) *session {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	return gs.sessions[id]
}

// defaultVersionProbe resolves the toolchain gdb path and runs `<gdb> --version`
// via Go exec, returning the path, the first line of the version output, and
// whether the probe succeeded. The Go exec works in the console-less daemon — the
// exact thing GDB-MCP's python subprocess probe could not do.
func defaultVersionProbe() (string, string, bool) {
	path := toolchain.DefaultGdbPath()
	version, ok := gdbVersionLine(path)
	return path, version, ok
}

// gdbVersionLine runs `<path> --version` via Go exec and returns the first
// non-empty line of its output and whether the probe succeeded. It is the single
// owner of the version-exec logic shared by debugger_status (via
// defaultVersionProbe) and gdb_start (via defaultStartVersion in session.go) —
// startSession spawns gdb with `-q`, which suppresses the startup banner the
// version used to be scraped from, so the session version must come from this
// dedicated `--version` probe instead. The Go exec works in the console-less
// daemon, the exact thing GDB-MCP's python subprocess probe could not do.
func gdbVersionLine(path string) (string, bool) {
	cmd := exec.Command(path, "--version")
	process.NoConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return firstNonEmptyLine(string(out)), true
}

// textResult wraps text in a non-error CallToolResult.
func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// errResult builds a tool-level error CallToolResult (IsError=true) with a
// single text message. Mirrors the drmemory/godbolt error-result helper.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
	}
}

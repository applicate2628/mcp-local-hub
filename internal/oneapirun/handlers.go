package oneapirun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/unsafegate"
)

// defaultTimeoutSec is the run timeout applied when the caller omits
// timeout_sec (or passes a non-positive value). Ten minutes matches the
// "long build / ctest / ASan run" use cases this tool targets.
const defaultTimeoutSec = 600

// waitDelayAfterKill bounds how long cmd.Run waits for the command's
// stdout/stderr pipes to close AFTER the process is killed by the timeout,
// before forcibly closing them and returning. Needed because a killed
// command's grandchild (e.g. cmd → ping) can inherit the pipe and keep
// cmd.Run blocked until the grandchild exits on its own. A short delay
// caps that tail so the timeout actually bounds the call.
const waitDelayAfterKill = 2 * time.Second

// maxStreamBytes caps each of stdout / stderr in the structured result.
// The captured bytes are embedded as JSON string values inside the
// CallToolResult, which travels over the StdioHost stdout scanner — keep
// well below the 1 MiB scanner cap (see godbolt/handlers.go comment) with
// headroom for JSON escaping and the envelope. 256 KiB per stream is ample
// for build / test logs while staying safe.
const maxStreamBytes = 256 * 1024

// truncationMarker is appended (as a separate field hint inside the text)
// when a stream is truncated, so the caller knows output was cut.
const truncationMarker = "\n…[truncated: output exceeded 256 KiB cap]"

// runResult is the structured payload returned by run_in_oneapi_env. It is
// marshalled to JSON and returned as the tool's TextContent so MCP clients
// get a machine-parseable result.
type runResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	EnvSource  string `json:"env_source"`
	DurationMs int64  `json:"duration_ms"`
	// TimedOut is set true when the run was killed by the timeout. The
	// exit_code in that case reflects the killed process (-1 on most
	// platforms) and is not meaningful — TimedOut is the authoritative
	// signal.
	TimedOut bool `json:"timed_out,omitempty"`
}

// enableUnsafeOneAPIRunEnv gates registration of run_in_oneapi_env. The tool
// intentionally executes caller-supplied native commands, which is an unsafe
// arbitrary-local-code-execution capability for broadly-configured MCP clients
// unless an operator explicitly opts in. (Incorporated from PR #345.)
const enableUnsafeOneAPIRunEnv = "MCP_LOCAL_HUB_ENABLE_UNSAFE_ONEAPI_RUN"

// oneAPIRunEnabled reports whether the operator opted into exposing the
// arbitrary-command execution tool (enableUnsafeOneAPIRunEnv == "1"). Thin
// wrapper over the shared unsafegate owner; pure, for tests.
func oneAPIRunEnabled() bool {
	return unsafegate.Enabled(enableUnsafeOneAPIRunEnv)
}

// registerTools attaches the oneapi-run tools to the MCP server. The
// read-only oneapi_env_status probe is ALWAYS registered (it runs no
// caller-supplied command — it only reports whether oneAPI is present). The
// arbitrary-command run_in_oneapi_env tool is registered ONLY after an
// explicit unsafe opt-in. Called once from Run during startup. When the
// opt-in is absent the daemon still runs and serves the MCP protocol exposing
// just the safe status probe (unsafegate.RegisterAllowed logs WHY the run
// tool is withheld to stderr so the secure-default is observable), so a
// misconfigured client cannot reach the arbitrary-command surface.
func registerTools(rs *OneAPIRunServer) {
	registerStatusTool(rs)

	if !unsafegate.RegisterAllowed(enableUnsafeOneAPIRunEnv, "oneapi-run") {
		return
	}

	rs.server.AddTool(&mcp.Tool{
		Name: "run_in_oneapi_env",
		Description: "Run ANY native command under the fully-initialized Visual-Studio + Intel-oneAPI environment. " +
			"Use this when gdb / lldb / a freshly built .exe / ctest / an ASan build fails because it can't see the VS toolchain DLLs or the oneAPI MKL/TBB runtime DLLs without a manual `vcvars64 && oneapi-shell` wrap, or when Git-Bash mangles native backslash paths. " +
			"The command is executed DIRECTLY via the OS (no shell interpretation) with an environment = the COMPLETE Visual-Studio + Intel-oneAPI environment captured from oneAPI's setvars.bat (full PATH for runtime DLLs, LIB for link libs incl. libircmt.lib + the MKL import libs, INCLUDE for headers incl. mkl.h, plus MKLROOT/CMPLR_ROOT/CPATH), with the oneAPI component DLL dirs additionally prepended to PATH. " +
			"Pass a NATIVE command and NATIVE-path args (e.g. command=\"ctest\", args=[\"-C\",\"Release\"], cwd=\"C:\\\\path\\\\to\\\\build\"). " +
			"Returns structured JSON: {exit_code, stdout, stderr, env_source, duration_ms, timed_out}. " +
			"env_source is \"setvars\" when the full setvars.bat environment was captured, \"oneapi-only\" when setvars.bat was not found (only the oneAPI runtime DLL dirs were prepended to PATH — a prebuilt MKL exe can RUN but a build would fail without LIB/INCLUDE), or \"plain\" when neither is available — the command always runs regardless.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The program to run (a native executable name or path, e.g. 'ctest', 'gdb', or 'C:\\\\build\\\\app.exe'). NOT a shell command line — args go in the args array.",
				},
				"args": map[string]any{
					"type":        "array",
					"description": "Arguments passed to command, each as a separate native-path string (no shell quoting). Optional.",
					"items":       map[string]any{"type": "string"},
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Working directory to run the command in (native path). Optional — inherits the server's cwd when omitted.",
				},
				"timeout_sec": map[string]any{
					"type":        "integer",
					"description": "Maximum seconds to let the command run before it is killed. Optional — defaults to 600 (10 minutes). Non-positive values fall back to the default.",
				},
			},
			"required": []string{"command"},
		},
	}, rs.runInOneAPIEnvTool)
}

// runInOneAPIEnvTool is the run_in_oneapi_env handler. It computes the
// VS+oneAPI environment, runs the requested native command under that env
// + cwd + timeout, and returns the structured runResult as JSON.
func (rs *OneAPIRunServer) runInOneAPIEnvTool(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, retErr error) {
	// NEVER return an empty/crashing result. A panic anywhere in the handler
	// (a marshal edge case, an env-merge surprise, a runtime fault) is
	// recovered here and turned into a structured runResult JSON with
	// exit_code -1 and the panic on stderr, so the MCP client always gets a
	// parseable answer instead of an empty result or a dropped daemon.
	defer func() {
		if r := recover(); r != nil {
			res := runResult{
				ExitCode: -1,
				Stderr:   fmt.Sprintf("oneapi-run: internal panic: %v", r),
			}
			payload, err := json.Marshal(res)
			if err != nil {
				// Last-resort: a hand-built JSON string so we still return
				// structured, non-empty content (never an empty result).
				payload = []byte(`{"exit_code":-1,"stderr":"oneapi-run: internal panic (and result marshal failed)"}`)
			}
			result = &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}},
			}
			retErr = nil
		}
	}()

	var args struct {
		Command    string   `json:"command"`
		Args       []string `json:"args"`
		Cwd        string   `json:"cwd"`
		TimeoutSec int      `json:"timeout_sec"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return toolErrorResult(fmt.Errorf("invalid arguments: %w", err)), nil
	}
	if args.Command == "" {
		return toolErrorResult(errors.New("missing required parameter: command")), nil
	}

	timeoutSec := args.TimeoutSec
	if timeoutSec <= 0 {
		timeoutSec = defaultTimeoutSec
	}

	env, source := computeRunEnv(rs.captureVSEnv, rs.oneAPIDLLDirs)

	res := runCommand(ctx, args.Command, args.Args, args.Cwd, env, source, time.Duration(timeoutSec)*time.Second)

	payload, err := json.Marshal(res)
	if err != nil {
		return toolErrorResult(fmt.Errorf("failed to marshal result: %w", err)), nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}},
	}, nil
}

// runCommand executes command with args in cwd under env, capping each
// output stream and enforcing timeout. It NEVER returns an error to the
// caller — every outcome (clean run, non-zero exit, spawn failure,
// timeout) is encoded in the returned runResult so the MCP client always
// gets a structured answer. A spawn failure (binary not found, bad cwd)
// is reported with exit_code -1 and the error text on stderr.
func runCommand(ctx context.Context, command string, args []string, cwd string, env []string, source string, timeout time.Duration) runResult {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()

	// Resolve the command against the COMPUTED CHILD env's PATH first.
	// exec.CommandContext resolves a bare command NAME via LookPath against
	// the SERVER process's PATH (os.Getenv) BEFORE cmd.Env is applied, so a
	// tool that only exists on the augmented child PATH (oneAPI / VS dirs
	// prepended) fails to start (observed live: command:"icx-cl" → "exec:
	// icx-cl: executable file not found in %PATH%" even though `where icx-cl`
	// under the child env finds it). Handing exec an already-resolved
	// absolute path makes it skip its own LookPath. resolveCommandPath
	// returns the command unchanged when it already bears a path separator
	// or when it cannot be resolved (so exec still surfaces a clear error).
	resolved := resolveCommandPath(command, env)

	// Run the command DIRECTLY — no shell interpretation. This avoids
	// Git-Bash path mangling and cmd.exe quoting surprises; the caller
	// supplies a native command + native-path args.
	cmd := exec.CommandContext(runCtx, resolved, args...)
	process.NoConsole(cmd) // suppress console flash on windowsgui parent
	cmd.Env = env
	if cwd != "" {
		cmd.Dir = cwd
	}

	// WaitDelay bounds how long cmd.Run blocks AFTER the process is killed
	// (by the context deadline) waiting for its stdout/stderr pipes to
	// close. Without it, a killed command whose GRANDCHILD still holds the
	// pipe (e.g. `cmd /c ping ...` — cmd is killed but ping inherits the
	// stdout handle) hangs cmd.Run until the grandchild exits on its own,
	// defeating the timeout entirely. With WaitDelay set, Run closes the
	// pipes and returns within the delay of the kill. (Diagnosed from a
	// failing timeout test that ran the full child duration instead of
	// being killed at the deadline.)
	cmd.WaitDelay = waitDelayAfterKill

	var stdout, stderr cappedBuffer
	stdout.limit = maxStreamBytes
	stderr.limit = maxStreamBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// RunUnderKillJob binds the command's whole subtree to a KILL_ON_JOB_CLOSE
	// job so a grandchild that outlives the timeout-killed direct child (e.g.
	// `cmd /c start ...`) is reaped instead of orphaning; WaitDelay above bounds
	// the Wait, the job reaps the orphan.
	runErr := process.RunUnderKillJob(cmd)
	durationMs := time.Since(start).Milliseconds()

	res := runResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		EnvSource:  source,
		DurationMs: durationMs,
	}

	// Timeout: the run context's deadline fired. cmd.Run returns a
	// non-nil error in that case and runCtx.Err() == DeadlineExceeded.
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		return res
	}

	if runErr == nil {
		res.ExitCode = 0
		return res
	}

	// Non-zero exit (a legitimate result for build/test failures): encode
	// the exit code, no error to the caller.
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res
	}

	// Spawn failure (binary not found, bad cwd, etc.): report it on
	// stderr with exit_code -1 so the caller sees the cause.
	res.ExitCode = -1
	if res.Stderr != "" {
		res.Stderr += "\n"
	}
	res.Stderr += fmt.Sprintf("oneapi-run: failed to start %q: %v", command, runErr)
	return res
}

// toolErrorResult wraps an error as an MCP tool-level error result.
func toolErrorResult(err error) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
	}
}

// cappedBuffer captures subprocess output up to limit bytes, then appends a
// single truncation marker and silently drops the rest. Unlike perftools'
// errOutputLimitExceeded approach (which aborts the run), this server wants
// the command to RUN TO COMPLETION (the agent needs the exit code) and just
// keep a bounded prefix of its output — a 10-minute build that prints 5 MiB
// must still report its exit code, not fail with "output limit exceeded".
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.truncated {
		// Already capped; pretend we consumed it so the pipe stays open
		// and the child keeps running to completion.
		return len(p), nil
	}
	remaining := c.limit - c.buf.Len()
	if len(p) <= remaining {
		return c.buf.Write(p)
	}
	if remaining > 0 {
		_, _ = c.buf.Write(p[:remaining])
	}
	c.buf.WriteString(truncationMarker)
	c.truncated = true
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	return c.buf.String()
}

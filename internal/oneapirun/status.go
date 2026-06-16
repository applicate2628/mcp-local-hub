package oneapirun

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// statusResult is the structured payload returned by oneapi_env_status. It is
// marshalled to JSON and returned as the tool's TextContent so MCP clients get
// a machine-parseable answer.
type statusResult struct {
	// OneAPIPresent reports whether an Intel oneAPI environment is usable by a
	// run: true when env_source is "setvars" or "oneapi-only", false when
	// "plain" (neither setvars.bat nor any oneAPI runtime DLL dir was found).
	OneAPIPresent bool `json:"oneapi_present"`
	// Root is the detected Intel oneAPI install root (oneapi.DetectRoot), or ""
	// when no install was located. Reported even when EnvSource is "oneapi-only"
	// (DLL dirs found) so an operator sees where the runtime came from.
	Root string `json:"root"`
	// EnvSource is the SAME three-way label a real run_in_oneapi_env call would
	// report for the current host ("setvars" / "oneapi-only" / "plain"),
	// computed by the shared envSourceFor owner.
	EnvSource string `json:"env_source"`
	// Components are the oneAPI component names whose runtime DLL dir was found
	// (e.g. ["mkl","tbb","compiler"]), derived from the enumerated DLL dirs.
	// Omitted when none were found.
	Components []string `json:"components,omitempty"`
}

// registerStatusTool attaches the read-only oneapi_env_status probe. It is
// registered UNCONDITIONALLY (no unsafe opt-in) because it runs no
// caller-supplied command — it only reports whether oneAPI is present, the
// install root, the env_source a real run would use, and the detected
// component DLL dirs.
func registerStatusTool(rs *OneAPIRunServer) {
	rs.server.AddTool(&mcp.Tool{
		Name: "oneapi_env_status",
		Description: "Report whether an Intel oneAPI environment is present, WITHOUT running any command. " +
			"Resolves the install root and the env_source a real run_in_oneapi_env call would use on this host. " +
			"Returns JSON {oneapi_present, root, env_source, components}. " +
			"env_source is \"setvars\" when the full Visual-Studio + oneAPI build environment is capturable from setvars.bat, " +
			"\"oneapi-only\" when only the oneAPI runtime DLL dirs were found (a prebuilt MKL exe can RUN but a build would lack LIB/INCLUDE), " +
			"or \"plain\" when no oneAPI install is found. components lists the oneAPI components whose runtime DLL dir was located (e.g. mkl, tbb, compiler).",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, rs.oneAPIEnvStatusTool)
}

// oneAPIEnvStatusTool is the oneapi_env_status handler. It is READ-ONLY:
// it resolves the oneAPI install root and the env_source label via the
// server's injectable seams (so tests fake detection without the real
// ~1-3s setvars subprocess) and returns the structured statusResult as JSON.
// Like the run handler it NEVER returns an empty/crashing result — a panic is
// recovered into a structured JSON answer so the MCP client always gets a
// parseable reply.
func (rs *OneAPIRunServer) oneAPIEnvStatusTool(ctx context.Context, req *mcp.CallToolRequest) (result *mcp.CallToolResult, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			res := statusResult{EnvSource: envSourcePlain}
			payload, err := json.Marshal(res)
			if err != nil {
				payload = []byte(`{"oneapi_present":false,"root":"","env_source":"plain"}`)
			}
			_ = r // panic detail intentionally not leaked into the status payload
			result = &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}},
			}
			retErr = nil
		}
	}()

	root, _ := rs.detectRoot()
	dllDirs := rs.oneAPIDLLDirs()
	_, vsOK := rs.captureVSEnv()

	source := envSourceFor(vsOK, len(dllDirs) > 0)

	res := statusResult{
		OneAPIPresent: source != envSourcePlain,
		Root:          root,
		EnvSource:     source,
		Components:    componentsFromDLLDirs(dllDirs),
	}

	payload, err := json.Marshal(res)
	if err != nil {
		return toolErrorResult(fmt.Errorf("failed to marshal status: %w", err)), nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}},
	}, nil
}

// componentsFromDLLDirs derives the oneAPI component names from the enumerated
// runtime DLL dirs. Each dir has the shape "<root>\<component>\latest\bin"
// (see oneapi.DLLDirs), so the component is the third path element from the
// end. A dir that does not match that shape is skipped rather than guessed.
// Returns nil for an empty input so the omitempty `components` field is
// dropped from the JSON when no component was found.
func componentsFromDLLDirs(dllDirs []string) []string {
	if len(dllDirs) == 0 {
		return nil
	}
	out := make([]string, 0, len(dllDirs))
	for _, dir := range dllDirs {
		if c := componentOf(dir); c != "" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// componentOf extracts the "<component>" segment from a
// "<root>/<component>/latest/bin" DLL dir, normalizing separators so both
// Windows ("\") and POSIX ("/") layouts resolve. Returns "" when the path is
// too short or does not end in the expected "latest/<bin>" tail.
func componentOf(dir string) string {
	cleaned := filepath.ToSlash(filepath.Clean(dir))
	parts := strings.Split(cleaned, "/")
	// Expect […, <component>, "latest", "bin"] — at least 3 trailing segments.
	if len(parts) < 3 {
		return ""
	}
	if !strings.EqualFold(parts[len(parts)-2], "latest") {
		return ""
	}
	return parts[len(parts)-3]
}

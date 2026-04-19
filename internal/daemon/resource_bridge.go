package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Registry entry for the resources-capability bridge. Translates
// tools/call __read_resource__ into resources/read, and resources/read
// responses back into tools/call content. See synthetic_tools.go for
// the generic machinery and types.

// readResourceToolDefinition is the static JSON for the injected tool
// entry. Stored as a pre-marshaled constant so splice into tools[]
// stays zero-alloc per request.
const readResourceToolDefinition = `{"name":"__read_resource__","description":"Read an MCP resource by URI. Resources carry server-side read-only data (catalogs, discovery info, workflow guides). Many MCP clients surface resources to the human user but not to the agent — this tool bridges the gap. Call resources/list for discoverable URIs, or consult the server's documentation for template-URI patterns (e.g. resource://compilers/{language_id}). Response is the resource body as text.","inputSchema":{"type":"object","properties":{"uri":{"type":"string","description":"The resource URI to read (absolute, e.g. 'resource://tools' or 'resource://compilers/c++')."}},"required":["uri"]}}`

func readResourceSyntheticTool() SyntheticTool {
	return SyntheticTool{
		Name:           "__read_resource__",
		Capability:     "resources",
		Definition:     json.RawMessage(readResourceToolDefinition),
		UpstreamMethod: "resources/read",
		MapArgs:        mapReadResourceArgs,
		MapResult:      mapReadResourceResult,
	}
}

// mapReadResourceArgs extracts the uri from tools/call arguments and
// rebuilds it as resources/read params. Returns a user-facing error
// when uri is missing or the arguments object is malformed — the
// bridge surfaces that as a tools/call isError response.
func mapReadResourceArgs(toolArgs json.RawMessage) (json.RawMessage, error) {
	var args struct {
		URI string `json:"uri"`
	}
	if len(toolArgs) > 0 {
		if err := json.Unmarshal(toolArgs, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if args.URI == "" {
		return nil, errors.New("missing required argument: uri")
	}
	return json.Marshal(map[string]string{"uri": args.URI})
}

// mapReadResourceResult converts a resources/read response body into
// the shape a tools/call response should have. Input result:
//
//	{"contents":[{"uri":..., "mimeType":..., "text":"..."}, ...]}
//
// Output result:
//
//	{"content":[{"type":"text","text":"..."}, ...]}
//
// On parse failure the body is returned unchanged so a malformed
// subprocess response still reaches the caller instead of being lost.
func mapReadResourceResult(respBody json.RawMessage) json.RawMessage {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &msg); err != nil {
		return respBody
	}
	resultRaw, ok := msg["result"]
	if !ok {
		return respBody
	}
	var result struct {
		Contents []struct {
			Text string `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return respBody
	}
	content := make([]map[string]string, 0, len(result.Contents))
	for _, c := range result.Contents {
		content = append(content, map[string]string{
			"type": "text",
			"text": c.Text,
		})
	}
	newResult, err := json.Marshal(map[string]any{"content": content})
	if err != nil {
		return respBody
	}
	msg["result"] = newResult
	out, err := json.Marshal(msg)
	if err != nil {
		return respBody
	}
	return out
}

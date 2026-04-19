package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Registry entries for the prompts-capability bridge. Two synthetic
// tools because prompts need both discovery AND materialization:
//
//	__list_prompts__  → prompts/list        (zero-arg discovery)
//	__get_prompt__    → prompts/get         (parameterized fetch)
//
// (resources have the same split in MCP — resources/list is already
// reachable via __read_resource__'s advertised discovery contract, so
// only __read_resource__ ships. For prompts we expose discovery as a
// distinct tool because the get call requires prompt-specific args the
// agent cannot guess without first listing.)

// --- __list_prompts__ ---

const listPromptsToolDefinition = `{"name":"__list_prompts__","description":"List all MCP prompts the server exposes. Each entry carries {name, description, arguments[]} where each argument has {name, description, required}. Call this FIRST when you don't know which prompts exist — then use __get_prompt__(name, arguments) to materialize one. Response is a JSON array as text.","inputSchema":{"type":"object","properties":{}}}`

func listPromptsSyntheticTool() SyntheticTool {
	return SyntheticTool{
		Name:           "__list_prompts__",
		Capability:     "prompts",
		Definition:     json.RawMessage(listPromptsToolDefinition),
		UpstreamMethod: "prompts/list",
		MapArgs:        mapListPromptsArgs,
		MapResult:      mapListPromptsResult,
	}
}

// mapListPromptsArgs rejects any argument (prompts/list takes no
// params) but is lenient about empty / omitted arguments, since agents
// may pass an empty object by convention.
func mapListPromptsArgs(toolArgs json.RawMessage) (json.RawMessage, error) {
	// Empty or {} is fine; reject anything with explicit keys to
	// surface accidental misuse early.
	if len(toolArgs) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(toolArgs, &probe); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	// Silently drop any keys — prompts/list has no params in the MCP spec.
	return json.RawMessage(`{}`), nil
}

// mapListPromptsResult converts a prompts/list response body into
// tools/call content. Input result: {"prompts":[...]}. Output result:
// single text content block containing the JSON-encoded prompts array.
//
// The agent treats the returned text as JSON and can parse it locally
// to drive subsequent __get_prompt__ calls. This is simpler than
// exploding each prompt into its own content block and preserves all
// argument-schema information.
func mapListPromptsResult(respBody json.RawMessage) json.RawMessage {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &msg); err != nil {
		return respBody
	}
	resultRaw, ok := msg["result"]
	if !ok {
		return respBody
	}
	var result struct {
		Prompts json.RawMessage `json:"prompts"`
	}
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return respBody
	}
	// Ensure there's at least an empty array so the agent can parse it.
	text := string(result.Prompts)
	if text == "" {
		text = "[]"
	}
	newResult, err := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
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

// --- __get_prompt__ ---

const getPromptToolDefinition = `{"name":"__get_prompt__","description":"Materialize an MCP prompt by name. Returns the prompt's messages array — a chat-style template {role, content} with role in {user, assistant}. Discover valid names and required arguments via __list_prompts__ first. Response is a JSON array of messages as text.","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Prompt name (from __list_prompts__)."},"arguments":{"type":"object","description":"Named arguments for the prompt. Shape depends on the specific prompt — consult __list_prompts__ for each prompt's argument schema.","additionalProperties":true}},"required":["name"]}}`

func getPromptSyntheticTool() SyntheticTool {
	return SyntheticTool{
		Name:           "__get_prompt__",
		Capability:     "prompts",
		Definition:     json.RawMessage(getPromptToolDefinition),
		UpstreamMethod: "prompts/get",
		MapArgs:        mapGetPromptArgs,
		MapResult:      mapGetPromptResult,
	}
}

// mapGetPromptArgs forwards the tool arguments to prompts/get params.
// MCP prompts/get params and the synthetic tool arguments share the
// same shape ({name, arguments}), so this is essentially a passthrough
// with validation that name is present.
func mapGetPromptArgs(toolArgs json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(toolArgs) > 0 {
		if err := json.Unmarshal(toolArgs, &args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	if args.Name == "" {
		return nil, errors.New("missing required argument: name")
	}
	// Build clean params — drop any extra top-level keys the agent might have sent.
	params := map[string]any{"name": args.Name}
	if len(args.Arguments) > 0 {
		params["arguments"] = args.Arguments
	}
	return json.Marshal(params)
}

// mapGetPromptResult converts a prompts/get response body into
// tools/call content. Input result:
//
//	{"description"?: "...", "messages": [{"role": "user", "content": {"type":"text", "text":"..."}}, ...]}
//
// Output result: the messages[] array serialized as a single JSON text
// block. The agent parses it locally to reconstruct the chat template.
//
// Serializing the whole messages array (not just the text parts)
// preserves multi-modal content (image, audio, resource) semantically
// — the agent still sees it even though tools/call's canonical content
// types may not cover every MCP prompt-content shape.
func mapGetPromptResult(respBody json.RawMessage) json.RawMessage {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(respBody, &msg); err != nil {
		return respBody
	}
	resultRaw, ok := msg["result"]
	if !ok {
		return respBody
	}
	var result struct {
		Messages json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(resultRaw, &result); err != nil {
		return respBody
	}
	text := string(result.Messages)
	if text == "" {
		text = "[]"
	}
	newResult, err := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
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

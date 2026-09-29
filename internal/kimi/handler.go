package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type runExecutor interface {
	Run(context.Context, RunRequest) (RunOutput, error)
}

type requestWire struct {
	Topic            string            `json:"topic"`
	PromptFile       string            `json:"prompt_file"`
	Cwd              string            `json:"cwd"`
	TaskClass        string            `json:"task_class"`
	Role             string            `json:"role"`
	Effort           string            `json:"effort,omitempty"`
	TimeoutSeconds   *int              `json:"timeout_seconds,omitempty"`
	CapabilityChoice *CapabilityChoice `json:"capability_choice,omitempty"`
}

func decodeRunRequest(arguments any) (RunRequest, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		return RunRequest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire requestWire
	if err := decoder.Decode(&wire); err != nil {
		return RunRequest{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return RunRequest{}, err
	}
	if wire.Topic == "" || wire.PromptFile == "" || wire.Cwd == "" || wire.TaskClass == "" || wire.Role == "" ||
		!filepath.IsAbs(wire.PromptFile) || !filepath.IsAbs(wire.Cwd) {
		return RunRequest{}, errors.New("invalid required Kimi request field")
	}
	info, err := os.Stat(wire.Cwd)
	if err != nil || !info.IsDir() {
		return RunRequest{}, errors.New("invalid Kimi working directory")
	}
	effort := wire.Effort
	if effort == "" {
		effort = "high"
	}
	if effort != "high" && effort != "max" {
		return RunRequest{}, errors.New("invalid Kimi effort")
	}
	timeout := maxTimeoutSeconds
	if wire.TimeoutSeconds != nil {
		timeout = *wire.TimeoutSeconds
	}
	if timeout <= 0 || timeout > maxTimeoutSeconds {
		return RunRequest{}, errors.New("invalid Kimi timeout")
	}
	choice := CapabilityChoice{Kind: "none"}
	if wire.CapabilityChoice != nil {
		choice = *wire.CapabilityChoice
	}
	switch choice.Kind {
	case "none":
		if choice.File != "" {
			return RunRequest{}, errors.New("invalid Kimi capability choice")
		}
	case "file":
		if choice.File == "" || !filepath.IsAbs(choice.File) {
			return RunRequest{}, errors.New("invalid Kimi capability file")
		}
	default:
		return RunRequest{}, errors.New("invalid Kimi capability choice")
	}
	return RunRequest{
		Topic: wire.Topic, PromptFile: wire.PromptFile, Cwd: wire.Cwd,
		TaskClass: wire.TaskClass, Role: wire.Role, Effort: effort,
		TimeoutSeconds: timeout, CapabilityChoice: choice,
	}, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func runKimiInputSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"topic":           map[string]any{"type": "string"},
			"prompt_file":     map[string]any{"type": "string", "description": "Absolute prompt-file path."},
			"cwd":             map[string]any{"type": "string", "description": "Absolute existing working directory."},
			"task_class":      map[string]any{"type": "string"},
			"role":            map[string]any{"type": "string"},
			"effort":          map[string]any{"type": "string", "enum": []string{"high", "max"}, "default": "high"},
			"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": maxTimeoutSeconds},
			"capability_choice": map[string]any{
				"oneOf": []any{
					map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"kind": map[string]any{"const": "none"}}, "required": []string{"kind"}},
					map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"kind": map[string]any{"const": "file"}, "file": map[string]any{"type": "string"}}, "required": []string{"kind", "file"}},
				},
				"default": map[string]any{"kind": "none"},
			},
		},
		"required": []string{"topic", "prompt_file", "cwd", "task_class", "role"},
	}
}

func (s *Server) runKimiTool(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	runRequest, err := decodeRunRequest(request.Params.Arguments)
	if err != nil {
		return failureResult(newFailure("E_KIMI_REQUEST_INVALID", err)), nil
	}
	output, err := s.runner.Run(ctx, runRequest)
	if err != nil {
		var failure *Failure
		if !errors.As(err, &failure) {
			failure = newFailure("E_KIMI_RUN_FAILED", err)
		}
		return failureResult(failure), nil
	}
	return &mcp.CallToolResult{
		IsError: output.Receipt.IsError(),
		Content: []mcp.Content{&mcp.TextContent{Text: output.Receipt.Payload}},
	}, nil
}

func failureResult(failure *Failure) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: string(failure.JSON())}},
	}
}

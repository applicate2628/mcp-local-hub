package kimi

import (
	"path/filepath"
	"testing"
)

func validArguments(root string) map[string]any {
	return map[string]any{
		"topic":       "bounded-review",
		"prompt_file": filepath.Join(root, "prompt.md"),
		"cwd":         root,
		"task_class":  "review",
		"role":        "external-reviewer",
	}
}

func TestDecodeRunRequestDefaultsHighAndRejectsUnsafeShapes(t *testing.T) {
	root := t.TempDir()
	args := validArguments(root)
	req, err := decodeRunRequest(args)
	if err != nil {
		t.Fatalf("decodeRunRequest: %v", err)
	}
	if req.Effort != "high" || req.TimeoutSeconds != maxTimeoutSeconds || req.CapabilityChoice.Kind != "none" {
		t.Fatalf("defaults = %+v", req)
	}

	bad := []map[string]any{
		func() map[string]any { v := validArguments(root); v["effort"] = "low"; return v }(),
		func() map[string]any { v := validArguments(root); v["timeout_seconds"] = 0; return v }(),
		func() map[string]any {
			v := validArguments(root)
			v["timeout_seconds"] = maxTimeoutSeconds + 1
			return v
		}(),
		func() map[string]any { v := validArguments(root); v["model"] = "kimi-code/k3"; return v }(),
		func() map[string]any { v := validArguments(root); v["raw_argv"] = []string{"--help"}; return v }(),
		func() map[string]any {
			v := validArguments(root)
			v["capability_choice"] = map[string]any{"kind": "inline", "data": "secret"}
			return v
		}(),
	}
	for i, value := range bad {
		if _, err := decodeRunRequest(value); err == nil {
			t.Fatalf("bad request %d accepted: %#v", i, value)
		}
	}
}

func TestDecodeRunRequestAcceptsExplicitMaxAndAbsoluteCapabilityFile(t *testing.T) {
	root := t.TempDir()
	args := validArguments(root)
	args["effort"] = "max"
	args["timeout_seconds"] = 30
	args["capability_choice"] = map[string]any{"kind": "file", "file": filepath.Join(root, "capabilities.json")}
	req, err := decodeRunRequest(args)
	if err != nil {
		t.Fatalf("decodeRunRequest: %v", err)
	}
	if req.Effort != "max" || req.TimeoutSeconds != 30 || req.CapabilityChoice.Kind != "file" {
		t.Fatalf("decoded = %+v", req)
	}
}

func TestRunKimiInputSchemaIsClosed(t *testing.T) {
	schema := runKimiInputSchema()
	if schema["type"] != "object" || schema["additionalProperties"] != false {
		t.Fatalf("schema is not closed: %#v", schema)
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 5 {
		t.Fatalf("required = %#v", schema["required"])
	}
}

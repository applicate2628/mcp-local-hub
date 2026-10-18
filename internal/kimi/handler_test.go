package kimi

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fixedRunExecutor struct {
	output RunOutput
	err    error
}

func (f fixedRunExecutor) Run(context.Context, RunRequest) (RunOutput, error) {
	return f.output, f.err
}

func roundTripKimiTool(t *testing.T, executor runExecutor) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()
	server := newServer(executor)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	defer func() {
		if err := serverSession.Close(); err != nil {
			t.Errorf("server session close failed: %v", err)
		}
	}()
	client := mcp.NewClient(&mcp.Implementation{Name: "kimi-test-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer func() {
		if err := clientSession.Close(); err != nil {
			t.Errorf("client session close failed: %v", err)
		}
	}()
	result, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "run_kimi", Arguments: validArguments(t.TempDir())})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	return result
}

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

func TestRunKimiCleanupFailureRoundTripsAcquiredReceiptAndCleanShape(t *testing.T) {
	record := ownerReceiptRecord(t, "high", "synthetic full owner result")
	receipt, err := parseProviderReceipt([]byte(record), "high", CapabilityChoice{Kind: "none"})
	if err != nil {
		t.Fatalf("parse synthetic receipt: %v", err)
	}
	payload := receipt.Payload

	failure := newFailure("E_KIMI_CLEANUP_FAILED", errors.Join(errors.New("primary cause"), errors.New("cleanup cause")))
	failure.primaryFailureID = "E_KIMI_RUN_FAILED"
	failure.cleanupFailures = []string{"receipt_directory"}
	failedResult := roundTripKimiTool(t, fixedRunExecutor{output: RunOutput{Receipt: receipt}, err: failure})
	if !failedResult.IsError || len(failedResult.Content) != 2 {
		t.Fatalf("cleanup result is_error=%t content_count=%d", failedResult.IsError, len(failedResult.Content))
	}
	failureText, ok := failedResult.Content[0].(*mcp.TextContent)
	if !ok || failureText.Text != `{"failure_id":"E_KIMI_CLEANUP_FAILED","primary_failure_id":"E_KIMI_RUN_FAILED","cleanup_failures":["receipt_directory"]}` {
		t.Fatalf("failure content = %#v", failedResult.Content[0])
	}
	receiptText, ok := failedResult.Content[1].(*mcp.TextContent)
	if !ok || receiptText.Text != payload {
		t.Fatalf("receipt content was not preserved byte-for-byte (text_content=%t payload_equal=%t)", ok, ok && receiptText.Text == payload)
	}

	cleanResult := roundTripKimiTool(t, fixedRunExecutor{output: RunOutput{Receipt: receipt}})
	if cleanResult.IsError || len(cleanResult.Content) != 1 {
		t.Fatalf("clean result is_error=%t content_count=%d", cleanResult.IsError, len(cleanResult.Content))
	}
	cleanText, ok := cleanResult.Content[0].(*mcp.TextContent)
	if !ok || cleanText.Text != payload {
		t.Fatalf("clean receipt content mismatch (text_content=%t payload_equal=%t)", ok, ok && cleanText.Text == payload)
	}
}

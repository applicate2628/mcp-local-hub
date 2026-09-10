package oneapirun

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestRunInOneAPIEnvTool_StartReturnsAcceptedReceipt catches dispatch that
// treats action=start as the legacy synchronous run instead of returning the
// durable asynchronous acceptance receipt.
func TestRunInOneAPIEnvTool_StartReturnsAcceptedReceipt(t *testing.T) {
	command, args, _ := trivialEcho()
	rs := newTestServer(nil, false, nil)
	owner, err := newAsyncRunOwner(t.Context(), asyncOwnerOptions{
		StatePath: t.TempDir() + "/runs-v1.json",
		Execute: func(context.Context, oneAPIRunRequest) asyncExecution {
			return asyncExecution{Result: runResult{ExitCode: 0, Stdout: "fixture"}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	rs.asyncOwner = owner
	raw, err := json.Marshal(map[string]any{
		"action":          "start",
		"command":         command,
		"args":            args,
		"idempotency_key": "caller-loss-1",
		"timeout_sec":     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := rs.runInOneAPIEnvTool(t.Context(), (&mockCallToolRequest{Arguments: raw}).toReal())
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	if result.IsError || len(result.Content) != 1 {
		t.Fatalf("start result = %#v, want accepted receipt", result)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("start JSON: %v", err)
	}
	if got["state"] != "accepted" || got["terminal"] != false || got["replayed"] != false || got["run_id"] == "" {
		t.Fatalf("start receipt = %s, want accepted async run receipt", text)
	}
}

// TestOneAPIRunInputSchema_AsyncQueriesNeedNoCommand catches an advertised
// schema that blocks a valid status/result/cancel follow-up, or permits fields
// the action-specific handler must reject.
func TestOneAPIRunInputSchema_AsyncQueriesNeedNoCommand(t *testing.T) {
	raw, err := json.Marshal(oneAPIRunInputSchema())
	if err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, valid := range []map[string]any{
		{"action": "status", "run_id": "run-1"},
		{"action": "result", "run_id": "run-1"},
		{"action": "cancel", "run_id": "run-1", "confirm": true},
	} {
		if err := resolved.Validate(valid); err != nil {
			t.Fatalf("valid async query %#v rejected: %v", valid, err)
		}
	}
	if err := resolved.Validate(map[string]any{"action": "status", "run_id": "run-1", "command": "must-not-be-ignored"}); err == nil {
		t.Fatal("status schema accepted irrelevant command")
	}
}

func TestNewOneAPIRunServer_GateOffSkipsStateRootAndOwner(t *testing.T) {
	called := false
	rs, err := newOneAPIRunServer(t.Context(), false, func() (string, error) {
		called = true
		return "", context.DeadlineExceeded
	})
	if err != nil {
		t.Fatalf("gate-off server: %v", err)
	}
	if called || rs.asyncOwner != nil {
		t.Fatalf("gate-off constructed state owner: called=%v owner=%v", called, rs.asyncOwner)
	}
}

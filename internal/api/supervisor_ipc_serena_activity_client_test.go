package api

import (
	"errors"
	"testing"
	"time"
)

func TestDecodeSerenaActivityCommitResponse_UnknownCommandIsExplicitlyUnsupported(t *testing.T) {
	_, err := decodeSerenaActivityCommitResponse(
		IPCRequest{ID: 7},
		SerenaActivityCommitRequestV1{},
		supervisorIPCRawResponse{ID: 7, Error: &IPCErr{Code: "UNKNOWN_COMMAND", Message: "unknown IPC command: commit_serena_activity"}},
	)
	if !errors.Is(err, ErrSerenaActivityCommitUnsupported) {
		t.Fatalf("err = %v, want explicit mixed-version unsupported error", err)
	}
}

func TestValidateSerenaActivityCommitRequest_LegacyRequiresExplicitMarker(t *testing.T) {
	base := SerenaActivityCommitRequestV1{ProtocolVersion: 1, WorkspaceKey: "key", WorkspacePath: "/workspace", TaskName: `\mcp-local-hub-serena-key`, ExpectedPort: 9301, ActivityAt: time.Now().UTC()}
	if err := validateSerenaActivityCommitRequest(base); err == nil {
		t.Fatal("zero generation without explicit legacy marker must be rejected")
	}
	base.LegacyGenerationUnspecified = true
	if err := validateSerenaActivityCommitRequest(base); err != nil {
		t.Fatalf("explicit legacy generation request rejected: %v", err)
	}
	base.RegisteredAt = time.Now().UTC()
	if err := validateSerenaActivityCommitRequest(base); err == nil {
		t.Fatal("legacy marker with nonzero generation must be rejected")
	}
}

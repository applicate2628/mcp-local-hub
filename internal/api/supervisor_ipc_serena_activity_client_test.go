package api

import (
	"errors"
	"testing"
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

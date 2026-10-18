package api

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Exact selected declarations from the frozen Backend producer. The broader
// stop controller and its effects are outside this consumer integration.
type StopBatchCauseV2 struct {
	Code        string `json:"code"`
	Stage       string `json:"stage"`
	TargetIndex *int   `json:"target_index"`
}

func stopBatchV2SHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

package api

import (
	"crypto/sha256"
	"encoding/hex"
)

// ManifestHashContent computes SHA-256 of a manifest file's bytes.
// Returned as lower-case hex, always 64 chars. Shared between
// ManifestGet (returned to client) and ManifestEdit (compared against
// client's expectedHash to detect external writes between Load and Save).
//
// We hash raw bytes, not parsed YAML, so whitespace/formatting changes
// from other editors are visible to the stale-detection flow. That is
// the intent — users should see "someone reformatted your manifest"
// as a stale event, not silently accept it.
func ManifestHashContent(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

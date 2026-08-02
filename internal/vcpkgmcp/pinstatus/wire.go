package pinstatus

import "encoding/json"

// MarshalJSON is the final pin-status public-result boundary. PinStatus
// redacts values as it produces them, and this second call is deliberately
// independent: early/error/cache-like callers that serialize a Result cannot
// reintroduce raw remote metadata by bypassing the normal producer path.
func (r Result) MarshalJSON() ([]byte, error) {
	type publicResult Result
	return json.Marshal(publicResult(redactResult(r)))
}

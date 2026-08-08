// Package publicresult owns the universal encoded-response boundary for vcpkg
// MCP tool results. Tool packages own semantic retention; this leaf measures
// the exact indented JSON text that the MCP server publishes.
package publicresult

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
)

// AbbreviateEncoded retains an unchanged string when its JSON encoding fits
// allowance. Otherwise it retains only a rune-safe prefix plus deterministic
// byte length and digest, never a caller-controlled suffix.
func AbbreviateEncoded(value string, allowance int) string {
	encoded, _ := json.Marshal(value)
	if len(encoded) <= allowance {
		return value
	}
	suffix := fmt.Sprintf("… [bytes=%d sha256=%x]", len(value), sha256.Sum256([]byte(value)))
	runes := []rune(value)
	best := suffix
	low, high := 0, len(runes)
	for low <= high {
		middle := low + (high-low)/2
		candidate := string(runes[:middle]) + suffix
		encoded, _ = json.Marshal(candidate)
		if len(encoded) <= allowance {
			best = candidate
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	return best
}

// MaxEncodedBytes is the largest indented JSON body published by a vcpkg MCP
// tool. The value applies to bytes, not runes or an estimated token count.
const MaxEncodedBytes = 256 << 10

var ErrBudgetInvariant = errors.New("VCPKG_RESULT_BUDGET_INVARIANT")

// OmissionReason is the closed vocabulary for an intentionally incomplete
// public projection.
type OmissionReason string

const InternalProjectionLimit OmissionReason = "internal_projection_limit"

// Omission names a semantic field the owning tool omitted from a minimal
// response. Omitted stays absent when the total is not knowable.
type Omission struct {
	Field    string         `json:"field"`
	Reason   OmissionReason `json:"reason"`
	Retained int            `json:"retained"`
	Omitted  *int           `json:"omitted,omitempty"`
}

// Projection is the additive wire metadata emitted only for a reduced result.
type Projection struct {
	Complete  bool       `json:"complete"`
	Omissions []Omission `json:"omissions,omitempty"`
}

// MinimalProjection records one whole-result semantic omission. Packages can
// build a more detailed projection when they retain an ordered prefix.
func MinimalProjection(field string) Projection {
	return Projection{Complete: false, Omissions: []Omission{{
		Field: field, Reason: InternalProjectionLimit,
	}}}
}

// Projectable is implemented by every registered vcpkg tool result. The
// implementation belongs to the package that owns field priority and must
// return a self-contained valid JSON value below the universal budget.
type Projectable interface {
	PublicResultProjection() any
}

// MarshalIndent first measures the ordinary public result and, only when it
// exceeds MaxEncodedBytes, asks its package-owned projector for a minimal
// result. It never slices JSON or reflects over fields to invent a projection.
func MarshalIndent(result Projectable) ([]byte, error) {
	body, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(body) <= MaxEncodedBytes {
		return body, nil
	}

	body, err = json.MarshalIndent(result.PublicResultProjection(), "", "  ")
	if err != nil {
		return nil, err
	}
	if len(body) > MaxEncodedBytes {
		return nil, ErrBudgetInvariant
	}
	return body, nil
}

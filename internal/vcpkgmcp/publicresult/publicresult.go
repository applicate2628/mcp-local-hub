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
	ProjectionAdmitter
	PublicResultProjection() any
}

// ProjectionAdmitter lets a semantic owner prove that an aggregate cannot fit
// the public budget without first materializing its complete JSON encoding.
// Implementations must be deterministic: true must prove the complete result
// cannot fit, while false must leave MarshalIndent with a bounded input.
type ProjectionAdmitter interface {
	PublicResultRequiresProjection(limit int) bool
}

// ProjectionAdmission is a saturating lower-bound counter for semantic owners.
// Owners add a scalar envelope and aggregate elements individually, so a large
// collection is rejected without ever materializing its complete JSON form.
// Every added value is disjoint wire content; JSON field names and array
// framing may be omitted because that only makes the lower bound smaller.
type ProjectionAdmission struct {
	remaining int
	required  bool
}

// NewProjectionAdmission creates a fail-closed admission counter.
func NewProjectionAdmission(limit int) ProjectionAdmission {
	return ProjectionAdmission{remaining: limit, required: limit < 0}
}

// AddJSON adds one bounded, disjoint value to the encoded-size lower bound.
// Encoding failure also requires projection so the complete aggregate is not
// attempted through the ordinary path.
func (a *ProjectionAdmission) AddJSON(value any) bool {
	if a.required {
		return true
	}
	body, err := json.Marshal(value)
	if err != nil || len(body) > a.remaining {
		a.required = true
		return true
	}
	a.remaining -= len(body)
	return false
}

// RequiresProjection reports whether the accumulated lower bound exceeds the
// caller's public-result limit.
func (a ProjectionAdmission) RequiresProjection() bool { return a.required }

// MarshalIndent requires owner-supplied bounded pre-admission before measuring
// the ordinary public result. Oversized values use the package-owned
// projection; JSON is never sliced or reflected over.
func MarshalIndent(result Projectable) ([]byte, error) {
	if result.PublicResultRequiresProjection(MaxEncodedBytes) {
		return marshalProjection(result)
	}

	body, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(body) <= MaxEncodedBytes {
		return body, nil
	}

	return marshalProjection(result)
}

func marshalProjection(result Projectable) ([]byte, error) {
	body, err := json.MarshalIndent(result.PublicResultProjection(), "", "  ")
	if err != nil {
		return nil, err
	}
	if len(body) > MaxEncodedBytes {
		return nil, ErrBudgetInvariant
	}
	return body, nil
}

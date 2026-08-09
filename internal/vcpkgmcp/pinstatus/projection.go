package pinstatus

import (
	"encoding/json"

	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

// PublicResultRequiresProjection measures the complete compact wire shape one
// bounded port row at a time. Compact JSON is a lower bound for the indented
// public encoding, so exceeding limit proves that the full result cannot fit
// without ever materializing the aggregate document.
func (r Result) PublicResultRequiresProjection(limit int) bool {
	if limit < 0 {
		return true
	}
	type publicResult Result
	envelope := publicResult(r)
	envelope.Ports = nil
	body, err := json.Marshal(envelope)
	if err != nil {
		return true
	}
	// Replace the envelope's literal null with the [] framing used below.
	encodedBytes := len(body) - len("null") + len("[]")
	for index := range r.Ports {
		row, err := json.Marshal(redactPortResult(r.Ports[index]))
		if err != nil {
			return true
		}
		if index != 0 {
			encodedBytes++
		}
		if encodedBytes > limit || len(row) > limit-encodedBytes {
			return true
		}
		encodedBytes += len(row)
	}
	return encodedBytes > limit
}

// PublicResultProjection preserves the batch verdict while omitting port rows
// only after the complete package-owned result exceeds the shared ceiling.
func (r Result) PublicResultProjection() any {
	failures := make([]projectedFailurePort, 0)
	for index, port := range r.Ports {
		if port.Failure == nil {
			continue
		}
		failures = append(failures, projectedFailurePort{
			PortIndex: index,
			Status:    port.Status,
			Reason:    port.Reason,
			Failure:   port.Failure,
		})
	}

	var selected any
	for retained := 0; retained <= len(failures); retained++ {
		candidate := projectedPinStatusResult(r, failures[:retained], retained, len(failures))
		body, err := json.MarshalIndent(candidate, "", "  ")
		if err != nil || len(body) > publicresult.MaxEncodedBytes {
			continue
		}
		selected = candidate
	}
	if selected == nil || (len(failures) > 0 && !projectionHasCausalTuple(selected)) {
		return budgetInvariantProjection{}
	}
	return selected
}

type projectedFailurePort struct {
	PortIndex int            `json:"port_index"`
	Status    Status         `json:"status"`
	Reason    Reason         `json:"reason,omitempty"`
	Failure   *PublicFailure `json:"failure"`
}

type projectedPinStatus struct {
	Status           Status                  `json:"status"`
	Reason           BatchReason             `json:"reason,omitempty"`
	Failure          *PublicFailure          `json:"failure,omitempty"`
	FailurePorts     []projectedFailurePort  `json:"failure_ports,omitempty"`
	ResultProjection publicresult.Projection `json:"result_projection"`
}

func projectedPinStatusResult(r Result, failures []projectedFailurePort, retained, total int) projectedPinStatus {
	portsOmitted := len(r.Ports)
	failuresOmitted := total - retained
	return projectedPinStatus{
		Status:       r.Status,
		Reason:       r.Reason,
		Failure:      r.Failure,
		FailurePorts: failures,
		ResultProjection: publicresult.Projection{Complete: false, Omissions: []publicresult.Omission{
			{Field: "ports", Reason: publicresult.InternalProjectionLimit, Retained: 0, Omitted: &portsOmitted},
			{Field: "failure_causal_rows", Reason: publicresult.InternalProjectionLimit, Retained: retained, Omitted: &failuresOmitted},
		}},
	}
}

func projectionHasCausalTuple(value any) bool {
	projection, ok := value.(projectedPinStatus)
	return ok && len(projection.FailurePorts) > 0
}

// budgetInvariantProjection has no wire form: the shared serializer receives
// ErrBudgetInvariant instead of publishing a reduced result with no causal row.
type budgetInvariantProjection struct{}

func (budgetInvariantProjection) MarshalJSON() ([]byte, error) {
	return nil, publicresult.ErrBudgetInvariant
}

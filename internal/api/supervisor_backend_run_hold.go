package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"time"
)

var (
	ErrSupervisorBackendHeld       = errors.New("supervisor backend was intentionally stopped")
	ErrSupervisorBackendHoldInvalid = errors.New("invalid supervisor backend stop record")
)

type SupervisorBackendStopOutcome string

const (
	SupervisorBackendStopPending  SupervisorBackendStopOutcome = "pending"
	SupervisorBackendStopNoChild  SupervisorBackendStopOutcome = "no-child"
	SupervisorBackendStopReleased SupervisorBackendStopOutcome = "released"
)

// SupervisorBackendStopWork names one original creator operation and its
// outcome. The phase names the outstanding creator-side obligation.
type SupervisorBackendStopWork struct {
	Operation    uint64                       `json:"operation"`
	Task         string                       `json:"task"`
	Outcome      SupervisorBackendStopOutcome `json:"outcome"`
	PendingPhase string                       `json:"pending_phase,omitempty"`
}

// SupervisorBackendRunHold stores one current operation in supervisor-intent.
// An absent target requires the original observed quiet singleton. A live
// target binds the original PID and kernel birth through the singleton owner.
type SupervisorBackendRunHold struct {
	Version      int                         `json:"version"`
	Installation string                      `json:"installation"`
	StopID       string                      `json:"stop_id"`
	Absent       bool                        `json:"absent,omitempty"`
	Original     SupervisorLockOwner         `json:"original,omitzero"`
	Settled      bool                        `json:"settled"`
	Work         []SupervisorBackendStopWork `json:"work,omitempty"`
}

func (hold SupervisorBackendRunHold) IsZero() bool {
	return hold.Version == 0 && hold.Installation == "" && hold.StopID == "" &&
		!hold.Absent && hold.Original == (SupervisorLockOwner{}) && !hold.Settled && hold.Work == nil
}

func (hold SupervisorBackendRunHold) clone() SupervisorBackendRunHold {
	if len(hold.Work) == 0 {
		hold.Work = nil
	} else {
		hold.Work = slices.Clone(hold.Work)
	}
	return hold
}

func (hold *SupervisorBackendRunHold) UnmarshalJSON(raw []byte) error {
	type wireHold SupervisorBackendRunHold
	if rejectStopBatchV2DuplicateKeys(raw) != nil || requireStopBatchV2ExactKeys(raw, reflect.TypeOf(wireHold{})) != nil {
		return ErrSupervisorBackendHoldInvalid
	}
	var value wireHold
	wire := struct {
		*wireHold
		Settled *bool `json:"settled"`
	}{wireHold: &value}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil || wire.Settled == nil {
		return ErrSupervisorBackendHoldInvalid
	}
	value.Settled = *wire.Settled
	parsed := SupervisorBackendRunHold(value)
	if err := parsed.validate(); err != nil {
		return err
	}
	*hold = parsed
	return nil
}

func (hold SupervisorBackendRunHold) validate() error {
	if hold.Version != 1 || !isLowerHex64(hold.Installation) || !isLowerHex64(hold.StopID) {
		return ErrSupervisorBackendHoldInvalid
	}
	if hold.Absent {
		if hold.Original != (SupervisorLockOwner{}) || !hold.Settled || len(hold.Work) != 0 {
			return ErrSupervisorBackendHoldInvalid
		}
		return nil
	}
	startedAt, err := time.Parse(time.RFC3339Nano, hold.Original.StartedAt)
	if err != nil || startedAt.IsZero() || hold.Original.PID <= 0 {
		return ErrSupervisorBackendHoldInvalid
	}
	var previous uint64
	for _, work := range hold.Work {
		if work.Operation <= previous || strings.TrimSpace(work.Task) == "" {
			return ErrSupervisorBackendHoldInvalid
		}
		switch work.Outcome {
		case SupervisorBackendStopPending:
			if hold.Settled || strings.TrimSpace(work.PendingPhase) == "" {
				return ErrSupervisorBackendHoldInvalid
			}
		case SupervisorBackendStopNoChild, SupervisorBackendStopReleased:
			if work.PendingPhase != "" {
				return ErrSupervisorBackendHoldInvalid
			}
		default:
			return ErrSupervisorBackendHoldInvalid
		}
		previous = work.Operation
	}
	return nil
}

// BackendStartError reports the persisted hold to guarded callers.
// A present hold blocks normal start regardless of settlement progress.
func (f *SupervisorIntentFile) BackendStartError() error {
	if f == nil || f.BackendRunHold.IsZero() {
		return nil
	}
	if err := f.BackendRunHold.validate(); err != nil {
		return err
	}
	return ErrSupervisorBackendHeld
}

// The legacy intent document keeps its original vocabulary. With a hold
// present, its reserved member must have exactly its canonical spelling.
func validateSupervisorBackendHoldDocument(raw []byte) error {
	if rejectStopBatchV2DuplicateKeys(raw) != nil {
		return ErrSupervisorBackendHoldInvalid
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ErrSupervisorBackendHoldInvalid
	}
	for name := range fields {
		if name != "backend_run_hold" && strings.EqualFold(name, "backend_run_hold") {
			return ErrSupervisorBackendHoldInvalid
		}
	}
	return nil
}

// Whole-intent writers may omit the hold in an older caller's snapshot.
// The held current intent is its only authority.
func preserveSupervisorBackendRunHold(current SupervisorBackendRunHold, desired *SupervisorIntentFile) error {
	held := current.clone()
	if proposed := desired.BackendRunHold; !proposed.IsZero() {
		if err := proposed.validate(); err != nil {
			return err
		}
		if !reflect.DeepEqual(proposed.clone(), held) {
			return ErrSupervisorBackendHoldInvalid
		}
	}
	desired.BackendRunHold = held
	return nil
}

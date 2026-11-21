package process

// PIDStatusObservation contains values from one short process observation.
// State describes the final exit check, not continued liveness after return.
// A caller must also check the returned error: Alive alone does not mean that
// the expected image/start time matched or that the observation handle closed.
// ResidentBytes is the working set in bytes, available only when the required
// observation succeeded. No process control or creator authority is conveyed.
type PIDStatusObservation struct {
	State              PIDState
	ResidentBytes      uint64
	ResidentBytesKnown bool
}

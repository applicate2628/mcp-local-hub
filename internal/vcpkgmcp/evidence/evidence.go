// Package evidence defines the tri-state result contract shared by every
// vcpkg-mcp tool: status is always one of ok | failed | unknown(reason), and
// every result carries an Evidence block (absolute paths, exact commands,
// file:line locations) so a caller can verify the answer instead of trusting
// a bare verdict.
//
// Behavioural invariant (work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md,
// "Behavioural invariants"): "Return evidence, not conclusions" and "Never
// hide uncertainty" — a tool must never coerce an unresolvable case into a
// false "ok", and Reason is always drawn from a CLOSED enum owned by the
// tool package that produces it, never free text.
package evidence

// Status is the tri-state outcome every tool result carries. This is the
// single owner of the tri-state vocabulary — every tool package reuses this
// type rather than re-declaring its own ok/failed/unknown strings.
type Status string

const (
	// StatusOK means the tool computed a concrete, verifiable answer.
	StatusOK Status = "ok"
	// StatusFailed means the underlying operation (e.g. a build) genuinely
	// failed and the tool identified the failure with confidence.
	StatusFailed Status = "failed"
	// StatusUnknown means the tool could not safely produce ok or failed.
	// Reason (owned per-tool, see each tool package's Reason enum) MUST be
	// populated whenever Status == StatusUnknown; free-text reasons are
	// never acceptable — an unauditable "reason" is worse than none.
	StatusUnknown Status = "unknown"
)

// Location is one file:line citation.
type Location struct {
	File string `json:"file"`
	// Line is 1-based; 0 means "file-level, no specific line".
	Line int `json:"line,omitempty"`
}

// Evidence is the material a result rests on. Every tool result embeds one
// (even when empty) so the JSON shape is uniform across tools.
type Evidence struct {
	// Paths lists absolute filesystem paths the answer cites (logs read,
	// directories probed, roots resolved).
	Paths []string `json:"paths,omitempty"`
	// Commands lists exact command lines the answer cites (e.g. the
	// recovered vcpkg invocation, a cmake configure command).
	Commands []string `json:"commands,omitempty"`
	// Locations lists file:line citations for specific findings (e.g. the
	// exact line a diagnostic was extracted from).
	Locations []Location `json:"locations,omitempty"`
}

// AddPath appends p to e.Paths if p is non-empty and not already present.
func (e *Evidence) AddPath(p string) {
	if p == "" {
		return
	}
	for _, existing := range e.Paths {
		if existing == p {
			return
		}
	}
	e.Paths = append(e.Paths, p)
}

// AddCommand appends c to e.Commands if c is non-empty.
func (e *Evidence) AddCommand(c string) {
	if c == "" {
		return
	}
	e.Commands = append(e.Commands, c)
}

// AddLocation appends a file:line citation.
func (e *Evidence) AddLocation(file string, line int) {
	if file == "" {
		return
	}
	e.Locations = append(e.Locations, Location{File: file, Line: line})
}

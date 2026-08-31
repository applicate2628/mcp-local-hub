// Package supervisorowner owns the small, dependency-free value contract for
// selecting the long-lived Windows supervisor owner. Both persistence and the
// autostart adapter consume this contract, so it cannot live in either layer.
package supervisorowner

// Mode is the durable ownership topology for the canonical autostart task.
type Mode string

const (
	ModeGUI       Mode = "gui"
	ModeSupervise Mode = "supervise"
)

// Valid reports whether a mode is safe to launch.
func (m Mode) Valid() bool {
	return m == ModeGUI || m == ModeSupervise
}

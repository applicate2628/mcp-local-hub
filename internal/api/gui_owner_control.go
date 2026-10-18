package api

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
)

const (
	GUIRunDesiredKey     = "gui.run_desired"
	GUIAutorunEnabledKey = "gui.autorun_enabled"
	GUIStopCauseKey      = "gui.stop_cause"
	GUIStopGenerationKey = "gui.stop_generation"
	GUIRunRunning        = "running"
	GUIRunStopped        = "stopped"
)

var ErrGUIStoppedByUser = errors.New("GUI_STOPPED_BY_USER: run mcphub gui --resume to start the GUI")
var ErrGUIControlConflict = errors.New("GUI_CONTROL_CONFLICT")
var ErrBootPolicyProjectionUnsupported = errors.New("BOOT_POLICY_PROJECTION_UNSUPPORTED")

// GUIOwnerControl is a settings observation, not a GUI lease or native proof.
// Nil AutorunEnabled preserves the existing verified legacy Task convention.
type GUIOwnerControl struct {
	RunDesired     string
	AutorunEnabled *bool
	StopCause      string
	StopGeneration string
}

type GUIOwnerControlResult struct {
	Control GUIOwnerControl
	// Applied means the exact typed state was independently read back while
	// holding the original settings flock. A later release error retains it.
	Applied bool
	// NewlyStopped is true only after readback of this invocation's running
	// to stopped transition. It conveys no historical daemon outcome.
	NewlyStopped bool
}

func decodeGUIOwnerControl(raw map[string]string) (GUIOwnerControl, error) {
	c := GUIOwnerControl{RunDesired: GUIRunRunning}
	if value, exists := raw[GUIRunDesiredKey]; exists {
		if value != GUIRunRunning && value != GUIRunStopped {
			return c, errors.New("GUI_CONTROL_INVALID: run desired")
		}
		c.RunDesired = value
	}
	if value, exists := raw[GUIAutorunEnabledKey]; exists {
		if value != "true" && value != "false" {
			return c, errors.New("GUI_CONTROL_INVALID: autorun policy")
		}
		enabled := value == "true"
		c.AutorunEnabled = &enabled
	}
	c.StopCause, c.StopGeneration = raw[GUIStopCauseKey], raw[GUIStopGenerationKey]
	if c.RunDesired == GUIRunRunning && (c.StopCause != "" || c.StopGeneration != "") {
		return c, errors.New("GUI_CONTROL_INVALID: running has stop attribution")
	}
	if c.RunDesired == GUIRunStopped {
		if !validGUIStopCause(c.StopCause) || (c.StopGeneration != "" && !validGUIControlGeneration(c.StopGeneration)) {
			return c, errors.New("GUI_CONTROL_INVALID: stopped attribution")
		}
	}
	return c, nil
}

func validGUIStopCause(cause string) bool {
	return cause == "tray-quit" || cause == "tray-quit-and-stop-all" || cause == "cli-quit" || cause == "cli-quit-and-stop-all"
}

func validGUIControlGeneration(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func equalGUIOwnerControl(a, b GUIOwnerControl) bool {
	if a.RunDesired != b.RunDesired || a.StopCause != b.StopCause || a.StopGeneration != b.StopGeneration {
		return false
	}
	if a.AutorunEnabled == nil || b.AutorunEnabled == nil {
		return a.AutorunEnabled == nil && b.AutorunEnabled == nil
	}
	return *a.AutorunEnabled == *b.AutorunEnabled
}

// WithGUIOwnerControlIn holds the EXISTING settings flock through the operation.
// fn must not reenter settings or wait for a child that needs this flock.
func (a *API) WithGUIOwnerControlIn(path string, fn func(GUIOwnerControl) error) error {
	if fn == nil {
		return errors.New("GUI control operation is required")
	}
	return mutateRawSettingsMapLockedThen(path, nil, func(raw map[string]string) error {
		control, err := decodeGUIOwnerControl(raw)
		if err != nil {
			return err
		}
		return fn(control)
	})
}

func (a *API) WithGUIOwnerControl(fn func(GUIOwnerControl) error) error {
	return a.WithGUIOwnerControlIn(SettingsPath(), fn)
}

// ValidateAutomaticStart is a DATA decision, not settings/lease authority.
// Producers apply it only to a current observation under their original lock.
func (c GUIOwnerControl) ValidateAutomaticStart() error {
	if c.RunDesired == GUIRunStopped {
		return ErrGUIStoppedByUser
	}
	if c.RunDesired != GUIRunRunning {
		return errors.New("GUI_CONTROL_INVALID: run desired")
	}
	return nil
}

// GUIAutomaticStartError retains successful original launch admission when
// the same settings operation subsequently fails to release its flock.
// It does not claim a child started or reached readiness.
type GUIAutomaticStartError struct {
	Admitted bool
	Cause    error
}

func (e *GUIAutomaticStartError) Error() string {
	return fmt.Sprintf("GUI_AUTOMATIC_START_UNCONFIRMED: admitted=%t: %v", e.Admitted, e.Cause)
}
func (e *GUIAutomaticStartError) Unwrap() error { return e.Cause }

// AdmitGUIAutomaticStart serializes the FINAL scheduler admission against Quit.
// The callback may start a queued Task but must not await child readiness.
func (a *API) AdmitGUIAutomaticStart(fn func() error) error {
	if fn == nil {
		return errors.New("GUI start operation is required")
	}
	admitted := false
	err := a.WithGUIOwnerControl(func(c GUIOwnerControl) error {
		if err := c.ValidateAutomaticStart(); err != nil {
			return err
		}
		if err := fn(); err != nil {
			return err
		}
		admitted = true
		return nil
	})
	if err != nil && admitted {
		return &GUIAutomaticStartError{Admitted: true, Cause: err}
	}
	return err
}

// RecordGUIStopIn is called under the genuine active or absent GUI lease.
// Native lifecycle validation belongs to that caller, not this DATA owner.
func (a *API) RecordGUIStopIn(ctx context.Context, path, generation, cause string) (result GUIOwnerControlResult, err error) {
	if ctx == nil {
		return result, errors.New("GUI_QUIT_CONTEXT_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if (generation != "" && !validGUIControlGeneration(generation)) || !validGUIStopCause(cause) {
		return result, errors.New("GUI_CONTROL_INVALID: stop request")
	}
	newlyStopped := false
	err = mutateRawSettingsMapLockedThen(path, func(raw map[string]string) error {
		// FINAL semantic admission: lifecycle and settings acquisition may have
		// waited since the caller's entry check. After this admission, durable
		// write/readback and required shutdown keep their original owners.
		if err := ctx.Err(); err != nil {
			return err
		}
		c, err := decodeGUIOwnerControl(raw)
		if err != nil {
			return err
		}
		if c.RunDesired == GUIRunStopped {
			if generation != "" && c.StopGeneration != generation {
				return ErrGUIControlConflict
			}
			// The first same-generation operation wins, including its breadth.
			result.Control = c
			return nil
		}
		raw[GUIRunDesiredKey], raw[GUIStopCauseKey], raw[GUIStopGenerationKey] = GUIRunStopped, cause, generation
		result.Control, err = decodeGUIOwnerControl(raw)
		newlyStopped = err == nil
		return err
	}, func(raw map[string]string) error {
		c, err := decodeGUIOwnerControl(raw)
		if err != nil {
			return err
		}
		if !equalGUIOwnerControl(c, result.Control) {
			return errors.New("GUI_CONTROL_READBACK_MISMATCH")
		}
		result.Applied = true
		result.NewlyStopped = newlyStopped
		return nil
	})
	if !result.Applied {
		err = errors.Join(err, ctx.Err())
	}
	return result, err
}

func (a *API) RecordGUIStop(ctx context.Context, generation, cause string) (GUIOwnerControlResult, error) {
	return a.RecordGUIStopIn(ctx, SettingsPath(), generation, cause)
}

// ResumeGUIOwnerIn compares the captured policy inside the original RMW. An
// explicit start/recovery holding a GUI lease cannot erase a newer Quit.
func (a *API) ResumeGUIOwnerIn(path string, expected GUIOwnerControl) (result GUIOwnerControlResult, err error) {
	err = mutateRawSettingsMapLockedThen(path, func(raw map[string]string) error {
		c, err := decodeGUIOwnerControl(raw)
		if err != nil {
			return err
		}
		if !equalGUIOwnerControl(c, expected) {
			return ErrGUIControlConflict
		}
		raw[GUIRunDesiredKey] = GUIRunRunning
		delete(raw, GUIStopCauseKey)
		delete(raw, GUIStopGenerationKey)
		result.Control, err = decodeGUIOwnerControl(raw)
		return err
	}, func(raw map[string]string) error {
		c, err := decodeGUIOwnerControl(raw)
		if err != nil {
			return err
		}
		if !equalGUIOwnerControl(c, result.Control) {
			return errors.New("GUI_CONTROL_READBACK_MISMATCH")
		}
		result.Applied = true
		return nil
	})
	return result, err
}

func (a *API) ResumeGUIOwner(expected GUIOwnerControl) (GUIOwnerControlResult, error) {
	return a.ResumeGUIOwnerIn(SettingsPath(), expected)
}

// SetGUIAutorunEnabledIn persists explicit logon preference, preserving run
// intent; project runs only after typed readback under the same settings lock.
func (a *API) SetGUIAutorunEnabledIn(path string, enabled bool, project func(GUIOwnerControl) error) (result GUIOwnerControlResult, err error) {
	if runtime.GOOS != "windows" {
		return result, ErrBootPolicyProjectionUnsupported
	}
	if project == nil {
		return result, errors.New("GUI_AUTORUN_PROJECTOR_REQUIRED")
	}
	err = mutateRawSettingsMapLockedThen(path, func(raw map[string]string) error {
		if _, err := decodeGUIOwnerControl(raw); err != nil {
			return err
		}
		raw[GUIAutorunEnabledKey] = fmt.Sprint(enabled)
		var err error
		result.Control, err = decodeGUIOwnerControl(raw)
		return err
	}, func(raw map[string]string) error {
		c, err := decodeGUIOwnerControl(raw)
		if err != nil {
			return err
		}
		if !equalGUIOwnerControl(c, result.Control) {
			return errors.New("GUI_CONTROL_READBACK_MISMATCH")
		}
		result.Applied = true
		return project(c)
	})
	return result, err
}

func (a *API) SetGUIAutorunEnabled(enabled bool, project func(GUIOwnerControl) error) (GUIOwnerControlResult, error) {
	return a.SetGUIAutorunEnabledIn(SettingsPath(), enabled, project)
}

// GUIStopIncludesDaemons reports the original accepted operation's breadth.
func (c GUIOwnerControl) GUIStopIncludesDaemons() bool {
	return strings.HasSuffix(c.StopCause, "-and-stop-all")
}

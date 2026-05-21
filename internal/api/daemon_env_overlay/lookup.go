package daemon_env_overlay

// LookupOverlay returns a copy of the env map for the daemon identified
// by taskName, or nil if no row matches. Callers MUST treat the result
// as transient — the underlying overlay is loaded once at supervisor
// startup and held by reference; mutating the returned map would alter
// the cached snapshot.
//
// The taskName is normalized via NormalizeOverlayKey before lookup so
// callers can pass either canonical (leading-backslash) or bare form.
// Operator hand-edits to the YAML file that omit the backslash still
// match daemons recorded canonically in supervisor-intent.json.
//
// Returns nil if:
//   - ov is nil (no overlay loaded).
//   - ov.Daemons is empty or unset.
//   - taskName (after normalization) is empty.
//   - no row matches the normalized key.
//
// The function returns a defensive copy so the caller can safely range
// over the result while subsequent calls do not race against it.
func LookupOverlay(ov *Overlay, taskName string) map[string]string {
	if ov == nil || len(ov.Daemons) == 0 {
		return nil
	}
	key := NormalizeOverlayKey(taskName)
	if key == "" {
		return nil
	}
	row, ok := ov.Daemons[key]
	if !ok {
		return nil
	}
	if len(row.Env) == 0 {
		return nil
	}
	out := make(map[string]string, len(row.Env))
	for k, v := range row.Env {
		out[k] = v
	}
	return out
}

package cli

// ensureSupervisorControlCompatibilityFn is the command-boundary seam.  The
// compatibility decision must finish before API Stop/Restart can write intent
// or issue a control verb; tests hold this seam to prove that ordering.
var ensureSupervisorControlCompatibilityFn = ensureSupervisorControlCompatibility

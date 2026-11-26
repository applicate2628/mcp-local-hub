//go:build windows

package scheduler

// WindowsTaskSpecMatchesXML compares an observed task definition with the
// existing Windows task writer's output for the current user. It does not read
// or mutate the scheduler, or establish ownership of an existing registration.
// Callers must observe runtime state separately before reporting it enabled.
func WindowsTaskSpecMatchesXML(observed []byte, spec TaskSpec) (bool, error) {
	ownerSID, err := currentWindowsTaskOwner()
	if err != nil {
		return false, err
	}
	return taskDefinitionsEquivalent(observed, []byte(buildCreateXML(spec, ownerSID)))
}

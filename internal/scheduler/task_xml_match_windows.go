//go:build windows

package scheduler

import "os/user"

// WindowsTaskSpecMatchesXML compares an observed task definition with the
// existing Windows task writer's output for the current user. It does not read
// or mutate the scheduler, or establish ownership of an existing registration.
// The writer leaves the scheduling engine unspecified; native true/false
// materialization is accepted without changing any task settings.
// Callers must observe runtime state separately before reporting it enabled.
func WindowsTaskSpecMatchesXML(observed []byte, spec TaskSpec) (bool, error) {
	ownerSID, err := currentWindowsTaskOwner()
	if err != nil {
		return false, err
	}
	// Current returns the same cached process-owner identity used above.
	// Native LogonTrigger exports can use its full account name instead of SID.
	current, err := user.Current()
	if err != nil {
		return false, err
	}
	return taskDefinitionsEquivalent(observed, []byte(buildCreateXML(spec, ownerSID)), taskDefinitionMatchContext{
		currentSID: ownerSID, currentAccount: current.Username,
	})
}

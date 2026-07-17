package api

// HubPortDependencyState classifies whether resetting the persisted hub port is
// safe. Unknown is distinct from Dependent because an unreadable source might
// contain a live URL pinned to the current port.
type HubPortDependencyState int

const (
	// DependencyStateClear means every applicable client is proved gate-OFF and
	// groups.yaml is missing or valid and empty.
	DependencyStateClear HubPortDependencyState = iota
	// DependencyStateDependent means at least one client or group is proved to
	// depend on the current hub port and no source was unreadable.
	DependencyStateDependent
	// DependencyStateUnknown means at least one client or groups.yaml could not
	// be read or parsed, so reset safety cannot be proven.
	DependencyStateUnknown
)

// HubPortDependencySource identifies one unreadable dependency source for an
// operator-facing refusal message.
type HubPortDependencySource struct {
	Kind string
	Name string
	Err  string
}

// HubPortDependencies is the typed snapshot consumed by hub-port reset
// callers. State is Clear only when no dependency is present and every source
// was read successfully.
type HubPortDependencies struct {
	GatedClients []string
	Groups       []string
	State        HubPortDependencyState
	Errors       []HubPortDependencySource
}

// ProbeHubPortDependencies reports every known consumer of the persisted hub
// port. It keeps ProbeHubGate's unreadable and adapter-construction failure
// sets plus LoadGroups' read or parse error so destructive callers fail closed.
func ProbeHubPortDependencies() HubPortDependencies {
	gate := ProbeHubGate()
	deps := HubPortDependencies{GatedClients: gate.GatedOn}
	for _, name := range gate.Unreadable {
		deps.Errors = append(deps.Errors, HubPortDependencySource{
			Kind: "client",
			Name: name,
			Err:  "config unreadable (parse/DACL)",
		})
	}
	for _, name := range gate.AdapterConstructionFailed {
		deps.Errors = append(deps.Errors, HubPortDependencySource{
			Kind: "client",
			Name: name,
			Err:  "adapter construction failed",
		})
	}

	groups, err := LoadGroups()
	if err != nil {
		deps.Errors = append(deps.Errors, HubPortDependencySource{
			Kind: "groups",
			Name: "groups.yaml",
			Err:  err.Error(),
		})
	} else {
		for _, group := range groups.Groups {
			deps.Groups = append(deps.Groups, group.Name)
		}
	}

	switch {
	case len(deps.Errors) > 0:
		deps.State = DependencyStateUnknown
	case len(deps.GatedClients)+len(deps.Groups) > 0:
		deps.State = DependencyStateDependent
	default:
		deps.State = DependencyStateClear
	}
	return deps
}

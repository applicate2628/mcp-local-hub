package cli

import (
	"errors"
	"os"
	"strings"

	"mcp-local-hub/internal/api"
)

func supervisorIntentManagedServerSignals() (map[string]struct{}, error) {
	out := map[string]struct{}{}
	intentPath, err := api.DefaultSupervisorIntentPath()
	if err != nil {
		return nil, err
	}
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}
	// Resolve the installed-server catalog once for the longest-installed-prefix
	// disambiguator (r37-1a). Best-effort: a read failure leaves the set empty,
	// which the resolver treats as "no installed prefix" (ok=false) and we fall
	// back to the existing taskName key — never a wrong-owner key.
	installed := map[string]struct{}{}
	if names, mErr := api.NewAPI().ManifestList(); mErr == nil {
		for _, n := range names {
			installed[n] = struct{}{}
		}
	}
	for _, d := range intent.Daemons {
		taskName := strings.TrimSpace(d.TaskName)
		if taskName != "" && api.IsMaintenanceTaskName(taskName) {
			continue
		}
		// Resolve the published server through the OWNER for EVERY row — not just
		// blank-field ones. DescriptorServerName returns the Server field when it
		// agrees with (or there is no) `--server` arg, and "" on a field/argv
		// MISMATCH — so a lying-cache row ({Server:memory, args --server time}) does
		// NOT publish `memory` (this signal gates hub client-config reconcile writes,
		// a publish decision, commission PR #505 r6b). Only when the args carry no
		// --server (a proxy / non-daemon-shaped row) fall back to the
		// longest-installed-prefix rule; a corrupt global argv (DescriptorServerName==""
		// but a global daemon argv) fails closed to the taskName signal below.
		server := api.DescriptorServerName(d)
		if server == "" && !api.DescriptorHasGlobalDaemonArgv(d) {
			if owner, ok := api.ServerOwningTaskByLongestInstalledPrefix(taskName, installed); ok {
				server = owner
			}
		}
		if server == "" {
			if taskName == "" {
				continue
			}
			out[taskName] = struct{}{}
			continue
		}
		out[server] = struct{}{}
	}
	return out, nil
}

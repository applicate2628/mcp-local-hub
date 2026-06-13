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
		server := strings.TrimSpace(d.Server)
		if server == "" {
			// Blank-Server legacy row: api.ServerFromTaskName's last-hyphen split
			// mis-attributes a hyphenated daemon name (\mcp-local-hub-demo-alpha-beta,
			// real server "demo", daemon "alpha-beta" → "demo-alpha"). Resolve the
			// true owner via the longest-installed-prefix rule instead so the gate
			// (install.go reconcile-hub filter + setup.go last-server maintenance
			// gate) keys on the real server. ok=false (no installed prefix — an
			// orphan/foreign row) → preserve the EXISTING taskName fallback below.
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

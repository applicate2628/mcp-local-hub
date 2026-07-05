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
			// Blank-Server legacy row. Prefer the OWNER's argv-recovered server (the
			// process spawns from `--server X`), which is authoritative and unambiguous
			// for a hyphenated daemon name — F5 no longer heals the Server field on
			// restart (bot PR #505). Only when the args carry no --server (a
			// non-daemon-shaped row) fall back to the longest-installed-prefix rule,
			// which mis-attributes a hyphenated daemon via ServerFromTaskName's
			// last-hyphen split otherwise. ok=false there → the taskName fallback below.
			if rs := api.DescriptorServerName(d); rs != "" {
				server = rs
			} else if owner, ok := api.ServerOwningTaskByLongestInstalledPrefix(taskName, installed); ok {
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

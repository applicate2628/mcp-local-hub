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
	for _, d := range intent.Daemons {
		taskName := strings.TrimSpace(d.TaskName)
		if taskName != "" && api.IsMaintenanceTaskName(taskName) {
			continue
		}
		server := strings.TrimSpace(d.Server)
		if server == "" {
			server = api.ServerFromTaskName(taskName)
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

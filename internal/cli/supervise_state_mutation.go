package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"mcp-local-hub/internal/api"
)

// supervisorStateFileMu serializes in-process read/modify/write
// transactions against supervisor-state.json. api.WriteStateFileAtomic
// already serializes the final atomic write across processes, but a
// caller that reads, mutates, then writes needs a wider critical
// section or another goroutine can overwrite its mutation with a stale
// snapshot.
var supervisorStateFileMu sync.Mutex

func mutateSupervisorStateFile(path string, mutate func(*api.SupervisorStateFile) error) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("empty supervisor state path")
	}
	supervisorStateFileMu.Lock()
	defer supervisorStateFileMu.Unlock()

	file, err := api.ReadSupervisorState(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read existing supervisor state: %w", err)
		}
		file = &api.SupervisorStateFile{Version: 1}
	}
	normalizeSupervisorStateFile(file)
	if mutate != nil {
		if err := mutate(file); err != nil {
			return err
		}
	}
	normalizeSupervisorStateFile(file)
	return api.WriteSupervisorState(path, file)
}

func readSupervisorStateFile(path string) (*api.SupervisorStateFile, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("empty supervisor state path")
	}
	supervisorStateFileMu.Lock()
	defer supervisorStateFileMu.Unlock()
	file, err := api.ReadSupervisorState(path)
	if err != nil {
		return nil, err
	}
	normalizeSupervisorStateFile(file)
	return file, nil
}

func normalizeSupervisorStateFile(file *api.SupervisorStateFile) {
	if file == nil {
		return
	}
	if file.Version == 0 {
		file.Version = 1
	}
	if file.Daemons == nil {
		file.Daemons = map[string]api.SupervisorDaemonState{}
	}
}

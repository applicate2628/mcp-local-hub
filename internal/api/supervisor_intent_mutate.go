package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gofrs/flock"
)

// MutateSupervisorIntentIfChanged serializes a supervisor-intent.json
// read-modify-write under the same path+".lock" flock used by
// WriteSupervisorIntent's final write. The mutate callback runs while the
// flock is held and must only edit the in-memory file.
func MutateSupervisorIntentIfChanged(path string, mutate func(*SupervisorIntentFile) (bool, error)) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("empty supervisor intent path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir supervisor intent dir: %w", err)
	}

	lockPath := path + supervisorIntentLockSuffix
	lock := flock.New(lockPath)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("supervisor-intent flock %s: %w", lockPath, err)
	}
	defer func() { _ = lock.Unlock() }()

	file, _, err := readSupervisorIntentForMerge(path)
	if err != nil {
		return fmt.Errorf("read existing supervisor intent: %w", err)
	}
	file = cloneSupervisorIntentFile(file)
	changed := true
	if mutate != nil {
		changed, err = mutate(file)
		if err != nil {
			return err
		}
	}
	if !changed {
		return nil
	}
	if file.Version == 0 {
		file.Version = 1
	}
	return writeSupervisorIntentLockHeld(path, file)
}

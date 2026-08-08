package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MutateSupervisorIntentIfChanged serializes a supervisor-intent.json
// read-modify-write under the same path+".lock" flock used by
// WriteSupervisorIntent's final write. The mutate callback runs while the
// flock is held and must only edit the in-memory file.
func MutateSupervisorIntentIfChanged(path string, mutate func(*SupervisorIntentFile) (bool, error)) (err error) {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("empty supervisor intent path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir supervisor intent dir: %w", err)
	}

	lockPath := supervisorIntentLockPath(path)
	release, lockErr := lockSupervisorIntent(path)
	if lockErr != nil {
		return fmt.Errorf("supervisor-intent flock %s: %w", lockPath, lockErr)
	}
	applied := false
	defer func() {
		releaseSupervisorIntentAndJoinApplied(&err, release, "release supervisor-intent flock", applied)
	}()

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
	if err := writeSupervisorIntentLockHeld(path, file); err != nil {
		return err
	}
	applied = true
	return nil
}

package api

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"mcp-local-hub/internal/config"
)

// MutateSupervisorIntentIfChanged serializes a supervisor-intent.json
// read-modify-write under the same path+".lock" flock used by
// WriteSupervisorIntent's final write. The mutate callback runs while the
// flock is held and must only edit the in-memory file.
//
// This is a thin, backward-compatible wrapper over
// MutateSupervisorIntentIfChangedReturning for the two existing callers
// (internal/cli/strict_mode.go, internal/api/reallocate_dynamic_pool.go)
// that only need the error, not the observed generation — it discards the
// returned snapshot rather than duplicating the read-modify-write body.
func MutateSupervisorIntentIfChanged(path string, mutate func(*SupervisorIntentFile) (bool, error)) error {
	_, err := MutateSupervisorIntentIfChangedReturning(path, mutate)
	return err
}

// MutateSupervisorIntentResult is the observed outcome of a locked
// supervisor-intent read-modify-write.
type MutateSupervisorIntentResult struct {
	// Intent is the EXACT generation this call observed under the flock: a
	// fresh disk read (never the caller's own pre-lock snapshot) with the
	// mutate callback's edits applied. Populated whether or not Changed is
	// true, because the fresh read itself can already differ from
	// whatever the caller loaded before acquiring the lock (a concurrent
	// writer may have committed something else in between) — a caller that
	// must converge with disk (see MutateSupervisorIntentIfChangedReturning's
	// doc comment) needs this snapshot even on a no-op mutation.
	Intent *SupervisorIntentFile
	// Changed reports whether the mutate callback reported a change (and
	// therefore whether a write actually happened).
	Changed bool
}

// MutateSupervisorIntentIfChangedReturning is
// MutateSupervisorIntentIfChanged, additionally returning the exact
// generation observed/committed under the flock.
//
// Why this exists (architecture-adversarial-reverify.md finding A3,
// work-items/active/2026-07-25-mcp-front-daemon/): a caller that reapplies
// its own mutate callback to its OWN pre-lock in-memory copy after this
// function reports success — instead of adopting the snapshot this function
// actually observed under the lock — can silently diverge from disk. If a
// second writer committed a change between the caller's initial load and
// this function's lock acquisition, that concurrent change lands on disk
// (this function's own read is fresh, under the lock) but is invisible to
// the caller's stale copy. QA proved this deterministically for
// internal/cli's supervisor-intent startup seeder: the disk generation
// carried an extra daemon, an extra stop, and StrictMode=true, while the
// seeder's reapply-to-stale-copy return value carried none of them.
//
// The fix: a caller with this requirement must call THIS entry point and
// replace its own local pointer with the returned Intent wholesale, rather
// than reapplying its callback to its own copy. MutateSupervisorIntentIfChanged
// stays the right choice for a caller that only needs the write to happen and
// does not hold, or does not care about, its own outdated snapshot afterward.
func MutateSupervisorIntentIfChangedReturning(path string, mutate func(*SupervisorIntentFile) (bool, error)) (result MutateSupervisorIntentResult, err error) {
	if strings.TrimSpace(path) == "" {
		return MutateSupervisorIntentResult{}, fmt.Errorf("empty supervisor intent path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return MutateSupervisorIntentResult{}, fmt.Errorf("mkdir supervisor intent dir: %w", err)
	}

	lockPath := supervisorIntentLockPath(path)
	release, lockErr := lockSupervisorIntent(path)
	if lockErr != nil {
		return MutateSupervisorIntentResult{}, fmt.Errorf("supervisor-intent flock %s: %w", lockPath, lockErr)
	}
	applied := false
	defer func() {
		releaseSupervisorIntentAndJoinApplied(&err, release, "release supervisor-intent flock", applied)
	}()

	file, _, err := readSupervisorIntentForMerge(path)
	if err != nil {
		return MutateSupervisorIntentResult{}, fmt.Errorf("read existing supervisor intent: %w", err)
	}
	file, changed, err := prepareSupervisorIntentMutation(file, mutate)
	if err != nil {
		return MutateSupervisorIntentResult{}, err
	}
	if !changed {
		return MutateSupervisorIntentResult{Intent: file, Changed: false}, nil
	}
	if err := writeSupervisorIntentLockHeld(path, file); err != nil {
		return MutateSupervisorIntentResult{}, err
	}
	applied = true
	return MutateSupervisorIntentResult{Intent: file, Changed: true}, nil
}

func prepareSupervisorIntentMutation(file *SupervisorIntentFile, mutate func(*SupervisorIntentFile) (bool, error)) (*SupervisorIntentFile, bool, error) {
	file = cloneSupervisorIntentFile(file)
	changed := true
	if mutate != nil {
		var err error
		changed, err = mutate(file)
		if err != nil {
			return nil, false, err
		}
	}
	if !changed {
		return file, false, nil
	}
	if file.Version == 0 {
		file.Version = 1
	}
	return file, true, nil
}

// ConvergeGeneratedSupervisorImage changes only descriptor image fields and timer
// commands whose legacy command and producer provenance both match. The returning writer
// owns the fresh generation; callers must use its snapshot, including on a no-op.
func ConvergeGeneratedSupervisorImage(path string) (MutateSupervisorIntentResult, error) {
	legacy, err := LocalInstallMcphubPath()
	if err != nil {
		return MutateSupervisorIntentResult{}, err
	}
	return MutateSupervisorIntentIfChangedReturning(path, func(f *SupervisorIntentFile) (bool, error) {
		if !supervisorIntentHasLegacyImage(f, legacy) {
			return false, nil
		}
		image, err := RuntimeMcphubExecutable()
		if err != nil {
			return false, err
		}
		return convergeGeneratedSupervisorImage(f, legacy, image)
	})
}

// PreviewGeneratedSupervisorImage uses the same producer checks on a clone and
// never writes the intent, registry, manifests, or scheduler.
func PreviewGeneratedSupervisorImage(intent *SupervisorIntentFile) (*SupervisorIntentFile, error) {
	legacy, err := LocalInstallMcphubPath()
	if err != nil {
		return nil, err
	}
	f := cloneSupervisorIntentFile(intent)
	if !supervisorIntentHasLegacyImage(f, legacy) {
		return f, nil
	}
	image, err := RuntimeMcphubExecutable()
	if err != nil {
		return nil, err
	}
	if _, err := convergeGeneratedSupervisorImage(f, legacy, image); err != nil {
		return nil, err
	}
	return f, nil
}

func supervisorIntentHasLegacyImage(f *SupervisorIntentFile, legacy string) bool {
	if f != nil {
		for _, d := range f.Daemons {
			if d.TaskName != BuiltinRouteTaskName && d.Command == legacy {
				return true
			}
		}
		for _, timer := range f.MaintenanceTimers {
			if timer.Command == legacy && generatedSupervisorImageTimer(timer) {
				return true
			}
		}
	}
	return false
}

func convergeGeneratedSupervisorImage(f *SupervisorIntentFile, legacy, image string) (bool, error) {
	if legacy == "" || image == "" || !filepath.IsAbs(legacy) || !filepath.IsAbs(image) || legacy != filepath.Clean(legacy) || image != filepath.Clean(image) {
		return false, fmt.Errorf("runtime image convergence requires clean absolute legacy and admitted image paths")
	}
	if f == nil || legacy == image {
		return false, nil
	}
	changed := false
	for i := range f.Daemons {
		d := &f.Daemons[i]
		if d.TaskName == BuiltinRouteTaskName {
			continue // The reserved factory owns its complete canonical row.
		}
		// The fixed old API policy is positive command provenance, not a
		// basename/package-layout heuristic. An arbitrary alternate Product
		// or foreign command is not ours to rewrite.
		if d.Command != legacy {
			continue
		}
		owned, err := generatedSupervisorImageDescriptor(*d)
		if err != nil {
			return false, fmt.Errorf("runtime image provenance for %s: %w", d.TaskName, err)
		}
		if !owned {
			continue
		}
		if d.WorkingDir == filepath.Dir(legacy) {
			d.WorkingDir = filepath.Dir(image)
		}
		d.Command = image
		changed = true
	}
	for i := range f.MaintenanceTimers {
		timer := &f.MaintenanceTimers[i]
		if timer.Command == legacy && generatedSupervisorImageTimer(*timer) {
			timer.Command = image
			changed = true
		}
	}
	return changed, nil
}

func generatedSupervisorImageTimer(timer MaintenanceTimer) bool {
	switch timer.Kind {
	case "server-weekly-refresh":
		server := MaintenanceTimerArgvServer(timer)
		return server != "" && maintenanceTimerOwnedBy(timer, server) &&
			canonicalIntentTaskKey(timer.Name) == canonicalIntentTaskKey("mcp-local-hub-"+server+"-weekly-refresh") &&
			reflect.DeepEqual(timer.Args, []string{"restart", "--server", server})
	case "workspace-weekly-refresh":
		return timer.Server == "" && canonicalIntentTaskKey(timer.Name) == canonicalIntentTaskKey(WeeklyRefreshTaskName) &&
			reflect.DeepEqual(timer.Args, []string{"workspace-weekly-refresh"})
	default:
		return false
	}
}

func generatedSupervisorImageDescriptor(d SupervisorDaemon) (bool, error) {
	if d.Workspace != "" {
		reg, ok, err := OpenLSPRegistryForReconcile()
		if err != nil {
			return false, err
		}
		if !ok {
			return false, fmt.Errorf("workspace provenance registry is unavailable")
		}
		for _, ws := range reg.Workspaces {
			if ws.WorkspacePath != d.Workspace || ws.Port != d.Port || ws.PendingSerenaRemoval {
				continue
			}
			var rows []SupervisorDaemon
			if ws.Language == SerenaLanguageSentinel {
				raw, err := loadManifestYAMLEmbedFirst(d.Server)
				if err != nil {
					return false, err
				}
				if d.ManifestHash == "" || d.ManifestHash != ManifestHashContent(raw) {
					continue
				}
				m, err := parseManifestForName(d.Server, raw)
				if err != nil {
					return false, err
				}
				rows = BuildSupervisorDaemonsForSerena(m, []WorkspaceEntry{ws}, d.ManifestHash, d.Command)
			} else {
				rows = []SupervisorDaemon{BuildSupervisorDaemonForLSP(ws, d.Command)}
			}
			for _, want := range rows {
				if d.TaskName == want.TaskName && d.Server == want.Server && d.Daemon == want.Daemon && reflect.DeepEqual(d.Args, want.Args) {
					return true, nil
				}
			}
		}
		return false, nil
	}
	if d.Server == "" || d.Daemon == "" || d.ManifestHash == "" {
		return false, nil
	}
	raw, err := loadManifestYAMLEmbedFirst(d.Server)
	if err != nil {
		return false, err
	}
	if d.ManifestHash != ManifestHashContent(raw) {
		return false, nil
	}
	m, err := parseManifestForName(d.Server, raw)
	if err != nil {
		return false, err
	}
	if m.Kind == config.KindWorkspaceScoped {
		return false, nil
	}
	if _, ok := findDaemon(m, d.Daemon); !ok {
		return false, nil
	}
	wantTask := canonicalIntentTaskKey(supervisorTaskNameForManifestDaemon(m.Name, d.Daemon))
	wantArgs := []string{"daemon", "--server", m.Name, "--daemon", d.Daemon}
	return d.TaskName == wantTask && reflect.DeepEqual(d.Args, wantArgs), nil
}

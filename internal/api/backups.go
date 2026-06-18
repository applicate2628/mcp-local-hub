package api

import (
	"fmt"
	"mcp-local-hub/internal/clients"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupsList returns all mcp-local-hub backups found next to managed client
// config files, classified as "original" (sentinel) or "timestamped". Missing
// client configs are silently skipped.
func (a *API) BackupsList() ([]BackupInfo, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var all []BackupInfo
	for _, c := range clientFiles(home) {
		rows, err := a.BackupsListIn(filepath.Dir(c.Path), filepath.Base(c.Path))
		if err != nil {
			continue
		}
		for i := range rows {
			rows[i].Client = c.Client
		}
		all = append(all, rows...)
	}
	return all, nil
}

// BackupsListIn inspects dir for backups of the given live-file name.
func (a *API) BackupsListIn(dir, liveName string) ([]BackupInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := liveName + ".bak-mcp-local-hub-"
	var out []BackupInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		kind := "timestamped"
		if strings.HasSuffix(name, "-original") {
			kind = "original"
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{
			Client:   clientNameFromLive(liveName),
			Path:     filepath.Join(dir, name),
			Kind:     kind,
			ModTime:  fi.ModTime(),
			SizeByte: fi.Size(),
		})
	}
	return out, nil
}

// BackupsClean prunes timestamped backups for managed clients, keeping only
// keepN most recent per client. Sentinels never touched.
func (a *API) BackupsClean(keepN int) ([]string, error) {
	return a.backupsCleanAll(keepN, false)
}

// BackupsCleanPreview returns the list of backup files that would be removed
// by BackupsClean(keepN) without actually deleting them. Used by the CLI's
// `backups clean --dry-run` flag so users can audit the prune list before
// committing to the deletion.
func (a *API) BackupsCleanPreview(keepN int) ([]string, error) {
	return a.backupsCleanAll(keepN, true)
}

// backupsCleanAll is the shared implementation of BackupsClean and
// BackupsCleanPreview. dryRun=true returns candidate paths without deleting.
func (a *API) backupsCleanAll(keepN int, dryRun bool) ([]string, error) {
	if keepN < 0 {
		return nil, fmt.Errorf("keepN must be >= 0")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, c := range clientFiles(home) {
		r, err := a.backupsCleanInImpl(filepath.Dir(c.Path), filepath.Base(c.Path), keepN, dryRun)
		if err != nil {
			continue
		}
		removed = append(removed, r...)
	}
	return removed, nil
}

// BackupsCleanIn is the tempdir-capable form of BackupsClean.
func (a *API) BackupsCleanIn(dir, liveName string, keepN int) ([]string, error) {
	if keepN < 0 {
		return nil, fmt.Errorf("keepN must be >= 0")
	}
	return a.backupsCleanInImpl(dir, liveName, keepN, false)
}

// backupsCleanInImpl is the shared core for BackupsCleanIn and the dry-run path.
func (a *API) backupsCleanInImpl(dir, liveName string, keepN int, dryRun bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := liveName + ".bak-mcp-local-hub-"
	type bak struct {
		path    string
		modTime time.Time
	}
	var ts []bak
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if strings.HasSuffix(name, "-original") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		ts = append(ts, bak{path: filepath.Join(dir, name), modTime: fi.ModTime()})
	}
	if len(ts) <= keepN {
		return nil, nil
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].modTime.After(ts[j].modTime) })
	var removed []string
	for _, b := range ts[keepN:] {
		if dryRun {
			// Dry-run: return the would-be-deleted path without touching
			// the filesystem. Caller lists these so the user can audit
			// the prune set before re-running without --dry-run.
			removed = append(removed, b.path)
			continue
		}
		if err := os.Remove(b.path); err == nil {
			removed = append(removed, b.path)
		} else {
			// Do not silently drop a failed delete. A backup the user previewed
			// as eligible but that os.Remove rejected (Windows sharing violation,
			// ACL denial, file locked by AV/editor) would otherwise vanish from
			// the reported "cleaned" count with no trace — the operator sees
			// "cleaned: 5" and cannot tell 3 deletions failed. Emit a warn event
			// so the failure is at least diagnosable. (Full per-file surfacing
			// into the GUI response errors[] field is a tracked follow-up; it
			// needs a signature change across BackupsClean/CleanIn/the backupsAPI
			// interface + CLI callers, out of scope for this fix.)
			_ = LogHubMcpEvent("warn", "backup-clean-remove-failed", map[string]any{
				"path": b.path, "err": err.Error(),
			})
		}
	}
	return removed, nil
}

// BackupShow returns the contents of the backup file at path.
func (a *API) BackupShow(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// RollbackOriginal restores each client config from its pristine-sentinel
// backup (if present). Returns per-client result so CLI/GUI can report.
func (a *API) RollbackOriginal() ([]RollbackResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var results []RollbackResult
	for _, live := range clientFiles(home) {
		sentinel := live.Path + ".bak-mcp-local-hub-original"
		client := live.Client
		if _, err := os.Stat(sentinel); os.IsNotExist(err) {
			results = append(results, RollbackResult{Client: client, Err: "no original backup"})
			continue
		}
		data, err := os.ReadFile(sentinel)
		if err != nil {
			results = append(results, RollbackResult{Client: client, Err: err.Error()})
			continue
		}
		if err := os.WriteFile(live.Path, data, 0600); err != nil {
			results = append(results, RollbackResult{Client: client, Err: err.Error()})
			continue
		}
		results = append(results, RollbackResult{Client: client, Restored: live.Path})
	}
	return results, nil
}

// RollbackResult is one row in a RollbackOriginal report.
type RollbackResult struct {
	Client   string
	Restored string
	Err      string
}

type clientFile struct {
	Client string
	Path   string
}

// clientFiles returns absolute paths to all managed client configs. The home
// argument is retained for call-site symmetry (every caller already resolves
// it) but is unused: config paths resolve through clients.ConfigPathForName.
func clientFiles(_ string) []clientFile {
	var out []clientFile
	for _, name := range clients.SupportedClientNames() {
		path, err := clients.ConfigPathForName(name)
		if err != nil {
			continue
		}
		out = append(out, clientFile{Client: name, Path: path})
	}
	return out
}

// clientNameFromLive maps a live config filename to the canonical client id.
func clientNameFromLive(name string) string {
	switch name {
	case ".claude.json":
		return "claude-code"
	case "config.toml":
		return "codex-cli"
	case "settings.json":
		return "gemini-cli"
	case "mcp_config.json":
		return "antigravity"
	}
	return name
}

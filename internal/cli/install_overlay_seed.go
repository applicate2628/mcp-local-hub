package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/binary_discovery"
	"mcp-local-hub/internal/api/daemon_env_overlay"
	"mcp-local-hub/internal/config"
)

// seedOverlayFromDiscovery is the install-time auto-discovery hook for
// per-daemon env overlay seeding. For each manifest's RequiredBinaries
// (server-level + per-language), it walks binary_discovery hints and
// writes a `source: auto-discovery` row into the overlay file keyed by
// canonical SupervisorDaemon.TaskName.
//
// CAS preservation: if the existing row carries `source: operator`, the
// seeder skips it and emits `daemon-env-overlay-skipped-operator-override`
// — the operator's hand-edits MUST NOT be clobbered by reinstall.
//
// Best-effort posture: a Discover error degrades to "no rows written",
// not a fatal install failure. The caller wires this AFTER the manifest
// install completes so a discovery failure cannot strand the operator
// with a half-installed server.
//
// Observability:
//   - `binary-discovery-ran` (info) fires once per call with
//     {scan_duration_ms, manifest_count, hits, misses}.
//   - `binary-discovery-missing` (warn) fires per absent binary with
//     {server, binary, scanned_hints}.
//   - `daemon-env-overlay-skipped-operator-override` (info) fires per
//     preserved operator row with {task_name, binary}.
//
// Parameters:
//   - ctx: cancellation surface; Discover honors it.
//   - manifests: parsed manifests the caller just installed.
//   - overlayPath: full path to daemon-env-overrides.yaml.
//   - hints: hint directories to search. Pass nil to use
//     binary_discovery.DefaultHints() (production path); tests inject a
//     synthetic temp dir slice.
//
// Returns nil on success and on best-effort partial success. Returns
// error only when the WriteOverlay mutator pipeline itself fails (lock
// acquisition, marshal, secure-write rejection).
func seedOverlayFromDiscovery(
	ctx context.Context,
	manifests []*config.ServerManifest,
	overlayPath string,
	hints []string,
) error {
	if hints == nil {
		hints = binary_discovery.DefaultHints()
	}

	// Collect the union of required binaries across manifests. The
	// per-binary discovery walk is shared (same hints, same result map)
	// so each binary is walked once regardless of how many manifests
	// declared it.
	binaryToServers := map[string][]string{}
	for _, m := range manifests {
		if m == nil {
			continue
		}
		for _, b := range m.RequiredBinaries {
			binaryToServers[b] = append(binaryToServers[b], m.Name)
		}
		for _, lang := range m.Languages {
			for _, b := range lang.RequiredBinaries {
				binaryToServers[b] = append(binaryToServers[b], m.Name)
			}
		}
	}
	if len(binaryToServers) == 0 {
		// No required_binaries declared; nothing to seed.
		return nil
	}

	allBinaries := make([]string, 0, len(binaryToServers))
	for b := range binaryToServers {
		allBinaries = append(allBinaries, b)
	}
	sort.Strings(allBinaries) // deterministic discovery order

	start := time.Now()
	found, discoverErr := binary_discovery.Discover(ctx, allBinaries, hints)
	scanMs := time.Since(start).Milliseconds()

	// Count hits / misses for the summary event.
	hits := 0
	for _, p := range found {
		if p != "" {
			hits++
		}
	}
	misses := len(allBinaries) - hits

	_ = api.LogHubMcpEvent("info", "binary-discovery-ran", map[string]any{
		"scan_duration_ms": scanMs,
		"manifest_count":   len(manifests),
		"binary_count":     len(allBinaries),
		"hits":             hits,
		"misses":           misses,
	})

	// Emit per-binary miss events with the servers that declared the
	// missing binary, so the operator log surfaces actionable context.
	for _, b := range allBinaries {
		if found[b] != "" {
			continue
		}
		servers := dedupeStrings(binaryToServers[b])
		_ = api.LogHubMcpEvent("warn", "binary-discovery-missing", map[string]any{
			"binary":         b,
			"servers":        servers,
			"scanned_hints":  len(hints),
		})
	}

	if discoverErr != nil {
		// Best-effort: surface the error in the audit log but do NOT
		// abort the install pipeline. Empty found-map means we'll write
		// no auto-discovery rows below, which is the correct fallback.
		_ = api.LogHubMcpEvent("warn", "binary-discovery-failed", map[string]any{
			"error": discoverErr.Error(),
		})
	}

	// Map each found binary back to the daemon(s) whose manifest declared
	// it. taskName format: \mcp-local-hub-<server>-<daemon> (canonical
	// leading-backslash form). For manifests with no Daemons entries the
	// default daemon name is "default" — this matches existing install
	// code's task-name construction.
	type binAssignment struct {
		taskName string
		binDir   string
		binary   string
		server   string
	}
	var assignments []binAssignment
	for _, m := range manifests {
		if m == nil {
			continue
		}
		daemonNames := manifestDaemonNamesForOverlay(m)
		// Server-level required_binaries → every daemon of this server.
		for _, b := range m.RequiredBinaries {
			abs := found[b]
			if abs == "" {
				continue
			}
			binDir := filepath.Dir(abs)
			for _, dname := range daemonNames {
				assignments = append(assignments, binAssignment{
					taskName: fmt.Sprintf(`\mcp-local-hub-%s-%s`, m.Name, dname),
					binDir:   binDir,
					binary:   b,
					server:   m.Name,
				})
			}
		}
		// Per-language required_binaries are workspace-scoped; their
		// SupervisorDaemon entries exist only after `mcphub register`
		// has run, not at install time. Skip here; a future task wires
		// register-time seeding.
	}

	if len(assignments) == 0 {
		return nil
	}

	// Single WriteOverlay call covers all assignments. The mutator
	// performs the source-preservation CAS in-memory before WriteOverlay
	// marshals + atomically renames.
	return daemon_env_overlay.WriteOverlay(overlayPath, func(o *daemon_env_overlay.Overlay) error {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		for _, a := range assignments {
			key := daemon_env_overlay.NormalizeOverlayKey(a.taskName)
			existing, present := o.Daemons[key]
			if present && existing.Source == "operator" {
				_ = api.LogHubMcpEvent("info", "daemon-env-overlay-skipped-operator-override", map[string]any{
					"task_name": key,
					"binary":    a.binary,
					"server":    a.server,
				})
				continue
			}
			env := map[string]string{}
			if existing.Env != nil {
				for k, v := range existing.Env {
					env[k] = v
				}
			}
			// Key must be uppercase "PATH" — `mergeDaemonEnv` (supervise.go:1664)
			// folds key case only on Windows, so on Linux/macOS a `Path` key in
			// the overlay map would NOT collide with the parent process's `PATH`
			// entry, and the discovered bin directory would be silently ignored
			// at spawn time. Storing `PATH` makes the override land on every OS.
			env["PATH"] = a.binDir + string(os.PathListSeparator) + "${parent_path}"
			o.Daemons[key] = daemon_env_overlay.DaemonRow{
				Env:          env,
				Source:       "auto-discovery",
				DiscoveredAt: now,
			}
		}
		return nil
	})
}

// manifestDaemonNamesForOverlay returns the daemon-name strings that the
// install pipeline uses for SupervisorDaemon.TaskName construction. For
// a manifest with no Daemons entries, the convention is a single
// "default" daemon (mirrors install.go's task-name derivation).
func manifestDaemonNamesForOverlay(m *config.ServerManifest) []string {
	if m == nil {
		return nil
	}
	if len(m.Daemons) == 0 {
		return []string{"default"}
	}
	out := make([]string, 0, len(m.Daemons))
	seen := map[string]struct{}{}
	for _, d := range m.Daemons {
		name := d.Name
		if name == "" {
			name = "default"
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// dedupeStrings keeps the first occurrence of each input string and
// preserves order. Used for the binary-discovery-missing event's
// "servers" field so an operator sees each server name once.
func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

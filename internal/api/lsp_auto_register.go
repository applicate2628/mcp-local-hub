package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

var lspEnsureLocks sync.Map // map[string]*sync.Mutex, keyed by workspaceKey + "\x00" + language

const lspExistingProxyProbeTimeout = 500 * time.Millisecond

// EnsureLSPRegistered idempotently creates the supervised workspace-proxy row
// for one (workspaceKey, language) tuple without writing any client configs.
// The per-tuple lock spans registry write, supervisor intent write, reconcile,
// and proxy readiness so concurrent first-touch calls cannot route to an
// unready port or allocate two ports for the same language.
func (a *API) EnsureLSPRegistered(ctx context.Context, workspaceKey, workspacePath, language string) (WorkspaceEntry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if language == "" {
		return WorkspaceEntry{}, fmt.Errorf("language is required")
	}
	canonical, err := CanonicalWorkspacePath(workspacePath)
	if err != nil {
		return WorkspaceEntry{}, err
	}
	computedKey := WorkspaceKey(canonical)
	if workspaceKey == "" {
		workspaceKey = computedKey
	} else if workspaceKey != computedKey {
		return WorkspaceEntry{}, fmt.Errorf("workspace key %q does not match canonical workspace %q key %q", workspaceKey, canonical, computedKey)
	}

	mu := lspEnsureMutex(workspaceKey, language)
	mu.Lock()
	defer mu.Unlock()

	if err := ctx.Err(); err != nil {
		return WorkspaceEntry{}, err
	}

	m, spec, err := loadLSPRegisterManifest(language)
	if err != nil {
		return WorkspaceEntry{}, err
	}
	if m.PortPool == nil {
		return WorkspaceEntry{}, fmt.Errorf("manifest %s has no port_pool", m.Name)
	}

	regPath, err := registryPathForRegister()
	if err != nil {
		return WorkspaceEntry{}, err
	}
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return WorkspaceEntry{}, fmt.Errorf("acquire registry lock: %w", err)
	}
	if err := reg.Load(); err != nil {
		unlock()
		return WorkspaceEntry{}, fmt.Errorf("load registry: %w", err)
	}
	if prior, ok := reg.Get(workspaceKey, language); ok {
		unlock()
		portReady := proxyReadinessFn(prior.Port, lspExistingProxyProbeTimeout) == nil
		owned, err := lspSupervisorIntentDescriptorExists(prior.WorkspaceKey, prior.Language)
		if err != nil {
			return WorkspaceEntry{}, err
		}
		if portReady && owned {
			return prior, nil
		}
		if err := ctx.Err(); err != nil {
			return WorkspaceEntry{}, err
		}
		canonicalExe, err := canonicalMcphubPath()
		if err != nil {
			return WorkspaceEntry{}, err
		}
		if _, err := os.Stat(canonicalExe); err != nil {
			return WorkspaceEntry{}, fmt.Errorf("%s not present — run `mcphub setup` once: %w", canonicalExe, err)
		}
		restoreLegacyTask := func() {}
		if !owned {
			sch, err := newScheduler()
			if err != nil {
				return WorkspaceEntry{}, fmt.Errorf("scheduler for LSP promote: %w", err)
			}
			var legacyXML []byte
			if xml, xerr := sch.ExportXML(prior.TaskName); xerr == nil {
				legacyXML = xml
			} else if !errors.Is(xerr, scheduler.ErrTaskNotFound) {
				return WorkspaceEntry{}, fmt.Errorf("export legacy LSP task %s: %w", prior.TaskName, xerr)
			}
			if len(legacyXML) > 0 {
				capturedXML := legacyXML
				restoreLegacyTask = func() {
					if prior.Port > 0 {
						_ = killByPortFn(prior.Port, 5*time.Second)
					}
					_ = sch.ImportXML(prior.TaskName, capturedXML)
					_ = sch.Run(prior.TaskName)
				}
			}
			if portReady {
				_ = killByPortFn(prior.Port, 5*time.Second)
			}
			if len(legacyXML) > 0 {
				if derr := sch.Delete(prior.TaskName); derr != nil && !errors.Is(derr, scheduler.ErrTaskNotFound) {
					restoreLegacyTask()
					return WorkspaceEntry{}, fmt.Errorf("delete legacy LSP task %s before promote: %w", prior.TaskName, derr)
				}
			}
		}
		restoreIntent, err := a.upsertLSPSupervisorIntent(prior, canonicalExe)
		if err != nil {
			restoreLegacyTask()
			return WorkspaceEntry{}, err
		}
		intentWritten := true
		rollbackIntent := func() {
			if intentWritten {
				restoreIntent()
				intentWritten = false
			}
		}

		reconcileCtx, cancel := context.WithTimeout(ctx, DefaultReconcileTimeout)
		if _, err := registerSupervisorReconcileFn(reconcileCtx, true); err != nil {
			cancel()
			rollbackIntent()
			restoreLegacyTask()
			return WorkspaceEntry{}, fmt.Errorf("supervisor reconcile after LSP intent write: %w", err)
		}
		cancel()

		if err := proxyReadinessFn(prior.Port, 10*time.Second); err != nil {
			rollbackIntent()
			restoreLegacyTask()
			return WorkspaceEntry{}, fmt.Errorf("proxy readiness on port %d: %w", prior.Port, err)
		}
		return prior, nil
	}

	port, err := AllocatePort(reg, *m.PortPool)
	if err != nil {
		unlock()
		return WorkspaceEntry{}, err
	}
	canonicalExe, err := canonicalMcphubPath()
	if err != nil {
		unlock()
		return WorkspaceEntry{}, err
	}
	if _, err := os.Stat(canonicalExe); err != nil {
		unlock()
		return WorkspaceEntry{}, fmt.Errorf("%s not present — run `mcphub setup` once: %w", canonicalExe, err)
	}

	entry := WorkspaceEntry{
		WorkspaceKey:  workspaceKey,
		WorkspacePath: canonical,
		Language:      language,
		Backend:       spec.Backend,
		Port:          port,
		TaskName:      LSPTaskNameForWorkspaceLanguage(workspaceKey, language),
		ClientEntries: map[string]string{},
		WeeklyRefresh: resolveWeeklyRefresh(a, RegisterOpts{}),
		Lifecycle:     LifecycleConfigured,
	}
	if err := reg.PutLSP(entry); err != nil {
		unlock()
		return WorkspaceEntry{}, fmt.Errorf("ensure LSP register: row write rejected: %w", err)
	}
	if err := reg.Save(); err != nil {
		unlock()
		return WorkspaceEntry{}, fmt.Errorf("persist registry: %w", err)
	}
	unlock()

	supervisorSpawnRequested := false
	rollback := func() {
		if supervisorSpawnRequested && entry.Port > 0 {
			_ = killByPortFn(entry.Port, 5*time.Second)
		}
		removeLSPRegistryRow(regPath, workspaceKey, language)
	}

	if err := ctx.Err(); err != nil {
		rollback()
		return WorkspaceEntry{}, err
	}
	restoreIntent, err := a.upsertLSPSupervisorIntent(entry, canonicalExe)
	if err != nil {
		rollback()
		return WorkspaceEntry{}, err
	}
	intentWritten := true
	rollbackIntent := func() {
		if intentWritten {
			restoreIntent()
			intentWritten = false
		}
	}

	reconcileCtx, cancel := context.WithTimeout(ctx, DefaultReconcileTimeout)
	if _, err := registerSupervisorReconcileFn(reconcileCtx, true); err != nil {
		cancel()
		rollbackIntent()
		rollback()
		return WorkspaceEntry{}, fmt.Errorf("supervisor reconcile after LSP intent write: %w", err)
	}
	cancel()
	supervisorSpawnRequested = true

	if err := proxyReadinessFn(port, 10*time.Second); err != nil {
		rollbackIntent()
		rollback()
		return WorkspaceEntry{}, fmt.Errorf("proxy readiness on port %d: %w", port, err)
	}

	return entry, nil
}

func lspEnsureMutex(workspaceKey, language string) *sync.Mutex {
	key := workspaceKey + "\x00" + language
	v, _ := lspEnsureLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func loadLSPRegisterManifest(language string) (*config.ServerManifest, config.LanguageSpec, error) {
	data, err := loadManifestYAMLEmbedFirst("mcp-language-server")
	if err != nil {
		return nil, config.LanguageSpec{}, fmt.Errorf("load manifest mcp-language-server: %w", err)
	}
	m, err := parseManifestForName("mcp-language-server", data)
	if err != nil {
		return nil, config.LanguageSpec{}, err
	}
	if m.Kind != config.KindWorkspaceScoped {
		return nil, config.LanguageSpec{}, fmt.Errorf("manifest %s: not workspace-scoped", m.Name)
	}
	for _, spec := range m.Languages {
		if spec.Name == language {
			return m, spec, nil
		}
	}
	return nil, config.LanguageSpec{}, fmt.Errorf("unknown language %q (manifest %s supports: %v)", language, m.Name, sortedLanguageNames(m))
}

func removeLSPRegistryRow(regPath, workspaceKey, language string) {
	reg := NewRegistry(regPath)
	unlock, err := reg.Lock()
	if err != nil {
		return
	}
	defer unlock()
	if err := reg.Load(); err != nil {
		return
	}
	reg.Remove(workspaceKey, language)
	_ = reg.Save()
}

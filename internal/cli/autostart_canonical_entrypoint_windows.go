//go:build windows

package cli

import (
	"fmt"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/autostart"
)

func newAutostartBackendForCLI() (autostart.Backend, error) {
	backend, err := autostart.New()
	if err != nil {
		return nil, err
	}
	return newCanonicalAutostartBackend(backend, api.CanonicalAutostartEntrypoint), nil
}

type canonicalAutostartBackend struct {
	backend autostart.Backend
	resolve func() (string, error)
}

func newCanonicalAutostartBackend(backend autostart.Backend, resolve func() (string, error)) autostart.Backend {
	return &canonicalAutostartBackend{backend: backend, resolve: resolve}
}

func (b *canonicalAutostartBackend) withCanonical(opts autostart.Options) (autostart.Options, error) {
	if opts.MCPHubPath != "" {
		return opts, nil
	}
	path, err := b.resolve()
	if err != nil {
		return autostart.Options{}, fmt.Errorf("E_AUTOSTART_CANONICAL_ENTRYPOINT_UNAVAILABLE: %w", err)
	}
	opts.MCPHubPath = path
	return opts, nil
}

func (b *canonicalAutostartBackend) Enable(opts autostart.Options) error {
	resolved, err := b.withCanonical(opts)
	if err != nil {
		return err
	}
	return b.backend.Enable(resolved)
}
func (b *canonicalAutostartBackend) Disable() error { return b.backend.Disable() }
func (b *canonicalAutostartBackend) Status(opts autostart.Options) (autostart.State, error) {
	resolved, err := b.withCanonical(opts)
	if err != nil {
		return autostart.StateAbsent, err
	}
	return b.backend.Status(resolved)
}
func (b *canonicalAutostartBackend) StatusSnapshot(opts autostart.Options) (autostart.StatusSnapshot, error) {
	resolved, err := b.withCanonical(opts)
	if err != nil {
		return autostart.StatusSnapshot{State: autostart.StateAbsent}, err
	}
	return b.backend.StatusSnapshot(resolved)
}

package cli

import (
	"context"
	"errors"
	"os"
	"strings"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/secrets"
)

// prepareRouteStdio reuses the fixed-daemon environment materializer while
// leaving process creation to the protected listener's private session owner.
// It never calls the ordinary daemon selector, whose explicit refusal remains
// the defense against publishing a shared port for a session-local provider.
func prepareRouteStdio(ctx context.Context, a *api.API, endpoint api.HubStdioEndpoint) (*daemon.StdioLaunchContext, daemon.HostConfig, error) {
	launch, err := daemon.CaptureStdioLaunchContext(ctx)
	if err != nil {
		return nil, daemon.HostConfig{}, err
	}
	raw, err := a.ManifestGet(endpoint.Server)
	if err != nil {
		return nil, daemon.HostConfig{}, err
	}
	m, err := launch.ParseManifest(strings.NewReader(raw))
	if err != nil {
		return nil, daemon.HostConfig{}, err
	}
	spec, err := m.SelectDaemon(endpoint.Daemon)
	if err != nil || spec == nil || api.SessionLocalStdioEndpoint(m, spec) != endpoint {
		return nil, daemon.HostConfig{}, daemon.ErrIsolatedStdioUnavailable
	}
	if cstLaunchCapabilityConfig(m.Name, spec.Name) != nil {
		return nil, daemon.HostConfig{}, daemon.ErrIsolatedStdioUnavailable
	}
	vault, vaultErr := secrets.OpenVaultOptional(defaultKeyPath(), defaultVaultPath())
	if vaultErr != nil && secrets.HasSecretRef(m.Env) {
		return nil, daemon.HostConfig{}, errors.Join(daemon.ErrIsolatedStdioUnavailable, vaultErr)
	}
	resolver, err := launch.BindResolver(secrets.NewResolver(vault, nil))
	if err != nil {
		return nil, daemon.HostConfig{}, err
	}
	env, unset, err := daemonEnvWithOverlaySnapshot(m.Name, spec.Name, m.Env, resolver, launch, m.EnvForwardLocal)
	if err != nil {
		return nil, daemon.HostConfig{}, err
	}
	command := m.Command
	if command == "mcphub" {
		command, err = os.Executable()
		if err != nil {
			return nil, daemon.HostConfig{}, err
		}
	}
	return launch, daemon.HostConfig{
		Command: command, Args: daemon.StdioArguments(m.BaseArgs, spec.ExtraArgs),
		Env: env, UnsetEnv: unset, WorkingDir: spec.Cwd,
		MCPProtocolCompatibilityProfile: spec.MCPProtocolCompatibilityProfile,
	}, nil
}

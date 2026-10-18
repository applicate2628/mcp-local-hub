package cli

import (
	"context"
	"errors"
	"fmt"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"
	"strings"
)

// This is the preparation branch of the SAME manifest/forward/overlay owner.
// It reads raw declarations before secret resolution. Runtime callers continue
// using daemonEnvWithOverlay and its existing optional-secret precedence.
func buildSerenaPreparationEnvironmentSource(ctx context.Context, manifest *config.ServerManifest, resolver *secrets.Resolver, taskName string, forwardedNames []string, bootstrap map[string]string) (api.SerenaPreparationEnvironmentSource, error) {
	if ctx == nil || ctx.Err() != nil || manifest == nil || resolver == nil || taskName == "" {
		return nil, errors.New("SERENA_PREPARATION_DECLARATION_INPUT_MISSING")
	}
	path, err := resolveDaemonOverlayPath()
	if err != nil {
		return nil, err
	}
	overlay, err := daemon_env_overlay.Load(path)
	if err != nil {
		// An already-applied wrapper environment has lost reference provenance.
		// Its legacy runtime fallback is not preparation identity evidence.
		return nil, fmt.Errorf("SERENA_PREPARATION_OVERLAY_PROVENANCE_UNAVAILABLE: %w", err)
	}
	return buildSerenaEnvironmentSourceFromOverlay(ctx, manifest, resolver, taskName, forwardedNames, bootstrap, overlay)
}

func buildSerenaEnvironmentSourceFromOverlay(ctx context.Context, manifest *config.ServerManifest, resolver *secrets.Resolver, taskName string, forwardedNames []string, bootstrap map[string]string, overlay *daemon_env_overlay.Overlay, binding ...api.SerenaConsumerEnvironmentSourceBinding) (api.SerenaPreparationEnvironmentSource, error) {
	rawOverlay := daemon_env_overlay.LookupOverlay(overlay, taskName)
	raw := mergeDaemonEnvMaps(manifest.Env, nil)
	forwarded := map[string]bool{}
	for _, key := range forwardedNames {
		if key == "" || strings.ContainsAny(key, "=\x00\r\n") {
			return nil, errors.New("SERENA_PREPARATION_FORWARD_KEY_INVALID")
		}
		present := false
		for existing := range raw {
			if strings.EqualFold(existing, key) {
				present = true
				break
			}
		}
		if !present {
			raw[key] = "${env:" + key + "}"
			forwarded[key] = true
		}
	}
	winning := mergeDaemonEnvMaps(raw, rawOverlay)
	kinds := map[string]string{}
	for key, value := range winning {
		if !api.IsSerenaPreparationBuildDeclaration(key) {
			continue
		}
		_, provenance, resolveErr := resolver.ResolveWithProvenance(value)
		if resolveErr != nil {
			return nil, resolveErr
		}
		kind := provenance.Kind()
		if forwarded[key] && kind == "environment-ref" {
			kind = "forwarded"
		}
		if supplied, ok := rawOverlay[key]; ok && supplied == value && kind == "literal" {
			kind = "overlay-literal"
		}
		kinds[key] = kind
	}
	return api.NewSerenaPreparationEnvironmentSource(ctx, manifest, resolver, winning, kinds, bootstrap, binding...)
}

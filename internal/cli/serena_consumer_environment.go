package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"
	"os"
	"path/filepath"
	"strings"
)

func apiConsumerSHA(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func (*serenaPreparationAssembly) ObserveConsumerEnvironment(ctx context.Context, manifest *config.ServerManifest, rawSHA, task string, override *api.SerenaPreparationPolicyV1) (*api.SerenaConsumerEnvironmentObservation, error) {
	if ctx == nil || ctx.Err() != nil || manifest == nil || task == "" {
		return nil, errors.New("SERENA_CONSUMER_ENVIRONMENT_INPUT_MISSING")
	}
	if _, finite := ctx.Deadline(); !finite {
		return nil, errors.New("SERENA_CONSUMER_ENVIRONMENT_DEADLINE_MISSING")
	}
	policy := api.DefaultSerenaPreparationPolicy()
	if override != nil {
		policy = *override
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	root, resolution, err := api.ResolveDaemonStateDirObserved(ctx)
	if err != nil {
		return nil, err
	}
	if err = resolution.Validate(ctx, root); err != nil {
		return nil, err
	}
	overlayPath := filepath.Join(root, overlayBaseName)
	if injected := os.Getenv(daemonOverlayPathEnvVar); injected != "" {
		if filepath.Clean(injected) != overlayPath {
			return nil, errors.New("SERENA_CONSUMER_OVERLAY_NAMESPACE_DRIFT")
		}
		overlayPath = injected
	}
	var reads []*api.StateFileReadObservation
	raw, reader, readErr := api.ReadStateFileInodeAnchoredObserved(ctx, overlayPath, int64(daemon_env_overlay.MaxOverlaySize))
	reads = append(reads, reader)
	var overlay *daemon_env_overlay.Overlay
	if reader == nil {
		return nil, errors.Join(readErr, errors.New("SERENA_CONSUMER_OVERLAY_READER_MISSING"))
	}
	if readErr != nil {
		if err = reader.ValidateAbsence(ctx, overlayPath, api.StateFileReadLeafNotFound); err != nil {
			return nil, errors.Join(readErr, err)
		}
		overlay, err = daemon_env_overlay.Decode(nil, overlayPath)
	} else {
		if err = reader.Validate(ctx, overlayPath, int64(daemon_env_overlay.MaxOverlaySize), raw, apiConsumerSHA(raw)); err != nil {
			return nil, err
		}
		overlay, err = daemon_env_overlay.Decode(raw, overlayPath)
	}
	if err != nil {
		return nil, err
	}
	// Determine whether the same winning declaration owner actually requires a
	// vault; unrelated optional runtime secrets remain the runtime owner's input.
	declarations := mergeDaemonEnvMaps(manifest.Env, daemon_env_overlay.LookupOverlay(overlay, task))
	needVault := false
	for key, value := range declarations {
		if api.IsSerenaPreparationBuildDeclaration(key) && strings.HasPrefix(value, "secret:") {
			needVault = true
		}
	}
	var vault *secrets.Vault
	if needVault {
		vault, err = secrets.OpenVaultWithReader(defaultKeyPath(), defaultVaultPath(), func(path string) ([]byte, error) {
			raw, proof, readErr := api.ReadStateFileInodeAnchoredObserved(ctx, path, 64<<20)
			reads = append(reads, proof)
			if proof == nil {
				return nil, errors.Join(readErr, errors.New("SERENA_CONSUMER_REFERENCE_READER_MISSING"))
			}
			if readErr != nil {
				return nil, readErr
			}
			return raw, proof.Validate(ctx, path, 64<<20, raw, apiConsumerSHA(raw))
		})
		if err != nil {
			return nil, err
		}
	}
	bootstrap := map[string]string{}
	for _, key := range api.SerenaBuildBootstrapDeclarationKeys() {
		value, known := os.LookupEnv(key)
		if !known || value == "" {
			return nil, errors.New("SERENA_BUILD_BOOTSTRAP_DECLARATION_MISSING")
		}
		bootstrap[key] = value
	}
	source, err := buildSerenaEnvironmentSourceFromOverlay(ctx, manifest, secrets.NewResolver(vault, nil), task, manifest.EnvForwardLocal, bootstrap, overlay, api.SerenaConsumerEnvironmentSourceBinding{TaskName: task, RawManifestSHA256: rawSHA, Reads: reads})
	if err != nil {
		return nil, err
	}
	return api.NewSerenaConsumerEnvironmentObservation(ctx, source, rawSHA, task, policy)
}

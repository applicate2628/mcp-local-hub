package api

import (
	"errors"
	"path/filepath"

	"mcp-local-hub/internal/config"
)

const SerenaPreparedRuntimeSpecVersion = 2

// materializePreparedSerenaRuntimeSpec is pure. The API owner supplies the
// already resolved published record/binding; this function never installs,
// reads files, resolves secrets or discovers an executable.
func materializePreparedSerenaRuntimeSpec(m *config.ServerManifest, ws WorkspaceEntry, sourceRequirement string, record PreparedSerenaRuntimeRecord, binding PreparedSerenaRuntimeBinding) (*DaemonRuntimeSpec, error) {
	if m == nil || m.Name != SerenaServerName || m.DaemonTemplate == nil || m.Transport != config.TransportNativeHTTP || m.Kind != config.KindWorkspaceScoped || m.DaemonTemplate.Context == "" || ws.WorkspacePath == "" {
		return nil, errors.New("SERENA_PREPARED_MATERIALIZE_INVALID")
	}
	if err := validatePreparedSerenaSelection(record, binding); err != nil {
		return nil, err
	}
	// Drop ONLY the exact inspected uvx --from/source/entrypoint prefix.
	if filepath.Base(m.Command) != "uvx" && filepath.Base(m.Command) != "uvx.exe" {
		return nil, errors.New("SERENA_PREPARED_SOURCE_PREFIX_INVALID")
	}
	if sourceRequirement == "" || len(m.BaseArgs) < 4 || m.BaseArgs[0] != "--from" || m.BaseArgs[1] != sourceRequirement || m.BaseArgs[2] != "serena" || m.BaseArgs[3] != "start-mcp-server" || config.ArgsContainContextFlag(m.BaseArgs) || config.ArgsContainContextFlag(m.DaemonTemplate.ExtraArgsTemplate) {
		return nil, errors.New("SERENA_PREPARED_SOURCE_PREFIX_INVALID")
	}
	clone := *m
	clone.BaseArgs = append([]string(nil), m.BaseArgs[3:]...)
	env, err := preparedSerenaBytecodeEnvironment(m.Env)
	if err != nil {
		return nil, err
	}
	spec := &DaemonRuntimeSpec{SpecVersion: SerenaPreparedRuntimeSpecVersion, PreparedRuntime: &binding, ChildCommand: record.EntrypointIdentity.Path, ChildArgs: buildSerenaChildArgs(&clone, ws.WorkspacePath), EnvRefs: env, UpstreamPort: ws.Port + config.NativeHTTPInternalPortOffset, ExternalPort: ws.Port, WorkspacePath: ws.WorkspacePath}
	return spec, nil
}

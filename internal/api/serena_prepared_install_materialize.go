package api

import (
	"encoding/json"
	"errors"
	"mcp-local-hub/internal/config"
	"strconv"
	"strings"
)

func validatePreparedSerenaSelection(record PreparedSerenaRuntimeRecord, binding PreparedSerenaRuntimeBinding) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	actual, err := record.Binding(raw)
	if err != nil || actual != binding {
		return errors.Join(err, errors.New("SERENA_PREPARED_BINDING_REQUIRED"))
	}
	return nil
}

// The caller supplies a selected record only after original publication and
// fresh physical read/close receiving. This transformation remains pure.
func materializePreparedSerenaDescriptors(m *config.ServerManifest, daemons []SupervisorDaemon, plan *Plan) ([]SupervisorDaemon, error) {
	if m == nil || !IsSerenaServer(m.Name) || len(daemons) == 0 {
		return daemons, nil
	}
	if plan == nil || plan.serenaPreparedBinding == nil || plan.serenaPreparedRecord == nil {
		return nil, errors.New("SERENA_INSTALL_QUALIFIED_SELECTION_REQUIRED")
	}
	if len(m.BaseArgs) < 4 {
		return nil, errors.New("SERENA_PREPARED_SOURCE_PREFIX_INVALID")
	}
	if err := validatePreparedSerenaSelection(*plan.serenaPreparedRecord, *plan.serenaPreparedBinding); err != nil {
		return nil, err
	}
	result := append([]SupervisorDaemon(nil), daemons...)
	for i := range result {
		d := &result[i]
		binding := *plan.serenaPreparedBinding
		if d.Workspace != "" {
			workspace := WorkspaceEntry{WorkspacePath: d.Workspace, Port: d.Port}
			spec, err := materializePreparedSerenaRuntimeSpec(m, workspace, m.BaseArgs[1], *plan.serenaPreparedRecord, binding)
			if err != nil {
				return nil, err
			}
			d.RuntimeSpec = spec
		} else {
			// The wrapper command/argv are unchanged. Its actual child command
			// comes from the same validated ticket's selected entrypoint.
			d.PreparedRuntime = &binding
			env, err := preparedSerenaBytecodeEnvironment(d.Env)
			if err != nil {
				return nil, err
			}
			d.Env = env
		}
		args, argsErr := preparedSerenaWrapperArguments(*d)
		if argsErr != nil {
			return nil, argsErr
		}
		d.Args = args
	}
	return result, nil
}

func preparedSerenaWrapperArguments(descriptor SupervisorDaemon) ([]string, error) {
	if descriptor.TaskName == "" || EffectiveStartupBindDeadlineSeconds(descriptor) <= 0 {
		return nil, errors.New("SERENA_PREPARED_START_SELECTOR_MISSING")
	}
	expected := map[string]string{"--task-name": descriptor.TaskName, "--startup-bind-deadline-seconds": strconv.Itoa(EffectiveStartupBindDeadlineSeconds(descriptor))}
	seen := map[string]bool{}
	args := append([]string(nil), descriptor.Args...)
	for i := 0; i < len(args); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		want, owned := expected[name]
		if !owned {
			continue
		}
		if seen[name] {
			return nil, errors.New("SERENA_PREPARED_START_SELECTOR_DUPLICATE")
		}
		seen[name] = true
		if !inline {
			if i+1 >= len(args) {
				return nil, errors.New("SERENA_PREPARED_START_SELECTOR_MISSING")
			}
			i++
			value = args[i]
		}
		if value != want {
			return nil, errors.New("SERENA_PREPARED_START_SELECTOR_DRIFT")
		}
	}
	for _, name := range []string{"--task-name", "--startup-bind-deadline-seconds"} {
		if !seen[name] {
			args = append(args, name, expected[name])
		}
	}
	return args, nil
}

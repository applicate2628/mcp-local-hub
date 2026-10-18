package cli

import (
	"context"
	"errors"
	"fmt"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/secrets"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type preparedTaskFlag struct {
	value string
	count int
}

func (v *preparedTaskFlag) Set(s string) error { v.count++; v.value = s; return nil }
func (v *preparedTaskFlag) String() string     { return v.value }
func (*preparedTaskFlag) Type() string         { return "string" }

type preparedDeadlineFlag struct {
	value int
	count int
}

func (v *preparedDeadlineFlag) Set(s string) error {
	v.count++
	n, e := strconv.Atoi(s)
	if e != nil || n <= 0 || int64(n) > int64(^uint64(0)>>1)/int64(time.Second) {
		return errors.New("prepared startup deadline invalid")
	}
	v.value = n
	return nil
}
func (v *preparedDeadlineFlag) String() string {
	if v.value == 0 {
		return "0"
	}
	return strconv.Itoa(v.value)
}
func (*preparedDeadlineFlag) Type() string { return "int" }
func preparedFlagContext(parent context.Context, task *preparedTaskFlag, deadline *preparedDeadlineFlag) (context.Context, context.CancelFunc, error) {
	if task == nil || deadline == nil || task.count != 1 || task.value == "" || deadline.count != 1 || deadline.value <= 0 {
		return nil, nil, errors.New("prepared task/deadline missing or duplicated")
	}
	return api.PreparedSerenaStartContext(parent, time.Duration(deadline.value)*time.Second)
}

func runPreparedSerenaWrapper(service, start context.Context, a *api.API, launch *api.PreparedSerenaLaunchObservation, descriptor api.SupervisorDaemon, logPath string) (err error) {
	if err = launch.Validate(start, descriptor); err != nil {
		return err
	}
	owner, err := a.PreparedSerenaConsumerOwnerObserved(start, launch)
	if err != nil {
		return err
	}
	manifest, err := launch.Manifest(start)
	if err != nil {
		return err
	}
	refs := descriptor.Env
	if descriptor.RuntimeSpec != nil {
		refs = descriptor.RuntimeSpec.EnvRefs
	}
	vault, verr := secrets.OpenVaultOptionalWithReader(defaultKeyPath(), defaultVaultPath(), func(path string) ([]byte, error) {
		raw, reader, e := api.ReadStateFileInodeAnchoredObserved(start, path, 64<<20)
		if reader == nil {
			return nil, errors.Join(e, errors.New("prepared runtime reference reader missing"))
		}
		if e != nil {
			if absent := reader.ValidateAbsence(start, path, api.StateFileReadLeafNotFound); absent != nil {
				return nil, errors.Join(e, absent)
			}
			return nil, e
		}
		return raw, reader.Validate(start, path, 64<<20, raw, apiConsumerSHA(raw))
	})
	if verr != nil && secrets.HasSecretRef(refs) {
		return daemonSecretVaultFatalError(descriptor.Server, descriptor.Daemon, defaultKeyPath(), defaultVaultPath(), verr)
	}
	resolved, omitted, err := secrets.NewResolver(vault, nil).ResolveMapBestEffort(refs)
	if err != nil {
		return err
	}
	env, unset, err := mergeResolvedDaemonEnvWithOverlayReader(descriptor.TaskName, resolved, omitted, func(task string) (map[string]string, error) {
		root, resolution, e := api.ResolveDaemonStateDirObserved(start)
		if e != nil {
			return nil, e
		}
		if e = resolution.Validate(start, root); e != nil {
			return nil, e
		}
		path := filepath.Join(root, overlayBaseName)
		if injected := os.Getenv(daemonOverlayPathEnvVar); injected != "" && injected != path {
			return nil, errors.New("prepared runtime overlay namespace drift")
		}
		raw, reader, e := api.ReadStateFileInodeAnchoredObserved(start, path, int64(daemon_env_overlay.MaxOverlaySize))
		if reader == nil {
			return nil, errors.Join(e, errors.New("prepared runtime overlay reader missing"))
		}
		if e != nil {
			if e = reader.ValidateAbsence(start, path, api.StateFileReadLeafNotFound); e != nil {
				return nil, e
			}
			return map[string]string{}, nil
		}
		if e = reader.Validate(start, path, int64(daemon_env_overlay.MaxOverlaySize), raw, apiConsumerSHA(raw)); e != nil {
			return nil, e
		}
		overlay, e := daemon_env_overlay.Decode(raw, path)
		if e != nil {
			return nil, e
		}
		values := daemon_env_overlay.LookupOverlay(overlay, task)
		if daemonOverlayAlreadyApplied(os.Environ()) {
			return overlayValuesFromEnv(values, os.Environ()), nil
		}
		return daemon_env_overlay.ExpandParentPath(values, os.Environ())
	}, manifest.EnvForwardLocal)
	if err != nil {
		return err
	}
	cfg := daemon.HTTPHostConfig{Env: env, UnsetEnv: unset, LogPath: logPath, WorkingDir: descriptor.WorkingDir, OuterBindDeadline: time.Duration(api.EffectiveStartupBindDeadlineSeconds(descriptor)) * time.Second}
	if descriptor.RuntimeSpec != nil {
		cfg.Command = descriptor.RuntimeSpec.ChildCommand
		cfg.Args = serenaProxyChildArgs(descriptor.RuntimeSpec)
		cfg.UpstreamPort = descriptor.RuntimeSpec.UpstreamPort
	} else {
		if filepath.Base(manifest.Command) != "uvx" && filepath.Base(manifest.Command) != "uvx.exe" || len(manifest.BaseArgs) < 4 || manifest.BaseArgs[0] != "--from" || manifest.BaseArgs[2] != "serena" || manifest.BaseArgs[3] != "start-mcp-server" {
			return errors.New("prepared global package prefix mismatch")
		}
		cfg.Args = append([]string(nil), manifest.BaseArgs[3:]...)
		matches := 0
		for _, d := range manifest.Daemons {
			if d.Name == descriptor.Daemon {
				matches++
				cfg.Args = append(cfg.Args, d.ExtraArgs...)
			}
		}
		if matches != 1 {
			return errors.New("prepared global daemon source ambiguous")
		}
		for _, arg := range cfg.Args {
			if arg == "--port" || strings.HasPrefix(arg, "--port=") {
				return errors.New("prepared global upstream port duplicated")
			}
		}
		cfg.UpstreamPort = descriptor.Port + config.NativeHTTPInternalPortOffset
		cfg.Args = append(cfg.Args, "--port", strconv.Itoa(cfg.UpstreamPort))
	}
	h, adapter, err := newPreparedConsumerHTTPHost(start, owner, descriptor, cfg)
	if err != nil {
		return err
	}
	started := false
	scope := &preparedWrapperServerScope{adapter: adapter}
	adapter.wrapper = scope
	defer func() {
		if !started {
			cleanupEnd, boundErr := daemon.PreparedHTTPHostStartupCleanupDeadline(start)
			if boundErr != nil {
				err = errors.Join(err, boundErr)
				return
			}
			cleanup, cancel := context.WithDeadline(context.WithoutCancel(start), cleanupEnd)
			defer cancel()
			scope.deadline = cleanupEnd
			_, stopErr := h.StopObserved(cleanup)
			err = errors.Join(err, stopErr, adapter.ReceiveWrapperServerClose(cleanup, scope.receive(cleanup)))
		}
	}()
	if _, err = h.StartObserved(start); err != nil {
		return fmt.Errorf("httphost.Start: %w%s", err, formatChildExit(h.ExitState()))
	}
	started = true
	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", descriptor.Port), Handler: h.HTTPHandler(), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	scope.server, scope.serveDone, scope.serveEntered = server, done, true
	go func() { defer close(done); done <- server.ListenAndServe() }()
	var cause error
	serverExited := false
	select {
	case cause = <-done:
		serverExited = true
		if errors.Is(cause, http.ErrServerClosed) {
			cause = nil
		}
	case <-service.Done():
	case <-h.ChildExited():
		cause = fmt.Errorf("native-http upstream exited unexpectedly%s", formatChildExit(h.ExitState()))
	}
	shutdown, cancel, shutdownErr := h.PreparedStopContext(context.WithoutCancel(service))
	if shutdownErr != nil {
		return errors.Join(cause, shutdownErr)
	}
	defer cancel()
	scope.deadline, _ = shutdown.Deadline()
	scope.shutdownEntered = true
	scope.shutdownDone = make(chan error, 1)
	var stopErr error
	scope.shutdownError, stopErr, scope.serveError, scope.shutdownReceived, scope.serveReceived = receivePreparedWrapperShutdown(shutdown, server.Shutdown, server.Close, func(ctx context.Context) error {
		_, e := h.StopObserved(ctx)
		return e
	}, scope.shutdownDone, done, serverExited, cause)
	shutdownErr = errors.Join(scope.shutdownError, scope.serveError)
	receiveErr := adapter.ReceiveWrapperServerClose(shutdown, scope.receive(shutdown))
	return errors.Join(cause, shutdownErr, stopErr, receiveErr)
}

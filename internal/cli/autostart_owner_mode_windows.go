//go:build windows

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/autostart"
)

var autostartOwnerModeRunnerFn = runAutostartOwnerModeFromCLI
var autostartStatusOptionsFn = persistedAutostartStatusOptions

func runAutostartOwnerModeFromCLI(owner autostart.OwnerMode, strict bool) error {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	backend, err := autostartBackendFactoryFn()
	if err != nil {
		return fmt.Errorf("autostart backend: %w", err)
	}
	return RunAutostartOwnerMode(owner, strict, StrictModeDeps{
		StateDir:         stateDir,
		GUISettingsPath:  api.SettingsPath(),
		EnableAutorun:    true,
		IntentPath:       filepath.Join(stateDir, "supervisor-intent.json"),
		BreadcrumbPath:   filepath.Join(stateDir, "strict-mode-mutation-incomplete.json"),
		AutostartBackend: backend,
	})
}

func withAutostartGUIControl(deps StrictModeDeps, enable bool, fn func(*api.GUIOwnerControl) error) error {
	if fn == nil {
		return errors.New("autostart GUI control operation is required")
	}
	if deps.GUISettingsPath == "" {
		return errors.New("autostart GUI settings path is required")
	}
	a := &api.API{}
	if enable {
		result, err := a.SetGUIAutorunEnabledIn(deps.GUISettingsPath, true, func(control api.GUIOwnerControl) error {
			return fn(&control)
		})
		if err != nil && result.Applied {
			return fmt.Errorf("autorun preference applied; projection or settings release failed: %w", err)
		}
		return err
	}
	return a.WithGUIOwnerControlIn(deps.GUISettingsPath, func(control api.GUIOwnerControl) error {
		return fn(&control)
	})
}

func disableAutostartForPlatform(backend autostart.Backend) error {
	result, err := (&api.API{}).SetGUIAutorunEnabled(false, func(api.GUIOwnerControl) error {
		return backend.Disable()
	})
	if err != nil && result.Applied {
		return fmt.Errorf("autorun preference applied; task removal or settings release failed: %w", err)
	}
	return err
}

func writeAutostartPlatformDetails(w io.Writer) error {
	return (&api.API{}).WithGUIOwnerControl(func(control api.GUIOwnerControl) error {
		policy := "legacy"
		if control.AutorunEnabled != nil {
			policy = "disabled"
			if *control.AutorunEnabled {
				policy = "enabled"
			}
		}
		_, err := fmt.Fprintf(w, "autorun-policy=%s\nrun-desired=%s\n", policy, control.RunDesired)
		return err
	})
}

func persistedAutostartStatusOptions() (autostart.Options, error) {
	path, err := api.DefaultSupervisorIntentPath()
	if err != nil {
		return autostart.Options{}, fmt.Errorf("resolve supervisor intent: %w", err)
	}
	intent, err := api.ReadSupervisorIntent(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return autostart.Options{OwnerMode: autostart.OwnerModeGUI}, nil
		}
		return autostart.Options{}, fmt.Errorf("read supervisor intent: %w", err)
	}
	mode := intent.EffectiveOwnerMode()
	if !mode.Valid() {
		return autostart.Options{}, fmt.Errorf("invalid persisted owner mode %q", mode)
	}
	return autostart.Options{OwnerMode: mode, StrictMode: intent.StrictMode}, nil
}

func enableAutostartForPlatform(owner autostart.OwnerMode, ownerExplicit, strict bool, strictExplicit bool) error {
	policy, err := autostartStatusOptionsFn()
	if err != nil {
		return fmt.Errorf("autostart enable policy: %w", err)
	}
	if ownerExplicit {
		policy.OwnerMode = owner
	}
	if strictExplicit {
		policy.StrictMode = strict
	}
	return autostartOwnerModeRunnerFn(policy.OwnerMode, policy.StrictMode)
}

func autostartStatusOptionsForPlatform(strict bool, strictExplicit bool) (autostart.Options, bool, error) {
	opts, err := autostartStatusOptionsFn()
	if err != nil {
		return autostart.Options{}, false, err
	}
	if strictExplicit {
		opts.StrictMode = strict
	}
	return opts, true, nil
}

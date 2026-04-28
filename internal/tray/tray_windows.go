//go:build windows

package tray

import (
	"context"

	"github.com/getlantern/systray"
)

func runImpl(ctx context.Context, cfg Config) error {
	onReady := func() {
		// Initial icon + tooltip default to Healthy. State events
		// (when StateCh is wired) overwrite both as transitions
		// arrive. SetIcon before any AddMenuItem so the very first
		// systray.Run frame already has the correct image.
		systray.SetIcon(IconBytes(StateHealthy))
		systray.SetTooltip("mcp-local-hub: healthy")
		mOpen := systray.AddMenuItem("Open dashboard", "Bring the GUI window to front")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit (keep daemons)", "Close the GUI but leave scheduler tasks running")
		go func() {
			for {
				select {
				case <-ctx.Done():
					systray.Quit()
					return
				case <-mOpen.ClickedCh:
					if cfg.ActivateWindow != nil {
						cfg.ActivateWindow()
					}
				case <-mQuit.ClickedCh:
					if cfg.Quit != nil {
						cfg.Quit()
					}
					systray.Quit()
					return
				case state, ok := <-cfg.StateCh:
					if !ok {
						// Producer closed the channel — treat as
						// context cancellation so we don't spin on
						// a closed-receive that always fires.
						systray.Quit()
						return
					}
					systray.SetIcon(IconBytes(state))
					systray.SetTooltip("mcp-local-hub: " + state.String())
				}
			}
		}()
	}
	onExit := func() {}
	systray.Run(onReady, onExit)
	return nil
}

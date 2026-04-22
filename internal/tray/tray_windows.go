//go:build windows

package tray

import (
	"context"

	"github.com/getlantern/systray"
)

func runImpl(ctx context.Context, cfg Config) error {
	onReady := func() {
		systray.SetTooltip("mcp-local-hub")
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
				}
			}
		}()
	}
	onExit := func() {}
	systray.Run(onReady, onExit)
	return nil
}

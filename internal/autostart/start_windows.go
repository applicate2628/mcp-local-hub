//go:build windows

package autostart

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/scheduler"
)

// StartOwner starts the installed Windows autostart owner immediately by
// re-firing its Task Scheduler entry.
func StartOwner() error {
	sch, err := scheduler.New()
	if err != nil {
		return fmt.Errorf("autostart owner start: scheduler: %w", err)
	}
	if err := sch.Run(WindowsTaskName); err != nil {
		return fmt.Errorf("autostart owner start: schtasks /Run %s: %w", WindowsTaskName, err)
	}
	return nil
}

// StartOwnerForExpectedProduct starts only the native GUI task whose definition
// matches the already journal-validated product. Normal StartOwner is unchanged.
func StartOwnerForExpectedProduct(ctx context.Context, product string) error {
	if ctx == nil {
		return fmt.Errorf("autostart guarded owner start: context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if product == "" || !filepath.IsAbs(product) || filepath.Clean(product) != product {
		return fmt.Errorf("autostart guarded owner start: canonical product required")
	}
	resolved, err := filepath.EvalSymlinks(product)
	if err != nil || !strings.EqualFold(resolved, product) {
		return fmt.Errorf("autostart guarded owner start: product resolution refused")
	}
	info, err := os.Stat(product)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("autostart guarded owner start: ordinary product required")
	}
	current, err := user.Current()
	if err != nil || current.Uid == "" {
		return fmt.Errorf("autostart guarded owner start: current owner unavailable")
	}
	sch, err := schedulerFactoryFn()
	if err != nil {
		return fmt.Errorf("autostart guarded owner start: scheduler: %w", err)
	}
	owned, ok := sch.(scheduler.OwnedRunner)
	if !ok {
		return fmt.Errorf("autostart guarded owner start: %w", scheduler.ErrNotImplemented)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := owned.RunOwned(ctx, scheduler.OwnedRunRequest{Name: WindowsTaskName, ExpectedCommand: product, ExpectedRole: "gui", ExpectedOwner: current.Uid}); err != nil {
		return fmt.Errorf("autostart guarded owner start: %w", err)
	}
	return nil
}

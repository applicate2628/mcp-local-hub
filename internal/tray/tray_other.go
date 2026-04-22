//go:build !windows

package tray

import "context"

func runImpl(ctx context.Context, cfg Config) error {
	<-ctx.Done()
	return nil
}

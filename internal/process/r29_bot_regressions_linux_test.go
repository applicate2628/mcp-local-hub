//go:build linux

package process

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestR29POSIXNormalExitKeepsLeaderIdentityThroughCleanup(t *testing.T) {
	cmd := containedPOSIXHelperCommand(t, "group")
	var stdout strings.Builder
	err := RunContainedStream(context.Background(), cmd, ContainedStreamOptions{CleanupTimeout: 5 * time.Second}, func(reader io.Reader) error {
		_, copyErr := io.Copy(&stdout, reader)
		return copyErr
	})
	if err != nil {
		var lifecycle *ContainedRunError
		_ = errors.As(err, &lifecycle)
		if lifecycle != nil {
			t.Fatalf("stage=%s cause=%v cleanup_stage=%s cleanup_cause=%v", lifecycle.Stage, lifecycle.Cause, lifecycle.CleanupStage, lifecycle.CleanupCause)
		}
		t.Fatalf("run=%v", err)
	}
	if cmd.ProcessState == nil {
		t.Fatal("direct child was not reaped after group cleanup")
	}
}

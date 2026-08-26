//go:build windows

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func withCOMRunner(t *testing.T, fn func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error)) {
	t.Helper()
	previous := schedulerCOMRun
	schedulerCOMRun = fn
	t.Cleanup(func() { schedulerCOMRun = previous })
}

func TestSchedulerCOMTypedOutcomesIgnoreHostileLocalizedText(t *testing.T) {
	for _, hostile := range []string{"ERROR: The system cannot find the file specified.", "ОШИБКА: Не удается найти указанный файл."} {
		withCOMRunner(t, func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error) {
			return schedulerCOMResponse{Kind: "task_absent", Phase: hostile}, nil
		})
		_, err := schedulerCOM(context.Background(), schedulerCOMRequest{Operation: "status", Name: `\mcp-local-hub-test`})
		if !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("hostile=%q err=%v", hostile, err)
		}
	}
}

func TestSchedulerCOMMapsTypedAccessCorruptAndUnavailable(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want error
	}{{"permission_denied", ErrPermissionDenied}, {"task_corrupt", ErrTaskCorrupt}, {"scheduler_unavailable", ErrUnavailable}} {
		t.Run(tc.kind, func(t *testing.T) {
			withCOMRunner(t, func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error) {
				return schedulerCOMResponse{Kind: tc.kind}, nil
			})
			_, err := schedulerCOM(context.Background(), schedulerCOMRequest{Operation: "status", Name: `\t`})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want=%v", err, tc.want)
			}
		})
	}
	withCOMRunner(t, func(context.Context, schedulerCOMRequest) (schedulerCOMResponse, error) {
		return schedulerCOMResponse{}, fmt.Errorf("bridge lost")
	})
	if _, err := schedulerCOM(context.Background(), schedulerCOMRequest{Operation: "status", Name: `\t`}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

func TestTaskStatusFromCOMUsesNumericStateOnly(t *testing.T) {
	for _, name := range []string{"English running", "Выполняется"} {
		status, err := taskStatusFromCOM(schedulerCOMTask{Name: name, State: 4})
		if err != nil || status.RuntimeState != TaskRuntimeRunning {
			t.Fatalf("name=%q status=%#v err=%v", name, status, err)
		}
	}
	if _, err := taskStatusFromCOM(schedulerCOMTask{Name: "x", State: 99}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v", err)
	}
}

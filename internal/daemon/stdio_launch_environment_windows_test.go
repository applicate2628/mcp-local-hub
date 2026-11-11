//go:build windows

package daemon

import (
	"context"
	"reflect"
	"testing"
)

func TestStdioLaunchEnvironmentRetainsDriveDirectories(t *testing.T) {
	parent := []string{`=C:=C:\first`, `=D:=D:\work`, `=c:=C:\last`, "Path=tools"}
	want := map[string]string{"=c:": `C:\last`, "=d:": `D:\work`, "path": "tools"}
	actual, err := effectiveStdioEnvironment(parent)
	if err != nil || !reflect.DeepEqual(actual, want) {
		t.Fatalf("effective drive environment = %v, %v", actual, err)
	}
	merged := MergeDaemonEnvironment(parent, map[string]string{"TEST": "value"}, nil)
	actual, err = effectiveStdioEnvironment(merged)
	want["test"] = "value"
	if err != nil || !reflect.DeepEqual(actual, want) {
		t.Fatalf("merged drive environment = %v, %v", actual, err)
	}
	actual, err = effectiveStdioEnvironment(composeChildEnvFrom(parent, nil, []string{"=d:"}))
	delete(want, "test")
	delete(want, "=d:")
	if err != nil || !reflect.DeepEqual(actual, want) {
		t.Fatalf("unset drive environment = %v, %v", actual, err)
	}

	snapshot, err := CaptureStdioLaunchContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, first := preparedStdioFixture(t, snapshot, nil)
	_, same := preparedStdioFixture(t, snapshot, nil)
	first.command.Env = append([]string(nil), parent...)
	same.command.Env = []string{`=d:=D:\work`, `=C:=C:\last`, "PATH=tools"}
	if equal, err := first.SameExecution(same); err != nil || !equal {
		t.Fatalf("same drive environment = %v, %v", equal, err)
	}
	if _, err := first.ObserveExecutionProfile(snapshot.ctx); err != nil {
		t.Fatalf("drive environment profile: %v", err)
	}
	same.command.Env[0] = `=D:=D:\changed`
	if equal, err := first.SameExecution(same); err != nil || equal {
		t.Fatalf("changed drive environment = %v, %v", equal, err)
	}
}

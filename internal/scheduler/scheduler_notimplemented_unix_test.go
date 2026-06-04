//go:build linux || darwin

package scheduler

import (
	"context"
	"errors"
	"testing"
)

func TestPlatformSchedulerReturnsTypedNotImplemented(t *testing.T) {
	_, err := newPlatformScheduler()
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("newPlatformScheduler error = %v, want ErrNotImplemented", err)
	}
}

func TestPlatformSchedulerMethodsReturnTypedNotImplemented(t *testing.T) {
	s := notImplementedSchedulerForTest()
	spec := TaskSpec{Name: "mcp-local-hub-test"}
	check := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrNotImplemented) {
			t.Fatalf("%s error = %v, want ErrNotImplemented", name, err)
		}
	}

	check("Create", s.Create(spec))
	check("Delete", s.Delete(spec.Name))
	check("Run", s.Run(spec.Name))
	check("Stop", s.Stop(spec.Name))
	_, err := s.Status(spec.Name)
	check("Status", err)
	_, err = s.List("mcp-local-hub-")
	check("List", err)
	_, err = s.ListContext(context.Background(), "mcp-local-hub-")
	check("ListContext", err)
	_, err = s.ExportXML(spec.Name)
	check("ExportXML", err)
	check("ImportXML", s.ImportXML(spec.Name, []byte("<Task/>")))
}

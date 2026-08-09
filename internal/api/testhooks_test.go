package api

import (
	"testing"

	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/scheduler"
)

func TestSetInstallAutostartFixtureForTest_BindsAndRestoresAllSeams(t *testing.T) {
	origSchedulerFactory := schedulerFactoryFn
	origFactory := installAutostartBackendFactoryFn
	origStartOwner := installAutostartOwnerStartFn
	t.Cleanup(func() {
		schedulerFactoryFn = origSchedulerFactory
		installAutostartBackendFactoryFn = origFactory
		installAutostartOwnerStartFn = origStartOwner
	})

	var restoredSchedulerFactoryCalls, restoredFactoryCalls, restoredStartOwnerCalls int
	schedulerFactoryFn = func() (scheduler.Scheduler, error) {
		restoredSchedulerFactoryCalls++
		return nil, nil
	}
	installAutostartBackendFactoryFn = func() (autostart.Backend, error) {
		restoredFactoryCalls++
		return nil, nil
	}
	installAutostartOwnerStartFn = func() error {
		restoredStartOwnerCalls++
		return nil
	}

	var fixtureSchedulerFactoryCalls, fixtureFactoryCalls, fixtureStartOwnerCalls int
	restore := SetInstallAutostartFixtureForTest(
		func() (scheduler.Scheduler, error) {
			fixtureSchedulerFactoryCalls++
			return nil, nil
		},
		func() (autostart.Backend, error) {
			fixtureFactoryCalls++
			return nil, nil
		},
		func() error {
			fixtureStartOwnerCalls++
			return nil
		},
	)

	if _, err := schedulerFactoryFn(); err != nil {
		t.Fatalf("fixture scheduler factory: %v", err)
	}
	if _, err := installAutostartBackendFactoryFn(); err != nil {
		t.Fatalf("fixture backend factory: %v", err)
	}
	if err := installAutostartOwnerStartFn(); err != nil {
		t.Fatalf("fixture start owner: %v", err)
	}
	if fixtureSchedulerFactoryCalls != 1 || fixtureFactoryCalls != 1 || fixtureStartOwnerCalls != 1 {
		t.Fatalf("fixture calls scheduler/factory/start = %d/%d/%d, want 1/1/1", fixtureSchedulerFactoryCalls, fixtureFactoryCalls, fixtureStartOwnerCalls)
	}
	if restoredSchedulerFactoryCalls != 0 || restoredFactoryCalls != 0 || restoredStartOwnerCalls != 0 {
		t.Fatalf("restored seams ran before restore: scheduler/factory/start = %d/%d/%d, want 0/0/0", restoredSchedulerFactoryCalls, restoredFactoryCalls, restoredStartOwnerCalls)
	}

	restore()
	restore()
	if _, err := schedulerFactoryFn(); err != nil {
		t.Fatalf("restored scheduler factory: %v", err)
	}
	if _, err := installAutostartBackendFactoryFn(); err != nil {
		t.Fatalf("restored backend factory: %v", err)
	}
	if err := installAutostartOwnerStartFn(); err != nil {
		t.Fatalf("restored start owner: %v", err)
	}
	if restoredSchedulerFactoryCalls != 1 || restoredFactoryCalls != 1 || restoredStartOwnerCalls != 1 {
		t.Fatalf("restored calls scheduler/factory/start = %d/%d/%d, want 1/1/1", restoredSchedulerFactoryCalls, restoredFactoryCalls, restoredStartOwnerCalls)
	}
}

func TestSetInstallAutostartFixtureForTest_RejectsNilWithoutPartialBinding(t *testing.T) {
	origSchedulerFactory := schedulerFactoryFn
	origFactory := installAutostartBackendFactoryFn
	origStartOwner := installAutostartOwnerStartFn
	t.Cleanup(func() {
		schedulerFactoryFn = origSchedulerFactory
		installAutostartBackendFactoryFn = origFactory
		installAutostartOwnerStartFn = origStartOwner
	})

	var schedulerFactoryCalls, factoryCalls, startOwnerCalls int
	schedulerFactoryFn = func() (scheduler.Scheduler, error) {
		schedulerFactoryCalls++
		return nil, nil
	}
	installAutostartBackendFactoryFn = func() (autostart.Backend, error) {
		factoryCalls++
		return nil, nil
	}
	installAutostartOwnerStartFn = func() error {
		startOwnerCalls++
		return nil
	}
	nonNilSchedulerFactory := func() (scheduler.Scheduler, error) { return nil, nil }
	nonNilFactory := func() (autostart.Backend, error) { return nil, nil }
	nonNilStartOwner := func() error { return nil }

	assertPanics(t, func() { SetInstallAutostartFixtureForTest(nil, nonNilFactory, nonNilStartOwner) })
	assertPanics(t, func() { SetInstallAutostartFixtureForTest(nonNilSchedulerFactory, nil, nonNilStartOwner) })
	assertPanics(t, func() { SetInstallAutostartFixtureForTest(nonNilSchedulerFactory, nonNilFactory, nil) })
	if _, err := schedulerFactoryFn(); err != nil {
		t.Fatalf("scheduler factory after rejected fixture: %v", err)
	}
	if _, err := installAutostartBackendFactoryFn(); err != nil {
		t.Fatalf("factory after rejected fixture: %v", err)
	}
	if err := installAutostartOwnerStartFn(); err != nil {
		t.Fatalf("start owner after rejected fixture: %v", err)
	}
	if schedulerFactoryCalls != 1 || factoryCalls != 1 || startOwnerCalls != 1 {
		t.Fatalf("seams after rejected fixture scheduler/factory/start = %d/%d/%d, want 1/1/1", schedulerFactoryCalls, factoryCalls, startOwnerCalls)
	}
}

func assertPanics(t *testing.T, call func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("call did not panic")
		}
	}()
	call()
}

package cli

import "testing"

func assertRegistryReleased(t testing.TB, release func() error) {
	t.Helper()
	if release == nil {
		t.Fatal("nil Registry release callback")
	}
	if err := release(); err != nil {
		t.Fatalf("release Registry lock: %v", err)
	}
}

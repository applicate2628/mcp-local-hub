package api

import (
	"slices"
	"testing"
)

// TestRemoteHTTPCapable_IncludesMimocode pins bot PR #420 finding 5: mimocode is
// HTTP-native (AddEntry writes url+headers, GetEntry round-trips them — pinned in
// internal/clients/headers_roundtrip_test.go), so a transport=remote-http binding
// for client:mimocode must be ON the capability matrix. Before this fix mimocode
// was absent, so BuildPlanWithOpts rejected it as off-matrix and the
// remote-http draft/import helpers never offered it.
func TestRemoteHTTPCapable_IncludesMimocode(t *testing.T) {
	if !isRemoteHTTPCapableClient("mimocode") {
		t.Fatalf("mimocode is not remote-http-capable; remoteHTTPCapableClients = %v", remoteHTTPCapableClients)
	}
	if !slices.Contains(remoteHTTPCapableClients, "mimocode") {
		t.Errorf("mimocode missing from remoteHTTPCapableClients: %v", remoteHTTPCapableClients)
	}
	// The list must stay alphabetically sorted (the file header guarantees stable
	// emitted YAML), so adding mimocode must not have broken ordering.
	sorted := slices.Clone(remoteHTTPCapableClients)
	slices.Sort(sorted)
	if !slices.Equal(sorted, remoteHTTPCapableClients) {
		t.Errorf("remoteHTTPCapableClients not alphabetically sorted: %v", remoteHTTPCapableClients)
	}
}

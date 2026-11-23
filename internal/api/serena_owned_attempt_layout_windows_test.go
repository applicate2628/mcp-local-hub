//go:build windows

package api

import (
	"path/filepath"
	"strings"
	"testing"
)

// These are path-value checks. No native observation, handle or cleanup
// authority is constructed, and no process or installation is started.
func TestSerenaOwnedAttemptLocationContract(t *testing.T) {
	request := SerenaOwnedTreeRequest{
		Namespace:             "D:\\unit-serena",
		PackageIdentitySHA256: strings.Repeat("1", 64),
		OperationID:           "operation-" + strings.Repeat("2", 32),
		GenerationID:          "generation-1",
		Kind:                  "attempt",
	}
	parent, leaf := serenaOwnedTreeLocation(request)
	// Fixed SHA-256 vector of the exact 131-byte domain/package/operation
	// encoding. The expected leaf does not use the production hash helper.
	const wantLeaf = "692983ac0f9f9fe8fa21f2ea196bad6fdfd4d44e4296db482a38c71e08ab582a"
	if parent != "D:\\unit-serena\\a" || leaf != wantLeaf {
		t.Fatalf("compact attempt = (%q, %q), want fixed parent/digest", parent, leaf)
	}
	legacy := filepath.Join(request.Namespace, request.PackageIdentitySHA256, "attempts", request.OperationID)
	if len(legacy)-len(filepath.Join(parent, leaf)) != 50 {
		t.Fatal("generated operation identifier did not recover 50 path characters")
	}
	for _, tc := range []struct {
		name         string
		change       func(*SerenaOwnedTreeRequest)
		wantParent   string
		wantSameLeaf bool
	}{
		{"package last digit", func(r *SerenaOwnedTreeRequest) {
			r.PackageIdentitySHA256 = strings.Repeat("1", 63) + "2"
		}, parent, false},
		{"operation last digit", func(r *SerenaOwnedTreeRequest) {
			r.OperationID = "operation-" + strings.Repeat("2", 31) + "3"
		}, parent, false},
		{"different generation", func(r *SerenaOwnedTreeRequest) {
			r.GenerationID = "generation-2"
		}, parent, true},
		{"different namespace", func(r *SerenaOwnedTreeRequest) {
			r.Namespace = "D:\\other-serena"
		}, "D:\\other-serena\\a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := request
			tc.change(&changed)
			gotParent, gotLeaf := serenaOwnedTreeLocation(changed)
			if gotParent != tc.wantParent || (gotLeaf == leaf) != tc.wantSameLeaf {
				t.Fatalf("location identity boundary = (%q, %q)", gotParent, gotLeaf)
			}
		})
	}
}

func TestSerenaOwnedTreeLocationPreservesGenerationAndQualificationLayouts(t *testing.T) {
	request := SerenaOwnedTreeRequest{
		Namespace:             "D:\\unit-serena",
		PackageIdentitySHA256: strings.Repeat("1", 64),
		OperationID:           "operation-1",
		GenerationID:          "generation-1",
		QualificationID:       "qualification-1",
	}
	for _, tc := range []struct {
		kind, branch, leaf string
	}{
		{"generation", "generations", "generation-1"},
		{"qualification", "qualifications", "qualification-1"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			changed := request
			changed.Kind = tc.kind
			if tc.kind == "generation" {
				changed.QualificationID = ""
			}
			parent, leaf := serenaOwnedTreeLocation(changed)
			wantParent := filepath.Join(request.Namespace, request.PackageIdentitySHA256, tc.branch)
			if parent != wantParent || leaf != tc.leaf {
				t.Fatalf("published layout changed: parent=%q leaf=%q", parent, leaf)
			}
		})
	}
}

func TestSerenaOwnedTreeParentPathUsesSharedLocation(t *testing.T) {
	for _, tc := range []struct {
		kind, wantParent string
	}{
		{"attempt", "D:\\unit-serena\\a"},
		{"generation", "D:\\unit-serena\\" + strings.Repeat("1", 64) + "\\generations"},
		{"qualification", "D:\\unit-serena\\" + strings.Repeat("1", 64) + "\\qualifications"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			// parentPath is a pure value calculation. This fixture is never
			// passed to native checks, inventory, acquisition or deletion.
			owner := windowsSerenaOwnedTree{request: SerenaOwnedTreeRequest{
				Namespace:             "D:\\unit-serena",
				PackageIdentitySHA256: strings.Repeat("1", 64),
				OperationID:           "operation-1",
				GenerationID:          "generation-1",
				QualificationID:       "qualification-1",
				Kind:                  tc.kind,
			}}
			if got := owner.parentPath(); got != tc.wantParent {
				t.Fatalf("owner parent = %q, want %q", got, tc.wantParent)
			}
		})
	}
}

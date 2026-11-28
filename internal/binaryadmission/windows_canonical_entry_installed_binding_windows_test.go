//go:build windows

package binaryadmission

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestInstalledV4ReaderRequiresFullProductBinding(t *testing.T) {
	rawRegistry, err := json.Marshal(boundRuntimeV4Registry(t))
	if err != nil {
		t.Fatal(err)
	}
	prior := compiledWindowsProductRegistryBase64
	compiledWindowsProductRegistryBase64 = base64.StdEncoding.EncodeToString(rawRegistry)
	t.Cleanup(func() { compiledWindowsProductRegistryBase64 = prior })

	t.Run("exact_binding", func(t *testing.T) {
		targets, binding, runtimePath := boundRuntimeV2Fixture(t)
		o, err := AcquireInstalledWindowsCanonicalEntryDispatchSetV2(context.Background(), targets.CanonicalEntryPath, binding)
		keepBoundRuntimeOwnerForCleanup(t, o)
		if err != nil {
			t.Fatal(err)
		}
		got, err := o.DispatchSnapshot()
		if err != nil || got.BuildID != binding.BuildID() || got.Product.Path != targets.ProductPath ||
			got.RuntimeIndex.Path != runtimePath || got.RuntimeIndex.SHA256 != heldRuntimeIndexFixtureSHA256 {
			t.Fatalf("exact installed reader binding or snapshot differs: %+v error=%v", got, err)
		}
		if err := o.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(runtimePath, runtimePath+".released"); err != nil {
			t.Fatalf("exact installed reader failed to release its private original: %v", err)
		}
		t.Log("exact installed V4 binding received synthetic originals and released them")
	})
	t.Run("same_build_id_wrong_full_binding", func(t *testing.T) {
		targets, binding, _ := boundRuntimeV2Fixture(t)
		wrongDigest := strings.Repeat("0", 64)
		if wrongDigest == binding.ExpectedCanonicalEntrySHA256() {
			t.Fatal("fixture launcher hash unexpectedly equals the wrong digest")
		}
		wrong, err := NewWindowsCanonicalEntryBuildBindingV1(binding.BuildID(), wrongDigest, binding.ReverseProjection())
		if err != nil {
			t.Fatal(err)
		}
		o, err := AcquireInstalledWindowsCanonicalEntryDispatchSetV2(context.Background(), targets.CanonicalEntryPath, wrong)
		keepBoundRuntimeOwnerForCleanup(t, o)
		if err == nil || err.Error() != "executing Product build binding disagrees with original Product" ||
			o.entry == nil || o.product == nil || o.identity == nil || o.runtimeIndex != nil || o.ready {
			t.Fatalf("same BuildID with wrong full binding did not refuse scoped original acquisition: %v", err)
		}
		if _, err = o.DispatchSnapshot(); err == nil {
			t.Fatal("refused installed binding lent a snapshot")
		}
		if err = o.Close(); err != nil {
			t.Fatal(err)
		}
		if err = os.Rename(targets.ProductPath, targets.ProductPath+".released"); err != nil {
			t.Fatalf("partial installed reader did not release the Product original: %v", err)
		}
		t.Log("same BuildID with wrong full binding refused; partial synthetic originals released")
	})
}

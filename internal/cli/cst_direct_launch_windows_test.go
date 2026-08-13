//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/daemon"
)

func TestCstLaunchCapabilityConfigDefaultOffWithoutProvisionReceipt(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	if got := cstLaunchCapabilityConfig(api.SupervisorCstTaskV1, "default"); got != nil {
		t.Fatal("missing provision receipt must keep cst direct launch default-off")
	}
}

func TestCstLaunchCapabilityConfigRejectsWrongIdentityBeforeReceiptRead(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	t.Cleanup(api.SetDaemonStateRootForTest(stateDir))
	if got := cstLaunchCapabilityConfig("not-cst", "default"); got != nil {
		t.Fatal("non-CST server admitted")
	}
	if got := cstLaunchCapabilityConfig(api.SupervisorCstTaskV1, "not-default"); got != nil {
		t.Fatal("non-default CST daemon admitted")
	}
}

func TestCstDirectReceiptParserRejectsUnknownAndMalformedProvisionIdentity(t *testing.T) {
	root := apitest.HardenedTempDir(t)
	image := filepath.Join(root, "runtime.exe")
	manifest := filepath.Join(root, "manifest.json")
	if err := os.WriteFile(image, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := `{"version":1,"launch_profile":"cst-direct-v1","image_path":"` + strings.ReplaceAll(image, `\`, `\\`) + `","image_sha256":"` + strings.Repeat("a", 64) + `","runtime_manifest_path":"` + strings.ReplaceAll(manifest, `\`, `\\`) + `","runtime_manifest_sha256":"` + strings.Repeat("b", 64) + `","provisioned_package_identity_schema":"mcphub.cst.provisioned-package-identity.v1","provisioned_package_identity_sha256":"` + strings.Repeat("c", 64) + `","frontend_args":["--role=frontend"]}`
	if _, err := daemon.ParseCstDirectImageReceiptV1([]byte(base[:len(base)-1] + `,"unknown":true}`)); err == nil {
		t.Fatal("unknown receipt field accepted")
	}
	receipt, err := daemon.ParseCstDirectImageReceiptV1([]byte(base))
	if err != nil {
		t.Fatalf("parse closed exact receipt shape: %v", err)
	}
	if receipt.LaunchProfile != daemon.CstDirectLaunchProfileV1 || len(receipt.FrontendArgs) != 1 {
		t.Fatalf("parsed receipt mismatch: %#v", receipt)
	}
}

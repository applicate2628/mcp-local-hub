//go:build windows

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"mcp-local-hub/internal/api"
)

type e2eEnrollmentClient struct {
	enrolled chan api.HubEnrollmentEnrollV1
}

func (c *e2eEnrollmentClient) Enroll(_ context.Context, req api.HubEnrollmentEnrollV1) (api.HubEnrollmentReceiptV1, error) {
	c.enrolled <- req
	return api.HubEnrollmentReceiptV1{Version: 1, Correlation: req.Correlation, State: api.HubEnrollmentStateEnrolled, ChannelSettled: true}, nil
}
func (c *e2eEnrollmentClient) Cancel(_ context.Context, req api.HubEnrollmentCancelV1) (api.HubEnrollmentReceiptV1, error) {
	return api.HubEnrollmentReceiptV1{Version: 1, Correlation: req.Correlation, State: api.HubEnrollmentStateCancelled, ChannelSettled: true}, nil
}

func TestCstDirectFrontendCrossesGoCapabilityAndFixedLocalPipe(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	nativeRoot := filepath.Join(root, "servers", "electromagnetics-mcp", "native", "cst-runtime")
	image := filepath.Join(t.TempDir(), "mcphub-cst-test-frontend.exe")
	build := exec.Command("powershell.exe", "-NoProfile", "-File", filepath.Join(nativeRoot, "build_test_frontend.ps1"), "-OutputPath", image)
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("build test-only native frontend: %v\n%s", buildErr, output)
	}
	manifest := filepath.Join(t.TempDir(), "cst-native-runtime-manifest-v1.json")
	hash := func(path string) string {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	}
	imageHash := hash(image)
	manifestRaw := fmt.Sprintf(`{"schema":"mcphub.cst.native-runtime-manifest.v1","runtime_image_sha256":"%s","roles":{"frontend":{"inherited_handles":["stdin","stdout","stderr","capability-read"],"revoked_before_package_code":true}},"package_load":{"required_receipt":"ProvisionedPackageIdentityV1"}}`, imageHash)
	if err := os.WriteFile(manifest, []byte(manifestRaw), 0o600); err != nil {
		t.Fatal(err)
	}
	pipe := `\\.\pipe\mcp-local-hub-cst-e2e-` + strings.ReplaceAll(t.Name(), "/", "-")
	listener, err := winio.ListenPipe(pipe, &winio.PipeConfig{MessageMode: false, InputBufferSize: 4096, OutputBufferSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan []byte, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			received <- nil
			return
		}
		defer conn.Close()
		raw, _ := io.ReadAll(io.LimitReader(conn, 64))
		received <- raw
	}()
	enrollment := &e2eEnrollmentClient{enrolled: make(chan api.HubEnrollmentEnrollV1, 1)}
	receipt := &CstDirectImageReceiptV1{Version: 1, LaunchProfile: CstDirectLaunchProfileV1, ImagePath: image, ImageSHA256: imageHash, RuntimeManifestPath: manifest, RuntimeManifestSHA256: hash(manifest), ProvisionedPackageIdentitySchema: provisionedPackageIdentitySchemaV1, ProvisionedPackageIdentitySHA256: strings.Repeat("a", 64), FrontendArgs: []string{"--role=frontend"}}
	host, err := NewStdioHost(HostConfig{Command: image, Args: []string{"--role=frontend"}, Env: map[string]string{"MCPHUB_CST_TEST_FRONTEND_PIPE": pipe}, LaunchCapability: &LaunchCapabilityConfig{Task: api.SupervisorCstTaskV1, Generation: 1, Enrollment: enrollment, DirectImage: receipt}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := host.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer host.Stop()
	var req api.HubEnrollmentEnrollV1
	select {
	case req = <-enrollment.enrolled:
	case <-ctx.Done():
		t.Fatal("enrollment not observed")
	}
	select {
	case raw := <-received:
		if len(raw) != 32 {
			t.Fatalf("native frontend local-pipe payload=%d bytes, want 32", len(raw))
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != req.CapabilitySHA256 {
			t.Fatal("native frontend did not deliver the Go-enrolled capability")
		}
	case <-ctx.Done():
		t.Fatal("native frontend never crossed the fixed local pipe")
	}
}

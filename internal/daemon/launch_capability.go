package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

const LaunchCapabilityHandleEnv = "MCPHUB_CST_LAUNCH_HANDLE"

const CstDirectLaunchProfileV1 = "cst-direct-v1"

// CstDirectImageReceiptV1 is the closed, provisioner-produced input accepted
// by the existing StdioHost launch owner.  It grants no authority by itself:
// the provisioned package receipt and both content identities must be present
// and are rechecked from retained files immediately before exec.Cmd.Start.
type CstDirectImageReceiptV1 struct {
	Version                          int      `json:"version"`
	LaunchProfile                    string   `json:"launch_profile"`
	ImagePath                        string   `json:"image_path"`
	ImageSHA256                      string   `json:"image_sha256"`
	RuntimeManifestPath              string   `json:"runtime_manifest_path"`
	RuntimeManifestSHA256            string   `json:"runtime_manifest_sha256"`
	ProvisionedPackageIdentitySchema string   `json:"provisioned_package_identity_schema"`
	ProvisionedPackageIdentitySHA256 string   `json:"provisioned_package_identity_sha256"`
	FrontendArgs                     []string `json:"frontend_args"`
}

const provisionedPackageIdentitySchemaV1 = "mcphub.cst.provisioned-package-identity.v1"

type cstNativeRuntimeManifestV1 struct {
	Schema             string `json:"schema"`
	RuntimeImageSHA256 string `json:"runtime_image_sha256"`
	Roles              struct {
		Frontend struct {
			InheritedHandles         []string `json:"inherited_handles"`
			RevokedBeforePackageCode bool     `json:"revoked_before_package_code"`
		} `json:"frontend"`
	} `json:"roles"`
	PackageLoad struct {
		RequiredReceipt string `json:"required_receipt"`
	} `json:"package_load"`
}

// ParseCstDirectImageReceiptV1 rejects extension fields so a newer receipt can
// never be silently interpreted under the v1 launch contract.
func ParseCstDirectImageReceiptV1(raw []byte) (*CstDirectImageReceiptV1, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var receipt CstDirectImageReceiptV1
	if err := dec.Decode(&receipt); err != nil {
		return nil, fmt.Errorf("decode CstDirectImageReceiptV1: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}
	return &receipt, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("CstDirectImageReceiptV1 has trailing JSON value")
		}
		return fmt.Errorf("decode CstDirectImageReceiptV1 trailing data: %w", err)
	}
	return nil
}

type LaunchCapabilityConfig struct {
	Task        string
	Generation  int
	Enrollment  api.HubEnrollmentClientV1
	DirectImage *CstDirectImageReceiptV1
}

type launchCapabilityPipe interface {
	writeAndClose([]byte) error
	close() error
	apply(*exec.Cmd) error
	locator() uintptr
}

type launchCapabilityOps struct {
	random32 func(*[32]byte) error
	zero32   func(*[32]byte)
	openPipe func() (launchCapabilityPipe, error)
}

type preparedLaunchCapability struct {
	pipe        launchCapabilityPipe
	client      api.HubEnrollmentClientV1
	correlation string
	directImage *verifiedCstDirectImage
	cancelOnce  sync.Once
}

type verifiedCstDirectImage struct {
	receipt  CstDirectImageReceiptV1
	image    *os.File
	manifest *os.File
}

func verifyCstDirectImage(receipt *CstDirectImageReceiptV1) (*verifiedCstDirectImage, error) {
	if receipt == nil || receipt.Version != 1 || receipt.LaunchProfile != CstDirectLaunchProfileV1 {
		return nil, fmt.Errorf("cst-direct-v1 receipt unavailable")
	}
	if receipt.ProvisionedPackageIdentitySchema != provisionedPackageIdentitySchemaV1 || !validSHA256(receipt.ProvisionedPackageIdentitySHA256) {
		return nil, fmt.Errorf("cst-direct-v1 provisioned package identity unavailable")
	}
	if len(receipt.FrontendArgs) != 1 || receipt.FrontendArgs[0] != "--role=frontend" {
		return nil, fmt.Errorf("cst-direct-v1 frontend argv mismatch")
	}
	if !validSHA256(receipt.ImageSHA256) || !validSHA256(receipt.RuntimeManifestSHA256) {
		return nil, fmt.Errorf("cst-direct-v1 content identity malformed")
	}
	image, err := openCstDirectIdentityFile(receipt.ImagePath)
	if err != nil {
		return nil, fmt.Errorf("open cst-direct-v1 image: %w", err)
	}
	manifest, err := openCstDirectIdentityFile(receipt.RuntimeManifestPath)
	if err != nil {
		_ = image.Close()
		return nil, fmt.Errorf("open cst-direct-v1 manifest: %w", err)
	}
	v := &verifiedCstDirectImage{receipt: *receipt, image: image, manifest: manifest}
	if err := v.verify(); err != nil {
		_ = v.close()
		return nil, err
	}
	return v, nil
}

func validSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func canonicalAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && path == filepath.Clean(path)
}

func hashOpenFile(file *os.File) (string, []byte, error) {
	if file == nil {
		return "", nil, fmt.Errorf("identity file unavailable")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return "", nil, err
	}
	if info.Size() < 0 || info.Size() > 8<<20 {
		return "", nil, fmt.Errorf("identity file size %d exceeds cst-direct-v1 bound", info.Size())
	}
	h := sha256.New()
	var capture bytes.Buffer
	if _, err := io.Copy(io.MultiWriter(h, &capture), file); err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(h.Sum(nil)), capture.Bytes(), nil
}

func (v *verifiedCstDirectImage) verify() error {
	if v == nil || !canonicalAbsolutePath(v.receipt.ImagePath) || !canonicalAbsolutePath(v.receipt.RuntimeManifestPath) {
		return fmt.Errorf("cst-direct-v1 paths must be canonical absolute paths")
	}
	imageHash, _, err := hashOpenFile(v.image)
	if err != nil || imageHash != v.receipt.ImageSHA256 {
		return fmt.Errorf("cst-direct-v1 image identity mismatch")
	}
	manifestHash, rawManifest, err := hashOpenFile(v.manifest)
	if err != nil || manifestHash != v.receipt.RuntimeManifestSHA256 {
		return fmt.Errorf("cst-direct-v1 manifest identity mismatch")
	}
	var manifest cstNativeRuntimeManifestV1
	if err := json.Unmarshal(rawManifest, &manifest); err != nil {
		return fmt.Errorf("decode cst-direct-v1 runtime manifest: %w", err)
	}
	wantHandles := []string{"stdin", "stdout", "stderr", "capability-read"}
	if manifest.Schema != "mcphub.cst.native-runtime-manifest.v1" ||
		manifest.RuntimeImageSHA256 != v.receipt.ImageSHA256 ||
		strings.Join(manifest.Roles.Frontend.InheritedHandles, "|") != strings.Join(wantHandles, "|") ||
		!manifest.Roles.Frontend.RevokedBeforePackageCode ||
		manifest.PackageLoad.RequiredReceipt != "ProvisionedPackageIdentityV1" {
		return fmt.Errorf("cst-direct-v1 runtime manifest contract mismatch")
	}
	return nil
}

func (v *verifiedCstDirectImage) close() error {
	if v == nil {
		return nil
	}
	var errs []error
	if v.image != nil {
		errs = append(errs, v.image.Close())
		v.image = nil
	}
	if v.manifest != nil {
		errs = append(errs, v.manifest.Close())
		v.manifest = nil
	}
	return errors.Join(errs...)
}

func prepareLaunchCapability(ctx context.Context, cfg LaunchCapabilityConfig, ops launchCapabilityOps) (*preparedLaunchCapability, error) {
	if cfg.Task != api.SupervisorCstTaskV1 || cfg.Generation <= 0 || cfg.Enrollment == nil {
		return nil, fmt.Errorf("launch capability config incomplete")
	}
	var directImage *verifiedCstDirectImage
	if cfg.DirectImage != nil {
		var err error
		directImage, err = verifyCstDirectImage(cfg.DirectImage)
		if err != nil {
			return nil, err
		}
	}
	closeDirectImage := true
	defer func() {
		if closeDirectImage {
			_ = directImage.close()
		}
	}()
	pipe, err := ops.openPipe()
	if err != nil {
		return nil, fmt.Errorf("create launch capability pipe: %w", err)
	}
	closePipe := true
	defer func() {
		if closePipe {
			_ = pipe.close()
		}
	}()

	var capability [32]byte
	if err := ops.random32(&capability); err != nil {
		ops.zero32(&capability)
		return nil, fmt.Errorf("generate launch capability: %w", err)
	}
	defer ops.zero32(&capability)
	var correlationBytes [16]byte
	if _, err := rand.Read(correlationBytes[:]); err != nil {
		return nil, fmt.Errorf("generate enrollment correlation: %w", err)
	}
	correlation := hex.EncodeToString(correlationBytes[:])
	digest := sha256.Sum256(capability[:])
	req := api.HubEnrollmentEnrollV1{
		Version: 1, Op: "enroll", Correlation: correlation, Task: cfg.Task,
		Generation: cfg.Generation, CapabilitySHA256: hex.EncodeToString(digest[:]),
	}
	receipt, err := cfg.Enrollment.Enroll(ctx, req)
	if err != nil || api.ValidateHubEnrollmentReceiptV1(receipt, correlation, api.HubEnrollmentStateEnrolled) != nil {
		cancelEnrollmentBounded(cfg.Enrollment, correlation)
		return nil, nil // default-off: child still launches, sampler receives no capability.
	}
	if err := pipe.writeAndClose(capability[:]); err != nil {
		cancelEnrollmentBounded(cfg.Enrollment, correlation)
		return nil, fmt.Errorf("write launch capability: %w", err)
	}
	closePipe = false
	closeDirectImage = false
	return &preparedLaunchCapability{pipe: pipe, client: cfg.Enrollment, correlation: correlation, directImage: directImage}, nil
}

func (p *preparedLaunchCapability) apply(cmd *exec.Cmd) error {
	if p == nil || p.pipe == nil {
		return fmt.Errorf("launch capability unavailable")
	}
	return p.pipe.apply(cmd)
}

func (p *preparedLaunchCapability) close() error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.pipe != nil {
		errs = append(errs, p.pipe.close())
	}
	if p.directImage != nil {
		errs = append(errs, p.directImage.close())
	}
	return errors.Join(errs...)
}

func (p *preparedLaunchCapability) start(start func() error) error {
	if p == nil {
		return start()
	}
	if p.directImage != nil {
		if err := p.directImage.verify(); err != nil {
			_ = p.close()
			p.cancel()
			return fmt.Errorf("verify cst-direct-v1 immediately before start: %w", err)
		}
	}
	err := start()
	_ = p.close()
	if err != nil {
		p.cancel()
	}
	return err
}

func (p *preparedLaunchCapability) cancel() {
	if p == nil || p.client == nil || p.correlation == "" {
		return
	}
	p.cancelOnce.Do(func() { cancelEnrollmentBounded(p.client, p.correlation) })
}

func cancelEnrollmentBounded(client api.HubEnrollmentClientV1, correlation string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = client.Cancel(ctx, api.HubEnrollmentCancelV1{Version: 1, Op: "cancel", Correlation: correlation})
}

func (p *preparedLaunchCapability) environmentKey() string { return LaunchCapabilityHandleEnv }
func (p *preparedLaunchCapability) environmentValue() string {
	if p == nil || p.pipe == nil {
		return ""
	}
	return strconv.FormatUint(uint64(p.pipe.locator()), 10)
}
func (p *preparedLaunchCapability) handleInventory() []string {
	return []string{"stdin", "stdout", "stderr", "capability-read"}
}

func mapWithOverride(src map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(src)+1)
	for k, v := range src {
		if envKeyNorm(k) != envKeyNorm(key) {
			out[k] = v
		}
	}
	out[key] = value
	return out
}

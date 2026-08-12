package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

const LaunchCapabilityHandleEnv = "MCPHUB_CST_LAUNCH_HANDLE"

type LaunchCapabilityConfig struct {
	Task       string
	Generation int
	Enrollment api.HubEnrollmentClientV1
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
	cancelOnce  sync.Once
}

func prepareLaunchCapability(ctx context.Context, cfg LaunchCapabilityConfig, ops launchCapabilityOps) (*preparedLaunchCapability, error) {
	if cfg.Task != api.SupervisorCstTaskV1 || cfg.Generation <= 0 || cfg.Enrollment == nil {
		return nil, fmt.Errorf("launch capability config incomplete")
	}
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
	return &preparedLaunchCapability{pipe: pipe, client: cfg.Enrollment, correlation: correlation}, nil
}

func (p *preparedLaunchCapability) apply(cmd *exec.Cmd) error {
	if p == nil || p.pipe == nil {
		return fmt.Errorf("launch capability unavailable")
	}
	return p.pipe.apply(cmd)
}

func (p *preparedLaunchCapability) start(start func() error) error {
	if p == nil {
		return start()
	}
	err := start()
	_ = p.pipe.close()
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

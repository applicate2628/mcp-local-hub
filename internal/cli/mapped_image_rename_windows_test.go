//go:build windows

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/process"
)

const mappedImageRenameHelperEnv = "MCPHUB_MAPPED_IMAGE_RENAME_HELPER"

// TestWindowsMappedImageRenameRequiresExactGenerationReceipt demonstrates the
// Windows retained-image condition without invoking product lifecycle code.
// It is a diagnostic fixture, not a replacement-authority or quiescence test.
func TestWindowsMappedImageRenameRequiresExactGenerationReceipt(t *testing.T) {
	if marker := os.Getenv(mappedImageRenameHelperEnv); marker != "" {
		if err := os.WriteFile(marker, []byte("ready"), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		return
	}

	canonical := copyCurrentTestBinaryAsReconcileMcphub(t)
	marker := filepath.Join(t.TempDir(), "mapped-image-ready")
	child := exec.Command(canonical, "-test.run=^TestWindowsMappedImageRenameRequiresExactGenerationReceipt$")
	child.Env = append(os.Environ(), mappedImageRenameHelperEnv+"="+marker)

	job, err := process.NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("new kill-on-close job: %v", err)
	}
	if _, err := process.StartWithJob(job, child); err != nil {
		_ = job.Close()
		t.Fatalf("start contained hidden helper: %v", err)
	}
	if child.Process == nil {
		_ = job.Close()
		t.Fatal("contained helper started without Process")
	}
	t.Cleanup(func() {
		if err := job.Close(); err != nil {
			t.Errorf("close helper job: %v", err)
		}
		if _, err := child.Process.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Errorf("wait helper after job close: %v", err)
			}
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("contained mapped-image helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}

	retained := canonical + ".old-mapped-image"
	if err := os.Rename(canonical, retained); err != nil {
		t.Fatalf("rename live canonical image aside: %v", err)
	}
	if err := os.WriteFile(canonical, []byte("different canonical test image"), 0o600); err != nil {
		t.Fatalf("write different canonical image: %v", err)
	}

	actualImage, startedAt, err := mappedImageTestIdentity(child.Process.Pid)
	if err != nil {
		t.Fatalf("query retained helper identity: %v", err)
	}
	if !strings.EqualFold(filepath.Clean(actualImage), filepath.Clean(retained)) {
		t.Fatalf("QueryFullProcessImageName image=%q, want exact retained path %q", actualImage, retained)
	}
	if process.PIDExecutableMatches(child.Process.Pid, canonical) {
		t.Fatalf("PIDExecutableMatches(%d, canonical %q)=true after live image rename", child.Process.Pid, canonical)
	}
	if err := process.VerifyPIDIdentity(process.PIDIdentityProof{
		PID:            child.Process.Pid,
		ExecutablePath: retained,
		StartedAt:      startedAt,
	}); err != nil {
		t.Fatalf("VerifyPIDIdentity(retained image, exact PID/start): %v", err)
	}
	foreign := filepath.Join(t.TempDir(), filepath.Base(retained))
	if err := os.WriteFile(foreign, []byte("foreign same-basename image"), 0o600); err != nil {
		t.Fatalf("write foreign same-basename image: %v", err)
	}
	if process.PIDExecutableMatches(child.Process.Pid, foreign) {
		t.Fatalf("PIDExecutableMatches(%d, foreign same-basename %q)=true", child.Process.Pid, foreign)
	}
}

func mappedImageTestIdentity(pid int) (imagePath, startedAt string, retErr error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", "", fmt.Errorf("open helper PID %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)

	for size := uint32(windows.MAX_PATH); size <= 32768; size *= 2 {
		buf := make([]uint16, size)
		n := size
		if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err == nil {
			var creation, exit, kernel, user windows.Filetime
			if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
				return "", "", fmt.Errorf("query helper PID %d process times: %w", pid, err)
			}
			return windows.UTF16ToString(buf[:n]), time.Unix(0, creation.Nanoseconds()).UTC().Format(time.RFC3339Nano), nil
		}
	}
	return "", "", fmt.Errorf("QueryFullProcessImageName helper PID %d", pid)
}

//go:build windows

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/buildinfo"
)

// This explicit private diagnostic is separate from the ordinary literal suite.
// The deadline selects an attempt; all image/source/native evidence is received
// from the real compiled image and original production observation command.
func TestPrivatePublisherCurrentImageObservation(t *testing.T) {
	deadlineText := os.Getenv("MCPHUB_PRIVATE_PUBLISHER_OBSERVATION_DEADLINE")
	if deadlineText == "" {
		t.Skip("requires the original source-bound private CLI observation builder")
	}
	end, err := time.Parse(time.RFC3339Nano, deadlineText)
	if err != nil || !time.Now().Before(end) {
		t.Fatalf("original observation deadline: %q %v", deadlineText, err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), end)
	defer cancel()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() > 64<<20 {
		t.Fatalf("original test image size: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := binaryadmission.ParseWindowsPublisherSourceIdentity(raw)
	if err != nil || parsed.Role != binaryadmission.WindowsArtifactRolePublisherTest {
		t.Fatalf("actual private image role/source: %+v %v", parsed, err)
	}
	source, err := buildinfo.CurrentPublisherSource()
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Validate(parsed.SourceEpoch); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	imageSHA := hex.EncodeToString(digest[:])
	wantRoleRefusal := fmt.Sprintf("VERSIONINFO role %q disagrees with expected role %q", binaryadmission.WindowsArtifactRolePublisherTest, binaryadmission.WindowsArtifactRoleProduct)
	if _, err := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{Path: path, Role: binaryadmission.WindowsArtifactRoleProduct, SHA256: imageSHA}); err == nil || !strings.Contains(err.Error(), wantRoleRefusal) {
		t.Fatalf("actual private image must refuse Product for its role: %v", err)
	}
	state := filepath.Join(t.TempDir(), "state-must-remain-uncreated")
	restore := api.SetDaemonStateRootForTest(state)
	defer restore()
	if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private state precondition: %v", err)
	}
	var out bytes.Buffer
	command := newVersionCmdReal()
	command.SetContext(ctx)
	command.SetOut(&out)
	command.SetErr(&out)
	command.SetArgs([]string{"--observe-runtime-prerequisites", "--observation-deadline", deadlineText})
	commandErr := command.Execute()
	// Preserve the actual command envelope even when a later assertion refuses it.
	if _, err := fmt.Fprintf(os.Stdout, "PRIVATE_PUBLISHER_OBSERVATION_JSON=%s\n", bytes.TrimSpace(out.Bytes())); err != nil {
		t.Fatalf("observation frame write: %v", err)
	}
	if commandErr != nil {
		t.Fatalf("actual read-only observation: %T: %v", commandErr, commandErr)
	}
	var observed currentProductObservationEnvelope
	decoder := json.NewDecoder(bytes.NewReader(out.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observed); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("extra observation output: %v", err)
	}
	if !observed.Complete || observed.PrimaryError != "" || len(observed.CleanupErrors) != 0 || observed.Image == nil || observed.Native == nil || observed.Root == nil {
		t.Fatalf("original observation incomplete: %+v", observed)
	}
	if observed.Native.PID != uint32(os.Getpid()) || observed.Native.NativeBirthFILETIME == 0 || !observed.Image.ResourcesClosed ||
		observed.Image.ImageSHA256 != imageSHA || observed.Image.SourceEpoch != source.Epoch() {
		t.Fatalf("actual self/image/source differ: %+v", observed)
	}
	stateDigest := sha256.Sum256([]byte(state))
	root := observed.Root
	if !root.Complete || root.ReleaseKind != "test-state-path-readonly" || root.PathSHA256 != hex.EncodeToString(stateDigest[:]) ||
		root.HRESULTDefined || root.HRESULT != nil || root.AllocationAcquired || root.DecodeSucceeded || root.FreeCalled || root.FreeReturned {
		t.Fatalf("test path asserted native KnownFolder facts or lost its original path: %+v", root)
	}
	if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only observation created state: %v", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
}

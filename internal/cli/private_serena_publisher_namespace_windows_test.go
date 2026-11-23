//go:build windows

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/process"
)

// An incomplete original API retains its native/namespace originals until this
// one diagnostic process exits. This is neither a registry nor another owner.
var retainedPrivateSerenaNamespaceAPI *api.API

type privateSerenaNamespaceTreeEntry struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
}

type privateSerenaNamespaceFrame struct {
	Schema          string                                       `json:"schema"`
	Complete        bool                                         `json:"complete"`
	Deadline        string                                       `json:"deadline"`
	StateDirectory  string                                       `json:"state_directory"`
	ImageSHA256     string                                       `json:"image_sha256"`
	ProductRefusal  string                                       `json:"product_refusal,omitempty"`
	Observation     *api.PrivateSerenaPublisherNamespaceSnapshot `json:"observation,omitempty"`
	Before          []privateSerenaNamespaceTreeEntry            `json:"before"`
	After           []privateSerenaNamespaceTreeEntry            `json:"after"`
	AfterRepeat     []privateSerenaNamespaceTreeEntry            `json:"after_repeat"`
	RepeatRefusal   string                                       `json:"repeat_refusal,omitempty"`
	OriginalError   string                                       `json:"original_error,omitempty"`
	ValidationError string                                       `json:"validation_error,omitempty"`
}

// This separate diagnostic never prepares or installs a runtime. The existing
// assembly is passed unchanged; only the original private namespace method runs.
// The controller supplies one deadline and an absent leaf below its protected
// retained D run. No TempDir cleanup removes the original namespace evidence.
func TestPrivateSerenaPublisherNamespace_PreInstallOriginalOwner(t *testing.T) {
	deadlineText := os.Getenv("MCPHUB_PRIVATE_PUBLISHER_OBSERVATION_DEADLINE")
	state := os.Getenv("MCPHUB_PRIVATE_SERENA_NAMESPACE_ROOT")
	if deadlineText == "" && state == "" {
		t.Skip("requires the original source-bound private namespace qualifier")
	}
	end, err := time.Parse(time.RFC3339Nano, deadlineText)
	if err != nil || !time.Now().Before(end) {
		t.Fatalf("original namespace deadline: %q %v", deadlineText, err)
	}
	ctx, cancel := context.WithDeadline(t.Context(), end)
	defer cancel()
	frame := privateSerenaNamespaceFrame{
		Schema: "private-serena-namespace-qualification-v1", Deadline: deadlineText,
		StateDirectory: state, Before: []privateSerenaNamespaceTreeEntry{},
		After: []privateSerenaNamespaceTreeEntry{}, AfterRepeat: []privateSerenaNamespaceTreeEntry{},
	}
	defer func() {
		raw, err := json.Marshal(frame)
		if err != nil {
			t.Errorf("namespace observation frame: %v", err)
			return
		}
		if _, err := fmt.Fprintf(os.Stdout, "PRIVATE_SERENA_NAMESPACE_OBSERVATION_JSON=%s\n", raw); err != nil {
			t.Errorf("namespace observation frame write: %v", err)
		}
	}()
	allowedRoot := `D:\dev\ChatGPT`
	relative, relErr := filepath.Rel(allowedRoot, state)
	if relErr != nil || !filepath.IsAbs(state) || filepath.Clean(state) != state ||
		relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.Base(state) != "namespace-state" || filepath.Base(filepath.Dir(state)) != "owner" {
		t.Fatalf("namespace path must be the original retained D run leaf: %q %v", state, relErr)
	}
	if err := privateSerenaNamespaceOrdinaryDirectory(filepath.Dir(state)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original namespace leaf must be absent before admission: %v", err)
	}
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
		t.Fatalf("original private image size/type: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := binaryadmission.ParseWindowsPublisherSourceIdentity(raw)
	if err != nil || identity.Role != binaryadmission.WindowsArtifactRolePublisherTest {
		t.Fatalf("actual private role/source: %+v %v", identity, err)
	}
	source, err := buildinfo.CurrentPublisherSource()
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Validate(identity.SourceEpoch); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	frame.ImageSHA256 = hex.EncodeToString(digest[:])
	_, productErr := binaryadmission.AdmitWindowsRole(binaryadmission.WindowsArtifact{Path: path, Role: binaryadmission.WindowsArtifactRoleProduct, SHA256: frame.ImageSHA256})
	wantProductRefusal := fmt.Sprintf("VERSIONINFO role %q disagrees with expected role %q", binaryadmission.WindowsArtifactRolePublisherTest, binaryadmission.WindowsArtifactRoleProduct)
	if productErr == nil || !strings.Contains(productErr.Error(), wantProductRefusal) {
		t.Fatalf("private image must refuse ordinary Product specifically for its role: %v", productErr)
	}
	frame.ProductRefusal = productErr.Error()
	restore := api.SetDaemonStateRootForTest(state)
	defer restore()
	a, err := api.NewAPIWithPrivateSerenaIntegrationPublisher(ctx, newSerenaPreparationAssembly())
	retainedPrivateSerenaNamespaceAPI = a
	if err != nil {
		frame.OriginalError = err.Error()
		t.Fatalf("original private publisher admission: %T: %v", err, err)
	}
	observation, observeErr := a.ObservePrivateSerenaPublisherNamespace(ctx)
	if observation != nil {
		snapshot := observation.Snapshot()
		frame.Observation = &snapshot
	}
	if observeErr != nil {
		frame.OriginalError = observeErr.Error()
		t.Fatalf("original namespace acquisition/binding/close: %T: %v", observeErr, observeErr)
	}
	if err := observation.Validate(ctx, a); err != nil {
		frame.ValidationError = err.Error()
		t.Fatalf("original closed namespace observation: %T: %v", err, err)
	}
	snapshot := observation.Snapshot()
	frame.Observation = &snapshot
	publisher, image, root := snapshot.Publisher, snapshot.Image, snapshot.StateRoot
	stateDigest := sha256.Sum256([]byte(state))
	if snapshot.Schema != "private-serena-publisher-namespace-v1" || !snapshot.Complete || !snapshot.Bound || !snapshot.NamespaceClosed ||
		snapshot.PrimaryError != "" || snapshot.CloseError != "" || snapshot.FinalizationError != "" ||
		publisher.Schema != process.PublisherIncarnationSchema || publisher.PID != uint32(os.Getpid()) || publisher.NativeBirthFILETIME == 0 ||
		publisher.ResourceScope != process.PublisherExclusiveLocalResources || publisher.SourceEpoch != source.Epoch() ||
		publisher.ImageSHA256 != frame.ImageSHA256 || publisher.ImageIdentitySHA256 != image.ImageIdentitySHA256 ||
		image.ImageSHA256 != frame.ImageSHA256 || image.SourceEpoch != source.Epoch() || !image.ResourcesClosed {
		t.Fatalf("original publisher/namespace tuple differs: %+v", snapshot)
	}
	if !root.Complete || root.ReleaseKind != "test-state-path-readonly" || root.PathSHA256 != hex.EncodeToString(stateDigest[:]) ||
		root.HRESULTDefined || root.HRESULT != nil || root.AllocationAcquired || root.DecodeSucceeded || root.FreeCalled || root.FreeReturned {
		t.Fatalf("test state source lost its original path or claimed native KnownFolder facts: %+v", root)
	}
	key := filepath.Base(snapshot.NamespaceDirectory)
	keyBytes, keyErr := hex.DecodeString(key)
	if keyErr != nil || len(keyBytes) != sha256.Size || strings.ToLower(key) != key || filepath.Dir(snapshot.NamespaceDirectory) != state {
		t.Fatalf("original namespace is not one direct digest child: %q", snapshot.NamespaceDirectory)
	}
	frame.After, err = privateSerenaNamespaceTree(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	wantTree := []privateSerenaNamespaceTreeEntry{{Path: ".", Directory: true}, {Path: key, Directory: true}}
	if !reflect.DeepEqual(frame.After, wantTree) {
		t.Fatalf("namespace-only method wrote beyond its two empty directories: %+v", frame.After)
	}
	repeated, repeatErr := a.ObservePrivateSerenaPublisherNamespace(ctx)
	if repeatErr != nil {
		frame.RepeatRefusal = repeatErr.Error()
	}
	if repeated != nil || repeatErr == nil || repeatErr.Error() != "SERENA_PRIVATE_PUBLISHER_ACTIVITY_ALREADY_CLAIMED" {
		t.Fatalf("same original API accepted another namespace attempt: observation=%v err=%v", repeated, repeatErr)
	}
	if err := observation.Validate(ctx, a); err != nil {
		frame.ValidationError = err.Error()
		t.Fatalf("repeat refusal changed the original closed observation: %v", err)
	}
	frame.AfterRepeat, err = privateSerenaNamespaceTree(ctx, state)
	if err != nil || !reflect.DeepEqual(frame.AfterRepeat, frame.After) {
		t.Fatalf("refused repeat changed the actual tree: %+v %v", frame.AfterRepeat, err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatal(err)
	}
	frame.Complete = true
	retainedPrivateSerenaNamespaceAPI = nil
}

func privateSerenaNamespaceOrdinaryDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attributes, err := windows.GetFileAttributes(name)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("original namespace path must be an ordinary non-reparse directory")
	}
	return nil
}

func privateSerenaNamespaceTree(ctx context.Context, root string) ([]privateSerenaNamespaceTreeEntry, error) {
	result := []privateSerenaNamespaceTreeEntry{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := errors.Join(ctx.Err(), walkErr); err != nil {
			return err
		}
		if len(result) >= 16 {
			return errors.New("namespace observation exceeds its directory-only bound")
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result = append(result, privateSerenaNamespaceTreeEntry{Path: relative, Directory: entry.IsDir()})
		if !entry.IsDir() {
			return errors.New("namespace-only observation created a file")
		}
		return privateSerenaNamespaceOrdinaryDirectory(path)
	})
	return result, err
}

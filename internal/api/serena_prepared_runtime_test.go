package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakePreparedSerena struct {
	raw                 []byte
	writes              int
	events              []string
	installFailure      bool
	noChildInstallation bool
	cleanupFailure      bool
	afterRenameFailure  bool
	foreignAfterWrite   bool
	pending             []PreparedSerenaConsumerReference
	state               string
}

func (f *fakePreparedSerena) LockPackage(context.Context, string) (func() error, error) {
	f.events = append(f.events, "package-lock")
	return func() error { f.events = append(f.events, "package-unlock"); return nil }, nil
}
func (f *fakePreparedSerena) LockIntentOperation(context.Context) (func() error, error) {
	f.events = append(f.events, "intent-lock")
	return func() error { f.events = append(f.events, "intent-unlock"); return nil }, nil
}
func (f *fakePreparedSerena) ReadReady(context.Context, string) ([]byte, bool, error) {
	return append([]byte(nil), f.raw...), len(f.raw) > 0, nil
}
func (f *fakePreparedSerena) ReadyNamespace(context.Context, string) (*SerenaRecordNamespaceObservation, error) {
	return nil, errors.New("synthetic namespace has no original capability")
}
func (f *fakePreparedSerena) WriteReady(_ context.Context, _ string, b []byte) error {
	f.writes++
	f.raw = append([]byte(nil), b...)
	if f.afterRenameFailure && f.writes == 1 {
		if f.foreignAfterWrite {
			f.raw = []byte("foreign")
		}
		return errors.New("after-rename")
	}
	return nil
}
func (f *fakePreparedSerena) ValidateRecord(context.Context, PreparedSerenaRuntimeRecord) error {
	return nil
}
func (f *fakePreparedSerena) PublicationAvailable(_ context.Context, r PreparedSerenaRuntimeRecord) (bool, error) {
	return r.GenerationID == "old" || f.state == "published", nil
}
func (f *fakePreparedSerena) AcquireRoots(_ context.Context, r PreparedSerenaRequest, _ string) (PreparedSerenaOwnedRoots, error) {
	f.events = append(f.events, "roots")
	return PreparedSerenaOwnedRoots{}, nil
}
func fakeClosure(id string) PreparedSerenaExecutionClosure {
	return PreparedSerenaExecutionClosure{nativeEvidence: fakePreparedNativeEvidence{}, OriginalOperationID: id, RootExitKnown: true, MembersExited: true, HandlesClosed: true, IOClosed: true, ReceiptSHA256: strings.Repeat("a", 64)}
}

type fakePreparedNativeEvidence struct{}

func (fakePreparedNativeEvidence) Complete() bool { return true }
func (f *fakePreparedSerena) Install(_ context.Context, r PreparedSerenaRequest, _ PreparedSerenaOwnedRoots) (PreparedSerenaInstallation, error) {
	f.events = append(f.events, "install")
	v := PreparedSerenaInstallation{Closure: fakeClosure(r.OperationID), SourceSHA256: strings.Repeat("b", 64), ResolutionSHA256: strings.Repeat("c", 64), InventorySHA256: strings.Repeat("d", 64), Entrypoint: PreparedSerenaFile{Path: `C:\runtime\serena.exe`, SHA256: strings.Repeat("e", 64), Device: "device", FileID: "file"}, UVReceiptSHA256: strings.Repeat("f", 64)}
	if f.noChildInstallation {
		v.Closure.RootExitKnown = false
		v.Closure.MembersExited = false
		v.Closure.NoChildKnown = true
		v.Closure.NoChildReleased = true
	}
	if f.installFailure {
		return v, errors.New("controlled-install-refusal")
	}
	return v, nil
}
func (f *fakePreparedSerena) Qualify(_ context.Context, r PreparedSerenaRequest, _ PreparedSerenaOwnedRoots, _ PreparedSerenaInstallation) (PreparedSerenaQualification, error) {
	f.events = append(f.events, "qualify")
	return PreparedSerenaQualification{Closure: fakeClosure(r.OperationID), ReceiptSHA256: strings.Repeat("f", 64), RuntimeProtocolQualified: true, IndependentOfAttemptRoots: true, FinalEnvironmentInventorySHA256: strings.Repeat("d", 64)}, nil
}
func (f *fakePreparedSerena) CleanupAttempt(context.Context, PreparedSerenaOwnedRoots, PreparedSerenaExecutionClosure) (PreparedSerenaCleanup, error) {
	f.events = append(f.events, "cleanup")
	if f.cleanupFailure {
		return PreparedSerenaCleanup{}, errors.New("cleanup")
	}
	return PreparedSerenaCleanup{ReceiptSHA256: strings.Repeat("f", 64), AttemptAbsent: true, LinksDischarged: true, NoPendingResources: true}, nil
}
func (f *fakePreparedSerena) BeginOperation(_ context.Context, _ PreparedSerenaRequest, _ string) (PreparedSerenaOperationUpdateV2, error) {
	return PreparedSerenaOperationUpdateV2{}, errors.New("synthetic operation has no issued publisher observation")
}
func (f *fakePreparedSerena) PersistOperation(_ context.Context, update PreparedSerenaOperationUpdateV2) (*StateFilePublicationObservation, error) {
	f.state = update.Record.Phase
	f.events = append(f.events, update.Record.Phase)
	return nil, errors.New("synthetic writer has no issued publication observation")
}
func (f *fakePreparedSerena) FinishPreparation(context.Context, PreparedSerenaRequest, *StateFilePublicationObservation) (*PreparedSerenaPreparationCloseObservation, error) {
	return nil, errors.New("synthetic effects have no issued final-close observation")
}
func (f *fakePreparedSerena) References(context.Context, string) (PreparedSerenaReferenceSnapshot, error) {
	return PreparedSerenaReferenceSnapshot{Pending: f.pending, OriginalConsumersClosed: true}, nil
}
func (f *fakePreparedSerena) RetireGeneration(context.Context, PreparedSerenaRuntimeRecord, PreparedSerenaReferenceSnapshot) error {
	f.events = append(f.events, "retired")
	return nil
}
func preparedRequest() PreparedSerenaRequest {
	return PreparedSerenaRequest{OperationID: "operation", GenerationID: "new", Namespace: `C:\state\runtime`, OwnedTreeLimits: DefaultSerenaTreeLimits(), ManifestSHA256: strings.Repeat("a", 64), PackageManifestProjectionSHA256: strings.Repeat("a", 64), BuildEnvironmentSHA256: strings.Repeat("b", 64), SourceRequirement: "git+https://example.invalid/serena@" + strings.Repeat("a", 40), SourceCommit: strings.Repeat("a", 40), Package: "serena-agent", UV: PreparedSerenaFile{`C:\uv.exe`, strings.Repeat("b", 64), "d", "u"}, Interpreter: PreparedSerenaFile{`C:\python.exe`, strings.Repeat("c", 64), "d", "p"}, Launcher: PreparedSerenaFile{`C:\stdlib\venvlauncher.exe`, strings.Repeat("d", 64), "d", "l"}, PlatformABISHA256: strings.Repeat("d", 64), PolicySHA256: strings.Repeat("e", 64), BuildSettingsSHA256: strings.Repeat("f", 64), WorkLimit: time.Second, CleanupLimit: time.Second, WorkDeadline: time.Now().Add(time.Second), AbsoluteCleanupDeadline: time.Now().Add(2*time.Second - time.Millisecond)}
}
func TestPreparedIdentityUsesPackageProjectionNotRawRuntimeManifest(t *testing.T) {
	r := preparedRequest()
	first, err := r.Identity()
	if err != nil {
		t.Fatal(err)
	}
	r.ManifestSHA256 = strings.Repeat("9", 64)
	other, err := r.Identity()
	if err != nil || other != first {
		t.Fatal("raw runtime manifest changed package identity")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["manifest_sha256"]; present {
		t.Fatal("raw manifest escaped private audit")
	}
	r.BuildEnvironmentSHA256 = strings.Repeat("8", 64)
	changed, err := r.Identity()
	if err != nil || changed == first {
		t.Fatal("build environment variation not bound")
	}
	r.BuildEnvironmentSHA256 = ""
	if _, err = r.Identity(); err == nil {
		t.Fatal("missing build projection admitted")
	}
}
func TestPreparedOwnerRequiresIssuedOperationBeforeAcquisition(t *testing.T) {
	f := &fakePreparedSerena{}
	o, err := NewPreparedSerenaRuntimeOwner(f)
	if err != nil {
		t.Fatal(err)
	}
	_, err = o.Prepare(context.Background(), preparedRequest())
	if err == nil || f.writes != 0 || strings.Contains(strings.Join(f.events, ","), "roots") {
		t.Fatal("unissued publisher operation reached acquisition")
	}
}
func syntheticPreparedRecord(r PreparedSerenaRequest) PreparedSerenaRuntimeRecord {
	key, _ := r.Identity()
	ready := true
	digest := strings.Repeat("a", 64)
	return PreparedSerenaRuntimeRecord{Schema: PreparedSerenaRecordSchema, OperationID: r.OperationID, GenerationID: r.GenerationID, RequestIdentitySHA256: key, NamespaceSHA256: digest, NamespaceSchema: SerenaPackageNamespaceSchema, LeafParentSHA256: digest, SourceSHA256: digest, UVSHA256: r.UV.SHA256, InterpreterSHA256: r.Interpreter.SHA256, PlatformABISHA256: r.PlatformABISHA256, PolicySHA256: r.PolicySHA256, ResolutionSHA256: digest, EnvironmentInventorySHA256: digest, EntrypointIdentity: PreparedSerenaFile{Path: `C:\runtime\serena.exe`, SHA256: digest, Device: "synthetic", FileID: "synthetic"}, QualificationReceiptSHA256: digest, CleanupReceiptSHA256: digest, Ready: &ready, PublishedAtUTC: time.Now().UTC().Format(time.RFC3339Nano)}
}
func TestPreparedOwnerWireAndIdentity(t *testing.T) {
	r := preparedRequest()
	a, e := r.Identity()
	if e != nil {
		t.Fatal(e)
	}
	r.OperationID = "another"
	r.GenerationID = "another-generation"
	b, _ := r.Identity()
	if a != b {
		t.Fatal("per-operation identity split package")
	}
	r.SourceRequirement = "https://credential@example.invalid/serena@" + r.SourceCommit
	if _, e = r.Identity(); e == nil {
		t.Fatal("credential identity accepted")
	}
	encoded, e := json.Marshal(syntheticPreparedRecord(preparedRequest()))
	if e != nil {
		t.Fatal(e)
	}
	f := &fakePreparedSerena{raw: encoded}
	for _, raw := range [][]byte{[]byte(strings.Replace(string(f.raw), `"ready":true`, `"READY":true`, 1)), []byte(strings.Replace(string(f.raw), `"ready":true`, `"ready":null`, 1)), []byte(strings.Replace(string(f.raw), `"ready":true`, `"ready":true,"ready":true`, 1)), []byte(strings.Replace(string(f.raw), `"operation_id":"operation"`, `"operation_id":""`, 1)), []byte(strings.Replace(string(f.raw), `"operation_id":"operation",`, "", 1))} {
		if _, e := decodePreparedSerenaRecord(raw); e == nil {
			t.Fatal("degraded wire accepted")
		}
	}
	if ValidatePreparedSerenaEnvironment(map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}) != nil {
		t.Fatal("valid policy refused")
	}
	if ValidatePreparedSerenaEnvironment(map[string]string{"PYTHONDONTWRITEBYTECODE": "1", "PYTHONPATH": "foreign"}) == nil {
		t.Fatal("import override accepted")
	}
}

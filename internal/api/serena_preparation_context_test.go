//go:build windows

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/secrets"
)

func interpreterABIData(t *testing.T, version string, tuple []int) []byte {
	t.Helper()
	raw, e := json.Marshal(serenaInterpreterABI{Version: version, VersionInfo: tuple, Platform: "win32", Machine: "AMD64", SOABI: "cp313-win_amd64", Stdlib: `C:\synthetic\Lib`})
	if e != nil {
		t.Fatal(e)
	}
	return append(raw, '\n')
}

// This complete outer operation is passive synthetic DATA. It creates no
// publisher/source/native issuer and cannot satisfy publication receiving.
func policyOperationData(t *testing.T) (PreparedSerenaRuntimeRecord, PreparedSerenaOperationRecordV2) {
	t.Helper()
	d := strings.Repeat("a", 64)
	p := DefaultSerenaPreparationPolicy()
	sha, _ := p.IdentitySHA256()
	raw := interpreterABIData(t, "3.13.11", []int{3, 13, 11})
	ready := true
	record := PreparedSerenaRuntimeRecord{Schema: PreparedSerenaRecordSchema, OperationID: "op-data", GenerationID: "generation-data", RequestIdentitySHA256: d, NamespaceSHA256: d, NamespaceSchema: SerenaPackageNamespaceSchema, LeafParentSHA256: d, SourceSHA256: d, UVSHA256: d, InterpreterSHA256: d, PlatformABISHA256: preparedSerenaHash(raw), PolicySHA256: sha, ResolutionSHA256: d, EnvironmentInventorySHA256: d, EntrypointIdentity: PreparedSerenaFile{Path: `C:\synthetic\serena.exe`, SHA256: d, Device: "data-device", FileID: "data-file"}, QualificationReceiptSHA256: d, CleanupReceiptSHA256: d, Ready: &ready, PublishedAtUTC: "2026-11-12T00:00:00Z"}
	publisher := &process.PublisherIncarnationRecordV1{Schema: process.PublisherIncarnationSchema, Version: 1, OwnerInstanceID: "data-owner", PID: 1, NativeBirthFILETIME: 1, NativeImagePathSHA256: d, ImageIdentitySHA256: d, ImageSHA256: d, PrincipalSIDSHA256: d, SourceEpoch: d, OperationID: record.OperationID, NamespaceSHA256: d, ResourceScope: process.PublisherExclusiveLocalResources, NativeCaptureSHA256: d}
	op := PreparedSerenaOperationRecordV2{Schema: PreparedSerenaPolicyOperationSchema, Version: 2, OperationID: record.OperationID, GenerationID: record.GenerationID, PackageKey: d, RequestIdentitySHA256: d, PackageManifestProjectionSHA256: d, BuildEnvironmentSHA256: d, NamespaceSHA256: d, NamespaceSchema: SerenaPackageNamespaceSchema, LeafParentSHA256: d, SourceEpoch: d, Phase: "prearmed", PublisherIncarnation: publisher, RootDispositions: []PreparedSerenaRootDispositionV1{}, CleanupErrors: []string{}, OutcomePublication: PreparedSerenaOutcomePublicationV1{Phase: "prearmed"}, PreparationPolicy: &p, InterpreterQualification: &PreparedSerenaInterpreterQualificationV1{ProbeStdout: string(raw), ClosureReceiptSHA256: d, InterpreterSHA256: d}}
	return record, op
}

func TestSerenaPolicyOperationCodecAndCurrentFloor(t *testing.T) {
	record, op := policyOperationData(t)
	raw, e := json.Marshal(op)
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := decodePreparedSerenaOperation(raw)
	if e != nil || !reflect.DeepEqual(decoded, op) {
		t.Fatal("production outer codec roundtrip", e)
	}
	if decoded.InterpreterQualification.ProbeStdout != op.InterpreterQualification.ProbeStdout || !strings.HasSuffix(decoded.InterpreterQualification.ProbeStdout, "\n") {
		t.Fatal("original ABI bytes changed")
	}
	current := DefaultSerenaPreparationPolicy()
	current.PythonMinimumVersion = "3.12"
	current.OwnedTreeLimits.Entries++
	selected, e := selectedSerenaPolicy(record, decoded, current)
	if e != nil || selected.limits != op.PreparationPolicy.OwnedTreeLimits || selected.current != current {
		t.Fatal("original context/current floor conflated", e)
	}
	crossed := record
	crossed.SourceSHA256 = preparedSerenaHash([]byte("synthetic package-source requirement"))
	if crossed.SourceSHA256 == op.SourceEpoch {
		t.Fatal("fixture failed to distinguish package-source hash and publisher epoch")
	}
	if _, e = selectedSerenaPolicy(crossed, decoded, current); e == nil {
		t.Fatal("package-source digest accepted as publisher source epoch")
	}
	current.PythonMinimumVersion = "3.14"
	if _, e = selectedSerenaPolicy(record, decoded, current); e == nil {
		t.Fatal("actual bound interpreter below current floor")
	}
	if sameSerenaOperationInputs(op, decoded) == false {
		t.Fatal("roundtrip immutable body")
	}
	changed := decoded
	policy := *decoded.PreparationPolicy
	policy.WorkLimit++
	changed.PreparationPolicy = &policy
	if sameSerenaOperationInputs(op, changed) {
		t.Fatal("phase update can replace policy")
	}
	if _, e = selectedSerenaPolicy(record, changed, DefaultSerenaPreparationPolicy()); e == nil {
		t.Fatal("policy/body hash drift accepted")
	}
	changed = decoded
	q := *decoded.InterpreterQualification
	q.ProbeStdout = strings.TrimSuffix(q.ProbeStdout, "\n")
	changed.InterpreterQualification = &q
	if _, e = selectedSerenaPolicy(record, changed, DefaultSerenaPreparationPolicy()); e == nil {
		t.Fatal("ABI original byte hash drift accepted")
	}
	changed = decoded
	q = *decoded.InterpreterQualification
	q.InterpreterSHA256 = strings.Repeat("b", 64)
	changed.InterpreterQualification = &q
	if _, e = selectedSerenaPolicy(record, changed, DefaultSerenaPreparationPolicy()); e == nil {
		t.Fatal("ABI executable drift accepted")
	}
}

func TestSerenaPolicyOperationStrictVariants(t *testing.T) {
	_, op := policyOperationData(t)
	raw, _ := json.Marshal(op)
	for _, mutate := range []func(map[string]json.RawMessage){
		func(f map[string]json.RawMessage) { delete(f, "preparation_policy") },
		func(f map[string]json.RawMessage) { f["preparation_policy"] = json.RawMessage(`null`) },
		func(f map[string]json.RawMessage) { f["interpreter_qualification"] = json.RawMessage(`{}`) },
		func(f map[string]json.RawMessage) { f["schema"] = json.RawMessage(`"serena-preparation-operation-v3"`) },
		func(f map[string]json.RawMessage) { f["schema"] = json.RawMessage(`"serena-preparation-operation-v2"`) },
		func(f map[string]json.RawMessage) { f["version"] = json.RawMessage(`3`) },
		func(f map[string]json.RawMessage) { f["unknown"] = json.RawMessage(`true`) },
		func(f map[string]json.RawMessage) {
			f["preparation_policy"] = json.RawMessage(`{"version":1,"uv_version":"0.9.21","interpreter_selector":"3.13.11"}`)
		},
	} {
		var fields map[string]json.RawMessage
		if e := json.Unmarshal(raw, &fields); e != nil {
			t.Fatal(e)
		}
		mutate(fields)
		wrong, _ := json.Marshal(fields)
		if _, e := decodePreparedSerenaOperation(wrong); e == nil {
			t.Fatal("unknown/crossed/missing body accepted", string(wrong))
		}
	}
	duplicate := append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"preparation_policy":null}`)...)
	if _, e := decodePreparedSerenaOperation(duplicate); e == nil {
		t.Fatal("duplicate policy accepted")
	}
	if _, e := decodePreparedSerenaOperation(append(raw, []byte(` {}`)...)); e == nil {
		t.Fatal("trailing body accepted")
	}
	old := op
	old.Schema = PreparedSerenaOperationSchema
	old.PreparationPolicy = nil
	old.InterpreterQualification = nil
	oldRaw, _ := json.Marshal(old)
	if bytes.Contains(oldRaw, []byte(`preparation_policy`)) || bytes.Contains(oldRaw, []byte(`interpreter_qualification`)) {
		t.Fatal("legacy outer wire changed")
	}
	if _, e := decodePreparedSerenaOperation(oldRaw); e != nil {
		t.Fatal("legacy decoder", e)
	}
}

func TestSerenaHistoricalSelectedContextAndRebindLimits(t *testing.T) {
	record, op := policyOperationData(t)
	op.Schema = PreparedSerenaOperationSchema
	op.PreparationPolicy = nil
	op.InterpreterQualification = nil
	record.PolicySHA256, _ = historicalSerenaPreparationPolicy().IdentitySHA256()
	current := DefaultSerenaPreparationPolicy()
	current.PythonMinimumVersion = "3.12"
	if _, e := selectedSerenaPolicy(record, op, current); e != nil {
		t.Fatal("historical original qualified guarantee", e)
	}
	current.PythonMinimumVersion = "3.14"
	if _, e := selectedSerenaPolicy(record, op, current); e == nil {
		t.Fatal("historical guarantee cannot prove higher floor")
	}
	record.PolicySHA256 = strings.Repeat("b", 64)
	if _, e := selectedSerenaPolicy(record, op, DefaultSerenaPreparationPolicy()); e == nil {
		t.Fatal("unknown historical context guessed")
	}
	record, op = policyOperationData(t)
	first, e := selectedSerenaPolicy(record, op, DefaultSerenaPreparationPolicy())
	if e != nil {
		t.Fatal(e)
	}
	secondPolicy := *op.PreparationPolicy
	secondPolicy.OwnedTreeLimits.Entries++
	op.PreparationPolicy = &secondPolicy
	record.PolicySHA256, _ = secondPolicy.IdentitySHA256()
	second, e := selectedSerenaPolicy(record, op, DefaultSerenaPreparationPolicy())
	if e != nil || first.limits == second.limits || second.limits != secondPolicy.OwnedTreeLimits {
		t.Fatal("selected limits retained first owner", e)
	}
	if _, e = receiveSerenaSelectedContext(context.Background(), record, op, nil, nil, nil); e == nil {
		t.Fatal("both missing contexts admitted")
	}
	// Historical shape/hash receiving is independent of the current live
	// floor. These passive data cannot issue the missing native publication.
	original := DefaultSerenaPreparationPolicy()
	original.UVMinimumVersion = "0.13.3"
	original.PythonMinimumVersion = "3.10"
	if _, e = ResolveSerenaPreparationPolicy(&original); e == nil {
		t.Fatal("historical data weakened live support")
	}
	op.PreparationPolicy = &original
	record.PolicySHA256, e = original.canonicalIdentitySHA256()
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(op)
	if e != nil {
		t.Fatal(e)
	}
	decoded, e := decodePreparedSerenaOperation(raw)
	if e != nil {
		t.Fatal("current factory reinterpreted historical body", e)
	}
	current = DefaultSerenaPreparationPolicy()
	current.PythonMinimumVersion = "3.12"
	if _, e = selectedSerenaPolicy(record, decoded, current); e != nil {
		t.Fatal("sufficient authenticated tuple rejected for old body floor", e)
	}
	current.PythonMinimumVersion = "3.14"
	if _, e = selectedSerenaPolicy(record, decoded, current); e == nil {
		t.Fatal("old body minimum substituted for actual current support")
	}
}

func TestSerenaInterpreterABIEvidence(t *testing.T) {
	valid := interpreterABIData(t, "4.0.0", []int{4, 0, 0})
	if _, e := decodeSerenaInterpreterABI(valid); e != nil {
		t.Fatal("higher numeric ABI", e)
	}
	for _, wrong := range [][]byte{
		nil, []byte(`{}`), bytes.Replace(valid, []byte(`"version_info":[4,0,0]`), []byte(`"version_info":null`), 1),
		bytes.Replace(valid, []byte(`"version_info":[4,0,0]`), []byte(`"version_info":[3,13,11]`), 1),
		bytes.Replace(valid, []byte(`"version_info":[4,0,0]`), []byte(`"version_info":[4,0,0.0]`), 1),
	} {
		if _, e := decodeSerenaInterpreterABI(wrong); e == nil {
			t.Fatal("missing/malformed/contradictory ABI admitted", string(wrong))
		}
	}
}

func TestSerenaInterpreterABINonnullableComponents(t *testing.T) {
	for _, sample := range []struct {
		name, version, tuple string
	}{
		{"future patch null", "4.0.0", `[4,0,null]`},
		{"future minor null", "4.0.0", `[4,null,0]`},
		{"current minimum patch null", "3.11.0", `[3,11,null]`},
		{"major null", "4.0.0", `[null,0,0]`},
		{"string component", "4.0.0", `[4,0,"0"]`},
		{"boolean component", "4.0.0", `[4,0,false]`},
		{"fraction component", "4.0.0", `[4,0,0.0]`},
		{"object component", "4.0.0", `[4,0,{}]`},
		{"array component", "4.0.0", `[4,0,[]]`},
	} {
		t.Run(sample.name, func(t *testing.T) {
			raw := interpreterABIData(t, sample.version, []int{4, 0, 0})
			raw = bytes.Replace(raw, []byte(`"version_info":[4,0,0]`), []byte(`"version_info":`+sample.tuple), 1)
			t.Run("direct", func(t *testing.T) {
				if _, err := decodeSerenaInterpreterABI(raw); err == nil {
					t.Fatal("malformed numeric component admitted")
				}
			})
			record, operation := policyOperationData(t)
			qualification := *operation.InterpreterQualification
			qualification.ProbeStdout = string(raw)
			operation.InterpreterQualification = &qualification
			// Bind the actual malformed bytes so a digest mismatch cannot mask
			// whether the one shared ABI receiver checks the component kind.
			record.PlatformABISHA256 = preparedSerenaHash(raw)
			t.Run("operation", func(t *testing.T) {
				encoded, err := json.Marshal(operation)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = decodePreparedSerenaOperation(encoded); err == nil {
					t.Fatal("operation admitted malformed numeric component")
				}
			})
			t.Run("selected", func(t *testing.T) {
				if _, err := selectedSerenaPolicy(record, operation, DefaultSerenaPreparationPolicy()); err == nil {
					t.Fatal("selected context admitted malformed numeric component")
				}
			})
		})
	}
}

func TestSerenaPriorOperationReadOutcomeRetention(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultSerenaPreparationPolicy().WorkLimit)
	defer cancel()
	path := filepath.Join(hardenedTempDir(t), "prior-operation-fixture.json")
	contents := []byte(`{"synthetic":"original read outcome"}`)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	finish := func(t *testing.T, reader *StateFileReadObservation) *PreparedSerenaPreparationCloseObservation {
		t.Helper()
		request := preparedRequest()
		deadline, _ := ctx.Deadline()
		request.AbsoluteCleanupDeadline = deadline.Add(DefaultSerenaPreparationPolicy().CleanupLimit)
		// This empty publication collector issues no publisher/source/native
		// token. Only the actual reader's existing finalization is exercised.
		effects := &preparedSerenaRuntimeEffects{
			acquisition:           preparedSerenaAcquisition{inputs: SerenaPreparationInputs{Request: request}},
			publication:           NewPreparedSerenaPublicationOwner(),
			priorOperationReaders: []*StateFileReadObservation{reader},
		}
		observation, err := effects.FinishPreparation(ctx, request, nil)
		if observation == nil || observation.owner != effects || len(observation.owner.priorOperationReaders) != 1 || observation.owner.priorOperationReaders[0] != reader {
			t.Fatal("finalization dropped the original read observation")
		}
		if reader == nil && (err == nil || observation.Validate(ctx, effects, request) == nil) {
			t.Fatal("unknown read outcome became successful closure")
		}
		return observation
	}
	t.Run("original read failure", func(t *testing.T) {
		cause := errors.New("controlled original leaf capture failure")
		captures := 0
		raw, reader, err := readStateFileInodeAnchoredObservedWithLeafCapture(ctx, path, 64<<20, func(_ uintptr, _ []byte) error {
			captures++
			return cause
		})
		if raw != nil || reader == nil || !errors.Is(err, cause) || captures != 1 {
			t.Fatal("original read fault seam unavailable", err)
		}
		before := reader.Snapshot()
		if before.Complete || before.PrimaryError == nil {
			t.Fatal("read failure was erased before receiving")
		}
		observation := finish(t, reader)
		if !reflect.DeepEqual(before, observation.owner.priorOperationReaders[0].Snapshot()) || captures != 1 || !errors.Is(err, cause) {
			t.Fatal("original cause/resource outcomes changed or were replayed")
		}
	})
	t.Run("original validation failure", func(t *testing.T) {
		raw, reader, err := ReadStateFileInodeAnchoredObserved(ctx, path, 64<<20)
		if err != nil || reader == nil {
			t.Fatal(err)
		}
		validationErr := reader.Validate(ctx, path, 64<<20, raw, preparedSerenaHash([]byte("different bytes")))
		if validationErr == nil {
			t.Fatal("controlled validation fault was not reached")
		}
		before := reader.Snapshot()
		observation := finish(t, reader)
		if !reflect.DeepEqual(before, observation.owner.priorOperationReaders[0].Snapshot()) {
			t.Fatal("validation failure replaced original resource outcomes")
		}
	})
	t.Run("missing original outcome", func(t *testing.T) {
		finish(t, nil)
	})
}

func TestSerenaNetworkProjectionOriginalContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultSerenaPreparationPolicy().WorkLimit)
	defer cancel()
	bootstrap := map[string]string{}
	for _, k := range SerenaBuildBootstrapDeclarationKeys() {
		v, ok := os.LookupEnv(k)
		if !ok || v == "" {
			t.Fatal("required bootstrap input unavailable", k)
		}
		bootstrap[k] = v
	}
	// This projection fixture declares one real command-directory input. It
	// does not modify the process PATH or claim normal Product discovery.
	bootstrap["PATH"] = filepath.Dir(bootstrap["COMSPEC"])
	manifest := &config.ServerManifest{Name: SerenaServerName, BaseArgs: []string{"--from", "git+https://github.com/oraios/serena@949a27ef1e5fda1a6e7b561e777bcece345c6ffd", "serena"}}
	var hashes []string
	for _, value := range []string{"", "false", "true"} {
		declarations := map[string]string{}
		if value != "" {
			declarations["UV_OFFLINE"] = value
		}
		source, e := NewSerenaPreparationEnvironmentSource(ctx, manifest, secrets.NewResolver(nil, nil), declarations, nil, bootstrap)
		if e != nil {
			t.Fatal(e)
		}
		projection, execution, e := BuildSerenaBuildEnvironmentProjection(ctx, source, DefaultSerenaPreparationPolicy())
		if e != nil {
			t.Fatal(e)
		}
		sha, e := projection.SHA256()
		if e != nil {
			t.Fatal(e)
		}
		hashes = append(hashes, sha)
		if execution.networkPolicy.offline != (value == "true") {
			t.Fatal("declared execution changed")
		}
		historical := historicalSerenaPreparationPolicy()
		oldSHA, _ := historical.IdentitySHA256()
		old, _, e := buildSerenaBuildEnvironmentProjection(ctx, source, oldSHA, "uv/0.9.21:package-network-default:offline=false")
		if e != nil {
			t.Fatal(e)
		}
		oldDigest, _ := old.SHA256()
		if oldDigest == sha {
			t.Fatal("old/current provenance collapsed")
		}
	}
	if hashes[0] == hashes[1] || hashes[1] == hashes[2] || hashes[0] == hashes[2] {
		t.Fatal("original declarations collapsed")
	}
}

func TestSerenaOriginalInputCommandOutcomes(t *testing.T) {
	p := DefaultSerenaPreparationPolicy()
	ctx, cancel := context.WithTimeout(context.Background(), p.WorkLimit)
	defer cancel()
	path := os.Getenv("COMSPEC")
	if !filepath.IsAbs(path) {
		t.Fatal("inert command fixture unavailable")
	}
	executable, _, e := qualifyPreparedSerenaFile(ctx, path, 64<<20)
	if e != nil {
		t.Fatal(e)
	}
	policySHA, _ := p.IdentitySHA256()
	end, _ := ctx.Deadline()
	r := preparedSerenaInvocation{OperationID: "policy-probe-fixture", Executable: executable, Environment: os.Environ(), WorkingDir: t.TempDir(), PolicySHA256: policySHA, CleanupLimit: p.CleanupLimit, AbsoluteCleanupDeadline: end.Add(p.CleanupLimit), StdoutLimit: 1 << 20, StderrLimit: 1 << 20}
	r.Args = []string{"/d", "/c", "echo uv 0.13.6"}
	v, e := runPreparedSerenaInvocation(ctx, r)
	if e = validateSerenaUVProbe(v, r.OperationID, p, e); e != nil {
		t.Fatal("original command/native/IO fixture", e)
	}
	r.Args = []string{"/d", "/c", "echo uv 0.13.6 & exit /b 7"}
	failed, e := runPreparedSerenaInvocation(ctx, r)
	if failed.Closure.RootExitCode != 7 || !failed.Closure.complete(r.OperationID) || len(failed.Stdout) == 0 || validateSerenaUVProbe(failed, r.OperationID, p, e) == nil {
		t.Fatal("nonzero partial output lost", e)
	}
	original := errors.New("late close cause")
	if e = validateSerenaUVProbe(v, r.OperationID, p, original); !errors.Is(e, original) {
		t.Fatal("late error suppressed", e)
	}
}

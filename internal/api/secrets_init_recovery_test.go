package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func recoveryFixture(t *testing.T) (*secretsInitRecoveryOwner, recoveryCapture) {
	t.Helper()
	key, vault := secretsTestEnv(t)
	guardSecretsInitFixture(t)
	root := t.TempDir()
	o := &secretsInitRecoveryOwner{root: root, write: SecureWriteClientConfig}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	c, err := captureSecretsInitRecoveryTargets(key, vault, cwd)
	if err != nil {
		t.Fatal(err)
	}
	return o, c
}
func recoveryLeaf(o *secretsInitRecoveryOwner, id, phase string) string {
	return filepath.Join(o.root, "secrets-init-recovery."+id+"."+phase+".json")
}
func recoveryProof(t *testing.T, name string, raw []byte) {
	t.Helper()
	root := os.Getenv("MCPHUB_RECOVERY_PROOF_DIR")
	if root == "" {
		return
	}
	if !filepath.IsAbs(root) {
		t.Fatal("invalid proof root")
	}
	if err := os.WriteFile(filepath.Join(root, name), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestSecretsInit_RecoveryStages(t *testing.T) {
	o, c := recoveryFixture(t)
	a, err := o.Reserve(c)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := os.ReadFile(recoveryLeaf(o, a.stage.record.AttemptID, "prepared"))
	if err != nil {
		t.Fatal(err)
	}
	recoveryProof(t, "prepared.sample.json", prepared)
	report, err := o.read(a.stage.record.AttemptID)
	if err != nil || report.State != "prepared" || report.Key != c.key || report.Vault != c.vault {
		t.Fatalf("pending report: %+v %v", report, err)
	}
	cause := errors.New("synthetic cleanup failure")
	targets := &secretsInitCleanupTargets{Vault: secretsInitTargetOutcome{c.vault.AbsolutePath, secretsInitRemoveFailed, cause}, Key: secretsInitTargetOutcome{c.key.AbsolutePath, secretsInitAbsent, os.ErrNotExist}}
	ref, err := o.Complete(a, "init_failed", targets)
	if err != nil || ref.Status != "ready" {
		t.Fatalf("complete: %+v %v", ref, err)
	}
	complete, err := os.ReadFile(recoveryLeaf(o, a.stage.record.AttemptID, "complete"))
	if err != nil {
		t.Fatal(err)
	}
	recoveryProof(t, "complete.sample.json", complete)
	if _, err = o.Complete(a, "init_failed", targets); err == nil {
		t.Fatal("replay accepted")
	}
	if _, err = o.retire(a); err == nil {
		t.Fatal("conflicting terminal accepted")
	}
	newer := &secretsInitRecoveryOwner{root: o.root, write: SecureWriteClientConfig}
	report, err = newer.read(ref.ID)
	if err != nil || report.State != "init_failed" || report.Vault.RemoveErrorClass != "other" || report.Key.RemoveErrorClass != "not_exist" {
		t.Fatalf("restart read: %+v %v", report, err)
	}
	b, err := o.Reserve(c)
	if err != nil {
		t.Fatal(err)
	}
	ret, err := o.retire(b)
	if err != nil || ret.Status != "ready" {
		t.Fatalf("retire: %v", err)
	}
	raw, err := os.ReadFile(recoveryLeaf(o, ret.ID, "retired"))
	if err != nil {
		t.Fatal(err)
	}
	recoveryProof(t, "retired.sample.json", raw)
	retiredPrepared, err := os.ReadFile(recoveryLeaf(o, ret.ID, "prepared"))
	if err != nil {
		t.Fatal(err)
	}
	recoveryProof(t, "retired.prepared.sample.json", retiredPrepared)
	after, _ := os.ReadFile(recoveryLeaf(o, ref.ID, "prepared"))
	if !bytes.Equal(after, prepared) {
		t.Fatal("older prepared changed")
	}
}
func TestSecretsInit_RecoveryRetentionFaults(t *testing.T) {
	for _, phase := range []string{"prepared", "complete", "retired"} {
		t.Run(phase, func(t *testing.T) {
			o, c := recoveryFixture(t)
			a, err := o.Reserve(c)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(recoveryLeaf(o, a.stage.record.AttemptID, "prepared"))
			fault := errors.New("synthetic uncertain publication")
			o.write = func(p string, raw []byte) error {
				if strings.Contains(p, "."+phase+".json") {
					if err := SecureWriteClientConfig(p, raw); err != nil {
						return err
					}
					return fault
				}
				return SecureWriteClientConfig(p, raw)
			}
			b, err := o.Reserve(c)
			if phase == "prepared" {
				if !errors.Is(err, fault) || b != nil {
					t.Fatalf("uncertain reserve admitted: %v %v", b, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if phase == "complete" {
					_, err = o.Complete(b, "init_refused", nil)
				} else {
					_, err = o.retire(b)
				}
				if !errors.Is(err, fault) {
					t.Fatalf("terminal uncertainty: %v", err)
				}
				if _, err = os.ReadFile(recoveryLeaf(o, b.stage.record.AttemptID, "prepared")); err != nil {
					t.Fatal("prepared B lost")
				}
			}
			after, _ := os.ReadFile(recoveryLeaf(o, a.stage.record.AttemptID, "prepared"))
			if !bytes.Equal(before, after) {
				t.Fatal("A lost")
			}
			if _, err = o.read(a.stage.record.AttemptID); err != nil {
				t.Fatal("independent A failed", err)
			}
		})
	}
}
func TestSecretsInit_RecoveryCodec(t *testing.T) {
	o, c := recoveryFixture(t)
	c.key.SelectedPath = "unicode \uFFFD quotes \" / \\ \n"
	r, err := o.Reserve(c)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(recoveryLeaf(o, r.stage.record.AttemptID, "prepared"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := decodeRecoveryStage(raw)
	if err != nil || s.record.Key.SelectedPath != c.key.SelectedPath {
		t.Fatalf("lossless: %v", err)
	}
	for _, bad := range []string{strings.Replace(string(raw), `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1), strings.Replace(string(raw), `"phase"`, `"Phase"`, 1), strings.Replace(string(raw), `"schema_version":1`, `"schema_version":1.0`, 1), strings.Replace(string(raw), `"key":{`, `"key":null,"unused":{`, 1), string(raw) + `{}`, "{}", "[]", strings.Replace(string(raw), `"phase":"prepared"`, `"phase":"unknown"`, 1), strings.Replace(string(raw), `"outcome":"not_attempted"`, `"outcome":"removed"`, 1), strings.Replace(string(raw), `unicode �`, `unicode \ud800`, 1), " " + string(raw)} {
		if _, err := decodeRecoveryStage([]byte(bad)); err == nil {
			t.Errorf("invalid codec accepted: %.90s", bad)
		}
	}
	c.key.SelectedPath = string([]byte{0xff})
	if err = validateRecoveryCapture(c); err == nil {
		t.Fatal("invalid UTF-8 admitted")
	}
}
func TestSecretsInit_RecoveryProducer(t *testing.T) {
	key, vault := secretsTestEnv(t)
	guardSecretsInitFixture(t)
	prior := initVaultFn
	t.Cleanup(func() { initVaultFn = prior })
	cause := errors.New("synthetic producer cause")
	initVaultFn = func(k, v string) error {
		if k != key || v != vault {
			t.Fatal("capture changed")
		}
		return cause
	}
	var calls []string
	_, err := (&API{}).secretsInitWithRemove(func(p string) error { calls = append(calls, p); return os.ErrNotExist })
	var failed *SecretsInitFailed
	if !errors.As(err, &failed) || !errors.Is(err, cause) || failed.Cause != cause {
		t.Fatalf("cause identity %v", err)
	}
	if len(calls) != 2 || calls[0] != vault || calls[1] != key {
		t.Fatal("cleanup order/count")
	}
	ref := failed.RecoveryReference()
	if ref.ID == "" || ref.Status != "ready" {
		t.Fatalf("missing durable reference: %+v", ref)
	}
}

func TestSecretsInit_RecoveryProducerPersistenceFailures(t *testing.T) {
	for _, phase := range []string{"prepared", "complete", "retired"} {
		t.Run(phase, func(t *testing.T) {
			o, c := recoveryFixture(t)
			guardSecretsInitFixture(t)
			prior := initVaultFn
			t.Cleanup(func() { initVaultFn = prior })
			cause := errors.New("synthetic exact init cause")
			fault := errors.New("synthetic storage failure")
			initCalls, removeCalls := 0, 0
			initVaultFn = func(k, v string) error {
				initCalls++
				if k != c.key.AbsolutePath || v != c.vault.AbsolutePath {
					t.Fatal("wrong capture")
				}
				if phase == "retired" {
					if err := os.WriteFile(k, []byte("SYNTHETIC-KEY"), 0600); err != nil {
						return err
					}
					if err := os.WriteFile(v, []byte("SYNTHETIC-VAULT"), 0600); err != nil {
						return err
					}
					return nil
				}
				return cause
			}
			o.write = func(p string, raw []byte) error {
				if strings.Contains(p, "."+phase+".json") {
					return fault
				}
				return SecureWriteClientConfig(p, raw)
			}
			_, err := (&API{}).secretsInitWithRecovery(func(p string) error { removeCalls++; return os.ErrNotExist }, func() (*secretsInitRecoveryOwner, error) { return o, nil })
			var persist *SecretsRecoveryPersistenceError
			if !errors.As(err, &persist) || !errors.Is(persist.PersistenceCause, fault) {
				t.Fatalf("failure hidden: %v", err)
			}
			if phase == "prepared" {
				if initCalls != 0 || removeCalls != 0 || persist.RecoveryReference().Status != "unavailable" {
					t.Fatal("unverified reserve admitted work")
				}
			} else {
				if initCalls != 1 || persist.RecoveryReference().Status != "incomplete" {
					t.Fatal("terminal failure lost")
				}
				if phase == "complete" {
					var failed *SecretsInitFailed
					if removeCalls != 2 || !errors.As(err, &failed) || failed.Cause != cause || !errors.Is(err, cause) {
						t.Fatal("original cause or cleanup lost")
					}
				} else {
					if removeCalls != 0 {
						t.Fatal("successful targets rolled back")
					}
					for _, p := range []string{c.key.AbsolutePath, c.vault.AbsolutePath} {
						if _, e := os.ReadFile(p); e != nil {
							t.Fatal("created target lost")
						}
					}
				}
			}
		})
	}
}

func TestSecretsInit_RecoveryStrictWriterFaults(t *testing.T) {
	for _, phase := range []string{"prepared", "complete", "retired"} {
		for _, kind := range []string{"definitive", "interrupted"} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				o, c := recoveryFixture(t)
				a, err := o.Reserve(c)
				if err != nil {
					t.Fatal(err)
				}
				before, _ := os.ReadFile(recoveryLeaf(o, a.stage.record.AttemptID, "prepared"))
				fault := errors.New("synthetic protected writer fault")
				priorVerify, priorOpen := postRenameVerifyFailHook, postRenameOpenFailHook
				t.Cleanup(func() { postRenameVerifyFailHook = priorVerify; postRenameOpenFailHook = priorOpen })
				o.write = func(path string, raw []byte) error {
					if strings.Contains(path, "."+phase+".json") {
						if kind == "definitive" {
							postRenameVerifyFailHook = func() error { return fault }
						} else {
							postRenameOpenFailHook = func() error { return fault }
						}
						defer func() { postRenameVerifyFailHook = priorVerify; postRenameOpenFailHook = priorOpen }()
					}
					return SecureWriteClientConfig(path, raw)
				}
				b, err := o.Reserve(c)
				if phase == "prepared" {
					if b != nil || !errors.Is(err, fault) {
						t.Fatalf("bad reserve: %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if phase == "complete" {
						_, err = o.Complete(b, "init_refused", nil)
					} else {
						_, err = o.retire(b)
					}
					if !errors.Is(err, fault) {
						t.Fatalf("fault hidden: %v", err)
					}
					if _, err = os.ReadFile(recoveryLeaf(o, b.stage.record.AttemptID, "prepared")); err != nil {
						t.Fatal("prepared B missing")
					}
				}
				after, _ := os.ReadFile(recoveryLeaf(o, a.stage.record.AttemptID, "prepared"))
				if !bytes.Equal(before, after) {
					t.Fatal("pending A lost")
				}
				if _, err = o.read(a.stage.record.AttemptID); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSecretsInit_RecoveryReadbackAndIndependentInspect(t *testing.T) {
	o, c := recoveryFixture(t)
	a, err := o.Reserve(c)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(recoveryLeaf(o, a.stage.record.AttemptID, "prepared"))
	o.write = func(p string, raw []byte) error { return SecureWriteClientConfig(p, []byte("[]")) }
	if b, err := o.Reserve(c); err == nil || b != nil {
		t.Fatal("corrupt protected readback admitted")
	}
	if _, err = o.candidateIDs(); err != nil {
		t.Fatal(err)
	}
	if _, err = o.read(a.stage.record.AttemptID); err != nil {
		t.Fatal("corrupt B blocked A", err)
	}
	if b, err := o.Reserve(c); err == nil || b != nil {
		t.Fatal("corrupt namespace accepted fresh attempt")
	}
	after, _ := os.ReadFile(recoveryLeaf(o, a.stage.record.AttemptID, "prepared"))
	if !bytes.Equal(before, after) {
		t.Fatal("A changed")
	}
	o.write = SecureWriteClientConfig
	record := a.stage.record
	record.Key.SelectedPath = "changed capture"
	raw, err := encodeRecoveryStage(recoveryStage{phase: "prepared", record: record})
	if err != nil {
		t.Fatal(err)
	}
	if err = SecureWriteClientConfig(recoveryLeaf(o, record.AttemptID, "prepared"), raw); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Complete(a, "init_refused", nil); err == nil {
		t.Fatal("stale captured reservation accepted")
	}
}

func TestSecretsInit_RecoveryCapacity(t *testing.T) {
	o, c := recoveryFixture(t)
	c.key.SelectedPath = strings.Repeat("x", 180000)
	a, err := o.Reserve(c)
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := recoveryAllocation(a.prepared)
	if err != nil {
		t.Fatal(err)
	}
	maxActual := 0
	for _, k := range []struct{ outcome, class string }{{"removed", "none"}, {"absent", "not_exist"}, {"remove_failed", "other"}} {
		for _, v := range []struct{ outcome, class string }{{"removed", "none"}, {"absent", "not_exist"}, {"remove_failed", "other"}} {
			r := a.stage.record
			r.State = "init_failed"
			r.Key.Outcome = k.outcome
			r.Key.RemoveErrorClass = k.class
			r.Vault.Outcome = v.outcome
			r.Vault.RemoveErrorClass = v.class
			raw, err := encodeRecoveryStage(recoveryStage{phase: "complete", record: r, digest: recoveryDigest(a.prepared)})
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) > maxActual {
				maxActual = len(raw)
			}
			if len(a.prepared)+len(raw) > allocation {
				t.Fatal("terminal allocation underestimated")
			}
		}
	}
	if maxActual == 0 {
		t.Fatal("finite variants not measured")
	}
	if _, err = o.retire(a); err != nil {
		t.Fatal(err)
	}
	b, err := o.Reserve(c)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(recoveryLeaf(o, b.stage.record.AttemptID, "prepared"))
	if _, err = o.Reserve(c); err == nil {
		t.Fatal("aggregate capacity/retired allocation bypassed")
	}
	after, _ := os.ReadFile(recoveryLeaf(o, b.stage.record.AttemptID, "prepared"))
	if !bytes.Equal(before, after) {
		t.Fatal("capacity evicted evidence")
	}
	if _, err = decodeRecoveryStage(make([]byte, maxStateFileBytes+1)); err == nil {
		t.Fatal("oversized read accepted")
	}
}

func TestSecretsInit_RecoveryReadOnlyReport(t *testing.T) {
	o, c := recoveryFixture(t)
	a, err := o.Reserve(c)
	if err != nil {
		t.Fatal(err)
	}
	prior := daemonStateRootOverride
	daemonStateRootOverride = o.root
	t.Cleanup(func() { daemonStateRootOverride = prior })
	before, err := os.ReadDir(o.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{c.key.AbsolutePath, c.vault.AbsolutePath} {
		if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(p, []byte("SYNTHETIC-REPLACED-TARGET"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	report, err := (&API{}).ReadSecretsInitRecovery(a.stage.record.AttemptID)
	if err != nil || report.Authority != "historical_attempt_only" || report.CurrentState != "not_inspected" || report.Key != c.key {
		t.Fatalf("report: %+v %v", report, err)
	}
	// The report exposes only the controlled operator projection, not capture CWD.
	reportBytes, err := json.Marshal(report)
	if err != nil || bytes.Contains(reportBytes, []byte(`"resolution_cwd"`)) {
		t.Fatal("private capture context escaped report")
	}
	summaries, err := (&API{}).ListSecretsInitRecovery()
	if err != nil || len(summaries) != 1 || summaries[0].State != "prepared" {
		t.Fatalf("list: %+v %v", summaries, err)
	}
	after, err := os.ReadDir(o.root)
	if err != nil || len(before) != len(after) {
		t.Fatal("reader mutated namespace")
	}
	for _, p := range []string{c.key.AbsolutePath, c.vault.AbsolutePath} {
		raw, e := os.ReadFile(p)
		if e != nil || string(raw) != "SYNTHETIC-REPLACED-TARGET" {
			t.Fatal("reader acted on target")
		}
	}
}

func TestSecretsInit_RecoveryConcurrentTerminalAndConflict(t *testing.T) {
	o, c := recoveryFixture(t)
	a, err := o.Reserve(c)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() { defer workers.Done(); <-start; _, err := o.Complete(a, "init_refused", nil); results <- err }()
	go func() { defer workers.Done(); <-start; _, err := o.retire(a); results <- err }()
	close(start)
	workers.Wait()
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("terminal exclusivity: %v / %v", first, second)
	}
	if _, err = o.read(a.stage.record.AttemptID); err != nil {
		t.Fatal(err)
	}
	// A same-owner corrupted namespace with both canonical terminals is refused.
	complete := a.stage.record
	complete.State = "init_refused"
	for _, s := range []recoveryStage{{phase: "complete", record: complete, digest: recoveryDigest(a.prepared)}, {phase: "retired", id: complete.AttemptID, digest: recoveryDigest(a.prepared), reason: "init_succeeded"}} {
		raw, err := encodeRecoveryStage(s)
		if err != nil {
			t.Fatal(err)
		}
		if err = SecureWriteClientConfig(recoveryLeaf(o, complete.AttemptID, s.phase), raw); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = o.read(a.stage.record.AttemptID); err == nil {
		t.Fatal("both terminals accepted")
	}
	if _, err = o.Reserve(c); err == nil {
		t.Fatal("conflicting namespace admitted fresh attempt")
	}
}

func TestSecretsInit_RecoveryCaptureRefusal(t *testing.T) {
	o, c := recoveryFixture(t)
	writes := 0
	o.write = func(string, []byte) error { writes++; return nil }
	for _, mutation := range []func(*recoveryCapture){
		func(c *recoveryCapture) { c.key.SelectedPath = "" },
		func(c *recoveryCapture) { c.vault.SelectedPath = string([]byte{0xff}) },
		func(c *recoveryCapture) { c.cwd = "relative" },
		func(c *recoveryCapture) { c.key.AbsolutePath = "relative" },
		func(c *recoveryCapture) { c.vault.AbsolutePath = c.key.AbsolutePath },
	} {
		bad := c
		mutation(&bad)
		if _, err := o.Reserve(bad); err == nil {
			t.Fatal("invalid capture admitted")
		}
	}
	if writes != 0 {
		t.Fatal("invalid capture reached writer")
	}
	if _, err := captureSecretsInitRecoveryTargets("same", "same", c.cwd); err == nil {
		t.Fatal("equal default pair admitted")
	}
	entries, err := os.ReadDir(o.root)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid capture created namespace state")
	}
}

func recoveryNamespaceFixtureCommand(t *testing.T, action, path string, extra ...string) []byte {
	t.Helper()
	helper := os.Getenv("MCPHUB_RECOVERY_NAMESPACE_FIXTURE_HELPER")
	program := os.Getenv("MCPHUB_RECOVERY_NAMESPACE_PWSH")
	root := os.Getenv("MCPHUB_RECOVERY_NAMESPACE_ROOT")
	if !filepath.IsAbs(helper) || !filepath.IsAbs(program) || !filepath.IsAbs(root) {
		t.Fatal("private namespace fixture binding missing")
	}
	rel, err := filepath.Rel(root, helper)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// The fixed helper is a sibling of correction/, bound by its expected digest.
		if filepath.Dir(helper) != filepath.Dir(root) {
			t.Fatal("private fixture helper escaped its creator namespace")
		}
	}
	info, err := os.Lstat(helper)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("private fixture helper is not ordinary")
	}
	raw, err := os.ReadFile(helper)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != os.Getenv("MCPHUB_RECOVERY_NAMESPACE_FIXTURE_SHA256") {
		t.Fatal("private fixture helper digest changed")
	}
	args := []string{"-NoProfile", "-NonInteractive", "-File", helper, "-Action", action}
	if path != "" {
		args = append(args, "-Path", path)
	}
	args = append(args, extra...)
	output, err := exec.Command(program, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("private namespace fixture %s failed: %v %s", action, err, output)
	}
	return bytes.TrimSpace(output)
}

func TestSecretsInit_RecoveryNamespaceBoundary(t *testing.T) {
	prior := daemonStateRootOverride
	t.Cleanup(func() { daemonStateRootOverride = prior })
	for _, name := range []string{"protected_empty", "missing", "not_directory", "broadened_empty", "broadened_ignored", "reparse"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "namespace")
			if name == "not_directory" {
				if err := os.WriteFile(root, []byte("SYNTHETIC-NON-DIRECTORY"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if name != "missing" && name != "reparse" {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if name == "broadened_ignored" {
				if err := os.WriteFile(filepath.Join(root, "unrelated.marker"), []byte("SYNTHETIC-IGNORED"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(name, "broadened") {
				if runtime.GOOS == "windows" {
					recoveryNamespaceFixtureCommand(t, "Broaden", root)
				} else if err := os.Chmod(root, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if name == "reparse" {
				target := filepath.Join(base, "target")
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				if runtime.GOOS == "windows" {
					recoveryNamespaceFixtureCommand(t, "Junction", root, "-Target", target)
					t.Cleanup(func() { recoveryNamespaceFixtureCommand(t, "RemoveJunction", root, "-Target", target) })
				} else {
					if err := os.Symlink(target, root); err != nil {
						t.Fatal(err)
					}
				}
			}
			daemonStateRootOverride = root
			var before []byte
			if runtime.GOOS == "windows" {
				before = recoveryNamespaceFixtureCommand(t, "Snapshot", root)
			}
			entries, err := (&API{}).ListSecretsInitRecovery()
			if name == "protected_empty" {
				if err != nil || entries == nil || len(entries) != 0 {
					t.Fatalf("protected empty root refused: %+v %v", entries, err)
				}
			} else if err == nil {
				t.Fatalf("unsupported %s returned successful empty evidence: %+v", name, entries)
			}
			if runtime.GOOS == "windows" {
				after := recoveryNamespaceFixtureCommand(t, "Snapshot", root)
				if !bytes.Equal(before, after) {
					t.Fatal("list changed namespace bytes/protection/entries")
				}
			}
			if name == "missing" {
				if _, err := os.Lstat(root); !os.IsNotExist(err) {
					t.Fatal("missing namespace was created")
				}
			}
		})
	}
	if runtime.GOOS == "windows" {
		root := filepath.Join(os.Getenv("MCPHUB_RECOVERY_NAMESPACE_ROOT"), "temp", "wrong-owner-fixture")
		proof := filepath.Join(os.Getenv("MCPHUB_RECOVERY_NAMESPACE_ROOT"), "proof", "wrong-owner-attempt.json")
		if _, err := os.Stat(proof); os.IsNotExist(err) {
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			recoveryNamespaceFixtureCommand(t, "WrongOwner", root)
		}
		raw, err := os.ReadFile(proof)
		if err != nil {
			t.Fatal(err)
		}
		var attempt struct {
			Assigned bool
			Failure  string
		}
		if err := json.Unmarshal(raw, &attempt); err != nil {
			t.Fatal(err)
		}
		if attempt.Assigned {
			t.Run("wrong_owner", func(t *testing.T) {
				daemonStateRootOverride = root
				before := recoveryNamespaceFixtureCommand(t, "Snapshot", root)
				if entries, err := (&API{}).ListSecretsInitRecovery(); err == nil {
					t.Fatalf("wrong owner accepted: %+v", entries)
				}
				if !bytes.Equal(before, recoveryNamespaceFixtureCommand(t, "Snapshot", root)) {
					t.Fatal("wrong-owner read changed descriptor/state")
				}
			})
		} else {
			t.Logf("UNVERIFIED actual wrong-owner receiving: single private assignment refused: %s", attempt.Failure)
		}
	}
}

func recoveryNamespaceHandleCount(t *testing.T) int {
	t.Helper()
	raw := recoveryNamespaceFixtureCommand(t, "Handles", "", "-ActorPid", strconv.Itoa(os.Getpid()))
	count, err := strconv.Atoi(string(raw))
	if err != nil || count <= 0 {
		t.Fatal("invalid actor handle count", string(raw), err)
	}
	return count
}

func TestSecretsInit_RecoveryNamespaceRetainedDirectory(t *testing.T) {
	base := t.TempDir()
	root, moved := filepath.Join(base, "namespace"), filepath.Join(base, "validated-object")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "original.marker"), []byte("SYNTHETIC-ORIGINAL"), 0600); err != nil {
		t.Fatal(err)
	}
	beforeHandles := 0
	if runtime.GOOS == "windows" {
		beforeHandles = recoveryNamespaceHandleCount(t)
	}
	callbacks := 0
	entries, err := readSecretsInitRecoveryDirectoryAfterValidation(root, func(file *os.File) error {
		callbacks++
		if file == nil {
			return errors.New("validated file missing")
		}
		if err := os.Rename(root, moved); err != nil {
			return err
		}
		if err := os.Mkdir(root, 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, "replacement.marker"), []byte("SYNTHETIC-REPLACEMENT"), 0600); err != nil {
			return err
		}
		if runtime.GOOS == "windows" {
			recoveryNamespaceFixtureCommand(t, "Broaden", root)
		} else if err := os.Chmod(root, 0755); err != nil {
			return err
		}
		return nil
	})
	if err != nil || callbacks != 1 || len(entries) != 1 || entries[0].Name() != "original.marker" {
		t.Fatalf("enumeration did not retain validated object: %+v callbacks=%d err=%v", entries, callbacks, err)
	}
	if entries, err := readSecretsInitRecoveryDirectory(root); err == nil || entries != nil {
		t.Fatalf("replacement pathname became protected evidence: %+v %v", entries, err)
	}
	if runtime.GOOS == "windows" {
		afterHandles := recoveryNamespaceHandleCount(t)
		if beforeHandles != afterHandles {
			t.Fatalf("directory handle residue before=%d after=%d", beforeHandles, afterHandles)
		}
		t.Logf("actor handles before=%d after=%d; retained object returned original.marker", beforeHandles, afterHandles)
	}
}

func TestSecretsInit_RecoveryNamespaceFailurePaths(t *testing.T) {
	root := t.TempDir()
	beforeHandles := 0
	if runtime.GOOS == "windows" {
		beforeHandles = recoveryNamespaceHandleCount(t)
	}
	for _, invalid := range []string{"", "relative", filepath.Join(root, "missing")} {
		if entries, err := readSecretsInitRecoveryDirectory(invalid); err == nil || entries != nil {
			t.Fatalf("invalid directory input accepted: %q %+v %v", invalid, entries, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Fatal("failed open created directory")
	}
	cause := errors.New("synthetic post-validation error")
	entries, err := readSecretsInitRecoveryDirectoryAfterValidation(root, func(*os.File) error { return cause })
	if entries != nil || !errors.Is(err, cause) {
		t.Fatalf("callback error hidden: %+v %v", entries, err)
	}
	entries, err = readSecretsInitRecoveryDirectoryAfterValidation(root, func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		return nil
	})
	if entries != nil || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed enumeration/close failure became success: %+v %v", entries, err)
	}
	entries, err = readSecretsInitRecoveryDirectoryAfterValidation(root, func(file *os.File) error {
		if err := file.Close(); err != nil {
			return err
		}
		return cause
	})
	if entries != nil || !errors.Is(err, cause) || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("primary or secondary close cause lost: %+v %v", entries, err)
	}
	if runtime.GOOS == "windows" {
		afterHandles := recoveryNamespaceHandleCount(t)
		if beforeHandles != afterHandles {
			t.Fatalf("failure-path handle residue before=%d after=%d", beforeHandles, afterHandles)
		}
		t.Logf("actor handles before=%d after=%d; visible closed-file and primary/secondary causes", beforeHandles, afterHandles)
	}
}

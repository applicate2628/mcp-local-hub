package clients

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// Unit adapter only: production-publisher race guards are exercised separately
// through API init and the actual secure writer. No fallback is installed in
// production or when the expectation-aware port is unavailable.
func installRemovalGuardedTestWriter(t *testing.T) {
	t.Helper()
	previous := WriteConfigFileRequest
	t.Cleanup(func() { WriteConfigFileRequest = previous })
	previousVerify := SecureVerifyParentDir
	t.Cleanup(func() { SecureVerifyParentDir = previousVerify })
	SecureVerifyParentDir = func(ctx context.Context, dir string) (bool, error) {
		if ctx != nil && ctx.Err() != nil {
			return false, ctx.Err()
		}
		_, _, release, err := removalAncestorIdentity(filepath.Join(dir, "config.json"))
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		release()
		return true, nil
	}
	WriteConfigFileRequest = func(req ConfigWriteRequest) error {
		if err := req.ExpectedSnapshot.ValidateBinding(req.Context, req.Path); err != nil {
			return err
		}
		file, err := openRemovalSourceFile(req.Path)
		if err != nil {
			return err
		}
		err = req.ExpectedSnapshot.VerifyCurrentFile(req.Context, file)
		file.Close()
		if err != nil {
			return err
		}
		return writeConfigFileWith(nil, req.Path, req.Contents)
	}
}

// The causal fixture has a current Kimi HTTP member with no historical
// pre-Hub backup or marker. Explicit removal must not need either proof.
func TestEntryRemovalKimiNoHistoricalProofAndSiblingToken(t *testing.T) {
	installRemovalGuardedTestWriter(t)
	path := filepath.Join(t.TempDir(), "mcp.json")
	before := "{\n  \"extra\": \"preserve\",\n  \"mcpServers\": {\n    \"one\": {\"url\":\"http://localhost:9121/mcp\"},\n    \"two\": {\"url\":\"https://example.invalid/mcp\",\"headers\":{\"Authorization\":\"synthetic\"}}\n  }\n}\n"
	if err := os.WriteFile(path, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	c := newLockingClient(&kimiCodeCLIClient{&jsonMCPClient{path: path, clientName: "kimi-code-cli", urlField: "url"}})
	one, two := DescribeEntryRemoval(c, "one"), DescribeEntryRemoval(c, "two")
	if !one.Supported || !two.Supported {
		t.Fatalf("capabilities: %+v %+v", one, two)
	}
	r := RemoveEntrySelected(context.Background(), c, "one", one.SourceToken, 5, nil)
	if r.Outcome != "removed" || !r.EffectiveAbsent {
		t.Fatalf("remove: %+v", r)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), `"extra": "preserve"`) || !strings.Contains(string(after), `"two": {"url":"https://example.invalid/mcp","headers":{"Authorization":"synthetic"}}`) {
		t.Fatalf("sibling bytes changed: %s", after)
	}
	if got := DescribeEntryRemoval(c, "two"); got.SourceToken != two.SourceToken {
		t.Fatal("sibling removal invalidated selected token")
	}
	r = RemoveEntrySelected(context.Background(), c, "two", two.SourceToken, 5, nil)
	if r.Outcome != "removed" {
		t.Fatalf("second remove: %+v", r)
	}
	r = RemoveEntrySelected(context.Background(), c, "two", two.SourceToken, 5, nil)
	if r.Code != "REMOVAL_SOURCE_CONFLICT" {
		t.Fatalf("lost response retry incorrectly credited: %+v", r)
	}
}

func TestEntryRemovalRawMemberConflict(t *testing.T) {
	installRemovalGuardedTestWriter(t)
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp","unknown":1}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := newLockingClient(&kimiCodeCLIClient{&jsonMCPClient{path: path, clientName: "kimi-code-cli", urlField: "url"}})
	cap := DescribeEntryRemoval(c, "one")
	changed := []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp","unknown":2}}}`)
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	r := RemoveEntrySelected(context.Background(), c, "one", cap.SourceToken, 5, nil)
	if r.Code != "REMOVAL_SOURCE_CONFLICT" {
		t.Fatalf("raw drift accepted: %+v", r)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(changed) {
		t.Fatal("conflict wrote config")
	}
}

func TestEntryRemovalMiMoSurvivorRefusesBeforeWrite(t *testing.T) {
	installRemovalGuardedTestWriter(t)
	for _, variant := range []string{"own", "lower", "inline", "import"} {
		t.Run(variant, func(t *testing.T) {
			isolateMimoCodeEnv(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "mimocode.json")
			before := []byte(`{"mcp":{"one":{"type":"remote","url":"https://example.invalid/mcp"}}}`)
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			m := &mimoCodeClient{path: path}
			switch variant {
			case "lower":
				if err := os.WriteFile(filepath.Join(dir, "config.json"), before, 0600); err != nil {
					t.Fatal(err)
				}
			case "inline":
				m.inlineContent = string(before)
			case "import":
				m.claudeHome = dir
				t.Setenv(mimoCodeDisableClaudeImportEnv, "")
				if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			c := newLockingClient(m)
			cap := DescribeEntryRemoval(c, "one")
			if variant == "own" {
				if !cap.Supported {
					t.Fatalf("own refused: %+v", cap)
				}
				r := RemoveEntrySelected(context.Background(), c, "one", cap.SourceToken, 5, nil)
				if r.Outcome != "removed" {
					t.Fatalf("own: %+v", r)
				}
			} else {
				if cap.Supported {
					t.Fatalf("survivor supported: %+v", cap)
				}
				got, _ := os.ReadFile(path)
				if string(got) != string(before) {
					t.Fatal("refusal changed bytes")
				}
			}
		})
	}
}

func TestEntryRemovalInversePreservesForeignSiblingAndRejectsForeignTarget(t *testing.T) {
	installRemovalGuardedTestWriter(t)
	for _, foreignTarget := range []bool{false, true} {
		t.Run(fmt.Sprint(foreignTarget), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			before := []byte(`{"extra":0,"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`)
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			c := newLockingClient(&kimiCodeCLIClient{&jsonMCPClient{path: path, clientName: "kimi-code-cli", urlField: "url"}})
			cap := DescribeEntryRemoval(c, "one")
			originalWriter := WriteConfigFile
			t.Cleanup(func() { WriteConfigFile = originalWriter })
			calls := 0
			WriteConfigFile = func(p string, data []byte) error {
				calls++
				if calls == 1 {
					data = []byte(strings.Replace(string(data), `"extra":0`, `"extra":1`, 1))
					if foreignTarget {
						data = []byte(`{"extra":1,"mcpServers":{"one":{"url":"https://foreign.invalid/mcp"}}}`)
					}
					if err := os.WriteFile(p, data, 0600); err != nil {
						return err
					}
					return errors.New("injected write-after-apply failure")
				}
				return originalWriter(p, data)
			}
			r := RemoveEntrySelected(context.Background(), c, "one", cap.SourceToken, 5, nil)
			if foreignTarget {
				if r.Outcome != "rollback-conflict" {
					t.Fatalf("foreign target: %+v", r)
				}
			} else {
				if r.Outcome != "failed" {
					t.Fatalf("compensated: %+v", r)
				}
			}
			got, _ := os.ReadFile(path)
			if !strings.Contains(string(got), `"extra":1`) {
				t.Fatal("inverse discarded foreign sibling")
			}
			want := "https://example.invalid/mcp"
			if foreignTarget {
				want = "https://foreign.invalid/mcp"
			}
			if !strings.Contains(string(got), want) {
				t.Fatal("inverse overwrote target or failed to restore")
			}
		})
	}
}

func TestEntryRemovalSharedSourceScopeAndCancelledRequest(t *testing.T) {
	installRemovalGuardedTestWriter(t)
	path := filepath.Join(t.TempDir(), "mcp.json")
	before := []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	one := newLockingClient(&jsonMCPClient{path: path, clientName: "one", urlField: "url"})
	two := newLockingClient(&jsonMCPClient{path: path, clientName: "two", urlField: "url"})
	cap := DescribeEntryRemoval(one, "one")
	r := RemoveEntrySelected(context.Background(), one, "one", cap.SourceToken, 5, func() error { return EntryRemovalScopeGuard(one, map[string]Client{"one": one, "two": two}, "one") })
	if r.Code != "REMOVAL_SHARED_SOURCE_SCOPE" {
		t.Fatalf("shared source: %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = RemoveEntrySelected(ctx, one, "one", cap.SourceToken, 5, nil)
	if r.Outcome != "failed" || r.Code != "REMOVAL_CANCELLED" {
		t.Fatalf("cancelled result: %+v", r)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(before) {
		t.Fatal("refused request wrote bytes")
	}
}

func TestEntryRemovalInspectionContextDeadlineMutexAndFlock(t *testing.T) {
	installRemovalGuardedTestWriter(t)
	for _, kind := range []string{"mutex", "flock"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "mcp.json")
			before := []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`)
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			c := newLockingClient(&jsonMCPClient{path: path, clientName: "kimi-code-cli", urlField: "url"})
			if kind == "mutex" {
				mu := perPathMutex(path)
				mu.Lock()
				defer mu.Unlock()
			} else {
				lock := flock.New(path + ".lock")
				if err := lock.Lock(); err != nil {
					t.Fatal(err)
				}
				defer lock.Unlock()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, err := DescribeEntryRemovalContext(ctx, c, "one")
			if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
				t.Fatalf("unbounded %s inspection: %v", kind, err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != string(before) {
				t.Fatal("inspection changed source")
			}
		})
	}
	missing := filepath.Join(t.TempDir(), "missing", "mcp.json")
	c := newLockingClient(&jsonMCPClient{path: missing, clientName: "kimi-code-cli", urlField: "url"})
	_, _ = DescribeEntryRemovalContext(context.Background(), c, "one")
	if _, err := os.Stat(filepath.Dir(missing)); !os.IsNotExist(err) {
		t.Fatal("read inspection created parent")
	}
}

func TestEntryRemovalInspectionMissingVerifierFailsClosed(t *testing.T) {
	previous := SecureVerifyParentDir
	SecureVerifyParentDir = nil
	t.Cleanup(func() { SecureVerifyParentDir = previous })
	path := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := newLockingClient(&jsonMCPClient{path: path, clientName: "kimi-code-cli", urlField: "url"})
	cap, err := DescribeEntryRemovalContext(context.Background(), c, "one")
	if err == nil || cap.Supported {
		t.Fatal("missing verifier treated as safe absence")
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("missing verifier created lock")
	}
}

type entryRemovalFailedBackupClient struct{ *kimiCodeCLIClient }

func (c *entryRemovalFailedBackupClient) BackupKeep(int) (string, error) {
	return "", errors.New("synthetic backup failure")
}
func TestEntryRemovalPreparationFailureIsOperational(t *testing.T) {
	installRemovalGuardedTestWriter(t)
	path := filepath.Join(t.TempDir(), "mcp.json")
	before := []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	c := newLockingClient(&entryRemovalFailedBackupClient{&kimiCodeCLIClient{&jsonMCPClient{path: path, clientName: "kimi-code-cli", urlField: "url"}}})
	cap := DescribeEntryRemoval(c, "one")
	r := RemoveEntrySelected(context.Background(), c, "one", cap.SourceToken, 5, nil)
	if r.Outcome != "failed" || r.Code != "REMOVAL_WRITE_FAILED" {
		t.Fatalf("failure hidden as capability: %+v", r)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(before) {
		t.Fatal("preparation failure changed source")
	}
}

func TestEntryRemovalInverseCaptureRejectsMismatchedAncestor(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "source")
	foreign := filepath.Join(root, "foreign")
	for _, p := range []string{dir, foreign} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte(`{"mcpServers":{},"sibling":"preserved"}`)
	path := filepath.Join(dir, "mcp.json")
	foreignPath := filepath.Join(foreign, "mcp.json")
	for _, p := range []string{path, foreignPath} {
		if err := os.WriteFile(p, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	foreignIdentity, _, release, err := removalAncestorIdentity(foreignPath)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	c := newLockingClient(&jsonMCPClient{path: path, clientName: "kimi-code-cli", urlField: "url"}).(*lockingClient)
	err = c.withMutationLock(func() error {
		snapshot, closeCapture, captureErr := c.captureConfigWriteSnapshot(context.Background(), data, foreignIdentity)
		if closeCapture != nil {
			defer closeCapture()
		}
		if snapshot != nil || !errors.Is(captureErr, ErrConfigWriteSnapshotConflict) {
			t.Fatalf("mismatched ancestor admitted: snapshot=%v err=%v", snapshot != nil, captureErr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, foreignPath} {
		got, err := os.ReadFile(p)
		if err != nil || string(got) != string(data) {
			t.Fatal("capture published or changed source")
		}
	}
}
func TestEntryRemovalFreshPhysicalReadbackRejectsRetainedScalar(t *testing.T) {
	installRemovalGuardedTestWriter(t)
	path := filepath.Join(t.TempDir(), "mcp.json")
	before := []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	c := newLockingClient(&kimiCodeCLIClient{&jsonMCPClient{path: path, clientName: "kimi-code-cli", urlField: "url"}})
	cap := DescribeEntryRemoval(c, "one")
	writer := WriteConfigFile
	t.Cleanup(func() { WriteConfigFile = writer })
	WriteConfigFile = func(p string, _ []byte) error {
		return os.WriteFile(p, []byte(`{"mcpServers":{"one":"foreign-invalid-member"}}`), 0600)
	}
	r := RemoveEntrySelected(context.Background(), c, "one", cap.SourceToken, 5, nil)
	if r.Outcome != "rollback-conflict" || r.EffectiveAbsent {
		t.Fatalf("false physical absence: %+v", r)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "foreign-invalid-member") {
		t.Fatal("inverse overwrote foreign scalar")
	}
}

func TestEntryRemovalSameBytesParentReplacementConflicts(t *testing.T) {
	installRemovalGuardedTestWriter(t)
	root := t.TempDir()
	dir := filepath.Join(root, "source")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mcp.json")
	before := []byte(`{"mcpServers":{"one":{"url":"https://example.invalid/mcp"}}}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	c := newLockingClient(&kimiCodeCLIClient{&jsonMCPClient{path: path, clientName: "kimi-code-cli", urlField: "url"}})
	cap := DescribeEntryRemoval(c, "one")
	if err := os.Rename(dir, dir+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	r := RemoveEntrySelected(context.Background(), c, "one", cap.SourceToken, 5, nil)
	if r.Code != "REMOVAL_SOURCE_CONFLICT" {
		t.Fatalf("redirected parent: %+v", r)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(before) {
		t.Fatal("replacement parent mutated")
	}
}

package sonnet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/hubtemp"
)

type fakeDriver struct {
	mu    sync.Mutex
	calls int
	path  string
	start chan struct{}
	done  chan struct{}
}

const admittedTestSON = "FTYP SONPROJ 19\nVER \"18.56\"\nFILEOUT\nFOLDER .\nEND FILEOUT\n"

func (d *fakeDriver) Run(ctx context.Context, _, project string) (DriverResult, error) {
	d.mu.Lock()
	d.calls++
	d.path = project
	d.mu.Unlock()
	if d.start != nil {
		close(d.start)
	}
	if d.done != nil {
		select {
		case <-ctx.Done():
			return DriverResult{}, ctx.Err()
		case <-d.done:
		}
	}
	return DriverResult{ExitCode: 0, Stdout: []byte("ok")}, nil
}

func TestRunReadyProjectCopiesOriginalAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "sample.son")
	if err := os.WriteFile(project, []byte(admittedTestSON), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := &fakeDriver{}
	s := NewServer(Config{RunRoot: t.TempDir(), Locator: func() (Product, error) { return Product{Available: true, Path: "fake-em"}, nil }, Driver: driver})
	defer s.Close(context.Background())
	first, err := s.Start(context.Background(), StartRequest{ProjectRoot: root, ProjectFile: "sample.son", TimeoutSeconds: 10, IdempotencyKey: "same"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Start(context.Background(), StartRequest{ProjectRoot: root, ProjectFile: "sample.son", TimeoutSeconds: 10, IdempotencyKey: "same"})
	if err != nil {
		t.Fatal(err)
	}
	if first.RunID != second.RunID || second.RequestDisposition != "replayed" {
		t.Fatalf("idempotency=%+v %+v", first, second)
	}
	deadline := time.Now().Add(time.Second)
	for func() bool { driver.mu.Lock(); defer driver.mu.Unlock(); return driver.calls == 0 }() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	driver.mu.Lock()
	calls, path := driver.calls, driver.path
	driver.mu.Unlock()
	if calls != 1 || path == project {
		t.Fatalf("driver calls=%d path=%q", calls, path)
	}
	if body, _ := os.ReadFile(project); string(body) != admittedTestSON {
		t.Fatalf("original changed: %q", body)
	}
}

func TestStatusAndResultNeverInvokeDriverAndCancelRequiresPositiveForce(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.son"), []byte(admittedTestSON), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := &fakeDriver{start: make(chan struct{}), done: make(chan struct{})}
	s := NewServer(Config{RunRoot: t.TempDir(), Locator: func() (Product, error) { return Product{Available: true, Path: "fake-em"}, nil }, Driver: driver})
	defer s.Close(context.Background())
	started, err := s.Start(context.Background(), StartRequest{ProjectRoot: root, ProjectFile: "sample.son", TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-driver.start
	if _, err := s.Status(started.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Result(started.RunID); !errors.Is(err, ErrResultPending) {
		t.Fatalf("result error=%v", err)
	}
	driver.mu.Lock()
	calls := driver.calls
	driver.mu.Unlock()
	if calls != 1 {
		t.Fatalf("poll invoked driver: %d", calls)
	}
	if _, err := s.Cancel(context.Background(), started.RunID, "op", false); !errors.Is(err, ErrForceRequired) {
		t.Fatalf("cancel false=%v", err)
	}
	if _, err := s.Cancel(context.Background(), started.RunID, "op", true); err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(started.RunID)
	if err != nil || status.State != StateForceCancelled {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}
func TestProductLocatorRejectsMultipleRegistryProducts(t *testing.T) {
	root := t.TempDir()
	em := filepath.Join(root, "bin", "em.exe")
	if err := os.MkdirAll(filepath.Dir(em), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(em, []byte("em"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := productLocator(Config{RegistryRoots: func() (map[string]string, error) { return map[string]string{"18.56": root}, nil }})()
	if err != nil || !p.Available || p.DiscoverySource != "vendor_registry" {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	_, err = productLocator(Config{RegistryRoots: func() (map[string]string, error) { return map[string]string{"a": root, "b": root}, nil }})()
	if err == nil || err.Error() != "SONNET_PRODUCT_AMBIGUOUS" {
		t.Fatalf("err=%v", err)
	}
}

func TestProductInfoUsesInjectedRegistryLocatorWhenNoExplicitLocator(t *testing.T) {
	t.Setenv("PATH", "")
	root := t.TempDir()
	em := filepath.Join(root, "bin", "em.exe")
	if err := os.MkdirAll(filepath.Dir(em), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(em, []byte("em"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(Config{RunRoot: t.TempDir(), RegistryRoots: func() (map[string]string, error) { return map[string]string{"test": root}, nil }, Driver: &fakeDriver{}})
	defer s.Close(context.Background())
	product, err := s.ProductInfo()
	if err != nil || !product.Available || product.Path != em || product.DiscoverySource != "vendor_registry" {
		t.Fatalf("product info=(%+v, %v)", product, err)
	}
}

func TestProductInfoReportsUnavailableWhenRegistryAndPathMiss(t *testing.T) {
	s := NewServer(Config{RunRoot: t.TempDir(), RegistryRoots: func() (map[string]string, error) { return map[string]string{}, nil }, LookupPath: func(string) (string, error) { return "", errors.New("em absent") }, Driver: &fakeDriver{}})
	defer s.Close(context.Background())
	product, err := s.ProductInfo()
	if err == nil || product.Available {
		t.Fatalf("missing product info=(%+v, %v)", product, err)
	}
}
func TestCancelOperationReceiptIsIdempotentAndCloseWaits(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.son"), []byte(admittedTestSON), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &fakeDriver{start: make(chan struct{}), done: make(chan struct{})}
	s := NewServer(Config{RunRoot: t.TempDir(), Locator: func() (Product, error) { return Product{Available: true, Path: "fake"}, nil }, Driver: d})
	x, err := s.Start(context.Background(), StartRequest{ProjectRoot: root, ProjectFile: "x.son", TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-d.start
	a, err := s.Cancel(context.Background(), x.RunID, "same", true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Cancel(context.Background(), x.RunID, "same", true)
	if err != nil || a.State != b.State {
		t.Fatalf("receipts=%+v %+v err=%v", a, b, err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type stubbornDriver struct {
	started chan struct{}
	release chan struct{}
}

type heldDriver struct {
	started chan struct{}
	release chan struct{}
	project string
}

type writingDriver struct{}

func (writingDriver) Run(_ context.Context, _ string, project string) (DriverResult, error) {
	return DriverResult{ExitCode: 0}, os.WriteFile(filepath.Join(filepath.Dir(project), "created.csv"), []byte("created"), 0o600)
}

type summaryDriver struct{}

func (summaryDriver) Run(_ context.Context, _ string, project string) (DriverResult, error) {
	dir := filepath.Join(filepath.Dir(project), "sondata", strings.TrimSuffix(filepath.Base(project), filepath.Ext(project)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return DriverResult{}, err
	}
	log := "Post-Analysis information for em run 1:\n  Total time for 3 frequencies: 0.185 seconds.\n  Em errors detected: 0    Em warnings detected: 0.\n"
	return DriverResult{ExitCode: 0}, os.WriteFile(filepath.Join(dir, "log_composite.log"), []byte(log), 0o600)
}

func (d *heldDriver) Run(_ context.Context, _ string, project string) (DriverResult, error) {
	d.project = project
	close(d.started)
	<-d.release
	return DriverResult{ExitCode: 0}, nil
}

func TestMCPWireSchemasAndFailuresAreTyped(t *testing.T) {
	s := NewServer(testConfig(t.TempDir(), &fakeDriver{}))
	defer s.Close(context.Background())
	client := wireClient(t, s)
	listed, err := client.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	required := map[string][]string{
		"sonnet_product_info":      {},
		"sonnet_run_ready_project": {"project_root", "project_file", "timeout_seconds"},
		"sonnet_run_status":        {"run_id"},
		"sonnet_run_result":        {"run_id"},
		"sonnet_run_cancel":        {"run_id", "operation_id", "force"},
	}
	for _, tool := range listed.Tools {
		want, known := required[tool.Name]
		if !known {
			t.Fatalf("unexpected advertised tool %q", tool.Name)
		}
		body, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Type     string   `json:"type"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(body, &schema); err != nil {
			t.Fatal(err)
		}
		if schema.Type != "object" || !sameStrings(schema.Required, want) {
			t.Fatalf("tool %q schema=(%s,%v), want object/%v", tool.Name, schema.Type, schema.Required, want)
		}
		delete(required, tool.Name)
	}
	if len(required) != 0 {
		t.Fatalf("missing advertised tools: %v", required)
	}
	valid, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_product_info", Arguments: map[string]any{}})
	if err != nil || valid.IsError {
		t.Fatalf("valid product-info result=(%+v, %v)", valid, err)
	}
	invalid, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_run_status", Arguments: "not-an-object"})
	if err != nil {
		t.Fatal(err)
	}
	if !invalid.IsError || failureID(t, invalid) != "SONNET_REQUEST_INVALID" {
		t.Fatalf("invalid status result=%+v failure=%q", invalid, failureID(t, invalid))
	}
	malformed, err := s.statusTool(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{`)}})
	if err != nil || !malformed.IsError || failureID(t, malformed) != "SONNET_REQUEST_INVALID" {
		t.Fatalf("malformed direct result=(%+v, %v)", malformed, err)
	}
}

func TestProductInfoToolsListSchemaDoesNotSerializeNullRequired(t *testing.T) {
	s := NewServer(testConfig(t.TempDir(), &fakeDriver{}))
	defer s.Close(context.Background())
	listed, err := wireClient(t, s).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != "sonnet_product_info" {
			continue
		}
		body, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]json.RawMessage
		if err := json.Unmarshal(body, &schema); err != nil {
			t.Fatal(err)
		}
		if required, present := schema["required"]; present && string(required) == "null" {
			t.Fatalf("tools/list serialized product-info required as null: %s", body)
		}
		return
	}
	t.Fatal("tools/list omitted sonnet_product_info")
}

func TestMCPProductInfoMapsAbsenceToTypedFailureAndRegistryProductToSuccess(t *testing.T) {
	absent := NewServer(Config{RunRoot: t.TempDir(), Locator: func() (Product, error) {
		return Product{DiscoverySource: "PATH"}, errors.New("SONNET_PRODUCT_NOT_FOUND")
	}, Driver: &fakeDriver{}})
	defer absent.Close(context.Background())
	absentResult, err := wireClient(t, absent).CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_product_info", Arguments: map[string]any{}})
	if err != nil || !absentResult.IsError || failureID(t, absentResult) != "SONNET_PRODUCT_NOT_FOUND" {
		t.Fatalf("absent product result=(%+v, %v)", absentResult, err)
	}
	absentBody := resultJSON(t, absentResult)
	if available, ok := absentBody["available"].(bool); !ok || available {
		t.Fatalf("absent product body=%v, want available:false", absentBody)
	}

	t.Setenv("PATH", "")
	registryRoot := t.TempDir()
	em := filepath.Join(registryRoot, "bin", "em.exe")
	if err := os.MkdirAll(filepath.Dir(em), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(em, []byte("em"), 0o600); err != nil {
		t.Fatal(err)
	}
	present := NewServer(Config{RunRoot: t.TempDir(), RegistryRoots: func() (map[string]string, error) { return map[string]string{"registry": registryRoot}, nil }, Driver: &fakeDriver{}})
	defer present.Close(context.Background())
	presentResult, err := wireClient(t, present).CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_product_info", Arguments: map[string]any{}})
	if err != nil || presentResult.IsError {
		t.Fatalf("registry product result=(%+v, %v)", presentResult, err)
	}
	presentBody := resultJSON(t, presentResult)
	if available, ok := presentBody["available"].(bool); !ok || !available || presentBody["discovery_source"] != "vendor_registry" {
		t.Fatalf("registry product body=%v", presentBody)
	}

	ambiguous := NewServer(Config{RunRoot: t.TempDir(), Locator: func() (Product, error) {
		return Product{DiscoverySource: "vendor_registry"}, errors.New("SONNET_PRODUCT_AMBIGUOUS")
	}, Driver: &fakeDriver{}})
	defer ambiguous.Close(context.Background())
	ambiguousResult, err := wireClient(t, ambiguous).CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_product_info", Arguments: map[string]any{}})
	if err != nil || !ambiguousResult.IsError || failureID(t, ambiguousResult) != "SONNET_PRODUCT_AMBIGUOUS" {
		t.Fatalf("ambiguous product result=(%+v, %v)", ambiguousResult, err)
	}
}

func TestStartMissingProductDoesNotWrapNilCause(t *testing.T) {
	projectRoot := writeProject(t)
	s := NewServer(Config{RunRoot: t.TempDir(), Locator: func() (Product, error) { return Product{}, nil }, Driver: &fakeDriver{}})
	defer s.Close(context.Background())
	_, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err == nil || err.Error() != "SONNET_PRODUCT_NOT_FOUND" {
		t.Fatalf("missing product start error=%v", err)
	}
}

func TestPreparedReceiptStoresRedactedResolvedProductIdentity(t *testing.T) {
	projectRoot := writeProject(t)
	s := NewServer(Config{RunRoot: t.TempDir(), Locator: func() (Product, error) {
		return Product{Available: true, Path: "C:\\private\\em.exe", SHA256: "product-hash", DiscoverySource: "vendor_registry"}, nil
	}, Driver: &fakeDriver{}})
	defer s.Close(context.Background())
	started, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	workspace := s.runs[started.RunID].workspace
	body, err := os.ReadFile(filepath.Join(workspace, "control", "00000001.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	identity, ok := receipt["resolved_product"].(map[string]any)
	if !ok || identity["sha256"] != "product-hash" || identity["discovery_source"] != "vendor_registry" {
		t.Fatalf("resolved product receipt=%v", receipt["resolved_product"])
	}
	if _, leaked := identity["resolved_em_path"]; leaked {
		t.Fatalf("resolved product leaked path: %v", identity)
	}
}

func TestMCPResultExposesObservedPostAnalysisSummary(t *testing.T) {
	projectRoot := writeProject(t)
	s := NewServer(testConfig(t.TempDir(), summaryDriver{}))
	defer s.Close(context.Background())
	client := wireClient(t, s)
	started, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, s, started.RunID)
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_run_result", Arguments: map[string]any{"run_id": started.RunID}})
	if err != nil || result.IsError {
		t.Fatalf("result=(%+v, %v)", result, err)
	}
	summary, ok := resultJSON(t, result)["run_summary"].(map[string]any)
	if !ok || summary["state"] != "observed" || summary["frequencies"] != float64(3) || summary["errors"] != float64(0) || summary["warnings"] != float64(0) {
		t.Fatalf("run summary=%v", summary)
	}
}

func TestStartRejectsUnsafeFILEOUTFolderBeforeDriverLaunch(t *testing.T) {
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "sample.son"), []byte("FTYP SONPROJ 19\nVER \"18.56\"\nFILEOUT\nFOLDER ..\\outside\nEND FILEOUT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := &fakeDriver{}
	s := NewServer(testConfig(t.TempDir(), driver))
	defer s.Close(context.Background())
	_, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err == nil || !strings.Contains(err.Error(), "SONNET_PROJECT_INVALID") {
		t.Fatalf("unsafe FILEOUT start error=%v", err)
	}
	driver.mu.Lock()
	calls := driver.calls
	driver.mu.Unlock()
	if calls != 0 {
		t.Fatalf("unsafe FILEOUT reached driver %d times", calls)
	}
}

func TestStartRejectsUnverifiedSPROJReferencesBeforeDriverLaunch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		directive string
		reference string
		contained bool
	}{
		{name: "contained", directive: "TYPE SPROJ", reference: "component.son", contained: true},
		{name: "outside", directive: "TYPE SPROJ", reference: `..\outside.son`},
		{name: "whitespace", directive: "\tTYPE\tSPROJ", reference: "component.son", contained: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectRoot := t.TempDir()
			project := "FTYP SONPROJ 19\nVER \"18.56\"\n" + tc.directive + " " + tc.reference + "\nFILEOUT\nFOLDER .\nEND FILEOUT\n"
			if err := os.WriteFile(filepath.Join(projectRoot, "sample.son"), []byte(project), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.contained {
				if err := os.WriteFile(filepath.Join(projectRoot, tc.reference), []byte("FTYP SONPROJ 19\nVER \"18.56\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			driver := &fakeDriver{}
			s := NewServer(testConfig(t.TempDir(), driver))
			defer s.Close(context.Background())

			_, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
			if err == nil {
				deadline := time.Now().Add(time.Second)
				for func() bool { driver.mu.Lock(); defer driver.mu.Unlock(); return driver.calls == 0 }() && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				driver.mu.Lock()
				calls := driver.calls
				driver.mu.Unlock()
				t.Fatalf("unverified SPROJ reference reached driver %d times", calls)
			}
			if !strings.Contains(err.Error(), "SONNET_PROJECT_INVALID") {
				t.Fatalf("SPROJ start error=%v, want SONNET_PROJECT_INVALID", err)
			}
			driver.mu.Lock()
			calls := driver.calls
			driver.mu.Unlock()
			if calls != 0 {
				t.Fatalf("unverified SPROJ reference reached driver %d times", calls)
			}
			s.mu.Lock()
			runs := len(s.runs)
			s.mu.Unlock()
			if runs != 0 || countRunWorkspaces(t, s.root) != 0 {
				t.Fatalf("unverified SPROJ reference retained prepared residue runs=%d workspaces=%d", runs, countRunWorkspaces(t, s.root))
			}
		})
	}
	for _, line := range []string{`NOTE "TYPE SPROJ component.son"`, "COMMENT TYPE SPROJ component.son"} {
		t.Run("non-directive_"+strings.Fields(line)[0], func(t *testing.T) {
			projectRoot := t.TempDir()
			project := "FTYP SONPROJ 19\nVER \"18.56\"\n" + line + "\nFILEOUT\nFOLDER .\nEND FILEOUT\n"
			if err := os.WriteFile(filepath.Join(projectRoot, "sample.son"), []byte(project), 0o600); err != nil {
				t.Fatal(err)
			}
			admission, err := admitCopiedProject(projectRoot, "sample.son", "copied")
			if err != nil || admission.References == nil || len(admission.References) != 0 {
				t.Fatalf("non-directive reference-like text admission=%+v err=%v", admission, err)
			}
		})
	}
}

func TestStartPersistsPathAdmissionReceiptForDotFILEOUT(t *testing.T) {
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "sample.son"), []byte("FTYP SONPROJ 19\nVER \"18.56\"\nFILEOUT\nFOLDER .\nEND FILEOUT\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(testConfig(t.TempDir(), &fakeDriver{}))
	defer s.Close(context.Background())
	started, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, s, started.RunID)
	workspace := s.runs[started.RunID].workspace
	path := filepath.Join(workspace, "control", "00000003.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	admission, ok := receipt["path_admission"].(map[string]any)
	if !ok || admission["rule_version"] != "sonnet-path-admission-v1" || admission["output_folder"] != "." || admission["copied_project_sha256"] != started.CopiedProjectSHA256 {
		t.Fatalf("path admission receipt=%v", receipt["path_admission"])
	}
}

func TestFILEOUTFolderAdmissionAcceptsOnlyCleanRelativeTargets(t *testing.T) {
	for _, folder := range []string{".", "outputs", "outputs\\nested"} {
		if !cleanOutputFolder(folder) {
			t.Fatalf("admitted folder rejected: %q", folder)
		}
	}
	for _, folder := range []string{"..", "..\\outside", "C:drive-relative", "C:\\absolute", "\\\\server\\share", "outputs\\..\\escape", "outputs//duplicate", "$TEMP"} {
		if cleanOutputFolder(folder) {
			t.Fatalf("unsafe folder admitted: %q", folder)
		}
	}
}

func TestStartRemovesOwnedWorkspaceWhenBaselineSnapshotFails(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := writeProject(t)
	s := NewServer(testConfig(storeRoot, &fakeDriver{}))
	defer s.Close(context.Background())

	baselineErr := errors.New("injected baseline snapshot failure")
	var workspace string
	var cleanupPath string
	s.removeWork = func(workRoot string) error {
		cleanupPath = workRoot
		return os.RemoveAll(workRoot)
	}
	s.snapshotOutput = func(workRoot string) (map[string]OutputFile, error) {
		workspace = filepath.Dir(workRoot)
		return nil, baselineErr
	}

	_, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if !errors.Is(err, baselineErr) {
		t.Fatalf("start error=%v, want baseline cause", err)
	}
	if cleanupPath != workspace {
		t.Fatalf("cleanup path=%q, want exact copied workspace %q", cleanupPath, workspace)
	}
	assertOwnedWorkspaceRemoved(t, storeRoot, workspace)
}

func TestStartConcurrentDuplicateKeyRemovesOnlyLosingCopiedWorkspace(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := writeProject(t)
	s := NewServer(testConfig(storeRoot, &fakeDriver{}))
	defer s.Close(context.Background())

	var cleanupPaths []string
	s.removeWork = func(workRoot string) error {
		cleanupPaths = append(cleanupPaths, workRoot)
		return os.RemoveAll(workRoot)
	}
	request := StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10, IdempotencyKey: "same-request"}
	first, second := startAtConcurrentPostCopyBarrier(t, s, storeRoot, request)
	if first.err != nil || second.err != nil {
		t.Fatalf("start errors=(%v, %v)", first.err, second.err)
	}
	if first.response.RunID == "" || first.response.RunID != second.response.RunID {
		t.Fatalf("run ids=(%q, %q), want one shared identity", first.response.RunID, second.response.RunID)
	}
	dispositions := map[string]int{
		first.response.RequestDisposition:  1,
		second.response.RequestDisposition: 1,
	}
	if first.response.RequestDisposition == second.response.RequestDisposition {
		dispositions[first.response.RequestDisposition] = 2
	}
	if dispositions["started"] != 1 || dispositions["replayed"] != 1 {
		t.Fatalf("request dispositions=(%q, %q), want one started and one replayed", first.response.RequestDisposition, second.response.RequestDisposition)
	}
	if got := countRunWorkspaces(t, storeRoot); got != 1 {
		t.Fatalf("settled workspaces=%d, want only the admitted run", got)
	}
	if len(cleanupPaths) != 1 || !within(filepath.Join(storeRoot, "runs"), cleanupPaths[0]) {
		t.Fatalf("cleanup paths=%v, want one owned losing workspace", cleanupPaths)
	}
	s.mu.Lock()
	winner := s.runs[first.response.RunID].workspace
	s.mu.Unlock()
	if cleanupPaths[0] == winner {
		t.Fatalf("cleanup removed admitted workspace %q", winner)
	}
}

func TestStartRemovesOwnedWorkspaceWhenActiveMarkerCreationFails(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := writeProject(t)
	s := NewServer(testConfig(storeRoot, &fakeDriver{}))
	defer s.Close(context.Background())

	foreign := filepath.Join(storeRoot, "runs", "foreign-project")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	foreignMarker := filepath.Join(foreign, "keep.txt")
	if err := os.WriteFile(foreignMarker, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	markerErr := errors.New("injected active marker failure")
	var workspace string
	var cleanupPath string
	s.removeWork = func(workRoot string) error {
		cleanupPath = workRoot
		return os.RemoveAll(workRoot)
	}
	s.markActive = func(workRoot string) (func(), error) {
		workspace = workRoot
		if err := os.WriteFile(filepath.Join(workRoot, "partial-marker"), []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		return nil, markerErr
	}

	_, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if !errors.Is(err, markerErr) {
		t.Fatalf("start error=%v, want active-marker cause", err)
	}
	if cleanupPath != workspace {
		t.Fatalf("cleanup path=%q, want exact copied workspace %q", cleanupPath, workspace)
	}
	assertOwnedWorkspaceRemoved(t, storeRoot, workspace)
	if body, err := os.ReadFile(foreignMarker); err != nil || string(body) != "foreign" {
		t.Fatalf("foreign workspace changed: body=%q err=%v", body, err)
	}
}

func TestStartPreservesUncertainPreparedReceiptWithoutPublishingRun(t *testing.T) {
	storeRoot := t.TempDir()
	projectRoot := writeProject(t)
	s := NewServer(testConfig(storeRoot, &fakeDriver{}))
	defer s.Close(context.Background())

	cleanupCalls := 0
	s.removeWork = func(string) error {
		cleanupCalls++
		return errors.New("uncertain workspace must not be removed")
	}
	persistErr := errors.New("injected post-rename persistence failure")
	s.snapshotWriter = func(path string, body []byte) error {
		if err := durableWrite(path, body); err != nil {
			return err
		}
		return persistErr
	}
	_, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10, IdempotencyKey: "uncertain-persist"})
	if !errors.Is(err, persistErr) {
		t.Fatalf("start error=%v, want persistence cause", err)
	}
	if cleanupCalls != 0 {
		t.Fatalf("uncertain persistence invoked workspace cleanup %d times", cleanupCalls)
	}
	entries, err := os.ReadDir(filepath.Join(storeRoot, "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("uncertain workspaces=(%v, %v), want one retained candidate", entries, err)
	}
	workspace := filepath.Join(storeRoot, "runs", entries[0].Name())
	record, found, err := loadRunRecord(workspace)
	if err != nil || !found || record.State != StatePrepared {
		t.Fatalf("uncertain prepared record=(%+v, found=%v, err=%v)", record, found, err)
	}
	if _, published := s.runs[record.RunID]; published {
		t.Fatalf("failed start published run %q in memory", record.RunID)
	}
	if _, published := s.keys["uncertain-persist"]; published {
		t.Fatal("failed start published idempotency key in memory")
	}
	if _, err := os.Stat(filepath.Join(workspace, activeMarkerLeaf)); !os.IsNotExist(err) {
		t.Fatalf("failed start retained active marker: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	restarted := NewServer(testConfig(storeRoot, &fakeDriver{}))
	defer restarted.Close(context.Background())
	status, err := restarted.Status(record.RunID)
	if err != nil || status.State != StateHostLost || status.FailureID != "SONNET_HOST_LOST" {
		t.Fatalf("recovered uncertain status=(%+v, %v), want host_lost evidence", status, err)
	}
}

func TestStartPreTransferCleanupFailuresRemainVisible(t *testing.T) {
	cleanupErr := errors.New("injected workspace cleanup failure")

	t.Run("baseline snapshot", func(t *testing.T) {
		storeRoot := t.TempDir()
		projectRoot := writeProject(t)
		s := NewServer(testConfig(storeRoot, &fakeDriver{}))
		defer s.Close(context.Background())
		baselineErr := errors.New("injected baseline snapshot failure")
		var workspace string
		s.snapshotOutput = func(workRoot string) (map[string]OutputFile, error) {
			workspace = filepath.Dir(workRoot)
			return nil, baselineErr
		}
		s.removeWork = func(workRoot string) error {
			if workRoot != workspace {
				t.Fatalf("cleanup path=%q, want %q", workRoot, workspace)
			}
			return cleanupErr
		}

		_, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
		if !errors.Is(err, baselineErr) || !errors.Is(err, cleanupErr) {
			t.Fatalf("start error=%v, want baseline and cleanup causes", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	})

	t.Run("active marker", func(t *testing.T) {
		storeRoot := t.TempDir()
		projectRoot := writeProject(t)
		s := NewServer(testConfig(storeRoot, &fakeDriver{}))
		defer s.Close(context.Background())
		markerErr := errors.New("injected active marker failure")
		var workspace string
		s.markActive = func(workRoot string) (func(), error) {
			workspace = workRoot
			return nil, markerErr
		}
		s.removeWork = func(workRoot string) error {
			if workRoot != workspace {
				t.Fatalf("cleanup path=%q, want %q", workRoot, workspace)
			}
			return cleanupErr
		}

		_, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
		if !errors.Is(err, markerErr) || !errors.Is(err, cleanupErr) || stableFailureID(err) != "SONNET_PROJECT_COPY_FAILED" {
			t.Fatalf("start error=%v, want typed marker and cleanup causes", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(workspace) })
	})

	t.Run("duplicate replay", func(t *testing.T) {
		storeRoot := t.TempDir()
		projectRoot := writeProject(t)
		s := NewServer(testConfig(storeRoot, &fakeDriver{}))
		defer s.Close(context.Background())
		var cleanupPath string
		s.removeWork = func(workRoot string) error {
			cleanupPath = workRoot
			return cleanupErr
		}
		request := StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10, IdempotencyKey: "same-request"}
		first, second := startAtConcurrentPostCopyBarrier(t, s, storeRoot, request)
		results := []concurrentStartResult{first, second}
		successes := 0
		failures := 0
		for _, result := range results {
			if result.err == nil {
				successes++
				if result.response.RequestDisposition != "started" {
					t.Fatalf("successful disposition=%q, want started", result.response.RequestDisposition)
				}
				continue
			}
			failures++
			if !errors.Is(result.err, cleanupErr) || stableFailureID(result.err) != "SONNET_WORKSPACE_CLEANUP_FAILED" {
				t.Fatalf("duplicate cleanup error=%v", result.err)
			}
		}
		if successes != 1 || failures != 1 {
			t.Fatalf("duplicate results=%+v, want one admitted run and one visible cleanup failure", results)
		}
		if cleanupPath == "" || !within(filepath.Join(storeRoot, "runs"), cleanupPath) {
			t.Fatalf("cleanup path=%q, want owned losing workspace", cleanupPath)
		}
		t.Cleanup(func() { _ = os.RemoveAll(cleanupPath) })
	})
}

func TestCleanupPreTransferWorkRefusesForeignWorkspace(t *testing.T) {
	storeRoot := t.TempDir()
	s := NewServer(testConfig(storeRoot, &fakeDriver{}))
	defer s.Close(context.Background())
	foreign := filepath.Join(storeRoot, "runs", "foreign-project")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(foreign, "keep.txt")
	if err := os.WriteFile(marker, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.removeWork = func(string) error {
		t.Fatal("foreign workspace reached removal primitive")
		return nil
	}
	primaryErr := errors.New("injected primary failure")

	err := s.cleanupPreTransferWork(foreign, primaryErr)
	if !errors.Is(err, primaryErr) || !strings.Contains(err.Error(), "SONNET_WORKSPACE_CLEANUP_FAILED") {
		t.Fatalf("cleanup error=%v, want cleanup refusal plus primary cause", err)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "foreign" {
		t.Fatalf("foreign workspace changed: body=%q err=%v", body, err)
	}
}

type concurrentStartResult struct {
	response StartResponse
	err      error
}

func startAtConcurrentPostCopyBarrier(t *testing.T, s *Server, storeRoot string, request StartRequest) (concurrentStartResult, concurrentStartResult) {
	t.Helper()
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	s.afterCopy = func() {
		arrived <- struct{}{}
		<-release
	}
	results := make(chan concurrentStartResult, 2)
	for range 2 {
		go func() {
			response, err := s.Start(context.Background(), request)
			results <- concurrentStartResult{response: response, err: err}
		}()
	}

	<-arrived
	secondArrived := false
	select {
	case <-arrived:
		secondArrived = true
	case <-time.After(5 * time.Second):
	}
	if secondArrived {
		if got := countRunWorkspaces(t, storeRoot); got != 2 {
			t.Fatalf("post-copy workspaces=%d, want two isolated candidates", got)
		}
	}
	close(release)
	first := <-results
	second := <-results
	if !secondArrived {
		t.Fatal("second start could not reach the post-copy barrier concurrently")
	}
	return first, second
}

func assertOwnedWorkspaceRemoved(t *testing.T, storeRoot, workspace string) {
	t.Helper()
	if workspace == "" || !within(filepath.Join(storeRoot, "runs"), workspace) {
		t.Fatalf("workspace %q is not an owned run root", workspace)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("owned workspace %q retained: %v", workspace, err)
	}
}

func countRunWorkspaces(t *testing.T, storeRoot string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(storeRoot, "runs"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count
}

func TestCopyProjectUsesCanonicalRelativeSourceManifest(t *testing.T) {
	projectRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectRoot, "sample.son"), []byte(admittedTestSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "nested", "input.dat"), []byte("input"), 0o600); err != nil {
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(projectRoot, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(projectRoot, "nested", "input.dat"), []byte("input"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := snapshotOutput(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(testConfig(t.TempDir(), &fakeDriver{}))
	defer s.Close(context.Background())
	work, sourceHash, _, _, err := s.copyProject(projectRoot, "sample.son")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(work)
	if want := canonicalManifestHashForTest(before); sourceHash != want {
		t.Fatalf("source manifest hash=%s want canonical=%s", sourceHash, want)
	}
}

func canonicalManifestHashForTest(files map[string]OutputFile) string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		file := files[path]
		_, _ = hash.Write([]byte(path + "\x00" + strconv.FormatInt(file.Size, 10) + "\x00" + file.SHA256 + "\n"))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestMCPRejectsNonObjectArgumentsForEveryTool(t *testing.T) {
	s := NewServer(testConfig(t.TempDir(), &fakeDriver{}))
	defer s.Close(context.Background())
	client := wireClient(t, s)
	for label, raw := range map[string]json.RawMessage{"null": json.RawMessage("null"), "array": json.RawMessage("[]"), "scalar": json.RawMessage(`"scalar"`)} {
		for _, name := range []string{"sonnet_product_info", "sonnet_run_ready_project", "sonnet_run_status", "sonnet_run_result", "sonnet_run_cancel"} {
			result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: raw})
			if err != nil {
				t.Fatalf("%s %s call: %v", name, label, err)
			}
			if !result.IsError || failureID(t, result) != "SONNET_REQUEST_INVALID" {
				t.Fatalf("%s %s result=%+v failure=%q", name, label, result, failureID(t, result))
			}
		}
	}
	valid, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_product_info", Arguments: map[string]any{}})
	if err != nil || valid.IsError {
		t.Fatalf("empty product-info result=(%+v, %v)", valid, err)
	}
}

func TestMCPResultProjectsOnlyCreatedOrModifiedOutputs(t *testing.T) {
	projectRoot := writeProject(t)
	if err := os.WriteFile(filepath.Join(projectRoot, "baseline.s2p"), []byte("baseline"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(testConfig(t.TempDir(), writingDriver{}))
	defer s.Close(context.Background())
	client := wireClient(t, s)
	started, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, s, started.RunID)
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_run_result", Arguments: map[string]any{"run_id": started.RunID}})
	if err != nil || result.IsError {
		t.Fatalf("result=(%+v, %v)", result, err)
	}
	body := resultJSON(t, result)
	manifest, ok := body["output_manifest"].(map[string]any)
	if !ok {
		t.Fatalf("result omitted output_manifest: %v", body)
	}
	outputs, ok := manifest["outputs"].([]any)
	if !ok || len(outputs) != 1 {
		t.Fatalf("public outputs=%v, want one created file", manifest["outputs"])
	}
	output, ok := outputs[0].(map[string]any)
	if !ok || output["path"] != "created.csv" || output["disposition"] != "created" {
		t.Fatalf("public output=%v, want created.csv", outputs[0])
	}
	if _, present := body["baseline.s2p"]; present {
		t.Fatalf("unchanged baseline leaked at result top level: %v", body)
	}
}

func TestMCPResultIncludesAnEmptyOutputListForValidZeroDelta(t *testing.T) {
	projectRoot := writeProject(t)
	s := NewServer(testConfig(t.TempDir(), &fakeDriver{}))
	defer s.Close(context.Background())
	client := wireClient(t, s)
	started, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, s, started.RunID)
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_run_result", Arguments: map[string]any{"run_id": started.RunID}})
	if err != nil || result.IsError {
		t.Fatalf("result=(%+v, %v)", result, err)
	}
	manifest := resultJSON(t, result)["output_manifest"].(map[string]any)
	outputs, ok := manifest["outputs"].([]any)
	if !ok || len(outputs) != 0 {
		t.Fatalf("zero-delta outputs=%v, want explicit empty list", manifest["outputs"])
	}
}

func TestTerminalPersistenceFailureDoesNotPublishCompletedResult(t *testing.T) {
	projectRoot := writeProject(t)
	driver := &heldDriver{started: make(chan struct{}), release: make(chan struct{})}
	s := NewServer(testConfig(t.TempDir(), driver))
	defer s.Close(context.Background())
	started, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	<-driver.started
	control := filepath.Join(filepath.Dir(filepath.Dir(driver.project)), "control")
	if err := os.RemoveAll(control); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(control, []byte("write fault"), 0o600); err != nil {
		t.Fatal(err)
	}
	close(driver.release)
	waitTerminal(t, s, started.RunID)
	status, err := s.Status(started.RunID)
	if err != nil || status.State != StateFailed || status.FailureID != "SONNET_RUN_STORE_WRITE_FAILED" || status.Reportable {
		t.Fatalf("terminal write fault status=(%+v, %v), want failed nonreportable store failure", status, err)
	}
}

func TestCancelRejectsEmptyOperationWithoutMutatingRun(t *testing.T) {
	projectRoot := writeProject(t)
	driver := &fakeDriver{start: make(chan struct{}), done: make(chan struct{})}
	s := NewServer(testConfig(t.TempDir(), driver))
	defer s.Close(context.Background())
	started, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-driver.start
	before, err := s.Status(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(context.Background(), started.RunID, "", true); !errors.Is(err, ErrForceRequired) {
		t.Fatalf("empty operation error=%v", err)
	}
	after, err := s.Status(started.RunID)
	if err != nil || after.Phase != before.Phase || after.State != before.State {
		t.Fatalf("empty operation mutated status before=%+v after=%+v err=%v", before, after, err)
	}
	if _, err := s.Cancel(context.Background(), started.RunID, "cleanup", true); err != nil {
		t.Fatal(err)
	}
}

func TestRunStoreWriteFaultsPersistFailedStateAcrossRestart(t *testing.T) {
	for _, tc := range []struct {
		name       string
		generation string
		driver     Driver
		release    func()
	}{
		{name: "running", generation: "00000002.json", driver: &fakeDriver{}},
		{name: "terminal", generation: "00000003.json", driver: &heldDriver{started: make(chan struct{}), release: make(chan struct{})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projectRoot := writeProject(t)
			storeRoot := t.TempDir()
			s := NewServer(testConfig(storeRoot, tc.driver))
			s.snapshotWriter = func(path string, body []byte) error {
				if filepath.Base(path) == tc.generation {
					return errors.New("injected snapshot failure")
				}
				return durableWrite(path, body)
			}
			started, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
			if err != nil {
				t.Fatal(err)
			}
			if driver, ok := tc.driver.(*heldDriver); ok {
				<-driver.started
				close(driver.release)
			}
			waitTerminal(t, s, started.RunID)
			status, err := s.Status(started.RunID)
			if err != nil || status.State != StateFailed || status.FailureID != "SONNET_RUN_STORE_WRITE_FAILED" || status.Reportable || !strings.Contains(status.Error, "injected snapshot failure") {
				t.Fatalf("write-fault status=(%+v, %v)", status, err)
			}
			if _, err := s.Result(started.RunID); err == nil || !strings.Contains(err.Error(), "SONNET_RUN_STORE_WRITE_FAILED") {
				t.Fatalf("write-fault result error=%v", err)
			}
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			restarted := NewServer(testConfig(storeRoot, &fakeDriver{}))
			defer restarted.Close(context.Background())
			recovered, err := restarted.Status(started.RunID)
			if err != nil || recovered.State != StateFailed || recovered.FailureID != "SONNET_RUN_STORE_WRITE_FAILED" || recovered.Reportable {
				t.Fatalf("restart write-fault status=(%+v, %v)", recovered, err)
			}
		})
	}
}

func (d *stubbornDriver) Run(context.Context, string, string) (DriverResult, error) {
	close(d.started)
	<-d.release
	return DriverResult{ExitCode: 0}, nil
}

func TestRunStoreLoadsTerminalRecordAfterRestart(t *testing.T) {
	projectRoot := writeProject(t)
	storeRoot := t.TempDir()
	first := NewServer(testConfig(storeRoot, &fakeDriver{}))
	started, err := first.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, first, started.RunID)
	if err := first.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted := NewServer(testConfig(storeRoot, &fakeDriver{}))
	defer restarted.Close(context.Background())
	got, err := restarted.Status(started.RunID)
	if err != nil || got.State != StateCompleted || got.Generation != 3 {
		t.Fatalf("restart status=(%+v, %v), want completed durable record", got, err)
	}
}

func TestRunStoreRecoversPendingRecordAsHostLostWithoutDriverAdoption(t *testing.T) {
	storeRoot := t.TempDir()
	writeRunRecord(t, storeRoot, "pending", `{"schema_version":"sonnet-run-v1","run_id":"pending","generation":4,"state":"running","phase":"running","updated_at":"2026-09-10T00:00:00Z"}`, time.Now())
	if err := os.Rename(filepath.Join(storeRoot, "runs", "pending", "control", "00000001.json"), filepath.Join(storeRoot, "runs", "pending", "control", "00000004.json")); err != nil {
		t.Fatal(err)
	}
	driver := &fakeDriver{}
	s := NewServer(testConfig(storeRoot, driver))
	defer s.Close(context.Background())
	status, err := s.Status("pending")
	if err != nil || status.State != StateHostLost || status.FailureID != "SONNET_HOST_LOST" || status.Reportable {
		t.Fatalf("recovered status=(%+v, %v), want nonreportable host_lost", status, err)
	}
	driver.mu.Lock()
	calls := driver.calls
	driver.mu.Unlock()
	if calls != 0 {
		t.Fatalf("startup recovery invoked driver %d times", calls)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted := NewServer(testConfig(storeRoot, &fakeDriver{}))
	defer restarted.Close(context.Background())
	status, err = restarted.Status("pending")
	if err != nil || status.State != StateHostLost || status.Generation != 5 {
		t.Fatalf("persisted recovery status=(%+v, %v), want generation 5 host_lost", status, err)
	}
}

func TestRecoveredHostLostResultPreservesFailureIdentity(t *testing.T) {
	storeRoot := t.TempDir()
	writeRunRecord(t, storeRoot, "pending", `{"schema_version":"sonnet-run-v1","run_id":"pending","generation":4,"state":"running","phase":"running","updated_at":"2026-09-10T00:00:00Z"}`, time.Now())
	if err := os.Rename(filepath.Join(storeRoot, "runs", "pending", "control", "00000001.json"), filepath.Join(storeRoot, "runs", "pending", "control", "00000004.json")); err != nil {
		t.Fatal(err)
	}
	driver := &fakeDriver{}
	s := NewServer(testConfig(storeRoot, driver))
	defer s.Close(context.Background())

	status, err := s.Status("pending")
	if err != nil || status.State != StateHostLost || status.FailureID != "SONNET_HOST_LOST" || status.Reportable {
		t.Fatalf("recovered status=(%+v, %v), want nonreportable host_lost", status, err)
	}
	if _, err := s.Result("pending"); err == nil || !strings.Contains(err.Error(), "SONNET_HOST_LOST") {
		t.Fatalf("recovered result error=%v, want SONNET_HOST_LOST", err)
	}
	result, err := wireClient(t, s).CallTool(context.Background(), &mcp.CallToolParams{Name: "sonnet_run_result", Arguments: map[string]any{"run_id": "pending"}})
	if err != nil || !result.IsError || failureID(t, result) != "SONNET_HOST_LOST" {
		t.Fatalf("recovered MCP result=(%+v, %v) failure=%q, want SONNET_HOST_LOST", result, err, failureID(t, result))
	}
	driver.mu.Lock()
	calls := driver.calls
	driver.mu.Unlock()
	if calls != 0 {
		t.Fatalf("result recovery invoked driver %d times", calls)
	}
}

func TestRunStorePersistsAppendOnlyGenerationSnapshots(t *testing.T) {
	projectRoot := writeProject(t)
	storeRoot := t.TempDir()
	s := NewServer(testConfig(storeRoot, &fakeDriver{}))
	started, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, s, started.RunID)
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	runDirs, err := os.ReadDir(filepath.Join(storeRoot, "runs"))
	if err != nil || len(runDirs) != 1 {
		t.Fatalf("run directories=(%v, %v), want one", runDirs, err)
	}
	entries, err := os.ReadDir(filepath.Join(storeRoot, "runs", runDirs[0].Name(), "control"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, entry := range entries {
		got[entry.Name()] = true
	}
	for _, name := range []string{"00000001.json", "00000002.json", "00000003.json"} {
		if !got[name] {
			t.Fatalf("missing durable generation snapshot %q in %v", name, got)
		}
	}
	if got["record.json"] {
		t.Fatalf("mutable record.json present beside append-only snapshots: %v", got)
	}
}

func TestRunStoreRejectsConcurrentOwner(t *testing.T) {
	projectRoot := writeProject(t)
	storeRoot := t.TempDir()
	first := NewServer(testConfig(storeRoot, &fakeDriver{}))
	defer first.Close(context.Background())
	second := NewServer(testConfig(storeRoot, &fakeDriver{}))
	_, err := second.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
	if err == nil || !strings.Contains(err.Error(), "SONNET_RUN_STORE_OWNED") {
		t.Fatalf("second owner error=%v, want SONNET_RUN_STORE_OWNED", err)
	}
}

func TestRunStoreRejectsMalformedAndFutureRecords(t *testing.T) {
	projectRoot := writeProject(t)
	for name, body := range map[string]string{
		"malformed": `{"schema_version":"sonnet-run-v1",`,
		"future":    `{"schema_version":"sonnet-run-v2","run_id":"future","generation":1,"state":"completed","phase":"settled","updated_at":"2026-09-10T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			storeRoot := t.TempDir()
			writeRunRecord(t, storeRoot, "seed", body, time.Now())
			driver := &fakeDriver{}
			s := NewServer(testConfig(storeRoot, driver))
			_, err := s.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
			if err == nil || !strings.Contains(err.Error(), "SONNET_RUN_STORE_INVALID") {
				t.Fatalf("start error=%v, want SONNET_RUN_STORE_INVALID", err)
			}
			driver.mu.Lock()
			calls := driver.calls
			driver.mu.Unlock()
			if calls != 0 {
				t.Fatalf("invalid store invoked driver %d times", calls)
			}
		})
	}
}

func TestRunStoreSweepRemovesOnlyExpiredOwnedTerminalOrAbandonedRuns(t *testing.T) {
	storeRoot := t.TempDir()
	runs := filepath.Join(storeRoot, "runs")
	expired := time.Now().Add(-25 * time.Hour)
	writeRunRecord(t, storeRoot, "expired-terminal", `{"schema_version":"sonnet-run-v1","run_id":"expired-terminal","generation":1,"state":"completed","phase":"settled","updated_at":"2026-09-09T00:00:00Z"}`, expired)
	writeRunRecord(t, storeRoot, "expired-abandoned", `{"schema_version":"sonnet-run-v1","run_id":"expired-abandoned","generation":1,"state":"host_lost","phase":"settled","updated_at":"2026-09-09T00:00:00Z"}`, expired)
	writeRunRecord(t, storeRoot, "unexpired-terminal", `{"schema_version":"sonnet-run-v1","run_id":"unexpired-terminal","generation":1,"state":"completed","phase":"settled","updated_at":"2026-09-10T00:00:00Z"}`, time.Now())
	writeRunRecord(t, storeRoot, "active-terminal", `{"schema_version":"sonnet-run-v1","run_id":"active-terminal","generation":1,"state":"completed","phase":"settled","updated_at":"2026-09-09T00:00:00Z"}`, expired)
	clearActive, err := hubtemp.MarkActive(filepath.Join(runs, "active-terminal"))
	if err != nil {
		t.Fatal(err)
	}
	defer clearActive()
	foreign := filepath.Join(runs, "foreign-expired")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(foreign, expired, expired); err != nil {
		t.Fatal(err)
	}
	sourceRoot := filepath.Join(storeRoot, "sources")
	if err := os.MkdirAll(sourceRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	s := NewServer(testConfig(storeRoot, &fakeDriver{}))
	defer s.Close(context.Background())
	for _, removed := range []string{"expired-terminal", "expired-abandoned"} {
		if _, err := os.Stat(filepath.Join(runs, removed)); !os.IsNotExist(err) {
			t.Fatalf("expired owned run %q retained: %v", removed, err)
		}
	}
	for _, kept := range []string{"unexpired-terminal", "active-terminal", "foreign-expired"} {
		if _, err := os.Stat(filepath.Join(runs, kept)); err != nil {
			t.Fatalf("protected run %q removed: %v", kept, err)
		}
	}
	if _, err := os.Stat(sourceRoot); err != nil {
		t.Fatalf("source root removed: %v", err)
	}
}

func TestCloseRetainsLeaseUntilStubbornWorkerSettles(t *testing.T) {
	projectRoot := writeProject(t)
	storeRoot := t.TempDir()
	driver := &stubbornDriver{started: make(chan struct{}), release: make(chan struct{})}
	first := NewServer(testConfig(storeRoot, driver))
	started, err := first.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	<-driver.started
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := first.Close(cancelled); err == nil || !strings.Contains(err.Error(), "SONNET_SHUTDOWN_INCOMPLETE") {
		t.Fatalf("Close error=%v, want SONNET_SHUTDOWN_INCOMPLETE", err)
	}
	second := NewServer(testConfig(storeRoot, &fakeDriver{}))
	if _, err := second.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10}); err == nil || !strings.Contains(err.Error(), "SONNET_RUN_STORE_OWNED") {
		t.Fatalf("second owner error=%v, want retained lease refusal", err)
	}
	close(driver.release)
	waitTerminal(t, first, started.RunID)
	eventually(t, time.Second, func() bool {
		next := NewServer(testConfig(storeRoot, &fakeDriver{}))
		_, err := next.Start(context.Background(), StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10})
		if err != nil {
			return false
		}
		_ = next.Close(context.Background())
		return true
	})
}

func TestCloseDuringPreparationRejectsLateStartAndRetainsLease(t *testing.T) {
	for _, scenario := range []string{"final admission", "pre-copy discovery", "post-copy replay", "cleanup cause", "cleanup wire projection", "already closed"} {
		t.Run(scenario, func(t *testing.T) {
			storeRoot := t.TempDir()
			projectRoot := writeProject(t)
			projectFile := filepath.Join(projectRoot, "sample.son")
			original, err := os.ReadFile(projectFile)
			if err != nil {
				t.Fatal(err)
			}
			driver := &fakeDriver{}
			s := NewServer(testConfig(storeRoot, driver))
			if s.initErr != nil {
				t.Fatal(s.initErr)
			}
			request := StartRequest{ProjectRoot: projectRoot, ProjectFile: "sample.son", TimeoutSeconds: 10}
			if scenario == "pre-copy discovery" || scenario == "post-copy replay" {
				request.IdempotencyKey = "same-request"
			}
			seed := func() {
				started, err := s.Start(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				s.mu.Lock()
				done := s.runs[started.RunID].done
				s.mu.Unlock()
				<-done
			}
			arrived, release := make(chan struct{}), make(chan struct{})
			cleanupArrived, cleanupRelease := make(chan struct{}), make(chan struct{})
			var releaseOnce, cleanupReleaseOnce sync.Once
			var observationMu sync.Mutex
			copies, writes, marks := 0, 0, 0
			var cleanupPath string
			cleanupCause := errors.New("injected closed-owner cleanup failure")
			cleanupFails := scenario == "cleanup cause" || scenario == "cleanup wire projection"
			s.snapshotWriter = func(path string, body []byte) error {
				observationMu.Lock()
				writes++
				observationMu.Unlock()
				return durableWrite(path, body)
			}
			s.markActive = func(work string) (func(), error) {
				observationMu.Lock()
				marks++
				observationMu.Unlock()
				return hubtemp.MarkActive(work)
			}
			s.removeWork = func(work string) error {
				cleanupPath = work
				if scenario == "final admission" {
					close(cleanupArrived)
					<-cleanupRelease
				}
				if cleanupFails {
					return cleanupCause
				}
				return os.RemoveAll(work)
			}
			if scenario == "pre-copy discovery" {
				s.locator = func() (Product, error) {
					close(arrived)
					<-release
					return Product{Available: true, Path: "fake-em"}, nil
				}
			} else {
				s.afterCopy = func() {
					observationMu.Lock()
					copies++
					first := copies == 1
					observationMu.Unlock()
					if first {
						close(arrived)
						<-release
					}
				}
			}
			type startOutcome struct {
				concurrentStartResult
				wire *mcp.CallToolResult
			}
			finished := make(chan startOutcome, 1)
			joined := false
			var successor *Server
			defer func() {
				releaseOnce.Do(func() { close(release) })
				cleanupReleaseOnce.Do(func() { close(cleanupRelease) })
				if !joined {
					<-finished
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := s.Close(ctx); err != nil {
					t.Errorf("owner cleanup: %v", err)
				}
				if successor != nil {
					if err := successor.Close(ctx); err != nil {
						t.Errorf("successor cleanup: %v", err)
					}
				}
			}()
			if scenario == "already closed" {
				if err := s.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			go func() {
				if scenario == "cleanup wire projection" {
					body, err := json.Marshal(request)
					if err != nil {
						finished <- startOutcome{concurrentStartResult: concurrentStartResult{err: err}}
						return
					}
					wire, err := s.startTool(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: body}})
					finished <- startOutcome{concurrentStartResult: concurrentStartResult{err: err}, wire: wire}
					return
				}
				response, err := s.Start(context.Background(), request)
				finished <- startOutcome{concurrentStartResult: concurrentStartResult{response: response, err: err}}
			}()
			var candidate string
			if scenario != "already closed" {
				<-arrived
				if scenario != "pre-copy discovery" {
					entries, err := os.ReadDir(filepath.Join(storeRoot, "runs"))
					if err != nil || len(entries) != 1 {
						t.Fatalf("held candidates=%d err=%v", len(entries), err)
					}
					candidate = filepath.Join(storeRoot, "runs", entries[0].Name())
				}
				if scenario == "post-copy replay" {
					seed()
				}
			}
			s.mu.Lock()
			registeredBefore := len(s.runs)
			s.mu.Unlock()
			observationMu.Lock()
			writesBefore, marksBefore := writes, marks
			observationMu.Unlock()
			driver.mu.Lock()
			callsBefore := driver.calls
			driver.mu.Unlock()
			if scenario != "already closed" {
				closing, cancel := context.WithCancel(context.Background())
				cancel()
				closeErr := s.Close(closing)
				if !errors.Is(closeErr, ErrShutdownIncomplete) {
					t.Errorf("held preparation Close=%v, want SONNET_SHUTDOWN_INCOMPLETE", closeErr)
				}
				successor = NewServer(testConfig(storeRoot, &fakeDriver{}))
				if !errors.Is(successor.initErr, ErrRunStoreOwned) {
					t.Errorf("held preparation successor=%v, want SONNET_RUN_STORE_OWNED", successor.initErr)
				}
			}
			releaseOnce.Do(func() { close(release) })
			var result startOutcome
			select {
			case <-cleanupArrived:
				probe := NewServer(testConfig(storeRoot, &fakeDriver{}))
				if !errors.Is(probe.initErr, ErrRunStoreOwned) {
					t.Errorf("cleanup-in-progress successor=%v, want retained custody", probe.initErr)
				}
				if err := probe.Close(context.Background()); err != nil {
					t.Error(err)
				}
				cleanupReleaseOnce.Do(func() { close(cleanupRelease) })
				result = <-finished
			case result = <-finished:
			}
			joined = true
			if scenario == "cleanup wire projection" {
				if result.err != nil || result.wire == nil || !result.wire.IsError || failureID(t, result.wire) != "SONNET_RUN_OWNER_CLOSED" {
					t.Errorf("closed wire=%+v err=%v, want exact SONNET_RUN_OWNER_CLOSED", result.wire, result.err)
				}
			} else {
				if result.err == nil || stableFailureID(result.err) != "SONNET_RUN_OWNER_CLOSED" || result.response.RunID != "" {
					t.Errorf("closed Start=%+v err=%v, want exact SONNET_RUN_OWNER_CLOSED and no run", result.response, result.err)
				}
				if cleanupFails && (!errors.Is(result.err, cleanupCause) || !strings.Contains(result.err.Error(), "SONNET_WORKSPACE_CLEANUP_FAILED")) {
					t.Errorf("closed cleanup error=%v, want retained cleanup cause", result.err)
				}
			}
			if err := s.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if successor != nil {
				if err := successor.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			observationMu.Lock()
			lateWrites, lateMarks := writes-writesBefore, marks-marksBefore
			observationMu.Unlock()
			driver.mu.Lock()
			lateCalls := driver.calls - callsBefore
			driver.mu.Unlock()
			s.mu.Lock()
			lateRuns, leaseHeld := len(s.runs)-registeredBefore, s.lease != nil
			s.mu.Unlock()
			if lateRuns != 0 || lateWrites != 0 || lateMarks != 0 || lateCalls != 0 || leaseHeld {
				t.Errorf("late runs/writes/markers/calls=%d/%d/%d/%d lease=%t, want all zero/false", lateRuns, lateWrites, lateMarks, lateCalls, leaseHeld)
			}
			if candidate != "" {
				if _, found, err := loadRunRecord(candidate); err != nil || found {
					t.Errorf("closed candidate persisted=%t err=%v, want no receipt", found, err)
				}
				if cleanupPath != candidate {
					t.Errorf("cleanup path=%q, want owned candidate %q", cleanupPath, candidate)
				}
				_, statErr := os.Stat(candidate)
				if cleanupFails && statErr != nil || !cleanupFails && !os.IsNotExist(statErr) {
					t.Errorf("candidate cleanup result=%v failureInjected=%t", statErr, cleanupFails)
				}
			} else if cleanupPath != "" {
				t.Errorf("no-copy rejection removed workspace %q", cleanupPath)
			}
			successor = NewServer(testConfig(storeRoot, &fakeDriver{}))
			if successor.initErr != nil {
				t.Fatalf("settled successor acquisition: %v", successor.initErr)
			}
			if body, err := os.ReadFile(projectFile); err != nil || sha256.Sum256(body) != sha256.Sum256(original) {
				t.Errorf("original project changed: %v", err)
			}
			t.Logf("settled: late runs/writes/markers/calls=%d/%d/%d/%d; candidate cleanup failure=%t; successor acquired", lateRuns, lateWrites, lateMarks, lateCalls, cleanupFails)
		})
	}
}

func testConfig(root string, driver Driver) Config {
	return Config{RunRoot: root, Locator: func() (Product, error) { return Product{Available: true, Path: "fake-em"}, nil }, Driver: driver}
}

func writeProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "sample.son"), []byte(admittedTestSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeRunRecord(t *testing.T, storeRoot, runID, body string, modified time.Time) {
	t.Helper()
	dir := filepath.Join(storeRoot, "runs", runID, "control")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "00000001.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Dir(dir), modified, modified); err != nil {
		t.Fatal(err)
	}
}

func waitTerminal(t *testing.T, s *Server, runID string) {
	t.Helper()
	eventually(t, time.Second, func() bool {
		status, err := s.Status(runID)
		return err == nil && terminal(status.State)
	})
}

func eventually(t *testing.T, limit time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func wireClient(t *testing.T, s *Server) *mcp.ClientSession {
	t.Helper()
	s.server = mcp.NewServer(&mcp.Implementation{Name: "sonnet-test-server", Version: "1"}, nil)
	s.register()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := s.server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "sonnet-test-client", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]bool, len(got))
	for _, value := range got {
		seen[value] = true
	}
	for _, value := range want {
		if !seen[value] {
			return false
		}
	}
	return true
}

func failureID(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("error content=%v, want one structured failure", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("error content type=%T, want text", result.Content[0])
	}
	var body struct {
		FailureID string `json:"failure_id"`
	}
	if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
		t.Fatal(err)
	}
	return body.FailureID
}

func resultJSON(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("result content=%v, want one JSON response", result.Content)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("result content type=%T, want text", result.Content[0])
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(text.Text), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

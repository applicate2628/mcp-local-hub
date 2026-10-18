// Package sonnet owns the BASIC copied-project Sonnet MCP lifecycle.
package sonnet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/hubtemp"
	"mcp-local-hub/internal/process"
)

const (
	maxTimeoutSeconds = 86400
	runStoreSchemaV1  = "sonnet-run-v1"
	runStoreRetention = 24 * time.Hour
	activeMarkerLeaf  = ".mcp-local-hub-active"
)

var (
	ErrIdempotencyConflict = errors.New("SONNET_IDEMPOTENCY_CONFLICT")
	ErrResultPending      = errors.New("SONNET_RESULT_PENDING")
	ErrForceRequired      = errors.New("SONNET_FORCE_REQUIRED")
	ErrUnknownRun         = errors.New("SONNET_RUN_NOT_FOUND")
	ErrRunStoreOwned      = errors.New("SONNET_RUN_STORE_OWNED")
	ErrRunStoreInvalid    = errors.New("SONNET_RUN_STORE_INVALID")
	ErrShutdownIncomplete = errors.New("SONNET_SHUTDOWN_INCOMPLETE")
)

type State string

const (
	StatePrepared         State = "prepared"
	StateRunning          State = "running"
	StateCompleted        State = "completed"
	StateCompletedNonzero State = "completed_nonzero"
	StateTimedOut         State = "timed_out"
	StateForceCancelled   State = "force_cancelled"
	StateFailed           State = "failed"
	StateHostLost         State = "host_lost"
)

type Product struct {
	Available       bool   `json:"available"`
	Path            string `json:"resolved_em_path"`
	Version         string `json:"product_version,omitempty"`
	SHA256          string `json:"em_sha256,omitempty"`
	DiscoverySource string `json:"discovery_source,omitempty"`
}
type DriverResult struct {
	ExitCode       int
	Stdout, Stderr []byte
	TimedOut       bool
}
type Driver interface {
	Run(context.Context, string, string) (DriverResult, error)
}
type Config struct {
	RunRoot, ConfiguredRoot string
	Locator                 func() (Product, error)
	Driver                  Driver
	LookupPath              func(string) (string, error)
	RegistryRoots           func() (map[string]string, error)
}
type StartRequest struct {
	ProjectRoot    string `json:"project_root"`
	ProjectFile    string `json:"project_file"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}
type StartResponse struct {
	RunID                string `json:"run_id"`
	State                State  `json:"state"`
	RequestDisposition   string `json:"request_disposition"`
	SourceManifestSHA256 string `json:"source_manifest_sha256"`
	CopiedProjectSHA256  string `json:"copied_project_sha256"`
}
type runIDRequest struct {
	RunID string `json:"run_id"`
}
type cancelToolRequest struct {
	RunID       string `json:"run_id"`
	OperationID string `json:"operation_id"`
	Force       *bool  `json:"force"`
}
type StatusResponse struct {
	RunID        string `json:"run_id"`
	Generation   int    `json:"generation"`
	State        State  `json:"state"`
	Phase        string `json:"phase"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	TimedOut     bool   `json:"timed_out"`
	Cancellation string `json:"cancellation"`
	FailureID    string `json:"failure_id,omitempty"`
	Error        string `json:"error,omitempty"`
	Reportable   bool   `json:"reportable"`
	UpdatedAt    string `json:"updated_at"`
}
type ResultResponse struct {
	StatusResponse
	StdoutBytes    int                  `json:"stdout_bytes"`
	StderrBytes    int                  `json:"stderr_bytes"`
	Stdout         string               `json:"stdout,omitempty"`
	Stderr         string               `json:"stderr,omitempty"`
	OutputManifest SonnetOutputResultV1 `json:"output_manifest"`
	RunSummary     SonnetRunSummaryV1   `json:"run_summary"`
}

type SonnetResolvedProductV1 struct {
	Version         string `json:"version,omitempty"`
	SHA256          string `json:"sha256"`
	DiscoverySource string `json:"discovery_source"`
}

type SonnetRunSummaryV1 struct {
	State       string `json:"state"`
	Frequencies *int   `json:"frequencies,omitempty"`
	Errors      *int   `json:"errors,omitempty"`
	Warnings    *int   `json:"warnings,omitempty"`
}

type SonnetPathAdmissionV1 struct {
	RuleVersion         string   `json:"rule_version"`
	ProjectFile         string   `json:"project_file"`
	CopiedProjectSHA256 string   `json:"copied_project_sha256"`
	OutputFolder        string   `json:"output_folder"`
	References          []string `json:"references"`
	VendorWrites        string   `json:"vendor_writes"`
}

type record struct {
	SchemaVersion string `json:"schema_version"`
	StatusResponse
	RequestHash, SourceManifestSHA256, CopiedProjectSHA256 string
	IdempotencyKey                                         string `json:"idempotency_key,omitempty"`
	Stdout, Stderr                                         string
	StdoutBytes, StderrBytes                               int
	Baseline                                               map[string]OutputFile `json:"-"`
	OutputManifest                                         SonnetOutputManifestV1
	PathAdmission                                          SonnetPathAdmissionV1     `json:"path_admission"`
	ResolvedProduct                                        SonnetResolvedProductV1   `json:"resolved_product"`
	RunSummary                                             SonnetRunSummaryV1        `json:"run_summary"`
	workspace                                              string                    `json:"-"`
	ProjectFile                                            string                    `json:"-"`
	cancel                                                 context.CancelFunc        `json:"-"`
	done                                                   chan struct{}             `json:"-"`
	operations                                             map[string]StatusResponse `json:"-"`
	clearActive                                            func()                    `json:"-"`
}
type Server struct {
	server         *mcp.Server
	root           string
	locator        func() (Product, error)
	driver         Driver
	mu             sync.Mutex
	next           int
	runs           map[string]*record
	keys           map[string]string
	lease          *flock.Flock
	initErr        error
	closed         bool
	workers        int
	workersChanged chan struct{}
	snapshotWriter func(string, []byte) error
	snapshotOutput func(string) (map[string]OutputFile, error)
	markActive     func(string) (func(), error)
	removeWork     func(string) error
	afterCopy      func()
	beforeOutput   func() // test barrier; set before starting workers
}

func NewServer(c Config) *Server {
	root := c.RunRoot
	if root == "" {
		root, _ = hubtemp.Dir("sonnet")
	}
	if c.Locator == nil {
		c.Locator = productLocator(c)
	}
	s := &Server{root: root, locator: c.Locator, driver: c.Driver, runs: map[string]*record{}, keys: map[string]string{}, workersChanged: make(chan struct{}), snapshotWriter: durableWrite}
	s.initStore()
	return s
}

func (s *Server) initStore() {
	if s.root == "" {
		s.initErr = fmt.Errorf("%w: no private run root", ErrRunStoreInvalid)
		return
	}
	if err := hubtemp.EnsurePrivateDir(s.root); err != nil {
		s.initErr = fmt.Errorf("%w: prepare private run root: %v", ErrRunStoreInvalid, err)
		return
	}
	lease := flock.New(filepath.Join(s.root, "sonnet-run-store-v1.lock"))
	locked, err := lease.TryLock()
	if err != nil || !locked {
		if err != nil {
			s.initErr = fmt.Errorf("%w: %v", ErrRunStoreOwned, err)
		} else {
			s.initErr = ErrRunStoreOwned
		}
		return
	}
	s.lease = lease
	s.sweepExpiredRuns()
	if err := s.load(); err != nil {
		_ = lease.Unlock()
		s.lease = nil
		s.initErr = fmt.Errorf("%w: %v", ErrRunStoreInvalid, err)
	}
}
func productLocator(c Config) func() (Product, error) {
	return func() (Product, error) {
		if c.ConfiguredRoot != "" {
			return productAtRoot(c.ConfiguredRoot, "configured_root")
		}
		roots := c.RegistryRoots
		if roots == nil {
			roots = defaultRegistryRoots
		}
		if values, err := roots(); err == nil {
			keys := make([]string, 0, len(values))
			for k := range values {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			valid := []Product{}
			for _, k := range keys {
				if p, e := productAtRoot(values[k], "vendor_registry"); e == nil {
					valid = append(valid, p)
				}
			}
			if len(valid) == 1 {
				return valid[0], nil
			}
			if len(valid) > 1 {
				return Product{DiscoverySource: "vendor_registry"}, errors.New("SONNET_PRODUCT_AMBIGUOUS")
			}
		}
		look := c.LookupPath
		if look == nil {
			look = exec.LookPath
		}
		p, e := look("em.exe")
		if e != nil {
			return Product{DiscoverySource: "PATH"}, e
		}
		return productAtExecutable(p, "PATH")
	}
}
func productAtRoot(root, source string) (Product, error) {
	return productAtExecutable(filepath.Join(root, "bin", "em.exe"), source)
}
func productAtExecutable(path, source string) (Product, error) {
	info, e := os.Stat(path)
	if e != nil || !info.Mode().IsRegular() {
		return Product{}, errors.New("SONNET_PRODUCT_NOT_FOUND")
	}
	f, e := os.Open(path)
	if e != nil {
		return Product{}, e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return Product{}, e
	}
	return Product{Available: true, Path: path, SHA256: fmt.Sprintf("%x", h.Sum(nil)), DiscoverySource: source}, nil
}

// ProductInfo resolves only the injected product locator. It launches no
// Sonnet command and does not inspect license, GUI, or status executables.
func (s *Server) ProductInfo() (Product, error) {
	s.mu.Lock()
	if s.initErr != nil {
		err := s.initErr
		s.mu.Unlock()
		return Product{}, err
	}
	locator := s.locator
	s.mu.Unlock()
	return locator()
}

func (s *Server) Start(ctx context.Context, req StartRequest) (StartResponse, error) {
	s.mu.Lock()
	if s.initErr != nil {
		err := s.initErr
		s.mu.Unlock()
		return StartResponse{}, err
	}
	if s.closed {
		s.mu.Unlock()
		return StartResponse{}, errors.New("SONNET_RUN_OWNER_CLOSED")
	}
	s.mu.Unlock()
	if req.TimeoutSeconds <= 0 || req.TimeoutSeconds > maxTimeoutSeconds {
		return StartResponse{}, fmt.Errorf("SONNET_PROJECT_INVALID: timeout_seconds must be 1..%d", maxTimeoutSeconds)
	}
	requestHash, err := startRequestHash(req)
	if err != nil {
		return StartResponse{}, err
	}
	s.mu.Lock()
	if req.IdempotencyKey != "" {
		if id := s.keys[req.IdempotencyKey]; id != "" {
			r := s.runs[id]
			if r.RequestHash != requestHash {
				s.mu.Unlock()
				return StartResponse{}, ErrIdempotencyConflict
			}
			out := startFrom(r)
			out.RequestDisposition = "replayed"
			s.mu.Unlock()
			return out, nil
		}
	}
	s.mu.Unlock()
	// Durable replays depend on their recorded request, not current product discovery.
	p, err := s.locator()
	if err != nil {
		if stableFailureID(err) != "SONNET_INTERNAL_ERROR" {
			return StartResponse{}, err
		}
		return StartResponse{}, fmt.Errorf("SONNET_PRODUCT_NOT_FOUND: %w", err)
	}
	if !p.Available || p.Path == "" {
		return StartResponse{}, errors.New("SONNET_PRODUCT_NOT_FOUND")
	}
	work, sourceHash, copiedHash, admission, err := s.copyProject(req.ProjectRoot, req.ProjectFile)
	if err != nil {
		return StartResponse{}, err
	}
	snapshot := s.snapshotOutput
	if snapshot == nil {
		snapshot = snapshotOutput
	}
	baseline, err := snapshot(filepath.Join(work, "work"))
	if err != nil {
		return StartResponse{}, s.cleanupPreTransferWork(work, err)
	}
	if s.afterCopy != nil {
		s.afterCopy()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Close may have released the store lease while the project was copied.
	// Revalidate admission under the same lock used for worker registration.
	if s.closed {
		return StartResponse{}, s.cleanupPreTransferWork(work, errors.New("SONNET_RUN_OWNER_CLOSED"))
	}
	if req.IdempotencyKey != "" {
		if id := s.keys[req.IdempotencyKey]; id != "" {
			r := s.runs[id]
			if r.RequestHash != requestHash {
				return StartResponse{}, s.cleanupPreTransferWork(work, ErrIdempotencyConflict)
			}
			out := startFrom(r)
			out.RequestDisposition = "replayed"
			if err := s.cleanupPreTransferWork(work, nil); err != nil {
				return StartResponse{}, err
			}
			return out, nil
		}
	}
	s.next++
	id := fmt.Sprintf("sonnet-%06d", s.next)
	workerCtx, cancel := context.WithTimeout(context.Background(), time.Duration(req.TimeoutSeconds)*time.Second)
	markActive := s.markActive
	if markActive == nil {
		markActive = hubtemp.MarkActive
	}
	clearActive, err := markActive(work)
	if err != nil {
		cancel()
		primary := fmt.Errorf("SONNET_PROJECT_COPY_FAILED: mark active workspace: %w", err)
		return StartResponse{}, s.cleanupPreTransferWork(work, primary)
	}
	r := &record{SchemaVersion: runStoreSchemaV1, RequestHash: requestHash, StatusResponse: StatusResponse{RunID: id, Generation: s.next, State: StatePrepared, Phase: "prepared", Reportable: false, UpdatedAt: now()}, SourceManifestSHA256: sourceHash, CopiedProjectSHA256: copiedHash, IdempotencyKey: req.IdempotencyKey, workspace: work, ProjectFile: filepath.ToSlash(filepath.Clean(req.ProjectFile)), Baseline: baseline, PathAdmission: admission, ResolvedProduct: resolvedProduct(p), RunSummary: SonnetRunSummaryV1{State: "unknown"}, cancel: cancel, done: make(chan struct{}), operations: map[string]StatusResponse{}, clearActive: clearActive}
	s.runs[id] = r
	if req.IdempotencyKey != "" {
		s.keys[req.IdempotencyKey] = id
	}
	if err := s.persist(r); err != nil {
		delete(s.runs, id)
		delete(s.keys, req.IdempotencyKey)
		clearActive()
		cancel()
		return StartResponse{}, fmt.Errorf("SONNET_RUN_STORE_INVALID: persist prepared record: %w", err)
	}
	s.workers++
	go s.run(workerCtx, r, p.Path)
	return startFrom(r), nil
}

func (s *Server) cleanupPreTransferWork(work string, primary error) error {
	runsRoot := filepath.Clean(filepath.Join(s.root, "runs"))
	cleanWork := filepath.Clean(work)
	leaf := filepath.Base(cleanWork)
	if filepath.Dir(cleanWork) != runsRoot || !strings.HasPrefix(leaf, "run-") || len(leaf) == len("run-") {
		cleanupErr := errors.New("SONNET_WORKSPACE_CLEANUP_FAILED: refuse non-owned pre-transfer workspace")
		if primary == nil {
			return cleanupErr
		}
		return errors.Join(primary, cleanupErr)
	}
	removeWork := s.removeWork
	if removeWork == nil {
		removeWork = os.RemoveAll
	}
	if cleanupErr := removeWork(work); cleanupErr != nil {
		cleanupErr = fmt.Errorf("SONNET_WORKSPACE_CLEANUP_FAILED: %w", cleanupErr)
		if primary == nil {
			return cleanupErr
		}
		return errors.Join(primary, cleanupErr)
	}
	return primary
}

// startRequestHash binds a key to normalized request arguments, not mutable
// source bytes. Legacy receipts without a digest cannot prove an exact replay.
func startRequestHash(req StartRequest) (string, error) {
	if req.ProjectRoot == "" || req.ProjectFile == "" || filepath.IsAbs(req.ProjectFile) ||
		strings.ToLower(filepath.Ext(req.ProjectFile)) != ".son" {
		return "", errors.New("SONNET_PROJECT_INVALID")
	}
	root, err := filepath.Abs(req.ProjectRoot)
	if err != nil {
		return "", fmt.Errorf("SONNET_PROJECT_INVALID: %w", err)
	}
	file := filepath.Clean(req.ProjectFile)
	if !within(root, filepath.Join(root, file)) {
		return "", errors.New("SONNET_PROJECT_INVALID")
	}
	// Windows project identity is case-insensitive, including the relative
	// project filename. Keep POSIX casing significant and do not alter the
	// actual paths used for copying or invoking the driver.
	rootKey, fileKey := filepath.ToSlash(root), filepath.ToSlash(file)
	if runtime.GOOS == "windows" {
		rootKey, fileKey = strings.ToLower(rootKey), strings.ToLower(fileKey)
	}
	payload, err := json.Marshal(struct {
		ProjectRoot    string
		ProjectFile    string
		TimeoutSeconds int
	}{rootKey, fileKey, req.TimeoutSeconds})
	if err != nil {
		return "", fmt.Errorf("SONNET_PROJECT_INVALID: %w", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(payload)), nil
}

func startFrom(r *record) StartResponse {
	return StartResponse{RunID: r.RunID, State: r.State, RequestDisposition: "started", SourceManifestSHA256: r.SourceManifestSHA256, CopiedProjectSHA256: r.CopiedProjectSHA256}
}
func (s *Server) run(ctx context.Context, r *record, em string) {
	defer func() {
		if r.clearActive != nil {
			r.clearActive()
		}
		s.mu.Lock()
		s.workers--
		s.signalWorkersChangedLocked()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			_ = s.releaseLeaseWhenWorkersSettled()
		}
	}()
	s.mu.Lock()
	r.State = StateRunning
	r.Phase = "running"
	r.Generation++
	r.UpdatedAt = now()
	if err := s.persist(r); err != nil {
		s.storeWriteFailureLocked(r, err)
		close(r.done)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	out, err := s.driver.Run(ctx, em, filepath.Join(r.workspace, "work", filepath.FromSlash(r.ProjectFile)))
	// Workspace, project file and baseline are immutable after admission.
	// Keep the worker/lease alive, but do not block status, cancellation or
	// shutdown admission while hashing potentially large output trees.
	if s.beforeOutput != nil {
		s.beforeOutput()
	}
	manifest, manifestErr := compareOutput(filepath.Join(r.workspace, "work"), r.Baseline)
	summary := summarizeRunLog(filepath.Join(r.workspace, "work"), r.ProjectFile)
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Stdout = string(out.Stdout)
	r.Stderr = string(out.Stderr)
	r.StdoutBytes = len(out.Stdout)
	r.StderrBytes = len(out.Stderr)
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			r.State = StateTimedOut
			r.TimedOut = true
			r.FailureID = "SONNET_RUN_TIMED_OUT"
		} else {
			r.State = StateForceCancelled
			r.FailureID = "SONNET_FORCE_CANCELLED"
		}
		r.Cancellation = "force"
	} else if err != nil {
		r.State = StateFailed
		var driverFailure *strictDriverFailure
		if errors.As(err, &driverFailure) && driverFailure.FailureID != "" {
			r.FailureID = driverFailure.FailureID
			if driverFailure.ExitCode != nil {
				v := *driverFailure.ExitCode
				r.ExitCode = &v
			}
		} else {
			r.FailureID = "SONNET_DRIVER_RESULT_INVALID"
		}
	} else if out.ExitCode != 0 {
		r.State = StateCompletedNonzero
		r.FailureID = "SONNET_EM_EXIT_NONZERO"
		v := out.ExitCode
		r.ExitCode = &v
	} else {
		r.State = StateCompleted
		v := 0
		r.ExitCode = &v
		r.Reportable = true
	}
	r.Phase = "settled"
	r.OutputManifest = manifest
	if manifestErr != nil || !manifest.Reportable {
		r.Reportable = false
		r.FailureID = "SONNET_OUTPUT_UNATTRIBUTABLE"
	}
	r.RunSummary = summary
	r.UpdatedAt = now()
	r.Generation++
	if err := s.persist(r); err != nil {
		s.storeWriteFailureLocked(r, err)
		close(r.done)
		return
	}
	if err := s.readbackTerminal(r); err != nil {
		s.storeWriteFailureLocked(r, err)
	}
	close(r.done)
}
func (s *Server) Status(id string) (StatusResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r == nil {
		return StatusResponse{}, ErrUnknownRun
	}
	return r.StatusResponse, nil
}
func (s *Server) Result(id string) (ResultResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r == nil {
		return ResultResponse{}, ErrUnknownRun
	}
	if !terminal(r.State) {
		return ResultResponse{}, ErrResultPending
	}
	if r.FailureID != "" && r.State == StateHostLost {
		return ResultResponse{}, errors.New(r.FailureID)
	}
	if r.FailureID != "" && r.State == StateFailed {
		return ResultResponse{}, errors.New(r.FailureID)
	}
	manifest, err := projectOutputManifest(r.OutputManifest)
	if err != nil {
		return ResultResponse{}, err
	}
	return ResultResponse{StatusResponse: r.StatusResponse, StdoutBytes: r.StdoutBytes, StderrBytes: r.StderrBytes, Stdout: r.Stdout, Stderr: r.Stderr, OutputManifest: manifest, RunSummary: r.RunSummary}, nil
}

func resolvedProduct(product Product) SonnetResolvedProductV1 {
	return SonnetResolvedProductV1{Version: product.Version, SHA256: product.SHA256, DiscoverySource: product.DiscoverySource}
}

var (
	postAnalysisFrequencies = regexp.MustCompile(`Total time for ([0-9]+) frequencies:`)
	postAnalysisErrors      = regexp.MustCompile(`Em errors detected: ([0-9]+)\s+Em warnings detected: ([0-9]+)`)
)

// summarizeRunLog parses only bounded, observed 18.56 post-analysis counters.
// Missing or changed log shape is explicit unknown; raw log text remains owned
// scratch evidence and is never surfaced through the result wire contract.
func summarizeRunLog(workRoot, projectFile string) SonnetRunSummaryV1 {
	stem := strings.TrimSuffix(filepath.Base(projectFile), filepath.Ext(projectFile))
	path := filepath.Join(workRoot, "sondata", stem, "log_composite.log")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return SonnetRunSummaryV1{State: "unknown"}
	}
	f, err := os.Open(path)
	if err != nil {
		return SonnetRunSummaryV1{State: "unknown"}
	}
	body, err := io.ReadAll(io.LimitReader(f, 64<<10+1))
	_ = f.Close()
	if err != nil || len(body) > 64<<10 {
		return SonnetRunSummaryV1{State: "unknown"}
	}
	frequencies := postAnalysisFrequencies.FindStringSubmatch(string(body))
	errorsAndWarnings := postAnalysisErrors.FindStringSubmatch(string(body))
	if len(frequencies) != 2 || len(errorsAndWarnings) != 3 {
		return SonnetRunSummaryV1{State: "unknown"}
	}
	freq, freqErr := strconv.Atoi(frequencies[1])
	errs, errsErr := strconv.Atoi(errorsAndWarnings[1])
	warnings, warningsErr := strconv.Atoi(errorsAndWarnings[2])
	if freqErr != nil || errsErr != nil || warningsErr != nil {
		return SonnetRunSummaryV1{State: "unknown"}
	}
	return SonnetRunSummaryV1{State: "observed", Frequencies: &freq, Errors: &errs, Warnings: &warnings}
}
func (s *Server) Cancel(ctx context.Context, id, operation string, force bool) (StatusResponse, error) {
	if !force {
		return StatusResponse{}, ErrForceRequired
	}
	s.mu.Lock()
	r := s.runs[id]
	if r == nil {
		s.mu.Unlock()
		return StatusResponse{}, ErrUnknownRun
	}
	if terminal(r.State) {
		out := r.StatusResponse
		s.mu.Unlock()
		return out, nil
	}
	if operation == "" {
		s.mu.Unlock()
		return StatusResponse{}, ErrForceRequired
	}
	if prior, ok := r.operations[operation]; ok {
		s.mu.Unlock()
		return prior, nil
	}
	r.Phase = "force_cancel_requested"
	r.cancel()
	done := r.done
	s.mu.Unlock()
	select {
	case <-done:
		out, err := s.Status(id)
		if err == nil {
			s.mu.Lock()
			r.operations[operation] = out
			s.mu.Unlock()
		}
		return out, err
	case <-ctx.Done():
		return StatusResponse{}, fmt.Errorf("SONNET_FORCE_CANCEL_FAILED: %w", ctx.Err())
	}
}
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.initErr != nil {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for _, r := range s.runs {
		if !terminal(r.State) {
			r.Phase = "force_cancel_requested"
			r.cancel()
		}
	}
	s.mu.Unlock()
	if err := s.waitWorkers(ctx); err != nil {
		return err
	}
	return s.releaseLeaseWhenWorkersSettled()
}

func (s *Server) waitWorkers(ctx context.Context) error {
	for {
		s.mu.Lock()
		if s.workers == 0 {
			s.mu.Unlock()
			return nil
		}
		changed := s.workersChanged
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrShutdownIncomplete, ctx.Err())
		}
	}
}

func (s *Server) signalWorkersChangedLocked() {
	close(s.workersChanged)
	s.workersChanged = make(chan struct{})
}
func terminal(v State) bool {
	return v == StateCompleted || v == StateCompletedNonzero || v == StateTimedOut || v == StateForceCancelled || v == StateFailed || v == StateHostLost
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
func (s *Server) persist(r *record) error {
	if err := os.MkdirAll(filepath.Join(r.workspace, "control"), 0o700); err != nil {
		return err
	}
	r.SchemaVersion = runStoreSchemaV1
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	writer := s.snapshotWriter
	if writer == nil {
		writer = durableWrite
	}
	return writer(filepath.Join(r.workspace, "control", snapshotName(r.Generation)), b)
}

func (s *Server) storeWriteFailureLocked(r *record, cause error) {
	r.State = StateFailed
	r.Phase = "failed"
	r.FailureID = "SONNET_RUN_STORE_WRITE_FAILED"
	r.Error = cause.Error()
	r.Reportable = false
	r.UpdatedAt = now()
	r.Generation++
	if err := s.persist(r); err != nil {
		r.Error = errors.Join(cause, err).Error()
	}
}

func (s *Server) readbackTerminal(r *record) error {
	stored, found, err := loadRunRecord(r.workspace)
	if err != nil {
		return err
	}
	if !found || stored.Generation != r.Generation || stored.State != r.State || stored.FailureID != r.FailureID || stored.Reportable != r.Reportable {
		return errors.New("terminal snapshot readback mismatch")
	}
	return nil
}

// load admits only records written by this package's current schema. It never
// inspects or acts on a PID: acquiring this process-local store lease proves
// only that no prior Sonnet owner remains to settle an in-process worker.
func (s *Server) load() error {
	runsRoot := filepath.Join(s.root, "runs")
	entries, err := os.ReadDir(runsRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		workspace := filepath.Join(runsRoot, entry.Name())
		candidate, found, err := loadRunRecord(workspace)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		recovered := !terminal(candidate.State)
		if recovered {
			candidate.State = StateHostLost
			candidate.Phase = "settled"
			candidate.FailureID = "SONNET_HOST_LOST"
			candidate.Reportable = false
			candidate.Generation++
			candidate.UpdatedAt = now()
		}
		candidate.workspace = workspace
		candidate.done = make(chan struct{})
		close(candidate.done)
		candidate.operations = map[string]StatusResponse{}
		s.runs[candidate.RunID] = candidate
		if candidate.IdempotencyKey != "" {
			s.keys[candidate.IdempotencyKey] = candidate.RunID
		}
		if candidate.Generation > s.next {
			s.next = candidate.Generation
		}
		if recovered {
			if err := s.persist(candidate); err != nil {
				return err
			}
		}
	}
	return nil
}

func loadRunRecord(workspace string) (*record, bool, error) {
	control := filepath.Join(workspace, "control")
	entries, err := os.ReadDir(control)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var path string
	var generation int
	for _, entry := range entries {
		g, ok := parseSnapshotName(entry.Name())
		if !ok || g <= generation {
			continue
		}
		path = filepath.Join(control, entry.Name())
		generation = g
	}
	if path == "" {
		return nil, false, nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 2<<20 {
		return nil, true, errors.New("record is not a bounded regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, true, err
	}
	var out record
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, true, err
	}
	if out.SchemaVersion != runStoreSchemaV1 || out.RunID == "" || out.Generation != generation || out.UpdatedAt == "" {
		return nil, true, errors.New("record schema or identity is invalid")
	}
	if !knownState(out.State) {
		return nil, true, errors.New("record state is invalid")
	}
	return &out, true, nil
}

func snapshotName(generation int) string { return fmt.Sprintf("%08d.json", generation) }

func parseSnapshotName(name string) (int, bool) {
	if len(name) != len("00000000.json") || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	generation, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
	return generation, err == nil && generation > 0
}

func knownState(v State) bool {
	return v == StatePrepared || v == StateRunning || terminal(v)
}

func (s *Server) sweepExpiredRuns() {
	// Startup cleanup is valid only after acquiring exclusive store ownership.
	if s.lease == nil {
		return
	}
	runsRoot := filepath.Join(s.root, "runs")
	entries, err := os.ReadDir(runsRoot)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-runStoreRetention)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		workspace := filepath.Join(runsRoot, entry.Name())
		r, found, err := loadRunRecord(workspace)
		if err != nil {
			continue
		}
		if !found {
			if expiredRecordlessWorkspace(workspace, entry.Name(), cutoff) {
				_ = os.RemoveAll(workspace)
			}
			continue
		}
		if !terminal(r.State) {
			continue
		}
		if marker, err := os.Lstat(filepath.Join(workspace, activeMarkerLeaf)); err == nil && marker.ModTime().After(cutoff) {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(workspace)
	}
}

// MkdirTemp(base, "run-") owns only this numeric namespace. Inspect metadata,
// not contents, and preserve anything recent, irregular or unreadable. A recent
// copied file/control temporary/active marker must protect an otherwise old root.
func expiredRecordlessWorkspace(workspace, name string, cutoff time.Time) bool {
	suffix := strings.TrimPrefix(name, "run-")
	if suffix == name || suffix == "" {
		return false
	}
	for _, ch := range suffix {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	err := filepath.WalkDir(workspace, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if (!info.IsDir() && !info.Mode().IsRegular()) || !info.ModTime().Before(cutoff) {
			return errors.New("recordless workspace is not safely expired")
		}
		return nil
	})
	return err == nil
}

func (s *Server) releaseLeaseWhenWorkersSettled() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed || s.workers != 0 || s.lease == nil {
		return nil
	}
	if err := s.lease.Unlock(); err != nil {
		return fmt.Errorf("SONNET_RUN_STORE_RELEASE_FAILED: %w", err)
	}
	s.lease = nil
	return nil
}

// durableWrite mirrors the repository's established temp-file, Sync, and
// rename protocol for package-private lifecycle receipts.
func durableWrite(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".record-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
func (s *Server) copyProject(root, rel string) (string, string, string, SonnetPathAdmissionV1, error) {
	if root == "" || rel == "" || filepath.IsAbs(rel) || strings.ToLower(filepath.Ext(rel)) != ".son" {
		return "", "", "", SonnetPathAdmissionV1{}, errors.New("SONNET_PROJECT_INVALID")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", "", "", SonnetPathAdmissionV1{}, err
	}
	project := filepath.Join(abs, filepath.Clean(rel))
	if !within(abs, project) {
		return "", "", "", SonnetPathAdmissionV1{}, errors.New("SONNET_PROJECT_INVALID")
	}
	sourceManifest, err := snapshotOutput(abs)
	if err != nil {
		return "", "", "", SonnetPathAdmissionV1{}, fmt.Errorf("SONNET_PROJECT_INVALID: source manifest: %w", err)
	}
	sourceHash := canonicalManifestHash(sourceManifest)
	base := filepath.Join(s.root, "runs")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", "", "", SonnetPathAdmissionV1{}, err
	}
	work, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return "", "", "", SonnetPathAdmissionV1{}, err
	}
	count := 0
	var total int64
	err = filepath.WalkDir(abs, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("SONNET_PROJECT_INVALID")
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return errors.New("SONNET_PROJECT_INVALID")
		}
		count++
		if count > 256 {
			return errors.New("SONNET_PROJECT_INVALID")
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		total += info.Size()
		if info.Size() > 64<<20 || total > 512<<20 {
			return errors.New("SONNET_PROJECT_INVALID")
		}
		relative, e := filepath.Rel(abs, path)
		if e != nil {
			return e
		}
		dst := filepath.Join(work, "work", relative)
		if e = os.MkdirAll(filepath.Dir(dst), 0o700); e != nil {
			return e
		}
		in, e := os.Open(path)
		if e != nil {
			return e
		}
		out, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if e == nil {
			_, e = io.Copy(out, in)
		}
		ce := errors.Join(in.Close(), out.Close())
		return errors.Join(e, ce)
	})
	if err != nil {
		_ = os.RemoveAll(work)
		return "", "", "", SonnetPathAdmissionV1{}, fmt.Errorf("SONNET_PROJECT_COPY_FAILED: %w", err)
	}
	b, err := os.ReadFile(filepath.Join(work, "work", filepath.Clean(rel)))
	if err != nil {
		_ = os.RemoveAll(work)
		return "", "", "", SonnetPathAdmissionV1{}, fmt.Errorf("SONNET_PROJECT_COPY_MISMATCH: %w", err)
	}
	copiedHash := fmt.Sprintf("%x", sha256.Sum256(b))
	copiedManifest, err := snapshotOutput(filepath.Join(work, "work"))
	if err != nil || !sameManifest(sourceManifest, copiedManifest) {
		_ = os.RemoveAll(work)
		if err != nil {
			return "", "", "", SonnetPathAdmissionV1{}, fmt.Errorf("SONNET_PROJECT_COPY_MISMATCH: copied manifest: %w", err)
		}
		return "", "", "", SonnetPathAdmissionV1{}, errors.New("SONNET_PROJECT_COPY_MISMATCH: copied manifest differs from source")
	}
	admission, err := admitCopiedProject(filepath.Join(work, "work"), filepath.ToSlash(filepath.Clean(rel)), copiedHash)
	if err != nil {
		_ = os.RemoveAll(work)
		return "", "", "", SonnetPathAdmissionV1{}, fmt.Errorf("SONNET_PROJECT_INVALID: %w", err)
	}
	return work, sourceHash, copiedHash, admission, nil
}

// canonicalManifestHash is the source-hash contract persisted in a run record:
// each regular member contributes slash-relative path, byte length, and content
// SHA-256 in ordinal byte-string path order. It contains no absolute source root.
func canonicalManifestHash(files map[string]OutputFile) string {
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
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func sameManifest(left, right map[string]OutputFile) bool {
	if len(left) != len(right) {
		return false
	}
	for path, file := range left {
		other, ok := right[path]
		if !ok || file.Size != other.Size || file.SHA256 != other.SHA256 {
			return false
		}
	}
	return true
}

// admitCopiedProject owns BASIC's deliberately narrow file-output admission.
// External-reference syntax has not been verified, so this version supports no
// external references rather than guessing; vendor cache/temp writes remain
// unobserved outside the owned copied work tree.
func admitCopiedProject(workRoot, projectFile, copiedHash string) (SonnetPathAdmissionV1, error) {
	body, err := os.ReadFile(filepath.Join(workRoot, filepath.FromSlash(projectFile)))
	if err != nil {
		return SonnetPathAdmissionV1{}, err
	}
	inFileOut := false
	folders := 0
	outputFolder := ""
	for _, raw := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "TYPE" && fields[1] == "SPROJ" {
			return SonnetPathAdmissionV1{}, errors.New("unverified TYPE SPROJ reference")
		}
		switch line {
		case "FILEOUT":
			if inFileOut {
				return SonnetPathAdmissionV1{}, errors.New("duplicate FILEOUT section")
			}
			inFileOut = true
			continue
		case "END FILEOUT":
			if !inFileOut {
				return SonnetPathAdmissionV1{}, errors.New("unmatched END FILEOUT")
			}
			inFileOut = false
			continue
		}
		if !inFileOut || line == "" {
			continue
		}
		if len(fields) != 2 || fields[0] != "FOLDER" {
			return SonnetPathAdmissionV1{}, errors.New("unsupported FILEOUT semantics")
		}
		folders++
		if folders != 1 {
			return SonnetPathAdmissionV1{}, errors.New("duplicate FILEOUT FOLDER")
		}
		if !cleanOutputFolder(fields[1]) {
			return SonnetPathAdmissionV1{}, errors.New("unsafe FILEOUT FOLDER")
		}
		outputFolder = fields[1]
	}
	if inFileOut || folders != 1 {
		return SonnetPathAdmissionV1{}, errors.New("FILEOUT FOLDER is required and unambiguous")
	}
	return SonnetPathAdmissionV1{RuleVersion: "sonnet-path-admission-v1", ProjectFile: projectFile, CopiedProjectSHA256: copiedHash, OutputFolder: outputFolder, References: []string{}, VendorWrites: "unobserved"}, nil
}

func cleanOutputFolder(folder string) bool {
	if folder == "." {
		return true
	}
	if folder == "" || filepath.IsAbs(folder) || filepath.VolumeName(folder) != "" || strings.HasPrefix(folder, `\\`) || strings.ContainsAny(folder, "$%{}") || strings.Contains(folder, "//") || strings.Contains(folder, `\\`) {
		return false
	}
	for _, part := range strings.FieldsFunc(folder, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." || part == "" {
			return false
		}
	}
	clean := filepath.Clean(folder)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}
func within(root, path string) bool {
	r, e := filepath.Rel(root, path)
	return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

type strictDriver struct{}

func (strictDriver) Run(ctx context.Context, em, project string) (DriverResult, error) {
	cmd := exec.Command(em, project)
	cmd.Dir = filepath.Dir(project)
	out, err := process.RunStrictlyContained(ctx, process.StrictRunInvocation{Command: cmd, Input: []byte{}, InputLimit: 1, StdoutLimit: 512 << 10, StderrLimit: 512 << 10})
	exitCode, err := classifyStrictDriverResult(cmd, err)
	return DriverResult{ExitCode: exitCode, Stdout: out.Stdout.Prefix, Stderr: out.Stderr.Prefix}, err
}

type strictDriverFailure struct {
	FailureID string
	ExitCode  *int
	Cause     error
}

func (e *strictDriverFailure) Error() string {
	if e == nil || e.Cause == nil {
		return e.FailureID
	}
	return e.FailureID + ": " + e.Cause.Error()
}

func (e *strictDriverFailure) Unwrap() error { return e.Cause }

func classifyStrictDriverResult(cmd *exec.Cmd, runErr error) (int, error) {
	var exitErr *exec.ExitError
	hasExitError := errors.As(runErr, &exitErr)

	var observedExitCode *int
	if cmd != nil && cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		code := cmd.ProcessState.ExitCode()
		observedExitCode = &code
	}
	statesAgree := true
	if hasExitError {
		statesAgree = exitErr.ProcessState != nil && exitErr.ProcessState.Exited() && observedExitCode != nil && exitErr.ExitCode() == *observedExitCode
		if !statesAgree {
			observedExitCode = nil
		}
	}

	fail := func(id string, cause error) (int, error) {
		if cause == nil {
			cause = errors.New("strict runner returned an invalid result")
		}
		return 0, &strictDriverFailure{FailureID: id, ExitCode: observedExitCode, Cause: cause}
	}
	if runErr == nil {
		if observedExitCode != nil && *observedExitCode == 0 {
			return 0, nil
		}
		return fail("SONNET_DRIVER_RESULT_INVALID", nil)
	}

	var strictErr *process.StrictRunError
	if !errors.As(runErr, &strictErr) {
		return fail("SONNET_DRIVER_RESULT_INVALID", runErr)
	}
	switch strictErr.Kind {
	case process.StrictRunInvalidInvocation:
		return fail("SONNET_DRIVER_INVALID_INVOCATION", runErr)
	case process.StrictRunContainmentFailed:
		return fail("SONNET_PROCESS_CONTAINMENT_FAILED", runErr)
	case process.StrictRunTimeout:
		return fail("SONNET_DRIVER_RESULT_INVALID", runErr)
	case process.StrictRunExecutionFailed:
		var startErr *process.StartWithJobError
		if errors.As(runErr, &startErr) {
			switch startErr.Phase {
			case process.StartWithJobLaunch:
				return fail("SONNET_LAUNCH_FAILED", runErr)
			case process.StartWithJobContainment:
				return fail("SONNET_PROCESS_CONTAINMENT_FAILED", runErr)
			case process.StartWithJobInvalid:
				return fail("SONNET_DRIVER_INVALID_INVOCATION", runErr)
			default:
				return fail("SONNET_DRIVER_RESULT_INVALID", runErr)
			}
		}
		if hasExitError && statesAgree && observedExitCode != nil {
			leaves := strictDriverErrorLeaves(runErr)
			if len(leaves) == 1 {
				if onlyExit, ok := leaves[0].(*exec.ExitError); ok && onlyExit.ExitCode() == *observedExitCode && *observedExitCode != 0 {
					return *observedExitCode, nil
				}
			}
			if exitErr.ExitCode() == 0 {
				return fail("SONNET_DRIVER_RESULT_INVALID", runErr)
			}
		}
		if hasExitError && !statesAgree {
			return fail("SONNET_DRIVER_RESULT_INVALID", runErr)
		}
		if observedExitCode != nil {
			return fail("SONNET_DRIVER_CLEANUP_FAILED", runErr)
		}
		return fail("SONNET_DRIVER_IO_FAILED", runErr)
	default:
		return fail("SONNET_DRIVER_RESULT_INVALID", runErr)
	}
}

func strictDriverErrorLeaves(err error) []error {
	if err == nil {
		return nil
	}
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		var leaves []error
		for _, child := range many.Unwrap() {
			leaves = append(leaves, strictDriverErrorLeaves(child)...)
		}
		if len(leaves) != 0 {
			return leaves
		}
	}
	if one, ok := err.(interface{ Unwrap() error }); ok {
		if child := one.Unwrap(); child != nil {
			return strictDriverErrorLeaves(child)
		}
	}
	return []error{err}
}

func Run(ctx context.Context) error {
	root, ok := hubtemp.Dir("sonnet")
	if !ok {
		return errors.New("derive sonnet root")
	}
	s := NewServer(Config{RunRoot: root, Driver: strictDriver{}})
	if s.initErr != nil {
		return fmt.Errorf("initialize sonnet run owner: %w", s.initErr)
	}
	s.server = mcp.NewServer(&mcp.Implementation{Name: "mcp-local-hub-sonnet", Version: "1.0.0"}, nil)
	s.register()
	runErr := s.server.Run(ctx, &mcp.StdioTransport{})
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return errors.Join(runErr, s.Close(shutdownCtx))
}
func (s *Server) register() {
	add := func(n string, schema map[string]any, h mcp.ToolHandler) {
		s.server.AddTool(&mcp.Tool{Name: n, InputSchema: schema}, h)
	}
	add("sonnet_product_info", objectSchema(map[string]any{}, nil), s.productInfoTool)
	add("sonnet_run_ready_project", objectSchema(map[string]any{
		"project_root":    map[string]any{"type": "string", "minLength": 1},
		"project_file":    map[string]any{"type": "string", "minLength": 1},
		"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": maxTimeoutSeconds},
		"idempotency_key": map[string]any{"type": "string"},
	}, []string{"project_root", "project_file", "timeout_seconds"}), s.startTool)
	add("sonnet_run_status", objectSchema(map[string]any{"run_id": map[string]any{"type": "string", "minLength": 1}}, []string{"run_id"}), s.statusTool)
	add("sonnet_run_result", objectSchema(map[string]any{"run_id": map[string]any{"type": "string", "minLength": 1}}, []string{"run_id"}), s.resultTool)
	add("sonnet_run_cancel", objectSchema(map[string]any{
		"run_id":       map[string]any{"type": "string", "minLength": 1},
		"operation_id": map[string]any{"type": "string", "minLength": 1},
		"force":        map[string]any{"type": "boolean", "const": true},
	}, []string{"run_id", "operation_id", "force"}), s.cancelTool)
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) != 0 {
		schema["required"] = required
	}
	return schema
}
func toolJSON(v any) *mcp.CallToolResult {
	b, _ := json.Marshal(v)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}
func toolFailure(id string) *mcp.CallToolResult {
	result := toolJSON(map[string]string{"failure_id": id})
	result.IsError = true
	return result
}

func productFailure(product Product, id string) *mcp.CallToolResult {
	product.Available = false
	body, _ := json.Marshal(product)
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	payload["failure_id"] = id
	result := toolJSON(payload)
	result.IsError = true
	return result
}

func decodeArguments(raw json.RawMessage, target any) error {
	object := bytes.TrimSpace(raw)
	if len(object) == 0 || object[0] != '{' || object[len(object)-1] != '}' {
		return errors.New("arguments must be one JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(object))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("arguments contain trailing JSON")
	}
	return nil
}

func stableFailureID(err error) string {
	if err == nil {
		return "SONNET_INTERNAL_ERROR"
	}
	for _, known := range []error{ErrIdempotencyConflict, ErrResultPending, ErrForceRequired, ErrUnknownRun, ErrRunStoreOwned, ErrRunStoreInvalid, ErrShutdownIncomplete} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	id := strings.SplitN(err.Error(), ":", 2)[0]
	if strings.HasPrefix(id, "SONNET_") {
		return id
	}
	return "SONNET_INTERNAL_ERROR"
}

func (s *Server) productInfoTool(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args map[string]json.RawMessage
	if err := decodeArguments(r.Params.Arguments, &args); err != nil || len(args) != 0 {
		return toolFailure("SONNET_REQUEST_INVALID"), nil
	}
	p, e := s.ProductInfo()
	if e != nil {
		return productFailure(p, stableFailureID(e)), nil
	}
	if !p.Available || p.Path == "" {
		return productFailure(p, "SONNET_PRODUCT_NOT_FOUND"), nil
	}
	return toolJSON(p), nil
}
func (s *Server) startTool(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var v StartRequest
	if e := decodeArguments(r.Params.Arguments, &v); e != nil || v.ProjectRoot == "" || v.ProjectFile == "" || v.TimeoutSeconds <= 0 {
		return toolFailure("SONNET_REQUEST_INVALID"), nil
	}
	x, e := s.Start(ctx, v)
	if e != nil {
		return toolFailure(stableFailureID(e)), nil
	}
	return toolJSON(x), nil
}
func (s *Server) statusTool(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var v runIDRequest
	if e := decodeArguments(r.Params.Arguments, &v); e != nil || v.RunID == "" {
		return toolFailure("SONNET_REQUEST_INVALID"), nil
	}
	x, e := s.Status(v.RunID)
	if e != nil {
		return toolFailure(stableFailureID(e)), nil
	}
	return toolJSON(x), nil
}
func (s *Server) resultTool(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var v runIDRequest
	if e := decodeArguments(r.Params.Arguments, &v); e != nil || v.RunID == "" {
		return toolFailure("SONNET_REQUEST_INVALID"), nil
	}
	x, e := s.Result(v.RunID)
	if e != nil {
		return toolFailure(stableFailureID(e)), nil
	}
	return toolJSON(x), nil
}
func (s *Server) cancelTool(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var v cancelToolRequest
	if e := decodeArguments(r.Params.Arguments, &v); e != nil || v.RunID == "" || v.OperationID == "" || v.Force == nil || !*v.Force {
		return toolFailure("SONNET_REQUEST_INVALID"), nil
	}
	x, e := s.Cancel(ctx, v.RunID, v.OperationID, *v.Force)
	if e != nil {
		return toolFailure(stableFailureID(e)), nil
	}
	return toolJSON(x), nil
}

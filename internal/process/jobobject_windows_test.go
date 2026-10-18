//go:build windows

package process

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The fixture keeps the original Go creation handle reachable until its one
// Wait completes, or a positively acknowledged transfer releases that handle.
// Metadata handles are borrowed observations and never creation receipts.
type jobFixtureCommandState interface {
	Pid() int
	Exited() bool
}
type jobFixtureCommandWait struct {
	err   error
	state jobFixtureCommandState
}
type jobFixtureCommandOps struct {
	pin     func(func(uintptr)) error
	observe func(uintptr) (uint32, error)
	kill    func() error
	wait    func() jobFixtureCommandWait
}
type jobFixtureCommand struct {
	cmd         *exec.Cmd
	process     *os.Process
	ops         *jobFixtureCommandOps
	waitOnce    sync.Once
	waitDone    chan struct{}
	waitResult  jobFixtureCommandWait
	cleanupOnce sync.Once
	cleanupErr  error
	transferred bool
}

func retainJobFixtureCommand(cmd *exec.Cmd) *jobFixtureCommand {
	return &jobFixtureCommand{cmd: cmd, process: cmd.Process, waitDone: make(chan struct{})}
}
func (c *jobFixtureCommand) operations() (jobFixtureCommandOps, error) {
	if c.process == nil || c.cmd == nil || c.cmd.Process != c.process || c.process.Pid <= 0 || c.waitDone == nil {
		return jobFixtureCommandOps{}, errors.New("fixture original command/process custody missing")
	}
	if c.ops != nil {
		if c.ops.pin == nil || c.ops.observe == nil || c.ops.kill == nil || c.ops.wait == nil {
			return jobFixtureCommandOps{}, errors.New("fixture cleanup override must supply all operations")
		}
		return *c.ops, nil
	}
	return jobFixtureCommandOps{
		pin:     c.process.WithHandle,
		observe: func(h uintptr) (uint32, error) { return windows.WaitForSingleObject(windows.Handle(h), 0) },
		kill:    c.process.Kill,
		wait: func() jobFixtureCommandWait {
			err := c.cmd.Wait()
			var state jobFixtureCommandState
			if c.cmd.ProcessState != nil {
				state = c.cmd.ProcessState
			}
			return jobFixtureCommandWait{err: err, state: state}
		},
	}, nil
}
func (c *jobFixtureCommand) await(deadline time.Time) error {
	if c == nil || c.transferred {
		return errors.New("fixture Wait has no original custody")
	}
	ops, err := c.operations()
	if err != nil {
		return err
	}
	return c.awaitWithOps(deadline, ops)
}
func (c *jobFixtureCommand) awaitWithOps(deadline time.Time, ops jobFixtureCommandOps) error {
	c.waitOnce.Do(func() { go func() { c.waitResult = ops.wait(); runtime.KeepAlive(c.process); close(c.waitDone) }() })
	select {
	case <-c.waitDone:
		return c.waitResult.err
	default:
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-c.waitDone:
		return c.waitResult.err
	case <-timer.C:
		return fmt.Errorf("fixture original Wait: %w", context.DeadlineExceeded)
	}
}
func (c *jobFixtureCommand) reaped() error {
	result := c.waitResult // Called only after waitDone synchronization.
	if result.err != nil {
		exit, ok := result.err.(*exec.ExitError)
		if !ok || exit == nil {
			return result.err
		}
	}
	var pid int
	var exited bool
	switch state := result.state.(type) {
	case *os.ProcessState:
		if state != nil {
			pid, exited = state.Pid(), state.Exited()
		}
	case jobFixtureCommandGuardState:
		pid, exited = state.Pid(), state.Exited()
	}
	if pid <= 0 || pid != c.process.Pid || !exited {
		return errors.New("fixture original Wait lacks positive matching exited ProcessState")
	}
	return nil
}
func jobFixtureExitRace(err error) bool {
	if err == os.ErrProcessDone || err == syscall.EINVAL {
		return true
	}
	call, ok := err.(*os.SyscallError)
	return ok && call != nil && call.Syscall == "TerminateProcess" && call.Err == windows.ERROR_ACCESS_DENIED
}
func jobFixtureObservation(status uint32, err error) error {
	if err != nil {
		return fmt.Errorf("fixture original exit observation: %w", err)
	}
	if status != uint32(windows.WAIT_OBJECT_0) && status != uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("fixture original exit observation unexpected status=%#x", status)
	}
	return nil
}
func (c *jobFixtureCommand) cleanup() error { return c.cleanupBy(time.Now().Add(5 * time.Second)) }
func (c *jobFixtureCommand) cleanupBy(deadline time.Time) error {
	if c == nil {
		return nil
	}
	c.cleanupOnce.Do(func() {
		if c.transferred {
			return
		}
		if deadline.IsZero() {
			c.cleanupErr = errors.New("fixture cleanup requires finite deadline")
			return
		}
		ops, err := c.operations()
		if err != nil {
			c.cleanupErr = err
			return
		}
		select {
		case <-c.waitDone:
			c.cleanupErr = c.reaped()
			return
		default:
		}
		var observationErr, killErr error
		postExited := false
		callbackRan := false
		pinErr := ops.pin(func(h uintptr) {
			callbackRan = true
			status, err := ops.observe(h)
			if observationErr = jobFixtureObservation(status, err); observationErr != nil {
				return
			}
			if status == uint32(windows.WAIT_OBJECT_0) {
				return
			}
			killErr = ops.kill()
			if killErr != nil {
				post, err := ops.observe(h)
				observationErr = jobFixtureObservation(post, err)
				postExited = observationErr == nil && post == uint32(windows.WAIT_OBJECT_0)
			}
		})
		waitErr := c.awaitWithOps(deadline, ops)
		var reapErr error
		select {
		case <-c.waitDone:
			reapErr = c.reaped()
			if errors.Is(waitErr, context.DeadlineExceeded) {
				reapErr = errors.Join(waitErr, reapErr)
			}
		default:
			reapErr = waitErr
		}
		if pinErr != nil {
			if !callbackRan && !errors.Is(pinErr, os.ErrNoHandle) && reapErr == nil {
				return
			}
			c.cleanupErr = errors.Join(fmt.Errorf("fixture original handle pin: %w", pinErr), observationErr, killErr, reapErr)
			return
		}
		if killErr != nil && jobFixtureExitRace(killErr) && postExited && reapErr == nil && observationErr == nil {
			killErr = nil
		}
		if killErr != nil {
			killErr = fmt.Errorf("fixture original Kill: %w", killErr)
		}
		c.cleanupErr = errors.Join(observationErr, killErr, reapErr)
		runtime.KeepAlive(c.process)
	})
	return c.cleanupErr
}

// The value is confined to complete synthetic operation overrides. The real
// adapter publishes only cmd.Wait's actual *os.ProcessState, never this value.
type jobFixtureCommandGuardState struct {
	pid    int
	exited bool
}

func (s jobFixtureCommandGuardState) Pid() int     { return s.pid }
func (s jobFixtureCommandGuardState) Exited() bool { return s.exited }

func checkJobFixtureCommandCleanup(t *testing.T) {
	t.Helper()
	var nilOwner *jobFixtureCommand
	if err := nilOwner.cleanup(); err != nil {
		t.Fatal("nil cleanup owner is not a no-op")
	}
	denied := os.NewSyscallError("TerminateProcess", windows.ERROR_ACCESS_DENIED)
	otherKill := os.NewSyscallError("DuplicateHandle", windows.ERROR_ACCESS_DENIED)
	observationCause := errors.New("synthetic original observation failure")
	ioCause := errors.New("synthetic command Wait I/O failure")
	pinCause := errors.New("synthetic unavailable released wrapper")
	for _, label := range []string{
		"dead-before-wait", "live", "exit-race", "wrapper-release", "live-denial", "unresolved-wait",
		"duplicate-success", "duplicate-error", "missing-original", "unknown-observation", "unexpected-status",
		"no-handle", "pin-unavailable-reaped", "pin-unavailable-unresolved", "missing-state", "default-state", "wrong-state-pid", "not-exited",
		"transferred", "unrelated-kill", "wait-io-joined", "partial-override",
	} {
		pins, observations, kills, waits := 0, 0, 0, 0
		process := &os.Process{Pid: 42}
		cmd := &exec.Cmd{Process: process}
		c := &jobFixtureCommand{cmd: cmd, process: process, waitDone: make(chan struct{})}
		waitStarted := make(chan struct{})
		waitRelease := make(chan struct{})
		held := label == "unresolved-wait" || label == "pin-unavailable-unresolved" || label == "wrapper-release" || label == "live-denial"
		pinEntered := make(chan struct{})
		pinRelease := make(chan struct{})
		duplicate := label == "duplicate-success" || label == "duplicate-error"
		result := jobFixtureCommandWait{state: jobFixtureCommandGuardState{42, true}}
		if label == "dead-before-wait" {
			result.err = &exec.ExitError{ProcessState: &os.ProcessState{}}
		}
		if label == "missing-state" {
			result.state = nil
		}
		if label == "default-state" {
			result.state = &os.ProcessState{}
		}
		if label == "wrong-state-pid" {
			result.state = jobFixtureCommandGuardState{43, true}
		}
		if label == "not-exited" {
			result.state = jobFixtureCommandGuardState{42, false}
		}
		if label == "wait-io-joined" {
			result.err = errors.Join(&exec.ExitError{ProcessState: &os.ProcessState{}}, ioCause)
		}
		c.ops = &jobFixtureCommandOps{
			pin: func(callback func(uintptr)) error {
				pins++
				if label == "no-handle" {
					return os.ErrNoHandle
				}
				if label == "pin-unavailable-reaped" || label == "pin-unavailable-unresolved" {
					return pinCause
				}
				if duplicate {
					close(pinEntered)
					<-pinRelease
				}
				callback(7)
				return nil
			},
			observe: func(uintptr) (uint32, error) {
				observations++
				if label == "unknown-observation" {
					return uint32(windows.WAIT_FAILED), observationCause
				}
				if label == "unexpected-status" {
					return uint32(windows.WAIT_ABANDONED), nil
				}
				if label == "dead-before-wait" || label == "missing-state" || label == "default-state" || label == "wrong-state-pid" || label == "not-exited" || label == "wait-io-joined" {
					return uint32(windows.WAIT_OBJECT_0), nil
				}
				if observations > 1 {
					if label == "live-denial" {
						close(waitRelease)
						return uint32(windows.WAIT_TIMEOUT), nil
					}
					if label == "duplicate-error" {
						return uint32(windows.WAIT_TIMEOUT), nil
					}
					if label == "wrapper-release" {
						close(waitRelease)
					}
					return uint32(windows.WAIT_OBJECT_0), nil
				}
				return uint32(windows.WAIT_TIMEOUT), nil
			},
			kill: func() error {
				kills++
				switch label {
				case "exit-race", "live-denial", "duplicate-error", "unresolved-wait":
					return denied
				case "wrapper-release":
					return syscall.EINVAL
				case "unrelated-kill":
					return otherKill
				}
				return nil
			},
			wait: func() jobFixtureCommandWait {
				waits++
				close(waitStarted)
				if held {
					<-waitRelease
				}
				return result
			},
		}
		if label == "missing-original" {
			c.process = nil
		}
		if label == "transferred" {
			c.transferred = true
		}
		if label == "partial-override" {
			c.ops.observe = nil
		}
		deadline := time.Now().Add(time.Second)
		if label == "unresolved-wait" || label == "pin-unavailable-unresolved" {
			deadline = time.Now().Add(-time.Millisecond)
		}
		var asyncWait chan error
		if label == "wrapper-release" {
			asyncWait = make(chan error, 1)
			go func() { asyncWait <- c.await(time.Now().Add(time.Second)) }()
			<-waitStarted // Wrapper release is represented before publication.
		}
		var err error
		if duplicate {
			first, second := make(chan error, 1), make(chan error, 1)
			secondPending := make(chan struct{})
			go func() { first <- c.cleanupBy(deadline) }()
			<-pinEntered
			go func() { close(secondPending); second <- c.cleanupBy(deadline) }()
			<-secondPending
			close(pinRelease)
			err = <-first
			if repeated := <-second; repeated != err {
				t.Fatalf("cleanup guard %s changed stored result", label)
			}
		} else {
			err = c.cleanupBy(deadline)
		}
		if label == "unresolved-wait" || label == "pin-unavailable-unresolved" {
			// An expired explicit join budget deterministically holds reap.
			// Release and join this same underlying waiter before returning.
			close(waitRelease)
			select {
			case <-c.waitDone:
			case <-time.After(time.Second):
				t.Fatalf("cleanup guard %s left its waiter outstanding", label)
			}
		}
		if asyncWait != nil {
			if waitErr := <-asyncWait; waitErr != nil {
				t.Fatalf("cleanup guard %s original waiter failed: %v", label, waitErr)
			}
		}
		switch label {
		case "dead-before-wait", "live", "exit-race", "wrapper-release", "duplicate-success", "pin-unavailable-reaped":
			if err != nil {
				t.Fatalf("cleanup guard %s rejected settled original: %v", label, err)
			}
		case "live-denial", "duplicate-error":
			if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				t.Fatalf("cleanup guard %s erased live denial: %v", label, err)
			}
		case "unresolved-wait", "pin-unavailable-unresolved":
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cleanup guard %s accepted unresolved reap: %v", label, err)
			}
			if label == "unresolved-wait" && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				t.Fatal("unresolved reap erased exit-race Kill failure")
			}
		case "unknown-observation":
			if !errors.Is(err, observationCause) {
				t.Fatalf("cleanup guard %s lost observation cause", label)
			}
		case "no-handle":
			if !errors.Is(err, os.ErrNoHandle) {
				t.Fatal("explicit no-handle became success")
			}
		case "unrelated-kill":
			var call *os.SyscallError
			if !errors.As(err, &call) || call.Syscall != "DuplicateHandle" {
				t.Fatal("unrelated Kill failure became success")
			}
		case "wait-io-joined":
			if !errors.Is(err, ioCause) {
				t.Fatal("joined Wait I/O failure was filtered by ExitError")
			}
		case "transferred":
			if err != nil || pins+observations+kills+waits != 0 {
				t.Fatal("transferred cleanup invoked an operation")
			}
		default:
			if err == nil {
				t.Fatalf("cleanup guard %s hid invalid/unknown evidence", label)
			}
		}
		if label != "transferred" && label != "missing-original" && label != "partial-override" && waits != 1 {
			t.Fatalf("cleanup guard %s Wait count=%d", label, waits)
		}
		wantKills := 0
		switch label {
		case "live", "exit-race", "wrapper-release", "live-denial", "unresolved-wait", "duplicate-success", "duplicate-error", "unrelated-kill":
			wantKills = 1
		}
		if kills != wantKills {
			t.Fatalf("cleanup guard %s Kill count=%d", label, kills)
		}
		oldPins, oldObservations, oldKills, oldWaits := pins, observations, kills, waits
		if repeated := c.cleanupBy(time.Now().Add(5 * time.Second)); repeated != err || pins != oldPins || observations != oldObservations || kills != oldKills || waits != oldWaits {
			t.Fatalf("cleanup guard %s renewed its settled outcome/actions", label)
		}
	}
}

func cleanupJobFixtureCommand(t *testing.T, c *jobFixtureCommand) {
	t.Helper()
	if err := c.cleanup(); err != nil {
		t.Errorf("original fixture cleanup: %v", err)
	}
}

func startJobFixtureCommandInJob(t *testing.T, job *Job, cmd *exec.Cmd) *jobFixtureCommand {
	t.Helper()
	custody, _, startErr := beginJobFixtureCommandInJob(job, cmd)
	// The original Job remains the containment owner even if the legacy
	// starter's post-create Go-wrapper acquisition fails.
	t.Cleanup(func() {
		if err := job.Close(); err != nil {
			t.Errorf("retained fixture Job close: %v", err)
		}
		if custody != nil {
			cleanupJobFixtureCommand(t, custody)
		}
	})
	if startErr != nil {
		t.Fatalf("StartWithJob: %v", startErr)
	}
	if custody == nil {
		t.Fatal("successful fixture start has no command custody")
	}
	return custody
}
func beginJobFixtureCommandInJob(job *Job, cmd *exec.Cmd) (*jobFixtureCommand, int, error) {
	pid, startErr := StartWithJob(job, cmd)
	var custody *jobFixtureCommand
	if cmd.Process != nil {
		custody = retainJobFixtureCommand(cmd)
	}
	return custody, pid, startErr
}

const jobFixtureEntryPrefix = "WJOB-ENTRY-V1 "
const jobFixtureEntryConfigEnv = "MCPHUB_JOB_FIXTURE_ENTRY_CONFIG"
const jobFixtureFrameLimit = 8192

type jobFixtureEntryBind struct {
	Run              string `json:"run"`
	Nonce            string `json:"nonce"`
	OwnerSource      string `json:"ownerSource"`
	ControllerSource string `json:"controllerSource"`
	Packet           string `json:"packet"`
	Binary           string `json:"binary"`
}
type jobFixtureEntryConfig struct {
	Bind          jobFixtureEntryBind `json:"bind"`
	ExpectedFlags uint32              `json:"expectedFlags"`
	EntryBytes    uint32              `json:"entryBytes"`
	SourceDir     string              `json:"sourceDir"`
	Companions    map[string]string   `json:"companions"`
}
type jobFixtureEntryNative struct {
	Class    uint32 `json:"class"`
	Bytes    uint32 `json:"bytes"`
	Returned uint32 `json:"returned"`
	Flags    uint32 `json:"flags"`
	Member   bool   `json:"member"`
	Alive    bool   `json:"alive"`
}
type jobFixtureEntryReady struct {
	V            uint32              `json:"v"`
	Kind         string              `json:"kind"`
	Bind         jobFixtureEntryBind `json:"bind"`
	Challenge    string              `json:"challenge"`
	PID          uint32              `json:"pid"`
	Created100ns string              `json:"created100ns"`
}
type jobFixtureEntryGrant struct {
	V               uint32                `json:"v"`
	Kind            string                `json:"kind"`
	Bind            jobFixtureEntryBind   `json:"bind"`
	Challenge       string                `json:"challenge"`
	PID             uint32                `json:"pid"`
	Created100ns    string                `json:"created100ns"`
	OwnerGeneration string                `json:"ownerGeneration"`
	Entry           jobFixtureEntryNative `json:"entry"`
	Confirm         bool                  `json:"confirm"`
}
type jobFixtureEntryAck struct {
	V               uint32 `json:"v"`
	Kind            string `json:"kind"`
	Nonce           string `json:"nonce"`
	Challenge       string `json:"challenge"`
	OwnerGeneration string `json:"ownerGeneration"`
	GrantSHA256     string `json:"grantSha256"`
	Confirm         bool   `json:"confirm"`
}
type jobFixtureEntryToken struct {
	Config jobFixtureEntryConfig
	Ready  jobFixtureEntryReady
	Ack    jobFixtureEntryAck
}

var jobFixtureEntryLease struct {
	sync.Mutex
	token     *jobFixtureEntryToken
	err       error
	attempted bool
}

func jobFixtureHex(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func jobFixtureDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func jobFixtureNonce() (string, error) {
	var raw [16]byte
	_, err := rand.Read(raw[:])
	return hex.EncodeToString(raw[:]), err
}

// Check every object before typed decoding: DisallowUnknownFields alone accepts
// duplicate keys. Number decoding remains typed, rejecting floats and overflow.
func jobFixtureJSONValue(d *json.Decoder) error {
	v, err := d.Token()
	if err != nil {
		return err
	}
	delim, container := v.(json.Delim)
	if !container {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errors.New("duplicate or invalid JSON key")
			}
			seen[name] = true
			if err := jobFixtureJSONValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := jobFixtureJSONValue(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}
func decodeJobFixtureJSON(raw []byte, target any) error {
	if target == nil || reflect.TypeOf(target).Kind() != reflect.Pointer || reflect.ValueOf(target).IsNil() || reflect.TypeOf(target).Elem().Kind() != reflect.Struct {
		return errors.New("typed non-nil fixture object required")
	}
	if len(raw) == 0 || len(raw) > jobFixtureFrameLimit {
		return errors.New("empty or oversized fixture JSON")
	}
	for _, b := range raw {
		if b > 127 || b < 32 && b != '\n' && b != '\r' && b != '\t' {
			return errors.New("fixture JSON must be ASCII UTF-8")
		}
	}
	check := json.NewDecoder(bytes.NewReader(raw))
	check.UseNumber()
	if err := jobFixtureJSONValue(check); err != nil {
		return err
	}
	if _, err := check.Token(); err != io.EOF {
		return errors.New("trailing fixture JSON")
	}
	if err := requireJobFixtureJSONFields(raw, reflect.TypeOf(target).Elem()); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	return nil
}
func requireJobFixtureJSONFields(raw []byte, shape reflect.Type) error {
	if shape.Kind() != reflect.Struct {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	allowed := map[string]bool{}
	for i := 0; i < shape.NumField(); i++ {
		field := shape.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" {
			name = field.Name
		}
		allowed[name] = true
	}
	for name := range fields {
		if !allowed[name] {
			return fmt.Errorf("unknown or noncanonical fixture field: %s", name)
		}
	}
	for i := 0; i < shape.NumField(); i++ {
		field := shape.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" {
			name = field.Name
		}
		value, found := fields[name]
		if !found || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("required non-null fixture field: %s", name)
		}
		if err := requireJobFixtureJSONFields(value, field.Type); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}
func jobFixtureReadFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("fixture input is not an ordinary file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, jobFixtureFrameLimit+1))
	return raw, errors.Join(readErr, f.Close())
}
func jobFixtureFileDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("fixture digest input is not an ordinary file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	err = errors.Join(copyErr, f.Close())
	return hex.EncodeToString(h.Sum(nil)), err
}
func loadJobFixtureEntryConfig() (jobFixtureEntryConfig, error) {
	var config jobFixtureEntryConfig
	path := os.Getenv(jobFixtureEntryConfigEnv)
	if path == "" || !filepath.IsAbs(path) {
		return config, errors.New("qualified entry configuration is required")
	}
	raw, err := jobFixtureReadFile(path)
	if err != nil {
		return config, err
	}
	if err := decodeJobFixtureJSON(raw, &config); err != nil {
		return config, err
	}
	b := config.Bind
	if b.Run == "" || !jobFixtureHex(b.Nonce, 32) || !jobFixtureHex(b.OwnerSource, 64) || !jobFixtureHex(b.ControllerSource, 64) || !jobFixtureHex(b.Packet, 64) || !jobFixtureHex(b.Binary, 64) {
		return config, errors.New("incomplete entry source/binary/run binding")
	}
	if config.ExpectedFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 || config.ExpectedFlags&(windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK|windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK) != 0 || config.EntryBytes != uint32(unsafe.Sizeof(windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{})) {
		return config, errors.New("entry policy or native layout mismatch")
	}
	if !filepath.IsAbs(config.SourceDir) || len(config.Companions) != 4 {
		return config, errors.New("qualified companion source tuple required")
	}
	_, compiledSource, _, ok := runtime.Caller(0)
	if !ok || !strings.EqualFold(filepath.Clean(config.SourceDir), filepath.Clean(filepath.Dir(compiledSource))) {
		return config, errors.New("companion directory differs from actual compiled source guard directory")
	}
	for _, name := range []string{"jobobject_windows.go", "current_job_windows.go", "jobobject_windows_test.go", "job_options_windows_test.go"} {
		expected := config.Companions[name]
		if !jobFixtureHex(expected, 64) {
			return config, fmt.Errorf("missing companion digest: %s", name)
		}
		actual, err := jobFixtureFileDigest(filepath.Join(config.SourceDir, name))
		if err != nil || actual != expected {
			return config, fmt.Errorf("companion mismatch %s: %w", name, errors.Join(err, errors.New("digest mismatch")))
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return config, err
	}
	actual, err := jobFixtureFileDigest(exe)
	if err != nil || actual != b.Binary {
		return config, errors.Join(err, errors.New("executed binary digest mismatch"))
	}
	return config, nil
}

func jobFixtureCreationTime(h windows.Handle) (string, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return "", err
	}
	n := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	if n == 0 {
		return "", errors.New("zero native creation time")
	}
	return strconv.FormatUint(n, 10), nil
}
func jobFixtureWriteEntry(output io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	frame := append(append([]byte(jobFixtureEntryPrefix), raw...), '\n')
	if len(frame) > jobFixtureFrameLimit {
		return errors.New("entry frame oversized")
	}
	n, err := output.Write(frame)
	if n != len(frame) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	return err
}
func receiveJobFixtureEntry() (token *jobFixtureEntryToken, resultErr error) {
	stdinCloseAttempted := false
	closeInput := func() error { stdinCloseAttempted = true; return os.Stdin.Close() }
	defer func() {
		// Even config/self/READY refusal owns closing the creator-configured
		// read end. A failed close remains failure, never a successful token.
		if !stdinCloseAttempted {
			if err := closeInput(); err != nil {
				token = nil
				resultErr = errors.Join(resultErr, fmt.Errorf("entry/stdin early failure close: %w", err))
			}
		}
	}()
	config, err := loadJobFixtureEntryConfig()
	if err != nil {
		return nil, fmt.Errorf("entry/config: %w", err)
	}
	created, err := jobFixtureCreationTime(windows.CurrentProcess())
	if err != nil {
		return nil, fmt.Errorf("entry/self-generation: %w", err)
	}
	challenge, err := jobFixtureNonce()
	if err != nil {
		return nil, err
	}
	ready := jobFixtureEntryReady{1, "ready", config.Bind, challenge, uint32(os.Getpid()), created}
	deadline := time.Now().Add(10 * time.Second)
	if err := jobFixtureWriteEntry(os.Stdout, ready); err != nil {
		return nil, fmt.Errorf("entry/ready: %w", err)
	}
	type input struct {
		raw []byte
		err error
	}
	done := make(chan input, 1)
	go func() {
		raw, err := io.ReadAll(io.LimitReader(os.Stdin, jobFixtureFrameLimit+1))
		done <- input{raw, err}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var received input
	select {
	case received = <-done:
	case <-timer.C:
		closeErr := closeInput()
		select {
		case <-done:
			return nil, errors.Join(context.DeadlineExceeded, closeErr)
		case <-time.After(100 * time.Millisecond):
			return nil, errors.Join(context.DeadlineExceeded, closeErr, errors.New("entry stdin reader remains unjoined"))
		}
	}
	if err := errors.Join(received.err, closeInput()); err != nil {
		return nil, fmt.Errorf("entry/stdin: %w", err)
	}
	raw := received.raw
	if len(raw) > jobFixtureFrameLimit || !bytes.HasPrefix(raw, []byte(jobFixtureEntryPrefix)) || len(raw) == 0 || raw[len(raw)-1] != '\n' || bytes.Count(raw, []byte{'\n'}) != 1 {
		return nil, errors.New("entry/grant truncated, oversized or extra frame")
	}
	payload := raw[len(jobFixtureEntryPrefix) : len(raw)-1]
	return completeJobFixtureEntry(config, ready, payload, deadline, os.Stdout, time.Now)
}
func completeJobFixtureEntry(config jobFixtureEntryConfig, ready jobFixtureEntryReady, payload []byte, deadline time.Time, output io.Writer, now func() time.Time) (*jobFixtureEntryToken, error) {
	if output == nil || now == nil || deadline.IsZero() {
		return nil, errors.New("entry/completion requires explicit writer, clock and deadline")
	}
	writer := reflect.ValueOf(output)
	switch writer.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Interface, reflect.Map, reflect.Slice, reflect.Chan:
		if writer.IsNil() {
			return nil, errors.New("entry/completion writer is nil")
		}
	}
	var grant jobFixtureEntryGrant
	if err := decodeJobFixtureJSON(payload, &grant); err != nil {
		return nil, fmt.Errorf("entry/grant-json: %w", err)
	}
	if err := validateJobFixtureGrant(config, ready, grant, deadline, now()); err != nil {
		return nil, err
	}
	ack := jobFixtureEntryAck{1, "ack", ready.Bind.Nonce, ready.Challenge, grant.OwnerGeneration, jobFixtureDigest(payload), true}
	if err := jobFixtureWriteEntry(output, ack); err != nil {
		return nil, fmt.Errorf("entry/ack: %w", err)
	}
	completedAt := now()
	if completedAt.IsZero() || !completedAt.Before(deadline) {
		return nil, fmt.Errorf("entry/ack completed after readiness deadline: %w", context.DeadlineExceeded)
	}
	return &jobFixtureEntryToken{config, ready, ack}, nil
}
func validateJobFixtureGrant(config jobFixtureEntryConfig, ready jobFixtureEntryReady, grant jobFixtureEntryGrant, deadline time.Time, observedNow time.Time) error {
	b := config.Bind
	created, err := strconv.ParseUint(ready.Created100ns, 10, 64)
	if b.Run == "" || !jobFixtureHex(b.Nonce, 32) || !jobFixtureHex(b.OwnerSource, 64) || !jobFixtureHex(b.ControllerSource, 64) || !jobFixtureHex(b.Packet, 64) || !jobFixtureHex(b.Binary, 64) || config.ExpectedFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 || config.ExpectedFlags&(windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK|windows.JOB_OBJECT_LIMIT_SILENT_BREAKAWAY_OK) != 0 || config.EntryBytes != uint32(unsafe.Sizeof(windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{})) || ready.V != 1 || ready.Kind != "ready" || ready.Bind != b || !jobFixtureHex(ready.Challenge, 32) || ready.PID == 0 || err != nil || created == 0 || strconv.FormatUint(created, 10) != ready.Created100ns {
		return errors.New("entry/required independent upper/self binding missing")
	}
	if deadline.IsZero() || observedNow.IsZero() || !observedNow.Before(deadline) || grant.V != 1 || grant.Kind != "grant" || grant.Bind != ready.Bind || grant.Challenge != ready.Challenge || grant.PID != ready.PID || grant.Created100ns != ready.Created100ns || !jobFixtureHex(grant.OwnerGeneration, 32) || !grant.Confirm || !grant.Entry.Member || !grant.Entry.Alive || grant.Entry.Class != uint32(windows.JobObjectExtendedLimitInformation) || grant.Entry.Bytes != config.EntryBytes || grant.Entry.Returned != config.EntryBytes || grant.Entry.Flags != config.ExpectedFlags {
		return errors.New("entry/grant binding, native flags, generation or deadline mismatch")
	}
	return nil
}
func requireJobFixtureEntry(t *testing.T) *jobFixtureEntryToken {
	t.Helper()
	jobFixtureEntryLease.Lock()
	defer jobFixtureEntryLease.Unlock()
	if !jobFixtureEntryLease.attempted {
		jobFixtureEntryLease.attempted = true
		jobFixtureEntryLease.token, jobFixtureEntryLease.err = receiveJobFixtureEntry()
	}
	if jobFixtureEntryLease.err != nil || jobFixtureEntryLease.token == nil {
		t.Fatalf("qualified original outer entry refused: %v", jobFixtureEntryLease.err)
	}
	if jobFixtureEntryLease.token.Ready.PID != uint32(os.Getpid()) {
		t.Fatal("cached entry generation mismatch")
	}
	return jobFixtureEntryLease.token
}

func TestJobCreatorObservationNeverTerminates(t *testing.T) {
	terminations := 0
	job := &Job{handle: windows.Handle(1), terminationOps: &jobTerminationOps{
		accounting: func(*Job) (jobObjectBasicAccountingInformation, error) {
			return jobObjectBasicAccountingInformation{TotalProcesses: 1}, nil
		},
		listMembers:  func(*Job) (jobProcessIDList, error) { return jobProcessIDList{}, nil },
		openProcess:  func(uint32) (windows.Handle, error) { return windows.Handle(3), nil },
		isMember:     func(windows.Handle, *Job) (bool, error) { return true, nil },
		terminate:    func(*Job) error { terminations++; return nil },
		wait:         func(windows.Handle) (uint32, error) { return uint32(windows.WAIT_OBJECT_0), nil },
		closeProcess: func(windows.Handle) error { return nil },
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	observed, err := job.observeCreatorClosure(ctx, containedJobOriginal{job: job, handle: windows.Handle(2), pid: 7, atCreate: true, required: true})
	if err != nil || !observed.ObservedCurrentMembersExited {
		t.Fatalf("closed observation failed: %v", err)
	}
	if terminations != 0 {
		t.Fatal("read-only creator observation invoked termination")
	}
}

func creatorObservationUnitJob() (*Job, containedJobOriginal, *int) {
	terminated := new(int)
	j := &Job{handle: windows.Handle(1), terminationOps: &jobTerminationOps{
		accounting: func(*Job) (jobObjectBasicAccountingInformation, error) {
			return jobObjectBasicAccountingInformation{TotalProcesses: 2}, nil
		},
		listMembers:  func(*Job) (jobProcessIDList, error) { return jobProcessIDList{Assigned: 1, PIDs: []uint32{11}}, nil },
		openProcess:  func(uint32) (windows.Handle, error) { return windows.Handle(3), nil },
		isMember:     func(windows.Handle, *Job) (bool, error) { return true, nil },
		terminate:    func(*Job) error { *terminated++; return nil },
		wait:         func(windows.Handle) (uint32, error) { return uint32(windows.WAIT_OBJECT_0), nil },
		closeProcess: func(windows.Handle) error { return nil },
	}}
	return j, containedJobOriginal{job: j, handle: windows.Handle(2), pid: 7, atCreate: true}, terminated
}

func TestJobCreatorObservationFaultsAndSafeDefaults(t *testing.T) {
	for _, name := range []string{"nil-context", "no-deadline", "expired", "zero-window", "missing-original", "snapshot", "member-list", "duplicate", "zero-pid", "member-open", "member-identity", "member-nonmember", "member-wait", "member-status", "handle-close", "active-cancel", "unsignaled-cancel", "lifetime-drift", "final-accounting"} {
		t.Run(name, func(t *testing.T) {
			j, original, terminated := creatorObservationUnitJob()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			var supplied context.Context = ctx
			cause := errors.New("synthetic Job observation failure")
			calls := 0
			switch name {
			case "nil-context":
				supplied = nil
			case "no-deadline":
				supplied = context.Background()
			case "expired":
				cancel()
			case "missing-original":
				original.handle = 0
			case "snapshot":
				j.terminationOps.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
					return jobObjectBasicAccountingInformation{}, cause
				}
			case "member-list":
				j.terminationOps.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{}, cause }
			case "duplicate":
				j.terminationOps.listMembers = func(*Job) (jobProcessIDList, error) {
					return jobProcessIDList{Assigned: 2, PIDs: []uint32{11, 11}}, nil
				}
			case "zero-pid":
				j.terminationOps.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{Assigned: 1, PIDs: []uint32{0}}, nil }
			case "member-open":
				j.terminationOps.openProcess = func(uint32) (windows.Handle, error) { return 0, cause }
			case "member-identity":
				j.terminationOps.isMember = func(windows.Handle, *Job) (bool, error) { return false, cause }
			case "member-nonmember":
				j.terminationOps.isMember = func(windows.Handle, *Job) (bool, error) { return false, nil }
			case "member-wait":
				j.terminationOps.wait = func(windows.Handle) (uint32, error) { return 0, cause }
			case "member-status":
				j.terminationOps.wait = func(windows.Handle) (uint32, error) { return 999, nil }
			case "handle-close":
				j.terminationOps.closeProcess = func(windows.Handle) error { return cause }
			case "active-cancel":
				j.terminationOps.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
					calls++
					if calls > 1 {
						cancel()
					}
					return jobObjectBasicAccountingInformation{TotalProcesses: 2, ActiveProcesses: 1}, nil
				}
			case "unsignaled-cancel":
				j.terminationOps.wait = func(windows.Handle) (uint32, error) { cancel(); return uint32(windows.WAIT_TIMEOUT), nil }
			case "lifetime-drift":
				j.terminationOps.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
					calls++
					total := uint32(2)
					if calls > 1 {
						total = 3
					}
					return jobObjectBasicAccountingInformation{TotalProcesses: total}, nil
				}
			case "final-accounting":
				j.terminationOps.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
					calls++
					if calls == 3 {
						return jobObjectBasicAccountingInformation{}, cause
					}
					return jobObjectBasicAccountingInformation{TotalProcesses: 2}, nil
				}
			}
			var observed jobTerminationObservation
			var err error
			if name == "zero-window" {
				observed, err = j.observeCurrentJob(0, original, ctx, false)
			} else {
				observed, err = j.observeCreatorClosure(supplied, original)
			}
			if err == nil || *terminated != 0 || j.handle != windows.Handle(1) {
				t.Fatalf("failure changed creator custody or terminated: observed=%+v err=%v", observed, err)
			}
			if name == "snapshot" || name == "member-list" || name == "member-open" || name == "member-identity" || name == "member-wait" || name == "handle-close" || name == "final-accounting" {
				if !errors.Is(err, cause) {
					t.Fatal("causal observation failure lost")
				}
			}
			if strings.HasSuffix(name, "-cancel") && !errors.Is(err, context.Canceled) {
				t.Fatal("observation cancellation lost")
			}
		})
	}
}

func TestJobCreatorObservationPreservesLegacyProjections(t *testing.T) {
	for _, mode := range []string{"read-only", "current-termination", "strict-termination", "zero-termination"} {
		t.Run(mode, func(t *testing.T) {
			j, original, terminated := creatorObservationUnitJob()
			j.terminationOps.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
				return jobObjectBasicAccountingInformation{TotalProcesses: 3}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			switch mode {
			case "read-only":
				obs, err := j.observeCreatorClosure(ctx, original)
				if err != nil || !obs.ObservedCurrentMembersExited || obs.StrictOriginalsSettled || obs.strictFailure == nil || *terminated != 0 {
					t.Fatalf("read current projection: %+v %v", obs, err)
				}
			case "current-termination":
				if err := j.terminateForContained(1000, original); err != nil || *terminated != 1 {
					t.Fatalf("legacy current projection: %v", err)
				}
			case "strict-termination":
				obs, err := j.terminateAllObserved(1000, original)
				if err != nil || obs.StrictOriginalsSettled || obs.strictFailure == nil || *terminated != 1 {
					t.Fatalf("legacy strict projection: %+v %v", obs, err)
				}
			case "zero-termination":
				if err := j.TerminateAll(0); err != nil || *terminated != 1 {
					t.Fatalf("legacy zero directive: %v", err)
				}
			}
		})
	}
}

func TestProvisionalCreatorClosureSourceUnit(t *testing.T) {
	metadata := collectCreatorClosureExitMetadata(t)
	runCreatorClosureSourceUnit(t, metadata)
}

// NONZERO-EXIT-METADATA is one Root-admitted auxiliary child. It supplies real
// wait metadata to sequencing fakes; it never calls/mints production closure.
func collectCreatorClosureExitMetadata(t *testing.T) creatorExitObservation {
	t.Helper()
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		_ = job.Close()
		t.Fatal(err)
	}
	stdout, err := os.CreateTemp(t.TempDir(), "aux-stdout-")
	if err != nil {
		_ = stdin.Close()
		_ = job.Close()
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(t.TempDir(), "aux-stderr-")
	if err != nil {
		_ = stdout.Close()
		_ = stdin.Close()
		_ = job.Close()
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestProvisionalCreatorClosureExitMetadataHelper$", "--", "--creator-closure-exit17")
	var process, thread windows.Handle
	var original containedJobOriginal
	waited, jobClosed, originalsClosed, streamsEOF := false, false, false, false
	membersObserved := false
	var waitDone chan error
	receipt := map[string]any{"auxiliaryID": "NONZERO-EXIT-METADATA", "spawnCount": 0, "waitCount": 0, "exitCodeKnown": false, "jobActiveZero": false, "originalHandlesClosed": false, "jobClosed": false, "streamsEOF": false}
	defer func() {
		// This separate creator-authorized failure cleanup cannot satisfy the
		// metadata observation criterion or turn failed census into success.
		if !membersObserved && (process != 0 || cmd.Process != nil) {
			receipt["failureCleanup"] = true
			receipt["failureCleanupTerminationRequested"] = true
			if err := job.TerminateAll(5000); err != nil {
				t.Errorf("auxiliary retained Job cleanup: %v", err)
			}
		}
		if !waited && waitDone != nil {
			select {
			case <-waitDone:
				waited = true
			case <-time.After(6 * time.Second):
				t.Error("auxiliary original wait did not reap")
			}
		}
		if !jobClosed {
			if err := job.Close(); err != nil {
				t.Errorf("auxiliary Job close: %v", err)
			} else {
				jobClosed = true
			}
		}
		if !originalsClosed {
			ok := true
			for _, h := range []windows.Handle{process, thread} {
				if h != 0 {
					if err := windows.CloseHandle(h); err != nil {
						ok = false
						t.Errorf("auxiliary original handle close: %v", err)
					}
				}
			}
			originalsClosed = ok
		}
		for _, f := range []*os.File{stdin, stdout, stderr} {
			_ = f.Close()
		}
		receipt["directReaped"], receipt["originalHandlesClosed"], receipt["jobClosed"], receipt["streamsEOF"] = waited, originalsClosed, jobClosed, streamsEOF
		if root := os.Getenv("MCPHUB_PROVISIONAL_CLOSURE_PROOF_ROOT"); root != "" {
			raw, err := json.MarshalIndent(receipt, "", "  ")
			if err == nil {
				err = os.WriteFile(root+string(os.PathSeparator)+"auxiliary-exit17.json", raw, 0600)
			}
			if err != nil {
				t.Errorf("auxiliary receipt: %v", err)
			}
		}
	}()
	_, err = startWithJobFilesObserved(job, cmd, stdin, stdout, stderr, &startWithJobObserverOptions{retainCreated: func(pi windows.ProcessInformation) {
		process, thread = pi.Process, pi.Thread
		original = containedJobOriginal{job: job, handle: pi.Process, pid: pi.ProcessId, atCreate: true}
		receipt["spawnCount"] = 1
	}})
	if err != nil {
		t.Fatal(err)
	}
	receipt["spawnCount"] = 1
	waitDone = make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	receipt["waitCount"] = 1
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var waitErr error
	select {
	case waitErr = <-waitDone:
		waited = true
	case <-ctx.Done():
		t.Fatal("auxiliary immediate exit timed out")
	}
	var rawExit *exec.ExitError
	if !errors.As(waitErr, &rawExit) || cmd.ProcessState == nil || rawExit.ProcessState != cmd.ProcessState || cmd.ProcessState.ExitCode() != 17 {
		t.Fatalf("auxiliary did not supply actual ExitError/ProcessState: %v", waitErr)
	}
	receipt["exitCodeKnown"], receipt["exitCode"] = true, cmd.ProcessState.ExitCode()
	observation, err := job.observeCreatorClosure(ctx, original)
	if err != nil || !observation.ObservedCurrentMembersExited {
		t.Fatalf("auxiliary original Job observation failed: %v", err)
	}
	membersObserved = true
	receipt["jobActiveZero"] = true
	if err := creatorClosureContext(ctx); err != nil {
		t.Fatalf("auxiliary observation context expired before close: %v", err)
	}
	if err := windows.CloseHandle(process); err != nil {
		t.Fatal(err)
	}
	process = 0
	if err := windows.CloseHandle(thread); err != nil {
		t.Fatal(err)
	}
	thread = 0
	originalsClosed = true
	if err := job.Close(); err != nil {
		t.Fatal(err)
	}
	jobClosed = true
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stderr.Close(); err != nil {
		t.Fatal(err)
	}
	out, outErr := os.ReadFile(stdout.Name())
	diagnostic, errErr := os.ReadFile(stderr.Name())
	if outErr != nil || errErr != nil || !strings.Contains(string(out), "NONZERO_EXIT_METADATA_ONLY") || string(diagnostic) != "SYNTHETIC_EXIT17\n" {
		t.Fatal("auxiliary synthetic output/EOF mismatch")
	}
	streamsEOF = true
	t.Log("NONZERO_EXIT_METADATA calls=1 exit=17 wait=1 job_empty=true original_handles_closed=true streams_eof=true")
	return creatorExitObservation{known: true, code: cmd.ProcessState.ExitCode(), raw: rawExit}
}

// TestJob_KillsAssignedProcessOnClose proves the actual contract:
// when the last handle to a job with KILL_ON_JOB_CLOSE is closed, the
// kernel terminates every process still in the job. Without this, the
// fix is theater — the syscalls succeed but orphans still survive.
//
// Spawns `timeout /T 30` (Windows builtin), assigns it to a job,
// closes the job, asserts the process is dead within ~1s.
func TestJob_KillsAssignedProcessOnClose(t *testing.T) {
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skipf("timeout.exe not on PATH: %v", err)
	}
	cmd := exec.Command("timeout", "/T", "30", "/NOBREAK")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	custody := retainJobFixtureCommand(cmd)
	defer cleanupJobFixtureCommand(t, custody)

	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer func() {
		if err := job.Close(); err != nil {
			t.Errorf("Job close: %v", err)
		}
	}()
	if err := job.Assign(cmd); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	// Verify process is alive immediately after assignment — orphan
	// protection should not pre-emptively kill anyone.
	if !processAlive(pid, t) {
		t.Fatalf("process pid=%d already dead before Close — assignment must not pre-kill", pid)
	}

	// Closing the job's last handle should cause the kernel to kill
	// the assigned process via KILL_ON_JOB_CLOSE.
	if err := job.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Poll for process death — kernel cleanup is fast but not instant.
	// 2s is generous; if it takes longer the contract is broken.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid, t) {
			return // success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process pid=%d still alive 2s after Job.Close — KILL_ON_JOB_CLOSE did not fire", pid)
}

// TestJob_AssignNilCmdReturnsError guards against a misuse of the API
// where a caller forgets to Start() the cmd before assigning. Without
// this check, the underlying OpenProcess(0) syscall returns a
// confusing error from the kernel layer instead of an actionable one.
func TestJob_AssignNilCmdReturnsError(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()

	if err := job.Assign(nil); err == nil {
		t.Error("Assign(nil) returned nil error; want error")
	}
	cmd := exec.Command("notepad.exe")
	// cmd.Process is nil here — Start was not called.
	if err := job.Assign(cmd); err == nil {
		t.Error("Assign(cmd-not-started) returned nil error; want error")
	}
}

// TestJob_CloseIdempotent guards against a double-Close panic. The
// daemon Stop path closes the job after killProcessTree — if a future
// refactor accidentally calls Close twice, the second call must not
// panic or return a misleading "handle invalid" error.
func TestJob_CloseIdempotent(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	if err := job.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := job.Close(); err != nil {
		t.Errorf("second Close: %v (must be no-op)", err)
	}
}

// TestJob_TerminateAllWaitsForExit is the load-bearing regression
// guard for ADR-#239 (per-task Job Object). Pre-ADR the TerminateAll
// stub returned immediately after TerminateJobObject, racing the
// kernel teardown — supervisor backoff respawn could rebind the
// port before the orphan socket was released. This test proves the
// post-ADR contract: TerminateAll(timeoutMs > 0) must NOT return
// until ActiveProcesses reaches zero OR the deadline expires AND
// every per-PID handle is signaled.
//
// Method: spawn ping (Windows builtin, console-free sleep proxy),
// assign to its own Job (mirrors the per-spawn allocation in
// runSupervise), call TerminateAll(5000), assert (a) call returned
// without timeout error, (b) wall-clock elapsed under the deadline,
// (c) the PID is no longer alive.
//
// Uses `ping -n 30 -w 1000 127.0.0.1` instead of `timeout.exe`
// because timeout.exe exits immediately with "Input redirection is
// not supported" when stdin is redirected/backgrounded (the
// default exec.Cmd behavior in `go test` runs with no controlling
// console attached). Closing bot P2 on PR #241. ping does not
// require console input and behaves consistently in CI.
func TestJob_TerminateAllWaitsForExit(t *testing.T) {
	if _, err := exec.LookPath("ping"); err != nil {
		t.Skipf("ping.exe not on PATH: %v", err)
	}
	// `ping -n 30 -w 1000 127.0.0.1` sends 30 echo requests at
	// ~1s intervals = ~30s wall-clock. The Job's TerminateAll
	// must reap the process well before that natural exit.
	cmd := exec.Command("ping", "-n", "30", "-w", "1000", "127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid
	custody := retainJobFixtureCommand(cmd)
	defer cleanupJobFixtureCommand(t, custody)

	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()

	if err := job.Assign(cmd); err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if !processAlive(pid, t) {
		t.Fatalf("pid=%d already dead before TerminateAll — assignment must not pre-kill (or ping exited early — unexpected)", pid)
	}

	start := time.Now()
	if err := job.TerminateAll(5000); err != nil {
		t.Fatalf("TerminateAll: %v (regression: real polling broken OR ActiveProcesses query failed)", err)
	}
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("TerminateAll took %v; should return well within 5s for one ping child (regression: returned timeout when it should have completed)", elapsed)
	}
	if processAlive(pid, t) {
		t.Errorf("pid=%d still alive after TerminateAll returned; the wait-for-exit contract is broken (caller will race respawn against still-tearing-down orphan)", pid)
	}
}

// TestJob_TerminateAllZeroTimeoutSkipsWait guards the explicit
// opt-out-of-wait path documented in jobobject_windows.go. Callers
// that pass timeoutMs=0 want the pre-ADR fire-and-return semantic
// (e.g. shutdown paths where the kernel will reap on process exit
// anyway). A future refactor accidentally making zero-timeout wait
// forever would deadlock supervisor shutdown — this test catches that.
func TestJob_TerminateAllZeroTimeoutSkipsWait(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.Close()

	start := time.Now()
	if err := job.TerminateAll(0); err != nil {
		t.Errorf("TerminateAll(0) on empty job: %v; want nil (opt-out-of-wait path)", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("TerminateAll(0) took %v; want near-instant return (regression: zero-timeout path is waiting when it should not)", elapsed)
	}
}

func TestJob_TerminateAllTimedNilOrClosedRejects(t *testing.T) {
	var nilJob *Job
	if err := nilJob.TerminateAll(1); err == nil {
		t.Fatal("timed nil Job falsely proved settlement")
	}
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Close(); err != nil {
		t.Fatal(err)
	}
	if err := job.TerminateAll(1); err == nil {
		t.Fatal("timed closed Job falsely proved settlement")
	}
}

func TestJob_TerminateAllTimedObservationFaultsNeverProveEmpty(t *testing.T) {
	if os.Getenv("MCPHUB_JOB_PROOF_CHILD") == "1" {
		time.Sleep(time.Minute)
		return
	}
	queryReal := func(j *Job) (jobObjectBasicAccountingInformation, error) {
		var info jobObjectBasicAccountingInformation
		var used uint32
		err := queryJobInformation(j.Handle(), windows.JobObjectBasicAccountingInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), &used)
		return info, err
	}
	fault := errors.New("test-only Job observation fault")
	for _, tc := range []struct {
		name, stage string
		configure   func(*jobTerminationOps, uint32)
		wantTimeout bool
		timeoutMs   uint32
	}{
		{name: "enumeration_error", stage: "member_list", configure: func(o *jobTerminationOps, _ uint32) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{}, fault }
		}},
		{name: "partial_list", stage: "member_list", configure: func(o *jobTerminationOps, p uint32) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{Assigned: 2, PIDs: []uint32{p}}, nil }
		}},
		{name: "malformed_zero_pid", stage: "member_list", configure: func(o *jobTerminationOps, _ uint32) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{Assigned: 1, PIDs: []uint32{0}}, nil }
		}},
		{name: "empty_list", stage: "member_list", configure: func(o *jobTerminationOps, _ uint32) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{}, nil }
		}},
		{name: "duplicate_pid", stage: "member_list", configure: func(o *jobTerminationOps, p uint32) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{Assigned: 2, PIDs: []uint32{p, p}}, nil }
		}},
		{name: "member_open", stage: "member_handle", configure: func(o *jobTerminationOps, _ uint32) {
			o.openProcess = func(uint32) (windows.Handle, error) { return 0, fault }
		}},
		{name: "member_identity_error", stage: "member_identity", configure: func(o *jobTerminationOps, _ uint32) {
			o.isMember = func(windows.Handle, *Job) (bool, error) { return false, fault }
		}},
		{name: "member_nonmember", stage: "member_identity", configure: func(o *jobTerminationOps, _ uint32) {
			o.isMember = func(windows.Handle, *Job) (bool, error) { return false, nil }
		}},
		{name: "wait_error", stage: "member_wait", configure: func(o *jobTerminationOps, _ uint32) {
			o.wait = func(windows.Handle) (uint32, error) { return uint32(windows.WAIT_FAILED), fault }
		}},
		{name: "wait_unexpected", stage: "member_wait", configure: func(o *jobTerminationOps, _ uint32) {
			o.wait = func(windows.Handle) (uint32, error) { return uint32(windows.WAIT_ABANDONED), nil }
		}},
		{name: "initial_accounting", stage: "snapshot", configure: func(o *jobTerminationOps, _ uint32) {
			o.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
				return jobObjectBasicAccountingInformation{}, fault
			}
		}},
		{name: "final_accounting", stage: "accounting", configure: func(o *jobTerminationOps, _ uint32) {
			calls := 0
			o.accounting = func(j *Job) (jobObjectBasicAccountingInformation, error) {
				calls++
				if calls > 1 {
					return jobObjectBasicAccountingInformation{}, fault
				}
				return queryReal(j)
			}
		}},
		{name: "lifetime_total_drift", stage: "membership_changed", configure: func(o *jobTerminationOps, _ uint32) {
			calls := 0
			o.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
				calls++
				if calls == 1 {
					return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 1}, nil
				}
				return jobObjectBasicAccountingInformation{TotalProcesses: 2, ActiveProcesses: 0}, nil
			}
		}},
		{name: "handle_close", stage: "handle_close", timeoutMs: 5000, configure: func(o *jobTerminationOps, _ uint32) {
			o.closeProcess = func(h windows.Handle) error { _ = windows.CloseHandle(h); return fault }
		}},
		{name: "zero_accounting_unsignaled_handle", stage: "member_wait", wantTimeout: true, configure: func(o *jobTerminationOps, _ uint32) {
			calls := 0
			o.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
				calls++
				if calls == 1 {
					return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 1}, nil
				}
				return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 0}, nil
			}
			o.terminate = func(*Job) error { return nil }
			o.wait = func(windows.Handle) (uint32, error) { return uint32(windows.WAIT_TIMEOUT), nil }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := NewKillOnCloseJob()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestJob_TerminateAllTimedObservationFaultsNeverProveEmpty$")
			cmd.Env = append(os.Environ(), "MCPHUB_JOB_PROOF_CHILD=1")
			custody := startJobFixtureCommandInJob(t, job, cmd)
			defer func() {
				if err := job.Close(); err != nil {
					t.Errorf("fixture Job close: %v", err)
				}
				cleanupJobFixtureCommand(t, custody)
				if cmd.Process != nil && IsPidAlive(cmd.Process.Pid) {
					t.Errorf("test child %d survived", cmd.Process.Pid)
				}
			}()
			pids, err := job.processIDs()
			if err != nil || len(pids) != 1 {
				t.Fatalf("owned Job members=%v err=%v", pids, err)
			}
			ops := &jobTerminationOps{}
			kills, opens, closes := 0, 0, 0
			ops.terminate = func(j *Job) error { return windows.TerminateJobObject(j.Handle(), 1) }
			ops.openProcess = func(pid uint32) (windows.Handle, error) {
				opens++
				return windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
			}
			ops.closeProcess = func(h windows.Handle) error { return windows.CloseHandle(h) }
			tc.configure(ops, pids[0])
			terminate := ops.terminate
			ops.terminate = func(j *Job) error { kills++; return terminate(j) }
			closeProcess := ops.closeProcess
			ops.closeProcess = func(h windows.Handle) error { closes++; return closeProcess(h) }
			job.terminationOps = ops
			timeoutMs := tc.timeoutMs
			if timeoutMs == 0 {
				timeoutMs = 1
			}
			err = job.TerminateAll(timeoutMs)
			var proof *JobTerminationProofError
			if !errors.As(err, &proof) || proof.Stage != tc.stage {
				t.Fatalf("fault=%s error=%v want stage=%s", tc.name, err, tc.stage)
			}
			if tc.name == "handle_close" && !errors.Is(err, fault) {
				t.Fatalf("injected handle-close cause was lost: %v", err)
			}
			if tc.wantTimeout && !errors.Is(err, ErrCleanupTimeout) {
				t.Fatalf("lost cleanup-timeout identity: %v", err)
			}
			if kills != 1 {
				t.Fatalf("observation fault skipped Job termination attempt: kills=%d", kills)
			}
			if tc.name != "member_open" && tc.name != "initial_accounting" && tc.name != "partial_list" && tc.name != "malformed_zero_pid" && tc.name != "empty_list" && tc.name != "duplicate_pid" && opens != closes {
				t.Fatalf("owned member handles not closed: opens=%d closes=%d", opens, closes)
			}
		})
	}
}

func TestJob_ContainedCurrentTreeDoesNotGrantStrictLifetimeProof(t *testing.T) {
	if os.Getenv("MCPHUB_JOB_CURRENT_TREE_CHILD") == "root" {
		return
	} else if os.Getenv("MCPHUB_JOB_CURRENT_TREE_CHILD") == "member" {
		time.Sleep(time.Minute)
		return
	}
	for _, strict := range []bool{false, true} {
		name := "contained"
		if strict {
			name = "public_strict"
		}
		t.Run(name, func(t *testing.T) {
			job, err := NewKillOnCloseJob()
			if err != nil {
				t.Fatal(err)
			}
			var member *exec.Cmd
			root := exec.Command(os.Args[0], "-test.run=^TestJob_ContainedCurrentTreeDoesNotGrantStrictLifetimeProof$")
			root.Env = append(os.Environ(), "MCPHUB_JOB_CURRENT_TREE_CHILD=root")
			rootCustody := startJobFixtureCommandInJob(t, job, root)
			borrowed, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(root.Process.Pid))
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(borrowed)
			if err := rootCustody.await(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			member = exec.Command(os.Args[0], "-test.run=^TestJob_ContainedCurrentTreeDoesNotGrantStrictLifetimeProof$")
			member.Env = append(os.Environ(), "MCPHUB_JOB_CURRENT_TREE_CHILD=member")
			memberCustody := startJobFixtureCommandInJob(t, job, member)
			defer func() {
				if err := job.Close(); err != nil {
					t.Errorf("fixture Job close: %v", err)
				}
				cleanupJobFixtureCommand(t, memberCustody)
				if IsPidAlive(member.Process.Pid) {
					t.Errorf("test member %d survived", member.Process.Pid)
				}
			}()
			calls := 0
			job.terminationOps = &jobTerminationOps{
				accounting: func(*Job) (jobObjectBasicAccountingInformation, error) {
					calls++
					active := uint32(0)
					if calls == 1 {
						active = 1
					}
					return jobObjectBasicAccountingInformation{TotalProcesses: 3, ActiveProcesses: active}, nil
				},
				listMembers: func(*Job) (jobProcessIDList, error) {
					return jobProcessIDList{Assigned: 1, PIDs: []uint32{uint32(member.Process.Pid)}}, nil
				},
			}
			if strict {
				err = job.TerminateAll(5000)
				var proof *JobTerminationProofError
				if !errors.As(err, &proof) || proof.Stage != "membership_changed" {
					t.Fatalf("public strict err=%v", err)
				}
			} else {
				err = job.terminateForContained(5000, containedJobOriginal{job: job, handle: borrowed, pid: uint32(root.Process.Pid), atCreate: true})
				if err != nil {
					t.Fatalf("contained current tree err=%v", err)
				}
			}
			if calls < 3 {
				t.Fatalf("missing final Job accounting: %d", calls)
			}
		})
	}
}

func TestJob_ContainedCurrentTreeFaultsDoNotProveExit(t *testing.T) {
	fault := errors.New("test-only current-tree observation fault")
	for _, tc := range []struct {
		name, stage string
		configure   func(*jobTerminationOps)
		noOriginal  bool
	}{
		{name: "missing_original", stage: "borrowed_handle", noOriginal: true},
		{name: "empty_without_original", stage: "borrowed_handle", noOriginal: true, configure: func(o *jobTerminationOps) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{}, nil }
		}},
		{name: "member_list", stage: "member_list", configure: func(o *jobTerminationOps) {
			o.listMembers = func(*Job) (jobProcessIDList, error) { return jobProcessIDList{}, fault }
		}},
		{name: "member_open", stage: "member_handle", configure: func(o *jobTerminationOps) { o.openProcess = func(uint32) (windows.Handle, error) { return 0, fault } }},
		{name: "member_identity", stage: "member_identity", configure: func(o *jobTerminationOps) {
			o.isMember = func(windows.Handle, *Job) (bool, error) { return false, fault }
		}},
		{name: "member_wait", stage: "member_wait", configure: func(o *jobTerminationOps) { o.wait = func(windows.Handle) (uint32, error) { return 0, fault } }},
		{name: "handle_close", stage: "handle_close", configure: func(o *jobTerminationOps) {
			o.closeProcess = func(h windows.Handle) error { _ = windows.CloseHandle(h); return fault }
		}},
		{name: "active_stays_nonzero", stage: "member_wait", configure: func(o *jobTerminationOps) {
			o.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
				return jobObjectBasicAccountingInformation{TotalProcesses: 3, ActiveProcesses: 1}, nil
			}
		}},
		{name: "total_drifts", stage: "membership_changed", configure: func(o *jobTerminationOps) {
			n := 0
			o.accounting = func(*Job) (jobObjectBasicAccountingInformation, error) {
				n++
				total := uint32(3)
				if n > 1 {
					total = 4
				}
				return jobObjectBasicAccountingInformation{TotalProcesses: total, ActiveProcesses: 0}, nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := NewKillOnCloseJob()
			if err != nil {
				t.Fatal(err)
			}
			defer job.Close()
			borrowed, err := windows.CreateEvent(nil, 1, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(borrowed)
			member, err := windows.CreateEvent(nil, 1, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			memberOwned := true
			defer func() {
				if memberOwned {
					_ = windows.CloseHandle(member)
				}
			}()
			calls, kills := 0, 0
			ops := &jobTerminationOps{
				accounting: func(*Job) (jobObjectBasicAccountingInformation, error) {
					calls++
					active := uint32(0)
					if calls == 1 {
						active = 1
					}
					return jobObjectBasicAccountingInformation{TotalProcesses: 3, ActiveProcesses: active}, nil
				},
				listMembers: func(*Job) (jobProcessIDList, error) { return jobProcessIDList{Assigned: 1, PIDs: []uint32{42}}, nil },
				openProcess: func(uint32) (windows.Handle, error) { memberOwned = false; return member, nil },
				isMember:    func(windows.Handle, *Job) (bool, error) { return true, nil },
				terminate:   func(*Job) error { kills++; return nil },
			}
			if tc.configure != nil {
				tc.configure(ops)
			}
			job.terminationOps = ops
			original := containedJobOriginal{job: job, handle: borrowed, pid: 7, atCreate: true}
			if tc.noOriginal {
				original = containedJobOriginal{}
			}
			err = job.terminateForContained(20, original)
			var proof *JobTerminationProofError
			if !errors.As(err, &proof) || proof.Stage != tc.stage {
				t.Fatalf("err=%v want stage=%s", err, tc.stage)
			}
			if kills != 1 {
				t.Fatalf("Job kill attempts=%d", kills)
			}
		})
	}
}

func TestJob_TerminateAllLateRealHandleSignalNeverProves(t *testing.T) {
	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestJob_TerminateAllTimedObservationFaultsNeverProveEmpty$")
	cmd.Env = append(os.Environ(), "MCPHUB_JOB_PROOF_CHILD=1")
	custody := startJobFixtureCommandInJob(t, job, cmd)
	defer cleanupJobFixtureCommand(t, custody)
	defer func() {
		if err := job.Close(); err != nil {
			t.Errorf("Job close: %v", err)
		}
	}()
	observedRealSignal := false
	job.terminationOps = &jobTerminationOps{wait: func(h windows.Handle) (uint32, error) {
		time.Sleep(5 * time.Millisecond)
		status, waitErr := windows.WaitForSingleObject(h, 2000)
		if waitErr == nil && status == uint32(windows.WAIT_OBJECT_0) {
			observedRealSignal = true
		}
		return status, waitErr
	}}
	err = job.TerminateAll(1)
	var proof *JobTerminationProofError
	if !observedRealSignal {
		t.Fatalf("test never observed the real original handle signal: %v", err)
	}
	if !errors.As(err, &proof) || proof.Stage != "member_wait" || !errors.Is(err, ErrCleanupTimeout) {
		t.Fatalf("late real handle signal falsely certified timed termination: %v", err)
	}
	if err := job.Close(); err != nil {
		t.Fatal(err)
	}
	if err := custody.await(time.Now().Add(5 * time.Second)); err == nil {
		t.Fatal("test child unexpectedly exited cleanly")
	}
	if cmd.Process == nil || IsPidAlive(cmd.Process.Pid) {
		t.Fatalf("test child survived: %v", cmd.Process)
	}
}

func TestJob_TerminateAllRechecksAccountingAfterOriginalHandleSignal(t *testing.T) {
	fault := errors.New("final accounting observation failed")
	for _, tc := range []struct {
		name, stage string
		finalErr    error
		finalTotal  uint32
	}{
		{name: "lifetime_drift", stage: "membership_changed", finalTotal: 2},
		{name: "query_error", stage: "accounting", finalErr: fault},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := NewKillOnCloseJob()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestJob_TerminateAllTimedObservationFaultsNeverProveEmpty$")
			cmd.Env = append(os.Environ(), "MCPHUB_JOB_PROOF_CHILD=1")
			custody := startJobFixtureCommandInJob(t, job, cmd)
			defer cleanupJobFixtureCommand(t, custody)
			defer func() {
				if err := job.Close(); err != nil {
					t.Errorf("Job close: %v", err)
				}
			}()
			queries, closed := 0, 0
			observedReal := false
			job.terminationOps = &jobTerminationOps{
				accounting: func(*Job) (jobObjectBasicAccountingInformation, error) {
					queries++
					if queries == 1 {
						return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 1}, nil
					}
					if queries == 2 {
						return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 0}, nil
					}
					if tc.finalErr != nil {
						return jobObjectBasicAccountingInformation{}, tc.finalErr
					}
					return jobObjectBasicAccountingInformation{TotalProcesses: tc.finalTotal, ActiveProcesses: 0}, nil
				},
				wait: func(h windows.Handle) (uint32, error) {
					status, waitErr := windows.WaitForSingleObject(h, 2000)
					if waitErr == nil && status == uint32(windows.WAIT_OBJECT_0) {
						observedReal = true
					}
					return status, waitErr
				},
				closeProcess: func(h windows.Handle) error { closed++; return windows.CloseHandle(h) },
			}
			err = job.TerminateAll(5000)
			var proof *JobTerminationProofError
			if !observedReal || queries < 3 || !errors.As(err, &proof) || proof.Stage != tc.stage {
				t.Fatalf("post-signal final account not enforced: real=%v queries=%d err=%v", observedReal, queries, err)
			}
			if tc.finalErr != nil && !errors.Is(err, tc.finalErr) {
				t.Fatalf("final query cause lost: %v", err)
			}
			if closed != 1 {
				t.Fatalf("original handle close count=%d want=1", closed)
			}
			if err := job.Close(); err != nil {
				t.Fatal(err)
			}
			if err := custody.await(time.Now().Add(5 * time.Second)); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("original Wait: %v", err)
				}
			}
			if cmd.Process == nil || IsPidAlive(cmd.Process.Pid) {
				t.Fatalf("owned child survived: %v", cmd.Process)
			}
		})
	}
}

func TestJob_TerminateAllAdaptiveShortWindowObservesRealHandle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		signalAt  time.Duration
		wantProof bool
	}{
		{name: "signal_by_10ms", signalAt: 10 * time.Millisecond, wantProof: true},
		{name: "signal_after_100ms", signalAt: 100 * time.Millisecond, wantProof: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := NewKillOnCloseJob()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestJob_TerminateAllTimedObservationFaultsNeverProveEmpty$")
			cmd.Env = append(os.Environ(), "MCPHUB_JOB_PROOF_CHILD=1")
			custody := startJobFixtureCommandInJob(t, job, cmd)
			defer cleanupJobFixtureCommand(t, custody)
			defer func() {
				if err := job.Close(); err != nil {
					t.Errorf("Job close: %v", err)
				}
			}()
			virtual := time.Unix(0, 0)
			start := virtual
			queries, sleeps, closes := 0, 0, 0
			realSignal := false
			job.terminationOps = &jobTerminationOps{
				now:   func() time.Time { return virtual },
				sleep: func(d time.Duration) { virtual = virtual.Add(d); sleeps++ },
				accounting: func(*Job) (jobObjectBasicAccountingInformation, error) {
					queries++
					if queries == 1 {
						return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 1}, nil
					}
					return jobObjectBasicAccountingInformation{TotalProcesses: 1, ActiveProcesses: 0}, nil
				},
				wait: func(h windows.Handle) (uint32, error) {
					if virtual.Sub(start) < tc.signalAt {
						return uint32(windows.WAIT_TIMEOUT), nil
					}
					status, waitErr := windows.WaitForSingleObject(h, 2000)
					if waitErr == nil && status == uint32(windows.WAIT_OBJECT_0) {
						realSignal = true
					}
					return status, waitErr
				},
				closeProcess: func(h windows.Handle) error { closes++; return windows.CloseHandle(h) },
			}
			err = job.TerminateAll(50)
			if tc.wantProof {
				if err != nil || !realSignal || virtual.Sub(start) >= 50*time.Millisecond || queries < 4 {
					t.Fatalf("fast genuine signal lacked bounded proof: err=%v real=%v elapsed=%v queries=%d", err, realSignal, virtual.Sub(start), queries)
				}
			} else {
				var proof *JobTerminationProofError
				if !errors.As(err, &proof) || proof.Stage != "member_wait" || !errors.Is(err, ErrCleanupTimeout) || realSignal {
					t.Fatalf("slow signal falsely proved: %v real=%v", err, realSignal)
				}
			}
			if queries > 12 || sleeps > 10 || closes != 1 {
				t.Fatalf("poll/handle bounds queries=%d sleeps=%d closes=%d", queries, sleeps, closes)
			}
			if err := job.Close(); err != nil {
				t.Fatal(err)
			}
			if err := custody.await(time.Now().Add(5 * time.Second)); err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("original Wait: %v", err)
				}
			}
			if cmd.Process == nil || IsPidAlive(cmd.Process.Pid) {
				t.Fatalf("owned child survived: %v", cmd.Process)
			}
		})
	}
}

// processAlive returns true if a process with pid is still in the OS
// process table. Uses windows.OpenProcess with SYNCHRONIZE — sufficient
// for liveness probe and tolerates the fast post-exit window where the
// process object exists but is signaled.
func processAlive(pid int, t *testing.T) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Most likely "no such process" once the kernel has reaped it.
		return false
	}
	defer windows.CloseHandle(h)
	// WaitForSingleObject with timeout 0 returns WAIT_OBJECT_0 if the
	// process has exited (process handle is signaled on exit).
	ev, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	return ev != uint32(windows.WAIT_OBJECT_0) // alive if NOT signaled
}

// silence unused-import on builds that strip the strconv usage.
var _ = strconv.Itoa
var _ = os.Stderr

// These source guards enforce the documented pointer conversion boundary;
// they do not stand in for compiler retention or native buffer receiving.
func TestJobInformationTransportSourceContract(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("test source location unavailable")
	}
	nativeSites := 0
	fixtureNativeSites := 0
	seams := 0
	procedures := 0
	for _, name := range []string{"jobobject_windows.go", "current_job_windows.go", "jobobject_windows_test.go", "job_options_windows_test.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(source), name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			if group, ok := declaration.(*ast.GenDecl); ok {
				for _, specification := range group.Specs {
					values, ok := specification.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, identifier := range values.Names {
						expected := map[string]string{
							"jobInformationDLL":             "windows.NewLazySystemDLL(\"kernel32.dll\")",
							"procQueryInformationJobObject": "jobInformationDLL.NewProc(\"QueryInformationJobObject\")",
							"procSetInformationJobObject":   "jobInformationDLL.NewProc(\"SetInformationJobObject\")",
						}[identifier.Name]
						if expected == "" {
							continue
						}
						procedures++
						if values.Type != nil || i >= len(values.Values) || jobInformationExpression(values.Values[i]) != expected {
							t.Errorf("%s must resolve through the concrete system LazyProc owner", identifier.Name)
						}
					}
				}
			}
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if function.Name.Name == "jobFixtureTypedGCSet" || function.Name.Name == "jobFixtureTypedGCQuery" {
				ast.Inspect(function.Body, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					callee, ok := call.Fun.(*ast.Ident)
					if ok && (callee.Name == "queryJobInformation" || callee.Name == "setJobInformation") {
						fixtureNativeSites++
					}
					return true
				})
			}
			if function.Name.Name == "queryJobInformation" || function.Name.Name == "setJobInformation" {
				seams++
				if err := checkJobInformationTransport(function); err != nil {
					t.Errorf("%s: %v", function.Name.Name, err)
				}
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
				if owner, ok := selector.X.(*ast.Ident); ok && owner.Name == "windows" && (selector.Sel.Name == "QueryInformationJobObject" || selector.Sel.Name == "SetInformationJobObject") {
					t.Errorf("%s retains an unannotated Job information buffer call", name)
				}
			}
			if callee, ok := call.Fun.(*ast.Ident); ok && (callee.Name == "queryJobInformation" || callee.Name == "setJobInformation") {
				nativeSites++
				if len(call.Args) < 3 || !strings.HasPrefix(jobInformationExpression(call.Args[2]), "unsafe.Pointer(") {
					t.Errorf("%s converts its information buffer before the typed seam", name)
				}
			}
			return true
		})
	}
	if seams != 2 || nativeSites-fixtureNativeSites != 6 || fixtureNativeSites != 2 || procedures != 3 {
		t.Fatalf("typed seams=%d native caller sites=%d procedure owners=%d, want 2, 8 (six existing plus two concrete fixture stress sites) and 3", seams, nativeSites, procedures)
	}
}

func checkJobInformationTransport(function *ast.FuncDecl) error {
	query := function.Name.Name == "queryJobInformation"
	procedure := "procSetInformationJobObject"
	class := "uint32"
	if query {
		procedure, class = "procQueryInformationJobObject", "int32"
	}
	parameters := map[string]string{}
	for _, field := range function.Type.Params.List {
		for _, name := range field.Names {
			parameters[name.Name] = jobInformationExpression(field.Type)
		}
	}
	for name, expected := range map[string]string{"job": "windows.Handle", "class": class, "information": "unsafe.Pointer", "bytes": "uint32"} {
		if parameters[name] != expected {
			return fmt.Errorf("%s must remain typed as %s", name, expected)
		}
	}
	arguments := []string{"uintptr(job)", "uintptr(class)", "uintptr(information)", "uintptr(bytes)"}
	if query {
		if parameters["returnedBytes"] != "*uint32" {
			return fmt.Errorf("returnedBytes must remain *uint32")
		}
		arguments = append(arguments, "uintptr(unsafe.Pointer(returnedBytes))")
	}
	calls := 0
	var violation error
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || jobInformationExpression(call.Fun) != procedure+".Call" {
			return true
		}
		calls++
		if call.Ellipsis.IsValid() || len(call.Args) != len(arguments) {
			violation = fmt.Errorf("native call must use %d direct arguments", len(arguments))
			return false
		}
		for i, argument := range call.Args {
			if jobInformationExpression(argument) != arguments[i] {
				violation = fmt.Errorf("native argument %d must convert at the annotated Call", i)
			}
		}
		return true
	})
	if calls != 1 {
		return fmt.Errorf("concrete annotated Call count=%d, want 1", calls)
	}
	return violation
}

func jobInformationExpression(node ast.Node) string {
	var text strings.Builder
	if err := format.Node(&text, token.NewFileSet(), node); err != nil {
		return ""
	}
	return text.String()
}

func TestJobInformationTransportPreparationBoundary(t *testing.T) {
	arguments := "uintptr(job), uintptr(class), uintptr(information), uintptr(bytes), uintptr(unsafe.Pointer(returnedBytes))"
	for _, fixture := range []struct {
		name, body string
		valid      bool
	}{
		{"typed-preparation-before-annotated-call", "prepare(information, returnedBytes); procQueryInformationJobObject.Call(" + arguments + ")", true},
		{"preconverted-information", "saved := uintptr(information); procQueryInformationJobObject.Call(uintptr(job), uintptr(class), saved, uintptr(bytes), uintptr(unsafe.Pointer(returnedBytes)))", false},
		{"preconverted-returned-length", "saved := uintptr(unsafe.Pointer(returnedBytes)); procQueryInformationJobObject.Call(uintptr(job), uintptr(class), uintptr(information), uintptr(bytes), saved)", false},
		{"argument-slice", "args := []uintptr{" + arguments + "}; procQueryInformationJobObject.Call(args...)", false},
		{"function-value-trampoline", "call := procQueryInformationJobObject.Call; call(" + arguments + ")", false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// Parse only. This fixture neither invokes prepare nor a native call.
			source := "package process\nfunc prepare(information unsafe.Pointer, returnedBytes *uint32) {}\nfunc queryJobInformation(job windows.Handle, class int32, information unsafe.Pointer, bytes uint32, returnedBytes *uint32) { " + fixture.body + " }"
			file, err := parser.ParseFile(token.NewFileSet(), "typed-buffer-fixture.go", source, 0)
			if err != nil {
				t.Fatal(err)
			}
			function := file.Decls[1].(*ast.FuncDecl)
			if err := checkJobInformationTransport(function); (err == nil) != fixture.valid {
				t.Fatalf("supported boundary=%v, want %v: %v", err == nil, fixture.valid, err)
			}
		})
	}
	t.Run("real-native-typed-gc-and-creator", runJobFixtureTypedGCAndCreator)
}

var jobFixtureProcedureLease sync.Mutex

//go:noinline
func jobFixtureGrowStack(depth int) byte {
	var pad [2048]byte
	pad[depth%len(pad)] = byte(depth)
	if depth > 0 {
		pad[0] ^= jobFixtureGrowStack(depth - 1)
	} else {
		runtime.GC()
	}
	runtime.KeepAlive(&pad)
	return pad[0]
}

//go:noinline
func jobFixtureTypedGCSet(job windows.Handle) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	jobFixtureGrowStack(48)
	_, err := setJobInformation(job, windows.JobObjectExtendedLimitInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)))
	runtime.KeepAlive(&info)
	return err
}

//go:noinline
func jobFixtureTypedGCQuery(job windows.Handle, optionalLength bool) (windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION, uint32, error) {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	var returned uint32
	length := &returned
	if optionalLength {
		length = nil
	}
	jobFixtureGrowStack(48)
	err := queryJobInformation(job, windows.JobObjectExtendedLimitInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), length)
	runtime.KeepAlive(&info)
	runtime.KeepAlive(&returned)
	return info, returned, err
}

type jobFixtureAccountingMismatch struct {
	job           *Job
	accounting    jobObjectBasicAccountingInformation
	members       jobProcessIDList
	membershipErr error
}

func (e *jobFixtureAccountingMismatch) Error() string {
	return fmt.Sprintf("pre-descendant native accounting=%+v", e.accounting)
}

func (e *jobFixtureAccountingMismatch) Unwrap() error { return e.membershipErr }

func stressJobFixtureTypedTransport(job *Job) error {
	jobFixtureProcedureLease.Lock()
	defer jobFixtureProcedureLease.Unlock()
	query, set := procQueryInformationJobObject, procSetInformationJobObject
	dll := windows.NewLazySystemDLL("kernel32.dll")
	procQueryInformationJobObject = dll.NewProc("QueryInformationJobObject")
	procSetInformationJobObject = dll.NewProc("SetInformationJobObject")
	defer func() { procQueryInformationJobObject, procSetInformationJobObject = query, set }()
	if err := jobFixtureTypedGCSet(job.Handle()); err != nil {
		return fmt.Errorf("real typed Set after stack growth/GC: %w", err)
	}
	for _, optional := range []bool{false, true} {
		info, returned, err := jobFixtureTypedGCQuery(job.Handle(), optional)
		if err != nil {
			return fmt.Errorf("real typed Query after stack growth/GC: %w", err)
		}
		if !optional && returned != uint32(unsafe.Sizeof(info)) {
			return fmt.Errorf("real typed returned length=%d want=%d", returned, unsafe.Sizeof(info))
		}
		if info.BasicLimitInformation.LimitFlags != windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE {
			return errors.New("real typed extended flags lost KillOnClose or added breakaway")
		}
	}
	accounting, err := job.queryAccounting()
	if err != nil {
		return err
	}
	if accounting.TotalProcesses != 1 || accounting.ActiveProcesses != 1 {
		members, membershipErr := job.queryProcessIDList()
		return &jobFixtureAccountingMismatch{job: job, accounting: accounting, members: members, membershipErr: membershipErr}
	}
	members, err := job.processIDs()
	if err != nil {
		return err
	}
	if len(members) != 1 {
		return fmt.Errorf("pre-descendant native member decoding=%v", members)
	}
	return nil
}

func runJobFixtureTypedGCAndCreator(t *testing.T) {
	checkJobFixtureEntryCodec(t)
	checkJobFixtureCommandCleanup(t)
	entry := requireJobFixtureEntry(t)
	root := t.TempDir()
	config, err := newJobFixtureCase(root, "native-root", entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishJobFixtureJSON(filepath.Join(root, "case.json"), config, false); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestJobBreakawayHelper$")
	cmd.Env = jobFixtureChildEnvironment("native-root", root)
	p, startErr := StartProvisional(cmd)
	// Keep even a returned post-create failure owner; failure cleanup is
	// separate and cannot mint the genuine natural closure criterion.
	proved := false
	defer func() {
		if p != nil && !proved {
			if err := p.TerminateAndWait(5 * time.Second); err != nil {
				t.Errorf("separate retained Provisional failure cleanup: %v", err)
			}
		}
	}()
	if startErr != nil || p == nil {
		t.Fatalf("real Provisional start: %v", startErr)
	}
	observer, ok := p.owner.(provisionalCreatorObserver)
	if !ok {
		t.Fatal("actual Windows creator observer unavailable")
	}
	binding, err := observer.creatorBinding()
	if err != nil {
		t.Fatal(err)
	}
	var ready jobFixtureNativeFrame
	readyRaw, err := awaitJobFixtureJSON(filepath.Join(root, "root-ready.json"), &ready, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateJobFixtureNativeFrame(config, ready, "native-root", config.Creator); err != nil {
		t.Fatal(err)
	}
	if int(ready.Process.PID) != binding.pid || ready.Process.StartedAt == "" || jobFixtureNativeStartedAt(ready.Process.StartedAt) != binding.startedAt {
		t.Fatal("root ready does not match original creator binding")
	}
	rootMetadata, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, ready.Process.PID)
	if err != nil {
		t.Fatal(err)
	}
	rootIdentityErr := validateJobFixtureObject(rootMetadata, ready.Process, exe, true)
	rootIdentityErr = errors.Join(rootIdentityErr, windows.CloseHandle(rootMetadata))
	if rootIdentityErr != nil {
		t.Fatal(rootIdentityErr)
	}
	t.Logf("JOB_FIXTURE_NATIVE_ROOT_READY json=%s sha256=%s original_creation_bound=true native_image_validated=true", readyRaw, jobFixtureDigest(readyRaw))
	if err := stressJobFixtureTypedTransport(p.job); err != nil {
		var mismatch *jobFixtureAccountingMismatch
		if errors.As(err, &mismatch) {
			sameOwner := mismatch.job == p.job
			rootPresent := "unknown"
			if sameOwner && mismatch.membershipErr == nil {
				rootPresent = "absent"
				for _, pid := range mismatch.members.PIDs {
					if pid == uint32(binding.pid) {
						rootPresent = "present"
						break
					}
				}
			}
			// Private bounded diagnostic only: sequential observations of the
			// same held Job. Member IDs are data, never creation/kill authority.
			t.Logf("JOB_FIXTURE_PRE_DESCENDANT_MEMBERSHIP accounting=%+v same_owner=%t assigned=%d listed=%d pids=%v root_pid=%d root_started_at=%q root_present=%s membership_error=%v sequential=true", mismatch.accounting, sameOwner, mismatch.members.Assigned, len(mismatch.members.PIDs), mismatch.members.PIDs, binding.pid, binding.startedAt, rootPresent, mismatch.membershipErr)
		}
		t.Fatal(err)
	}
	// Procedures are restored before any wait/observer/descendant phase.
	if err := publishJobFixtureJSON(filepath.Join(root, "root-release.json"), jobFixtureControl{1, config.Nonce, "release"}, false); err != nil {
		t.Fatal(err)
	}
	var childReady jobFixtureNativeFrame
	childReadyRaw, err := awaitJobFixtureJSON(filepath.Join(root, "descendant-ready.json"), &childReady, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateJobFixtureNativeFrame(config, childReady, "native-descendant", ready.Process); err != nil {
		t.Fatal(err)
	}
	childHandle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, childReady.Process.PID)
	if err != nil {
		t.Fatal(err)
	}
	childClosed := false
	defer func() {
		if !childClosed {
			if err := windows.CloseHandle(childHandle); err != nil {
				t.Errorf("observed native descendant handle close: %v", err)
			}
		}
	}()
	if err := validateJobFixtureObject(childHandle, childReady.Process, exe, true); err != nil {
		t.Fatal(err)
	}
	member, err := jobFixtureObjectMember(childHandle, p.job.Handle())
	if err != nil || !member {
		t.Fatalf("actual descendant membership: member=%v err=%v", member, err)
	}
	t.Logf("JOB_FIXTURE_NATIVE_DESCENDANT_READY json=%s sha256=%s original_job_member=true native_generation_image_validated=true", childReadyRaw, jobFixtureDigest(childReadyRaw))
	select {
	case <-p.wait():
	case <-time.After(5 * time.Second):
		t.Fatal("original producer Wait did not complete")
	}
	exit := observer.creatorExitObservation()
	if !p.waitResult.exited || p.waitResult.err != nil || !exit.known || exit.code != 17 || exit.raw == nil || exit.raw.ProcessState == nil || exit.raw.ProcessState != cmd.ProcessState || exit.raw.ProcessState.ExitCode() != 17 {
		t.Fatalf("actual creator exit17/raw Wait missing: result=%+v observation=%+v", p.waitResult, exit)
	}
	if err := verifyJobFixtureOutput(filepath.Join(root, "root.out"), "ROOT_READY\nROOT_CLOSED\n"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	proof, blockedErr := p.WaitCreatorClosure(ctx)
	cancel()
	if proof != nil || !errors.Is(blockedErr, context.DeadlineExceeded) || p.closeAttempted {
		t.Fatalf("held descendant falsely yielded creator closure: proof=%v error=%v", proof, blockedErr)
	}
	afterBinding, err := observer.creatorBinding()
	if err != nil || afterBinding != binding {
		t.Fatalf("canceled observation lost original custody: %v", err)
	}
	accounting, err := p.job.queryAccounting()
	if err != nil || accounting.TotalProcesses != 2 || accounting.ActiveProcesses != 1 {
		t.Fatalf("held native descendant accounting=%+v error=%v", accounting, err)
	}
	members, err := p.job.processIDs()
	if err != nil || len(members) != 1 || members[0] != childReady.Process.PID {
		t.Fatalf("held native descendant census=%v error=%v", members, err)
	}
	if err := validateJobFixtureObject(childHandle, childReady.Process, exe, true); err != nil {
		t.Fatal(err)
	}
	if err := publishJobFixtureJSON(filepath.Join(root, "descendant-release.json"), jobFixtureControl{1, config.Nonce, "release"}, false); err != nil {
		t.Fatal(err)
	}
	status, err := windows.WaitForSingleObject(childHandle, 5000)
	if err != nil || status != uint32(windows.WAIT_OBJECT_0) {
		t.Fatalf("released actual descendant wait=%#x error=%v", status, err)
	}
	var childExit uint32
	if err := windows.GetExitCodeProcess(childHandle, &childExit); err != nil || childExit != 0 {
		t.Fatalf("released descendant did not exit naturally with success: code=%d error=%v", childExit, err)
	}
	if err := windows.CloseHandle(childHandle); err != nil {
		t.Fatal(err)
	}
	childClosed = true
	var closed jobFixtureNativeFrame
	closedRaw, err := awaitJobFixtureJSON(filepath.Join(root, "descendant-closed.json"), &closed, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateJobFixtureNativeFrame(config, closed, "native-descendant-closed", ready.Process); err != nil || closed.Process != childReady.Process {
		t.Fatalf("actual descendant closure frame mismatch: %v", err)
	}
	if err := verifyJobFixtureOutput(filepath.Join(root, "descendant.out"), "DESCENDANT_READY\nDESCENDANT_CLOSED\n"); err != nil {
		t.Fatal(err)
	}
	t.Logf("JOB_FIXTURE_NATIVE_DESCENDANT_CLOSED json=%s sha256=%s native_exit=0 output_exclusive_closed=true", closedRaw, jobFixtureDigest(closedRaw))
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proof, err = p.WaitCreatorClosure(ctx)
	if err != nil || proof == nil {
		t.Fatalf("actual creator closure: %v", err)
	}
	snapshot := proof.Snapshot()
	if proof.owner != p || snapshot.OriginalPID != binding.pid || snapshot.OriginalStartedAt != binding.startedAt || !snapshot.DirectWaitComplete || !snapshot.CurrentMembersExited || !snapshot.HandlesClosed || !snapshot.OriginalExitCodeKnown || snapshot.OriginalExitCode != 17 || snapshot.RawExitError == nil || snapshot.RawExitError.ProcessState != cmd.ProcessState || snapshot.RawExitError.ProcessState.ExitCode() != 17 || !jobFixtureHex(snapshot.EvidenceSHA256, 64) || !p.settled {
		t.Fatalf("genuine snapshot omitted original17/Wait/current closure: %+v", snapshot)
	}
	proved = true
	t.Logf("JOB_FIXTURE_NATIVE_ORIGINAL_SNAPSHOT pid=%d startedAt=%q evidenceSha256=%s exit=17 wait=1 current_members_exited=true handles_closed=true forced=false", snapshot.OriginalPID, snapshot.OriginalStartedAt, snapshot.EvidenceSHA256)
	t.Log("NATIVE_TYPED_CREATOR root=1 descendant=1 exit=17 wait=1 held_observation=no_proof released_observation=genuine current_members_exited=true original_handles_closed=true output_exclusive_closed=true forced=false")
}

func checkJobFixtureEntryCodec(t *testing.T) {
	t.Helper()
	// This exact 44A specimen is codec evidence only: it never opens entry or
	// constructs an owner/snapshot. The genuine exchange still follows below.
	const specimen = `{"v":1,"kind":"grant","bind":{"run":"synthetic-outer-entry-01","nonce":"11111111111111111111111111111111","ownerSource":"2222222222222222222222222222222222222222222222222222222222222222","controllerSource":"3333333333333333333333333333333333333333333333333333333333333333","packet":"4444444444444444444444444444444444444444444444444444444444444444","binary":"5555555555555555555555555555555555555555555555555555555555555555"},"challenge":"66666666666666666666666666666666","pid":4242,"created100ns":"133700000000000001","ownerGeneration":"77777777777777777777777777777777","entry":{"class":9,"bytes":144,"returned":144,"flags":8192,"member":true,"alive":true},"confirm":true}`
	if len(specimen) != 675 || jobFixtureDigest([]byte(specimen)) != "8bb7f2f3d72298c14028c5c5ba8924bfe657e116ea1ae78507f09cd98f4cef93" {
		t.Fatal("frozen cross-language grant specimen drift")
	}
	var grant jobFixtureEntryGrant
	if err := decodeJobFixtureJSON([]byte(specimen), &grant); err != nil {
		t.Fatal(err)
	}
	config := jobFixtureEntryConfig{Bind: grant.Bind, ExpectedFlags: 8192, EntryBytes: 144}
	ready := jobFixtureEntryReady{1, "ready", grant.Bind, grant.Challenge, grant.PID, grant.Created100ns}
	observedNow := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	deadline := observedNow.Add(10 * time.Second)
	if err := validateJobFixtureGrant(config, ready, grant, deadline, observedNow); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		strings.Replace(specimen, `"confirm":true`, `"confirm":false`, 1),
		strings.Replace(specimen, `"flags":8192`, `"flags":0`, 1),
		strings.Replace(specimen, `"returned":144`, `"returned":0`, 1),
		strings.Replace(specimen, `"member":true`, `"member":false`, 1),
		strings.Replace(specimen, `"alive":true`, `"alive":false`, 1),
		strings.Replace(specimen, `"pid":4242`, `"pid":0`, 1),
		strings.Replace(specimen, `"v":1`, `"v":2`, 1),
		strings.Replace(specimen, `"kind":"grant"`, `"kind":"ack"`, 1),
		strings.Replace(specimen, `"created100ns":"133700000000000001"`, `"created100ns":"0133700000000000001"`, 1),
		strings.Replace(specimen, `"confirm":true`, `"confirm":null`, 1),
		strings.Replace(specimen, `"confirm":true`, `"confirm":true,"confirm":true`, 1),
		strings.Replace(specimen, `"bytes":144`, `"bytes":144,"bytes":144`, 1),
		strings.Replace(specimen, `"pid":4242,`, ``, 1),
		strings.Replace(specimen, `"pid":4242`, `"pid":4294967296`, 1),
		strings.Replace(specimen, `"pid":4242`, `"pid":4242.0`, 1),
		strings.Replace(specimen, `"confirm":true`, `"confirm":true,"unknown":1`, 1),
		strings.Replace(specimen, `"confirm":true`, `"confirm":true,"Confirm":true`, 1),
		specimen + ` {}`, specimen[:len(specimen)-1], strings.Repeat(" ", jobFixtureFrameLimit+1),
	} {
		var invalid jobFixtureEntryGrant
		err := decodeJobFixtureJSON([]byte(raw), &invalid)
		if err == nil {
			err = validateJobFixtureGrant(config, ready, invalid, deadline, observedNow)
		}
		if err == nil {
			t.Fatal("degraded entry JSON/binding falsely accepted")
		}
		assertJobFixtureCompletionRefuses(t, config, ready, []byte(raw), observedNow, deadline)
	}
	for _, change := range []func(*jobFixtureEntryGrant){
		func(g *jobFixtureEntryGrant) { g.Bind.Binary = strings.Repeat("a", 64) },
		func(g *jobFixtureEntryGrant) { g.Bind.OwnerSource = strings.Repeat("a", 64) },
		func(g *jobFixtureEntryGrant) { g.Bind.ControllerSource = strings.Repeat("a", 64) },
		func(g *jobFixtureEntryGrant) { g.Bind.Packet = strings.Repeat("a", 64) },
		func(g *jobFixtureEntryGrant) { g.Bind.Nonce = strings.Repeat("a", 32) },
		func(g *jobFixtureEntryGrant) { g.Challenge = strings.Repeat("a", 32) },
		func(g *jobFixtureEntryGrant) { g.OwnerGeneration = "" },
		func(g *jobFixtureEntryGrant) { g.Entry.Class = 0 },
		func(g *jobFixtureEntryGrant) { g.Entry.Bytes = 0 },
		func(g *jobFixtureEntryGrant) { g.Entry.Flags |= windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK },
	} {
		invalid := grant
		change(&invalid)
		if err := validateJobFixtureGrant(config, ready, invalid, deadline, observedNow); err == nil {
			t.Fatal("wrong source/policy/generation falsely accepted")
		}
		invalidPayload, err := json.Marshal(invalid)
		if err != nil {
			t.Fatal(err)
		}
		assertJobFixtureCompletionRefuses(t, config, ready, invalidPayload, observedNow, deadline)
	}
	if err := validateJobFixtureGrant(config, ready, grant, observedNow.Add(-time.Second), observedNow); err == nil {
		t.Fatal("expired grant accepted")
	}
	assertJobFixtureCompletionRefuses(t, config, ready, []byte(specimen), observedNow, observedNow.Add(-time.Second))
	if err := validateJobFixtureGrant(jobFixtureEntryConfig{}, jobFixtureEntryReady{}, jobFixtureEntryGrant{}, deadline, observedNow); err == nil {
		t.Fatal("equal-by-default missing entry accepted")
	}
	assertJobFixtureCompletionRefuses(t, jobFixtureEntryConfig{}, jobFixtureEntryReady{}, []byte(specimen), observedNow, deadline)
	checkJobFixtureEntryCompletion(t, config, ready, []byte(specimen), observedNow, deadline)
	checkJobFixturePeerCodec(t)
}

type jobFixtureEntryWriteFunc func([]byte) (int, error)

func (write jobFixtureEntryWriteFunc) Write(frame []byte) (int, error) { return write(frame) }

func assertJobFixtureCompletionRefuses(t *testing.T, config jobFixtureEntryConfig, ready jobFixtureEntryReady, payload []byte, observedNow, deadline time.Time) {
	t.Helper()
	writes := 0
	writer := jobFixtureEntryWriteFunc(func(frame []byte) (int, error) { writes++; return len(frame), nil })
	token, err := completeJobFixtureEntry(config, ready, payload, deadline, writer, func() time.Time { return observedNow })
	if token != nil || err == nil || writes != 0 {
		t.Fatal("invalid completion input reached ACK write/token")
	}
}

func checkJobFixtureEntryCompletion(t *testing.T, config jobFixtureEntryConfig, ready jobFixtureEntryReady, payload []byte, start, deadline time.Time) {
	t.Helper()
	for _, completion := range []time.Time{deadline.Add(-time.Nanosecond), deadline, deadline.Add(time.Nanosecond)} {
		clock := start
		writes := 0
		var observed bytes.Buffer
		writer := jobFixtureEntryWriteFunc(func(frame []byte) (int, error) {
			writes++
			n, err := observed.Write(frame)
			clock = completion
			return n, err
		})
		token, err := completeJobFixtureEntry(config, ready, payload, deadline, writer, func() time.Time { return clock })
		if writes != 1 || observed.Len() == 0 {
			t.Fatal("completion check did not observe one successful full ACK write")
		}
		frame := observed.Bytes()
		if !bytes.HasPrefix(frame, []byte(jobFixtureEntryPrefix)) || frame[len(frame)-1] != '\n' || bytes.Count(frame, []byte{'\n'}) != 1 {
			t.Fatal("completion ACK frame changed")
		}
		var ack jobFixtureEntryAck
		if decodeErr := decodeJobFixtureJSON(frame[len(jobFixtureEntryPrefix):len(frame)-1], &ack); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		expected := jobFixtureEntryAck{1, "ack", ready.Bind.Nonce, ready.Challenge, strings.Repeat("7", 32), jobFixtureDigest(payload), true}
		if ack != expected {
			t.Fatal("completion ACK digest/binding changed")
		}
		if completion.Before(deadline) {
			if err != nil || token == nil || token.Config.Bind != config.Bind || token.Ready != ready || token.Ack != expected {
				t.Fatalf("on-time successful ACK rejected: %v", err)
			}
		} else if token != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("equal/late successful ACK admitted: %v", err)
		}
		// Synthetic token is local evidence only; no cache, Start or owner call.
	}
	writeCause := errors.New("synthetic ACK writer failure")
	for _, cause := range []error{writeCause, io.ErrShortWrite} {
		writes := 0
		clock := start
		writer := jobFixtureEntryWriteFunc(func(frame []byte) (int, error) {
			writes++
			clock = deadline.Add(time.Nanosecond)
			if cause == writeCause {
				return 0, writeCause
			}
			return len(frame) - 1, nil
		})
		token, err := completeJobFixtureEntry(config, ready, payload, deadline, writer, func() time.Time { return clock })
		if writes != 1 || token != nil || !errors.Is(err, cause) {
			t.Fatalf("ACK write failure cause lost: %v", err)
		}
	}
	writes := 0
	writer := jobFixtureEntryWriteFunc(func(frame []byte) (int, error) { writes++; return len(frame), nil })
	for _, missing := range []struct {
		output   io.Writer
		clock    func() time.Time
		deadline time.Time
	}{
		{nil, func() time.Time { return start }, deadline},
		{jobFixtureEntryWriteFunc(nil), func() time.Time { return start }, deadline},
		{writer, nil, deadline},
		{writer, func() time.Time { return time.Time{} }, deadline},
		{writer, func() time.Time { return start }, time.Time{}},
	} {
		token, err := completeJobFixtureEntry(config, ready, payload, missing.deadline, missing.output, missing.clock)
		if token != nil || err == nil || writes != 0 {
			t.Fatal("missing completion input wrote ACK or returned token")
		}
	}
}

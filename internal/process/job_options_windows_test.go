//go:build windows

package process

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	breakawayHelperModeEnv = "MCPHUB_JOB_BREAKAWAY_HELPER_MODE"
	breakawayPIDFileEnv    = "MCPHUB_JOB_BREAKAWAY_PID_FILE"
)

func TestNewJob_ConfiguresKillOnCloseAndBreakaway(t *testing.T) {
	job, err := NewJob(JobOptions{KillOnClose: true, BreakawayOK: true})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	defer job.Close()

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := queryJobInformation(
		job.Handle(),
		windows.JobObjectExtendedLimitInformation,
		unsafe.Pointer(&info),
		uint32(unsafe.Sizeof(info)),
		nil,
	); err != nil {
		t.Fatalf("QueryInformationJobObject: %v", err)
	}
	want := uint32(windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK)
	if got := uint32(info.BasicLimitInformation.LimitFlags) & want; got != want {
		t.Fatalf("job limit flags=%#x, want both %#x", info.BasicLimitInformation.LimitFlags, want)
	}
}

func TestJob_BreakawayEnabledDescendantEscapesClose(t *testing.T) {
	entry := requireJobFixtureEntry(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	config, err := newJobFixtureCase(root, "breakaway-parent", entry)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishJobFixtureJSON(filepath.Join(root, "case.json"), config, false); err != nil {
		t.Fatal(err)
	}
	job, err := NewJob(JobOptions{KillOnClose: true, BreakawayOK: true})
	if err != nil {
		t.Fatalf("NewJob: %v", err)
	}
	jobClosed := false
	defer func() {
		if !jobClosed {
			if err := job.Close(); err != nil {
				t.Errorf("inner Job close: %v", err)
			}
		}
	}()

	parent := exec.Command(exe, "-test.run=^TestJobBreakawayHelper$")
	parent.Env = jobFixtureChildEnvironment("parent", root)
	var parentCustody *jobFixtureCommand
	createdParent := false
	defer func() {
		if parentCustody != nil {
			cleanupJobFixtureCommand(t, parentCustody)
		}
	}()
	adopted := false
	var offer jobFixtureOffer
	var offerRaw []byte
	var descendantHandle windows.Handle
	defer func() {
		if createdParent && !adopted {
			// An abort competes for the same single-assignment decision. It
			// cannot overwrite a committed adopt or invent an acknowledgment.
			if offerRaw != nil {
				abort := jobFixtureDecisionFor(config, offer, offerRaw, jobFixtureIdentity{entry.Ready.PID, entry.Ready.Created100ns}, false)
				winner, decisionRaw, err := commitJobFixtureDecision(root, abort)
				if err == nil && winner.Kind == "abort-creator" {
					var ack jobFixtureHandoffAck
					_, err = awaitJobFixtureJSON(filepath.Join(root, "ack.json"), &ack, time.Now().Add(5*time.Second))
					if err == nil {
						err = validateJobFixtureAck(config, offer, offerRaw, winner, decisionRaw, ack)
					}
					if err == nil && (!ack.WaitComplete || !ack.HandleClosed || ack.Error != "") {
						err = errors.New("creator abort remains incomplete")
					}
				}
				if err != nil || winner.Kind != "abort-creator" {
					t.Errorf("breakaway abort/ack unresolved; original outer backstop required: %v", err)
				}
			} else {
				if err := publishJobFixtureJSON(filepath.Join(root, "abort-request.json"), jobFixtureControl{1, config.Nonce, "abort"}, false); err != nil {
					t.Errorf("abort request: %v", err)
				}
				var receipt jobFixtureAbortReceipt
				_, err := awaitJobFixtureJSON(filepath.Join(root, "abort-cleanup.json"), &receipt, time.Now().Add(5*time.Second))
				if err != nil || receipt.Schema != "job-fixture-abort-v1" || receipt.Nonce != config.Nonce || !receipt.WaitComplete || !receipt.HandleClosed || receipt.Error != "" {
					t.Errorf("pre-offer creator cleanup remains unverified: %v", err)
				}
			}
		}
		if descendantHandle != 0 {
			if adopted {
				if err := windows.TerminateProcess(descendantHandle, 1); err != nil {
					t.Errorf("adopted descendant termination: %v", err)
				}
				status, err := windows.WaitForSingleObject(descendantHandle, 5000)
				if err != nil || status != uint32(windows.WAIT_OBJECT_0) {
					t.Errorf("adopted descendant wait: status=%#x error=%v", status, err)
				}
			}
			if err := windows.CloseHandle(descendantHandle); err != nil {
				t.Errorf("borrowed/adopted descendant close: %v", err)
			}
		}
	}()
	var parentPID int
	var startErr error
	parentCustody, parentPID, startErr = beginJobFixtureCommandInJob(job, parent)
	createdParent = parentPID > 0 || parent.Process != nil
	if startErr != nil || parentCustody == nil {
		t.Fatalf("StartWithJob(parent) original custody: %v", startErr)
	}
	offerRaw, err = awaitJobFixtureJSON(filepath.Join(root, "offer.json"), &offer, time.Now().Add(10*time.Second))
	if err != nil {
		t.Fatalf("breakaway offer: %v", err)
	}
	if err = validateJobFixtureOffer(config, offer); err != nil {
		t.Fatal(err)
	}
	if offer.Creator.PID != uint32(parent.Process.Pid) {
		t.Fatal("offer creator does not name original parent")
	}
	creatorHandle, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, offer.Creator.PID)
	if err != nil {
		t.Fatalf("creator observation: %v", err)
	}
	creatorErr := validateJobFixtureObject(creatorHandle, offer.Creator, exe, true)
	creatorErr = errors.Join(creatorErr, windows.CloseHandle(creatorHandle))
	if creatorErr != nil {
		t.Fatal(creatorErr)
	}
	descendantPID := int(offer.Child.PID)
	descendantHandle, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, offer.Child.PID)
	if err != nil {
		t.Fatalf("OpenProcess(descendant=%d): %v", descendantPID, err)
	}
	if err := validateJobFixtureObject(descendantHandle, offer.Child, exe, true); err != nil {
		t.Fatal(err)
	}

	if !job.HasMember(parent.Process.Pid) {
		t.Fatalf("direct child pid=%d is not a Job member", parent.Process.Pid)
	}
	member, err := jobFixtureObjectMember(descendantHandle, job.Handle())
	if err != nil {
		t.Fatalf("descendant membership: %v", err)
	}
	if member {
		t.Fatalf("breakaway descendant pid=%d remained in the Job", descendantPID)
	}
	// Some Job membership remains true while inner membership is false.
	// This corroborates the qualified no-breakaway ancestor rule; it does
	// not expose or query the controller's original outer Job handle.
	outerMember, err := jobFixtureObjectMember(descendantHandle, 0)
	if err != nil || !outerMember {
		t.Fatalf("descendant lost enclosing membership: member=%v err=%v", outerMember, err)
	}
	t.Logf("JOB_FIXTURE_BREAKAWAY_OFFER json=%s sha256=%s native_creator_generation=true native_child_generation=true inner_member=false any_job_member=true original_reference_owner=creator", offerRaw, jobFixtureDigest(offerRaw))
	decision := jobFixtureDecisionFor(config, offer, offerRaw, jobFixtureIdentity{entry.Ready.PID, entry.Ready.Created100ns}, true)
	winner, decisionRaw, err := commitJobFixtureDecision(root, decision)
	if err != nil || winner != decision {
		t.Fatalf("positive adoption lost single winner: %v", err)
	}
	t.Logf("JOB_FIXTURE_BREAKAWAY_DECISION json=%s sha256=%s adopted_handle_cleanup_armed=true", decisionRaw, jobFixtureDigest(decisionRaw))
	var ack jobFixtureHandoffAck
	ackRaw, err := awaitJobFixtureJSON(filepath.Join(root, "ack.json"), &ack, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatalf("adoption acknowledgment: %v", err)
	}
	if err := validateJobFixtureAck(config, offer, offerRaw, winner, decisionRaw, ack); err != nil {
		t.Fatal(err)
	}
	if ack.CleanupOwner != "recipient" || !ack.CleanupArmed || ack.Forced || ack.WaitComplete || ack.HandleClosed || ack.Error != "" {
		t.Fatal("adoption acknowledgment does not preserve live recipient custody")
	}
	t.Logf("JOB_FIXTURE_BREAKAWAY_ACK json=%s sha256=%s decision_matched=true cleanup_owner=recipient", ackRaw, jobFixtureDigest(ackRaw))
	adopted = true
	if err := awaitJobFixtureControl(filepath.Join(root, "creator-release.json"), config.Nonce, "released", time.Now().Add(5*time.Second)); err != nil {
		t.Fatalf("creator original handle release: %v", err)
	}
	if err := job.Close(); err != nil {
		t.Fatalf("Job.Close: %v", err)
	}
	jobClosed = true
	waitForProcessExit(t, parent.Process.Pid, 5*time.Second)
	if err := parentCustody.await(time.Now().Add(5 * time.Second)); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("original parent Wait: %v", err)
		}
	}

	if status, err := windows.WaitForSingleObject(descendantHandle, 0); err != nil {
		t.Fatalf("probe descendant: %v", err)
	} else if status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("breakaway descendant status=%#x, want WAIT_TIMEOUT (still alive)", status)
	}
	t.Log("BREAKAWAY_CUSTODY offer=validated decision=adopt ack=matched inner_parent_exited=true descendant_survived=true cleanup_owner=recipient")
}

func TestJobBreakawayHelper(t *testing.T) {
	switch os.Getenv(breakawayHelperModeEnv) {
	case "parent":
		runJobFixtureBreakawayCreator(t)
	case "descendant":
		config, root, err := loadJobFixtureCase("breakaway-parent")
		if err != nil {
			t.Fatal(err)
		}
		if err := awaitJobFixtureControl(filepath.Join(root, "descendant-release.json"), config.Nonce, "release", config.until()); err != nil {
			t.Fatal(err)
		}
	case "native-root":
		runJobFixtureNativeRoot(t)
		if !t.Failed() {
			os.Exit(17)
		}
	case "native-descendant":
		runJobFixtureNativeDescendant(t)
	default:
		if mode := os.Getenv(breakawayHelperModeEnv); mode != "" {
			t.Fatalf("unknown private companion mode %q", mode)
		}
	}
}

func waitForProcessExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid, t) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("process pid=%d remained alive after %v", pid, timeout))
}

func TestJobInformationNativeResultMapping(t *testing.T) {
	for _, tc := range []struct {
		name      string
		result    uintptr
		lastError error
		want      error
	}{
		{"success-ignores-stale-error", 1, windows.ERROR_ACCESS_DENIED, nil},
		{"false-with-zero-error", 0, syscall.Errno(0), syscall.EINVAL},
		{"false-preserves-native-error", 0, windows.ERROR_MORE_DATA, windows.ERROR_MORE_DATA},
		{"false-with-missing-error", 0, nil, syscall.EINVAL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobInformationCallError(tc.result, tc.lastError); got != tc.want {
				t.Fatalf("result/error mapping=%v, want %v", got, tc.want)
			}
		})
	}
}

type jobFixtureIdentity struct {
	PID       uint32 `json:"pid"`
	StartedAt string `json:"started_at"`
}

func (i jobFixtureIdentity) valid() bool {
	n, err := strconv.ParseUint(i.StartedAt, 10, 64)
	return i.PID != 0 && err == nil && n != 0 && strconv.FormatUint(n, 10) == i.StartedAt
}

type jobFixtureCase struct {
	Schema       string             `json:"schema"`
	Nonce        string             `json:"nonce"`
	Role         string             `json:"role"`
	SourceSHA256 string             `json:"source_sha256"`
	BinarySHA256 string             `json:"binary_sha256"`
	Creator      jobFixtureIdentity `json:"creator"`
	Entry        jobFixtureEntryAck `json:"entry"`
	Deadline     string             `json:"deadline"`
}

func (c jobFixtureCase) until() time.Time {
	deadline, _ := time.Parse(time.RFC3339Nano, c.Deadline)
	return deadline
}

type jobFixtureOffer struct {
	Schema                string             `json:"schema"`
	Nonce                 string             `json:"nonce"`
	Role                  string             `json:"role"`
	SourceSHA256          string             `json:"source_sha256"`
	BinarySHA256          string             `json:"binary_sha256"`
	Creator               jobFixtureIdentity `json:"creator"`
	Child                 jobFixtureIdentity `json:"child"`
	OriginalReferenceHeld bool               `json:"original_reference_held"`
	Deadline              string             `json:"deadline"`
}
type jobFixtureDecision struct {
	Schema      string             `json:"schema"`
	Nonce       string             `json:"nonce"`
	OfferSHA256 string             `json:"offer_sha256"`
	Kind        string             `json:"kind"`
	Creator     jobFixtureIdentity `json:"creator"`
	Child       jobFixtureIdentity `json:"child"`
	Recipient   jobFixtureIdentity `json:"recipient"`
	Adopt       bool               `json:"adopt"`
}
type jobFixtureHandoffAck struct {
	Schema         string             `json:"schema"`
	Nonce          string             `json:"nonce"`
	OfferSHA256    string             `json:"offer_sha256"`
	DecisionSHA256 string             `json:"decision_sha256"`
	Creator        jobFixtureIdentity `json:"creator"`
	Child          jobFixtureIdentity `json:"child"`
	Recipient      jobFixtureIdentity `json:"recipient"`
	CleanupOwner   string             `json:"cleanup_owner"`
	CleanupArmed   bool               `json:"cleanup_armed"`
	Forced         bool               `json:"forced"`
	WaitComplete   bool               `json:"wait_complete"`
	HandleClosed   bool               `json:"handle_closed"`
	Error          string             `json:"error"`
}
type jobFixtureControl struct {
	V      uint32 `json:"v"`
	Nonce  string `json:"nonce"`
	Action string `json:"action"`
}
type jobFixtureAbortReceipt struct {
	Schema       string `json:"schema"`
	Nonce        string `json:"nonce"`
	Forced       bool   `json:"forced"`
	WaitComplete bool   `json:"wait_complete"`
	HandleClosed bool   `json:"handle_closed"`
	Error        string `json:"error"`
}

func newJobFixtureCase(root, role string, entry *jobFixtureEntryToken) (jobFixtureCase, error) {
	if entry == nil || !entry.Ack.Confirm || !filepath.IsAbs(root) {
		return jobFixtureCase{}, errors.New("case has no qualified original entry")
	}
	nonce, err := jobFixtureNonce()
	if err != nil {
		return jobFixtureCase{}, err
	}
	return jobFixtureCase{"job-fixture-case-v1", nonce, role, entry.Config.Bind.Packet, entry.Config.Bind.Binary, jobFixtureIdentity{entry.Ready.PID, entry.Ready.Created100ns}, entry.Ack, time.Now().Add(30 * time.Second).UTC().Format(time.RFC3339Nano)}, nil
}
func jobFixtureChildEnvironment(mode, root string) []string {
	var env []string
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(key) {
		case breakawayHelperModeEnv, breakawayPIDFileEnv, jobFixtureEntryConfigEnv, "MCPHUB_JOB_PROOF_CHILD", "MCPHUB_JOB_CURRENT_TREE_CHILD", "MCPHUB_PROVISIONAL_CLOSURE_PROOF_ROOT":
			continue
		}
		env = append(env, value)
	}
	return append(env, breakawayHelperModeEnv+"="+mode, breakawayPIDFileEnv+"="+root)
}
func loadJobFixtureCase(role string) (jobFixtureCase, string, error) {
	var c jobFixtureCase
	root := os.Getenv(breakawayPIDFileEnv)
	if !filepath.IsAbs(root) {
		return c, root, errors.New("private companion control root required")
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return c, root, errors.Join(err, errors.New("private companion root is not ordinary"))
	}
	raw, err := jobFixtureReadFile(filepath.Join(root, "case.json"))
	if err != nil {
		return c, root, err
	}
	if err := decodeJobFixtureJSON(raw, &c); err != nil {
		return c, root, err
	}
	if c.Schema != "job-fixture-case-v1" || c.Role != role || !jobFixtureHex(c.Nonce, 32) || !jobFixtureHex(c.SourceSHA256, 64) || !jobFixtureHex(c.BinarySHA256, 64) || !c.Creator.valid() || !time.Now().Before(c.until()) || time.Until(c.until()) > 30*time.Second || c.Entry.V != 1 || c.Entry.Kind != "ack" || !c.Entry.Confirm || !jobFixtureHex(c.Entry.Nonce, 32) || !jobFixtureHex(c.Entry.Challenge, 32) || !jobFixtureHex(c.Entry.OwnerGeneration, 32) || !jobFixtureHex(c.Entry.GrantSHA256, 64) {
		return c, root, errors.New("companion binding/role/deadline/positive entry missing")
	}
	exe, err := os.Executable()
	if err != nil {
		return c, root, err
	}
	actual, err := jobFixtureFileDigest(exe)
	if err != nil || actual != c.BinarySHA256 {
		return c, root, errors.Join(err, errors.New("companion binary mismatch"))
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, c.Creator.PID)
	if err != nil {
		return c, root, err
	}
	err = errors.Join(validateJobFixtureObject(h, c.Creator, exe, true), windows.CloseHandle(h))
	if err != nil {
		return c, root, fmt.Errorf("companion original parent generation: %w", err)
	}
	return c, root, nil
}
func publishJobFixtureJSON(path string, value any, exclusive bool) (resultErr error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > jobFixtureFrameLimit {
		return errors.New("control frame oversized")
	}
	if exclusive {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		n, writeErr := f.Write(raw)
		if n != len(raw) {
			writeErr = errors.Join(writeErr, io.ErrShortWrite)
		}
		return errors.Join(writeErr, f.Sync(), f.Close())
	}
	// One producer owns each ordinary leaf; immutable publication uses a
	// flushed/closed sibling followed by rename and exact readback.
	if _, err := os.Lstat(path); err == nil {
		return errors.New("control leaf already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".publish-")
	if err != nil {
		return err
	}
	temp := f.Name()
	removed := false
	defer func() {
		if !removed {
			if err := os.Remove(temp); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, fmt.Errorf("owned publication sibling cleanup: %w", err))
			}
		}
	}()
	n, writeErr := f.Write(raw)
	if n != len(raw) {
		writeErr = errors.Join(writeErr, io.ErrShortWrite)
	}
	if err := errors.Join(writeErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	removed = true
	readback, err := jobFixtureReadFile(path)
	if err != nil || !bytes.Equal(raw, readback) {
		return errors.Join(err, errors.New("control readback mismatch"))
	}
	return nil
}
func awaitJobFixtureJSON(path string, target any, deadline time.Time) ([]byte, error) {
	for time.Now().Before(deadline) {
		raw, err := jobFixtureReadFile(path)
		if err == nil {
			if err := decodeJobFixtureJSON(raw, target); err != nil {
				return raw, err
			}
			return raw, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, fmt.Errorf("control publication %s: %w", filepath.Base(path), context.DeadlineExceeded)
}
func awaitJobFixtureControl(path, nonce, action string, deadline time.Time) error {
	var c jobFixtureControl
	if _, err := awaitJobFixtureJSON(path, &c, deadline); err != nil {
		return err
	}
	if c.V != 1 || c.Nonce != nonce || c.Action != action {
		return errors.New("control generation or positive action mismatch")
	}
	return nil
}
func jobFixtureObjectIdentity(h windows.Handle) (jobFixtureIdentity, error) {
	pid, err := windows.GetProcessId(h)
	if err != nil {
		return jobFixtureIdentity{}, err
	}
	created, err := jobFixtureCreationTime(h)
	return jobFixtureIdentity{pid, created}, err
}
func validateJobFixtureObject(h windows.Handle, expected jobFixtureIdentity, exe string, alive bool) error {
	if h == 0 || h == windows.InvalidHandle || !expected.valid() {
		return errors.New("invalid borrowed object or native identity")
	}
	actual, err := jobFixtureObjectIdentity(h)
	if err != nil || actual != expected {
		return errors.Join(err, errors.New("native object generation mismatch"))
	}
	var image [32768]uint16
	size := uint32(len(image))
	if err := windows.QueryFullProcessImageName(h, 0, &image[0], &size); err != nil {
		return err
	}
	if size == 0 || size >= uint32(len(image)) || !strings.EqualFold(filepath.Clean(windows.UTF16ToString(image[:size])), filepath.Clean(exe)) {
		return errors.New("native object image mismatch")
	}
	if alive {
		status, err := windows.WaitForSingleObject(h, 0)
		if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
			return errors.Join(err, errors.New("native object is not live"))
		}
	}
	return nil
}
func jobFixtureObjectMember(h, job windows.Handle) (bool, error) {
	var member uint32
	r, _, err := procIsProcessInJob.Call(uintptr(h), uintptr(job), uintptr(unsafe.Pointer(&member)))
	if err := jobInformationCallError(r, err); err != nil {
		return false, err
	}
	return member != 0, nil
}
func validateJobFixtureOffer(c jobFixtureCase, o jobFixtureOffer) error {
	if o.Schema != "job-fixture-offer-v1" || o.Nonce != c.Nonce || o.Role != "breakaway-descendant" || o.SourceSHA256 != c.SourceSHA256 || o.BinarySHA256 != c.BinarySHA256 || o.Deadline != c.Deadline || !time.Now().Before(c.until()) || !o.Creator.valid() || !o.Child.valid() || o.Creator.PID == o.Child.PID || !o.OriginalReferenceHeld {
		return errors.New("offer source/generation/original-custody mismatch")
	}
	return nil
}
func jobFixtureDecisionFor(c jobFixtureCase, o jobFixtureOffer, raw []byte, recipient jobFixtureIdentity, adopt bool) jobFixtureDecision {
	kind := "abort-creator"
	if adopt {
		kind = "adopt"
	}
	return jobFixtureDecision{"job-fixture-decision-v1", c.Nonce, jobFixtureDigest(raw), kind, o.Creator, o.Child, recipient, adopt}
}
func validateJobFixtureDecision(c jobFixtureCase, o jobFixtureOffer, raw []byte, d jobFixtureDecision) error {
	if d.Adopt && !time.Now().Before(c.until()) {
		return errors.New("late adopt decision/acknowledgment refused")
	}
	if d.Schema != "job-fixture-decision-v1" || d.Nonce != c.Nonce || d.OfferSHA256 != jobFixtureDigest(raw) || d.Creator != o.Creator || d.Child != o.Child || d.Recipient != c.Creator || !d.Recipient.valid() || d.Kind != "adopt" && d.Kind != "abort-creator" || d.Adopt != (d.Kind == "adopt") {
		return errors.New("single-winner decision binding mismatch")
	}
	return nil
}
func commitJobFixtureDecision(root string, proposal jobFixtureDecision) (jobFixtureDecision, []byte, error) {
	path := filepath.Join(root, "decision.json")
	err := publishJobFixtureJSON(path, proposal, true)
	if err == nil {
		raw, readErr := jobFixtureReadFile(path)
		if readErr != nil {
			return jobFixtureDecision{}, nil, readErr
		}
		// The committed digest is published only after payload flush/close.
		if err := publishJobFixtureJSON(path+".committed", struct {
			SHA256 string `json:"sha256"`
		}{jobFixtureDigest(raw)}, false); err != nil {
			return jobFixtureDecision{}, nil, err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return jobFixtureDecision{}, nil, err
	}
	return readJobFixtureDecision(root, time.Now().Add(5*time.Second))
}
func readJobFixtureDecision(root string, deadline time.Time) (jobFixtureDecision, []byte, error) {
	path := filepath.Join(root, "decision.json")
	var commit struct {
		SHA256 string `json:"sha256"`
	}
	if _, err := awaitJobFixtureJSON(path+".committed", &commit, deadline); err != nil {
		return jobFixtureDecision{}, nil, err
	}
	raw, err := jobFixtureReadFile(path)
	if err != nil {
		return jobFixtureDecision{}, nil, err
	}
	if !jobFixtureHex(commit.SHA256, 64) || jobFixtureDigest(raw) != commit.SHA256 {
		return jobFixtureDecision{}, raw, errors.New("single-winner committed payload mismatch")
	}
	var decision jobFixtureDecision
	err = decodeJobFixtureJSON(raw, &decision)
	return decision, raw, err
}
func validateJobFixtureAck(c jobFixtureCase, o jobFixtureOffer, offerRaw []byte, d jobFixtureDecision, decisionRaw []byte, a jobFixtureHandoffAck) error {
	if err := validateJobFixtureDecision(c, o, offerRaw, d); err != nil {
		return err
	}
	wantOwner := "creator"
	if d.Adopt {
		wantOwner = "recipient"
	}
	if a.Schema != "job-fixture-ack-v1" || a.Nonce != c.Nonce || a.OfferSHA256 != jobFixtureDigest(offerRaw) || a.DecisionSHA256 != jobFixtureDigest(decisionRaw) || a.Creator != o.Creator || a.Child != o.Child || a.Recipient != d.Recipient || a.CleanupOwner != wantOwner || !a.CleanupArmed {
		return errors.New("handoff acknowledgment binding mismatch")
	}
	return nil
}
func runJobFixtureBreakawayCreator(t *testing.T) {
	c, root, err := loadJobFixtureCase("breakaway-parent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "abort-request.json")); err == nil {
		t.Fatal("creator aborted before descendant creation")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=^TestJobBreakawayHelper$")
	child.Env = jobFixtureChildEnvironment("descendant", root)
	child.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_BREAKAWAY_FROM_JOB}
	if err := child.Start(); err != nil {
		t.Fatalf("breakaway original creation: %v", err)
	}
	custody := retainJobFixtureCommand(child)
	transferChosen := false
	defer func() {
		if !transferChosen {
			cleanupErr := custody.cleanup()
			text := ""
			if cleanupErr != nil {
				text = cleanupErr.Error()
				t.Errorf("breakaway creator cleanup: %v", cleanupErr)
			}
			waited := false
			select {
			case <-custody.waitDone:
				waited = true
			default:
			}
			receipt := jobFixtureAbortReceipt{"job-fixture-abort-v1", c.Nonce, true, waited, waited && child.ProcessState != nil, text}
			if err := publishJobFixtureJSON(filepath.Join(root, "abort-cleanup.json"), receipt, false); err != nil {
				t.Errorf("abort receipt: %v", err)
			}
		}
		runtime.KeepAlive(custody.process)
	}()
	metadata, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(child.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	childIdentity, identityErr := jobFixtureObjectIdentity(metadata)
	identityErr = errors.Join(identityErr, validateJobFixtureObject(metadata, childIdentity, exe, true), windows.CloseHandle(metadata))
	if identityErr != nil {
		t.Fatal(identityErr)
	}
	created, err := jobFixtureCreationTime(windows.CurrentProcess())
	if err != nil {
		t.Fatal(err)
	}
	offer := jobFixtureOffer{"job-fixture-offer-v1", c.Nonce, "breakaway-descendant", c.SourceSHA256, c.BinarySHA256, jobFixtureIdentity{uint32(os.Getpid()), created}, childIdentity, true, c.Deadline}
	if err := publishJobFixtureJSON(filepath.Join(root, "offer.json"), offer, false); err != nil {
		t.Fatal(err)
	}
	offerRaw, err := jobFixtureReadFile(filepath.Join(root, "offer.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Original Cmd/Process stays strongly retained in this finite control loop.
	for time.Now().Before(c.until()) {
		if _, err := os.Lstat(filepath.Join(root, "decision.json.committed")); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if _, err := os.Lstat(filepath.Join(root, "abort-request.json")); err == nil {
			if err := awaitJobFixtureControl(filepath.Join(root, "abort-request.json"), c.Nonce, "abort", c.until()); err != nil {
				t.Fatal(err)
			}
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
		runtime.KeepAlive(custody.process)
	}
	if _, err := os.Lstat(filepath.Join(root, "decision.json")); errors.Is(err, os.ErrNotExist) {
		if _, _, err := commitJobFixtureDecision(root, jobFixtureDecisionFor(c, offer, offerRaw, c.Creator, false)); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	decision, decisionRaw, err := readJobFixtureDecision(root, time.Now().Add(5*time.Second))
	if err != nil {
		// A reserved but uncommitted decision is uncertain. Neither participant
		// gets a default cleanup claim; retained outer containment owns failure.
		transferChosen = true
		for time.Now().Before(c.until()) {
			time.Sleep(10 * time.Millisecond)
			runtime.KeepAlive(custody.process)
		}
		t.Fatalf("uncertain decision requires original outer containment: %v", err)
	}
	if err := validateJobFixtureDecision(c, offer, offerRaw, decision); err != nil {
		transferChosen = true
		for time.Now().Before(c.until()) {
			time.Sleep(10 * time.Millisecond)
			runtime.KeepAlive(custody.process)
		}
		t.Fatal(err)
	}
	ack := jobFixtureHandoffAck{Schema: "job-fixture-ack-v1", Nonce: c.Nonce, OfferSHA256: jobFixtureDigest(offerRaw), DecisionSHA256: jobFixtureDigest(decisionRaw), Creator: offer.Creator, Child: offer.Child, Recipient: decision.Recipient, CleanupArmed: true}
	if decision.Adopt {
		transferChosen = true
		ack.CleanupOwner = "recipient"
		if err := publishJobFixtureJSON(filepath.Join(root, "ack.json"), ack, false); err != nil {
			for time.Now().Before(c.until()) {
				time.Sleep(10 * time.Millisecond)
				runtime.KeepAlive(custody.process)
			}
			t.Fatal(err)
		}
		custody.transferred = true
		if err := custody.process.Release(); err != nil {
			t.Fatalf("transferred original handle release: %v", err)
		}
		if err := publishJobFixtureJSON(filepath.Join(root, "creator-release.json"), jobFixtureControl{1, c.Nonce, "released"}, false); err != nil {
			t.Fatal(err)
		}
		// The recipient must observe survival before its adopted cleanup.
		// The direct helper stays in the inner Job until its close kills it.
		for time.Now().Before(c.until()) {
			time.Sleep(10 * time.Millisecond)
			runtime.KeepAlive(custody.process)
		}
		t.Fatal("inner Job did not close before companion deadline")
	}
	ack.CleanupOwner = "creator"
	ack.Forced = true
	cleanupErr := custody.cleanup()
	if cleanupErr != nil {
		ack.Error = cleanupErr.Error()
	}
	select {
	case <-custody.waitDone:
		ack.WaitComplete = true
	default:
	}
	ack.HandleClosed = ack.WaitComplete && child.ProcessState != nil
	if err := publishJobFixtureJSON(filepath.Join(root, "ack.json"), ack, false); err != nil {
		t.Fatal(err)
	}
	if cleanupErr != nil {
		t.Fatal(cleanupErr)
	}
	t.Fatal("breakaway creator received abort")
}

type jobFixtureNativeFrame struct {
	Schema       string             `json:"schema"`
	Nonce        string             `json:"nonce"`
	SourceSHA256 string             `json:"source_sha256"`
	BinarySHA256 string             `json:"binary_sha256"`
	Role         string             `json:"role"`
	Process      jobFixtureIdentity `json:"process"`
	Creator      jobFixtureIdentity `json:"creator"`
}

func newJobFixtureNativeFrame(c jobFixtureCase, role string, process, creator jobFixtureIdentity) jobFixtureNativeFrame {
	return jobFixtureNativeFrame{"job-fixture-native-v1", c.Nonce, c.SourceSHA256, c.BinarySHA256, role, process, creator}
}
func validateJobFixtureNativeFrame(c jobFixtureCase, f jobFixtureNativeFrame, role string, creator jobFixtureIdentity) error {
	if f.Schema != "job-fixture-native-v1" || f.Nonce != c.Nonce || f.SourceSHA256 != c.SourceSHA256 || f.BinarySHA256 != c.BinarySHA256 || f.Role != role || !f.Process.valid() || f.Creator != creator || f.Process.PID == creator.PID || !time.Now().Before(c.until()) {
		return errors.New("native companion frame binding mismatch")
	}
	return nil
}
func jobFixtureNativeStartedAt(decimal string) string {
	n, err := strconv.ParseUint(decimal, 10, 64)
	if err != nil || n == 0 {
		return ""
	}
	time100ns := windows.Filetime{LowDateTime: uint32(n), HighDateTime: uint32(n >> 32)}
	return time.Unix(0, time100ns.Nanoseconds()).UTC().Format(time.RFC3339Nano)
}
func openJobFixtureOutput(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
}
func writeJobFixtureOutput(f *os.File, text string) error {
	n, err := io.WriteString(f, text)
	if n != len(text) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	return errors.Join(err, f.Sync())
}
func verifyJobFixtureOutput(path, want string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(err, errors.New("ordinary output file missing"))
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	// A successful exclusive read proves no writer/read descriptor is still
	// holding this output object. Merely reading a Closed JSON flag cannot.
	h, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return fmt.Errorf("output is not actually closed: %w", err)
	}
	f := os.NewFile(uintptr(h), path)
	if f == nil {
		return errors.Join(errors.New("output handle wrapper missing"), windows.CloseHandle(h))
	}
	raw, readErr := io.ReadAll(f)
	err = errors.Join(readErr, f.Close())
	if err != nil {
		return err
	}
	if string(raw) != want {
		return errors.New("complete synthetic output/EOF mismatch")
	}
	return nil
}
func runJobFixtureNativeRoot(t *testing.T) {
	c, root, err := loadJobFixtureCase("native-root")
	if err != nil {
		t.Fatal(err)
	}
	self, err := jobFixtureObjectIdentity(windows.CurrentProcess())
	if err != nil {
		t.Fatal(err)
	}
	out, err := openJobFixtureOutput(filepath.Join(root, "root.out"))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			if err := out.Close(); err != nil {
				t.Errorf("root output failure close: %v", err)
			}
		}
	}()
	if err := writeJobFixtureOutput(out, "ROOT_READY\n"); err != nil {
		t.Fatal(err)
	}
	frame := newJobFixtureNativeFrame(c, "native-root", self, c.Creator)
	if err := publishJobFixtureJSON(filepath.Join(root, "root-ready.json"), frame, false); err != nil {
		t.Fatal(err)
	}
	if err := awaitJobFixtureControl(filepath.Join(root, "root-release.json"), c.Nonce, "release", c.until()); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=^TestJobBreakawayHelper$")
	child.Env = jobFixtureChildEnvironment("native-descendant", root)
	child.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	custody := retainJobFixtureCommand(child)
	defer cleanupJobFixtureCommand(t, custody)
	var childReady jobFixtureNativeFrame
	if _, err := awaitJobFixtureJSON(filepath.Join(root, "descendant-ready.json"), &childReady, c.until()); err != nil {
		t.Fatal(err)
	}
	if err := validateJobFixtureNativeFrame(c, childReady, "native-descendant", self); err != nil {
		t.Fatal(err)
	}
	metadata, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(child.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	identityErr := validateJobFixtureObject(metadata, childReady.Process, exe, true)
	identityErr = errors.Join(identityErr, windows.CloseHandle(metadata))
	if identityErr != nil {
		t.Fatal(identityErr)
	}
	if childReady.Process.PID != uint32(child.Process.Pid) {
		t.Fatal("ready child differs from original ordinary creator")
	}
	if err := writeJobFixtureOutput(out, "ROOT_CLOSED\n"); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	// Child ready is verified while its original creation handle is held.
	// Thereafter the parent still owns the at-create Job and its one Wait.
	custody.transferred = true
	if err := custody.process.Release(); err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(custody.process)
}
func runJobFixtureNativeDescendant(t *testing.T) {
	c, root, err := loadJobFixtureCase("native-root")
	if err != nil {
		t.Fatal(err)
	}
	var creator jobFixtureNativeFrame
	if _, err := awaitJobFixtureJSON(filepath.Join(root, "root-ready.json"), &creator, c.until()); err != nil {
		t.Fatal(err)
	}
	if err := validateJobFixtureNativeFrame(c, creator, "native-root", c.Creator); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, creator.Process.PID)
	if err != nil {
		t.Fatal(err)
	}
	identityErr := validateJobFixtureObject(h, creator.Process, exe, true)
	identityErr = errors.Join(identityErr, windows.CloseHandle(h))
	if identityErr != nil {
		t.Fatal(identityErr)
	}
	self, err := jobFixtureObjectIdentity(windows.CurrentProcess())
	if err != nil {
		t.Fatal(err)
	}
	out, err := openJobFixtureOutput(filepath.Join(root, "descendant.out"))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			if err := out.Close(); err != nil {
				t.Errorf("descendant output failure close: %v", err)
			}
		}
	}()
	if err := writeJobFixtureOutput(out, "DESCENDANT_READY\n"); err != nil {
		t.Fatal(err)
	}
	frame := newJobFixtureNativeFrame(c, "native-descendant", self, creator.Process)
	if err := publishJobFixtureJSON(filepath.Join(root, "descendant-ready.json"), frame, false); err != nil {
		t.Fatal(err)
	}
	if err := awaitJobFixtureControl(filepath.Join(root, "descendant-release.json"), c.Nonce, "release", c.until()); err != nil {
		t.Fatal(err)
	}
	if err := writeJobFixtureOutput(out, "DESCENDANT_CLOSED\n"); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	frame.Role = "native-descendant-closed"
	if err := publishJobFixtureJSON(filepath.Join(root, "descendant-closed.json"), frame, false); err != nil {
		t.Fatal(err)
	}
}

func checkJobFixturePeerCodec(t *testing.T) {
	t.Helper()
	// Exact late corrective freeze 8AE2FCBBC7FE06294829F422634A77E817849498D4C825FD2EAF3D5BF8422BD8.
	// Local synthetic bytes exercise the actual producer types; no file,
	// environment, publishing, process, Job or cleanup capability is used.
	const peer0 = `{"schema":"job-fixture-offer-v1","nonce":"11111111111111111111111111111111","role":"breakaway-descendant","source_sha256":"2222222222222222222222222222222222222222222222222222222222222222","binary_sha256":"3333333333333333333333333333333333333333333333333333333333333333","creator":{"pid":4242,"started_at":"133700000000000001"},"child":{"pid":4243,"started_at":"133700000000000002"},"original_reference_held":true,"deadline":"2030-01-01T00:00:30Z"}`
	const peer1 = `{"schema":"job-fixture-decision-v1","nonce":"11111111111111111111111111111111","offer_sha256":"83addd165df8b521cab2e49e173d5957c0b8616814b59d9c3a20f0bab650741e","kind":"adopt","creator":{"pid":4242,"started_at":"133700000000000001"},"child":{"pid":4243,"started_at":"133700000000000002"},"recipient":{"pid":4241,"started_at":"133700000000000000"},"adopt":true}`
	const peer2 = `{"schema":"job-fixture-ack-v1","nonce":"11111111111111111111111111111111","offer_sha256":"83addd165df8b521cab2e49e173d5957c0b8616814b59d9c3a20f0bab650741e","decision_sha256":"9b5f51bf1c7ef25357c32d54ab015c866cbe82029016ce1dc1433fb39bd1e129","creator":{"pid":4242,"started_at":"133700000000000001"},"child":{"pid":4243,"started_at":"133700000000000002"},"recipient":{"pid":4241,"started_at":"133700000000000000"},"cleanup_owner":"recipient","cleanup_armed":true,"forced":false,"wait_complete":false,"handle_closed":false,"error":""}`
	const peer3 = `{"schema":"job-fixture-decision-v1","nonce":"11111111111111111111111111111111","offer_sha256":"83addd165df8b521cab2e49e173d5957c0b8616814b59d9c3a20f0bab650741e","kind":"abort-creator","creator":{"pid":4242,"started_at":"133700000000000001"},"child":{"pid":4243,"started_at":"133700000000000002"},"recipient":{"pid":4241,"started_at":"133700000000000000"},"adopt":false}`
	const peer4 = `{"schema":"job-fixture-ack-v1","nonce":"11111111111111111111111111111111","offer_sha256":"83addd165df8b521cab2e49e173d5957c0b8616814b59d9c3a20f0bab650741e","decision_sha256":"24276932f292baba85c950ab1e83ae1e85c3408b21321a24c9683574199f2cea","creator":{"pid":4242,"started_at":"133700000000000001"},"child":{"pid":4243,"started_at":"133700000000000002"},"recipient":{"pid":4241,"started_at":"133700000000000000"},"cleanup_owner":"creator","cleanup_armed":true,"forced":true,"wait_complete":true,"handle_closed":true,"error":""}`
	const peer5 = `{"schema":"job-fixture-abort-v1","nonce":"11111111111111111111111111111111","forced":true,"wait_complete":true,"handle_closed":true,"error":""}`
	const peer6 = `{"schema":"job-fixture-abort-v1","nonce":"11111111111111111111111111111111","forced":true,"wait_complete":false,"handle_closed":false,"error":"synthetic cleanup failure"}`
	const peer7 = `{"v":1,"nonce":"11111111111111111111111111111111","action":"abort"}`
	const peer8 = `{"v":1,"nonce":"11111111111111111111111111111111","action":"released"}`
	const peer9 = `{"sha256":"9b5f51bf1c7ef25357c32d54ab015c866cbe82029016ce1dc1433fb39bd1e129"}`
	const peer10 = `{"sha256":"24276932f292baba85c950ab1e83ae1e85c3408b21321a24c9683574199f2cea"}`

	creator := jobFixtureIdentity{PID: 4242, StartedAt: "133700000000000001"}
	child := jobFixtureIdentity{PID: 4243, StartedAt: "133700000000000002"}
	recipient := jobFixtureIdentity{PID: 4241, StartedAt: "133700000000000000"}
	nonce := strings.Repeat("1", 32)
	const offerHash = "83addd165df8b521cab2e49e173d5957c0b8616814b59d9c3a20f0bab650741e"
	const adoptHash = "9b5f51bf1c7ef25357c32d54ab015c866cbe82029016ce1dc1433fb39bd1e129"
	const abortHash = "24276932f292baba85c950ab1e83ae1e85c3408b21321a24c9683574199f2cea"
	offer := jobFixtureOffer{Schema: "job-fixture-offer-v1", Nonce: nonce, Role: "breakaway-descendant", SourceSHA256: strings.Repeat("2", 64), BinarySHA256: strings.Repeat("3", 64), Creator: creator, Child: child, OriginalReferenceHeld: true, Deadline: "2030-01-01T00:00:30Z"}
	adopt := jobFixtureDecision{Schema: "job-fixture-decision-v1", Nonce: nonce, OfferSHA256: offerHash, Kind: "adopt", Creator: creator, Child: child, Recipient: recipient, Adopt: true}
	abort := jobFixtureDecision{Schema: "job-fixture-decision-v1", Nonce: nonce, OfferSHA256: offerHash, Kind: "abort-creator", Creator: creator, Child: child, Recipient: recipient, Adopt: false}
	adoptAck := jobFixtureHandoffAck{Schema: "job-fixture-ack-v1", Nonce: nonce, OfferSHA256: offerHash, DecisionSHA256: adoptHash, Creator: creator, Child: child, Recipient: recipient, CleanupOwner: "recipient", CleanupArmed: true, Forced: false, WaitComplete: false, HandleClosed: false, Error: ""}
	abortAck := jobFixtureHandoffAck{Schema: "job-fixture-ack-v1", Nonce: nonce, OfferSHA256: offerHash, DecisionSHA256: abortHash, Creator: creator, Child: child, Recipient: recipient, CleanupOwner: "creator", CleanupArmed: true, Forced: true, WaitComplete: true, HandleClosed: true, Error: ""}
	completeAbort := jobFixtureAbortReceipt{Schema: "job-fixture-abort-v1", Nonce: nonce, Forced: true, WaitComplete: true, HandleClosed: true, Error: ""}
	incompleteAbort := jobFixtureAbortReceipt{Schema: "job-fixture-abort-v1", Nonce: nonce, Forced: true, WaitComplete: false, HandleClosed: false, Error: "synthetic cleanup failure"}
	abortRequest := jobFixtureControl{V: 1, Nonce: nonce, Action: "abort"}
	released := jobFixtureControl{V: 1, Nonce: nonce, Action: "released"}
	adoptCommit := struct {
		SHA256 string `json:"sha256"`
	}{adoptHash}
	abortCommit := struct {
		SHA256 string `json:"sha256"`
	}{abortHash}
	decoded0 := new(jobFixtureOffer)
	decoded1 := new(jobFixtureDecision)
	decoded2 := new(jobFixtureHandoffAck)
	decoded3 := new(jobFixtureDecision)
	decoded4 := new(jobFixtureHandoffAck)
	decoded5 := new(jobFixtureAbortReceipt)
	decoded6 := new(jobFixtureAbortReceipt)
	decoded7 := new(jobFixtureControl)
	decoded8 := new(jobFixtureControl)
	decoded9 := new(struct {
		SHA256 string `json:"sha256"`
	})
	decoded10 := new(struct {
		SHA256 string `json:"sha256"`
	})
	for _, sample := range []struct {
		name, payload     string
		bytes             int
		digest            string
		expected, decoded any
	}{
		{"offer", peer0, 449, "83addd165df8b521cab2e49e173d5957c0b8616814b59d9c3a20f0bab650741e", offer, decoded0},
		{"decision-adopt", peer1, 360, "9b5f51bf1c7ef25357c32d54ab015c866cbe82029016ce1dc1433fb39bd1e129", adopt, decoded1},
		{"ack-adopt", peer2, 531, "037116f666a12d798287231d721be4fc62e68d3e3e17222b431bf91f1fa989e2", adoptAck, decoded2},
		{"decision-abort", peer3, 369, "24276932f292baba85c950ab1e83ae1e85c3408b21321a24c9683574199f2cea", abort, decoded3},
		{"ack-abort", peer4, 526, "dd5f4ee447a0b8fcc769babaeb56371126cb252d45bf24db3bb8c64b04d9a8f0", abortAck, decoded4},
		{"abort-receipt-complete", peer5, 143, "79d3bcd101a56f5614795268008d5cd47ac3fc418295e5e9cb5d2c4328d5f1d2", completeAbort, decoded5},
		{"abort-receipt-incomplete", peer6, 170, "40abbf48c28ff8bc4d23e9ba015fdfc8157d55a16f65697d677a18058fbd2584", incompleteAbort, decoded6},
		{"abort-request", peer7, 67, "4837d4a90e9d9aa1225046b0a77b651507b841db9c06f09ce06488b81697ee9a", abortRequest, decoded7},
		{"creator-released", peer8, 70, "d0cd3682eee86e20868bcbfb6ca7ada226c1a9f4df469b20972e485ed1512f53", released, decoded8},
		{"decision-adopt-committed", peer9, 77, "297e63646533b2b17a08d2031541c237be202d287656f0af17e058bfa4997722", adoptCommit, decoded9},
		{"decision-abort-committed", peer10, 77, "c1a0632ad6fe1e07e65125a9b0b1a64fc279cde5cf7f1d669f00d75fd58ca587", abortCommit, decoded10},
	} {
		if len(sample.payload) != sample.bytes || jobFixtureDigest([]byte(sample.payload)) != sample.digest {
			t.Fatalf("peer sample %s length/digest drift", sample.name)
		}
		encoded, err := json.Marshal(sample.expected)
		if err != nil {
			t.Fatalf("peer sample %s producer Marshal: %v", sample.name, err)
		}
		if !bytes.Equal(encoded, []byte(sample.payload)) {
			t.Fatalf("peer sample %s producer bytes/tag order changed", sample.name)
		}
		if err := decodeJobFixtureJSON([]byte(sample.payload), sample.decoded); err != nil {
			t.Fatalf("peer sample %s strict decode: %v", sample.name, err)
		}
		if !reflect.DeepEqual(reflect.ValueOf(sample.decoded).Elem().Interface(), sample.expected) {
			t.Fatalf("peer sample %s typed fields changed", sample.name)
		}
	}
	// Byte links are checked against the actual frozen payloads, not labels.
	for _, decision := range []*jobFixtureDecision{decoded1, decoded3} {
		if decision.OfferSHA256 != jobFixtureDigest([]byte(peer0)) || decision.Nonce != decoded0.Nonce || decision.Creator != decoded0.Creator || decision.Child != decoded0.Child || decision.Recipient != recipient {
			t.Fatal("peer decision Offer/identity byte link changed")
		}
	}
	for _, pair := range []struct {
		ack        *jobFixtureHandoffAck
		decision   *jobFixtureDecision
		payload    string
		commitHash string
	}{
		{decoded2, decoded1, peer1, decoded9.SHA256},
		{decoded4, decoded3, peer3, decoded10.SHA256},
	} {
		if pair.ack.OfferSHA256 != jobFixtureDigest([]byte(peer0)) || pair.ack.DecisionSHA256 != jobFixtureDigest([]byte(pair.payload)) || pair.commitHash != jobFixtureDigest([]byte(pair.payload)) || pair.ack.Nonce != pair.decision.Nonce || pair.ack.Creator != pair.decision.Creator || pair.ack.Child != pair.decision.Child || pair.ack.Recipient != pair.decision.Recipient {
			t.Fatal("peer ACK/commit decision byte link changed")
		}
	}
	for _, sampleNonce := range []string{decoded5.Nonce, decoded6.Nonce, decoded7.Nonce, decoded8.Nonce} {
		if sampleNonce != decoded0.Nonce {
			t.Fatal("peer abort/control nonce link changed")
		}
	}
}

//go:build windows && cst_vptr_probe

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/e2e/cstwatchdogwire"
	"mcp-local-hub/internal/process"
)

type f4RunConfig struct {
	Binary       string
	BinarySHA256 string
	Cell         string
	Fault        string
	RootParent   string
	ForeignPID   uint32
	SourcePath   string
	SourceSHA256 string
}

type f4RunState struct {
	config            f4RunConfig
	root              *f4OwnedRoot
	job               *process.Job
	control           *f4Control
	nonce             [16]byte
	helperCmd         *exec.Cmd
	hostCmd           *exec.Cmd
	helperOriginal    windows.Handle
	hostOriginal      windows.Handle
	custodianOriginal windows.Handle
	targetObservation windows.Handle
	foreignOriginal   windows.Handle
	foreignCreated    int64
	source            *f4SourceSnapshot
	jobCloseCount     int
}

func f4ValidateBinary(path, expectedHash string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(expectedHash) != 64 {
		return errors.New("binary path/hash must be exact and absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("fake test binary is not an ordinary file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), expectedHash) {
		return errors.New("fake test binary hash drift")
	}
	return nil
}

func f4ProcessID(handle windows.Handle) (uint32, error) {
	if handle == 0 {
		return 0, errors.New("missing original process handle")
	}
	id, err := windows.GetProcessId(handle)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("GetProcessId(original): id=%d: %w", id, err)
	}
	return id, nil
}

func f4ProcessCreated(handle windows.Handle) (int64, error) {
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return 0, err
	}
	return created.Nanoseconds(), nil
}

func f4OriginalAlive(handle windows.Handle) error {
	result, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || result != uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("original process not live: result=%d err=%v", result, err)
	}
	return nil
}

func (s *f4RunState) launchHelper() error {
	releaseChild, err := f4DuplicateInheritable(s.control.helperRelease)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(releaseChild)
	cmd := exec.Command(s.config.Binary, "-test.run=^TestCSTObserverCustodyFakeFault$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(), "MCPHUB_CST_F4_CHILD=helper", fmt.Sprintf("MCPHUB_CST_F4_HELPER_RELEASE=%d", releaseChild))
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		return errors.New("F4 helper standard files must be nil")
	}
	s.helperCmd = cmd
	_, err = process.StartCSTObserverAtCreateForTest(s.job, cmd, process.CSTObserverAtCreateHooks{
		ChildHandles: []windows.Handle{releaseChild},
		RetainOriginal: func(handle windows.Handle, _ uint32) error {
			s.helperOriginal = handle
			return nil
		},
	})
	if err != nil {
		return err
	}
	if s.helperOriginal == 0 || cmd.Process == nil {
		return errors.New("helper original or command process missing")
	}
	id, err := f4ProcessID(s.helperOriginal)
	if err != nil || int(id) != cmd.Process.Pid {
		return fmt.Errorf("helper original identity mismatch: id=%d err=%v", id, err)
	}
	member, active, pids, err := process.CSTObserverOriginalMemberForTest(s.job, s.helperOriginal)
	if err != nil || !member || active == 0 || active != uint32(len(pids)) || !f4ContainsPID(pids, id) {
		return fmt.Errorf("helper outer-Job membership uncertain: member=%v active=%d listed=%d err=%v", member, active, len(pids), err)
	}
	return f4OriginalAlive(s.helperOriginal)
}

func (s *f4RunState) launchHost() error {
	allowlist, err := s.control.childHandles()
	if err != nil {
		return err
	}
	defer f4CloseHandles(allowlist)
	cell := s.config.Cell
	cmd := exec.Command(s.config.Binary, "-test.run=^TestCSTObserverCustodyFakeFault$/^"+cell+"$", "-test.timeout=30s")
	cmd.Env = append(os.Environ(),
		"MCPHUB_CST_VPTR_FAKE_RUN=1", "MCPHUB_CST_F4_CELL="+cell,
		"MCPHUB_CST_F4_FAULT="+s.config.Fault,
		"MCPHUB_CST_F4_ROOT="+s.root.Path,
		"MCPHUB_CST_F4_ROOT_TOKEN="+hex.EncodeToString(s.root.Token[:]),
		"MCPHUB_CST_F4_NONCE="+hex.EncodeToString(s.nonce[:]),
		fmt.Sprintf("MCPHUB_CST_F4_HANDLES=%d,%d,%d", allowlist[0], allowlist[1], allowlist[2]),
		fmt.Sprintf("MCPHUB_CST_F4_D_MS=%d", f4D.Milliseconds()),
		fmt.Sprintf("MCPHUB_CST_F4_A_MS=%d", f4A.Milliseconds()))
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		return errors.New("F4 test-host standard files must be nil")
	}
	s.hostCmd = cmd
	hooks := process.CSTObserverAtCreateHooks{
		ChildHandles: allowlist,
		RetainOriginal: func(handle windows.Handle, _ uint32) error {
			s.hostOriginal = handle
			return nil
		},
	}
	if cell == "watchdog_postcreate_findprocess" {
		hooks.FindProcess = func(int) (*os.Process, error) { return nil, errors.New("injected second-launch FindProcess failure") }
	}
	_, err = process.StartCSTObserverAtCreateForTest(s.job, cmd, hooks)
	if err != nil {
		return err
	}
	if s.hostOriginal == 0 || cmd.Process == nil {
		return errors.New("host original or command process missing")
	}
	id, err := f4ProcessID(s.hostOriginal)
	if err != nil || int(id) != cmd.Process.Pid {
		return fmt.Errorf("host original identity mismatch: id=%d err=%v", id, err)
	}
	member, active, pids, err := process.CSTObserverOriginalMemberForTest(s.job, s.hostOriginal)
	if err != nil || !member || active == 0 || active != uint32(len(pids)) || !f4ContainsPID(pids, id) {
		return fmt.Errorf("host outer-Job membership uncertain: member=%v active=%d listed=%d err=%v", member, active, len(pids), err)
	}
	return f4OriginalAlive(s.hostOriginal)
}

func f4ContainsPID(pids []uint32, wanted uint32) bool {
	for _, pid := range pids {
		if pid == wanted {
			return true
		}
	}
	return false
}

// f4ClassifyMembers uses only a kernel Job list and a process snapshot; it
// never reopens a PID to recover custody. Every extra live member must trace
// to a retained, unsignaled launch original.
func (s *f4RunState) classifyMembers() error {
	base := s.hostOriginal
	if base == 0 {
		base = s.helperOriginal
	}
	_, active, pids, err := process.CSTObserverOriginalMemberForTest(s.job, base)
	if err != nil || active == 0 || active != uint32(len(pids)) {
		return fmt.Errorf("outer-Job accounting/list uncertain: active=%d listed=%d err=%v", active, len(pids), err)
	}
	known := map[uint32]bool{}
	for _, handle := range []windows.Handle{s.helperOriginal, s.hostOriginal, s.custodianOriginal} {
		if handle == 0 {
			continue
		}
		if err := f4OriginalAlive(handle); err != nil {
			return err
		}
		pid, err := f4ProcessID(handle)
		if err != nil || !f4ContainsPID(pids, pid) {
			return fmt.Errorf("original not on live Job list: pid=%d err=%v", pid, err)
		}
		known[pid] = true
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entries := map[uint32]windows.ProcessEntry32{}
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return err
	}
	for {
		entries[entry.ProcessID] = entry
		entry.Size = uint32(unsafe.Sizeof(entry))
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return err
		}
	}
	for _, pid := range pids {
		if known[pid] {
			continue
		}
		seen := map[uint32]bool{}
		current := pid
		for !known[current] {
			if seen[current] || len(seen) > len(pids) {
				return fmt.Errorf("unclassified Job member pid=%d", pid)
			}
			seen[current] = true
			parent, exists := entries[current]
			if !exists || parent.ParentProcessID == 0 {
				return fmt.Errorf("Job member parent identity unavailable pid=%d", pid)
			}
			current = parent.ParentProcessID
		}
	}
	return nil
}

func (s *f4RunState) captureCustodian(ephemeral uint64) error {
	if s.hostOriginal == 0 || s.custodianOriginal != 0 || ephemeral == 0 {
		return errors.New("F4 custodian transfer precondition")
	}
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(s.hostOriginal, windows.Handle(ephemeral), windows.CurrentProcess(), &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = windows.CloseHandle(duplicate)
		}
	}()
	custPID, err := f4ProcessID(duplicate)
	if err != nil {
		return err
	}
	hostPID, err := f4ProcessID(s.hostOriginal)
	if err != nil || custPID == hostPID || custPID == 0 {
		return errors.New("custodian handle duplicates host or has no PID")
	}
	hostCreated, err := f4ProcessCreated(s.hostOriginal)
	if err != nil {
		return err
	}
	custCreated, err := f4ProcessCreated(duplicate)
	if err != nil || custCreated < hostCreated {
		return fmt.Errorf("custodian creation identity drift: host=%d custodian=%d err=%v", hostCreated, custCreated, err)
	}
	member, _, pids, err := process.CSTObserverOriginalMemberForTest(s.job, duplicate)
	if err != nil || !member || !f4ContainsPID(pids, custPID) {
		return fmt.Errorf("custodian outer-Job membership uncertain: member=%v listed=%v err=%v", member, f4ContainsPID(pids, custPID), err)
	}
	// A snapshot parent relation is separate evidence from the duplicated
	// process handle and the Job membership query.
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snapshot, &entry); err != nil {
		return err
	}
	parentFound := false
	for {
		if entry.ProcessID == custPID {
			parentFound = entry.ParentProcessID == hostPID
			break
		}
		entry.Size = uint32(unsafe.Sizeof(entry))
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return err
		}
	}
	if !parentFound || f4OriginalAlive(duplicate) != nil {
		return errors.New("custodian is not a live direct host child")
	}
	s.custodianOriginal = duplicate
	keep = true
	return s.classifyMembers()
}

// captureTargetObservation binds the child-attested tuple to one limited-rights
// process handle. This is parent observation, never target custody or a PID
// recovery path; the custodian alone retains the original target handle.
func (s *f4RunState) captureTargetObservation(target *cstwatchdogwire.Task2Target) error {
	if target == nil || target.PID == 0 || s.targetObservation != 0 || s.job == nil || s.job.Handle() == 0 {
		return errors.New("Task 2 target observation precondition")
	}
	if !strings.EqualFold(target.ImageSHA256, s.config.BinarySHA256) {
		return errors.New("target image digest differs from admitted binary")
	}
	observed, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, target.PID)
	if err != nil {
		return fmt.Errorf("open observation-only target: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = windows.CloseHandle(observed)
		}
	}()
	pid, err := f4ProcessID(observed)
	if err != nil || pid != target.PID {
		return fmt.Errorf("target PID identity mismatch: observed=%d err=%v", pid, err)
	}
	created, err := f4ProcessCreated(observed)
	if err != nil || created <= 0 || uint64(created) != target.CreationTicks {
		return fmt.Errorf("target creation identity mismatch: observed=%d err=%v", created, err)
	}
	if err := f4OriginalAlive(observed); err != nil {
		return fmt.Errorf("target not live at barrier: %w", err)
	}
	image := make([]uint16, windows.MAX_PATH*2)
	imageLength := uint32(len(image))
	if err := windows.QueryFullProcessImageName(observed, 0, &image[0], &imageLength); err != nil {
		return fmt.Errorf("query target image: %w", err)
	}
	if !strings.EqualFold(filepath.Clean(windows.UTF16ToString(image[:imageLength])), filepath.Clean(s.config.Binary)) {
		return errors.New("target process image path differs from admitted binary")
	}
	if err := f4ValidateBinary(s.config.Binary, s.config.BinarySHA256); err != nil {
		return fmt.Errorf("admitted target binary drift: %w", err)
	}
	member, active, pids, err := process.CSTObserverOriginalMemberForTest(s.job, observed)
	if err != nil || !member || active == 0 || !f4ContainsPID(pids, target.PID) {
		return fmt.Errorf("target outer-Job membership uncertain: member=%v active=%d listed=%d err=%v", member, active, len(pids), err)
	}
	s.targetObservation = observed
	keep = true
	return nil
}

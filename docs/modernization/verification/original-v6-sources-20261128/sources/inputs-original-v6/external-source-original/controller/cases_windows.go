//go:build windows && amd64

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	owner "mcp-local-hub/.scratch/__CANONICAL_EXTERNAL_STAGE__/canonicalcaseowner"
	fixture "mcp-local-hub/internal/canonicalnativefixture"
)

type terminal struct {
	Test    string `json:"test"`
	Outcome string `json:"outcome"`
}
type caseLedger struct {
	RootRuns   int        `json:"root_runs"`
	RootPasses int        `json:"root_passes"`
	RootFails  int        `json:"root_fails"`
	RootSkips  int        `json:"root_skips"`
	Terminals  []terminal `json:"terminals"`
	Complete   bool       `json:"complete"`
	Passed     bool       `json:"passed"`
}
type caseRecord struct {
	Test                      string                      `json:"test"`
	Slot                      string                      `json:"slot"`
	Attempted                 bool                        `json:"attempted"`
	Request                   *fixture.Request            `json:"request"`
	Config                    fixture.Config              `json:"config"`
	InitialRoot               *owner.InitialRootAdmission `json:"initial_root"`
	InitialOwnerMatched       bool                        `json:"initial_owner_matched"`
	Ready                     *fixture.Frame              `json:"ready"`
	Grant                     *fixture.Frame              `json:"grant"`
	ActorFinal                *fixture.Frame              `json:"actor_final"`
	Native                    *owner.Result               `json:"native"`
	NativeOwnerMatched        bool                        `json:"native_owner_matched"`
	PhysicalComplete          bool                        `json:"physical_complete"`
	CustodyRetained           bool                        `json:"custody_retained"`
	ReadyReceived             bool                        `json:"ready_received"`
	GrantPublished            bool                        `json:"grant_published"`
	FinalReceived             bool                        `json:"final_received"`
	RawStdout                 fixture.Artifact            `json:"raw_stdout"`
	RawStderr                 fixture.Artifact            `json:"raw_stderr"`
	Ledger                    caseLedger                  `json:"ledger"`
	RequestParentMatchesOuter bool                        `json:"request_parent_matches_outer"`
	Failure                   string                      `json:"failure"`
}

type nativeReturn struct {
	Value    owner.Result
	Cause    error
	Returned bool
}
type caseCustody struct {
	Record        caseRecord
	Original      atomic.Pointer[owner.Owner]
	Command       *exec.Cmd
	Context       context.Context
	Cancel        context.CancelFunc
	Done          chan nativeReturn
	Cause         error
	Env           []string
	Root, Temp    string
	Work, Cleanup time.Time
}

func caseKind(test string) (owner.CaseKind, error) {
	switch test {
	case fixture.TestGUIReplay:
		return owner.CaseGUIReplay, nil
	case fixture.TestSuccessor:
		return owner.CaseSuccessor, nil
	default:
		return "", errors.New("unsupported fixed canonical native case")
	}
}

func exactTime(text string) (time.Time, error) {
	value, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || value.UTC().Format(time.RFC3339Nano) != text {
		return time.Time{}, errors.Join(err, errors.New("canonical original UTC endpoint required"))
	}
	return value, nil
}

func positiveBirth(text string) bool {
	value, err := strconv.ParseUint(text, 10, 64)
	return err == nil && value != 0 && strconv.FormatUint(value, 10) == text
}

func (s *session) acceptRequest(c *caseCustody, request fixture.Request, binding fixture.SlotBinding) error {
	if request.Version != fixture.Version || request.RunNonce != s.channel.RunNonce ||
		request.Slot != binding.Slot || request.ParentPID == 0 || !positiveBirth(request.ParentBirth) {
		return errors.New("canonical request run/slot/original-parent data differs")
	}
	config := request.Bind
	if config.Version != fixture.Version || config.Test != binding.Test || config.Nonce != binding.Nonce ||
		config.Image != s.plan.Binary.Path || config.ImageSHA256 != s.plan.Binary.SHA256 ||
		config.Fixture != s.plan.Fixture.Path || config.FixtureSHA256 != s.plan.Fixture.SHA256 {
		return errors.New("canonical request differs from original controller inputs")
	}
	work, err := exactTime(config.WorkDeadline)
	if err != nil {
		return err
	}
	cleanup, err := exactTime(config.CleanupDeadline)
	if err != nil {
		return err
	}
	parent, err := exactTime(request.ParentDeadline)
	if err != nil {
		return err
	}
	timeout, err := time.ParseDuration(config.Timeout)
	now := time.Now()
	if err != nil || timeout <= 0 || timeout > 4*time.Minute ||
		!now.Before(work) || cleanup.Sub(work) != 10*time.Second ||
		cleanup.After(now.Add(4*time.Minute)) || cleanup.After(parent.Add(-time.Second)) ||
		cleanup.After(s.workDeadline) {
		return errors.Join(err, errors.New("canonical request changes or exceeds its original finite endpoints"))
	}
	c.Record.Request = &request
	c.Record.Config = config
	c.Work, c.Cleanup = work, cleanup
	return nil
}

func (s *session) service() {
	defer func() { s.record.Channel.Joined = true; s.record.Channel.Stopped = true }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s.collectOuter() || s.ctx.Err() != nil || s.unknown {
			s.cancel()
			return
		}
		for _, binding := range s.channel.Slots {
			c := s.caseByTest(binding.Test)
			if c.Record.Request != nil {
				continue
			}
			var request fixture.Request
			err := fixture.Read(filepath.Join(c.Root, "request.json"), &request)
			if errors.Is(err, os.ErrNotExist) && !fixture.HasRetainedFile(err) {
				continue
			}
			if err != nil {
				s.failControl(err)
				return
			}
			s.record.Channel.RequestCount++
			if err := s.acceptRequest(c, request, binding); err != nil {
				c.Record.Request = &request
				c.Cause = errors.Join(c.Cause, err)
				c.Record.Failure = errorDescription(c.Cause)
				s.failControl(err)
				return
			}
			if s.collectOuter() || s.ctx.Err() != nil {
				s.failControl(errors.New("original whole-suite owning call ended before canonical creation"))
				return
			}
			s.runCase(c)
			if s.unknown {
				return
			}
		}
		select {
		case result := <-s.outerDone:
			s.outer = &result
			s.cancel()
			return
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *session) runCase(c *caseCustody) {
	kind, err := caseKind(c.Record.Test)
	if err != nil {
		s.failControl(err)
		return
	}
	if err := fixture.Publish(filepath.Join(c.Root, "config.json"), c.Record.Config); err != nil {
		s.failControl(err)
		return
	}
	c.Env, err = actorEnvironment(s.plan, s.root, c.Root, c.Temp)
	if err != nil {
		s.failControl(err)
		return
	}
	c.Command = exec.Command(s.plan.Binary.Path, "-test.paniconexit0", "-test.v", "-test.count=1",
		"-test.timeout="+c.Record.Config.Timeout, "-test.run=^"+c.Record.Test+"$")
	c.Command.Env, c.Command.Dir = c.Env, s.plan.SourceDir
	c.Context, c.Cancel = context.WithDeadline(context.Background(), c.Cleanup)
	defer c.Cancel()
	c.Done = make(chan nativeReturn, 1)
	images := owner.CanonicalImages{
		Actor:      owner.ImageInput{Path: s.plan.Binary.Path, SHA256: s.plan.Binary.SHA256},
		Product:    owner.ImageInput{Path: s.plan.Product.Path, SHA256: s.plan.Product.SHA256},
		PowerShell: owner.ImageInput{Path: s.plan.Tools.PowerShell.Path, SHA256: s.plan.Tools.PowerShell.SHA256},
		Netstat:    owner.ImageInput{Path: s.plan.Tools.Netstat.Path, SHA256: s.plan.Tools.Netstat.SHA256},
		Netsh:      owner.ImageInput{Path: s.plan.Tools.Netsh.Path, SHA256: s.plan.Tools.Netsh.SHA256},
		Conhost:    owner.ImageInput{Path: s.plan.Tools.Conhost.Path, SHA256: s.plan.Tools.Conhost.SHA256},
	}
	if s.plan.Tools.WMIC != nil {
		images.WMIC = &owner.ImageInput{Path: s.plan.Tools.WMIC.Path, SHA256: s.plan.Tools.WMIC.SHA256}
	}
	if s.collectOuter() || s.ctx.Err() != nil {
		s.failControl(errors.New("original whole-suite owning call ended before canonical creation"))
		return
	}
	c.Record.Attempted = true
	go func() {
		normal := false
		defer func() {
			if !normal {
				recovered := recover()
				cause := fmt.Errorf("original canonical owner did not return: %v", recovered)
				if originalError, ok := recovered.(error); ok {
					cause = fmt.Errorf("original canonical owner did not return: %w", originalError)
				}
				c.Done <- nativeReturn{Cause: cause}
			}
		}()
		value, cause := owner.Run(c.Context, owner.Spec{
			Case: kind, Command: c.Command, Images: images, CaseTempRoot: c.Temp,
			ActorTimeout: c.Record.Config.Timeout, WorkDeadline: c.Work, CleanupDeadline: c.Cleanup,
			StdoutLimit: 4 << 20, StderrLimit: 4 << 20,
		}, func(original *owner.Owner) error {
			if original == nil || !c.Original.CompareAndSwap(nil, original) {
				return errors.New("original canonical owner retention is not one-shot")
			}
			return nil
		})
		normal = true
		c.Done <- nativeReturn{Value: value, Cause: cause, Returned: true}
	}()
	s.awaitCase(c)
}

func (s *session) initial(c *caseCustody, value owner.InitialRootAdmission) error {
	original := c.Original.Load()
	if original == nil || value.Original != original || value.PID == 0 || !positiveBirth(value.NativeBirthFILETIME) ||
		value.InAnyJob || !value.CreateContinued || value.Image.SHA256 != s.plan.Binary.SHA256 ||
		!strings.EqualFold(value.Image.Path, s.plan.Binary.Path) || value.Image.FileID == "" ||
		value.Image.Bytes <= 0 || value.Image.Machine != 0x8664 || value.Image.Subsystem != 2 ||
		value.CreateReceivedAt.After(value.PublishedAt) || !value.PublishedAt.Before(c.Cleanup) {
		return errors.New("original initial CREATE admission does not bind this canonical actor")
	}
	c.Record.InitialRoot = &value
	c.Record.InitialOwnerMatched = true
	return nil
}

func (s *session) awaitCase(c *caseCustody) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(time.Until(c.Cleanup))
	defer timer.Stop()
	outerCanceled := false
	for {
		if !outerCanceled && (s.collectOuter() || s.ctx.Err() != nil) {
			outerCanceled = true
			c.Cause = errors.Join(c.Cause, errors.New("original whole-suite owning call ended during canonical case"))
			c.Cancel()
		}
		if c.Record.InitialRoot == nil {
			if original := c.Original.Load(); original != nil {
				select {
				case value := <-original.InitialRoot():
					if err := s.initial(c, value); err != nil {
						c.Cause = errors.Join(c.Cause, err)
						c.Record.CustodyRetained = true
						s.unknown = true
						s.cancel()
						c.Cancel()
					}
				default:
				}
			}
		}
		if c.Record.InitialRoot != nil && !c.Record.ReadyReceived && c.Cause == nil {
			var ready fixture.Frame
			err := fixture.Read(filepath.Join(c.Root, "ready.json"), &ready)
			if err == nil {
				c.Record.Ready = &ready
				initial := c.Record.InitialRoot
				if ready.Bind != c.Record.Config || ready.Kind != "ready" || ready.PID != initial.PID ||
					ready.Birth != initial.NativeBirthFILETIME || !ready.NoJobChecked || !ready.InitialNoJob ||
					ready.BeforeFlags != 0 || ready.InnerFlags != 0x800 || !ready.Assigned || !ready.InnerMember ||
					ready.StrictMember || ready.InnerHandleClosed || ready.Failed || ready.Error != "" {
					err = errors.New("canonical READY differs from original initial admission and inner Job facts")
				} else {
					c.Record.ReadyReceived = true
					grant := ready
					grant.Kind = "grant"
					c.Record.Grant = &grant
					err = fixture.Publish(filepath.Join(c.Root, "grant.json"), grant)
					c.Record.GrantPublished = err == nil
				}
			}
			if err != nil && (!errors.Is(err, os.ErrNotExist) || fixture.HasRetainedFile(err)) {
				c.Cause = errors.Join(c.Cause, err)
				c.Cancel()
				if fixture.HasRetainedFile(err) {
					c.Record.CustodyRetained = true
					s.unknown = true
					s.cancel()
				}
			}
		}
		select {
		case result := <-c.Done:
			if c.Record.InitialRoot == nil {
				if original := c.Original.Load(); original != nil {
					select {
					case value := <-original.InitialRoot():
						if err := s.initial(c, value); err != nil {
							c.Cause = errors.Join(c.Cause, err)
							c.Record.CustodyRetained = true
							s.unknown = true
							s.cancel()
						}
					default:
					}
				}
			}
			s.receiveCase(c, result)
			return
		case <-timer.C:
			c.Cancel()
			c.Record.CustodyRetained = true
			c.Cause = errors.Join(c.Cause, context.DeadlineExceeded, errors.New("original canonical return was not received by cleanup"))
			c.Record.Failure = errorDescription(c.Cause)
			s.unknown = true
			s.cancel()
			return
		case <-ticker.C:
		}
	}
}

func (s *session) receiveCase(c *caseCustody, returned nativeReturn) {
	if returned.Returned {
		c.Record.Native = &returned.Value
	}
	c.Cause = errors.Join(c.Cause, returned.Cause)
	native := returned.Value
	c.Record.NativeOwnerMatched = c.Original.Load() != nil && native.Original == c.Original.Load()
	kind, _ := caseKind(c.Record.Test)
	physical := returned.Returned && c.Record.NativeOwnerMatched && native.Complete && native.NativeComplete &&
		native.IOComplete && !native.Unknown && !native.NoCreateAttempted && native.HostNoJobChecked &&
		native.KillOnExitArmed && native.CreatingThreadID != 0 && native.CreatingThreadID != startupThreadID &&
		native.CreatingThreadSignaled && native.CreatingThreadCloseAttempted && native.CreatingThreadClosed &&
		native.ImagePinClosed && native.ImagePinsClosed && native.RootPID != 0 && native.RootWait.Returned &&
		native.RootWait.Known && native.Case == kind && native.CaseTempRoot == c.Temp &&
		native.CommandPath == c.Command.Path && reflect.DeepEqual(native.CommandArgs, c.Command.Args) &&
		native.WorkDeadline.Equal(c.Work) && native.CleanupDeadline.Equal(c.Cleanup) &&
		!native.CompleteReceivedAt.IsZero() && native.CompleteReceivedAt.Before(c.Cleanup)
	roots := 0
	birth := ""
	for _, process := range native.Processes {
		if !process.InitialRoot {
			continue
		}
		roots++
		birth = process.NativeBirthFILETIME
		physical = physical && process.PID == native.RootPID && positiveBirth(birth) &&
			process.ExitCode == uint32(native.RootWait.ExitCode)
		if c.Record.InitialRoot != nil {
			physical = physical && c.Record.InitialRoot.PID == process.PID &&
				c.Record.InitialRoot.NativeBirthFILETIME == birth
		}
	}
	physical = physical && roots == 1 && native.RootWait.ExitCode >= 0
	if !physical || c.Record.CustodyRetained || fixture.HasRetainedFile(c.Cause) {
		c.Record.CustodyRetained = true
		c.Record.Failure = errorDescription(errors.Join(c.Cause, errors.New("original canonical physical receiving incomplete")))
		s.unknown = true
		s.cancel()
		return
	}
	// A: the original debug owner and its owned I/O are terminal. Only this
	// case's regular output/control files may now be received.
	out, outErr := writeRaw(filepath.Join(c.Root, "stdout.raw"), native.StdoutBytes, 4<<20)
	if outErr != nil {
		s.caseIOFailure(c, outErr)
		return
	}
	c.Record.RawStdout = out
	errOut, errErr := writeRaw(filepath.Join(c.Root, "stderr.txt"), native.StderrBytes, 4<<20)
	if errErr != nil {
		s.caseIOFailure(c, errErr)
		return
	}
	c.Record.RawStderr = errOut
	var final fixture.Frame
	finalErr := fixture.Read(filepath.Join(c.Root, "actor-final.json"), &final)
	if finalErr == nil {
		c.Record.ActorFinal = &final
		c.Record.FinalReceived = true
	} else if !errors.Is(finalErr, os.ErrNotExist) || fixture.HasRetainedFile(finalErr) {
		if fixture.HasRetainedFile(finalErr) {
			s.caseIOFailure(c, finalErr)
			return
		}
		c.Cause = errors.Join(c.Cause, finalErr)
	}
	c.Record.Ledger = receiveLedger(c.Record.Test, native.StdoutBytes)
	finalOK := c.Record.ReadyReceived && c.Record.GrantPublished && c.Record.FinalReceived
	if finalOK {
		want := *c.Record.Ready
		want.Kind, want.InnerHandleClosed = "final", true
		finalOK = final == want
	}
	c.Record.PhysicalComplete = true
	admitted, products := true, 0
	for _, process := range native.Processes {
		admitted = admitted && process.ImageDisposition == owner.ImageAdmitted
		if process.InitialRoot {
			admitted = admitted && process.Role == owner.ActorImage
		}
		if process.Role == owner.ProductImage {
			products++
		}
	}
	expectedProducts := 2
	if kind == owner.CaseSuccessor {
		expectedProducts = 3
	}
	captureComplete := !native.Stdout.Overflow && !native.Stderr.Overflow &&
		native.Stdout.Bytes == uint64(len(native.StdoutBytes)) && native.Stderr.Bytes == uint64(len(native.StderrBytes))
	passed := c.Cause == nil && c.Record.InitialOwnerMatched && native.RootWait.ExitCode == 0 &&
		admitted && products == expectedProducts && captureComplete && c.Record.Ledger.Passed && finalOK
	if !passed {
		c.Cause = errors.Join(c.Cause, errors.New("original canonical actor/control/ledger did not pass"))
	}
	c.Record.Failure = errorDescription(c.Cause)
	reply := fixture.Reply{Version: fixture.Version, RunNonce: s.channel.RunNonce, Slot: c.Record.Slot,
		Bind: c.Record.Config, Kind: "settled", RootPID: native.RootPID, RootBirth: birth,
		RootExitCode: native.RootWait.ExitCode, PhysicalComplete: true, Passed: passed,
		Ready: c.Record.Ready, Final: c.Record.ActorFinal, Stdout: out, Stderr: errOut,
		Error: errorDescription(c.Cause)}
	if err := fixture.Publish(filepath.Join(c.Root, "reply.json"), reply); err != nil {
		s.caseIOFailure(c, err)
	}
}

func (s *session) caseIOFailure(c *caseCustody, cause error) {
	c.Cause = errors.Join(c.Cause, cause)
	c.Record.PhysicalComplete = false
	c.Record.CustodyRetained = true
	c.Record.Failure = errorDescription(c.Cause)
	s.unknown = true
	s.cancel()
}

func receiveLedger(test string, body []byte) caseLedger {
	names, _ := fixture.TerminalNames(test)
	wanted := make(map[string]bool, len(names))
	runs, ends := map[string]int{}, map[string]int{}
	for _, name := range names {
		wanted[name] = true
	}
	result := caseLedger{}
	valid := true
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "=== RUN   ") {
			name := strings.TrimSpace(strings.TrimPrefix(line, "=== RUN   "))
			if !wanted[name] {
				valid = false
			}
			runs[name]++
			continue
		}
		for _, outcome := range []string{"PASS", "FAIL", "SKIP"} {
			prefix := "--- " + outcome + ": "
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			fields := strings.Fields(strings.TrimPrefix(line, prefix))
			if len(fields) == 0 {
				valid = false
				break
			}
			name := fields[0]
			if !wanted[name] {
				valid = false
			}
			ends[name]++
			result.Terminals = append(result.Terminals, terminal{Test: name, Outcome: strings.ToLower(outcome)})
			if name == test {
				switch outcome {
				case "PASS":
					result.RootPasses++
				case "FAIL":
					result.RootFails++
				case "SKIP":
					result.RootSkips++
				}
			}
			break
		}
	}
	result.RootRuns = runs[test]
	result.Complete = valid && len(result.Terminals) == len(names)
	for _, name := range names {
		result.Complete = result.Complete && runs[name] == 1 && ends[name] == 1
	}
	result.Passed = result.Complete
	for _, item := range result.Terminals {
		result.Passed = result.Passed && item.Outcome == "pass"
	}
	return result
}

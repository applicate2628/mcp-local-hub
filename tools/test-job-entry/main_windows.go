//go:build windows

// test-job-entry supplies the native creator contract required by the two
// process Job fixtures. It is a test controller, not a production launcher.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/process"
)

const (
	entryPrefix    = "WJOB-ENTRY-V1 "
	entryConfigEnv = "MCPHUB_JOB_FIXTURE_ENTRY_CONFIG"
	frameLimit     = 8192
	streamLimit    = 2 << 20
	selectedTests  = "^Test(Job_BreakawayEnabledDescendantEscapesClose|JobInformationTransportPreparationBoundary)$"
)

var requiredPasses = []string{
	"TestJob_BreakawayEnabledDescendantEscapesClose",
	"TestJobInformationTransportPreparationBoundary",
	"TestJobInformationTransportPreparationBoundary/real-native-typed-gc-and-creator",
}

type entryBind struct {
	Run              string `json:"run"`
	Nonce            string `json:"nonce"`
	OwnerSource      string `json:"ownerSource"`
	ControllerSource string `json:"controllerSource"`
	Packet           string `json:"packet"`
	Binary           string `json:"binary"`
}
type entryConfig struct {
	Bind          entryBind         `json:"bind"`
	ExpectedFlags uint32            `json:"expectedFlags"`
	EntryBytes    uint32            `json:"entryBytes"`
	SourceDir     string            `json:"sourceDir"`
	Companions    map[string]string `json:"companions"`
}
type entryNative struct {
	Class    uint32 `json:"class"`
	Bytes    uint32 `json:"bytes"`
	Returned uint32 `json:"returned"`
	Flags    uint32 `json:"flags"`
	Member   bool   `json:"member"`
	Alive    bool   `json:"alive"`
}
type entryReady struct {
	V            uint32    `json:"v"`
	Kind         string    `json:"kind"`
	Bind         entryBind `json:"bind"`
	Challenge    string    `json:"challenge"`
	PID          uint32    `json:"pid"`
	Created100ns string    `json:"created100ns"`
}
type entryGrant struct {
	V               uint32      `json:"v"`
	Kind            string      `json:"kind"`
	Bind            entryBind   `json:"bind"`
	Challenge       string      `json:"challenge"`
	PID             uint32      `json:"pid"`
	Created100ns    string      `json:"created100ns"`
	OwnerGeneration string      `json:"ownerGeneration"`
	Entry           entryNative `json:"entry"`
	Confirm         bool        `json:"confirm"`
}
type entryAck struct {
	V               uint32 `json:"v"`
	Kind            string `json:"kind"`
	Nonce           string `json:"nonce"`
	Challenge       string `json:"challenge"`
	OwnerGeneration string `json:"ownerGeneration"`
	GrantSHA256     string `json:"grantSha256"`
	Confirm         bool   `json:"confirm"`
}

type options struct {
	binary, sourceDir, runRoot string
	timeout                    time.Duration
}
type runPacket struct {
	Schema                 string            `json:"schema"`
	Run                    string            `json:"run"`
	Nonce                  string            `json:"nonce"`
	Binary                 string            `json:"binary"`
	BinarySHA256           string            `json:"binarySha256"`
	ControllerBinarySHA256 string            `json:"controllerBinarySha256"`
	OwnerSourceSHA256      string            `json:"ownerSourceSha256"`
	ControllerSourceSHA256 string            `json:"controllerSourceSha256"`
	SourceDir              string            `json:"sourceDir"`
	Companions             map[string]string `json:"companions"`
	Args                   []string          `json:"args"`
}
type runReceipt struct {
	Schema             string                           `json:"schema"`
	Started            string                           `json:"started"`
	Finished           string                           `json:"finished"`
	Bind               entryBind                        `json:"bind"`
	Ready              *entryReady                      `json:"ready"`
	Grant              *entryGrant                      `json:"grant"`
	Ack                *entryAck                        `json:"ack"`
	Observation        *grantObservation                `json:"observation"`
	OriginalWaitKnown  bool                             `json:"originalWaitKnown"`
	OriginalExitCode   int                              `json:"originalExitCode"`
	NativeSettlement   *process.ContainedNativeSnapshot `json:"nativeSettlement"`
	SettlementComplete bool                             `json:"settlementComplete"`
	StdoutEOF          bool                             `json:"stdoutEOF"`
	StderrEOF          bool                             `json:"stderrEOF"`
	Passed             []string                         `json:"passed"`
	Success            bool                             `json:"success"`
	Error              string                           `json:"error"`
}

func main() {
	var opt options
	flag.StringVar(&opt.binary, "test-binary", "", "absolute process.test.exe built from this checkout with -H windowsgui and without trimpath")
	flag.StringVar(&opt.sourceDir, "source-dir", "", "absolute internal/process directory used to build both executables")
	flag.StringVar(&opt.runRoot, "run-root", "", "new absolute output directory under an existing private parent")
	flag.DurationVar(&opt.timeout, "timeout", 90*time.Second, "test work deadline (1s to 5m); cleanup has at most 10s more")
	flag.Parse()
	if flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "unexpected positional arguments")
		os.Exit(2)
	}
	if err := execute(opt); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("qualified Job fixtures passed with original owner settlement")
}

func execute(opt options) (retErr error) {
	for name, path := range map[string]string{"test-binary": opt.binary, "source-dir": opt.sourceDir, "run-root": opt.runRoot} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("%s must be an absolute clean path", name)
		}
	}
	if opt.timeout < time.Second || opt.timeout > 5*time.Minute {
		return errors.New("timeout must be between 1s and 5m")
	}
	_, compiledSource, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(compiledSource) {
		return errors.New("controller requires absolute compiled source paths; build without trimpath")
	}
	controllerDir := filepath.Dir(compiledSource)
	compiledProcessDir := filepath.Join(controllerDir, "..", "..", "internal", "process")
	if !strings.EqualFold(filepath.Clean(compiledProcessDir), opt.sourceDir) {
		return errors.New("source-dir differs from the controller's actual compiled checkout")
	}
	if err := os.Mkdir(opt.runRoot, 0o700); err != nil {
		return fmt.Errorf("create fresh run-root (existing directories are refused): %w", err)
	}
	root, err := os.OpenRoot(opt.runRoot)
	if err != nil {
		return err
	}
	receipt := runReceipt{Schema: "job-fixture-controller-result-v1", Started: time.Now().UTC().Format(time.RFC3339Nano), Passed: []string{}}
	defer func() {
		receipt.Finished = time.Now().UTC().Format(time.RFC3339Nano)
		receipt.Success = retErr == nil
		if retErr != nil {
			receipt.Error = retErr.Error()
		}
		raw, err := json.MarshalIndent(receipt, "", "  ")
		if err == nil {
			err = writeExclusive(root, "result.json", append(raw, '\n'))
		}
		retErr = errors.Join(retErr, err, root.Close())
	}()

	packet, err := preparePacket(opt, controllerDir)
	if err != nil {
		return err
	}
	packetRaw, err := json.Marshal(packet)
	if err != nil {
		return err
	}
	if err := writeExclusive(root, "packet.json", packetRaw); err != nil {
		return err
	}
	receipt.Bind = entryBind{packet.Run, packet.Nonce, packet.OwnerSourceSHA256, packet.ControllerSourceSHA256, digest(packetRaw), packet.BinarySHA256}
	job, err := process.NewJobObserved(process.JobOptions{KillOnClose: true})
	if err != nil {
		if job != nil {
			err = errors.Join(err, job.Close())
		}
		return err
	}
	// Until the at-create owner is returned, this scope owns the empty Job.
	transferred := false
	defer func() {
		if !transferred {
			retErr = errors.Join(retErr, job.Close())
		}
	}()
	limits, err := observeJobLimits(job)
	if err != nil {
		return err
	}
	config := entryConfig{receipt.Bind, limits.Flags, limits.Bytes, opt.sourceDir, packet.Companions}
	configRaw, err := json.Marshal(config)
	if err != nil || !asciiJSON(configRaw) || len(configRaw) > frameLimit {
		return errors.Join(err, errors.New("entry config must fit the ASCII wire contract"))
	}
	if err := writeExclusive(root, "entry-config.json", configRaw); err != nil {
		return err
	}
	return runFixture(opt, root, job, limits, packet.Args, &receipt, &transferred)
}

func preparePacket(opt options, controllerDir string) (runPacket, error) {
	p := runPacket{Schema: "job-fixture-controller-packet-v1", Binary: opt.binary, SourceDir: opt.sourceDir, Companions: map[string]string{}, Args: []string{"-test.v", "-test.count=1", "-test.timeout=" + opt.timeout.String(), "-test.run=" + selectedTests}}
	if err := requireGUIFixtureImage(opt.binary); err != nil {
		return p, err
	}
	var err error
	p.Nonce, err = nonce()
	if err != nil {
		return p, err
	}
	p.Run = "job-entry-" + p.Nonce
	p.BinarySHA256, err = fileDigest(opt.binary)
	if err != nil {
		return p, err
	}
	exe, err := os.Executable()
	if err != nil {
		return p, err
	}
	p.ControllerBinarySHA256, err = fileDigest(exe)
	if err != nil {
		return p, err
	}
	p.OwnerSourceSHA256, err = fileDigest(filepath.Join(controllerDir, "owner_windows.go"))
	if err != nil {
		return p, err
	}
	p.ControllerSourceSHA256, err = fileDigest(filepath.Join(controllerDir, "main_windows.go"))
	if err != nil {
		return p, err
	}
	for _, name := range []string{"jobobject_windows.go", "current_job_windows.go", "jobobject_windows_test.go", "job_options_windows_test.go"} {
		p.Companions[name], err = fileDigest(filepath.Join(opt.sourceDir, name))
		if err != nil {
			return p, err
		}
	}
	return p, nil
}

// A console-subsystem test image can bring a native conhost into the new Job
// even with CREATE_NO_WINDOW. This fixture requires the original test process
// to be the sole entry member, so reject that build before creating any child.
func requireGUIFixtureImage(path string) (retErr error) {
	image, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("read fixture PE header: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, image.Close()) }()
	var subsystem uint16
	switch header := image.OptionalHeader.(type) {
	case *pe.OptionalHeader32:
		subsystem = header.Subsystem
	case *pe.OptionalHeader64:
		subsystem = header.Subsystem
	default:
		return errors.New("fixture requires a PE executable optional header")
	}
	if subsystem != pe.IMAGE_SUBSYSTEM_WINDOWS_GUI {
		return errors.New("fixture requires GUI subsystem: build process.test.exe with -ldflags=-H=windowsgui")
	}
	return nil
}

func runFixture(opt options, root *os.Root, job *process.Job, limits jobLimits, args []string, receipt *runReceipt, transferred *bool) (retErr error) {
	workDeadline := time.Now().Add(opt.timeout)
	finalDeadline := workDeadline.Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), workDeadline)
	defer cancel()
	var stdinRead, stdinWrite, stdoutRead, stdoutWrite, stderrRead, stderrWrite *os.File
	var owner *process.AtCreateJobProcess
	var stdout, stderr *streamCapture
	var wait process.OriginalRootWait
	var waitDone chan struct{}
	var grantWriterDone chan error
	natural := false
	defer func() {
		// One cleanup deadline and one original terminal action. Forced failure
		// cleanup never replaces a failed protocol/test run with success.
		cleanupDeadline := time.Now().Add(10 * time.Second)
		if finalDeadline.Before(cleanupDeadline) {
			cleanupDeadline = finalDeadline
		}
		cleanupCtx, cleanupCancel := context.WithDeadline(context.Background(), cleanupDeadline)
		defer cleanupCancel()
		retErr = errors.Join(retErr, closeFile(&stdinRead), closeFile(&stdoutWrite), closeFile(&stderrWrite))
		if owner != nil {
			view, settleErr := owner.ObserveOriginal(cleanupCtx, natural && retErr == nil)
			retErr = errors.Join(retErr, settleErr)
			if natural && view.TerminalAction != "not-requested" {
				retErr = errors.Join(retErr, errors.New("test left Job members requiring terminal action"))
			}
			if waitDone != nil {
				select {
				case <-waitDone:
					receipt.OriginalWaitKnown, receipt.OriginalExitCode = wait.Known, wait.ExitCode
					retErr = errors.Join(retErr, wait.Error)
				case <-cleanupCtx.Done():
					retErr = errors.Join(retErr, errors.New("original waiter remains unreceived"), cleanupCtx.Err())
				}
			}
			settled, releaseErr := owner.ReleaseOriginal()
			if settled != nil {
				view := settled.Snapshot()
				receipt.NativeSettlement = &view
				receipt.SettlementComplete = settled.Complete()
			}
			retErr = errors.Join(retErr, releaseErr)
			if !receipt.SettlementComplete {
				retErr = errors.Join(retErr, errors.New("original owner settlement incomplete"))
			}
		}
		// On a failed write, terminate the original owner before joining the
		// pipe writer. Its retained deadline is never extended for that join.
		retErr = errors.Join(retErr, closeFile(&stdinWrite))
		if grantWriterDone != nil {
			select {
			case writeErr := <-grantWriterDone:
				retErr = errors.Join(retErr, writeErr)
			case <-cleanupCtx.Done():
				retErr = errors.Join(retErr, errors.New("GRANT writer remains unjoined"), cleanupCtx.Err())
			}
		}
		// Forced closure unblocks a failed/oversized stream; it cannot mint EOF.
		if retErr != nil {
			retErr = errors.Join(retErr, closeFile(&stdoutRead), closeFile(&stderrRead))
		}
		for _, capture := range []*streamCapture{stdout, stderr} {
			if capture == nil {
				continue
			}
			select {
			case <-capture.done:
				retErr = errors.Join(retErr, capture.result.err)
			case <-cleanupCtx.Done():
				retErr = errors.Join(retErr, errors.New("stream worker remains unjoined"), cleanupCtx.Err())
			}
		}
		retErr = errors.Join(retErr, closeFile(&stdoutRead), closeFile(&stderrRead))
	}()
	var err error
	stdinRead, stdinWrite, err = os.Pipe()
	if err != nil {
		return err
	}
	stdoutRead, stdoutWrite, err = os.Pipe()
	if err != nil {
		return err
	}
	stderrRead, stderrWrite, err = os.Pipe()
	if err != nil {
		return err
	}
	stdout, err = startCapture(root, "stdout.log", stdoutRead, true)
	if err != nil {
		return err
	}
	stderr, err = startCapture(root, "stderr.log", stderrRead, false)
	if err != nil {
		return err
	}
	cmd := exec.Command(opt.binary, args...)
	cmd.Dir = opt.sourceDir
	cmd.Env = fixtureEnvironment(os.Environ(), filepath.Join(opt.runRoot, "entry-config.json"))
	process.NoConsole(cmd)
	owner, err = process.StartWithJobFilesOriginal(job, cmd, stdinRead, stdoutWrite, stderrWrite)
	if owner != nil {
		*transferred = true
		if owner.Created() {
			waitDone = make(chan struct{})
			go func() { wait = owner.WaitOriginal(); close(waitDone) }()
		}
	}
	closeErr := errors.Join(closeFile(&stdinRead), closeFile(&stdoutWrite), closeFile(&stderrWrite))
	if err != nil || closeErr != nil || owner == nil || waitDone == nil {
		return errors.Join(err, closeErr, errors.New("test process did not transfer a complete at-create owner"))
	}
	readyRaw, err := nextFrame(ctx, stdout, stderr)
	if err != nil {
		return err
	}
	var ready entryReady
	if err := decodeCanonical(readyRaw, &ready); err != nil {
		return err
	}
	if err := validateReady(ready, receipt.Bind); err != nil {
		return err
	}
	receipt.Ready = &ready
	// The receiver starts its own ten-second clock immediately before READY.
	// A stricter local window leaves room for its write/EOF/ACK checks.
	handshake, handshakeCancel := context.WithTimeout(ctx, 8*time.Second)
	defer handshakeCancel()
	native, observed, err := observeGrant(handshake, owner, job, ready, limits)
	receipt.Observation = &observed
	if err != nil {
		return err
	}
	generation, err := nonce()
	if err != nil {
		return err
	}
	grant := entryGrant{1, "grant", ready.Bind, ready.Challenge, ready.PID, ready.Created100ns, generation, native, true}
	grantRaw, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	frame := append(append([]byte(entryPrefix), grantRaw...), '\n')
	if len(frame) > frameLimit {
		return errors.New("GRANT exceeds frame limit")
	}
	grantWriterDone = make(chan error, 1)
	writeDone := grantWriterDone
	go func(writer *os.File) {
		n, err := writer.Write(frame)
		if n != len(frame) {
			err = errors.Join(err, io.ErrShortWrite)
		}
		writeDone <- err
	}(stdinWrite)
	select {
	case err = <-writeDone:
		grantWriterDone = nil
	case <-handshake.Done():
		return handshake.Err()
	}
	err = errors.Join(err, closeFile(&stdinWrite)) // EOF is part of GRANT, not optional cleanup.
	if err != nil {
		return err
	}
	receipt.Grant = &grant
	ackRaw, err := nextFrame(handshake, stdout, stderr)
	if err != nil {
		return err
	}
	var ack entryAck
	if err := decodeCanonical(ackRaw, &ack); err != nil {
		return err
	}
	if err := validateAck(ack, ready, grant, grantRaw); err != nil {
		return err
	}
	if err := handshake.Err(); err != nil {
		return err
	}
	receipt.Ack = &ack
	if err := awaitCompletion(ctx, waitDone, stdout, stderr); err != nil {
		return err
	}
	if !wait.Known || wait.Error != nil || wait.ExitCode != 0 {
		return errors.Join(wait.Error, fmt.Errorf("test original wait known=%v exit=%d", wait.Known, wait.ExitCode))
	}
	if len(stdout.frames) != 0 || stdout.result.frames != 2 {
		return errors.New("unexpected control frame after ACK")
	}
	receipt.StdoutEOF, receipt.StderrEOF = stdout.result.eof, stderr.result.eof
	for _, name := range requiredPasses {
		if !stdout.result.passed[name] {
			return fmt.Errorf("required test did not pass: %s", name)
		}
		receipt.Passed = append(receipt.Passed, name)
	}
	natural = true
	return nil
}

type captureResult struct {
	eof    bool
	err    error
	frames int
	passed map[string]bool
}
type streamCapture struct {
	frames chan []byte
	done   chan struct{}
	result captureResult
}

func startCapture(root *os.Root, name string, reader *os.File, control bool) (*streamCapture, error) {
	log, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	c := &streamCapture{frames: make(chan []byte, 2), done: make(chan struct{})}
	go func() {
		c.result = captureStream(reader, log, control, c.frames)
		c.result.err = errors.Join(c.result.err, log.Sync(), log.Close())
		close(c.done)
	}()
	return c, nil
}

func captureStream(input io.Reader, log io.Writer, control bool, frames chan<- []byte) captureResult {
	r := captureResult{passed: map[string]bool{}}
	reader := bufio.NewReader(io.LimitReader(input, streamLimit+1))
	written := 0
	for {
		line, err := reader.ReadBytes('\n')
		if written+len(line) > streamLimit {
			r.err = errors.New("fixture stream exceeds 2 MiB")
			return r
		}
		if len(line) != 0 {
			n, writeErr := log.Write(line)
			written += n
			if writeErr != nil || n != len(line) {
				r.err = errors.Join(writeErr, io.ErrShortWrite)
				return r
			}
			if len(line) > 64<<10 {
				r.err = errors.New("fixture stream line exceeds 64 KiB")
				return r
			}
			if control && bytes.HasPrefix(line, []byte(entryPrefix)) {
				if len(line) > frameLimit || line[len(line)-1] != '\n' || r.frames == 2 {
					r.err = errors.New("oversized, truncated or extra entry frame")
					return r
				}
				r.frames++
				frames <- append([]byte(nil), line[len(entryPrefix):len(line)-1]...)
			}
			if control {
				text := strings.TrimSpace(string(line))
				for _, name := range requiredPasses {
					if strings.HasPrefix(text, "--- PASS: "+name+" (") {
						r.passed[name] = true
					}
				}
			}
		}
		if err != nil {
			r.eof = err == io.EOF
			if !r.eof {
				r.err = err
			}
			return r
		}
	}
}

func nextFrame(ctx context.Context, stdout, stderr *streamCapture) ([]byte, error) {
	stderrDone := stderr.done
	for {
		select {
		case raw := <-stdout.frames:
			return raw, nil
		case <-stdout.done:
			// Buffered frames precede EOF even if the scheduler observes done first.
			select {
			case raw := <-stdout.frames:
				return raw, nil
			default:
			}
			return nil, errors.Join(stdout.result.err, errors.New("stdout ended before required entry frame"))
		case <-stderrDone:
			if stderr.result.err != nil {
				return nil, stderr.result.err
			}
			stderrDone = nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func awaitCompletion(ctx context.Context, waitDone <-chan struct{}, stdout, stderr *streamCapture) error {
	outDone, errDone := (<-chan struct{})(stdout.done), (<-chan struct{})(stderr.done)
	for waitDone != nil || outDone != nil || errDone != nil {
		select {
		case <-waitDone:
			waitDone = nil
		case <-outDone:
			if stdout.result.err != nil || !stdout.result.eof {
				return errors.Join(stdout.result.err, errors.New("stdout did not finish at EOF"))
			}
			outDone = nil
		case <-errDone:
			if stderr.result.err != nil || !stderr.result.eof {
				return errors.Join(stderr.result.err, errors.New("stderr did not finish at EOF"))
			}
			errDone = nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Both receiver messages are produced by json.Marshal on these fixed structs.
// Requiring those exact bytes also rejects duplicate/missing/noncanonical keys.
func decodeCanonical(raw []byte, target any) error {
	if len(raw) == 0 || len(raw) > frameLimit || !asciiJSON(raw) {
		return errors.New("invalid entry JSON size or encoding")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing entry JSON")
	}
	canonical, err := json.Marshal(target)
	if err != nil || !bytes.Equal(raw, canonical) {
		return errors.Join(err, errors.New("entry JSON differs from canonical receiver encoding"))
	}
	return nil
}
func validateReady(ready entryReady, bind entryBind) error {
	created, err := strconv.ParseUint(ready.Created100ns, 10, 64)
	if ready.V != 1 || ready.Kind != "ready" || ready.Bind != bind || !lowerHex(ready.Challenge, 32) || ready.PID == 0 || err != nil || created == 0 || strconv.FormatUint(created, 10) != ready.Created100ns {
		return errors.New("READY version, run binding, challenge or native generation differs")
	}
	return nil
}
func validateAck(ack entryAck, ready entryReady, grant entryGrant, raw []byte) error {
	if ack.V != 1 || ack.Kind != "ack" || ack.Nonce != ready.Bind.Nonce || ack.Challenge != ready.Challenge || ack.OwnerGeneration != grant.OwnerGeneration || ack.GrantSHA256 != digest(raw) || !ack.Confirm {
		return errors.New("ACK does not confirm this exact GRANT and owner generation")
	}
	return nil
}
func fixtureEnvironment(environment []string, config string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		// Exclude inherited MCPHUB test-helper modes and stale fixture inputs.
		if !strings.HasPrefix(strings.ToUpper(key), "MCPHUB_") {
			result = append(result, entry)
		}
	}
	return append(result, entryConfigEnv+"="+config)
}
func asciiJSON(raw []byte) bool {
	for _, b := range raw {
		if b > 127 || b < 32 {
			return false
		}
	}
	return true
}
func lowerHex(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func nonce() (string, error) {
	var raw [16]byte
	_, err := rand.Read(raw[:])
	return hex.EncodeToString(raw[:]), err
}
func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
func fileDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("digest input is not an ordinary file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, readErr := io.Copy(h, f)
	err = errors.Join(readErr, f.Close())
	return hex.EncodeToString(h.Sum(nil)), err
}
func writeExclusive(root *os.Root, name string, raw []byte) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, writeErr := f.Write(raw)
	if n != len(raw) {
		writeErr = errors.Join(writeErr, io.ErrShortWrite)
	}
	return errors.Join(writeErr, f.Sync(), f.Close())
}
func closeFile(file **os.File) error {
	if *file == nil {
		return nil
	}
	f := *file
	*file = nil
	return f.Close()
}

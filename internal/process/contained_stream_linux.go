//go:build linux

package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	LinuxProcfsClassifierHelperCommand = "__linux-procfs-classifier"
	linuxClassifierFramePrefix         = "mcphub-linux-procfs-v1:"
	linuxClassifierMaxFrameBytes       = 128
)

var (
	errLinuxHelperStartFailed     = errors.New("LINUX_GROUP_SETTLEMENT_HELPER_START_FAILED")
	errLinuxHelperProtocolInvalid = errors.New("LINUX_GROUP_SETTLEMENT_HELPER_PROTOCOL_INVALID")
	errLinuxProcOpenFailed        = errors.New("LINUX_GROUP_SETTLEMENT_OPEN_FAILED")
	errLinuxProcReadFailed        = errors.New("LINUX_GROUP_SETTLEMENT_READ_FAILED")
	errLinuxProcParseFailed       = errors.New("LINUX_GROUP_SETTLEMENT_PARSE_FAILED")
	errLinuxProcCloseFailed       = errors.New("LINUX_GROUP_SETTLEMENT_CLOSE_FAILED")
	errLinuxProcWorkerFailed      = errors.New("LINUX_GROUP_SETTLEMENT_WORKER_FAILED")
)

type linuxProcSource interface {
	ReadDirNames(int) ([]string, error)
	ReadStat(string) ([]byte, error)
	Close() error
}

type realLinuxProcSource struct{ dir *os.File }

func (s *realLinuxProcSource) ReadDirNames(count int) ([]string, error) {
	entries, err := s.dir.ReadDir(count)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, err
}

func (*realLinuxProcSource) ReadStat(name string) ([]byte, error) {
	return os.ReadFile("/proc/" + name + "/stat")
}

func (s *realLinuxProcSource) Close() error { return s.dir.Close() }

type linuxHelperCommandFactory func(context.Context, string, ...string) *exec.Cmd

func platformContainedGroupClassifier() posixGroupClassifier {
	return runLinuxGroupClassifier
}

func defaultLinuxHelperCommand(ctx context.Context, executable string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, executable, args...)
}

type boundedLinuxHelperOutput struct {
	data     []byte
	overflow bool
}

func (w *boundedLinuxHelperOutput) Write(data []byte) (int, error) {
	remaining := linuxClassifierMaxFrameBytes + 1 - len(w.data)
	if remaining > 0 {
		w.data = append(w.data, data[:min(len(data), remaining)]...)
	}
	if len(w.data) > linuxClassifierMaxFrameBytes || len(data) > remaining {
		w.overflow = true
	}
	return len(data), nil
}

func runLinuxGroupClassifier(ctx context.Context, pgid int, budget posixSettlementBudget) (bool, error) {
	return runLinuxGroupClassifierWithFactory(ctx, pgid, budget, defaultLinuxHelperCommand)
}

func runLinuxGroupClassifierWithFactory(
	ctx context.Context,
	pgid int,
	budget posixSettlementBudget,
	factory linuxHelperCommandFactory,
) (bool, error) {
	if pgid <= 0 || factory == nil || budget.helperShutdownReserve <= 0 ||
		!budget.helperDeadline.Before(budget.settlementDeadline) ||
		!timeBeforeNow(budget.helperDeadline) {
		return false, errLinuxGroupSettlementBudgetExhausted
	}
	executable, err := os.Executable()
	if err != nil {
		return false, errors.Join(errLinuxHelperStartFailed, err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return false, errors.Join(errLinuxHelperStartFailed, err)
	}
	if !filepath.IsAbs(executable) {
		return false, errors.Join(errLinuxHelperStartFailed, errors.New("resolved executable is not absolute"))
	}

	helperCtx, cancel := context.WithDeadline(ctx, budget.helperDeadline)
	defer cancel()
	cmd := factory(helperCtx, executable, LinuxProcfsClassifierHelperCommand, strconv.Itoa(pgid))
	if cmd == nil {
		return false, errLinuxHelperStartFailed
	}
	output := &boundedLinuxHelperOutput{}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = budget.helperShutdownReserve
	SetParentDeathSignal(cmd)
	if err := cmd.Start(); err != nil {
		return false, errors.Join(errLinuxHelperStartFailed, err)
	}
	waitErr := cmd.Wait()
	frame, frameErr := parseLinuxClassifierFrame(output.data, output.overflow)
	deadlineErr := helperCtx.Err()
	if frameErr != nil {
		if errors.Is(deadlineErr, context.DeadlineExceeded) && len(output.data) == 0 && !output.overflow && linuxHelperWasDeadlineKilled(waitErr) {
			return false, errLinuxGroupSettlementBudgetExhausted
		}
		return false, errors.Join(errLinuxHelperProtocolInvalid, frameErr, fixedLinuxHelperWaitError(waitErr), deadlineErr)
	}
	if frame.err != nil {
		return false, errors.Join(frame.err, fixedLinuxHelperWaitError(waitErr), deadlineErr)
	}
	if waitErr != nil {
		return false, errors.Join(errLinuxHelperProtocolInvalid, fixedLinuxHelperWaitError(waitErr), deadlineErr)
	}
	return frame.settled, nil
}

func timeBeforeNow(deadline time.Time) bool { return time.Now().Before(deadline) }

func linuxHelperWasDeadlineKilled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ProcessState == nil {
		return false
	}
	status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

func fixedLinuxHelperWaitError(err error) error {
	if err == nil {
		return nil
	}
	return errors.New("LINUX_GROUP_SETTLEMENT_HELPER_WAIT_FAILED")
}

type linuxClassifierFrame struct {
	settled bool
	err     error
}

func parseLinuxClassifierFrame(data []byte, overflow bool) (linuxClassifierFrame, error) {
	if overflow || len(data) == 0 || len(data) > linuxClassifierMaxFrameBytes || data[len(data)-1] != '\n' || bytes.Count(data, []byte{'\n'}) != 1 {
		return linuxClassifierFrame{}, errLinuxHelperProtocolInvalid
	}
	body := strings.TrimSuffix(string(data), "\n")
	if !strings.HasPrefix(body, linuxClassifierFramePrefix) {
		return linuxClassifierFrame{}, errLinuxHelperProtocolInvalid
	}
	payload := strings.TrimPrefix(body, linuxClassifierFramePrefix)
	switch payload {
	case "settled":
		return linuxClassifierFrame{settled: true}, nil
	case "live":
		return linuxClassifierFrame{}, nil
	}
	if !strings.HasPrefix(payload, "failure:") {
		return linuxClassifierFrame{}, errLinuxHelperProtocolInvalid
	}
	parts := strings.Split(strings.TrimPrefix(payload, "failure:"), ",")
	var out error
	lastOrder := -1
	for _, part := range parts {
		order, cause := linuxFrameFailure(part)
		if cause == nil || order <= lastOrder {
			return linuxClassifierFrame{}, errLinuxHelperProtocolInvalid
		}
		lastOrder = order
		out = errors.Join(out, cause)
	}
	if out == nil {
		return linuxClassifierFrame{}, errLinuxHelperProtocolInvalid
	}
	return linuxClassifierFrame{err: out}, nil
}

func linuxFrameFailure(value string) (int, error) {
	switch value {
	case "open":
		return 0, errLinuxProcOpenFailed
	case "read":
		return 1, errLinuxProcReadFailed
	case "parse":
		return 2, errLinuxProcParseFailed
	case "close":
		return 3, errLinuxProcCloseFailed
	case "worker":
		return 4, errLinuxProcWorkerFailed
	default:
		return -1, nil
	}
}

// RunLinuxProcfsClassifierHelper is the Linux-only worker invoked by the
// hidden CLI adapter. It emits exactly one fixed, bounded frame and never raw
// procfs data or errors.
func RunLinuxProcfsClassifierHelper(pgid int, output io.Writer) error {
	if pgid <= 0 || output == nil {
		return errLinuxProcWorkerFailed
	}
	settled, err := containedGroupSettled(context.Background(), pgid)
	frame := encodeLinuxClassifierFrame(settled, err)
	_, writeErr := io.WriteString(output, frame)
	return writeErr
}

func encodeLinuxClassifierFrame(settled bool, err error) string {
	if err == nil {
		if settled {
			return linuxClassifierFramePrefix + "settled\n"
		}
		return linuxClassifierFramePrefix + "live\n"
	}
	parts := make([]string, 0, 4)
	for _, item := range []struct {
		name string
		err  error
	}{
		{"open", errLinuxProcOpenFailed},
		{"read", errLinuxProcReadFailed},
		{"parse", errLinuxProcParseFailed},
		{"close", errLinuxProcCloseFailed},
	} {
		if errors.Is(err, item.err) {
			parts = append(parts, item.name)
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "worker")
	}
	return linuxClassifierFramePrefix + "failure:" + strings.Join(parts, ",") + "\n"
}

func containedGroupSettled(ctx context.Context, pgid int) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	dir, err := os.Open("/proc")
	if err != nil {
		return false, errors.Join(errLinuxProcOpenFailed, err)
	}
	return containedGroupSettledFromSource(ctx, pgid, &realLinuxProcSource{dir: dir})
}

func containedGroupSettledFromSource(ctx context.Context, pgid int, source linuxProcSource) (settled bool, resultErr error) {
	if source == nil {
		return false, errLinuxProcWorkerFailed
	}
	defer func() {
		if err := source.Close(); err != nil {
			settled = false
			resultErr = errors.Join(resultErr, errLinuxProcCloseFailed, err)
		}
	}()
	settlement := linuxGroupSettlement{pgid: pgid}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		names, readErr := source.ReadDirNames(128)
		if err := ctx.Err(); err != nil {
			return false, err
		}
		for _, name := range names {
			pid, parseErr := strconv.Atoi(name)
			if parseErr != nil || pid <= 0 {
				continue
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			data, statErr := source.ReadStat(name)
			if statErr != nil {
				if errors.Is(statErr, os.ErrNotExist) {
					continue
				}
				return false, errors.Join(errLinuxProcReadFailed, statErr)
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if statErr := settlement.observe(string(data)); statErr != nil {
				return false, errors.Join(errLinuxProcParseFailed, statErr)
			}
			if settlement.live {
				return false, nil
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return false, errors.Join(errLinuxProcReadFailed, readErr)
		}
	}
	return true, nil
}

type linuxGroupSettlement struct {
	pgid int
	live bool
}

func (s *linuxGroupSettlement) observe(stat string) error {
	state, memberPGID, err := linuxProcStateAndGroup(stat)
	if err != nil {
		return err
	}
	if memberPGID == s.pgid && state != 'Z' {
		s.live = true
	}
	return nil
}

func (s linuxGroupSettlement) settled() bool { return !s.live }

func linuxProcStateAndGroup(stat string) (byte, int, error) {
	end := strings.LastIndex(stat, ")")
	if end < 0 {
		return 0, 0, fmt.Errorf("malformed proc stat")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 3 {
		return 0, 0, fmt.Errorf("short proc stat")
	}
	group, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, err
	}
	if len(fields[0]) != 1 {
		return 0, 0, fmt.Errorf("invalid proc state")
	}
	return fields[0][0], group, nil
}

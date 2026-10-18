//go:build windows

package process

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type identityQueryBackend uint8

var errIdentityCollectorsUnavailable = errors.New("process: PowerShell CLM-locked AND wmic.exe absent: cannot resolve process identity")

const (
	identityQueryPowerShell identityQueryBackend = iota + 1
	identityQueryWMIC
)

// LookupProcessIdentitiesContext freshly queries only the unique positive
// requested PIDs. Missing keys mean proven query absence. Any collection,
// decoding or cancellation failure returns nil, never a partial identity map.
// Selection and results live only until this synchronous operation returns.
func LookupProcessIdentitiesContext(ctx context.Context, pids []int) (map[int]ProcessIdentity, error) {
	return lookupProcessIdentitiesWithQuery(ctx, pids, queryProcessIdentitySet)
}

func lookupProcessIdentitiesWithQuery(ctx context.Context, pids []int, query func(context.Context, identityQueryBackend, []int) ([]byte, error)) (map[int]ProcessIdentity, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	requested := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		if pid <= 0 {
			return nil, errors.New("process: identity set requires positive PIDs")
		}
		if _, duplicate := requested[pid]; duplicate {
			return nil, errors.New("process: duplicate requested identity PID")
		}
		requested[pid] = struct{}{}
	}
	if len(pids) == 0 {
		return make(map[int]ProcessIdentity), nil
	}
	// Copy caller input before acquisition; no retained input or global cache.
	pids = append([]int(nil), pids...)
	backend, err := selectIdentityQueryBackend(ctx)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 1; attempt <= lookupRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out, err := query(ctx, backend, pids)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		var set map[int]ProcessIdentity
		if err == nil {
			if backend == identityQueryPowerShell {
				set, err = parsePowerShellIdentitySet(out, requested)
			} else {
				set, err = parseWmicIdentitySet(out, requested)
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err == nil {
			return set, nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		lastErr = err
		if attempt < lookupRetries {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(lookupRetryDelay):
			}
		}
	}
	return nil, fmt.Errorf("process: identity set failed after %d attempts: %w", lookupRetries, lastErr)
}

func queryProcessIdentitySet(ctx context.Context, backend identityQueryBackend, pids []int) ([]byte, error) {
	filters := make([]string, len(pids))
	for i, pid := range pids {
		filters[i] = "ProcessId=" + strconv.Itoa(pid)
	}
	filter := strings.Join(filters, " OR ")
	var cmd *exec.Cmd
	if backend == identityQueryPowerShell {
		// -InputObject deliberately preserves array shape for zero/one/many.
		// Collector and conversion errors terminate, never serialize partial rows.
		script := "$ErrorActionPreference='Stop'; $rows=@(Get-CimInstance -ClassName Win32_Process -Filter '" + filter +
			"' -ErrorAction Stop | Select-Object ProcessId, Name, CommandLine, ExecutablePath, " +
			"@{Name='CreationDateUnix';Expression={[int64]([System.DateTimeOffset]::new($_.CreationDate).ToUnixTimeSeconds())}}); " +
			"ConvertTo-Json -InputObject $rows -Compress"
		cmd = exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	} else {
		cmd = exec.CommandContext(ctx, "wmic.exe", "process", "where", filter, "get", "ProcessId,Name,CommandLine,CreationDate,ExecutablePath", "/format:csv")
	}
	NoConsole(cmd)
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, fmt.Errorf("process: identity set collector %d: %w", backend, err)
	}
	return out, nil
}

type psCimSetRecord struct {
	ProcessID        *int    `json:"ProcessId"`
	Name             *string `json:"Name"`
	CommandLine      *string `json:"CommandLine"`
	ExecutablePath   *string `json:"ExecutablePath"`
	CreationDateUnix *int64  `json:"CreationDateUnix"`
}

func parsePowerShellIdentitySet(out []byte, requested map[int]struct{}) (map[int]ProcessIdentity, error) {
	trimmed := strings.TrimSpace(string(out))
	if !strings.HasPrefix(trimmed, "[") {
		return nil, errors.New("process: PowerShell identity set must be a JSON array")
	}
	var rows []psCimSetRecord
	if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
		return nil, fmt.Errorf("process: PowerShell identity set decode: %w", err)
	}
	set := make(map[int]ProcessIdentity, len(rows))
	for _, row := range rows {
		if row.ProcessID == nil || row.Name == nil || row.CommandLine == nil || row.ExecutablePath == nil || row.CreationDateUnix == nil {
			return nil, errors.New("process: PowerShell identity set required field missing or null")
		}
		if err := validateIdentitySetPID(*row.ProcessID, requested, set); err != nil {
			return nil, err
		}
		set[*row.ProcessID] = identityFromCIMRecord(*row.ProcessID, psCimRecord{Name: *row.Name, CommandLine: *row.CommandLine, ExecutablePath: *row.ExecutablePath, CreationDateUnix: *row.CreationDateUnix})
	}
	return set, nil
}

func validateIdentitySetPID(pid int, requested map[int]struct{}, set map[int]ProcessIdentity) error {
	if pid <= 0 {
		return errors.New("process: identity set returned nonpositive PID")
	}
	if _, ok := requested[pid]; !ok {
		return errors.New("process: identity set returned unrequested PID")
	}
	if _, exists := set[pid]; exists {
		return errors.New("process: identity set returned duplicate PID")
	}
	return nil
}

func parseWmicIdentitySet(out []byte, requested map[int]struct{}) (map[int]ProcessIdentity, error) {
	records, err := ReadWmicCSVRecords(strings.NewReader(string(out)))
	if err != nil {
		return nil, fmt.Errorf("process: WMIC identity set decode: %w", err)
	}
	var header WmicCSVHeader
	headerOK := false
	set := make(map[int]ProcessIdentity)
	for _, record := range records {
		if strings.TrimSpace(record) == "" {
			continue
		}
		if !headerOK {
			header, headerOK = ParseWmicCSVHeader(record, "Node", "ProcessId", "Name", "CommandLine", "ExecutablePath", "CreationDate")
			if !headerOK {
				return nil, errors.New("process: WMIC identity set required header invalid")
			}
			continue
		}
		row, ok := ParseWmicProcessCSVRecord(record, header)
		if !ok {
			// The shared tolerant WMIC parser also validates fixed field shapes.
			// Qualify unusable creation without treating a rejected row as absence.
			fields := SplitWmicCSVLine(record)
			if len(fields) == len(header.columns) {
				if _, err := normalizeWmicCreationDate(fields[header.index["CreationDate"]], wmicCreationUTCSet); err != nil {
					return nil, err
				}
			}
			return nil, errors.New("process: WMIC identity set malformed row")
		}
		pid, err := strconv.Atoi(row["ProcessId"])
		if err != nil {
			return nil, fmt.Errorf("process: WMIC identity set PID type: %w", err)
		}
		if err := validateIdentitySetPID(pid, requested, set); err != nil {
			return nil, err
		}
		created, err := normalizeWmicCreationDate(row["CreationDate"], wmicCreationUTCSet)
		if err != nil {
			return nil, err
		}
		set[pid] = identityFromCIMRecord(pid, psCimRecord{Name: row["Name"], CommandLine: row["CommandLine"], ExecutablePath: row["ExecutablePath"], CreationDateUnix: created})
	}
	if !headerOK {
		return nil, errors.New("process: WMIC identity set required header missing")
	}
	return set, nil
}

type wmicCreationProjection uint8

const (
	wmicCreationLegacyScalar wmicCreationProjection = iota
	wmicCreationUTCSet
)

// Both projections parse the civil prefix here. Scalar explicitly retains its
// prefix-only/parse-fallback contract; the new set requires the represented UTC
// instant, including the reported minute offset, independently of host zone.
func normalizeWmicCreationDate(raw string, projection wmicCreationProjection) (int64, error) {
	var civil time.Time
	var err error
	if len(raw) >= 14 {
		civil, err = time.Parse("20060102150405", raw[:14])
	} else {
		err = errors.New("short creation date")
	}
	if projection == wmicCreationLegacyScalar {
		if err != nil {
			return 0, nil
		}
		return civil.Unix(), nil
	}
	if err != nil || len(raw) != 25 || raw[14] != '.' || (raw[21] != '+' && raw[21] != '-') {
		return 0, errors.New("process: WMIC creation normalization invalid date/fraction/offset")
	}
	for i := 15; i < len(raw); i++ {
		if i == 21 {
			continue
		}
		if raw[i] < '0' || raw[i] > '9' {
			return 0, errors.New("process: WMIC creation normalization unknown or malformed fraction/offset")
		}
	}
	minutes, err := strconv.Atoi(raw[21:])
	if err != nil {
		return 0, fmt.Errorf("process: WMIC creation normalization offset: %w", err)
	}
	return civil.Add(-time.Duration(minutes) * time.Minute).UTC().Unix(), nil
}

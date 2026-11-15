package api

import (
	"context"
	"encoding/json"
	"errors"
	"mcp-local-hub/internal/process"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

type OriginalLoopbackTCPRowV1 struct {
	State      string `json:"state"`
	LocalHost  string `json:"local_host"`
	LocalPort  uint16 `json:"local_port"`
	RemoteHost string `json:"remote_host"`
	RemotePort uint16 `json:"remote_port"`
	PID        uint32 `json:"pid"`
}
type LoopbackTCPRowsObservation struct {
	issuer                                   *struct{ marker byte }
	receipt                                  process.ContainedStreamReceiptV1
	rows                                     []OriginalLoopbackTCPRowV1
	sourceSHA256, frameSHA256, receiptSHA256 string
	err                                      error
}

func (o *LoopbackTCPRowsObservation) Validate(ctx context.Context) error {
	if o == nil || ctx == nil || ctx.Err() != nil || o.issuer == nil || o.err != nil || !o.receipt.RootWaitKnown || o.receipt.RootExitCode != 0 || !o.receipt.NativeSettlement.Complete() || !o.receipt.IOSettlement.Complete() || !preparedSerenaDigest(o.sourceSHA256) || !preparedSerenaDigest(o.frameSHA256) || !preparedSerenaDigest(o.receiptSHA256) {
		return errors.New("ORIGINAL_TCP_ROWS_UNQUALIFIED")
	}
	return nil
}
func (o *LoopbackTCPRowsObservation) Rows(ctx context.Context) ([]OriginalLoopbackTCPRowV1, error) {
	if err := o.Validate(ctx); err != nil {
		return nil, err
	}
	return append([]OriginalLoopbackTCPRowV1(nil), o.rows...), nil
}
func (o *LoopbackTCPRowsObservation) ReceiptSHA256() string {
	if o == nil || o.issuer == nil || o.err != nil {
		return ""
	}
	return o.receiptSHA256
}
func parseOriginalLoopbackTCPRows(raw []byte) ([]OriginalLoopbackTCPRowV1, error) {
	if len(raw) == 0 || len(raw) > 64<<20 || strings.ContainsRune(string(raw), 0) {
		return nil, errors.New("ORIGINAL_TCP_ROW_FRAME_INVALID")
	}
	const (
		englishTitle  = "Active Connections"
		englishHeader = "Proto Local Address Foreign Address State PID"
		// Observed Russian netstat labels in OEM code page 866:
		// "Активные подключения" and
		// "Имя Локальный адрес Внешний адрес Состояние PID".
		// Match their exact bytes without guessing or decoding other locales.
		russianOEMTitle  = "\x80\xaa\xe2\xa8\xa2\xad\xeb\xa5 \xaf\xae\xa4\xaa\xab\xee\xe7\xa5\xad\xa8\xef"
		russianOEMHeader = "\x88\xac\xef \x8b\xae\xaa\xa0\xab\xec\xad\xeb\xa9 \xa0\xa4\xe0\xa5\xe1 \x82\xad\xa5\xe8\xad\xa8\xa9 \xa0\xa4\xe0\xa5\xe1 \x91\xae\xe1\xe2\xae\xef\xad\xa8\xa5 PID"
	)
	rows := []OriginalLoopbackTCPRowV1{}
	seen := map[string]bool{}
	header := false
	expectedHeader := ""
	for line := range strings.SplitSeq(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if !header {
			switch line := strings.Join(fields, " "); line {
			case englishTitle, russianOEMTitle:
				if expectedHeader != "" {
					return nil, errors.New("ORIGINAL_TCP_ROW_FRAME_INVALID")
				}
				expectedHeader = englishHeader
				if line == russianOEMTitle {
					expectedHeader = russianOEMHeader
				}
			case englishHeader, russianOEMHeader:
				if expectedHeader != "" && line != expectedHeader {
					return nil, errors.New("ORIGINAL_TCP_ROW_HEADER_UNSUPPORTED")
				}
				header = true
			default:
				if fields[0] == "Proto" {
					return nil, errors.New("ORIGINAL_TCP_ROW_HEADER_UNSUPPORTED")
				}
				return nil, errors.New("ORIGINAL_TCP_ROW_FRAME_UNRECOGNIZED")
			}
			continue
		}
		if fields[0] == "UDP" {
			if len(fields) != 4 {
				return nil, errors.New("ORIGINAL_TCP_ROW_FRAME_INVALID")
			}
			continue
		}
		if fields[0] != "TCP" || len(fields) != 5 {
			return nil, errors.New("ORIGINAL_TCP_ROW_TYPE_INVALID")
		}
		localHost, localPort, localErr := parseOriginalTCPEndpoint(fields[1])
		remoteHost, remotePort, remoteErr := parseOriginalTCPEndpoint(fields[2])
		if localErr != nil || remoteErr != nil {
			return nil, errors.Join(localErr, remoteErr)
		}
		pid, err := strconv.ParseUint(fields[4], 10, 32)
		if err != nil {
			return nil, errors.New("ORIGINAL_TCP_ROW_PID_INVALID")
		}
		switch fields[3] {
		case "LISTENING", "ESTABLISHED":
			// Wildcard listeners can occupy this loopback endpoint. Retain them
			// as refusal evidence; they can never supply positive ownership.
			if localHost != "127.0.0.1" && !(fields[3] == "LISTENING" && (localHost == "0.0.0.0" || localHost == "::")) {
				continue
			}
			if pid == 0 || localPort == 0 || (fields[3] == "ESTABLISHED" && remotePort == 0) {
				return nil, errors.New("ORIGINAL_TCP_ROW_ENDPOINT_INVALID")
			}
			row := OriginalLoopbackTCPRowV1{fields[3], localHost, localPort, remoteHost, remotePort, uint32(pid)}
			key := row.State + "\x00" + fields[1] + "\x00" + fields[2]
			if seen[key] {
				return nil, errors.New("ORIGINAL_TCP_ROW_DUPLICATE_OR_CONFLICT")
			}
			seen[key] = true
			rows = append(rows, row)
		case "CLOSED", "SYN_SENT", "SYN_RECEIVED", "FIN_WAIT_1", "FIN_WAIT_2", "CLOSE_WAIT", "CLOSING", "LAST_ACK", "TIME_WAIT", "DELETE_TCB":
		default:
			return nil, errors.New("ORIGINAL_TCP_ROW_STATE_UNSUPPORTED")
		}
	}
	if !header {
		return nil, errors.New("ORIGINAL_TCP_ROW_HEADER_MISSING")
	}
	return rows, nil
}
func parseOriginalTCPEndpoint(value string) (string, uint16, error) {
	host, portText, err := net.SplitHostPort(value)
	if err != nil {
		return "", 0, errors.New("ORIGINAL_TCP_ROW_ADDRESS_INVALID")
	}
	if zoneStart := strings.IndexByte(host, '%'); zoneStart >= 0 {
		zone := host[zoneStart+1:]
		if len(zone) == 0 || len(zone) > 10 {
			return "", 0, errors.New("ORIGINAL_TCP_ROW_ADDRESS_INVALID")
		}
		for i := 0; i < len(zone); i++ {
			if zone[i] < '0' || zone[i] > '9' {
				return "", 0, errors.New("ORIGINAL_TCP_ROW_ADDRESS_INVALID")
			}
		}
		if _, err := strconv.ParseUint(zone, 10, 32); err != nil {
			return "", 0, errors.New("ORIGINAL_TCP_ROW_ADDRESS_INVALID")
		}
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return "", 0, errors.New("ORIGINAL_TCP_ROW_ADDRESS_INVALID")
	}
	number, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return "", 0, errors.New("ORIGINAL_TCP_ROW_PORT_INVALID")
	}
	// Keep IPv6 spelling (including mapped IPv4) out of positive IPv4
	// ownership. Normalize only unspecified IPv6 for wildcard refusal.
	if strings.Contains(host, ":") {
		if address.WithZone("").IsUnspecified() {
			return "::", uint16(number), nil
		}
		return host, uint16(number), nil
	}
	return address.String(), uint16(number), nil
}
func tcpObservedReceiptSHA(o *LoopbackTCPRowsObservation) (string, error) {
	raw, err := json.Marshal(struct {
		Schema, Source, Frame string
		Root                  process.ContainedRootIdentity
		Rows                  []OriginalLoopbackTCPRowV1
	}{"original-loopback-tcp-rows-v1", o.sourceSHA256, o.frameSHA256, o.receipt.RootIdentity, o.rows})
	if err != nil {
		return "", err
	}
	return preparedSerenaHash(raw), nil
}

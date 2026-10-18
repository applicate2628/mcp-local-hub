package api

import (
	"context"
	"encoding/json"
	"errors"
	"mcp-local-hub/internal/process"
	"net"
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
	rows := []OriginalLoopbackTCPRowV1{}
	seen := map[string]bool{}
	header := false
	for line := range strings.SplitSeq(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) == 2 && fields[0] == "Active" && fields[1] == "Connections" {
			continue
		}
		if fields[0] == "Proto" {
			if strings.Join(fields, " ") != "Proto Local Address Foreign Address State PID" {
				return nil, errors.New("ORIGINAL_TCP_ROW_HEADER_UNSUPPORTED")
			}
			header = true
			continue
		}
		if !header {
			return nil, errors.New("ORIGINAL_TCP_ROW_FRAME_UNRECOGNIZED")
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
	address := net.ParseIP(host)
	if address == nil {
		return "", 0, errors.New("ORIGINAL_TCP_ROW_ADDRESS_INVALID")
	}
	number, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return "", 0, errors.New("ORIGINAL_TCP_ROW_PORT_INVALID")
	}
	// IPv6/non-loopback rows are parsed but cannot enter this explicitly IPv4
	// original-endpoint evidence. They are not converted to a wildcard owner.
	if strings.Contains(host, ":") {
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

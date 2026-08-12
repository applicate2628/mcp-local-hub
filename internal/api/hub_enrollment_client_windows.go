//go:build windows

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
)

const HubEnrollmentEndpointV1 = `\\.\pipe\mcp-local-hub-cst-saved-field-enrollment-v1`

type windowsHubEnrollmentClientV1 struct{}

type hubEnrollmentChallengeV1 struct {
	Version   int    `json:"version"`
	Challenge string `json:"challenge"`
}

func NewHubEnrollmentClientV1() HubEnrollmentClientV1 { return &windowsHubEnrollmentClientV1{} }

func (c *windowsHubEnrollmentClientV1) Enroll(ctx context.Context, req HubEnrollmentEnrollV1) (HubEnrollmentReceiptV1, error) {
	var zero HubEnrollmentReceiptV1
	conn, err := dialHubEnrollmentV1(ctx)
	if err != nil {
		return zero, err
	}
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, HubEnrollmentMaxFrameBytes+1)
	challengeRaw, err := readHubEnrollmentLineV1(reader)
	if err != nil {
		return zero, err
	}
	var challenge hubEnrollmentChallengeV1
	if err := DecodeHubEnrollmentFrameV1(challengeRaw, &challenge); err != nil || challenge.Version != 1 || challenge.Challenge == "" {
		return zero, fmt.Errorf("invalid enrollment challenge")
	}
	req.Version, req.Op, req.Challenge = 1, "enroll", challenge.Challenge
	if err := ValidateHubEnrollmentEnrollV1(req); err != nil {
		return zero, err
	}
	if err := writeHubEnrollmentFrameV1(conn, req); err != nil {
		return zero, err
	}
	receiptRaw, err := readHubEnrollmentLineV1(reader)
	if err != nil {
		return zero, err
	}
	var receipt HubEnrollmentReceiptV1
	if err := DecodeHubEnrollmentFrameV1(receiptRaw, &receipt); err != nil {
		return zero, err
	}
	if err := ValidateHubEnrollmentReceiptV1(receipt, req.Correlation, HubEnrollmentStateEnrolled); err != nil {
		return zero, err
	}
	return receipt, nil
}

func (c *windowsHubEnrollmentClientV1) Cancel(ctx context.Context, req HubEnrollmentCancelV1) (HubEnrollmentReceiptV1, error) {
	var zero HubEnrollmentReceiptV1
	conn, err := dialHubEnrollmentV1(ctx)
	if err != nil {
		return zero, err
	}
	defer conn.Close()
	// Cancellation is a fresh authenticated exchange. The daemon's initial
	// challenge is read and discarded because Cancel binds the existing
	// correlation, not a new capability proof.
	reader := bufio.NewReaderSize(conn, HubEnrollmentMaxFrameBytes+1)
	challengeRaw, err := readHubEnrollmentLineV1(reader)
	if err != nil {
		return zero, err
	}
	var challenge hubEnrollmentChallengeV1
	if err := DecodeHubEnrollmentFrameV1(challengeRaw, &challenge); err != nil || challenge.Version != 1 || challenge.Challenge == "" {
		return zero, fmt.Errorf("invalid enrollment challenge")
	}
	req.Version, req.Op = 1, "cancel"
	if err := ValidateHubEnrollmentCancelV1(req); err != nil {
		return zero, err
	}
	if err := writeHubEnrollmentFrameV1(conn, req); err != nil {
		return zero, err
	}
	receiptRaw, err := readHubEnrollmentLineV1(reader)
	if err != nil {
		return zero, err
	}
	var receipt HubEnrollmentReceiptV1
	if err := DecodeHubEnrollmentFrameV1(receiptRaw, &receipt); err != nil {
		return zero, err
	}
	if err := ValidateHubEnrollmentReceiptV1(receipt, req.Correlation, HubEnrollmentStateCancelled); err != nil {
		return zero, err
	}
	return receipt, nil
}

func dialHubEnrollmentV1(ctx context.Context) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	timeout := time.Until(deadline)
	if timeout <= 0 {
		return nil, context.DeadlineExceeded
	}
	conn, err := winio.DialPipe(HubEnrollmentEndpointV1, &timeout)
	if err != nil {
		return nil, fmt.Errorf("dial hub enrollment endpoint: %w", err)
	}
	_ = conn.SetDeadline(deadline)
	return conn, nil
}

func readHubEnrollmentLineV1(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read enrollment frame: %w", err)
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	if len(line) == 0 || len(line) > HubEnrollmentMaxFrameBytes {
		return nil, fmt.Errorf("enrollment frame size invalid")
	}
	return line, nil
}

func writeHubEnrollmentFrameV1(conn net.Conn, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(raw) > HubEnrollmentMaxFrameBytes {
		return fmt.Errorf("enrollment frame exceeds limit")
	}
	raw = append(raw, '\n')
	for len(raw) > 0 {
		n, err := conn.Write(raw)
		if err != nil {
			return fmt.Errorf("write enrollment frame: %w", err)
		}
		if n <= 0 {
			return fmt.Errorf("write enrollment frame: short write")
		}
		raw = raw[n:]
	}
	return nil
}
